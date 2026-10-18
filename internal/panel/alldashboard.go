// alldashboard.go 「All Account」统一面板：把所有平台的账号汇总到一个视图，
// 支持合并签到 / 合并保活 / 合并刷新额度。
//
// 设计取舍 —— **为什么在网关侧聚合而不是前端各拉各的再拼**：
//   - 前端拼接要发 N 次请求、N 次鉴权、N 套错误处理，任一平台缺席时还得在
//     每个渲染点重复判断"半个后端不在"；聚合在后端做一次，前端就是一张表；
//   - 合并动作需要「任一边失败不掩盖另一边」的结果语义，放在后端能保证
//     前端拿到的结构永远稳定（永远是同样的字段）；
//   - 各平台的凭据只留在网关侧，前端永远接触不到。
//
// 合并语义（每处都显式标注，避免调用方误解）：
//   - 账号列表：各平台拼接，platform 字段标明来源；
//   - 动作：各平台并发触发，一边失败不影响另一边，各自的结果分区返回；
//   - 计数：total 是各平台之和，healthy 等分项也是各平台之和（不做"全局可用率"
//     这种混合口径——各上游的可用定义不同，混算出来的数字没有行动意义）。
//
// 相对合并前的变化：这里从「写死 WorkBuddy + Trae 两个来源」改成
// **遍历注册表**。新增平台时本文件零改动。
package panel

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"allplatforms/internal/platform"
	"allplatforms/internal/platform/workbuddy/pool"
)

// ── 数据结构 ─────────────────────────────────────────────────────────────

// AllAccount 一行账号（各平台统一形态）。
//
// 直接复用 platform.Account：合并前这里有一份自己的 AllAccount 结构，
// 与平台的账号模型重复；统一到抽象层后，面板与平台不可能再漂移。
type AllAccount = platform.Account

// SideResult 单平台批量动作的结果。
type SideResult struct {
	Platform  string       `json:"platform"`
	OK        bool         `json:"ok"`        // 该平台是否成功完成
	Enabled   bool         `json:"enabled"`   // 该平台是否已注册（false = 配置里没开，不算错误）
	Supported bool         `json:"supported"` // 该平台是否支持该动作（不支持不算错误）
	Error     string       `json:"error,omitempty"`
	Total     int          `json:"total"`
	Ok        int          `json:"ok_count"`
	Skipped   int          `json:"skipped"`
	Failed    int          `json:"failed"`
	Async     bool         `json:"async"`
	Results   []SideDetail `json:"results,omitempty"`
}

// SideDetail 单账号明细（各平台统一字段）。
type SideDetail struct {
	UID     string `json:"uid"`
	Name    string `json:"name,omitempty"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped"`
	Msg     string `json:"msg,omitempty"`
	Err     string `json:"error,omitempty"`
}

// AllDashboard 汇总面板的一次性响应。
type AllDashboard struct {
	GeneratedAt string `json:"generated_at"`

	Counts  map[string]int `json:"counts"` // total / healthy / cooling / disabled / paused / checked_in + 各平台计数
	Credits int64          `json:"credits"`

	// Platforms 各平台元信息（含能力集与接入路径），前端据此渲染分区与引导。
	Platforms []platform.PlatformInfo `json:"platforms"`

	Accounts []AllAccount `json:"accounts"`
}

// ── 汇总视图 ─────────────────────────────────────────────────────────────

// allOverview 返回所有平台账号的合并快照。
func (p *Panel) allOverview(w http.ResponseWriter, r *http.Request) {
	out := AllDashboard{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Counts:      map[string]int{},
	}
	if p.cfg.Platforms == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	out.Platforms = p.cfg.Platforms.Infos()
	out.Accounts = p.cfg.Platforms.Accounts()
	out.Counts = p.cfg.Platforms.Counts()
	for _, a := range out.Accounts {
		out.Credits += a.Credits
	}

	// 排序由 Registry.Accounts() 保证（平台注册顺序 + 积分降序），
	// 这里只做一次稳定兜底，防止未来某个平台返回乱序。
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		if out.Accounts[i].Platform != out.Accounts[j].Platform {
			return out.Accounts[i].Platform < out.Accounts[j].Platform
		}
		return out.Accounts[i].Credits > out.Accounts[j].Credits
	})

	writeJSON(w, http.StatusOK, out)
}

// ── 合并动作 ────────────────────────────────────────────────────────────

