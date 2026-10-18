package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/scheduler"
	"allplatforms/internal/platform/trae/upstream"
)

// ── 测试夹具 ────────────────────────────────────────────────────────────

// fakeUpstream 起一套假上游，按路径给出可编程的响应。
type fakeUpstream struct {
	srv *httptest.Server
	// 各端点的响应；状态码 0 表示用默认 200 + resp
	chatBody   string // SSE 文本
	chatStatus int
	chatBody2  string

	modelsBody   string
	modelsStatus int

	checkinStatusBody string
	checkinClaimBody  string
	checkinClaimCode  int
	entUsageBody      string

	exchangeBody string
	userInfoBody string

	// 记录收到的请求头，便于断言上游头族
	gotAuthHeader string
	gotUgHeader   string
	callChat      int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		chatBody: "event:output\ndata:{\"response\":\"hi\"}\n\n" +
			"event:token_usage\ndata:{\"total_tokens\":5}\n\n" +
			"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n",
		modelsBody:        `{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}}]}`,
		checkinStatusBody: `{"checked_in":false,"credits":10,"enable":true}`,
		checkinClaimBody:  `{"code":0,"message":"ok"}`,
		entUsageBody:      `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":300}}]}`,
		exchangeBody:      `{"Result":{"Token":"NEW_AT","RefreshToken":"NEW_RT","TokenExpireAt":1786847930141}}`,
		userInfoBody:      `{"Result":{"UserID":"u-from-userinfo","ScreenName":"昵称","EnterpriseID":"e1"}}`,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/ide/v1/get_detail_param", func(w http.ResponseWriter, r *http.Request) {
		f.gotAuthHeader = r.Header.Get("Authorization")
		st := f.modelsStatus
		if st == 0 {
			st = 200
		}
		w.WriteHeader(st)
		_, _ = io.WriteString(w, f.modelsBody)
	})

	mux.HandleFunc("/api/agent/v3/llm_utils_chat", func(w http.ResponseWriter, r *http.Request) {
		f.callChat++
		f.gotAuthHeader = r.Header.Get("Authorization")
		f.gotUgHeader = r.Header.Get("X-Uid")
		if f.chatStatus != 0 {
			w.WriteHeader(f.chatStatus)
			_, _ = io.WriteString(w, f.chatBody2)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, f.chatBody)
	})

	mux.HandleFunc("/trae/api/v2/ug/checkin_credits/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.checkinStatusBody)
	})
	mux.HandleFunc("/trae/api/v2/ug/checkin_credits/claim", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.checkinClaimBody)
	})
	mux.HandleFunc("/trae/api/v2/pay/ide_user_ent_usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.entUsageBody)
	})
	mux.HandleFunc("/cloudide/api/v3/trae/oauth/ExchangeToken", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.exchangeBody)
	})
	mux.HandleFunc("/cloudide/api/v3/trae/GetUserInfo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, f.userInfoBody)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newTestHandler 组一套完整的 handler（池 + 假上游 + 调度器）。
func newTestHandler(t *testing.T, apiKey string, uids ...string) (*Handler, *fakeUpstream, *pool.Pool, string) {
	t.Helper()
	f := newFakeUpstream(t)
	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	up := upstream.New(5*time.Second, 5*time.Second, 5*time.Second)
	// 三类 host 全部指向假上游
	up.SetHosts(f.srv.URL, f.srv.URL, f.srv.URL)

	for _, uid := range uids {
		if strings.TrimSpace(uid) == "" {
			// 传空串表示「这个用例要一个空池」。直接跳过，否则会造出一个
			// UID 为空的假账号，掩盖真正的「无可用号」路径。
			continue
		}
		a := &auth.Auth{
			AccessToken: "AT-" + uid, RefreshToken: "RT-" + uid,
			UID: uid, Nickname: "n-" + uid,
			MachineID: "M", DeviceID: "1234567890123456",
			ApiHost:  f.srv.URL,
			FilePath: filepath.Join(dir, "trae-"+uid+".json"),
		}
		p.Add(a)
	}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, RefreshSkew: time.Hour})

	h := NewHandler(Config{
		Pool: p, Upstream: up, Sched: sch,
		APIKey:  apiKey,
		AuthDir: dir,
		Version: "test",
		Start:   time.Now(),
		PoolCfg: PoolOptions{
			MaxRotate: 3, PlanCooldown: time.Hour, SoftCooldown: time.Minute,
			ErrThreshold: 3, ErrCooldown: 10 * time.Minute, RefreshSkew: time.Hour,
			DefaultModel: "glm-5.2",
		},
	})
	// 收尾顺序很关键：先等后台任务（导入后的首次签到会写 dir 下的文件），
	// 再让 t.TempDir() 的自动清理删目录。顺序反了就会出现
	// "TempDir RemoveAll cleanup: The directory is not empty" 的随机失败
	//（实测 ~1/6；原始 traeapi2 仓库同样复现）。
	//
	// t.Cleanup 是 LIFO：这里注册的晚于 t.TempDir()，故会先执行——正好是
	// 我们需要的顺序。
	t.Cleanup(h.WaitBackground)
	return h, f, p, dir
}

func req(h *Handler, method, path, body string, key string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rr := httptest.NewRequest(method, path, r)
	if key != "" {
		rr.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, rr)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	return m
}

