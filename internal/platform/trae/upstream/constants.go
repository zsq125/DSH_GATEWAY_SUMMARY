// Package upstream Trae SOLO 上游客户端：常量、头族、HTTP 调用、错误分类。
package upstream

// 上游技术常量（实测，勿随意改动）。
const (
	// 三类 Host：对话/模型走 agent，签到/额度走 ug，换 token 与用户信息走 oauth。
	AgentHost = "https://trae-api-cn.mchost.guru"
	UgHost    = "https://api.trae.cn"
	OAuthHost = "https://api.trae.com.cn"
	// ConsoleHost 登录页 https://www.trae.cn/authorization。
	ConsoleHost = "https://www.trae.cn"

	ClientID = "en1oxy7wnw8j9n"

	// PluginVersion 登录 URL 的 plugin_version 取值。
	//
	// 来源：官方客户端 product.json 的 tronBuildVersion（本机 TRAE SOLO CN
	// 实测值），与官方运行时日志里打开的登录 URL 完全一致。
	//
	// 只用于 BuildLoginURL —— **不要**拿它替换 IdeVersion：
	// IdeVersion 决定出站请求头 X-Ide-Version / X-App-Version，动它会波及
	// 对话、签到、额度等全部上游调用，需要重新验证（见下方注释）。
	PluginVersion = "2.3.91089"

	// SOLO 客户端指纹常量（与官方同款，上游按此归因用量与风控）。
	//
	// 注意 IdeVersion 与官方当前版本（0.2.2）不一致，但**刻意保持不动**：
	// 它是出站请求头的一部分，改它等于换了一整套上游调用指纹，
	// 会波及对话/签到/额度全部路径，需要逐条重新验证后再动。
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "Apple"
	OSVersion      = "macOS 15.7.4"
	Function       = "solo_work_lite"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	// 签到业务码：10001=今日已签到（幂等成功，不报错）；
	// 9074=参与用户太多/服务器繁忙（限流），需退避重试；
	// 其他非 0 码视为失败。
	CheckinCodeAlready = 10001
	CheckinCodeBusy    = 9074

	// DefaultConfigName 缺省模型（config_name 口径）。
	DefaultConfigName = "glm-5.2"

	// DefaultContextWindow /v1/models 兜底上下文窗口（1M）。
	// 上游 get_detail_param 未稳定透出真实 context_window 字段（字段名未确认），
	// 故统一公告 1M 兜底，避免下游客户端（DSH/Codex 等）被错误引导在 128K 早缩。
	// 与 workbuddy panel-main context_catalog.go 的 DefaultContextWindow 同策略。
	DefaultContextWindow = 1000000
)

// ---------------------------------------------------------------------------
// 真实上下文窗口覆盖表（2026-10 实测修复）
//
// 背景：DefaultContextWindow(1M) 是给「未知窗口」模型的兜底，但把它套在
// 只有 128K 窗口的 DeepSeek-V4-Flash 上会致命高估——Trae 从 /v1/models
// 读到 1M 后一路堆上下文，真实窗口(128K)撑爆，上游直接回
// CONTEXT_WINDOW_EXCEEDED（表现：聊两轮就报
// "pi-ai detected context overflow for model deepseek-v4.1-flash"）。
//
// 原则：公告值必须 <= 真实窗口。宁可让下游早一点压缩，也不能让它溢出。
// 只登记已确认真实窗口的模型；未登记的继续走 1M 兜底（不擅自收窄）。
// ---------------------------------------------------------------------------
var contextWindowOverride = map[string]int64{
	// DeepSeek V4.1 系列真实窗口 128K（联网核对结论）。
	"DeepSeek-V4-Flash": 128000,
}

// ContextWindowForID 返回该模型应公告的上下文窗口：命中覆盖表用真实值，
// 否则回退 DefaultContextWindow。/v1/models 统一走这里，避免把小窗口模型
// 硬编码公告成 1M。
func ContextWindowForID(id string) int64 {
	if v, ok := contextWindowOverride[id]; ok && v > 0 {
		return v
	}
	return DefaultContextWindow
}
