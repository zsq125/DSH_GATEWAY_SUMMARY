// login.go Trae Web 登录闭环：生成登录 URL → 回调捕获 → ExchangeToken →
// GetUserInfo → 落盘 → 热加载进池；统一 panel 轮询 result。
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/upstream"
)

// pendingState pending 登录状态。
type pendingState string

const (
	pendingActive   pendingState = "pending"
	pendingSuccess  pendingState = "success"
	pendingFailed   pendingState = "failed"
	pendingCanceled pendingState = "canceled"
)

// DefaultCallbackPath Trae 登录回调节默认路径。
const DefaultCallbackPath = "/authorize"

// pendingLogin 单次登录上下文（内存态，重启即失）。
type pendingLogin struct {
	state       pendingState
	machineID   string
	deviceID    string
	callbackURL string
	createdAt   time.Time
	uid         string
	nickname    string
	errMsg      string

	// codeVerifier PKCE 的 code_verifier（与登录 URL 里的 code_challenge 配对）。
	//
	// 为什么必须跨请求保存：challenge 在**生成 URL 时**产生（只有它的哈希
	// 发给上游），而 verifier 必须在**回调到达时**拿出来做授权码交换。
	// 上游校验 challenge == SHA256(verifier)，两者不配对则交换必失败。
	//
	// 存在 pending 会话里（而不是凭证文件）：它只在这 15 分钟的登录窗口内有效，
	// 登录完成后即无用，不该持久化。
	codeVerifier string
}

// loginTTL pending 最长有效期。
const loginTTL = 15 * time.Minute

// loginStart POST /api/login/start：生成登录 URL + pending 态。
func (h *Handler) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackPort int `json:"callback_port"`
	}
	_ = decodeJSON(r, &req)
	port := req.CallbackPort
	if port <= 0 {
		port = h.cfg.LoginConf.CallbackPort
	}
	if port <= 0 {
		port = 18081
	}
	callbackURL := "http://127.0.0.1:" + strconv.Itoa(port) + h.cfg.LoginConf.CallbackPath

	machineID, err := auth.NewHexID(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	deviceID, err := auth.NewCheckinDeviceID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	pendingID, err := auth.NewHexID(8)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	// PKCE：生成 code_verifier，其 SHA256 作为 code_challenge 拼进登录 URL。
	// verifier 存进 pending 会话，等回调到达时做授权码交换（两者必须配对）。
	verifier, challenge, err := NewPKCEPair()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}

	h.loginMu.Lock()
	h.logins[pendingID] = &pendingLogin{
		state: pendingActive, machineID: machineID, deviceID: deviceID,
		callbackURL: callbackURL, createdAt: time.Now(),
		codeVerifier: verifier,
	}
	h.loginMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"login_url":    BuildLoginURL(machineID, deviceID, callbackURL, challenge),
		"pending_id":   pendingID,
		"callback_url": callbackURL,
	})
}

// loginResult GET /api/login/result?pending_id=
func (h *Handler) loginResult(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("pending_id")
	h.loginMu.Lock()
	pl, ok := h.logins[id]
	if ok && time.Since(pl.createdAt) > loginTTL {
		delete(h.logins, id)
		pl, ok = nil, false
	}
	h.loginMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "pending login not found (expired or invalid)")
		return
	}
	resp := map[string]any{"pending_id": id, "state": string(pl.state)}
	switch pl.state {
	case pendingSuccess:
		resp["uid"] = pl.uid
		resp["nickname"] = pl.nickname
	case pendingFailed:
		resp["error"] = pl.errMsg
	}
	writeJSON(w, http.StatusOK, resp)
}

// loginCancel POST /api/login/cancel
func (h *Handler) loginCancel(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("pending_id")
	if id == "" {
		var req struct {
			PendingID string `json:"pending_id"`
		}
		_ = decodeJSON(r, &req)
		id = req.PendingID
	}
	h.loginMu.Lock()
	_, ok := h.logins[id]
	delete(h.logins, id)
	h.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"pending_id": id, "canceled": ok})
}

