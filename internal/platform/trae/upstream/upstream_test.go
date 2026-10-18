package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareBodyBasics(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	out := PrepareBody(src, "glm-5.2")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("改写后不是合法 JSON: %v", err)
	}
	// 出站强制流式（非流式由网关本地聚合）
	if obj["stream"] != true {
		t.Errorf("stream 未强制为 true: %v", obj["stream"])
	}
	if obj["function"] != Function {
		t.Errorf("function 应为 %q，实为 %v", Function, obj["function"])
	}
	// model 同时写进 config_name（上游按 config_name 选档）
	if obj["config_name"] != "glm-5.2" || obj["model"] != "glm-5.2" {
		t.Errorf("model/config_name 未正确设置: %v / %v", obj["model"], obj["config_name"])
	}
	// content 字符串 → 上游的文本块数组
	msgs := obj["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	arr, ok := m0["content"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("content 未转成数组: %#v", m0["content"])
	}
	blk := arr[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "hi" {
		t.Errorf("文本块内容不对: %#v", blk)
	}
}

func TestPrepareBodyEmptyModelFallsBack(t *testing.T) {
	out := PrepareBody([]byte(`{"messages":[]}`), "")
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["config_name"] != DefaultConfigName {
		t.Errorf("空 model 应回落默认 %q，实为 %v", DefaultConfigName, obj["config_name"])
	}
}

func TestPrepareBodyUnparsablePassthrough(t *testing.T) {
	src := []byte(`not-json`)
	if got := PrepareBody(src, "x"); string(got) != string(src) {
		t.Errorf("无法解析的请求体应原样透传，实为 %q", got)
	}
}

// assistant 的 tool_calls 必须转成上游的 function_call 形态，否则上游反序列化失败。
func TestPrepareBodyAssistantToolCalls(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"","arguments":"{}"}}
		]}
	]}`)
	out := PrepareBody(src, "glm-5.2")
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	as := msgs[1].(map[string]any)
	tcs, ok := as["tool_calls"].([]any)
	if !ok {
		t.Fatalf("tool_calls 丢失: %#v", as)
	}
	// 无 name 的条目被剔除（上游要求 name 必填）
	if len(tcs) != 1 {
		t.Fatalf("应只剩 1 条 tool_call，实为 %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if _, has := tc["function"]; has {
		t.Error("OpenAI 的 function 字段应被替换掉")
	}
	fc, ok := tc["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("未转成 function_call: %#v", tc)
	}
	if fc["name"] != "ls" {
		t.Errorf("function_call.name 不对: %v", fc["name"])
	}
}

// tools[].function.parameters 是 object，上游 struct 声明为 string。
func TestPrepareBodyToolsParametersStringified(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[],"tools":[
		{"type":"function","function":{"name":"ls","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}},
		{"type":"function"}
	]}`)
	out := PrepareBody(src, "glm-5.2")
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	list := obj["tools"].([]any)
	// 缺 function 的条目被剔除
	if len(list) != 1 {
		t.Fatalf("应只剩 1 个 tool，实为 %d", len(list))
	}
	fn := list[0].(map[string]any)["function"].(map[string]any)
	s, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 未转成字符串: %#v", fn["parameters"])
	}
	if !strings.Contains(s, `"type":"object"`) {
		t.Errorf("parameters 字符串内容不对: %s", s)
	}
}

func TestToolChoiceNoneSuppressesTools(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[],"tools":[{"type":"function","function":{"name":"ls"}}],"tool_choice":"none"}`)
	out := PrepareBody(src, "glm-5.2")
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if _, has := obj["tool_choice"]; has {
		t.Error("none 应删除 tool_choice")
	}
	if _, has := obj["tools"]; has {
		t.Error("none 应一并删除 tools")
	}
}

func TestToolChoiceObjectNormalized(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"required"}`, "required"},
		{`{"type":"function","function":{"name":"ls"}}`, "ls"},
	}
	for _, c := range cases {
		src := []byte(`{"model":"glm-5.2","messages":[],"tool_choice":` + c.in + `}`)
		out := PrepareBody(src, "glm-5.2")
		var obj map[string]any
		_ = json.Unmarshal(out, &obj)
		if obj["tool_choice"] != c.want {
			t.Errorf("tool_choice %s → %v，期望 %v", c.in, obj["tool_choice"], c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"plan 权益不足", 400, `{"code":1005,"message":"plan limit"}`, ErrPlanLimit},
		{"401 凭证失效", 401, `unauthorized`, ErrSessionDead},
		{"403 凭证失效", 403, `forbidden`, ErrSessionDead},
		{"429 限流", 429, ``, ErrSoftRate},
		{"404", 404, ``, ErrNotFound},
		{"5xx", 500, ``, ErrServer},
		{"其他 4xx", 400, `bad request`, ErrClient},
		{"2xx 成功", 200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d) = %v，期望 %v", c.name, c.status, got, c.want)
		}
	}
}

func TestSOLOStreamErrorKind(t *testing.T) {
	cases := []struct {
		code int64
		want ErrKind
	}{
		{1005, ErrPlanLimit},
		{4008, ErrPlanLimit},
		{4011, ErrSoftRate},
		{9999, ErrClient},
	}
	for _, c := range cases {
		e := &SOLOStreamError{Code: c.code, Msg: "msg"}
		if got := e.Kind(); got != c.want {
			t.Errorf("code=%d → %v，期望 %v", c.code, got, c.want)
		}
		// 错误串要含 code 与 msg（面板直接展示给用户排错）
		s := e.Error()
		if !strings.Contains(s, "code=") || !strings.Contains(s, "msg") {
			t.Errorf("错误串内容不对: %q", s)
		}
	}
}