// TestMain 在每个用例前后清掉包级动态模型缓存。
//
// dynModels/dynFetched/dynFail 是包级变量（生产里就该是全进程一份），测试用例
// 之间会互相污染：A 用例缓存了 12 个模型，B 用例想验"上游挂了要回落内置目录"
// 就会命中 A 的缓存而假绿/假红。
func TestMain(m *testing.M) {
	resetModels()
	code := m.Run()
	resetModels()
	os.Exit(code)
}

// ── 鉴权 ────────────────────────────────────────────────────────────────

func TestAuthRequired(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "k1")
	rec := req(h, "GET", "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Errorf("healthz 不该鉴权，实为 %d", rec.Code)
	}
	for _, path := range []string{"/status", "/v1/models", "/api/overview", "/api/accounts"} {
		if rec := req(h, "GET", path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 未带 key 应 401，实为 %d", path, rec.Code)
		}
		if rec := req(h, "GET", path, "", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 错 key 应 401，实为 %d", path, rec.Code)
		}
		if rec := req(h, "GET", path, "", "k1"); rec.Code != http.StatusOK {
			t.Errorf("%s 正确 key 应 200，实为 %d", path, rec.Code)
		}
	}
}

func TestAuthViaXTraeKeyAndQuery(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "k1")
	// X-Trae-Key
	rr := httptest.NewRequest("GET", "/api/overview", nil)
	rr.Header.Set("X-Trae-Key", "k1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, rr)
	if rec.Code != http.StatusOK {
		t.Errorf("X-Trae-Key 鉴权失败: %d", rec.Code)
	}
	// query ?key=
	if rec := req(h, "GET", "/api/accounts?key=k1", "", ""); rec.Code != http.StatusOK {
		t.Errorf("query key 鉴权失败: %d", rec.Code)
	}
}

// api_key 为空 = 不鉴权（与主服务同语义）。
func TestNoAPIKeyMeansOpen(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	if rec := req(h, "GET", "/status", "", ""); rec.Code != http.StatusOK {
		t.Errorf("空 key 时不该 401，实为 %d", rec.Code)
	}
}

// ── CORS ────────────────────────────────────────────────────────────────

func TestCORSPreflight(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	rr := httptest.NewRequest("OPTIONS", "/api/overview", nil)
	rr.Header.Set("Origin", "http://127.0.0.1:7863")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, rr)
	if rec.Code != http.StatusNoContent {
		t.Errorf("预检应回 204，实为 %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:7863" {
		t.Errorf("CORS 头不对: %q", got)
	}
	// 带 Origin 的实际请求也要带 CORS 头（不走预检的场景）
	rr2 := httptest.NewRequest("GET", "/api/accounts", nil)
	rr2.Header.Set("Origin", "http://127.0.0.1:7863")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, rr2)
	if rec2.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("带 Origin 的请求缺 CORS 头")
	}
}

// ── 安全头 ──────────────────────────────────────────────────────────────

func TestSecurityHeaders(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	rec := req(h, "GET", "/healthz", "", "")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff 缺失: %q", got)
	}
}

// ── 观测接口 ────────────────────────────────────────────────────────────

func TestHealthzAndStatus(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1", "u2")
	p.Disable("u2", "dead")

	got := decode(t, req(h, "GET", "/healthz", "", ""))
	if got["service"] != "traeapi2" {
		t.Errorf("service 标识不对: %v", got["service"])
	}
	if got["total"] != float64(2) {
		t.Errorf("total 不对: %v", got["total"])
	}
	if got["disabled"] != float64(1) {
		t.Errorf("disabled 不对: %v", got["disabled"])
	}

	st := decode(t, req(h, "GET", "/status", "", ""))
	accs, _ := st["accounts"].([]any)
	if len(accs) != 2 {
		t.Errorf("status 账号数不对: %d", len(accs))
	}
}

func TestOverviewAggregatesCredits(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1", "u2")
	p.SetUsage("u1", 700, 1000, 300, 1)
	p.SetUsage("u2", 300, 1000, 700, 1)
	got := decode(t, req(h, "GET", "/api/overview", "", ""))
	if got["credits"] != float64(1000) {
		t.Errorf("积分合计不对: %v", got["credits"])
	}
	if got["service"] != "trae" {
		t.Errorf("service 不对: %v", got["service"])
	}
}

// ── 模型列表 ────────────────────────────────────────────────────────────

func TestModelsDynamicAndFallback(t *testing.T) {
	// 动态拉取成功
	h, f, _, _ := newTestHandler(t, "", "u1")
	f.modelsBody = `{"config_info_list":[{"config_name":"glm-5.2"},{"config_name":"kimi-k3"}]}`
	resetModels()
	got := decode(t, req(h, "GET", "/v1/models", "", ""))
	data, _ := got["data"].([]any)
	if len(data) != 2 {
		t.Errorf("应返回 2 个模型，实为 %d", len(data))
	}
	if data[0].(map[string]any)["id"] != "glm-5.2" {
		t.Errorf("模型 ID 不对: %v", data[0])
	}

	// 动态失败 → 回落内置目录
	h2, f2, _, _ := newTestHandler(t, "", "u1")
	f2.modelsStatus = 500
	resetModels()
	got2 := decode(t, req(h2, "GET", "/v1/models", "", ""))
	data2, _ := got2["data"].([]any)
	if len(data2) == 0 {
		t.Error("上游失败时该回落内置目录")
	}

	// 无账号 → 也回落
	h3, _, _, _ := newTestHandler(t, "", "")
	resetModels()
	if got3 := decode(t, req(h3, "GET", "/v1/models", "", "")); len(got3["data"].([]any)) == 0 {
		t.Error("无账号时也该回落内置目录")
	}
}