// ── PKCE（RFC 7636）──────────────────────────────────────────────────────
//
// 上游 2026-10 起在登录 URL 里要求 PKCE 参数。官方客户端的实现（从
// resources/app/out/main.js 提取）：
//
//	function IPe() {
//	  const t = crypto.randomBytes(48).toString("base64url");        // verifier
//	  const e = crypto.createHash("sha256").update(t).digest("base64url"); // challenge
//	  return { codeVerifier: t, codeChallenge: e };
//	}
//
// 即：verifier = 48 字节随机数的 base64url；challenge = SHA256(verifier) 的 base64url。
// 本函数与官方逐字节等价（同样的随机长度、同样的摘要、同样的 base64url 无填充编码）。

// NewPKCEPair 生成一对 PKCE 参数（verifier, challenge）。
func NewPKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 48) // 与官方一致：48 字节
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf) // base64url 无填充
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// BuildLoginURL 构造 Trae 登录页 URL（参数集对齐官方客户端）。
//
// ── 参数来源（2026-10-14 从官方客户端实测提取，不是猜的）──────────────
//
// 权威依据：本机 TRAE SOLO CN 客户端的运行时日志里，官方自己打开浏览器时
// 用的完整 URL（`%APPDATA%\TRAE SOLO CN\logs\<时间戳>\main.log`）：
//
//	authorization?login_version=1&auth_from=solo&login_channel=native_ide
//	  &plugin_version=2.3.91089&auth_type=local&client_id=en1oxy7wnw8j9n
//	  &redirect=0&login_trace_id=<uuid>
//	  &auth_callback_url=http://127.0.0.1:<port>/authorize   ← 必须是根路径
//	  &machine_id=<64位hex>&device_id=<16位数字>&x_device_id=…&x_machine_id=…
//	  &x_device_brand=20378&x_device_type=windows&x_os_version=Windows%2010%20Pro
//	  &x_env=&x_app_version=0.2.2&x_app_type=stable
//	  &code_challenge=<PKCE>&code_challenge_method=S256
//	  &hide_saas_login=true&channel_name=common
//
// ── 三个曾踩过的坑（都验证过，勿再改回去）────────────────────────────
//
//  1. auth_from 只有 "solo" / "trae" 两个合法值（官方源码：
//     `Pl(this.a) === an.SOLO_Lite ? "solo" : "trae"`）。**没有 "ide"**。
//
//  2. auth_from=solo 时**必须**带 hide_saas_login=true
//     （官方源码：`E === "solo" && (A += "&hide_saas_login=true")`）。
//
//  3. auth_callback_url 的**路径必须是根路径 /authorize**，不能带平台前缀。
//     实测对照（同浏览器同时刻，唯一变量是路径）：
//     `.../authorize` 正常跳登录页；`.../trae/authorize` 报「登录失败/网络错误」。
//
// 三者任一错，症状都是同一个「登录失败 / 网络错误，请刷新页面重试」——
// 上游 HTTP 仍返回 200、页面没有任何失败请求，是前端 JS 渲染的错误页，
// 极难定位。故这里每条都写明依据，并有回归测试钉住。
//
// challenge 为空时不带 PKCE 参数（保持向后兼容；正常路径都会传）。
func BuildLoginURL(machineID, deviceID, callbackURL, challenge string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", upstream.PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", upstream.ClientID)
	v.Set("redirect", "0")
	// auth_from=solo 时必须带：官方源码里这一条是条件追加的，
	// 漏掉会被上游前端判为非法请求。
	v.Set("hide_saas_login", "true")
	if challenge != "" {
		v.Set("code_challenge", challenge)
		v.Set("code_challenge_method", "S256")
	}
	return upstream.ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(machineTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(upstream.IdeVersion) +
		"&x_app_type=stable"
}

