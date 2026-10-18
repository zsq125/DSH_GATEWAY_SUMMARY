// Package platform 定义「多平台后端」的统一抽象。
//
// 背景（为什么要有这一层）：
//   - 合并前，WorkBuddy 与 Trae 是两个独立进程、两个端口（:7863 / :7862），
//     面板靠 HTTP 反向代理把 Trae 拼进来；每加一个平台就要多一个进程、
//     多一个端口、多一套 key、多一份前端分叉。
//   - 合并后，所有平台都在**同一个进程**里，只监听**一个端口**；
//     平台之间靠 URL 路径前缀区分（/workbuddy/... 、/trae/... ），
//     DSH 等下游只认一个 base URL，按路径选平台。
//
// 本包只放「平台之间真正共用的东西」：
//   - Platform 接口：一个平台必须能回答的四个问题（身份、路由、账号快照、批量动作）；
//   - Registry：平台注册表，统一派发路由与聚合动作；
//   - Account / Action：跨平台统一的账号与动作结果模型。
//
// 刻意**不**在这里做抽象的部分（否则会变成假抽象）：
//   - 选号策略、冷却语义、上游协议：各平台上游完全不同（WorkBuddy 是
//     workbuddy.ai 的 OpenAI 兼容面 + 双 realm；Trae 是 trae-api-cn 的
//     SOLO SSE 协议 + Cloud-IDE-JWT），强行统一只会把两边的差异挤进
//     一堆 if/switch 里。各平台在自己的包内实现，本层只要求它们
//     满足同一组接口。
//   - 配置结构：各平台配置差异极大，统一结构会逼出大量可选字段。
//     做法是「一个顶层 config.json，每个平台一个独立 section」。
package platform

import (
	"net/http"
	"sort"
	"strings"
)

// Kind 平台标识。作为 URL 路径前缀、配置 section 名、注册表 key 使用。
//
// 取值受约束（会拼进 URL 路径），只允许小写字母、数字、连字符。
type Kind string

// 已内置的平台。
const (
	KindWorkBuddy Kind = "workbuddy"
	KindTrae      Kind = "trae"
)

// Account 跨平台统一的账号快照。
//
// 为什么要有统一形态：面板的「All Account」视图要把所有平台的账号放进
// 同一张表，前端只认这一套字段。各平台内部的状态机差异（WorkBuddy 有
// paused/cooling/disabled，Trae 有 cooling/disabled）在各自的 Platform
// 实现里归一到 Status 词表。
type Account struct {
	Platform Kind   `json:"platform"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Realm 仅部分平台有分域概念（WorkBuddy: cn / global）；无则为空。
	Realm string `json:"realm,omitempty"`
	// Status 归一化状态词表：ok / cooling / disabled / paused。
	Status string `json:"status"`
	// Label 展示用中文状态（与 Status 一一对应）。
	Label   string `json:"label"`
	Credits int64  `json:"credits"`
	Limit   int64  `json:"credits_total"`
	Used    int64  `json:"credits_used"`
	// CheckedIn 今日是否已签到。
	CheckedIn bool   `json:"checked_in"`
	Note      string `json:"note,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Until     string `json:"until,omitempty"`
	// ExpiresAt 凭证过期时刻（部分平台有，如 Trae）。
	ExpiresAt    string `json:"expires_at,omitempty"`
	SuccessCount int64  `json:"success_count,omitempty"`
	ErrTotal     int64  `json:"err_total,omitempty"`
	LastSuccess  string `json:"last_success,omitempty"`

	// Extra 平台专有属性的扩展位（可选）。
	//
	// 为什么留这个袋子而不是为每个专有属性加一个字段：
	// 各平台的账号属性集合本来就不一样（Trae 有「权益包数 / 共同管理账号数」，
	// WorkBuddy 没有；将来新平台还会有别的）。若每个专有属性都在这里加一个
	// 具名字段，抽象层就会被具体平台的历史包袱持续污染，且新平台作者不敢加字段。
	//
	// 约定：
	//   - 只放**展示用**属性（面板渲染），不参与任何选号/路由决策；
	//   - 键名由平台自己定义，面板按 `extra.<key>` 读取，缺失即为未提供；
	//   - 保持扁平（不要嵌套结构），避免面板渲染时要做多层判空。
	Extra map[string]any `json:"extra,omitempty"`
}

// Action 平台支持的可触发批量/单账号动作。
//
// 用字符串常量而不是接口方法：动作集合是**可枚举的有限集**，
// 面板按钮、聚合入口、审计日志都要按名字引用；方法名做不到这点。
type Action string

const (
	ActionCheckin   Action = "checkin"   // 签到
	ActionKeepalive Action = "keepalive" // 凭证保活 / 刷新
	ActionBalance   Action = "balance"   // 刷新额度
)

