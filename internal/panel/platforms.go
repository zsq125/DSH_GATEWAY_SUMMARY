// platforms.go 面板的「多平台」层。
//
// 相对合并前的 trae.go（把 traeapi2 的 /trae/api/* 反向代理到 /panel/api/trae/*），
// 这里的根本变化是：**不再有反代**。Trae 与 WorkBuddy 现在同进程，
// 面板直接调平台对象（进程内调用），因此：
//   - 没有第二个端口、第二个主机、第二把服务间密钥；
//   - 不存在「后端不可达」这个状态（合并前最常见的报错来源）；
//   - 新增平台不需要在面板里写任何代理代码。
//
// 路由设计（对外契约）：
//
//	GET  /panel/api/platforms                      → 平台列表（导航 + 能力集 + 接入路径）
//	GET  /panel/api/platforms/{kind}/overview      → 该平台概览
//	GET  /panel/api/platforms/{kind}/accounts      → 该平台账号（统一形态）
//	POST /panel/api/platforms/{kind}/actions/{act} → 全量动作（checkin/keepalive/balance）
//	POST /panel/api/platforms/{kind}/accounts/{uid}/{act} → 单账号动作
//	GET  /panel/api/platforms/{kind}/models        → 上游模型目录原始数据
//	POST /panel/api/platforms/{kind}/login/start   → 浏览器登录（能力探测：login）
//	GET  /panel/api/platforms/{kind}/login/result  → 登录轮询
//	POST /panel/api/platforms/{kind}/login/cancel  → 取消登录
//	POST /panel/api/platforms/{kind}/import        → 导入凭证
//
// 全部接口由 Registry 驱动：面板**不认识**任何具体平台包，
// 新增平台只需在装配处注册一行（见 cmd/server/main.go）。
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"allplatforms/internal/platform"
)

// itoa 局部小工具（汇总层的提示文案要用，避免为一个 strconv 引包）。
func itoa(n int) string { return strconv.Itoa(n) }

// Platforms 面板可见的平台注册表（由 cmd/server 注入）。
//
// nil = 未接线（测试裸用场景）：所有 /panel/api/platforms/* 返回 501。
type Platforms interface {
	All() []platform.Platform
	Get(k platform.Kind) platform.Platform
	Kinds() []platform.Kind
	Infos() []platform.PlatformInfo
	Capabilities(k platform.Kind) (platform.Capabilities, bool)
	TriggerAll(action platform.Action) map[platform.Kind]platform.ActionResult
	Accounts() []platform.Account
	Counts() map[string]int
}

// platformRoutes 注册多平台路由。
func (p *Panel) platformRoutes() {
	p.mux.HandleFunc("GET /panel/api/platforms", p.withAuth(p.platformsList))
	p.mux.HandleFunc("GET /panel/api/platforms/{kind}/overview", p.withAuth(p.platformOverview))
	p.mux.HandleFunc("GET /panel/api/platforms/{kind}/accounts", p.withAuth(p.platformAccounts))
	p.mux.HandleFunc("GET /panel/api/platforms/{kind}/models", p.withAuth(p.platformModels))
	p.mux.HandleFunc("POST /panel/api/platforms/{kind}/actions/{action}", p.withAuth(p.platformActionAll))
	p.mux.HandleFunc("POST /panel/api/platforms/{kind}/accounts/{uid}/{action}", p.withAuth(p.platformAccountAction))
	p.mux.HandleFunc("PATCH /panel/api/platforms/{kind}/accounts/{uid}", p.withAuth(p.platformAccountAction))
	p.mux.HandleFunc("POST /panel/api/platforms/{kind}/login/start", p.withAuth(p.platformLoginStart))
	p.mux.HandleFunc("GET /panel/api/platforms/{kind}/login/result", p.withAuth(p.platformLoginResult))
	p.mux.HandleFunc("POST /panel/api/platforms/{kind}/login/cancel", p.withAuth(p.platformLoginCancel))
	p.mux.HandleFunc("POST /panel/api/platforms/{kind}/import", p.withAuth(p.platformImport))
	// 从本机官方客户端零输入导入（能力探测：client_import）。
	p.mux.HandleFunc("GET /panel/api/platforms/{kind}/client/probe", p.withAuth(p.platformClientProbe))
	p.mux.HandleFunc("POST /panel/api/platforms/{kind}/client/import", p.withAuth(p.platformClientImport))
}

