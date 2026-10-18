package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/auth"
)

// fakeWriter 实现 http.ResponseWriter 但只落内容，用于验 Stream 的输出文本。
type fakeWriter struct{ b *strings.Builder }

func (w fakeWriter) Header() http.Header         { return http.Header{} }
func (w fakeWriter) WriteHeader(int)             {}
func (w fakeWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

var _ = json.Marshal

// ── 夹具 ────────────────────────────────────────────────────────────────

// newTestClient 起一个假上游 + 指向它的客户端（三类 host 全指向同一台）。
func newTestClient(t *testing.T, mux *http.ServeMux) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(3*time.Second, 3*time.Second, 3*time.Second)
	c.SetHosts(srv.URL, srv.URL, srv.URL)
	c.UserAgent = "" // 走生产默认 UA，让头族断言有意义
	return c, srv
}

func testAuth() *auth.Auth {
	return &auth.Auth{
		AccessToken: "AT", RefreshToken: "RT", UID: "u1",
		MachineID: "M", DeviceID: "1234567890123456",
	}
}

// jsonHandler 回固定 JSON 的辅助。
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// ── RefreshToken ─────────────────────────────────────────────────────────

func TestRefreshTokenSuccess(t *testing.T) {
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc(EpExchange, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{"Result":{"Token":"NEW","RefreshToken":"NEW_RT","TokenExpireAt":1786847930141}}`)
	})
	c, _ := newTestClient(t, mux)

	a := testAuth()
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix() // 强制触发刷新
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if a.JWT() != "NEW" || a.RefreshTokenValue() != "NEW_RT" {
		t.Errorf("token 未更新: %s / %s", a.JWT(), a.RefreshTokenValue())
	}
	if a.ExpiresAtUnix() != 1786847930 {
		t.Errorf("过期时间未按毫秒归一: %d", a.ExpiresAtUnix())
	}
	// 请求体必须带上 refreshToken
	if !strings.Contains(gotBody, `"RefreshToken":"RT"`) {
		t.Errorf("请求体不对: %s", gotBody)
	}
}

// TokenExpireAt 缺失时用 TokenExpireDuration 推算。
func TestRefreshTokenFallsBackToDuration(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpExchange, jsonHandler(`{"Result":{"Token":"NEW","TokenExpireDuration":3600}}`))
	c, _ := newTestClient(t, mux)

	a := testAuth()
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	exp := a.ExpiresAtUnix()
	if exp < time.Now().Add(50*time.Minute).Unix() || exp > time.Now().Add(70*time.Minute).Unix() {
		t.Errorf("过期时间推算不对: %d", exp)
	}
}

// 上游不给 token → 报错，且**不改写**本地凭证（保住旧 refreshToken 可重试）。
func TestRefreshTokenNoTokenKeepsOld(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpExchange, jsonHandler(`{"Result":{}}`))
	c, _ := newTestClient(t, mux)

	a := testAuth()
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("无 token 应报错")
	}
	if a.JWT() != "AT" || a.RefreshTokenValue() != "RT" {
		t.Errorf("失败时不该改写凭证: %s / %s", a.JWT(), a.RefreshTokenValue())
	}
}

func TestRefreshTokenNoRefreshTokenInCred(t *testing.T) {
	c, _ := newTestClient(t, http.NewServeMux())
	a := &auth.Auth{AccessToken: "AT", UID: "u1"}
	if err := c.RefreshToken(a); err == nil {
		t.Error("无 refreshToken 应报错")
	}
}

func TestRefreshTokenUpstreamError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpExchange, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid refresh token"}`)
	})
	c, _ := newTestClient(t, mux)

	a := testAuth()
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("上游 401 应报错")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应返回 *Error，实际 %T", err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("错误分类不对: %v", ue.Kind)
	}
}