// 空目录不算成功（否则会把上游的"空列表"当真相缓存一小时）。
func TestModelsEmptyListTreatedAsFailure(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "u1")
	f.modelsBody = `{"config_info_list":[]}`
	resetModels()
	got := decode(t, req(h, "GET", "/api/overview", "", ""))
	_ = got
	// 通过 /api/models 直接打：应报错而不是给空列表
	if rec := req(h, "GET", "/api/models", "", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("空目录应视为失败，实为 %d", rec.Code)
	}
}

// ── 对话 ────────────────────────────────────────────────────────────────

func TestChatNonStream(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}],"stream":false}`
	rec := req(h, "POST", "/v1/chat/completions", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("对话失败: %d %s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	if got["object"] != "chat.completion" {
		t.Errorf("object 不对: %v", got["object"])
	}
	ch := got["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "hi" {
		t.Errorf("内容不对: %v", msg["content"])
	}
	// 上游头族：Cloud-IDE-JWT + X-Uid
	if !strings.HasPrefix(f.gotAuthHeader, "Cloud-IDE-JWT ") {
		t.Errorf("上游 Authorization 头不对: %q", f.gotAuthHeader)
	}
	if f.gotUgHeader != "u1" {
		t.Errorf("X-Uid 头不对: %q", f.gotUgHeader)
	}
	// 成功计数入池
	st, _ := p.Status("u1")
	if st.SuccessCount != 1 || st.ErrTotal != 0 {
		t.Errorf("成功计数不对: %+v", st)
	}
	if st.TokensDelta != 5 {
		t.Errorf("token 累计不对: %d", st.TokensDelta)
	}
}

func TestChatStream(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := req(h, "POST", "/v1/chat/completions", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("流式对话失败: %d", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "chat.completion.chunk") {
		t.Errorf("未产出 chunk: %s", out)
	}
	if !strings.Contains(out, `"content":"hi"`) {
		t.Errorf("内容未透出: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("缺 [DONE]: %s", out)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type 不对: %q", ct)
	}
}

// 空模型 / auto → 回落默认模型。
func TestMapModelFallback(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	if got := h.mapModel(""); got != "glm-5.2" {
		t.Errorf("空模型应回落默认: %q", got)
	}
	if got := h.mapModel("auto"); got != "glm-5.2" {
		t.Errorf("auto 应回落默认: %q", got)
	}
	if got := h.mapModel("  "); got != "glm-5.2" {
		t.Errorf("空白应回落默认: %q", got)
	}
	if got := h.mapModel("kimi-k3"); got != "kimi-k3" {
		t.Errorf("显式模型应原样: %q", got)
	}
}

// 未知模型不该被网关拦（上游自有兜底），而是原样转发。
func TestMapModelPassthroughUnknown(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	if got := h.mapModel("some-future-model"); got != "some-future-model" {
		t.Errorf("未知模型应透传: %q", got)
	}
}

func TestSetModelInBody(t *testing.T) {
	out := setModelInBody([]byte(`{"model":"old","x":1}`), "new")
	got := decodeBytes(t, out)
	if got["model"] != "new" || got["x"] != float64(1) {
		t.Errorf("model 替换不对: %v", got)
	}
	// 非法 JSON 原样返回
	bad := []byte(`{`)
	if string(setModelInBody(bad, "new")) != "{" {
		t.Error("非法 JSON 应原样返回")
	}
}

// 会话粘性键的优先级：X-Session-ID > X-Conversation-ID > body.user > 首轮指纹。
func TestExtractSessionKey(t *testing.T) {
	rr := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rr.Header.Set("X-Session-ID", "s1")
	rr.Header.Set("X-Conversation-ID", "c1")
	if got := extractSessionKey(rr, nil); got != "s1" {
		t.Errorf("X-Session-ID 应优先: %q", got)
	}
	rr2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rr2.Header.Set("X-Conversation-ID", "c1")
	if got := extractSessionKey(rr2, nil); got != "c1" {
		t.Errorf("X-Conversation-ID 次之: %q", got)
	}
	rr3 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if got := extractSessionKey(rr3, []byte(`{"user":"u9"}`)); got != "u9" {
		t.Errorf("body.user 应生效: %q", got)
	}
	// 首轮内容 → 稳定指纹（同样输入必得同键）
	b := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"q1"}]}`)
	k1 := extractSessionKey(httptest.NewRequest("POST", "/x", nil), b)
	k2 := extractSessionKey(httptest.NewRequest("POST", "/x", nil), b)
	if k1 == "" || k1 != k2 {
		t.Errorf("首轮指纹应稳定非空: %q vs %q", k1, k2)
	}
	// 不同首轮 → 不同键
	k3 := extractSessionKey(httptest.NewRequest("POST", "/x", nil), []byte(`{"messages":[{"role":"user","content":"q2"}]}`))
	if k3 == k1 {
		t.Error("不同首轮应产出不同指纹")
	}
	// 无从提取 → 空串
	if got := extractSessionKey(httptest.NewRequest("POST", "/x", nil), []byte(`{}`)); got != "" {
		t.Errorf("无法提取时返回空串: %q", got)
	}
}