// allCheckin **异步**触发所有平台签到，立刻返回"已触发"。
//
// 契约与既有的 /panel/api/checkin_all 一致：那是 fire-and-forget，前端点完
// 按钮立刻恢复、toast 提示已开始，之后靠面板既有的定时轮询把积分到账
// 无感刷出来。让合并入口同步等待会把这个交互毁掉——按钮卡几十秒。
func (p *Panel) allCheckin(w http.ResponseWriter, r *http.Request) {
	p.allAction(w, platform.ActionCheckin, "签到")
}

// allKeepalive **异步**触发所有平台保活。
func (p *Panel) allKeepalive(w http.ResponseWriter, r *http.Request) {
	p.allAction(w, platform.ActionKeepalive, "保活")
}

// allBalance **异步**触发所有平台余额刷新。
func (p *Panel) allBalance(w http.ResponseWriter, r *http.Request) {
	p.allAction(w, platform.ActionBalance, "刷新余额")
}

// allAction 三个合并入口的共同实现。
//
// 响应形状：{"platforms": {kind: SideResult}, "summary": "..."}。
// 用 map 而非固定字段：新增平台时响应自动多一项，前端零改动。
func (p *Panel) allAction(w http.ResponseWriter, action platform.Action, label string) {
	if p.cfg.Platforms == nil {
		writeErr(w, http.StatusNotImplemented, "platform registry not configured")
		return
	}
	res := p.cfg.Platforms.TriggerAll(action)

	sides := map[string]SideResult{}
	for _, pl := range p.cfg.Platforms.All() {
		k := pl.Kind()
		ar, ok := res[k]
		if !ok {
			continue
		}
		sides[string(k)] = SideResult{
			Platform:  string(k),
			OK:        ar.OK,
			Enabled:   true,
			Supported: ar.Supported,
			Error:     ar.Error,
			Total:     ar.Total,
			Ok:        ar.Ok,
			Skipped:   ar.Skipped,
			Failed:    ar.Failed,
			Async:     ar.Async,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"platforms": sides,
		"summary":   buildSummaryMulti(label, sides),
	})
}

// buildSummaryMulti 一句话概括各平台结果（前端直接 toast 用）。
//
// 三种呈现，刻意区分「不支持」和「失败」：
//   - 支持且成功 → "Trae 签到 N 个"
//   - 不支持     → 静默跳过（能力缺口，不是错误）
//   - 失败       → 明说原因（用户需要知道为什么少了一半）
func buildSummaryMulti(action string, sides map[string]SideResult) string {
	// 固定按平台名排序，保证同一次动作的文案顺序稳定（map 迭代随机）。
	keys := make([]string, 0, len(sides))
	for k := range sides {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := []string{}
	for _, k := range keys {
		s := sides[k]
		name := platformDisplayName(k)
		switch {
		case !s.Supported:
			// 能力缺口：不提（不是错误）
		case s.OK:
			parts = append(parts, name+" "+action+" "+itoa(s.Ok)+" 个")
		default:
			parts = append(parts, name+" "+action+"失败（"+s.Error+"）")
		}
	}
	if len(parts) == 0 {
		return action + "：没有平台支持该动作"
	}
	return strings.Join(parts, "　|　")
}

// platformDisplayName 平台标识 → 展示名。
//
// 不查注册表而用固定映射：buildSummaryMulti 可能在注册表不可用的路径被调
// （例如单测直接构造 sides）。未知 Kind 回落原样输出，不丢信息。
func platformDisplayName(k string) string {
	switch platform.Kind(k) {
	case platform.KindWorkBuddy:
		return "WorkBuddy"
	case platform.KindTrae:
		return "Trae"
	}
	return k
}

// ── WorkBuddy 专用状态归一化（保留：WorkBuddy 账号池视图也用它）───────
//
// 为什么留在这里而不是搬进平台包：pool.Status → 统一词表的映射是
// **面板展示口径**（disabled > paused > cooling > ok 的优先级），
// 与平台的选号逻辑无关。放在平台包里会让平台多一个只给 UI 用的函数。

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

// wbCounts WorkBuddy 账号池总数（面板既有接口用）。
func (p *Panel) wbCounts() int {
	if p.cfg.Pool == nil {
		return 0
	}
	total, _, _, _, _, _ := p.cfg.Pool.CountsDetailedWithPaused()
	return total
}
