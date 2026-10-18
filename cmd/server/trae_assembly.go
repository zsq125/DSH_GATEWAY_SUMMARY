// trae_assembly.go Trae 平台在统一进程里的装配。
//
// 合并前的形态：traeapi2 是**独立进程**（internal/app/app.go），自己加载
// traeapi2-config.json、自己监听 :7862、自己起调度器与 OAuth 回调监听。
//
// 合并后的形态：本文件把那段装配逻辑搬进主进程，但**不放回独立监听**——
// 路由挂到 /trae/ 前缀下（由 platform.Platform.Register 完成），
// 与 WorkBuddy 共用同一个 :7863。
//
// 刻意保留的部分（行为零回归）：
//   - 凭证目录 / 状态文件路径规则（traeauths / data/trae-state.json）
//   - 调度器排程（签到 / 保活 / 余额后台刷新）
//   - OAuth 回调：合并前它另开一个 :18081 监听，因为浏览器回调必须落到
//     一个「不带鉴权、不经过主路由」的地址。合并后回调改为挂在
//     /trae/authorize（主端口的子路径）——**不再需要第二个端口**。
//     见 buildTrae 里的说明。
package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"allplatforms/internal/platform"
	"allplatforms/internal/platform/trae"
	traeauth "allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/scheduler"
	"allplatforms/internal/platform/trae/server"
	"allplatforms/internal/platform/trae/upstream"
)

// traeVersion Trae 平台版本（透出到 /trae/status 与面板）。
const traeVersion = "0.2.0-unified"