// machineTraceID 由 machineID+deviceID 派生稳定的 login_trace_id（hex16）。
func machineTraceID(machineID, deviceID string) string {
	s := machineID + deviceID
	if len(s) >= 16 {
		return s[len(s)-16:]
	}
	return strings.Repeat("0", 16-len(s)) + s
}

// authorizeCallback GET /authorize：Trae 登录回调落点。
// 浏览器跳转不带 Bearer，故此路径不鉴权（回调节内完成换 token 与落盘）。
func (h *Handler) authorizeCallback(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.String()
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "http://127.0.0.1" + rawURL
	}
	res, err := h.importCredentialFromCallback(rawURL, r.URL.Query())
	if err != nil {
		renderAuthPage(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	renderAuthPage(w, http.StatusOK, "登录成功",
		"账号 "+res["uid"].(string)+" 已加入 Trae 账号池，可关闭此窗口返回面板。")
}

// importCredentialFromCallback 解析回调 → 落盘 → 进池。
func (h *Handler) importCredentialFromCallback(rawURL string, q url.Values) (map[string]any, error) {
	info, err := ParseCallback(rawURL)
	if err != nil {
		return nil, err
	}
	machineID := q.Get("machine_id")
	deviceID := q.Get("device_id")
	// 从 pending 会话补齐设备指纹与 PKCE verifier。
	//
	// 为什么需要：新协议的回调只带 authCodeInfo，**不带** machine_id/device_id
	// （那是我们生成 URL 时自己塞进去的，上游不回传），而授权码交换需要
	// code_verifier（必须与生成 URL 时的 code_challenge 配对）。
	// 两者都只能从 pending 会话里取 —— 这正是 pending 存在的意义。
	trace := q.Get("loginTraceID")
	if trace == "" {
		trace = info.LoginTraceID
	}
	codeVerifier := ""
	if trace != "" {
		if pl, ok := h.findPendingByTrace(trace); ok {
			if machineID == "" {
				machineID = pl.machineID
			}
			if deviceID == "" {
				deviceID = pl.deviceID
			}
			codeVerifier = pl.codeVerifier
		}
	}

	a := &auth.Auth{
		AccessToken: info.AccessToken, RefreshToken: info.RefreshToken,
		UID: info.UID, Nickname: info.Nickname, EnterpriseID: info.EnterpriseID,
		Domain: "trae.cn",
		// ApiHost 留空 = 用客户端配置的 OAuthHost（默认 api.trae.com.cn）。
		//
		// 不在这里硬编码 upstream.OAuthHost 常量：那会绕过 Operator 注入的 host，
		// 让「测试打假上游」和「自建反代」都失效。客户端已经能配 OAuthHost，
		// 凭证级 ApiHost 只在用户手写凭证时才有意义（那时才由 Parse 带入）。
		ApiHost:   "",
		MachineID: machineID,
		DeviceID:  deviceID,
		ExpiresAt: info.ExpiresAt,
	}

	// 新协议：回调给的是授权码，必须先换成 refreshToken 才能入库。
	//
	// 官方流程（从客户端源码 + 运行时日志提取）：回调带 authCodeInfo，
	// 客户端用 AuthCode + CodeVerifier 调 ExchangeToken 换 token。
	// 旧协议直给 refreshToken，则不需要这一步（下面 finishImport 会走
	// refreshToken 轮换路径）。
	if info.RefreshToken == "" && info.AuthCode != "" {
		if codeVerifier == "" {
			// 没有 verifier 就换不了（上游要校验 challenge 配对）。
			// 常见原因：pending 会话已过期（>15min）或服务重启过。
			return nil, errors.New("授权码交换失败：找不到本次登录的 code_verifier" +
				"（登录会话可能已过期或服务重启过，请重新发起登录）")
		}
		if err := h.cfg.Upstream.ExchangeByAuthCode(a, info.AuthCode, codeVerifier); err != nil {
			return nil, fmt.Errorf("授权码交换失败: %w", err)
		}
	}
	return h.finishImport(a, q)
}

// finishImport 补信息 → 落盘 → 热加载进池。
func (h *Handler) finishImport(a *auth.Auth, q url.Values) (map[string]any, error) {
	if a.RefreshToken != "" {
		if err := h.cfg.Upstream.RefreshToken(a); err != nil {
			h.markPendingByMachine(q, pendingFailed, "", "", err.Error())
			return nil, fmt.Errorf("ExchangeToken 失败: %w", err)
		}
	}
	if ui, err := h.cfg.Upstream.GetUserInfo(a); err == nil && ui.UID != "" {
		a.UID = ui.UID
		if ui.Nickname != "" {
			a.Nickname = ui.Nickname
		}
		if ui.EnterpriseID != "" {
			a.EnterpriseID = ui.EnterpriseID
		}
	}
	if a.UID == "" {
		err := errors.New("无法确定 uid（回调与 GetUserInfo 均无）")
		h.markPendingByMachine(q, pendingFailed, "", "", err.Error())
		return nil, err
	}
	if a.AccessToken == "" {
		err := errors.New("换 token 后仍无 accessToken")
		h.markPendingByMachine(q, pendingFailed, "", "", err.Error())
		return nil, err
	}
	a.FilePath = auth.FilePathFor(h.cfg.AuthDir, a.UID)

	// 补齐签到设备指纹（did）。
	//
	// 为什么必须在这里补（2026-10-14 实测发现）：
	//   签到/额度走 api.trae.cn，请求头必须带合法的小写 x-device-id（16 位纯数字）。
	//   缺失时上游返回 9004「缺少设备字段」→ 该账号**永远签不上**。
	//   而「粘贴凭证导入」这条路径拿到的 JSON 通常**不含** did（用户从别处
	//   复制的凭证、或从官方客户端 storage.json 解出来的），于是导入后
	//   刷额度正常（那条接口不校验 did）但签到必失败——症状很隐蔽。
	//
	// 原先只有 LoadDir（进程启动时）会补 did，运行期导入不会补：
	//   用户导入后必须重启一次才能签到。这里补上，消除该隐性依赖。
	//
	// 放在 FilePath 赋值之后、SaveAtomic 之前：did 与凭证一次落盘，
	// 不多写一次文件（EnsureCheckinDeviceID 内部会 SaveAtomic，但此时
	// FilePath 已设好，写的就是同一个文件）。
	//
	// 安全性：EnsureCheckinDeviceID 对**已有合法 did** 的账号原样返回、
	// 不覆盖——所以对真机/已有账号零影响，只补缺失或非法的。
	if _, derr := a.EnsureCheckinDeviceID(); derr != nil {
		// 补 did 失败不阻断导入：账号仍可用（刷额度/对话不依赖 did），
		// 只是签到会失败。如实记日志，让运维能查到。
		logf("import %s: 补 deviceId 失败（签到会 9004）: %v", a.UID, derr)
	}

	if err := a.SaveAtomic(); err != nil {
		h.markPendingByMachine(q, pendingFailed, a.UID, a.Nickname, err.Error())
		return nil, err
	}
	h.cfg.Pool.Add(a)

	// 首次签到 + 查额度：**同步**执行，并把结果带进响应。
	//
	// ── 为什么从异步改成同步（2026-10-14 修复）────────────────────────
	//
	// 原实现走 h.bg.Go（后台异步），于是出现一个用户直接可见的缺陷：
	//
	//	导入请求返回时首签还没跑完 → 台账里还没有 credits
	//	→ 面板立刻刷新看到「积分 0」→ 用户以为新账号没积分
	//	（而老账号有值，对比之下更像数据丢失）
	//
	// 导入是**低频操作**（用户手动点一次），多等 1~2 秒换来「导入后积分
	// 立刻可见」，体验上明显更好。这里同步执行并把额度回填进响应。
	//
	// 保留 h.bg 的原有价值：同步执行已不会与测试收尾竞态（写盘在返回前完成），
	// 所以不再需要后台跟踪。但如果首签**超时**，最坏情况是导入响应变慢——
	// 用 RefreshTokenIfNeeded/CheckinStatus/EntUsage 各自的 HTTP 超时兜底，
	// 不会无限阻塞。
	var firstCredits int64
	var firstNote string
	if acct := h.cfg.Pool.AuthByUID(a.UID); acct != nil {
		if remain, note, cerr := checkinQuietImpl(h.cfg.Upstream, h.cfg.Pool, acct); cerr != nil {
			logf("login checkin %s: %v", a.UID, cerr)
			firstNote = "首签失败（可稍后手动签到）"
		} else {
			firstCredits = remain
			firstNote = note
		}
	}

	h.markPendingByMachine(q, pendingSuccess, a.UID, a.Nickname, "")
	out := map[string]any{
		"ok":       true,
		"uid":      a.UID,
		"nickname": a.Nickname,
	}
	// 带上首签结果：面板可直接显示额度，不必再等后台刷新。
	if firstCredits > 0 {
		out["credits"] = firstCredits
	}
	if firstNote != "" {
		out["checkin"] = firstNote
	}
	return out, nil
}

// markPendingByMachine 用 trace/machine 反查 pending 并标记结果。
func (h *Handler) markPendingByMachine(q url.Values, st pendingState, uid, nick, errMsg string) {
	trace := q.Get("loginTraceID")
	machineID := q.Get("machine_id")
	if trace == "" && machineID == "" {
		return
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, pl := range h.logins {
		if machineID != "" && pl.machineID == machineID {
			pl.state, pl.uid, pl.nickname, pl.errMsg = st, uid, nick, errMsg
			return
		}
		if trace != "" && machineTraceID(pl.machineID, pl.deviceID) == trace {
			pl.state, pl.uid, pl.nickname, pl.errMsg = st, uid, nick, errMsg
			return
		}
	}
}

func (h *Handler) findPendingByTrace(trace string) (*pendingLogin, bool) {
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, pl := range h.logins {
		if machineTraceID(pl.machineID, pl.deviceID) == trace {
			return pl, true
		}
	}
	return nil, false
}

// CallbackInfo 回调解析结果。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64

	// AuthCode 上游新协议（2026-10 起）下发的 OAuth2 授权码。
	//
	// 回调形态从「直接给 refreshToken」变成了「给授权码」：
	//
	//	authCodeInfo={"AuthCode":"...","ExpireAt":...,"ExpireDuration":600000}
	//
	// 拿到它之后必须自己调 ExchangeToken（配合 PKCE 的 code_verifier）
	// 才能换到真正的 refreshToken —— 见 upstream.ExchangeByAuthCode。
	//
	// 两种形态并存：旧账号/旧链接仍可能直给 refreshToken，
	// 故本结构同时保留两个字段，由调用方决定走哪条路。
	AuthCode string
	// LoginTraceID 回调带回的登录追踪 id（用于找回 pending 会话里的
	// machineID/deviceID 与 code_verifier）。
	LoginTraceID string
}

// ParseCallback 解析 Trae 回调链接。
//
// 兼容两种形态（上游 2026-10 改过协议，两者并存）：
//
//	旧：?refreshToken=..&userInfo={..}&userJwt={..}
//	新：?authCodeInfo={"AuthCode":..}&loginTraceID=..&userInfo={..}&host=..
//
// 新版只给授权码，换 token 的动作由调用方完成（见 finishImport 与
// upstream.ExchangeByAuthCode）。这里只负责**如实解析**，不在这里做交换——
// 因为交换需要 pending 会话里的 code_verifier，那是 handler 的上下文。
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("empty callback url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
		LoginTraceID: q.Get("loginTraceID"),
	}

	ui := parseJSONParam(q.Get("userInfo"))
	info.UID = getStr(ui, "UserID")
	info.Nickname = getStr(ui, "ScreenName")
	info.EnterpriseID = getStr(ui, "TenantID")

	// 新协议：授权码。
	//
	// authCodeInfo 形如 {"AuthCode":"...","ExpireAt":...,"ExpireDuration":...}，
	// 且它自身可能是**双层编码**（同 userInfo），故复用 parseJSONParam。
	ac := parseJSONParam(q.Get("authCodeInfo"))
	info.AuthCode = getStr(ac, "AuthCode")

	uj := parseJSONParam(q.Get("userJwt"))
	jwtToken := getStr(uj, "Token")
	if info.RefreshToken == "" {
		info.RefreshToken = getStr(uj, "RefreshToken")
	}
	// 有授权码就不再要求 refreshToken —— 那正是新协议的正常形态，
	// 交换动作由调用方完成（旧逻辑在这里报错会把新协议登录全部拒掉）。
	if info.RefreshToken == "" && info.AuthCode == "" {
		info.AccessToken = jwtToken
		if jwtToken == "" {
			return nil, errors.New("callback missing refreshToken / authCodeInfo.AuthCode / userJwt.Token")
		}
		if exp := getInt64(uj, "TokenExpireAt"); exp > 0 {
			info.ExpiresAt = normalizeExpire(exp)
		}
	}
	return info, nil
}