// ── 平台解析（所有路由共用的入口守卫）───────────────────────────────────

// resolvePlatform 从路径取出平台并校验存在性。
//
// 统一在这里做三重校验（缺一不可）：
//  1. Kind 字面合法（platform.ValidKind）——它是路径片段，必须防穿越；
//  2. 注册表里确实有该平台——防止拼错平台名时静默走到「空实现」；
//  3. 平台对象非 nil。
//
// 写回 JSON 后返回 false 表示已经响应过，调用方直接 return。
func (p *Panel) resolvePlatform(w http.ResponseWriter, r *http.Request) (platform.Platform, bool) {
	if p.cfg.Platforms == nil {
		writeErr(w, http.StatusNotImplemented, "platform registry not configured")
		return nil, false
	}
	kind := platform.Kind(r.PathValue("kind"))
	if !platform.ValidKind(kind) {
		writeErr(w, http.StatusBadRequest, "invalid platform kind")
		return nil, false
	}
	pl := p.cfg.Platforms.Get(kind)
	if pl == nil {
		writeErr(w, http.StatusNotFound, "unknown platform: "+string(kind))
		return nil, false
	}
	return pl, true
}

// ── 列表 ────────────────────────────────────────────────────────────────

// platformsList 返回全部平台元信息（前端导航与能力卡片的数据源）。
func (p *Panel) platformsList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Platforms == nil {
		writeErr(w, http.StatusNotImplemented, "platform registry not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"platforms": p.cfg.Platforms.Infos(),
		"counts":    p.cfg.Platforms.Counts(),
	})
}

// ── 概览 / 账号 ─────────────────────────────────────────────────────────

// platformOverview 该平台概览。
//
// 优先用平台自己的 Status()（若实现了 StatusProvider）；
// 否则用统一形态的账号计数兜底。两种都拿不到就返回空计数——
// 刻意不返回错误：一个平台没有概览数据不是故障。
func (p *Panel) platformOverview(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	out := map[string]any{
		"platform":     pl.Kind(),
		"display_name": pl.DisplayName(),
		"models_path":  pl.ModelsPath(),
	}
	if sp, isSP := pl.(platform.StatusProvider); isSP {
		out["status"] = sp.Status()
	}
	accs := pl.Accounts()
	out["total"] = len(accs)
	// 能力集一并透出：前端据此决定渲染哪些按钮，避免「点了才发现 501」。
	if caps, found := p.cfg.Platforms.Capabilities(pl.Kind()); found {
		out["capabilities"] = caps
	}
	writeJSON(w, http.StatusOK, out)
}

// platformAccounts 该平台账号列表（统一形态）。
func (p *Panel) platformAccounts(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	accs := pl.Accounts()
	if accs == nil {
		accs = []platform.Account{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"platform": pl.Kind(),
		"accounts": accs,
	})
}

// ── 动作 ────────────────────────────────────────────────────────────────

