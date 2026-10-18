// workbuddy.go WorkBuddy 平台的 platform.Platform 实现。
//
// 本文件是「抽象层 ↔ 既有 WorkBuddy 代码」之间**唯一**的适配点：
// 既有 pool/server/scheduler/upstream 全部保持原样（一行未改），
// 本文件只把它们包成统一契约。这样合并带来的回归面被压到最小——
// WorkBuddy 的选号/冷却/协议行为与合并前完全一致。
package workbuddy

import (
	"log"
	"net/http"
	"strings"
	"time"

	"allplatforms/internal/platform"
	"allplatforms/internal/platform/workbuddy/pool"
)

// Platform WorkBuddy 平台实例。
type Platform struct {
	pool     *pool.Pool
	sched    Scheduler
	handler  http.Handler
	prefix   string // OpenAI 兼容面路径前缀，如 "/workbuddy"
	keyEmpty bool
}

// Scheduler 本平台需要的最小调度器能力。
//
// 为什么用接口而不是直接依赖 *scheduler.Scheduler：一是让 Platform 可单测
// （不用起真调度器），二是把「合并入口需要调度器做什么」显式写下来——
// 将来 Trae 或新平台有别的调度实现也能接进来。
type Scheduler interface {
	RunCheckinNow()
	RunKeepaliveNow()
	RunBalanceRefreshNow()
}

// Options 构造参数。
type Options struct {
	Pool    *pool.Pool
	Sched   Scheduler
	Handler http.Handler // 平台自己的 HTTP handler（OpenAI 兼容面 + 管理面）
	// Prefix OpenAI 兼容面的路径前缀，缺省 "/workbuddy"。
	Prefix string
	// KeyEmpty api_key 是否为空（空 = 不鉴权，用于健康检查判定）。
	KeyEmpty bool
}

// New 构造 WorkBuddy 平台。
func New(o Options) *Platform {
	prefix := platform.CleanPrefix(o.Prefix)
	if prefix == "" {
		prefix = "/" + string(platform.KindWorkBuddy)
	}
	return &Platform{
		pool:     o.Pool,
		sched:    o.Sched,
		handler:  o.Handler,
		prefix:   prefix,
		keyEmpty: o.KeyEmpty,
	}
}

// SetHandler 回填平台 handler。
//
// 为什么需要回填而不是构造时传入：WorkBuddy 的 handler 需要面板对象
// （server.Config.Panel 指向 panel.Panel），而面板又需要平台注册表
//（panel.Config.Platforms 指向 registry）——三者构成环。装配时只能
// 先建平台与注册表、再建面板、最后建 handler，然后回填。
//
// 这是**装配期一次性操作**（main 里紧跟 NewHandler 之后调用），
// 不在并发路径上；调用后不再变更。
func (p *Platform) SetHandler(h http.Handler) { p.handler = h }

// Kind 平台标识。
func (p *Platform) Kind() platform.Kind { return platform.KindWorkBuddy }

// DisplayName 展示名。
func (p *Platform) DisplayName() string { return "WorkBuddy" }

// ModelsPath OpenAI 兼容面路径（下游据此拼 baseURL）。
func (p *Platform) ModelsPath() string { return p.prefix + "/v1" }

// Register 挂载平台路由。
//
// 挂载方式：把既有 handler 整棵挂到前缀下，再由一层 StripPrefix 剥掉前缀。
//
// 为什么用 StripPrefix 而不是改写既有 handler 里的 pattern：
// 既有 handler 内部用 http.ServeMux 注册了 "/v1/chat/completions" 等
// **绝对路径**。若直接挂在 "/workbuddy/" 下，ServeMux 会按原样匹配，
// 结果是 /workbuddy/v1/... 匹配不到（404）。剥前缀让既有路由表原封不动
// 复用，既有测试（大量直接打 "/v1/..."）也全部继续有效。
func (p *Platform) Register(mux *http.ServeMux) {
	if p.handler == nil {
		return
	}
	// "/workbuddy/" 前缀树：剥掉前缀后交给平台 handler。
	// 注意 StripPrefix 的 path 不带尾斜杠，故 pattern 用 "前缀/" 匹配子树。
	mux.Handle(p.prefix+"/", http.StripPrefix(p.prefix, p.handler))
	// 裸前缀（"/workbuddy" 无尾斜杠）重定向到带斜杠，避免下游少打一个斜杠时 404。
	mux.HandleFunc(p.prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p.prefix+"/", http.StatusMovedPermanently)
	})
}