// RefreshTokenIfNeeded：未到窗口不刷新、到了才刷新。
func TestRefreshTokenIfNeededSkips(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc(EpExchange, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"Result":{"Token":"NEW","TokenExpireAt":1786847930141}}`)
	})
	c, _ := newTestClient(t, mux)

	// 远未过期 → 不该调上游
	a := testAuth()
	a.ExpiresAt = time.Now().Add(10 * time.Hour).Unix()
	refreshed, err := c.RefreshTokenIfNeeded(a, time.Hour)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if refreshed || calls != 0 {
		t.Errorf("未到窗口不该刷新: refreshed=%v calls=%d", refreshed, calls)
	}

	// 已过期 → 刷新
	a2 := testAuth()
	a2.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	refreshed2, err := c.RefreshTokenIfNeeded(a2, time.Hour)
	if err != nil || !refreshed2 {
		t.Errorf("已过期应刷新: %v %v", refreshed2, err)
	}
	if calls != 1 {
		t.Errorf("上游应被调 1 次，实为 %d", calls)
	}
}

// ── GetUserInfo ─────────────────────────────────────────────────────────

func TestGetUserInfo(t *testing.T) {
	var gotToken string
	mux := http.NewServeMux()
	mux.HandleFunc(EpUserInfo, func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Cloudide-Token")
		_, _ = io.WriteString(w, `{"Result":{"UserID":"uid-9","ScreenName":"名字","EnterpriseID":"ent"}}`)
	})
	c, _ := newTestClient(t, mux)

	ui, err := c.GetUserInfo(testAuth())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if ui.UID != "uid-9" || ui.Nickname != "名字" || ui.EnterpriseID != "ent" {
		t.Errorf("解析不对: %+v", ui)
	}
	if gotToken != "AT" {
		t.Errorf("X-Cloudide-Token 头不对: %q", gotToken)
	}
}

func TestGetUserInfoError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpUserInfo, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	c, _ := newTestClient(t, mux)
	if _, err := c.GetUserInfo(testAuth()); err == nil {
		t.Error("上游 5xx 应报错")
	}
}

// 响应不是预期结构 → 解析错误（不是静默返回空）。
func TestGetUserInfoBadPayload(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpUserInfo, jsonHandler(`{"unexpected":true}`))
	c, _ := newTestClient(t, mux)
	ui, err := c.GetUserInfo(testAuth())
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if ui.UID != "" {
		t.Errorf("结构不符应返回空: %+v", ui)
	}
}

// ── FetchModels ─────────────────────────────────────────────────────────

func TestFetchModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, jsonHandler(`{"config_info_list":[
		{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}},
		{"config_name":"kimi-k3"}
	]}`))
	c, _ := newTestClient(t, mux)

	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("拉模型失败: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("应返回 2 个，实为 %d", len(infos))
	}
	if infos[0].ID != "glm-5.2" || infos[0].Name != "GLM 5.2" {
		t.Errorf("模型字段不对: %+v", infos[0])
	}
	if infos[1].ID != "kimi-k3" {
		t.Errorf("无 display_name 的条目也该保留: %+v", infos[1])
	}
}

// 空目录视为失败：否则会把「上游说没有」缓存一小时。
func TestFetchModelsEmptyIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, jsonHandler(`{"config_info_list":[]}`))
	c, _ := newTestClient(t, mux)
	if _, err := c.FetchModels(testAuth()); err == nil {
		t.Error("空目录应报错")
	}
}

// 无 config_name 的条目剔除。
func TestFetchModelsSkipsEntriesWithoutName(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, jsonHandler(`{"config_info_list":[{"config_name":""},{"config_name":"ok"}]}`))
	c, _ := newTestClient(t, mux)
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "ok" {
		t.Errorf("应剔除无名条目: %+v", infos)
	}
}

func TestFetchModelsUpstreamError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, _ := newTestClient(t, mux)
	if _, err := c.FetchModels(testAuth()); err == nil {
		t.Error("上游 429 应报错")
	}
}

// ── ChatStream ──────────────────────────────────────────────────────────

func TestChatStreamSuccess(t *testing.T) {
	var gotAccept, gotContentType string
	mux := http.NewServeMux()
	mux.HandleFunc(EpChat, func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"hi\"}\n\n")
	})
	c, _ := newTestClient(t, mux)

	rc, status, respBody, err := c.ChatStream(context.Background(), testAuth(), []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("传输层出错: %v", err)
	}
	if status != 200 {
		t.Errorf("状态码不对: %d", status)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("流式请求 Accept 不对: %q", gotAccept)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type 不对: %q", gotContentType)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(raw), "hi") {
		t.Errorf("流内容不对: %s", raw)
	}
	_ = respBody
}

// 非 2xx 不返回 err（让调用方拿 body 去做错误分类），只回状态码与 body。
func TestChatStreamUpstreamErrorNoErr(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpChat, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":1005,"message":"plan limit"}`)
	})
	c, _ := newTestClient(t, mux)

	rc, status, respBody, err := c.ChatStream(context.Background(), testAuth(), []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("非 2xx 不该返回传输错误: %v", err)
	}
	if rc != nil {
		t.Error("非 2xx 不该返回流")
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("状态码应透出: %d", status)
	}
	if Classify(status, string(respBody)) != ErrPlanLimit {
		t.Errorf("body 应可被分类: %s", respBody)
	}
}