// 会话粘性真的生效：同一 conversation 打到同一账号。
func TestChatStickySameAccount(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1", "u2")
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"sticky"}],"stream":false}`
	for i := 0; i < 4; i++ {
		rr := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rr.Header.Set("X-Session-ID", "sess-x")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, rr)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次对话失败: %d", i, rec.Code)
		}
	}
	// 两次之间不该换号：成功计数应集中在同一账号
	var maxCount int64
	for _, st := range p.List() {
		if st.SuccessCount > maxCount {
			maxCount = st.SuccessCount
		}
	}
	if maxCount != 4 {
		t.Errorf("粘性失效：最大成功计数 %d，应为 4", maxCount)
	}
}

func TestChatNoHealthyAccount(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "")
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无账号应 503，实为 %d", rec.Code)
	}
	got := decode(t, rec)
	errObj, _ := got["error"].(map[string]any)
	if errObj["code"] != "no_healthy_account" {
		t.Errorf("错误码不对: %v", errObj)
	}
}

func TestChatBodyTooLarge(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	big := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + strings.Repeat("x", maxBodyBytes+10) + `"}]}`
	rec := req(h, "POST", "/v1/chat/completions", big, "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("超限 body 应 413，实为 %d", rec.Code)
	}
}

// 1005 权益不足 → 长冷却；换号后仍无号 → 503。
func TestChatPlanLimitCooldownsAndRotates(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1", "u2")
	f.chatStatus = 400
	f.chatBody2 = `{"code":1005,"message":"plan limit"}`
	body := `{"model":"glm-5.2","messages":[],"stream":false}`
	rec := req(h, "POST", "/v1/chat/completions", body, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("两号都被冷却后应 503，实为 %d", rec.Code)
	}
	// 两个号都该进入 plan 冷却
	for _, st := range p.List() {
		if !st.Cooling || !strings.Contains(st.Reason, "plan") {
			t.Errorf("账号 %s 未按 plan 冷却: %+v", st.UID, st)
		}
	}
	// 上游应被调了两次（首号 + 换号）
	if f.callChat != 2 {
		t.Errorf("应换号重试一次，共 2 次上游调用，实为 %d", f.callChat)
	}
}

// 401 → 凭证失效禁用（不是冷却）。
func TestChatSessionDeadDisables(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	f.chatStatus = 401
	f.chatBody2 = `{"message":"unauthorized"}`
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("应 503，实为 %d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("401 应禁用账号: %+v", st)
	}
	if st.Cooling {
		t.Errorf("401 是禁用不是冷却: %+v", st)
	}
}

// 429 → 短冷却（不累计错误）。
func TestChatSoftRateCooldowns(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	f.chatStatus = 429
	f.chatBody2 = `too many requests`
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("应 503，实为 %d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || !strings.Contains(st.Reason, "429") {
		t.Errorf("429 应短冷却: %+v", st)
	}
}

// 404 → 短冷却（不累计错误，也不禁用）。
func TestChatNotFoundCooldowns(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	f.chatStatus = 404
	f.chatBody2 = `not found`
	req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	st, _ := p.Status("u1")
	if !st.Cooling || st.Disabled {
		t.Errorf("404 应短冷却且不禁用: %+v", st)
	}
}

// 5xx → 累计错误，达阈冷却。
func TestChatServerErrorAccumulates(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	f.chatStatus = 500
	f.chatBody2 = `boom`
	req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	st, _ := p.Status("u1")
	if st.ErrTotal == 0 {
		t.Errorf("5xx 应记错误: %+v", st)
	}
}

// 流内 error 事件：非流式路径必须换号而不是把错误当成功返回。
func TestChatStreamErrorInsideSSERotates(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1", "u2")
	f.chatBody = "event:error\ndata:{\"code\":1005,\"message\":\"plan\"}\n\n"
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[],"stream":false}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("流内权益不足应换号后 503，实为 %d %s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("流内 1005 应冷却: %+v", st)
	}
}