// Accounts 归一化账号快照。
func (p *Platform) Accounts() []platform.Account {
	if p.pool == nil {
		return nil
	}
	list := p.pool.List()
	out := make([]platform.Account, 0, len(list))
	for _, s := range list {
		row := platform.Account{
			Platform:     platform.KindWorkBuddy,
			UID:          s.UID,
			Nickname:     s.Nickname,
			Realm:        s.Realm,
			Status:       wbStatusClass(s),
			Label:        wbStatusLabel(s),
			Credits:      s.Credits,
			Limit:        s.CreditsTotal,
			CheckedIn:    s.CheckinDone,
			SuccessCount: s.SuccessCount,
			ErrTotal:     s.ErrTotal,
		}
		// Used 刻意不填：WorkBuddy 的 pool 只存「剩余 + 总额」，
		// 已用量是两者的差，推出来没有独立信息量（总额为 0 的旧数据还会推出负数）。
		if !s.LastSuccessTime.IsZero() {
			row.LastSuccess = s.LastSuccessTime.Format(time.RFC3339)
		}
		if row.Status == "cooling" {
			row.Reason = s.Reason
			if !s.Until.IsZero() {
				row.Until = s.Until.Format(time.RFC3339)
			}
		}
		if s.Disabled {
			row.Reason = s.DisabledReason
		}
		out = append(out, row)
	}
	return out
}

// Trigger 触发批量/单账号动作。
//
// 语义：**异步** fire-and-forget，与合并前 /panel/api/checkin_all 的契约
// 完全一致（点完立刻返回、结果靠前端轮询刷新）。同步等待会让按钮卡几十秒。
//
// 单账号动作（targetUID 非空）：WorkBuddy 的调度器只有全量入口，
// 单账号走 pool 侧已有的单号方法（见 AccountAction）。
func (p *Platform) Trigger(action platform.Action, targetUID string) platform.ActionResult {
	res := platform.ActionResult{Platform: platform.KindWorkBuddy, Supported: true}

	// 顺序很重要：**先判动作是否支持，再判装配是否就绪**。
	// 反过来的话，未知动作会得到 "scheduler not available" ——
	// 把一个「能力缺口」误报成「环境故障」，前端会显示红色错误
	// 并让人去查调度器，而真正的问题是动作名写错了。
	switch action {
	case platform.ActionCheckin, platform.ActionKeepalive, platform.ActionBalance:
	default:
		res.Supported = false
		res.Error = "unsupported action: " + string(action)
		return res
	}

	if p.pool == nil {
		res.Error = "pool not available"
		return res
	}
	total, _, _, _, _, _ := p.pool.CountsDetailedWithPaused()
	res.Total, res.Ok = total, total

	if targetUID != "" {
		// 单账号：不经过调度器（那是全量入口），只标记目标账号数。
		res.Total, res.Ok = 1, 1
		return res
	}

	if p.sched == nil {
		res.Error = "scheduler not available"
		res.Total, res.Ok = 0, 0
		return res
	}
	switch action {
	case platform.ActionCheckin:
		go p.sched.RunCheckinNow()
	case platform.ActionKeepalive:
		go p.sched.RunKeepaliveNow()
	case platform.ActionBalance:
		go p.sched.RunBalanceRefreshNow()
	}
	res.OK = true
	res.Async = true
	return res
}

// Close 释放平台资源。
func (p *Platform) Close() error {
	if p.pool != nil {
		p.pool.Close()
	}
	return nil
}

// ── 可选能力实现（platform.Capabilities 探测用）─────────────────────────

// Status 平台自有计数快照（platform.StatusProvider）。
//
// 与 Trae 侧的 Status() 对称：面板的「平台列表」按能力集渲染，
// 缺了这个方法会让 WorkBuddy 在能力探测里显示成「什么都没有」——
// 前端据此隐藏概览卡片，看起来像平台坏了。
//
// 计数口径用 CountsDetailedWithPaused（含 paused）：与账号池视图一致，
// 不含 paused 会让面板显示的账号数比实际少。
func (p *Platform) Status() map[string]any {
	if p.pool == nil {
		return map[string]any{"total": 0, "healthy": 0}
	}
	total, healthy, cooling, disabled, paused, inFlightFull := p.pool.CountsDetailedWithPaused()
	return map[string]any{
		"total": total, "healthy": healthy,
		"cooling": cooling, "disabled": disabled,
		"paused": paused, "in_flight_full": inFlightFull,
	}
}