// ── 签到 ────────────────────────────────────────────────────────────────

func TestCheckinStatus(t *testing.T) {
	var gotDevice string
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinStatus, func(w http.ResponseWriter, r *http.Request) {
		gotDevice = r.Header.Get("x-device-id")
		_, _ = io.WriteString(w, `{"checked_in":true,"credits":25,"enable":true}`)
	})
	c, _ := newTestClient(t, mux)

	checked, credits, enable, err := c.CheckinStatus(testAuth())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !checked || credits != 25 || !enable {
		t.Errorf("解析不对: %v %v %v", checked, credits, enable)
	}
	// 对齐 PC 客户端：小写 x-device-id = 纯 deviceId（不带 uid 后缀）。
	if gotDevice != "1234567890123456" {
		t.Errorf("x-device-id 头不对: %q", gotDevice)
	}
}

func TestCheckinStatusError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinStatus, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `boom`)
	})
	c, _ := newTestClient(t, mux)
	if _, _, _, err := c.CheckinStatus(testAuth()); err == nil {
		t.Error("上游 5xx 应报错")
	}
}

func TestCheckinClaimSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, jsonHandler(`{"code":0,"message":"ok"}`))
	c, _ := newTestClient(t, mux)
	if _, err := c.CheckinClaim(testAuth()); err != nil {
		t.Errorf("签到应成功: %v", err)
	}
}

// code≠0 → 报错（带 code 与 message，便于面板展示）。
func TestCheckinClaimBusinessError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, jsonHandler(`{"code":10002,"message":"已签到"}`))
	c, _ := newTestClient(t, mux)
	code, err := c.CheckinClaim(testAuth())
	if err == nil {
		t.Fatal("code≠0 应报错")
	}
	if code != 10002 {
		t.Errorf("应透传业务 code=10002，实为 %d", code)
	}
	if !strings.Contains(err.Error(), "10002") || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("错误信息应含 code 与 message: %v", err)
	}
}

func TestCheckinClaimHTTPError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `denied`)
	})
	c, _ := newTestClient(t, mux)
	if _, err := c.CheckinClaim(testAuth()); err == nil {
		t.Error("HTTP 403 应报错")
	}
}

// ── EntUsage ────────────────────────────────────────────────────────────

func TestEntUsageAggregation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpEntUsage, jsonHandler(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":300}},
		{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}
	]}`))
	c, _ := newTestClient(t, mux)

	remain, limit, used, packs, err := c.EntUsage(testAuth())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if limit != 1500 || used != 400 || remain != 1100 || packs != 2 {
		t.Errorf("聚合不对: remain=%d limit=%d used=%d packs=%d", remain, limit, used, packs)
	}
}

// 已用超过总量（上游偶发）→ 剩余不能为负。
func TestEntUsageNegativeRemain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpEntUsage, jsonHandler(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":150}}
	]}`))
	c, _ := newTestClient(t, mux)
	remain, _, _, _, err := c.EntUsage(testAuth())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if remain < 0 {
		t.Errorf("剩余不应为负: %d", remain)
	}
}