// 流式路径遇 error 事件：写 error 帧 + [DONE]，不算请求失败。
func TestChatStreamErrorEmitsErrorFrame(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "u1")
	f.chatBody = "event:output\ndata:{\"response\":\"partial\"}\n\n" +
		"event:error\ndata:{\"code\":4011,\"message\":\"rate\"}\n\n"
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[],"stream":true}`, "")
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Errorf("应写出 error 帧: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("error 后仍要补 [DONE]: %s", out)
	}
	if !strings.Contains(out, `"content":"partial"`) {
		t.Errorf("error 前的内容应已下发: %s", out)
	}
}

// token 刷新失败（401）应禁用该号并继续换号。
func TestChatRefreshFailureDisables(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	// 让 ExchangeToken 返回 401：refresh 必然失败
	f.exchangeBody = `{"error":"unauthorized"}`
	f.modelsStatus = 500 // 不影响，只是保证不缓存模型
	// 凭证设为立即过期，强制刷新
	a := p.AuthByUID("u1")
	a.Lock()
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	a.Unlock()
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("刷新失败后无号可用应 503，实为 %d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.ErrTotal == 0 {
		t.Errorf("刷新失败应记错误: %+v", st)
	}
}

// ── 管理接口 ────────────────────────────────────────────────────────────

func TestAccountActions(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1")

	// balance
	rec := req(h, "POST", "/api/accounts/u1/balance", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("balance 失败: %d %s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	if got["credits"] != float64(700) {
		t.Errorf("余额解析不对: %v", got["credits"])
	}
	if got["packs"] != float64(1) {
		t.Errorf("权益包数不对: %v", got["packs"])
	}

	// disable → revive
	req(h, "POST", "/api/accounts/u1/disable", "", "")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Error("disable 未生效")
	}
	req(h, "POST", "/api/accounts/u1/revive", "", "")
	if st, _ := p.Status("u1"); st.Disabled {
		t.Error("revive 未生效")
	}

	// patch
	rec2 := req(h, "PATCH", "/api/accounts/u1", `{"enabled":false,"note":"维护中","nickname":"新名"}`, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("patch 失败: %d", rec2.Code)
	}
	st, _ := p.Status("u1")
	if st.Enabled || st.Note != "维护中" {
		t.Errorf("patch 未生效: %+v", st)
	}

	// remove（凭证文件也该删）
	if rec3 := req(h, "POST", "/api/accounts/u1/remove", "", ""); rec3.Code != http.StatusOK {
		t.Fatalf("remove 失败: %d", rec3.Code)
	}
	if _, ok := p.Status("u1"); ok {
		t.Error("remove 未从池中删除")
	}
}

// 传输层故障（连不上上游）要累计错误并冷却，而不是让请求裸崩。
func TestChatTransportFailure(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	// 把 client 的 host 指向一个不存在的地址，制造传输层错误
	f.srv.Close()
	rec := req(h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("传输失败应 503，实为 %d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.ErrTotal == 0 {
		t.Errorf("传输失败应记错误: %+v", st)
	}
	if st.LastErrMsg == "" {
		t.Errorf("应记错误信息便于排错: %+v", st)
	}
}

// 回调反查 pending：只带 loginTraceID 时要能把 machine/device 找回来。
func TestLoginCallbackRecoversMachineByTrace(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "")
	// 先开一个 pending，拿到它的 trace id
	start := decode(t, req(h, "POST", "/api/login/start", `{"callback_port":18081}`, ""))
	pendingID := start["pending_id"].(string)

	h.loginMu.Lock()
	pl := h.logins[pendingID]
	machine, device := pl.machineID, pl.deviceID
	trace := machineTraceID(machine, device)
	h.loginMu.Unlock()

	if trace == "" || len(trace) != 16 {
		t.Fatalf("trace id 不对: %q", trace)
	}
	// 只传 trace（真实浏览器回调就是这样：不回传 machine_id/device_id）
	cb := "/authorize?refreshToken=RT-TRACE&loginTraceID=" + trace +
		"&userInfo=" + urlEscape(`{"UserID":"u-trace"}`)
	rec := req(h, "GET", cb, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("凭 trace 反查应成功: %d %s", rec.Code, rec.Body.String())
	}
	// pending 应被标成功
	r := decode(t, req(h, "GET", "/api/login/result?pending_id="+pendingID, "", ""))
	if r["state"] != "success" {
		t.Errorf("pending 未标成功: %v", r)
	}
	_ = f
}

func TestAccountActionsOnUnknownUID(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	if rec := req(h, "POST", "/api/accounts/nope/balance", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("未知 uid 应 404，实为 %d", rec.Code)
	}
	// 非法 uid 应 400
	if rec := req(h, "POST", "/api/accounts/..%2fetc/balance", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 uid 应 400，实为 %d", rec.Code)
	}
}

func TestAccountRefresh(t *testing.T) {
	h, _, _, dir := newTestHandler(t, "", "u1")
	rec := req(h, "POST", "/api/accounts/u1/refresh", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh 失败: %d %s", rec.Code, rec.Body.String())
	}
	// 换来的新 token 必须落盘
	fp := filepath.Join(dir, "trae-u1.json")
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("凭证未落盘: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	authPart, _ := doc["auth"].(map[string]any)
	if authPart["accessToken"] != "NEW_AT" {
		t.Errorf("新 token 未落盘: %v", authPart["accessToken"])
	}
	if authPart["refreshToken"] != "NEW_RT" {
		t.Errorf("轮换后的 refreshToken 未落盘: %v", authPart["refreshToken"])
	}
}

// 签到：status 报未签 → claim → 回查余额入池。
func TestCheckinAllFlow(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1")
	rec := req(h, "POST", "/api/checkin_all", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("签到失败: %d %s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	if got["ok"] != float64(1) {
		t.Errorf("应签到成功 1 个: %v", got["ok"])
	}
	if !p.TodayCheckedIn("u1") {
		t.Error("签到状态未记入池")
	}
	if st, _ := p.Status("u1"); st.Credits != 700 {
		t.Errorf("签到后未回查余额: %d", st.Credits)
	}
}

// 今日已签 → 跳过（幂等）。
func TestCheckinIdempotent(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "u1")
	req(h, "POST", "/api/checkin_all", "", "")
	f.checkinStatusBody = `{"checked_in":true,"credits":0,"enable":true}`
	rec := req(h, "POST", "/api/checkin_all", "", "")
	got := decode(t, rec)
	if got["skipped"] != float64(1) {
		t.Errorf("今日已签应跳过: %v", got["skipped"])
	}
}

// 签到未开启 → 不报错，记 0 成功也不记失败。
func TestCheckinNotEnabled(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "u1")
	f.checkinStatusBody = `{"checked_in":false,"credits":0,"enable":false}`
	rec := req(h, "POST", "/api/checkin_all", "", "")
	got := decode(t, rec)
	if got["failed"] != float64(0) {
		t.Errorf("未开启签到不该算失败: %v", got["failed"])
	}
}

// claim 返回业务错误 → 按幂等成功处理（上游「已签到」也走这条路）。
func TestCheckinClaimIdempotentError(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "u1")
	f.checkinClaimBody = `{"code":10001,"message":"already checked in"}`
	rec := req(h, "POST", "/api/checkin_all", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("幂等错误不该 5xx: %d", rec.Code)
	}
}

// TestCheckinAlreadyCheckedStillWritesUsage 钉住「今日已签到」也要回写额度。
//
// ── 回归背景（2026-10-14 用户反馈）──────────────────────────────────
//
// 用户导入新账号后看到「积分 0」，而老账号有值 —— 看着像数据丢失。
// 根因：新账号在官方客户端登录时通常**已经签过到**，于是导入时走
// checked=true 分支，而该分支原先**直接 return，不调 EntUsage**，
// 导致台账里只有 checked_in、没有 credits，要等后台周期刷新（30 分钟）
// 或手动刷新才出现真实值。
//
// 修法：所有分支都回写额度（抽成 refreshUsageQuiet）。本用例确保
// 「已签到」这条最容易漏的路径也会写 usage。
func TestCheckinAlreadyCheckedStillWritesUsage(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	// 上游说「今天已经签过了」
	f.checkinStatusBody = `{"checked_in":true,"credits":0,"enable":true}`
	// EntUsage 返回 limit=1000 / used=300 → remain=700
	f.entUsageBody = `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":300}}]}`

	_ = req(h, "POST", "/api/checkin_all", "", "")

	// 关键断言：台账里必须有额度（原先这条路径不会写）
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应仍在池中")
	}
	if st.Limit == 0 && st.Credits == 0 {
		t.Fatalf("已签到分支也必须回写额度，实为 credits=%d limit=%d", st.Credits, st.Limit)
	}
	if st.Credits != 700 {
		t.Errorf("额度应为 700（1000-300），实为 %d", st.Credits)
	}
}