// buildTrae 装配 Trae 平台并返回 platform.Platform。
//
// 返回 (nil, nil) 表示 config 里 trae.enabled=false：平台不注册，
// 面板与聚合逻辑自动跳过它（不是错误态）。
func buildTrae(ctx context.Context, cfg *Config) (platform.Platform, func(), error) {
	if !cfg.Trae.Enabled {
		return nil, func() {}, nil
	}

	// 1) 平台自己的配置（复用其 Default/Normalize，保证与独立进程同默认值）。
	tc := cfg.TraeConfigToPlatform()

	// 2) 凭证池。
	auths, err := traeauth.LoadDir(tc.AuthDir)
	if err != nil {
		return nil, nil, err
	}
	log.Printf("[trae] loaded %d account(s) from %s", len(auths), tc.AuthDir)

	tp := pool.New(tc.StateFile)
	tp.SetMaxInFlight(tc.Pool.MaxInFlight)
	tp.SetWeights(tc.Pool.IdleWeightPerHour, tc.Pool.IdleWeightMax)
	tp.SyncToDir(auths)

	// 3) 上游 client。
	up := upstream.New(
		time.Duration(tc.Upstream.TimeoutSeconds)*time.Second,
		time.Duration(tc.Upstream.HeaderTimeoutSeconds)*time.Second,
		time.Duration(tc.Upstream.IdleTimeoutSeconds)*time.Second,
	)
	up.UserAgent = tc.Upstream.UserAgent

	// 4) 调度器。
	sch := scheduler.New(scheduler.Config{
		Pool:              tp,
		Upstream:          up,
		CheckinHours:      tc.Schedule.CheckinHours,
		KeepaliveHours:    tc.Schedule.KeepaliveHours,
		CheckinDisabled:   !tc.Schedule.CheckinEnabled,
		KeepaliveDisabled: !tc.Schedule.KeepaliveEnabled,
		RefreshSkew:       tc.RefreshSkewDur,
		BalanceEnabled:    tc.Schedule.BalanceRefreshEnabled,
		BalanceInterval:   tc.BalanceRefreshInterval,
	})

	// 5) HTTP handler。
	//
	// OAuth 回调路径的处理（关键设计点，2026-10-14 实测修正）：
	//
	// ⚠ 回调 URL 的**路径必须是根路径 /authorize**，不能带平台前缀。
	//
	// 这是上游的硬约束（实测结论，见下方对照）。合并项目时曾把回调改成
	// /trae/authorize（理由是"平台前缀下天然隔离、不需要第二个端口"），
	// 结果登录页直接报「登录失败/网络错误」——而且失败形态极隐蔽：
	// 上游 HTTP 仍返回 200，页面没有任何失败请求，是前端 JS 渲染的错误页。
	//
	// 实测对照（同一浏览器、同一时刻，只改 auth_callback_url 的路径）：
	//
	//	http://127.0.0.1:12588/authorize      → 正常跳转登录页（手机验证码界面）
	//	http://127.0.0.1:17863/authorize      → 正常跳转登录页
	//	http://127.0.0.1:17863/trae/authorize → 「登录失败 / 网络错误，请刷新页面重试」
	//
	// 同时排除了两个干扰项：端口（12588 vs 17863 都行）与 URL 编码
	//（编码/未编码都行）。唯一变量就是路径。
	//
	// 所以：对外回调地址用根路径 /authorize，由 main.go 里的根路径别名
	// 转发到平台内部的 /trae/authorize（见 registerTraeCallbackAlias）。
	callbackPort := gatewayPort(cfg.Listen)
	loginConf := server.LoginSett{
		ClientID:     tc.OAuth.ClientID,
		CallbackPort: callbackPort,
		CallbackPath: traeCallbackPath, // "/authorize"（根路径，上游硬约束）
	}

	h := server.NewHandler(server.Config{
		Pool:     tp,
		Upstream: up,
		PoolCfg: server.PoolOptions{
			MaxRotate:    tc.Pool.MaxRotate,
			PlanCooldown: tc.PlanCooldownDur,
			SoftCooldown: tc.SoftCooldownDur,
			ErrThreshold: tc.Pool.ErrThreshold,
			ErrCooldown:  tc.ErrCooldownDur,
			RefreshSkew:  tc.RefreshSkewDur,
			DefaultModel: tc.Pool.DefaultModel,
		},
		// APIKey：与主网关同口径（面板用同一把 key 访问 /panel 与 /trae/*）。
		// 合并前这里需要单独配一把「服务间密钥」并两边同步；合并后同进程，
		// 沿用主 key 即可，少一套要维护的秘密。
		APIKey:  cfg.APIKey,
		AuthDir: tc.AuthDir,
		Version: traeVersion,
		Sched:   sch,
		Start:   nowFunc(),
		LoginConf: loginConf,
		GetCfg: func() map[string]any {
			return map[string]any{
				"listen":     cfg.Listen,
				"auth_dir":   tc.AuthDir,
				"state_file": tc.StateFile,
				"upstream":   map[string]any{"timeout_seconds": tc.Upstream.TimeoutSeconds},
				"schedule": map[string]any{
					"checkin_hours":     tc.Schedule.CheckinHours,
					"keepalive_hours":   tc.Schedule.KeepaliveHours,
					"checkin_enabled":   tc.Schedule.CheckinEnabled,
					"keepalive_enabled": tc.Schedule.KeepaliveEnabled,
				},
			}
		},
	})

	// 6) 后台任务：调度器 + 余额周期刷新。
	// 用主进程的 ctx：主进程退出时这些协程一并结束。
	go sch.Run(ctx)
	if tc.Schedule.BalanceRefreshEnabled {
		sch.StartBalanceRefresh(ctx, tc.BalanceRefreshInterval)
	}

	pl := trae.New(trae.Options{
		Pool:    tp,
		Sched:   sch,
		Handler: h,
		Prefix:  traePathPrefix,
	})

	// 7) 收尾函数（主进程退出时调用）。
	cleanup := func() {
		tp.Flush()
	}
	return pl, cleanup, nil
}

