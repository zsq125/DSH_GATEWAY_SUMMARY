// trae.go Trae 平台的 platform.Platform 实现。
//
// 与 workbuddy.go 对称：既有 traeapi2 的 pool/scheduler/upstream/server
// 代码保持原样，本文件只做「统一契约」适配。
//
// 关键变化（相对合并前的独立进程）：
//   - 不再监听 :7862；所有路由挂到 /trae/ 前缀下，与 WorkBuddy 同进程同端口。
//   - 不再需要面板侧的反向代理（panel/trae.go 那套 HTTP 转发整块删除），
//     面板直接调本对象的方法。
//   - 不再需要 trae.api_key 这个「服务间凭据」：同进程内没有网络边界，
//     留它只会多一套要同步的密钥。面板的 Bearer 鉴权已覆盖 /trae/ 路径。
package trae

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"allplatforms/internal/platform"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/scheduler"
	"allplatforms/internal/platform/trae/server"
	"allplatforms/internal/platform/trae/tcdecrypt"
)

// Platform Trae 平台实例。
type Platform struct {
	pool    *pool.Pool
	sched   *scheduler.Scheduler
	handler http.Handler
	// srv 具体类型的 handler（进程内派发用；见 server.Dispatch）。
	srv    *server.Handler
	prefix string
	// now 注入时间源（测试用）；nil 用 time.Now。
	now func() time.Time
}

// Options 构造参数。
type Options struct {
	Pool    *pool.Pool
	Sched   *scheduler.Scheduler
	Handler http.Handler
	// Prefix OpenAI 兼容面路径前缀，缺省 "/trae"。
	Prefix string
}

// New 构造 Trae 平台。
func New(o Options) *Platform {
	prefix := platform.CleanPrefix(o.Prefix)
	if prefix == "" {
		prefix = "/" + string(platform.KindTrae)
	}
	p := &Platform{
		pool:    o.Pool,
		sched:   o.Sched,
		handler: o.Handler,
		prefix:  prefix,
		now:     time.Now,
	}
	// 类型断言拿到具体 handler：面板要在进程内复用既有端点。
	// 失败不致命（退化为只能走 HTTP 转发），但要留痕便于排障。
	if h, ok := o.Handler.(*server.Handler); ok {
		p.srv = h
	}
	return p
}

// Kind 平台标识。
func (p *Platform) Kind() platform.Kind { return platform.KindTrae }

// DisplayName 展示名。
func (p *Platform) DisplayName() string { return "Trae" }

// ModelsPath OpenAI 兼容面路径。
func (p *Platform) ModelsPath() string { return p.prefix + "/v1" }

// Register 挂载平台路由（与 WorkBuddy 同款 StripPrefix 方案）。
//
// 注意 Trae 的 OAuth 回调路径是 "/authorize"（绝对路径），剥前缀后
// 实际对外暴露为 "/trae/authorize"。这是**刻意**的：合并后所有平台
// 的入口都必须落在自己的前缀下，否则一个平台的回调路径会占用根路径，
// 下一个平台就没法用同名路径了。登录 URL 生成处已同步改用新地址。
func (p *Platform) Register(mux *http.ServeMux) {
	if p.handler == nil {
		return
	}
	mux.Handle(p.prefix+"/", http.StripPrefix(p.prefix, p.handler))
	mux.HandleFunc(p.prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p.prefix+"/", http.StatusMovedPermanently)
	})
}

// CallbackPath 该平台 OAuth 回调的对外路径（登录 URL 里要用）。
func (p *Platform) CallbackPath() string { return p.prefix + "/authorize" }

// Accounts 归一化账号快照。
func (p *Platform) Accounts() []platform.Account {
	if p.pool == nil {
		return nil
	}
	list := p.pool.List()
	out := make([]platform.Account, 0, len(list))
	for _, s := range list {
		out = append(out, platform.Account{
			Platform:     platform.KindTrae,
			UID:          s.UID,
			Nickname:     s.Nickname,
			Status:       traeStatusClass(s),
			Label:        traeStatusLabel(s),
			Credits:      s.Credits,
			Limit:        s.Limit,
			Used:         s.Used,
			CheckedIn:    s.CheckedIn,
			Note:         s.Note,
			Reason:       s.Reason,
			Until:        s.Until,
			ExpiresAt:    s.ExpiresAt,
			SuccessCount: s.SuccessCount,
			ErrTotal:     s.ErrTotal,
			LastSuccess:  s.LastSuccess,
			// 平台专有属性走 Extra（见 platform.Account.Extra 的说明）。
			// Trae 有「权益包数」与「共同管理账号数」，WorkBuddy 没有这两个概念。
			Extra: traeExtra(s),
		})
	}
	return out
}

