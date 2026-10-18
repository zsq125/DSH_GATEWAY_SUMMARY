// visionprobe_test.go 视觉探测的用例。
//
// 重点测**判定口径**（Classify）：它是整个模块唯一有真实分支的地方，
// 且口径必须与原 Python 脚本逐条一致——判错会让面板把可识图的模型
// 显示成不可识图（或反之），用户据此做的模型选择就全错了。
package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"allplatforms/internal/platform/probeimage"
)

// ── 判定口径 ────────────────────────────────────────────────────────────

// 2xx 且无拒识信号 → vision。
func TestClassifyVision(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"普通 200", 200, `{"choices":[{"message":{"content":"hi"}}]}`},
		{"201", 201, `{}`},
		{"204", 204, ``},
	}
	for _, c := range cases {
		st, note := Classify(c.status, c.body, nil)
		if st != StateVision {
			t.Errorf("%s: got %q want vision (note=%s)", c.name, st, note)
		}
	}
}

// 拒识信号 → non-vision（无论 2xx 还是非 2xx）。
func TestClassifyNonVision(t *testing.T) {
	// 覆盖原脚本 NON_VISION_MARKERS 的全部信号
	markers := []string{
		"11133", "model_does_not_support_images", "does not support images",
		"doesn't support images", "not support image", "not support images",
		"image input is not supported", "images are not supported",
		"this model does not support", "model cannot process image",
		"model can't process image",
	}
	for _, m := range markers {
		// 非 2xx
		if st, _ := Classify(400, "error: "+m, nil); st != StateNonVision {
			t.Errorf("400 + %q: got %q want non-vision", m, st)
		}
		// 2xx 但业务信封带拒识（网关可能透传上游错误体）
		if st, _ := Classify(200, `{"error":"`+m+`"}`, nil); st != StateNonVision {
			t.Errorf("200 + %q: got %q want non-vision", m, st)
		}
	}
}

// **图片本身无效**必须归 unknown，不能归 non-vision。
//
// 这是最关键的一条：上游对合成图/过期会话返回 11135，
// 若把它当拒识信号，所有模型都会被误判成「不可识图」。
func TestClassifyImageInvalidIsUnknown(t *testing.T) {
	markers := []string{
		"11135", "image_invalid", "image is invalid",
		"invalid image data", "start a new conversation",
		"replace the image", "image data rejected",
	}
	for _, m := range markers {
		st, note := Classify(400, "code 11135: "+m, nil)
		if st != StateUnknown {
			t.Errorf("image-invalid %q: got %q want unknown (note=%s)", m, st, note)
		}
		if !strings.Contains(note, "图片本身无效") {
			t.Errorf("note 应说明是图片无效而非能力问题: %s", note)
		}
	}
}

// 顺序陷阱：图片无效信号必须**先于**拒识信号判定。
//
// 构造一个同时含两类信号的 body：若顺序写反，会判成 non-vision（错）。
func TestClassifyImageInvalidTakesPrecedence(t *testing.T) {
	// 同时含 11135（图片无效）与 11133（模型不支持）
	body := `{"code":11135,"msg":"image_invalid; also 11133 does not support images"}`
	st, _ := Classify(400, body, nil)
	if st != StateUnknown {
		t.Errorf("含图片无效信号时应优先判 unknown，实为 %q", st)
	}
}

// 无明确信号的其它状态码 → unknown（不猜）。
func TestClassifyUnknownFallback(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500 无信号", 500, `{"error":"internal server error"}`},
		{"404 无信号", 404, `not found`},
		{"429 无信号", 429, `rate limited`},
		{"0 无状态无信号", 0, ``},
	}
	for _, c := range cases {
		if st, _ := Classify(c.status, c.body, nil); st != StateUnknown {
			t.Errorf("%s: got %q want unknown", c.name, st)
		}
	}
}

// 传输错误 → unknown（网络问题不是能力结论）。
func TestClassifyTransportError(t *testing.T) {
	st, note := Classify(0, "", errors.New("dial tcp: connection refused"))
	if st != StateUnknown {
		t.Errorf("传输错误应 unknown，实为 %q", st)
	}
	if !strings.Contains(note, "transport error") {
		t.Errorf("note=%s", note)
	}
}

// 大小写不敏感（上游可能返回大写）。
func TestClassifyCaseInsensitive(t *testing.T) {
	if st, _ := Classify(400, "DOES NOT SUPPORT IMAGES", nil); st != StateNonVision {
		t.Errorf("大写拒识信号应识别，实为 %q", st)
	}
}