// parseJSONParam 容错解析回调里 URL 编码的 JSON 参数。
//
// Trae 回调的 userInfo / userJwt 可能是**双层编码**：parse_qs 已解一层，
// 取到的值有时仍是 percent-encoded 的 JSON 串；有时又只解一层就已是明文。
// 所以这里反复「能解就解」直到解出 JSON 对象为止（最多 maxUnwrap 层，防止
// 病态输入把 CPU 打满）。
func parseJSONParam(raw string) map[string]any {
	const maxUnwrap = 4
	cur := raw
	for i := 0; i < maxUnwrap && cur != ""; i++ {
		var obj map[string]any
		if json.Unmarshal([]byte(cur), &obj) == nil && obj != nil {
			return obj
		}
		// 解不出 JSON 就试着再解一层 URL 编码
		uq, err := url.QueryUnescape(cur)
		if err != nil || uq == cur {
			break
		}
		cur = uq
	}
	return nil
}

func getStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

func getInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// normalizeExpire 毫秒 → 秒。
func normalizeExpire(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// importCredential 支持两种输入：JSON 凭证串 或 回调链接。
func (h *Handler) importCredential(payload string) (map[string]any, error) {
	if payload == "" {
		return nil, errors.New("payload 为空")
	}
	if strings.HasPrefix(payload, "{") {
		a, err := auth.Parse([]byte(payload))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(a.RefreshToken) == "" {
			return nil, errors.New("凭证缺少 refreshToken（无法续期）")
		}
		// ApiHost 缺省就留空 → refreshLocked 回落到客户端配置的 OAuthHost。
		// 只有用户手写凭证并显式填了 apiHost 时才沿用它（多区域部署场景）。
		if a.Domain == "" {
			a.Domain = "trae.cn"
		}
		return h.finishImport(a, url.Values{})
	}
	u, err := url.Parse(payload)
	if err != nil {
		// 可能是纯 token 串：当作 refreshToken 兜底处理
		if len(payload) > 20 && !strings.ContainsAny(payload, " \t\n") {
			a := &auth.Auth{RefreshToken: payload, Domain: "trae.cn"}
			return h.finishImport(a, url.Values{})
		}
		return nil, fmt.Errorf("无法识别的 payload：%w", err)
	}
	return h.importCredentialFromCallback(payload, u.Query())
}

// checkinQuiet 已由 checkinQuietImpl 实现（helpers.go），此处仅为兼容早前调用名。