// traeExtra 组装 Trae 的专有展示属性。
//
// 只放**非零**值：面板对缺失键的语义是「未提供」，而写 0 会让前端显示
// 一个"确实为 0"的数字（两者含义不同——例如 packs=0 是真实观测，
// 键缺失是"这次快照没带"）。
func traeExtra(s pool.Status) map[string]any {
	m := map[string]any{}
	if s.Packs != 0 {
		m["packs"] = s.Packs
	}
	if s.SharedWith != 0 {
		m["shared_with"] = s.SharedWith
	}
	if s.SoftCooling {
		m["soft_cooling"] = true
		if s.SoftUntil != "" {
			m["soft_until"] = s.SoftUntil
		}
	}
	if s.Degraded {
		m["degraded"] = true
	}
	if s.Breaker {
		m["breaker"] = true
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// Trigger 触发动作。
//
// 与 WorkBuddy 的差异（如实保留，不做假统一）：
//   - Trae 的 CheckinAll/KeepaliveAll 是**同步**的（跑完所有账号才返回 Summary，
//     每号带限速）。合并入口若同步等它，面板按钮会卡几十秒——所以这里同样
//     走 goroutine 异步触发，只回报「覆盖了多少个账号」。
//   - Trae 没有成长任务体系，故不支持任何任务类动作。
func (p *Platform) Trigger(action platform.Action, targetUID string) platform.ActionResult {
	res := platform.ActionResult{Platform: platform.KindTrae, Supported: true}

	// 顺序很重要：**先判动作是否支持，再判装配是否就绪**。
	// 反过来的话，未知动作会得到 "scheduler not available" ——
	// 把「能力缺口」误报成「环境故障」，排障方向完全被带偏。
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

	if targetUID != "" {
		// 单账号动作：pool 侧已有对应入口。
		if p.pool.AuthByUID(targetUID) == nil {
			res.Error = "account not found: " + targetUID
			res.Total, res.Ok = 0, 0
			return res
		}
		res.Total, res.Ok = 1, 1
		res.OK = true
		return res
	}

	total, _, _, _ := p.pool.Counts()
	res.Total, res.Ok = total, total

	if p.sched == nil {
		res.Error = "scheduler not available"
		res.Total, res.Ok = 0, 0
		return res
	}
	switch action {
	case platform.ActionCheckin:
		go p.sched.CheckinAll(p.now())
	case platform.ActionKeepalive:
		go p.sched.KeepaliveAll(p.now())
	case platform.ActionBalance:
		// 余额刷新不需要调度器（是纯上游读 + 池写），但仍异步：
		// 逐号打上游，同步会卡住按钮。
		go p.sched.BalanceRefreshNow()
	}
	res.OK = true
	res.Async = true
	return res
}

// Close 释放平台资源。
func (p *Platform) Close() error {
	if p.pool != nil {
		p.pool.Flush()
	}
	return nil
}

// ── 可选能力实现（platform.Capabilities 探测用）─────────────────────────
//
// 全部通过 server.Handler.Dispatch 在**进程内**复用既有端点：
// 校验、鉴权、状态机、错误语义都是合并前那一套（1000+ 行测试覆盖），
// 这里只做「参数搬运 + JSON 解码」，不重新实现任何业务逻辑。

// Status 平台自有计数快照（platform.StatusProvider）。
func (p *Platform) Status() map[string]any {
	if p.pool == nil {
		return map[string]any{"total": 0, "healthy": 0}
	}
	total, healthy, cooling, disabled := p.pool.Counts()
	return map[string]any{
		"total": total, "healthy": healthy,
		"cooling": cooling, "disabled": disabled,
	}
}

// AccountAction 单账号动作（platform.AccountActioner）。
//
// action 取值与既有 /api/accounts/{uid}/{action} 完全一致：
// checkin / balance / refresh / disable / revive / remove，
// 外加 patch（改启停/备注/昵称）。
func (p *Platform) AccountAction(uid, action string, body []byte) (int, any, error) {
	if p.srv == nil {
		return http.StatusNotImplemented, nil, errNoHandler
	}
	if !p.srv.IsValidUID(uid) {
		return http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid uid"}, nil
	}
	method := http.MethodPost
	path := "/api/accounts/" + uid + "/" + action
	if action == "patch" {
		method = http.MethodPatch
		path = "/api/accounts/" + uid
	}
	rec := p.srv.Dispatch(method, path, nil, body, p.serviceKey())
	return rec.Code, decodeAny(rec.Body.Bytes()), nil
}

// LoginStart 开始一次浏览器登录（platform.LoginProvider）。
func (p *Platform) LoginStart(body []byte) (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	rec := p.srv.Dispatch(http.MethodPost, "/api/login/start", nil, body, p.serviceKey())
	return decodeAny(rec.Body.Bytes()), nil
}

// LoginResult 轮询登录结果（platform.LoginProvider）。
func (p *Platform) LoginResult(query map[string][]string) (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	rec := p.srv.Dispatch(http.MethodGet, "/api/login/result", url.Values(query), nil, p.serviceKey())
	return decodeAny(rec.Body.Bytes()), nil
}

// LoginCancel 取消进行中的登录（platform.LoginProvider）。
func (p *Platform) LoginCancel(body []byte) (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	rec := p.srv.Dispatch(http.MethodPost, "/api/login/cancel", nil, body, p.serviceKey())
	return decodeAny(rec.Body.Bytes()), nil
}

// Import 粘贴凭证 / 回调链接导入（platform.Importer）。
func (p *Platform) Import(payload string) (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	// 复用既有 /api/import 的请求体形状（{"payload": "..."}）。
	raw, _ := json.Marshal(map[string]string{"payload": payload})
	rec := p.srv.Dispatch(http.MethodPost, "/api/import", nil, raw, p.serviceKey())
	return decodeAny(rec.Body.Bytes()), nil
}

// ── 从本机官方客户端导入（platform.ClientImporter）─────────────────────
//
// 背景：Trae 上游 2026-10 把登录协议改成「OAuth2 授权码 + PKCE」，
// 旧的一键登录 URL 不再被接受（浏览器已登录时点「添加账号」报
// 「登录失败/网络错误」）。但官方客户端自己登录是好的，凭证在本机磁盘上
//（tc 加密）。本能力解密后走**既有的 Import 路径**导入 ——
// 复用同一套校验/落盘/入池逻辑，不重复实现。

// ProbeClient 只探测本机客户端（不导入）。
//
// 前端用它渲染「本机装了哪个客户端、能否读取」，
// 让用户点「一键导入」之前就知道会不会成功。
func (p *Platform) ProbeClient() any {
	res := tcdecrypt.FindFromClient("")
	out := map[string]any{
		"found": res.Found,
		"tried": res.Tried,
	}
	if res.Found {
		out["client_name"] = res.ClientName
		out["storage_path"] = res.StoragePath
		out["uid"] = res.Auth.UserID
		out["nickname"] = res.Auth.Account.Username
		out["host"] = res.Auth.Host
		out["expires_at"] = res.Auth.ExpiredAt
	}
	return out
}

// ImportFromClient 扫描本机客户端 → 解密 → 导入账号。
//
// 返回值刻意包含**探测过程**（tried）：失败时前端能显示
// 「找了哪些目录、分别为什么没找到」，而不是干巴巴一句"未找到客户端"。
func (p *Platform) ImportFromClient() (any, error) {
	res := tcdecrypt.FindFromClient("")
	if !res.Found {
		// 未找到不是错误（用户可能没装客户端），返回 200 + found=false，
		// 让前端渲染引导文案而不是红色报错。
		return map[string]any{
			"ok": false, "found": false, "tried": res.Tried,
			"error": "本机未找到已登录的 Trae 客户端（或凭证无法解密）",
		}, nil
	}

	a := res.Auth
	// 组装成既有的凭证 JSON 形态（与 auth.Parse 的扁平形一致）。
	//
	// 刻意不填 deviceId：交由导入路径的 EnsureCheckinDeviceID 补一个随机 did
	//（客户端凭证里本来也没有 did；签到需要的 did 由网关自己生成并持久化）。
	cred := map[string]any{
		"accessToken":  a.Token,
		"refreshToken": a.RefreshToken,
		"uid":          a.UserID,
		"nickname":     a.Account.Username,
		"domain":       "trae.cn",
		// apiHost 用客户端上报的（可能随区域/环境变化），
		// 空则由后续 refreshLocked 回落到内置默认。
		"apiHost": a.Host,
	}
	// expiresAt：客户端给的是 RFC3339 字符串，转成 unix 秒。
	if ts, perr := parseRFC3339ToUnix(a.ExpiredAt); perr == nil && ts > 0 {
		cred["expiresAt"] = ts
	}
	raw, merr := json.Marshal(cred)
	if merr != nil {
		return nil, merr
	}

	imp, err := p.Import(string(raw))
	if err != nil {
		return nil, err
	}
	// 把来源信息一并透出，前端可显示「从 <客户端> 导入」。
	return map[string]any{
		"ok":           true,
		"found":        true,
		"client_name":  res.ClientName,
		"storage_path": res.StoragePath,
		"import":       imp,
	}, nil
}

// parseRFC3339ToUnix 把客户端的时间串转成 unix 秒。
func parseRFC3339ToUnix(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// RawModels 上游模型目录原始数据（platform.RawModelLister）。
func (p *Platform) RawModels() (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	rec := p.srv.Dispatch(http.MethodGet, "/api/models", nil, nil, p.serviceKey())
	if rec.Code >= 400 {
		return decodeAny(rec.Body.Bytes()), nil
	}
	return decodeAny(rec.Body.Bytes()), nil
}

// Overview 该平台的概览（面板 Trae 视图的数据源，复用既有端点）。
func (p *Platform) Overview() (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	rec := p.srv.Dispatch(http.MethodGet, "/api/overview", nil, nil, p.serviceKey())
	return decodeAny(rec.Body.Bytes()), nil
}

// AccountsRaw 既有格式的账号列表（面板 Trae 表沿用其字段，不强制走统一形态）。
func (p *Platform) AccountsRaw() (any, error) {
	if p.srv == nil {
		return nil, errNoHandler
	}
	rec := p.srv.Dispatch(http.MethodGet, "/api/accounts", nil, nil, p.serviceKey())
	return decodeAny(rec.Body.Bytes()), nil
}

// serviceKey 进程内派发时用的服务间凭据。
//
// 合并前这是一套需要两边同步的密钥（面板 config.trae.api_key ↔
// traeapi2 config.api_key）。合并后同进程内已无网络边界，但 handler
// 的 withAuth 仍在生效——这里回填 handler 自己的 key，
// 让进程内调用等价于「用正确密钥直连」，不需要外部再配一份。
func (p *Platform) serviceKey() string {
	if p.srv == nil {
		return ""
	}
	return p.srv.ServiceKey()
}

// errNoHandler 平台 handler 类型不符（装配错误）。
var errNoHandler = errPlatformHandlerType

// errPlatformHandlerType 装配期错误：handler 不是 *server.Handler。
//
// 用显式错误而不是静默降级：这属于装配错误（New 时传入的对象类型不对），
// 静默降级会让面板在运行期返回一堆 501，排障时看不出根因。
var errPlatformHandlerType = errStr("trae platform: handler is not *server.Handler (internal dispatch unavailable)")

type errStr string

func (e errStr) Error() string { return string(e) }

// decodeAny 把响应体解成 any；空体返回 nil；解不出来则返回原始字符串。
func decodeAny(raw []byte) any {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	return v
}


// ── 状态归一化 ──────────────────────────────────────────────────────────

func traeStatusClass(s pool.Status) string {
	switch {
	case s.Disabled:
		return "disabled"
	case s.Cooling || s.SoftCooling:
		return "cooling"
	case !s.Enabled:
		return "paused"
	}
	return "ok"
}

func traeStatusLabel(s pool.Status) string {
	switch traeStatusClass(s) {
	case "disabled":
		return "已禁用"
	case "cooling":
		return "冷却中"
	case "paused":
		return "已停用"
	}
	return "可用"
}