// TestCheckinNotEnabledStillWritesUsage 「签到未开启」也要回写额度。
func TestCheckinNotEnabledStillWritesUsage(t *testing.T) {
	h, f, p, _ := newTestHandler(t, "", "u1")
	f.checkinStatusBody = `{"checked_in":false,"credits":0,"enable":false}`
	f.entUsageBody = `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":300}}]}`

	_ = req(h, "POST", "/api/checkin_all", "", "")

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应仍在池中")
	}
	if st.Limit == 0 && st.Credits == 0 {
		t.Fatalf("签到未开启分支也必须回写额度，实为 credits=%d limit=%d", st.Credits, st.Limit)
	}
}

// TestUsageLookupFailureDoesNotFailCheckin 额度查不到不该让签到算失败。
//
// 签到本身已经成功，额度只是附加信息；查额度失败应静默（记日志），
// 下次后台刷新会补上。若把它算成签到失败，用户会看到「签到失败」
// 却其实已经签上了 —— 误导性更强。
func TestUsageLookupFailureDoesNotFailCheckin(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "u1")
	f.checkinStatusBody = `{"checked_in":true,"credits":0,"enable":true}`
	f.entUsageBody = `not json at all` // 额度接口返回坏数据

	rec := req(h, "POST", "/api/checkin_all", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("额度查询失败不该让请求 5xx: %d", rec.Code)
	}
	got := decode(t, rec)
	if got["failed"] != float64(0) {
		t.Errorf("额度查询失败不该计为签到失败: %v", got["failed"])
	}
}

func TestCheckinAllWithNoScheduler(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	h.cfg.Sched = nil
	rec := req(h, "POST", "/api/checkin_all", "", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("无调度器应 501，实为 %d", rec.Code)
	}
}

func TestKeepaliveAndBalanceAll(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1", "u2")
	rec := req(h, "POST", "/api/balance_all", "", "")
	got := decode(t, rec)
	if got["ok"] != float64(2) {
		t.Errorf("额度刷新应 2 成功: %v", got["ok"])
	}
	if st, _ := p.Status("u1"); st.Credits != 700 {
		t.Errorf("额度未入池: %d", st.Credits)
	}
	rec2 := req(h, "POST", "/api/keepalive_all", "", "")
	if rec2.Code != http.StatusOK {
		t.Errorf("保活失败: %d", rec2.Code)
	}
}

func TestSingleAccountCheckin(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "u1")
	rec := req(h, "POST", "/api/accounts/u1/checkin", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("单号签到失败: %d %s", rec.Code, rec.Body.String())
	}
	if !p.TodayCheckedIn("u1") {
		t.Error("单号签到未记入池")
	}
}

// ── 登录闭环 ────────────────────────────────────────────────────────────