func TestParseSOLOLine(t *testing.T) {
	ev, err := ParseSOLOLine("output", `{"response":"你好","reasoning_content":"想一想","tool_calls":null}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if ev.Response != "你好" || ev.Reasoning != "想一想" {
		t.Errorf("output 解析不对: %+v", ev)
	}
	ev, err = ParseSOLOLine("done", `{"finish_reason":"stop"}`)
	if err != nil || ev.FinishReason != "stop" {
		t.Errorf("done 解析不对: %v %+v", err, ev)
	}
	ev, err = ParseSOLOLine("error", `{"code":1005,"message":"plan"}`)
	if err != nil || ev.ErrorCode != 1005 {
		t.Errorf("error 解析不对: %v %+v", err, ev)
	}
	// 空 data 不应报错（上游存在 data 为空的事件）
	if _, err := ParseSOLOLine("metadata", ""); err != nil {
		t.Errorf("空 data 不该报错: %v", err)
	}
}

// Aggregate 把整条 SSE 聚合成一个 chat.completion。
func TestAggregate(t *testing.T) {
	sse := "event:metadata\ndata:{\"session_id\":\"s1\"}\n\n" +
		"event:output\ndata:{\"response\":\"你好\"}\n\n" +
		"event:output\ndata:{\"response\":\"世界\",\"reasoning_content\":\"想\"}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	resp, err := Aggregate(strings.NewReader(sse), "glm-5.2")
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	ch := resp["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "你好世界" {
		t.Errorf("内容拼接不对: %q", msg["content"])
	}
	if msg["reasoning_content"] != "想" {
		t.Errorf("思考链不对: %q", msg["reasoning_content"])
	}
	if ch["finish_reason"] != "stop" {
		t.Errorf("finish_reason 不对: %v", ch["finish_reason"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model 未回填: %v", resp["model"])
	}
	u := resp["usage"].(map[string]any)
	if u["total_tokens"] != float64(3) {
		t.Errorf("usage 不对: %#v", u)
	}
}

// 流内 error 事件必须让 Aggregate 返回错误（调用方据此冷却账号并换号）。
func TestAggregateUpstreamError(t *testing.T) {
	sse := "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n"
	_, err := Aggregate(strings.NewReader(sse), "glm-5.2")
	if err == nil {
		t.Fatal("流内 error 应返回错误")
	}
	var se *SOLOStreamError
	if !errorsAs(err, &se) || se.Code != 1005 {
		t.Errorf("错误类型/码不对: %v", err)
	}
}

// errorsAs 小工具（避免为一行引 stderrors 之外的依赖）。
func errorsAs(err error, target **SOLOStreamError) bool {
	if e, ok := err.(*SOLOStreamError); ok {
		*target = e
		return true
	}
	return false
}

// 上游中断（无 done）时聚合仍应产出已收到的内容，而不是报错。
func TestAggregateNoDoneStillYieldsContent(t *testing.T) {
	sse := "event:output\ndata:{\"response\":\"半句\"}\n\n"
	resp, err := Aggregate(strings.NewReader(sse), "glm-5.2")
	if err != nil {
		t.Fatalf("上游无 done 不该报错: %v", err)
	}
	ch := resp["choices"].([]any)[0].(map[string]any)
	if ch["message"].(map[string]any)["content"] != "半句" {
		t.Errorf("内容不对: %#v", ch["message"])
	}
}

func TestMergeToolCallDelta(t *testing.T) {
	merged := map[string]any{}
	mergeToolCallDelta(merged, map[string]any{"id": "c1", "type": "function", "function_call": map[string]any{"name": "ls", "arguments": `{"a`}})
	mergeToolCallDelta(merged, map[string]any{"function_call": map[string]any{"arguments": `":1}`}})
	// 上游用 function_call 字段名，输出必须归一到 OpenAI 的 function
	fn, ok := merged["function"].(map[string]any)
	if !ok {
		t.Fatalf("未归一到 function: %#v", merged)
	}
	if fn["name"] != "ls" {
		t.Errorf("name 不对: %v", fn["name"])
	}
	if fn["arguments"] != `{"a":1}` {
		t.Errorf("arguments 拼接不对: %v", fn["arguments"])
	}
	if merged["id"] != "c1" || merged["type"] != "function" {
		t.Errorf("id/type 未保留: %#v", merged)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("abcdef", 3); got != "abc" {
		t.Errorf("截断不对: %q", got)
	}
	if got := Truncate("ab", 10); got != "ab" {
		t.Errorf("短串不该变: %q", got)
	}
}

func TestNormalizeExpiresAt(t *testing.T) {
	// 毫秒 → 秒
	if got := normalizeExpiresAt(1786847930141); got != 1786847930 {
		t.Errorf("毫秒归一化不对: %d", got)
	}
	// 已是秒则原样
	if got := normalizeExpiresAt(1786847930); got != 1786847930 {
		t.Errorf("秒被误改: %d", got)
	}
}