// ── 路径常量 ────────────────────────────────────────────────────────────
//
// 这些是**下游接入契约**的一部分（DSH 等按路径选平台），
// 刻意不做成配置项：改了会静默打断所有已配置的下游。
const (
	// traePathPrefix Trae 平台的路径前缀。
	traePathPrefix = "/trae"
	// traeCallbackPath OAuth 回调的**对外路径**。
	//
	// ⚠ 必须是根路径 "/authorize"，**不能**加平台前缀 —— 这是上游硬约束，
	// 带前缀会被上游前端判为非法请求（实测，见 buildTrae 里的对照表）。
	//
	// 平台内部的 handler 仍把回调注册在 /trae/authorize（见 trae.go 的 Register），
	// 根路径由 registerTraeCallbackAlias 转发进来。这样既满足上游要求，
	// 又保持"每个平台的路径都在自己前缀下"的内部结构。
	traeCallbackPath = "/authorize"
)

// traeCallbackInternalPath 回调在平台前缀下的内部路径（根别名转发到此）。
const traeCallbackInternalPath = traePathPrefix + "/authorize"

// registerTraeCallbackAlias 把根路径 /authorize 转发到 /trae/authorize。
//
// 为什么需要（上游硬约束 + 内部结构冲突）：
//
//	上游要求 auth_callback_url 的路径必须是 "/authorize"（实测：带平台前缀
//	/trae/authorize 会被判非法，登录页报「登录失败/网络错误」）；
//	而平台路由又都挂在自己的前缀下（/trae/...），根路径默认落到 WorkBuddy。
//
// 做法与 registerRootAlias 同款：注册一个根路径入口，内部重写路径后交回 mux。
// 这样回调实际走的是**平台已注册的那条路由**，与内部路径同一条代码路径，
// 不可能行为漂移。
//
// 只在 Trae 已注册时挂载：没注册就该 404（正确行为）。
func registerTraeCallbackAlias(mux *http.ServeMux, reg *platform.Registry) {
	if reg.Get(platform.KindTrae) == nil {
		return
	}
	// 用 HandleFunc 精确匹配 "/authorize"；带子路径的回调不会出现
	//（上游只回跳这一个地址），故不需要 "/authorize/" 前缀匹配。
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		rewriteAndServe(mux, w, r, traeCallbackInternalPath)
	})
	log.Printf("[trae] OAuth 回调已挂到根路径 /authorize（上游硬约束：不接受带平台前缀的回调路径）")
}

// gatewayPort 从 listen 地址（":7863" / "0.0.0.0:7863"）取出端口字符串。
//
// 用途：生成 OAuth 回调 URL 时，回调地址必须指向**用户浏览器能访问到**的
// 主网关地址。合并前 traeapi2 用自己的 callback_port；合并后回调在主端口上，
// 故从 listen 推端口。异常输入回落 7863（默认端口）。
func gatewayPort(listen string) int {
	s := strings.TrimSpace(listen)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if n, err := parseInt(s[i+1:]); err == nil && n > 0 {
			return n
		}
	}
	return 7863
}

// traeLoginURL 便捷函数：给定 machineID/deviceID 生成 Trae 登录 URL。
//
// 面板的「添加 Trae 账号」按钮最终会拿到这个 URL（经 /panel/api/platforms/trae/login/start）。
// 保留为独立函数便于单测直接校验 URL 形状。
//
// challenge 为 PKCE 的 code_challenge（空则不带 PKCE 参数）。
func traeLoginURL(machineID, deviceID, callbackURL, challenge string) string {
	return server.BuildLoginURL(machineID, deviceID, callbackURL, challenge)
}

// traeCallbackURL 生成对外回调地址（主网关地址 + 根路径回调）。
//
// ⚠ 路径用根路径 traeCallbackPath（"/authorize"），**不带平台前缀** ——
// 上游硬约束，带前缀会导致登录页报「登录失败/网络错误」（实测，见 buildTrae 注释）。
// 根路径由 registerTraeCallbackAlias 转发到平台内部的 /trae/authorize。
func traeCallbackURL(listen string) string {
	return "http://127.0.0.1:" + itoa(gatewayPort(listen)) + traeCallbackPath
}