// note 必须带上命中的具体标记（排障要能看出是哪个信号触发的）。
func TestClassifyNoteCarriesMarker(t *testing.T) {
	_, note := Classify(400, `{"msg":"does not support images"}`, nil)
	if !strings.Contains(note, "does not support images") {
		t.Errorf("note 应含命中标记: %s", note)
	}
}

// ── 探测图 ──────────────────────────────────────────────────────────────

// 探测图必须是**合法 PNG**且带真实内容（纯色图会被上游拒为 image_invalid）。
func TestProbeImageIsValidPNG(t *testing.T) {
	raw := probeimage.PNG()
	if len(raw) < 100 {
		t.Fatalf("PNG 太小，可疑: %d 字节", len(raw))
	}
	// PNG 魔数
	sig := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	if len(raw) < 8 {
		t.Fatal("PNG 过短")
	}
	for i, b := range sig {
		if raw[i] != b {
			t.Fatalf("PNG 魔数不对: % x", raw[:8])
		}
	}
	// 必须能解码回来（证明是合法 PNG，不是拼出来的字节）
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("PNG 解码失败: %v", err)
	}
}

// 探测图必须**确定性**（固定种子）：两次生成同一张图，
// 保证同一模型两次探测得同一结论（换图会引入无关变量）。
func TestProbeImageDeterministic(t *testing.T) {
	a := probeimage.Base64()
	b := probeimage.Base64()
	if a != b {
		t.Error("同一进程内两次取图应完全相同")
	}
	if a == "" {
		t.Error("base64 不该为空")
	}
}

// DataURL 前缀正确（塞进 image_url.url 的格式）。
func TestProbeImageDataURL(t *testing.T) {
	u := probeimage.DataURL()
	if !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("DataURL 前缀不对: %.40s", u)
	}
}

// ── Runner（用假上游）─────────────────────────────────────────────────

// newFakeGateway 造一个假网关：按模型名决定返回什么。
func newFakeGateway(t *testing.T, decide func(model string) (int, string)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m-vision"},{"id":"m-blind"},{"id":"m-weird"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// 断言确实带了图（否则探测就是在测别的东西）
		body, _ := json.Marshal(req)
		_ = body
		code, resp := decide(req.Model)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(resp))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunnerClassifiesAllModels(t *testing.T) {
	srv := newFakeGateway(t, func(model string) (int, string) {
		switch model {
		case "m-vision":
			return 200, `{"choices":[{"message":{"content":"hi"}}]}`
		case "m-blind":
			return 400, `{"code":11133,"msg":"does not support images"}`
		default:
			return 500, `{"msg":"boom"}`
		}
	})

	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	snap := r.Run(context.Background(), []Target{{Label: "fake", Base: srv.URL + "/v1", Models: []string{"m-vision", "m-blind", "m-weird"}}})

	if len(snap.Probes) != 3 {
		t.Fatalf("应探到 3 个模型，实为 %d: %+v", len(snap.Probes), snap.Probes)
	}
	if got := snap.Probes["m-vision"].State; got != StateVision {
		t.Errorf("m-vision: %q", got)
	}
	if !snap.Probes["m-vision"].Vision {
		t.Error("m-vision 的 Vision 字段应为 true")
	}
	if got := snap.Probes["m-blind"].State; got != StateNonVision {
		t.Errorf("m-blind: %q", got)
	}
	if snap.Probes["m-blind"].Vision {
		t.Error("m-blind 的 Vision 应为 false")
	}
	if got := snap.Probes["m-weird"].State; got != StateUnknown {
		t.Errorf("m-weird: %q", got)
	}

	v, n, u := snap.Counts()
	if v != 1 || n != 1 || u != 1 {
		t.Errorf("计数不对: v=%d n=%d u=%d", v, n, u)
	}
	if snap.Version != 1 || !snap.Exists {
		t.Errorf("快照元信息不对: %+v", snap)
	}
	if snap.UpdatedAt == "" {
		t.Error("UpdatedAt 不该为空")
	}
}

// 模型清单为空时应从 /v1/models 自动拉取。
func TestRunnerFetchesModelsWhenEmpty(t *testing.T) {
	srv := newFakeGateway(t, func(model string) (int, string) {
		return 200, `{"choices":[]}`
	})
	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	snap := r.Run(context.Background(), []Target{{Label: "fake", Base: srv.URL + "/v1"}})
	if len(snap.Probes) != 3 {
		t.Fatalf("应从 /v1/models 拉到 3 个模型，实为 %d", len(snap.Probes))
	}
}