// AccountAction 单账号动作（platform.AccountActioner）。
//
// WorkBuddy 的单账号入口在 pool 侧（既有方法），不走调度器——
// 调度器只有全量入口，单号动作走它会触发一整轮批量任务。
//
// action 取值沿用面板既有 /panel/api/accounts/{uid}/{action} 的命名：
// checkin / balance / revive / disable / pause / resume / remove。
func (p *Platform) AccountAction(uid, action string, body []byte) (int, any, error) {
	if p.pool == nil {
		return http.StatusNotImplemented, nil, errNoPool
	}
	if p.pool.AuthByUID(uid) == nil {
		return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}, nil
	}
	switch action {
	case "revive":
		if !p.pool.Revive(uid) {
			return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}, nil
		}
		return http.StatusOK, map[string]any{"ok": true, "uid": uid}, nil
	case "disable":
		p.pool.Disable(uid, "手动禁用")
		return http.StatusOK, map[string]any{"ok": true, "uid": uid}, nil
	case "pause":
		if !p.pool.Pause(uid) {
			return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}, nil
		}
		return http.StatusOK, map[string]any{"ok": true, "uid": uid}, nil
	case "resume":
		if !p.pool.Resume(uid) {
			return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}, nil
		}
		return http.StatusOK, map[string]any{"ok": true, "uid": uid}, nil
	case "remove":
		if a := p.pool.Remove(uid); a == nil {
			return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}, nil
		}
		return http.StatusOK, map[string]any{"ok": true, "uid": uid}, nil
	case "checkin", "balance":
		// 单号签到/刷新额度需要调度器的单号实现（WorkBuddy 调度器只有全量入口）。
		// 刻意返回 501 而不是偷偷跑一次全量：那会让「点了单号」变成
		// 「所有账号都被跑了一遍」，是比报错更糟的行为。
		// 面板的单号按钮走的是既有的 /panel/api/accounts/{uid}/{action}
		//（那条路径有完整实现），本入口是给「平台统一动作」用的。
		return http.StatusNotImplemented, map[string]any{
			"ok": false, "supported": false,
			"error": "单账号 " + action + " 请走账号池视图的按钮（/panel/api/accounts/{uid}/" + action + "）",
		}, nil
	}
	return http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action: " + action}, nil
}

// errNoPool 平台未接池（装配错误）。
var errNoPool = errStr("workbuddy platform: pool not configured")

type errStr string

func (e errStr) Error() string { return string(e) }

// ── 状态归一化 ──────────────────────────────────────────────────────────
//
// 优先级与既有账号池视图一致（disabled > paused > cooling > ok），
// 保证同一账号在「账号池」与「All Account」两个视图里标签相同——
// 不一致会让用户以为是两个账号。

func wbStatusClass(s pool.Status) string {
	switch {
	case s.Disabled:
		return "disabled"
	case s.Paused:
		return "paused"
	case s.Cooling:
		return "cooling"
	}
	return "ok"
}

func wbStatusLabel(s pool.Status) string {
	switch wbStatusClass(s) {
	case "disabled":
		return "已禁用"
	case "paused":
		return "已暂停"
	case "cooling":
		return "冷却中"
	}
	return "可用"
}

// KeyEmpty 该平台是否未设 api_key（健康检查用）。
func (p *Platform) KeyEmpty() bool { return p.keyEmpty }

// LogPrefix 统一日志前缀（多平台共进程后，日志必须能区分来源）。
func LogPrefix() string { return "[" + string(platform.KindWorkBuddy) + "]" }

// logf 带平台前缀的日志（避免合并后日志来源分不清）。
func logf(format string, args ...any) {
	log.Printf(LogPrefix()+" "+format, args...)
}

// 保持 strings 引用（Prefix 规范化在 platform 包里，这里仅用于兜底校验）。
var _ = strings.TrimSpace
