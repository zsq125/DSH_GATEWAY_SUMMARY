// exports.go 对既有 Trae handler 的**纯增量**补充：把「按路径派发一次请求」
// 暴露成可编程方法，供上层（平台适配层 / 面板）在**进程内**复用既有全部
// 管理端点，而不必重写一遍业务逻辑。
//
// 为什么这么做（而不是把每个端点都改写成 (any, error) 形态）：
//   - handler.go / login.go 里的端点已经承载了完整的校验、鉴权、
//     状态机与错误语义，且被 1000+ 行既有测试覆盖。逐端点改写 =
//     把已测代码重新实现一遍，等于主动制造回归。
//   - 进程内派发用 httptest.ResponseRecorder 承接响应，零网络开销、
//     零端口占用，但走的是**同一条**代码路径。
//
// 安全说明：本方法仅供进程内调用（不监听端口）。调用方必须先完成
// 自己的鉴权——它是「已授权之后的内部派发」，不替代鉴权。
package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
)

// Dispatch 在进程内按路径派发一次请求，返回状态码与响应体。
//
// method 形如 "GET"/"POST"；path 是**剥掉平台前缀后**的相对路径
// （如 "/api/accounts" 或 "/v1/models"），与 routes() 注册的
// pattern 完全一致。query 可为 nil。body 可为 nil。
//
// 注意：本方法**不做鉴权**（Handler.cfg.APIKey 仍在，withAuth 会检查）。
// 若调用方需要免鉴权（同进程内已授权），传 apiKey 参数即可。
func (h *Handler) Dispatch(method, path string, query url.Values, body []byte, apiKey string) *httptest.ResponseRecorder {
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	w := httptest.NewRecorder()
	// 走 Handler 自己的 ServeHTTP（含安全头与 CORS 预检），保证与真实
	// HTTP 路径行为一致——绕过它会漏掉安全头，面板拿到的响应就与直连不同。
	h.ServeHTTP(w, r)
	return w
}

// IsValidUID 暴露既有 uid 校验（面板与适配层共用同一口径，
// 避免出现第二套不一致的规则）。
func (h *Handler) IsValidUID(uid string) bool { return h.isValidUID(uid) }

// CallbackPath 该 handler 配置的 OAuth 回调路径。
func (h *Handler) CallbackPath() string { return h.cfg.LoginConf.CallbackPath }

// KeyEmpty 该 handler 是否未设 api_key（空 = 不鉴权）。
func (h *Handler) KeyEmpty() bool { return h.cfg.APIKey == "" }

// ServiceKey 返回该 handler 的 api_key，供**同进程内**派发使用
// （进程内调用已无网络边界，但仍要过 withAuth，故回填自己的 key）。
func (h *Handler) ServiceKey() string { return h.cfg.APIKey }

// 确保 net/http 引用（Dispatch 的返回类型是 httptest，http 用于 Header）。
var _ = http.MethodGet