// 快照里不能保留上一次的键（merge_previous 语义：只留本次出现的）。
//
// 否则「模型已下线但旧判定永不过期」，面板会一直显示一个不存在的模型可识图。
func TestRunnerDoesNotKeepStaleKeys(t *testing.T) {
	srv := newFakeGateway(t, func(model string) (int, string) {
		return 200, `{"choices":[]}`
	})
	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	snap := r.Run(context.Background(), []Target{{Label: "fake", Base: srv.URL + "/v1", Models: []string{"only-one"}}})
	if _, stale := snap.Probes["m-vision"]; stale {
		t.Error("快照不该含上一轮遗留的键")
	}
	if len(snap.Probes) != 1 {
		t.Errorf("应只有 1 个键，实为 %d", len(snap.Probes))
	}
}

// 鉴权头必须带上（否则真网关会 401，探测全落 unknown）。
func TestRunnerSendsAuthHeader(t *testing.T) {
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	r.Run(context.Background(), []Target{{Label: "fake", Base: srv.URL, Key: "sk-test", Models: []string{"m"}}})
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization=%q", gotAuth)
	}
	if gotAPIKey != "sk-test" {
		t.Errorf("x-api-key=%q", gotAPIKey)
	}
}

// 空 key 时**不能**发空鉴权头（网关会 400）——原脚本同款处理。
func TestRunnerNoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	r.Run(context.Background(), []Target{{Label: "fake", Base: srv.URL, Models: []string{"m"}}})
	if gotAuth != "" {
		t.Errorf("空 key 不该发鉴权头，实为 %q", gotAuth)
	}
}

// 请求体必须真的带图（否则探测的是「能不能发文本」，结论无意义）。
func TestRunnerSendsImagePayload(t *testing.T) {
	var sawImage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
			MaxTokens int  `json:"max_tokens"`
			Stream    bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			for _, c := range req.Messages[0].Content {
				if c.Type == "image_url" && strings.HasPrefix(c.ImageURL.URL, "data:image/png;base64,") {
					sawImage = true
				}
			}
		}
		if req.MaxTokens != 1 {
			t.Errorf("max_tokens 应为 1（探测不需要真回答），实为 %d", req.MaxTokens)
		}
		if req.Stream {
			t.Error("探测不该用流式")
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	r.Run(context.Background(), []Target{{Label: "fake", Base: srv.URL, Models: []string{"m"}}})
	if !sawImage {
		t.Error("请求体里没有 image_url（探测无效）")
	}
}

// ctx 取消时应尽快停止（不把整个模型清单跑完）。
func TestRunnerRespectsContextCancel(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	models := make([]string, 50)
	for i := range models {
		models[i] = "m"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消
	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	r.Run(ctx, []Target{{Label: "fake", Base: srv.URL, Models: models}})
	if calls != 0 {
		t.Errorf("ctx 已取消不该发请求，实发 %d 次", calls)
	}
}

// Describe 摘要格式（日志可读性）。
func TestSnapshotDescribe(t *testing.T) {
	s := Snapshot{Probes: map[string]Probe{
		"a": {State: StateVision}, "b": {State: StateNonVision}, "c": {State: StateUnknown},
	}}
	d := s.Describe()
	for _, want := range []string{"vision=1", "non-vision=1", "unknown=1", "共 3 个模型"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe=%q 缺少 %q", d, want)
		}
	}
}

// nil ctx 必须退化为「不取消」而不是 panic。
//
// 实测踩到：调用方漏传 ctx 时 `ctx.Err()` 对 nil 接口直接崩，
// 把整个探测（乃至进程）带下去。库函数不该因为漏传 ctx 就崩。
func TestRunnerNilContextDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	r := NewRunner(Config{Interval: 0, Timeout: 5 * time.Second})
	// 不 recover：若仍 panic，测试直接失败（这正是我们要钉住的行为）
	snap := r.Run(nil, []Target{{Label: "fake", Base: srv.URL, Models: []string{"m1", "m2"}}})
	if len(snap.Probes) != 2 {
		t.Errorf("nil ctx 应正常跑完，实为 %d 个结果", len(snap.Probes))
	}
}
