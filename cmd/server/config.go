// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	traecfg "allplatforms/internal/platform/trae/config"
	"allplatforms/internal/platform/workbuddy/prompt"
)

// 保活自动化在**配置层**的默认值（2026-10-23）。
//
// 为什么在配置层再定义一份、而不直接依赖调度器默认：
// 配置层的「空字符串」必须解析成一个**具体 Duration** 才能传给调度器，
// 而调度器层的 0 是「禁用」语义。两者混用会让「没写配置」变成「静默禁用」
// （实测踩过：config 里没有 keepalive_interval 键 → 间隔循环根本没启动）。
//
// 数值与 scheduler.defaultKeepalive* 保持一致；有测试钉住两者相等。
const (
	defaultKeepaliveIntervalCfg     = 4 * time.Hour
	defaultKeepaliveVerifyWithinCfg = 7 * 24 * time.Hour
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Panel struct {
		// PackageDetailLimit 积分构成页单账号默认展示的最近到期包数；<=0 回落 5。
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Logging struct {
		// RequestArchiveEnabled 请求元数据 JSONL 归档开关，缺省 true。
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		// RequestRetentionDays 归档保留天数，缺省 7；<=0 回落默认。
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB 归档总上限（MiB），缺省 100；<=0 回落默认。
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
		// RequestClientInfo 是否在请求日志（归档事件 + stdout 流水行 + 面板运行
		// 日志）里记录调用来源：客户端 IP 与 User-Agent。缺省 true。
		//
		// 为什么做成开关而不是恒开：来源信息是排查"谁在打网关"的第一手线索，
		// 但它比 token 计数敏感（IP 属个人信息），共享部署/多租户场景可能需要
		// 关掉。关闭后 Event.ClientIP/UserAgent 保持为空，归档里不出现该字段。
		// 热生效（经 livecfg 快照），无需重启。
		RequestClientInfo bool `json:"request_client_info"`
	} `json:"logging"`

	Server struct {
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（issue #100）。
		// http.Server 的 ReadTimeout 覆盖整个请求读取：大上下文/文件块请求经
		// 反代链转发时上传可超过旧固定值 60s，被掐后客户端拿到
		// 400 "read body: ... i/o timeout"。缺省 "300s"；"0" = 不限制
		//（慢速 body 可无限占用连接，自担风险）；改动需重启进程。
		ReadTimeout string `json:"read_timeout"` // "300s"；"0" = 不限制
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		GrowthHours    []int `json:"growth_hours"`    // [1] 成长任务队列（Sequential 族每日零点解锁，01:00 自动扫描执行）

		// ── 保活自动化（2026-10-23 新增，四层防护）──────────────────────
		//
		// 事故背景：refreshToken 被上游作废后，因 accessToken 仍有效，
		// 对话照常成功 → 用户完全无感；直到 accessToken 也失效才突然 503。
		// 原设计保活每天只跑 1 次（22:00），最长 24 小时才发现账号已死。
		//
		// 以下四个参数把这件事自动化，无需人工干预：
		//
		//	keepalive_interval       Layer 1：固定间隔循环（默认 "4h"）
		//	keepalive_retry          Layer 3：非确定性失败重试（默认 [30,120,600] 秒）
		//	keepalive_verify_within  Layer 4：提前验证窗口（默认 "168h" = 7 天）
		//	（Layer 2 的即时告警不需要配置，12153 一律立即写入账号状态）
		//
		// 全部可留空回落默认；显式 "0"/"0s" 可关闭对应行为。
		KeepaliveInterval string `json:"keepalive_interval"` // 默认 "4h"；"0"=关闭间隔循环
		// KeepaliveRetry 重试间隔（秒）。空=默认 [30,120,600]；[]=不重试。
		// 只对非确定性失败重试；12153（refreshToken 真失效）不重试。
		KeepaliveRetry []int `json:"keepalive_retry"`
		// KeepaliveVerifyWithin accessToken 剩余寿命小于该值时保活主动刷新。
		// 默认 "168h"（7 天）：保证 refreshToken 至少每周被验证一次。
		KeepaliveVerifyWithin string `json:"keepalive_verify_within"`
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子
		GrowthEnabled    bool `json:"growth_enabled"`    // 缺省 true；false = 关成长任务自动排程

		// IncludeDisabledInTasks 让「保号类」定时任务（签到 / 活跃上报 / token 保活 /
		// 余额刷新）对**已禁用（disabled）**的账号也执行。
		//
		// 为什么需要它：面板「禁用」的语义是「不再参与选号」（见面板确认文案），但这四类
		// 任务此前一律 `if st.Disabled { continue }`，等于把「停用流量」放大成「停止一切
		// 上游保号行为」——被禁用的号拿不到签到积分、不续 token、余额也不再刷新；而
		// ReenableIfCredits 明确不复活 disabled 账号（见 pool.state.go），于是签到这条唯一
		// 的自动回血路径也断了，账号只能靠人工「解冻」回来。
		//
		// 对「一次只放开一个号、用禁用做流量开关」的轮换用法（同 IP 多号防风控），闲置
		// 待命的号恰恰是最需要签到的那批——本开关即为该用法提供出口。
		//
		// 缺省 false = 保持既有行为，对老配置零影响。打开后禁用号仍会签到 / 保活，但
		// **依旧不参与选号**：pool 选号侧的 disabled 过滤不受本开关影响。
		IncludeDisabledInTasks bool `json:"include_disabled_in_tasks"`

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5
	} `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// 默认值已对齐官方 WorkBuddy 桌面形态（三段式），用户仍可配完全自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与归属头
		// X-IDE-Version）。空 = 内置默认（对齐官方 5.5.4 分发包）。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（官方内置 CLI 2.137.1）。
		CliVersion string `json:"cli_version"`
		// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	// Ratedash 模型费率 Dashboard 综合评分权重（仅前端展示用，不影响路由/计费）。
	// 三项加权求和（已各自归一化到 0~1）：费率比(rate) / 长度归一(len) / 能力(cap)。
	// 任一配 <=0 时回落默认 0.4/0.3/0.3；保存即生效，前端 Rate Dashboard 立即重算。
	Ratedash struct {
		RateW float64 `json:"rate_w"` // 费率比权重（默认 0.4）
		LenW  float64 `json:"len_w"`  // 长度归一权重（默认 0.3）
		CapW  float64 `json:"cap_w"`  // 能力权重（默认 0.3）
	} `json:"ratedash"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "passthrough" / "custom" / "append"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 风控紧域压低并发），0 = 回落默认 2
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114）：ErrClient/传输层这类「不罚号」失败连续计数，达阈
		// 临时出池。与冷却/熔断并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// PreferExpiring 快过期积分加权开关，默认 true。开启且 expiring_soon 窗口内
		// 存在有效批次时，该账号选号权重 ×3（虚拟实例，见 pool 路由加权）；
		// 不按到期时间排序、与批次金额无关（issue #101 对齐实现口径）。
		// 关闭后完全不使用到期信息选号。
		PreferExpiring bool `json:"prefer_expiring"`
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的批次令账号命中上述 ×3 加权；窗口开大 → 命中账号变多、
		// 偏好被稀释。空/0 = 禁用该加权门槛。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloor 积分保底：账号余额低于该值时，对实测收费模型（tier 2）不再
		// 参与选号——防止收费请求把余额打穿、连免费模型都 402 冷却到次日签到。
		// tier 0（免费）/ tier 1（无观测）不受限；签到回血越过 floor 自动恢复。
		// 默认 0 = 关闭；负值钳 0。
		CreditFloor int64 `json:"credit_floor"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// Trae 平台配置段。
	//
	// 变化说明（合并前 → 合并后）：
	//   - 合并前这是「面板 → 独立 traeapi2 进程」的接线参数（base_url / api_key /
	//     timeout_seconds），因为 Trae 是另一个进程、另一个端口（:7862）。
	//   - 合并后 Trae 在同进程内，**不再需要 base_url 与 api_key**：
	//     没有网络边界，面板直接调对象方法；多留这两个键只会多一套要同步的
	//     密钥与一个会过期的地址。
	//   - 保留 `enabled`（可整体关掉 Trae 平台，不注册它的路由）与
	//     平台自己的运行参数（auth_dir / state_file / schedule / pool / oauth）。
	//
	// 路径前缀刻意**不做成配置项**：它是下游（DSH 等）接入契约的一部分，
	// 改了会静默打断所有已配置的下游。固定为 /trae。
	Trae TraeConfig `json:"trae"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	ExpiringSoonDur        time.Duration `json:"-"`
	// KeepaliveIntervalDur 解析后的保活固定间隔（Layer 1）；0 = 不启动间隔循环。
	KeepaliveIntervalDur time.Duration `json:"-"`
	// KeepaliveRetryDurs 解析后的保活重试间隔（Layer 3）；空 = 不重试。
	KeepaliveRetryDurs []time.Duration `json:"-"`
	// KeepaliveVerifyWithinDur 解析后的提前验证窗口（Layer 4）；0 = 用调度器默认。
	KeepaliveVerifyWithinDur time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
	// ServerReadTimeoutDur 解析后的入站请求读取上限（issue #100）；0 = 不限制。
	ServerReadTimeoutDur time.Duration `json:"-"`
}

// TraeConfig Trae 平台在统一配置里的段落。
//
// 结构上刻意「扁平地摊开 Trae 自己的键」，而不是嵌一层 trae.raw：
// 用户在 config.json 里手改 Trae 的签到时间时，路径应当一眼可读
//（trae.schedule.checkin_hours），而不是 trae.config.schedule.checkin_hours。
type TraeConfig struct {
	// Enabled 是否启用 Trae 平台。false = 不注册 /trae/ 路由、
	// 不启动其调度器、面板隐藏该模块（不是错误状态）。
	Enabled bool `json:"enabled"`

	// AuthDir Trae 凭证目录（存放 trae-<uid>.json）。空 = ./traeauths。
	AuthDir string `json:"auth_dir"`
	// StateFile Trae 池状态文件。空 = ./data/trae-state.json。
	StateFile string `json:"state_file"`

	Upstream struct {
		BaseURL              string `json:"base_url"`
		APIKey               string `json:"api_key"`
		TimeoutSeconds       int    `json:"timeout_seconds"`
		HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
		IdleTimeoutSeconds   int    `json:"idle_timeout_seconds"`
		UserAgent            string `json:"user_agent"`
	} `json:"upstream"`

	OAuth struct {
		BaseURL      string `json:"base_url"`
		ClientID     string `json:"client_id"`
		CallbackURL  string `json:"callback_url"`
		CallbackPort int    `json:"callback_port"`
	} `json:"oauth"`

	Features struct {
		AutoRefresh bool `json:"auto_refresh"`
	} `json:"features"`

	Pool struct {
		MaxInFlight       int     `json:"max_in_flight"`
		PlanCooldown      string  `json:"plan_cooldown"`
		SoftCooldown      string  `json:"soft_cooldown"`
		ErrThreshold      int     `json:"err_threshold"`
		ErrCooldown       string  `json:"err_cooldown"`
		RefreshSkew       string  `json:"refresh_skew"`
		DefaultModel      string  `json:"default_model"`
		IdleWeightPerHour float64 `json:"idle_weight_per_hour"`
		IdleWeightMax     float64 `json:"idle_weight_max"`
		MaxRotate         int     `json:"max_rotate"`
	} `json:"pool"`

	Schedule struct {
		CheckinHours          []int `json:"checkin_hours"`
		KeepaliveHours        []int `json:"keepalive_hours"`
		CheckinEnabled        bool  `json:"checkin_enabled"`
		KeepaliveEnabled      bool  `json:"keepalive_enabled"`
		BalanceRefreshEnabled bool  `json:"balance_refresh_enabled"`
		BalanceRefreshMinutes int   `json:"balance_refresh_minutes"`
	} `json:"schedule"`

	Logging struct {
		Level string `json:"level"`
	} `json:"logging"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.ReadTimeout = "300s"
	c.Panel.PackageDetailLimit = 5
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// 缺省 true 靠显式赋值实现（同 Schedule 开关）：JSON 里键缺席时字段保留此值，
	// 只有显式 false 才关闭来源记录。
	c.Logging.RequestClientInfo = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：透传客户端原始 system（对齐上游；custom 由用户显式选择）
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403 修复
	// P1-1）；0/负数 normalize 回落默认（与 max_in_flight 的 0=不限语义不同，分档键
	// 的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	// Trae 平台缺省启用：合并后它就在同进程内，不存在「后端没起」的中间态。
	// 默认路径与合并前的独立进程保持一致（traeauths / data/trae-state.json），
	// 这样从旧部署迁移过来时不用搬数据文件。
	c.Trae.Enabled = true
	c.Trae.AuthDir = "./traeauths"
	c.Trae.StateFile = "./data/trae-state.json"
	c.Trae.Upstream.TimeoutSeconds = 120
	c.Trae.Pool.MaxInFlight = 3
	c.Trae.Pool.PlanCooldown = "12h"
	c.Trae.Pool.SoftCooldown = "60s"
	c.Trae.Pool.ErrThreshold = 3
	c.Trae.Pool.ErrCooldown = "10m"
	c.Trae.Pool.RefreshSkew = "24h"
	c.Trae.Pool.DefaultModel = "glm-5.2"
	c.Trae.Pool.IdleWeightPerHour = 0.5
	c.Trae.Pool.IdleWeightMax = 5.0
	c.Trae.Pool.MaxRotate = 3
	c.Trae.Schedule.CheckinHours = []int{9, 21}
	c.Trae.Schedule.KeepaliveHours = []int{22}
	c.Trae.Schedule.CheckinEnabled = true
	c.Trae.Schedule.KeepaliveEnabled = true
	c.Trae.Schedule.BalanceRefreshEnabled = true
	c.Trae.Schedule.BalanceRefreshMinutes = 30
	c.Trae.OAuth.CallbackPort = 0 // 0 = 不另开监听（见下方 normalize 注释）
	c.Trae.OAuth.ClientID = "en1oxy7wnw8j9n"
	c.Trae.OAuth.BaseURL = "https://api.trae.com.cn"
	c.Trae.Logging.Level = "info"
	return c
}

// TraeConfigToPlatform 把顶层 trae 段转成 Trae 平台自己的 Config 类型。
//
// 为什么要这一层转换而不是让两边共用同一个结构体：
//   - Trae 的 config 包是从独立进程原样移植的，它自己的 Default()/normalize()
//     承载了该平台的默认值与校验（比如时长字符串解析、小时范围校验）。
//     让它继续拥有这套逻辑，就能保证「统一进程」与「独立进程」行为一致。
//   - 顶层 Config 只负责 JSON 解析与拼装，不复制 Trae 的业务默认值——
//     两处各写一套默认值必然漂移。
func (c *Config) TraeConfigToPlatform() *traecfg.Config {
	t := traecfg.Default()
	t.APIKey = c.APIKey // 管理面鉴权与主网关同口径（面板用同一把 key）
	t.AuthDir = c.Trae.AuthDir
	t.StateFile = c.Trae.StateFile

	t.Upstream.BaseURL = c.Trae.Upstream.BaseURL
	t.Upstream.APIKey = c.Trae.Upstream.APIKey
	t.Upstream.TimeoutSeconds = c.Trae.Upstream.TimeoutSeconds
	t.Upstream.HeaderTimeoutSeconds = c.Trae.Upstream.HeaderTimeoutSeconds
	t.Upstream.IdleTimeoutSeconds = c.Trae.Upstream.IdleTimeoutSeconds
	t.Upstream.UserAgent = c.Trae.Upstream.UserAgent

	t.OAuth.BaseURL = c.Trae.OAuth.BaseURL
	t.OAuth.ClientID = c.Trae.OAuth.ClientID
	t.OAuth.CallbackURL = c.Trae.OAuth.CallbackURL
	t.OAuth.CallbackPort = c.Trae.OAuth.CallbackPort

	t.Features.AutoRefresh = c.Trae.Features.AutoRefresh

	t.Pool.MaxInFlight = c.Trae.Pool.MaxInFlight
	t.Pool.PlanCooldown = c.Trae.Pool.PlanCooldown
	t.Pool.SoftCooldown = c.Trae.Pool.SoftCooldown
	t.Pool.ErrThreshold = c.Trae.Pool.ErrThreshold
	t.Pool.ErrCooldown = c.Trae.Pool.ErrCooldown
	t.Pool.RefreshSkew = c.Trae.Pool.RefreshSkew
	t.Pool.DefaultModel = c.Trae.Pool.DefaultModel
	t.Pool.IdleWeightPerHour = c.Trae.Pool.IdleWeightPerHour
	t.Pool.IdleWeightMax = c.Trae.Pool.IdleWeightMax
	t.Pool.MaxRotate = c.Trae.Pool.MaxRotate

	t.Schedule.CheckinHours = c.Trae.Schedule.CheckinHours
	t.Schedule.KeepaliveHours = c.Trae.Schedule.KeepaliveHours
	t.Schedule.CheckinEnabled = c.Trae.Schedule.CheckinEnabled
	t.Schedule.KeepaliveEnabled = c.Trae.Schedule.KeepaliveEnabled
	t.Schedule.BalanceRefreshEnabled = c.Trae.Schedule.BalanceRefreshEnabled
	t.Schedule.BalanceRefreshMinutes = c.Trae.Schedule.BalanceRefreshMinutes

	t.Logging.Level = c.Trae.Logging.Level

	// 让既有的 TRA2A_* 环境变量继续生效（部署脚本兼容）。
	t.ApplyEnv()
	_ = t.Normalize()
	return t
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 或删除该目录（程序会自动生成配置）", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(stripBOM(raw), c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// stripBOM 去掉 UTF-8 BOM（EF BB BF）。
//
// 为什么需要：Windows 上最常见的两个写文件方式是记事本「另存为 UTF-8」与
// PowerShell 的 `Set-Content -Encoding UTF8`，**两者都会写入 BOM**。
// encoding/json 不接受前导 BOM，会报一句很难懂的
// "invalid character '\ufeff' looking for beginning of value"——
// 用户拿着自己刚手改过的 config.json 完全看不出哪里错了。
//
// 容忍 BOM 的代价是零（BOM 在 JSON 里没有任何合法含义），
// 收益是「手改配置不会莫名启动失败」。
func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_PREFER_EXPIRING"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pool.PreferExpiring = b
		}
	}
	if v := os.Getenv("WB2A_TRAE_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Trae.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_TRAE_AUTH_DIR"); v != "" {
		c.Trae.AuthDir = v
	}
	if v := os.Getenv("WB2A_TRAE_STATE_FILE"); v != "" {
		c.Trae.StateFile = v
	}
}

func (c *Config) normalize() error {
	var err error
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	// 入站读取上限（issue #100）：空值回落默认 300s；"0" 合法（不限制）；
	// 负值无语义，fail fast（静默钳 0 会把保护悄悄关掉）。
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	if c.ServerReadTimeoutDur, err = time.ParseDuration(c.Server.ReadTimeout); err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	if c.ServerReadTimeoutDur < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义", c.Server.ReadTimeout)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
		c.Pool.ExpiringSoon = "0"
	}

	// ── 保活自动化（四层防护，2026-10-23）────────────────────────────
	//
	// 三个参数都是「空 = 回落默认」，与既有 soft_rate / expiring_soon 的处理风格一致：
	// 老 config 没有这些键 → 零值 → 用默认值（间隔 4h / 重试 [30,120,600] / 验证 7 天），
	// 即老配置**自动获得新行为**，无需改文件。显式写 "0" 可关闭。
	//
	// keepalive_interval：空 = 用默认 4h；"0" = 关闭间隔循环（只剩整点排程，回到旧行为）。
	//
	// ⚠ 语义分层（易踩）：配置层的「空」= 用默认，必须解析成**具体值**再传给调度器；
	// 调度器层的「0」= 禁用。若把空值解析成 0 传下去，间隔循环会被静默关掉
	// （实测踩过：配置没写该键 → 循环没启动，日志里看不到「保活间隔循环已启动」）。
	if c.Schedule.KeepaliveInterval == "" {
		c.KeepaliveIntervalDur = defaultKeepaliveIntervalCfg
	} else if c.KeepaliveIntervalDur, err = time.ParseDuration(c.Schedule.KeepaliveInterval); err != nil {
		return fmt.Errorf("schedule.keepalive_interval: %w", err)
	}
	if c.KeepaliveIntervalDur < 0 {
		c.KeepaliveIntervalDur = 0
		c.Schedule.KeepaliveInterval = "0"
	}
	// keepalive_retry：nil = 默认序列；空数组 [] = 不重试（显式关闭）。
	if c.Schedule.KeepaliveRetry != nil {
		for i, sec := range c.Schedule.KeepaliveRetry {
			if sec < 0 {
				return fmt.Errorf("schedule.keepalive_retry[%d]: 不能为负", i)
			}
			c.KeepaliveRetryDurs = append(c.KeepaliveRetryDurs, time.Duration(sec)*time.Second)
		}
	}
	// keepalive_verify_within：空 = 默认 7 天；"0" = 不提前验证（只按 RefreshSkew）。
	if c.Schedule.KeepaliveVerifyWithin == "" {
		c.KeepaliveVerifyWithinDur = defaultKeepaliveVerifyWithinCfg
	} else if c.KeepaliveVerifyWithinDur, err = time.ParseDuration(c.Schedule.KeepaliveVerifyWithin); err != nil {
		return fmt.Errorf("schedule.keepalive_verify_within: %w", err)
	}
	if c.KeepaliveVerifyWithinDur < 0 {
		c.KeepaliveVerifyWithinDur = 0
		c.Schedule.KeepaliveVerifyWithin = "0"
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	//（"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	// 积分保底：负值钳 0（= 关闭）。0 是合法默认（关闭），无需空值回落。
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	// Rate Dashboard 三维评分权重：缺省回落 0.4/0.3/0.3；任一 <=0 视为未配置也回落默认。
	if c.Ratedash.RateW <= 0 || c.Ratedash.LenW <= 0 || c.Ratedash.CapW <= 0 {
		c.Ratedash.RateW, c.Ratedash.LenW, c.Ratedash.CapW = 0.4, 0.3, 0.3
	}
	if err := c.normalizeTrae(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizeTrae 补齐 Trae 平台段的缺省值并校验。
//
// 校验走 Trae 自己的 config.Normalize()（复用该平台既有规则），
// 但**刻意在调用前先填好默认**：Trae 的 Default() 里有它自己的默认路径
// （./auths、./data/state.json），若直接对零值结构体 normalize，会得到
// 与合并前不一致的路径（两个平台抢同一个 auths 目录 = 凭证互相污染）。
func (c *Config) normalizeTrae() error {
	if !c.Trae.Enabled {
		// 关闭时不做任何归一：避免「配了半截的 trae 段」在关掉后仍触发校验失败，
		// 让用户无法通过 enabled=false 绕过一个写错的子字段。
		return nil
	}
	// 路径默认值在 Default() 已置；此处兜底显式空串（用户手写 "auth_dir": ""）。
	if strings.TrimSpace(c.Trae.AuthDir) == "" {
		c.Trae.AuthDir = "./traeauths"
	}
	if strings.TrimSpace(c.Trae.StateFile) == "" {
		c.Trae.StateFile = "./data/trae-state.json"
	}
	// 用平台自己的归一逻辑校验（时长字符串、小时范围等）。
	// 若失败，说明用户把 trae 段写错了——启动即报错，不静默吞掉。
	if err := c.TraeConfigToPlatform().Normalize(); err != nil {
		return fmt.Errorf("trae: %w", err)
	}
	return nil
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 passthrough/custom/append）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom / append）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
		return err
	}
	return checkHourRange("schedule.growth_hours", "growth_enabled", c.Schedule.GrowthHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
