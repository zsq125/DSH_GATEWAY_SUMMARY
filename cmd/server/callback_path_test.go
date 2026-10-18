// callback_path_test.go 钉住 Trae OAuth 回调路径必须是**根路径** /authorize。
//
// ── 回归背景（2026-10-14 实测踩坑）────────────────────────────────────
//
// 合并项目时把回调路径改成了 /trae/authorize（当时理由是"平台前缀下天然
// 隔离、不需要第二个端口"）。结果登录页直接报「登录失败 / 网络错误，
// 请刷新页面重试」——而且失败形态**极其隐蔽**：
//
//   - 上游 HTTP 仍返回 200 + 正常 HTML（靠状态码判断不出来）
//   - 页面**没有任何失败的网络请求**（靠抓包也看不出来）
//   - 是上游前端 JS 检测到参数非法后主动渲染的错误页
//
// 排查过程：从官方客户端日志里拿到官方自己用的 URL（auth_callback_url=
// http://127.0.0.1:<port>/authorize），再逐项做对照实验，最终定位到
// **唯一变量是回调路径**：
//
//	http://127.0.0.1:12588/authorize       → 正常跳登录页
//	http://127.0.0.1:17863/authorize       → 正常跳登录页
//	http://127.0.0.1:17863/trae/authorize  → 「登录失败/网络错误」
//
// 同时排除了两个干扰项：端口（12588 vs 17863 都行）、URL 编码（都行）。
//
// 之所以必须用测试锁死：这个约束在上游是**隐式**的（没有任何文档），
// 而"给回调加平台前缀"看起来是完全合理的重构——不加测试，
// 下次重构还会踩同一个坑，且症状同样难以定位。
package main

import (
	"strings"
	"testing"
)

// TestTraeCallbackURLUsesRootPath 对外回调地址必须是根路径 /authorize。
func TestTraeCallbackURLUsesRootPath(t *testing.T) {
	got := traeCallbackURL(":7863")
	want := "http://127.0.0.1:7863/authorize"
	if got != want {
		t.Errorf("回调地址应为根路径：\n  want %s\n  got  %s", want, got)
	}
}

// TestTraeCallbackURLHasNoPlatformPrefix 回调地址**不得**带平台前缀。
//
// 这是本文件的核心断言：带前缀会被上游判为非法请求。
func TestTraeCallbackURLHasNoPlatformPrefix(t *testing.T) {
	got := traeCallbackURL(":7863")
	if strings.Contains(got, traePathPrefix) {
		t.Errorf("回调地址不得包含平台前缀 %q（上游会拒绝）: %s", traePathPrefix, got)
	}
	if strings.Contains(got, "/trae/authorize") {
		t.Errorf("回调地址不得是 /trae/authorize（上游会报「登录失败/网络错误」）: %s", got)
	}
}

// TestTraeCallbackPathConstantIsRoot 常量本身也必须是根路径。
func TestTraeCallbackPathConstantIsRoot(t *testing.T) {
	if traeCallbackPath != "/authorize" {
		t.Errorf("traeCallbackPath 必须是 /authorize，实为 %q", traeCallbackPath)
	}
}

// TestTraeCallbackInternalPathHasPrefix 内部路径（根别名转发目标）带前缀。
//
// 内部路径与对外路径**刻意不同**：对外必须是根路径（上游硬约束），
// 内部仍挂在平台前缀下（保持"每平台路径在自己前缀下"的结构）。
// 两者由 registerTraeCallbackAlias 桥接。
func TestTraeCallbackInternalPathHasPrefix(t *testing.T) {
	want := "/trae/authorize"
	if traeCallbackInternalPath != want {
		t.Errorf("内部回调路径应为 %s，实为 %q", want, traeCallbackInternalPath)
	}
	if !strings.HasPrefix(traeCallbackInternalPath, traePathPrefix) {
		t.Errorf("内部回调路径应在平台前缀 %q 下: %s", traePathPrefix, traeCallbackInternalPath)
	}
}

// TestGatewayPortParsing 回调端口从 listen 推导（回归：别把端口也改坏）。
func TestGatewayPortParsing(t *testing.T) {
	cases := map[string]int{
		":7863":          7863,
		"0.0.0.0:7863":   7863,
		"127.0.0.1:9000": 9000,
		"":               7863, // 异常输入回落默认端口
	}
	for in, want := range cases {
		if got := gatewayPort(in); got != want {
			t.Errorf("gatewayPort(%q) = %d，期望 %d", in, got, want)
		}
	}
}
