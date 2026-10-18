package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"

	"allplatforms/internal/platform/trae/upstream"
)

// mustJSONString 把任意串序列化为 JSON 字符串字面量（测试断言用）。
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// ParseCallback 必须同时支持 refreshToken 与 userJwt 两种回调形态。
func TestParseCallbackRefreshToken(t *testing.T) {
	raw := "http://127.0.0.1:18081/authorize?refreshToken=RT123" +
		"&userInfo=" + url.QueryEscape(`{"UserID":"u1","ScreenName":"昵称"}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if info.RefreshToken != "RT123" {
		t.Errorf("refreshToken 不对: %q", info.RefreshToken)
	}
	if info.UID != "u1" || info.Nickname != "昵称" {
		t.Errorf("userInfo 解析不对: uid=%q nick=%q", info.UID, info.Nickname)
	}
}

// 回调缺 refreshToken 时回退 userJwt.Token（登录脚本的既有兜底分支）。
func TestParseCallbackUserJwtFallback(t *testing.T) {
	raw := "http://127.0.0.1:18081/authorize" +
		"?userJwt=" + url.QueryEscape(`{"Token":"AT999","TokenExpireAt":1786847930141}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if info.AccessToken != "AT999" {
		t.Errorf("accessToken 兜底不对: %q", info.AccessToken)
	}
	if info.ExpiresAt != 1786847930 {
		t.Errorf("过期时间未按毫秒归一: %d", info.ExpiresAt)
	}
}

func TestParseCallbackMissingBoth(t *testing.T) {
	if _, err := ParseCallback("http://127.0.0.1:18081/authorize"); err == nil {
		t.Error("既无 refreshToken 也无 userJwt.Token 时应报错")
	}
	if _, err := ParseCallback(""); err == nil {
		t.Error("空串应报错")
	}
}

// userInfo 里的企业号字段是 TenantID（不是 EnterpriseID）。
func TestParseCallbackEnterpriseField(t *testing.T) {
	raw := "http://127.0.0.1:18081/authorize?refreshToken=RT" +
		"&userInfo=" + url.QueryEscape(`{"UserID":"u2","TenantID":"ent-9"}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if info.EnterpriseID != "ent-9" {
		t.Errorf("TenantID 未映射到 EnterpriseID: %q", info.EnterpriseID)
	}
}

// login_trace_id 由 machine+device 派生，必须稳定且长度固定（回调靠它反查 pending）。
func TestMachineTraceID(t *testing.T) {
	m := "0123456789abcdef0123456789abcdef"
	d := "1234567890123456"
	got := machineTraceID(m, d)
	if len(got) != 16 {
		t.Errorf("trace id 长度应为 16，实为 %d (%q)", len(got), got)
	}
	// 稳定性：同一输入必须同一输出（回调反查依赖这点）
	if got != machineTraceID(m, d) {
		t.Error("trace id 不稳定")
	}
	// 短输入左侧补零
	short := machineTraceID("ab", "cd")
	if len(short) != 16 || short != "000000000000abcd" {
		t.Errorf("短输入补零不对: %q", short)
	}
}

func TestBuildLoginURL(t *testing.T) {
	u := BuildLoginURL("M1", "D1", "http://127.0.0.1:18081/authorize", "CHAL")
	// 必须指向官方登录页，且带上 callback 与 client_id
	for _, want := range []string{"/authorization?", "client_id=", "auth_callback_url=", "machine_id=M1", "device_id=D1"} {
		if !contains(u, want) {
			t.Errorf("登录 URL 缺少 %q: %s", want, u)
		}
	}
}

// TestBuildLoginURLUsesAuthFromSolo 钉住 auth_from 必须是官方合法值。
//
// 回归背景（2026-10-14，两次踩坑）：
//
//  1. 原值 "solo" 曾被误判为"上游已不支持"而改成 "ide" —— 那个判断是**错的**，
//     依据是"auth_from=ide 时登录页能打开"。后来从官方客户端日志拿到权威 URL，
//     证明官方一直在用 "solo"，而 "ide" 根本不在官方取值集合里
//     （官方源码：`Pl(this.a) === an.SOLO_Lite ? "solo" : "trae"`）。
//
//  2. 真正的失败原因是缺 `hide_saas_login=true`（官方在 solo 分支条件追加）
//     以及缺 PKCE —— 不是 auth_from 的值。
//
// 之所以要用测试钉住：这个参数的失败形态**极其隐蔽** ——
// 上游 HTTP 仍返回 200 + 正常 HTML，页面**没有任何失败的网络请求**，
// 是前端 JS 检测到参数非法后主动渲染「登录失败/网络错误」。
// 靠"状态码 200"或"抓包无错"都判断不出来，只能靠测试锁死取值。
func TestBuildLoginURLUsesAuthFromSolo(t *testing.T) {
	u := BuildLoginURL("M1", "D1", "http://127.0.0.1:18081/authorize", "CHAL")
	if !contains(u, "auth_from=solo") {
		t.Errorf("auth_from 必须为 solo（官方合法值只有 solo/trae）: %s", u)
	}
	if contains(u, "auth_from=ide") {
		t.Errorf("不该出现 auth_from=ide（官方无此取值）: %s", u)
	}
}

// TestBuildLoginURLHasHideSaasLogin 钉住 solo 模式下必须带 hide_saas_login。
//
// 官方源码：`E === "solo" && (A += "&hide_saas_login=true")`。
// 漏掉它会被上游前端判为非法请求（症状同上的"登录失败/网络错误"）。
func TestBuildLoginURLHasHideSaasLogin(t *testing.T) {
	u := BuildLoginURL("M1", "D1", "http://127.0.0.1:18081/authorize", "CHAL")
	if !contains(u, "hide_saas_login=true") {
		t.Errorf("auth_from=solo 时必须带 hide_saas_login=true: %s", u)
	}
}

// TestBuildLoginURLUsesCurrentPluginVersion 钉住 plugin_version 用真实值。
//
// 旧值 2.3.62834 是硬编码的过期版本号；官方当前为 2.3.91089
//（来源：客户端 product.json 的 tronBuildVersion，与官方日志里的 URL 一致）。
func TestBuildLoginURLUsesCurrentPluginVersion(t *testing.T) {
	u := BuildLoginURL("M1", "D1", "http://127.0.0.1:18081/authorize", "CHAL")
	if !contains(u, "plugin_version="+upstream.PluginVersion) {
		t.Errorf("plugin_version 应为 %s: %s", upstream.PluginVersion, u)
	}
}

// TestNewPKCEPairMatchesOfficialAlgorithm 钉住 PKCE 生成与官方算法等价。
//
// 官方实现（客户端 main.js 的 IPe 函数）：
//
//	verifier  = crypto.randomBytes(48).toString("base64url")
//	challenge = crypto.createHash("sha256").update(verifier).digest("base64url")
//
// 即：48 字节随机数 → base64url（无填充）；challenge = SHA256(verifier) 的 base64url。
// 长度不符或编码不符都会让上游校验失败（challenge != SHA256(verifier)）。
func TestNewPKCEPairMatchesOfficialAlgorithm(t *testing.T) {
	v, c, err := NewPKCEPair()
	if err != nil {
		t.Fatalf("生成 PKCE 失败: %v", err)
	}
	// 48 字节 → base64url 无填充 = 64 个字符
	if len(v) != 64 {
		t.Errorf("verifier 应为 64 字符（48 字节 base64url），实为 %d: %q", len(v), v)
	}
	// SHA256 → 32 字节 → base64url 无填充 = 43 个字符
	if len(c) != 43 {
		t.Errorf("challenge 应为 43 字符（32 字节 base64url），实为 %d: %q", len(c), c)
	}
	// 必须是 base64url 字符集（无 + / 与填充 =）
	for _, s := range []string{v, c} {
		for i := 0; i < len(s); i++ {
			ch := s[i]
			okCh := (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') ||
				(ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
			if !okCh {
				t.Fatalf("含非 base64url 字符 %q: %q", ch, s)
			}
		}
	}
	// challenge 必须等于 SHA256(verifier) 的 base64url
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if c != want {
		t.Errorf("challenge 必须是 SHA256(verifier) 的 base64url：\n  want %s\n  got  %s", want, c)
	}
}

// TestNewPKCEPairIsRandom 两次生成必须不同（否则等于没有 PKCE 保护）。
func TestNewPKCEPairIsRandom(t *testing.T) {
	v1, c1, err := NewPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	v2, c2, err := NewPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 || c1 == c2 {
		t.Error("两次生成的 PKCE 参数相同 —— 随机数失效")
	}
}

// TestBuildLoginURLCarriesPKCE 登录 URL 必须带 PKCE 参数。
func TestBuildLoginURLCarriesPKCE(t *testing.T) {
	u := BuildLoginURL("M1", "D1", "http://127.0.0.1:18081/authorize", "CHAL")
	for _, want := range []string{"code_challenge=CHAL", "code_challenge_method=S256"} {
		if !contains(u, want) {
			t.Errorf("登录 URL 缺少 %q: %s", want, u)
		}
	}
	// 空 challenge 时不带（向后兼容路径）
	u2 := BuildLoginURL("M1", "D1", "http://127.0.0.1:18081/authorize", "")
	if contains(u2, "code_challenge=") {
		t.Errorf("空 challenge 时不该带 code_challenge: %s", u2)
	}
}

// TestParseCallbackAcceptsAuthCodeInfo 钉住新协议回调（授权码形态）能被解析。
//
// 回归背景（2026-10 上游协议变更）：回调从「直给 refreshToken」改成
// 「给授权码」，参数形如：
//
//	authCodeInfo={"AuthCode":"...","ExpireAt":...,"ExpireDuration":600000}
//
// 旧解析只认 refreshToken/userJwt，遇到新格式直接报
// 「callback missing refreshToken and userJwt.Token」→ 登录闭环全断。
func TestParseCallbackAcceptsAuthCodeInfo(t *testing.T) {
	codeInfo := `{"AuthCode":"AC-TEST-123","ExpireAt":1791948849249,"ExpireDuration":600000}`
	userInfo := `{"UserID":"u-1","ScreenName":"测试","TenantID":"t-1"}`
	raw := "http://127.0.0.1:17863/authorize?isRedirect=true&scope=solo" +
		"&authCodeInfo=" + url.QueryEscape(codeInfo) +
		"&loginTraceID=trace-1" +
		"&host=" + url.QueryEscape("https://api.trae.com.cn") +
		"&userRegion=cn" +
		"&userInfo=" + url.QueryEscape(userInfo)

	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("新协议回调应能解析，实报错: %v", err)
	}
	if info.AuthCode != "AC-TEST-123" {
		t.Errorf("AuthCode 解析错误: %q", info.AuthCode)
	}
	if info.LoginTraceID != "trace-1" {
		t.Errorf("LoginTraceID 解析错误: %q", info.LoginTraceID)
	}
	if info.UID != "u-1" {
		t.Errorf("UID 解析错误: %q", info.UID)
	}
	if info.Nickname != "测试" {
		t.Errorf("Nickname 解析错误: %q", info.Nickname)
	}
}

// TestParseCallbackStillAcceptsRefreshToken 旧协议回调仍要能用（向后兼容）。
func TestParseCallbackStillAcceptsRefreshToken(t *testing.T) {
	raw := "http://127.0.0.1:18081/authorize?refreshToken=RT-1&userInfo=" +
		url.QueryEscape(`{"UserID":"u-2","ScreenName":"旧账号"}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatalf("旧协议回调应仍能解析: %v", err)
	}
	if info.RefreshToken != "RT-1" {
		t.Errorf("RefreshToken 解析错误: %q", info.RefreshToken)
	}
	if info.AuthCode != "" {
		t.Errorf("旧协议不该有 AuthCode: %q", info.AuthCode)
	}
}

// TestParseCallbackRejectsEmpty 三种凭证来源都缺时应报错（不能静默放行）。
func TestParseCallbackRejectsEmpty(t *testing.T) {
	if _, err := ParseCallback("http://127.0.0.1:18081/authorize?foo=bar"); err == nil {
		t.Error("无任何凭证来源时应报错")
	}
}

// uid 校验要挡住路径穿越：uid 会拼进 /api/accounts/{uid}/... 的下游路径。
func TestIsValidUID(t *testing.T) {
	h := &Handler{}
	ok := []string{"abc", "u-1", "u_1", "1234567890", "trae-uid-demo-0001"}
	for _, s := range ok {
		if !h.isValidUID(s) {
			t.Errorf("%q 应放行", s)
		}
	}
	bad := []string{"", "../etc", "a/b", `a\b`, "a..b", "."}
	for _, s := range bad {
		if h.isValidUID(s) {
			t.Errorf("%q 应拒绝", s)
		}
	}
}

// 回调里的 JSON 参数可能被双重编码（parse_qs 解一层后仍是 JSON 编码形态）。
func TestParseJSONParamDoubleEncoded(t *testing.T) {
	inner := `{"UserID":"u1"}`
	once := url.QueryEscape(inner)
	twice := url.QueryEscape(once)
	got := parseJSONParam(twice)
	if getStr(got, "UserID") != "u1" {
		t.Errorf("双层编码未解出: %#v", got)
	}
	if parseJSONParam("") != nil {
		t.Error("空串应返回 nil")
	}
	if parseJSONParam("not json") != nil {
		t.Error("非 JSON 应返回 nil")
	}
}

// 回调字段取值要容错：上游有 string / number / bool / null 各种形态。
func TestGetStrInt64Tolerance(t *testing.T) {
	m := map[string]any{"s": "str", "f": float64(3.5), "b": true, "n": nil}
	if getStr(m, "s") != "str" || getStr(m, "f") != "3.5" {
		t.Errorf("字符串取值不对: %q %q", getStr(m, "s"), getStr(m, "f"))
	}
	if getStr(m, "b") != "true" {
		t.Errorf("bool 应转字符串: %q", getStr(m, "b"))
	}
	if getStr(m, "n") != "" || getStr(nil, "x") != "" {
		t.Error("nil 值应返回空串")
	}
	if getInt64(m, "f") != 3 {
		t.Errorf("float 取整不对: %d", getInt64(m, "f"))
	}
	if getInt64(map[string]any{"s": "42"}, "s") != 42 {
		t.Error("字符串数字应可解析")
	}
	if getInt64(m, "n") != 0 {
		t.Error("nil 应为 0")
	}
}

// ParseCSVTokens / ToTokenCounter 是统计口径的辅助函数.
func TestToTokenCounter(t *testing.T) {
	if got := ToTokenCounter(map[string]any{"usage": map[string]any{"total_tokens": float64(42)}}); got != 42 {
		t.Errorf("total_tokens 取值不对: %d", got)
	}
	if got := ToTokenCounter(map[string]any{}); got != 0 {
		t.Errorf("无 usage 应为 0: %d", got)
	}
}

// 错误串要安全嵌进 JSON：上游 body 可能带引号/换行/反斜杠。
func TestEscapeForJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`he said "hi"`, `he said \"hi\"`},
		{"line\nbreak", `line\nbreak`},
		{`back\slash`, `back\\slash`},
		{"tab\there", `tab\there`},
	}
	for _, c := range cases {
		got := mustJSONString(c.in)
		if len(got) < 2 || got[0] != '"' {
			t.Errorf("应返回带引号的 JSON 串: %q", got)
		}
		// 解回来必须与原串一致（转义是双向安全的）
		var back string
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Errorf("转义结果不是合法 JSON: %v (%q)", err, got)
			continue
		}
		if back != c.in {
			t.Errorf("往返不一致: %q → %q", c.in, back)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
