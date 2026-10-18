// rootalias.go 根路径的兼容入口。
//
// 背景（为什么需要这个）：合并前 WorkBuddy 独占根路径（/v1/...、/panel/...），
// DSH 等下游就是按 http://127.0.0.1:7863/v1 配置的。合并后我们引入了
// /workbuddy/v1 与 /trae/v1 两个**平台前缀**路径，但**不能**顺手把根路径
// 删掉——那会静默打断所有既有下游配置（用户只会看到 404，不会知道要改配置）。
//
// 做法：根路径保留为 WorkBuddy 的别名，并显式标注「已弃用」。
//   - http://127.0.0.1:7863/v1          → WorkBuddy（兼容旧配置，仍可用）
//   - http://127.0.0.1:7863/workbuddy/v1 → WorkBuddy（推荐，路径明确）
//   - http://127.0.0.1:7863/trae/v1      → Trae
//
// 这样「只对接 7863 一个端口、用路径区分平台」的诉求达成，
// 同时老配置零改动继续工作。
package main

import (
	"log"
	"net/http"

	"allplatforms/internal/platform"
)

// registerRootAlias 把根路径下的 OpenAI 兼容面别名指向 WorkBuddy 平台。
//
// 只在 WorkBuddy 已注册时挂载：若它没注册，根路径留给 catch-all handler
//（那会返回 404，是正确行为——没有平台就不该有别名）。
func registerRootAlias(mux *http.ServeMux, reg *platform.Registry, listen string) {
	wb := reg.Get(platform.KindWorkBuddy)
	if wb == nil {
		return
	}
	// 根路径的 /v1/* 与 /status、/healthz 指向 WorkBuddy 平台。
	//
	// 实现方式：借 platform.Platform 的 Register 无法做到（它固定挂前缀），
	// 故这里直接复用 WorkBuddy 平台对象自己的 handler——
	// 但 Platform 接口不暴露 handler。因此改用「注册一个零长度前缀的别名平台」
	// 不现实（会与真平台冲突）。
	//
	// 最干净的做法：在 Registry 之外，让根 mux 把 /v1/ 与 /status 转发给
	// WorkBuddy 平台已挂载的前缀树——即内部重写路径再交给 mux 自己。
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		rewriteAndServe(mux, w, r, wb.ModelsPath()+trimV1(r.URL.Path))
	})
	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		rewriteAndServe(mux, w, r, wb.ModelsPath())
	})
	// /status 与 /healthz 是 WorkBuddy 的观测端点，同样保留根路径别名。
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		rewriteAndServe(mux, w, r, "/"+string(platform.KindWorkBuddy)+"/status")
	})
	log.Printf("[gateway] 根路径 /v1 保留为 WorkBuddy 别名（兼容旧下游配置）；"+
		"推荐改用 %s（WorkBuddy）与 /trae/v1（Trae）", wb.ModelsPath())
}

// trimV1 从 "/v1/chat/completions" 取出 "/chat/completions"。
func trimV1(p string) string {
	const v1 = "/v1"
	if len(p) >= len(v1) && p[:len(v1)] == v1 {
		return p[len(v1):]
	}
	return p
}

// rewriteAndServe 把请求路径改写成目标路径后，再交给同一个 mux 处理。
//
// 为什么要「再交给 mux」而不是直接调平台的 handler：
// 平台的 handler 是私有字段（Platform 接口不暴露），直接调会破坏抽象边界。
// 交给 mux 则走的是**已注册的平台路由**，与下游直连 /workbuddy/v1 完全同一条
// 代码路径——别名与正式路径不可能行为漂移。
func rewriteAndServe(mux *http.ServeMux, w http.ResponseWriter, r *http.Request, target string) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = target
	r2.RequestURI = target
	mux.ServeHTTP(w, r2)
}