// limit≤0 的包（已过期/已耗尽的壳）要被跳过，不参与聚合。
func TestEntUsageSkipsZeroLimitPacks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpEntUsage, jsonHandler(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":0}},"usage":{"credits_amount":0}},
		{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":10}}
	]}`))
	c, _ := newTestClient(t, mux)
	remain, limit, _, packs, err := c.EntUsage(testAuth())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if packs != 1 || limit != 100 || remain != 90 {
		t.Errorf("零额度包应跳过: packs=%d limit=%d remain=%d", packs, limit, remain)
	}
}

func TestEntUsageError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpEntUsage, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `denied`)
	})
	c, _ := newTestClient(t, mux)
	if _, _, _, _, err := c.EntUsage(testAuth()); err == nil {
		t.Error("上游 401 应报错")
	}
}

// ── 头族 ────────────────────────────────────────────────────────────────

// SOLO 头族是上游归因与风控的依据，缺一个都可能被判异常。
func TestSOLOHeadersComplete(t *testing.T) {
	var h http.Header
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		_, _ = io.WriteString(w, `{"config_info_list":[{"config_name":"x"}]}`)
	})
	c, _ := newTestClient(t, mux)
	_, _ = c.FetchModels(testAuth())

	required := map[string]string{
		"Authorization":        "Cloud-IDE-JWT AT",
		"X-Cloudide-Token":     "AT",
		"X-Ide-Token":          "AT",
		"X-Uid":                "u1",
		"X-App-Id":             AppID,
		"X-Ide-Version":        IdeVersion,
		"X-Device-Type":        "macos",
		"X-Device-Brand":       DeviceBrand,
		"Request-Traffic-Type": "prod",
		"X-Machine-Id":         "M",
		"X-Device-Id":          "1234567890123456",
	}
	for k, want := range required {
		if got := h.Get(k); got != want {
			t.Errorf("头 %s = %q，期望 %q", k, got, want)
		}
	}
	if ua := h.Get("User-Agent"); ua != "Trae/"+IdeVersion {
		t.Errorf("UA 不对: %q", ua)
	}
	// 非流式请求的 Accept 要是 json
	if got := h.Get("Accept"); got != "application/json" {
		t.Errorf("非流式 Accept 不对: %q", got)
	}
}

func TestSOLOHeadersStreamAcceptAndCustomUA(t *testing.T) {
	var h http.Header
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		_, _ = io.WriteString(w, `{"config_info_list":[{"config_name":"x"}]}`)
	})
	c, _ := newTestClient(t, mux)
	c.UserAgent = "MyAgent/1.0"
	_, _ = c.FetchModels(testAuth())
	if got := h.Get("User-Agent"); got != "MyAgent/1.0" {
		t.Errorf("自定义 UA 不生效: %q", got)
	}
}

// 凭证缺 uid 时不该带 X-Uid（上游会拒绝）。
func TestSOLOHeadersOmitUIDWhenEmpty(t *testing.T) {
	var h http.Header
	mux := http.NewServeMux()
	mux.HandleFunc(EpModels, func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		_, _ = io.WriteString(w, `{"config_info_list":[{"config_name":"x"}]}`)
	})
	c, _ := newTestClient(t, mux)
	_, _ = c.FetchModels(&auth.Auth{AccessToken: "AT"})
	if h.Get("X-Uid") != "" {
		t.Errorf("空 uid 不该带 X-Uid: %q", h.Get("X-Uid"))
	}
}

// ── Client 构造 ─────────────────────────────────────────────────────────

func TestSetHostsIgnoresEmpty(t *testing.T) {
	c := New(time.Second, time.Second, time.Second)
	agent, ug, oauth := c.AgentHost, c.UgHost, c.OAuthHost
	c.SetHosts("https://custom-agent", "", "")
	if c.AgentHost != "https://custom-agent" {
		t.Errorf("AgentHost 未覆盖: %q", c.AgentHost)
	}
	if c.UgHost != ug || c.OAuthHost != oauth {
		t.Errorf("空值不该覆盖: ug=%q oauth=%q", c.UgHost, c.OAuthHost)
	}
	_ = agent
}

func TestNewAppliesTimeoutDefaults(t *testing.T) {
	// 非正值全部回落默认，避免 0 超时导致请求无限挂起
	c := New(0, 0, 0)
	if c.HTTP.Timeout <= 0 {
		t.Errorf("短请求 client 缺总超时: %v", c.HTTP.Timeout)
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("流式 client 不该有总超时（会截断长流）: %v", c.StreamHTTP.Timeout)
	}
}

func TestErrorTypeFormat(t *testing.T) {
	e := &Error{Kind: ErrPlanLimit, Status: 400, Msg: "boom"}
	msg := e.Error()
	if !strings.Contains(msg, "plan_limit") || !strings.Contains(msg, "400") || !strings.Contains(msg, "boom") {
		t.Errorf("错误串不对: %s", msg)
	}
}

func TestErrKindString(t *testing.T) {
	want := map[ErrKind]string{
		ErrNone: "none", ErrPlanLimit: "plan_limit", ErrSoftRate: "soft_rate",
		ErrSessionDead: "session_dead", ErrNotFound: "not_found",
		ErrServer: "server", ErrClient: "client",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("ErrKind(%d) = %q，期望 %q", int(k), k.String(), s)
		}
	}
}

// ── Stream 输出 ─────────────────────────────────────────────────────────

// Stream 必须写出 OpenAI chunk 形态并补 [DONE]。
func TestStreamWritesOpenAIChunks(t *testing.T) {
	var buf strings.Builder
	sse := "event:metadata\ndata:{\"session_id\":\"s\"}\n\n" +
		"event:output\ndata:{\"response\":\"Hel\"}\n\n" +
		"event:output\ndata:{\"response\":\"lo\",\"reasoning_content\":\"think\"}\n\n" +
		"event:token_usage\ndata:{\"total_tokens\":9}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	if err := Stream(fakeWriter{&buf}, strings.NewReader(sse), "glm-5.2", nil); err != nil {
		t.Fatalf("流转换失败: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("未产出 chunk: %s", out)
	}
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Errorf("内容增量不对: %s", out)
	}
	if !strings.Contains(out, `"reasoning_content":"think"`) {
		t.Errorf("思考链增量不对: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason 不对: %s", out)
	}
	if !strings.Contains(out, `"usage":{"total_tokens":9}`) && !strings.Contains(out, `"total_tokens":9`) {
		t.Errorf("usage 未随最后一个 chunk 下发: %s", out)
	}
	if strings.Count(out, "[DONE]") != 1 {
		t.Errorf("[DONE] 应恰好一次: %d", strings.Count(out, "[DONE]"))
	}
}

// 上游中断（无 done）仍补 [DONE]，客户端不至于挂着等。
func TestStreamAlwaysWritesDone(t *testing.T) {
	var buf strings.Builder
	if err := Stream(fakeWriter{&buf}, strings.NewReader("event:output\ndata:{\"response\":\"x\"}\n\n"), "m", nil); err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !strings.Contains(buf.String(), "[DONE]") {
		t.Errorf("缺 [DONE]: %s", buf.String())
	}
}

// 流内 error 要回调 onErr（供池冷却）并写出 error 帧。
func TestStreamReportsErrorEvent(t *testing.T) {
	var buf strings.Builder
	var got *SOLOStreamError
	sse := "event:error\ndata:{\"code\":1005,\"message\":\"plan\"}\n\n"
	err := Stream(fakeWriter{&buf}, strings.NewReader(sse), "m", func(se *SOLOStreamError) { got = se })
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if got == nil || got.Code != 1005 {
		t.Errorf("onErr 未被调用或码不对: %+v", got)
	}
	if !strings.Contains(buf.String(), "event: error") {
		t.Errorf("缺 error 帧: %s", buf.String())
	}
}

// 工具调用增量：上游 function_call → OpenAI function。
func TestStreamToolCallsConverted(t *testing.T) {
	var buf strings.Builder
	sse := "event:output\ndata:{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function_call\":{\"name\":\"ls\",\"arguments\":\"{\\\"a\\\":1}\"}}]}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"tool_calls\"}\n\n"
	if err := Stream(fakeWriter{&buf}, strings.NewReader(sse), "m", nil); err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"tool_calls"`) {
		t.Errorf("工具调用未透出: %s", out)
	}
	if !strings.Contains(out, `"name":"ls"`) {
		t.Errorf("函数名丢失: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason 不对: %s", out)
	}
}