func TestLoginStartCreatesPending(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	rec := req(h, "POST", "/api/login/start", `{}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("获取登录链接失败: %d", rec.Code)
	}
	got := decode(t, rec)
	id, _ := got["pending_id"].(string)
	url, _ := got["login_url"].(string)
	if id == "" || url == "" {
		t.Fatalf("pending_id / login_url 缺失: %v", got)
	}
	if !strings.Contains(url, "trae.cn/authorization") {
		t.Errorf("登录 URL 不对: %q", url)
	}
	// result 应处于 pending
	r2 := decode(t, req(h, "GET", "/api/login/result?pending_id="+id, "", ""))
	if r2["state"] != "pending" {
		t.Errorf("初始 state 应为 pending: %v", r2["state"])
	}
	// 取消
	r3 := decode(t, req(h, "POST", "/api/login/cancel", `{"pending_id":"`+id+`"}`, ""))
	if r3["canceled"] != true {
		t.Errorf("取消未生效: %v", r3)
	}
	// 取消后再查应 404
	if rec := req(h, "GET", "/api/login/result?pending_id="+id, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("取消后应 404，实为 %d", rec.Code)
	}
}

func TestLoginResultUnknownPending(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "u1")
	if rec := req(h, "GET", "/api/login/result?pending_id=nope", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("未知 pending 应 404，实为 %d", rec.Code)
	}
}

// 回调：refreshToken → ExchangeToken → GetUserInfo → 落盘 → 进池。
func TestAuthorizeCallbackImportsAccount(t *testing.T) {
	h, _, p, dir := newTestHandler(t, "", "")
	cb := "/authorize?refreshToken=RT-FROM-CB" +
		"&userInfo=" + urlEscape(`{"UserID":"u-cb","ScreenName":"回调号"}`) +
		"&machine_id=M1&device_id=1234567890123456"
	rec := req(h, "GET", cb, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("回调失败: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("回调应返回 HTML 页面: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "登录成功") {
		t.Errorf("回调页面应显示成功: %s", rec.Body.String())
	}
	// GetUserInfo 会覆盖 uid（假上游返回 u-from-userinfo）
	if _, ok := p.Status("u-from-userinfo"); !ok {
		t.Fatalf("账号未进池: %+v", p.List())
	}
	// 凭证应落盘
	if _, err := os.Stat(filepath.Join(dir, "trae-u-from-userinfo.json")); err != nil {
		t.Errorf("凭证未落盘: %v", err)
	}
}

func TestAuthorizeCallbackMissingCreds(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "")
	rec := req(h, "GET", "/authorize", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺凭证应 400，实为 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "登录失败") {
		t.Errorf("应显示失败页: %s", rec.Body.String())
	}
}

// ExchangeToken 失败 → 回调报错，不落盘。
func TestAuthorizeCallbackExchangeFails(t *testing.T) {
	h, f, p, dir := newTestHandler(t, "", "u1")
	f.exchangeBody = `{"error":"invalid_grant"}`
	cb := "/authorize?refreshToken=RT-BAD&userInfo=" + urlEscape(`{"UserID":"u-bad"}`)
	rec := req(h, "GET", cb, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("换 token 失败应 400，实为 %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "trae-u-bad.json")); err == nil {
		t.Error("失败时不该落盘凭证")
	}
	_ = p
}

// 回调结果页的 XSS 转义：uid / 昵称会原样进 HTML，必须转义后才安全。
//
// 直接测 renderAuthPage 而非走整条回调链：uid 在 finishImport 里会被
// GetUserInfo 的返回值覆盖，走回调链时那段注入串根本到不了页面，
// 测出来的"没转义"是假象（用 uid=合法 + GetUserInfo 返回注入值才能命中）。
func TestRenderAuthPageEscapesHTML(t *testing.T) {
	rec := httptest.NewRecorder()
	renderAuthPage(rec, http.StatusOK, "登录成功",
		"账号 <script>alert(1)</script> 已加入")
	body := rec.Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("未转义 XSS: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("应转义为 HTML 实体: %s", body)
	}
	if strings.Contains(body, "alert(1)</script>") {
		t.Errorf("闭合标签未被转义: %s", body)
	}
	// 失败页也要转义（错误信息里带的是上游 body，同样不可信）
	rec2 := httptest.NewRecorder()
	renderAuthPage(rec2, http.StatusBadRequest, "登录失败", `upstream said "<img onerror=x>"`)
	body2 := rec2.Body.String()
	if strings.Contains(body2, "<img onerror=x>") {
		t.Errorf("失败页未转义: %s", body2)
	}
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("状态码应透出 400，实为 %d", rec2.Code)
	}
	// 标题也要转义
	rec3 := httptest.NewRecorder()
	renderAuthPage(rec3, http.StatusOK, `<b>标题</b>`, "详情")
	if strings.Contains(rec3.Body.String(), "<b>标题</b>") {
		t.Error("标题未转义")
	}
}

// ── 导入 ────────────────────────────────────────────────────────────────

func TestImportJSONCredential(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "")
	payload := `{"accessToken":"AT-X","refreshToken":"RT-X","uid":"u-imp","machineId":"M","deviceId":"1234567890123456"}`
	rec := req(h, "POST", "/api/import", `{"payload":`+mustJSONString(payload)+`}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	if got["ok"] != true {
		t.Errorf("导入未成功: %v", got)
	}
	// 缺 refreshToken 的凭证应被拒（无法续期）
	bad := `{"accessToken":"AT-Y","uid":"u-bad2"}`
	if rec2 := req(h, "POST", "/api/import", `{"payload":`+mustJSONString(bad)+`}`, ""); rec2.Code != http.StatusBadRequest {
		t.Errorf("缺 refreshToken 应 400，实为 %d", rec2.Code)
	}
	_ = p
}

// TestImportResponseCarriesCredits 导入响应必须**同步**带上额度。
//
// ── 回归背景（2026-10-14 用户反馈「新账号积分没显示」）──────────────
//
// 原先首签走后台 goroutine（h.bg.Go）异步执行，于是：
//
//	导入请求返回时额度还没写 → 面板立刻刷新看到「积分 0」
//	→ 用户以为新账号没积分（老账号有值，对比之下更像数据丢失）
//
// 修法：首签改为**同步**执行并把额度回填进响应，导入后积分立即可见。
// 本用例断言响应里带 credits（假上游 EntUsage 返回 limit=1000/used=300）。
func TestImportResponseCarriesCredits(t *testing.T) {
	h, f, _, _ := newTestHandler(t, "", "")
	// 假上游：已签到 + 额度 1000/300 → 剩 700
	f.checkinStatusBody = `{"checked_in":true,"credits":0,"enable":true}`
	f.entUsageBody = `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":300}}]}`

	payload := `{"accessToken":"AT-C","refreshToken":"RT-C","uid":"u-cred","deviceId":"1234567890123456"}`
	rec := req(h, "POST", "/api/import", `{"payload":`+mustJSONString(payload)+`}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	// 关键断言：响应里直接带额度（异步时代这里必然是空）
	if got["credits"] != float64(700) {
		t.Errorf("导入响应应同步带回额度 700，实为 %v（响应: %v）", got["credits"], got)
	}
}
//
// 回归背景（2026-10-14 实测发现）：签到走 api.trae.cn，请求头必须带
// 合法的小写 x-device-id（16 位纯数字）。缺失时上游返回 9004「缺少设备字段」，
// 该账号**永远签不上**。
//
// 而「粘贴凭证导入」这条路径拿到的 JSON 通常不含 did（用户从别处复制的凭证、
// 或从官方客户端 storage.json 解出来的），症状很隐蔽：
//   - 刷额度正常（那条接口不校验 did）
//   - 对话正常
//   - 只有签到失败 → 用户以为「签到功能坏了」
//
// 原先只有 LoadDir（进程启动时）会补 did，运行期导入不会补，
// 用户导入后必须重启一次才能签到。本用例确保导入时就补上。
func TestImportFillsMissingDeviceID(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "")

	// 凭证里**故意不带** deviceId（复刻粘贴导入的真实形态）
	payload := `{"accessToken":"AT-D","refreshToken":"RT-D","uid":"u-nodid"}`
	rec := req(h, "POST", "/api/import", `{"payload":`+mustJSONString(payload)+`}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}

	// 注意：finishImport 会用 GetUserInfo 的结果覆盖 UID（假上游固定返回
	// u-from-userinfo），所以这里必须按**导入后的实际 uid** 取账号，
	// 不能用请求里写的 uid。
	uid := decode(t, rec)["uid"].(string)
	a := p.AuthByUID(uid)
	if a == nil {
		t.Fatalf("账号未进池（uid=%s）", uid)
	}
	did := a.DeviceID
	if len(did) != 16 {
		t.Errorf("应补 16 位 did，实为 %q（len=%d）", did, len(did))
	}
	for i := 0; i < len(did); i++ {
		if did[i] < '0' || did[i] > '9' {
			t.Fatalf("did 必须是纯数字，实为 %q", did)
		}
	}
}

// TestImportKeepsExistingDeviceID 已有合法 did 的凭证**不能被覆盖**。
//
// 理由：真机/官方下发的 did 是上游登记过的设备指纹，覆盖会让上游
// 把它当成新设备，可能触发风控。EnsureCheckinDeviceID 的设计就是
// 「合法就沿用，只补缺失/非法」——本用例钉住这条语义。
func TestImportKeepsExistingDeviceID(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "")

	const want = "1234567890123456"
	payload := `{"accessToken":"AT-K","refreshToken":"RT-K","uid":"u-hasdid","deviceId":"` + want + `"}`
	rec := req(h, "POST", "/api/import", `{"payload":`+mustJSONString(payload)+`}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", rec.Code, rec.Body.String())
	}
	// 同上前提：UID 会被 GetUserInfo 覆盖，按导入结果取。
	uid := decode(t, rec)["uid"].(string)
	a := p.AuthByUID(uid)
	if a == nil {
		t.Fatalf("账号未进池（uid=%s）", uid)
	}
	if a.DeviceID != want {
		t.Errorf("已有合法 did 不该被覆盖：want %q got %q", want, a.DeviceID)
	}
}

// 粘贴回调链接导入（用户从浏览器地址栏复制）。
func TestImportCallbackURL(t *testing.T) {
	h, _, p, _ := newTestHandler(t, "", "")
	cb := "http://127.0.0.1:18081/authorize?refreshToken=RT-URL" +
		"&userInfo=" + urlEscape(`{"UserID":"u-url","ScreenName":"链接号"}`)
	rec := req(h, "POST", "/api/import", `{"payload":`+mustJSONString(cb)+`}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("回调链接导入失败: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := p.Status("u-from-userinfo"); !ok {
		t.Errorf("账号未进池: %+v", p.List())
	}
}

func TestImportRejectsBadInput(t *testing.T) {
	h, _, _, _ := newTestHandler(t, "", "")
	if rec := req(h, "POST", "/api/import", `{"payload":""}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("空 payload 应 400，实为 %d", rec.Code)
	}
	if rec := req(h, "POST", "/api/import", `{"payload":"http://x/authorize"}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("无凭证的回调链接应 400，实为 %d", rec.Code)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func decodeBytes(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("非法 JSON: %v (%s)", err, b)
	}
	return m
}

func urlEscape(s string) string {
	r := strings.NewReplacer(`\`, `%5C`, `&`, `%26`, `=`, `%3D`, `{`, `%7B`, `}`, `%7D`, `:`, `%3A`, `"`, `%22`, ` `, `%20`)
	return r.Replace(s)
}

// resetModels 清掉动态模型缓存（测试间隔离）。
func resetModels() {
	dynMu.Lock()
	dynModels = nil
	dynFetched = time.Time{}
	dynFail = time.Time{}
	dynMu.Unlock()
}