// ActionResult 一次动作在单个平台上的结果。
type ActionResult struct {
	Platform  Kind   `json:"platform"`
	OK        bool   `json:"ok"`        // 动作本身是否成功完成
	Supported bool   `json:"supported"` // 该平台是否支持这个动作（不支持不算错误）
	Total     int    `json:"total"`
	Ok        int    `json:"ok_count"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
	Error     string `json:"error,omitempty"`
	// Async=true 表示动作是 fire-and-forget（触发即返回，结果靠轮询）。
	Async bool `json:"async"`
}

// Platform 一个平台后端必须满足的契约。
//
// 实现约定：实现必须是并发安全的（HTTP handler 会被并发调用）。
type Platform interface {
	// Kind 平台标识（同时是 URL 路径前缀与配置 section 名）。
	Kind() Kind
	// DisplayName 展示名（面板导航、日志用）。
	DisplayName() string
	// ModelsPath 该平台 OpenAI 兼容面的路径前缀，如 "/workbuddy/v1"。
	// 下游（DSH）用 baseURL = http://host:7863 + ModelsPath 接入。
	ModelsPath() string
	// Register 把该平台的 HTTP 路由注册到 mux 上（含自己的路径前缀）。
	Register(mux *http.ServeMux)
	// Accounts 返回当前账号快照（已归一化为统一形态）。
	Accounts() []Account
	// Trigger 触发一个动作。async 语义由实现决定，结果如实回报。
	// 单账号动作通过 targetUID 指定；targetUID 为空表示全量。
	Trigger(action Action, targetUID string) ActionResult
	// Close 释放平台资源（落盘状态、停调度器）。进程退出时调用。
	Close() error
}

// Registry 平台注册表。
//
// 存在意义：面板、聚合入口、健康检查都只跟 Registry 打交道，
// 不 import 任何具体平台包。新增平台 = 写一个实现 + 在装配处注册一行，
// 面板与聚合逻辑零改动。
type Registry struct {
	// 有序切片而非 map：面板导航顺序、聚合结果的字段顺序都要稳定，
	// map 迭代顺序随机会让前端每次刷新表格列序抖动。
	order []Kind
	byKey map[Kind]Platform
}

// NewRegistry 建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{byKey: map[Kind]Platform{}}
}

// Register 注册一个平台。重复 Kind 会被拒绝（返回 false），
// 避免两个平台抢同一个路径前缀导致路由互相覆盖。
func (r *Registry) Register(p Platform) bool {
	if p == nil {
		return false
	}
	k := p.Kind()
	if k == "" {
		return false
	}
	if _, dup := r.byKey[k]; dup {
		return false
	}
	r.byKey[k] = p
	r.order = append(r.order, k)
	return true
}

// Get 按 Kind 取平台；不存在返回 nil。
func (r *Registry) Get(k Kind) Platform { return r.byKey[k] }

// All 按注册顺序返回全部平台。
func (r *Registry) All() []Platform {
	out := make([]Platform, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.byKey[k])
	}
	return out
}

// Kinds 按注册顺序返回全部平台标识。
func (r *Registry) Kinds() []Kind {
	out := make([]Kind, len(r.order))
	copy(out, r.order)
	return out
}

// Len 已注册平台数。
func (r *Registry) Len() int { return len(r.order) }

// RegisterAll 把每个平台的 HTTP 路由挂到 mux 上。
//
// 路由冲突由 http.ServeMux 自己发现（重复 pattern 会 panic）——
// 这是刻意的：两个平台挂到同一前缀属于装配期错误，应该在启动时
// 立刻炸掉，而不是运行时静默地一半请求走错平台。
func (r *Registry) RegisterAll(mux *http.ServeMux) {
	for _, p := range r.All() {
		p.Register(mux)
	}
}

// Accounts 汇总所有平台的账号，按平台注册顺序 + 积分降序排列。
func (r *Registry) Accounts() []Account {
	var out []Account
	for _, p := range r.All() {
		out = append(out, p.Accounts()...)
	}
	// 排序键：先平台注册顺序（面板分区与导航一致），再积分降序。
	rank := map[Kind]int{}
	for i, k := range r.order {
		rank[k] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return rank[out[i].Platform] < rank[out[j].Platform]
		}
		return out[i].Credits > out[j].Credits
	})
	return out
}

// TriggerAll 对**所有支持该动作的平台**触发一次，返回逐平台结果。
//
// 关键语义：不支持该动作的平台返回 Supported=false 而不是错误——
// 「Trae 没有成长任务体系」是能力缺口，不是故障，面板据此显示
// 「暂不支持」而不是红色报错。
func (r *Registry) TriggerAll(action Action) map[Kind]ActionResult {
	out := make(map[Kind]ActionResult, len(r.order))
	for _, p := range r.All() {
		out[p.Kind()] = p.Trigger(action, "")
	}
	return out
}

// Counts 汇总账号状态计数。
//
// 返回的 map 键：total / healthy / cooling / disabled / paused / checked_in，
// 外加每个平台自己的账号数（键为平台 Kind）。
// 刻意不给「全局可用率」——各平台上游的可用定义不同，混算没有行动意义。
func (r *Registry) Counts() map[string]int {
	c := map[string]int{}
	for _, p := range r.All() {
		n := 0
		for _, a := range p.Accounts() {
			n++
			c["total"]++
			switch a.Status {
			case "ok":
				c["healthy"]++
			case "cooling":
				c["cooling"]++
			case "disabled":
				c["disabled"]++
			case "paused":
				c["paused"]++
			}
			if a.CheckedIn {
				c["checked_in"]++
			}
		}
		c[string(p.Kind())] = n
	}
	return c
}

// ValidKind 校验一个 Kind 是否可作为 URL 路径片段安全使用。
//
// 这是**安全边界**，不是风格检查：Kind 会直接拼进路由 pattern 与
// 下游 baseURL 路径。允许集合刻意收窄到 [a-z0-9-]，排除 '/'、'.'、
// '..'、空格与大小写（大小写混用会让 /Trae/ 与 /trae/ 变成两个不同
// 平台，排障时极其难查）。
func ValidKind(k Kind) bool {
	s := string(k)
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '-':
		default:
			return false
		}
	}
	return true
}

// CleanPrefix 规范化平台路径前缀：去尾斜杠、补前导斜杠。
// 输入 "workbuddy/v1/" → "/workbuddy/v1"；空串返回 ""。
func CleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}