// ── 解析细节 ────────────────────────────────────────────────────────────

// data 行可能跨多行拼接，事件边界才解析。
func TestParseSOLOLineMultiLineData(t *testing.T) {
	st := &sseState{}
	st.event = "output"
	st.data.WriteString(`{"response":`)
	// 第一行还没到事件边界，不该产出事件
	if ev := scanLine(st, `data:{"response":`); ev != nil {
		t.Errorf("未到边界不该产出事件: %+v", ev)
	}
	st = &sseState{}
	st.event = "output"
	st.data.WriteString(`{"response":"分两行"}`)
	if ev := scanLine(st, ""); ev == nil || ev.Response != "分两行" {
		t.Errorf("边界处应解析出事件: %+v", ev)
	}
}

// 注释行与未知前缀要忽略，不能破坏解析。
func TestScanLineIgnoresComments(t *testing.T) {
	st := &sseState{}
	if ev := scanLine(st, ": keep-alive"); ev != nil {
		t.Errorf("注释行不该产出事件: %+v", ev)
	}
	if ev := scanLine(st, "id: 1"); ev != nil {
		t.Errorf("id 行不该产出事件: %+v", ev)
	}
	if ev := scanLine(st, "unknown: x"); ev != nil {
		t.Errorf("未知行不该产出事件: %+v", ev)
	}
}

// 非法 JSON 的 data 行要跳过而不是让整条流失败。
func TestScanLineSkipsBadJSON(t *testing.T) {
	st := &sseState{}
	st.event = "output"
	st.data.WriteString(`{broken`)
	if ev := scanLine(st, ""); ev != nil {
		t.Errorf("非法 JSON 应跳过而非报错: %+v", ev)
	}
}

func TestNormalizeExpire(t *testing.T) {
	if got := normalizeExpiresAt(1786847930141); got != 1786847930 {
		t.Errorf("毫秒转秒不对: %d", got)
	}
	if got := normalizeExpiresAt(1786847930); got != 1786847930 {
		t.Errorf("已是秒不该变: %d", got)
	}
}