// platformActionAll 触发全量动作。
func (p *Panel) platformActionAll(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	action := platform.Action(r.PathValue("action"))
	if !validAction(action) {
		writeErr(w, http.StatusBadRequest, "invalid action: "+string(action))
		return
	}
	res := pl.Trigger(action, "")
	if !res.Supported {
		// 能力缺口不是错误：返回 501 让前端显示「暂不支持」而不是红色报错。
		writeJSON(w, http.StatusNotImplemented, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// platformAccountAction 单账号动作。
//
// PATCH 走同一条路由（PathValue("action") 为空时视为 patch），
// 因为「改备注/启停」与「签到/刷新」在面板里是同一个按钮组，
// 拆成两条路由只会让前端多写一个分支。
func (p *Panel) platformAccountAction(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	uid := r.PathValue("uid")
	action := r.PathValue("action")
	if r.Method == http.MethodPatch {
		action = "patch"
	}
	if action == "" {
		writeErr(w, http.StatusBadRequest, "missing action")
		return
	}
	// uid 作为下游单段路径片段使用，沿用既有白名单校验（与 WorkBuddy 侧同口径）。
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "invalid uid")
		return
	}
	aa, isAA := pl.(platform.AccountActioner)
	if !isAA {
		writeErr(w, http.StatusNotImplemented, "platform does not support account actions")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	status, resp, err := aa.AccountAction(uid, action, raw)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, status, resp)
}

// ── 模型 ────────────────────────────────────────────────────────────────

// platformModels 上游模型目录原始数据（仅面板可见，不给下游客户端）。
func (p *Panel) platformModels(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	rm, isRM := pl.(platform.RawModelLister)
	if !isRM {
		writeErr(w, http.StatusNotImplemented, "platform does not expose raw models")
		return
	}
	v, err := rm.RawModels()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── 登录 / 导入 ─────────────────────────────────────────────────────────

func (p *Panel) platformLoginStart(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	lp, isLP := pl.(platform.LoginProvider)
	if !isLP {
		writeErr(w, http.StatusNotImplemented, "platform does not support browser login")
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	v, err := lp.LoginStart(raw)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *Panel) platformLoginResult(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	lp, isLP := pl.(platform.LoginProvider)
	if !isLP {
		writeErr(w, http.StatusNotImplemented, "platform does not support browser login")
		return
	}
	v, err := lp.LoginResult(r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *Panel) platformLoginCancel(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	lp, isLP := pl.(platform.LoginProvider)
	if !isLP {
		writeErr(w, http.StatusNotImplemented, "platform does not support browser login")
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	v, err := lp.LoginCancel(raw)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *Panel) platformImport(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	imp, isImp := pl.(platform.Importer)
	if !isImp {
		writeErr(w, http.StatusNotImplemented, "platform does not support credential import")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// 兼容两种提交形状：{"payload":"..."} 与裸字符串/裸 JSON。
	var probe struct {
		Payload string `json:"payload"`
	}
	payload := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &probe) == nil && probe.Payload != "" {
		payload = probe.Payload
	}
	v, err := imp.Import(payload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── 从本机官方客户端导入 ────────────────────────────────────────────────

// platformClientProbe 探测本机是否有可读的官方客户端凭证（不导入）。
func (p *Panel) platformClientProbe(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	ci, isCI := pl.(platform.ClientImporter)
	if !isCI {
		writeErr(w, http.StatusNotImplemented, "platform does not support client import")
		return
	}
	writeJSON(w, http.StatusOK, ci.ProbeClient())
}

// platformClientImport 从本机官方客户端读取凭证并导入。
//
// 语义：**未找到客户端不是错误** —— 平台层返回 ok=false + 探测过程，
// 这里原样 200 透出，让前端渲染引导文案（而不是红色报错）。
// 真正的错误（解密失败/导入失败）才走 4xx。
func (p *Panel) platformClientImport(w http.ResponseWriter, r *http.Request) {
	pl, ok := p.resolvePlatform(w, r)
	if !ok {
		return
	}
	ci, isCI := pl.(platform.ClientImporter)
	if !isCI {
		writeErr(w, http.StatusNotImplemented, "platform does not support client import")
		return
	}
	v, err := ci.ImportFromClient()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// validAction 动作白名单（枚举，防止把任意路径片段当动作透传）。
func validAction(a platform.Action) bool {
	switch a {
	case platform.ActionCheckin, platform.ActionKeepalive, platform.ActionBalance:
		return true
	}
	return false
}
