// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"allplatforms/internal/platform/workbuddy/auth"
	"allplatforms/internal/platform/workbuddy/logfmt"
	"allplatforms/internal/platform/workbuddy/pool"
	"allplatforms/internal/platform/workbuddy/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// KeepaliveInterval 保活**间隔**（非整点）。>0 时启用「固定间隔循环」，
	// 与 KeepaliveHours（整点排程）**并行运行**，两者互不排斥。
	//
	// ── 为什么需要它（2026-10-23 事故）────────────────────────────────
	//
	// 原设计只有整点排程（每天 22:00 一次）。实测事故链：
	//
	//	10-22 14:00  refreshToken 被上游作废（此时无人知晓）
	//	10-22 22:00  保活运行 → 刷新失败 → 只写一行日志
	//	10-23 00:07  accessToken 仍可用，对话照常成功（用户无感）
	//	10-23 11:58  accessToken 也失效 → 突然 503（用户才发现）
	//
	// 即：refreshToken 死了之后，系统最长要 **24 小时**才发现，
	// 而期间 accessToken 还能用，形成「静默失效窗口」。
	//
	// 固定间隔循环把发现窗口从 24 小时压到 KeepaliveInterval（默认 4h）。
	//
	// 取 4h 而非更短：刷新是真实上游调用（每次 rotate refreshToken），
	// 过密会增加上游调用量与风控面。4h 对「30 天有效期的 accessToken」
	// 完全够用，且一天只多 5 次刷新。
	KeepaliveInterval time.Duration

	// KeepaliveRetry 保活失败的重试间隔序列（秒）。空=不重试。
	//
	// 只对**非确定性**失败重试（网络抖动 / 上游瞬时故障）；
	// `12153`（refreshToken 真失效）与「响应缺 accessToken」是确定性失败，不重试。
	// 默认 [30, 120, 600]：30 秒 → 2 分钟 → 10 分钟，覆盖绝大多数瞬时故障。
	//
	// ⚠ 重试是**串行阻塞**的（保活循环里逐号处理）。为避免「所有号都瞬时失败
	// 时把一轮保活拖成 N×12 分钟」，实际等待受 KeepaliveRetryBudget 约束。
	KeepaliveRetry []time.Duration

	// KeepaliveRetryBudget 单轮保活的总重试时间预算。默认 90 秒。
	//
	// 为什么需要：重试间隔合计 12.5 分钟，若某轮多个账号同时瞬时失败，
	// 串行重试会让这轮保活迟迟不结束（手动触发时面板请求也会挂住）。
	// 预算耗尽后不再重试，剩余账号走「非终态失败」路径（下一轮循环再试）。
	//
	// 90 秒 ≈ 覆盖前两档（30s + 2min 中的前 60s），足以扛过绝大多数抖动；
	// 真正的长故障交给下一轮间隔循环（4h 后），而不是在这里死等。
	KeepaliveRetryBudget time.Duration

	// KeepaliveVerifyWithin 提前验证 refreshToken 的有效性窗口。
	//
	// accessToken 剩余寿命小于该值时，保活会主动刷新（相当于把
	// RefreshSkew 的语义扩展到保活侧）。默认 168h（7 天）：
	// 保证「即使 refreshToken 中途失效，也至少每周被验证一次」。
	KeepaliveVerifyWithin time.Duration

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此做最早到期优先，见
	// entry.creditsEarliestExpiry）。<=0 时不做路由门槛；展示仍使用完整逐包数据。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool

	// IncludeDisabledInTasks 让「保号类」四任务（签到 / 活跃上报 / token 保活 / 余额刷新）
	// 对已禁用（disabled）的账号也执行（schedule.include_disabled_in_tasks）。
	// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到保号，但**仍不参与
	// 选号**——pool.pick 侧的 disabled 过滤与本开关无关。
	IncludeDisabledInTasks bool

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()

	// TraeCheckinHook Trae 签到回调（panel 接线：打 Trae 后端的 /trae/api/checkin_all）。
	//
	// 存在的理由：需求要求「WorkBuddy 的定时自动签到把 Trae 账号也带上」。
	// Trae 是独立进程独立账号池，调度器无法直接遍历它的账号，所以由 panel
	// 注入一个回调去触发。Trae 后端自己的排程照常运行，两边重复调用无害
	//（上游签到按自然日幂等）。nil 时只签 WorkBuddy（未接线 Trae 的部署）。
	TraeCheckinHook func()

	// VisionHours 视觉能力探测时点的本地小时（默认 [3]：03:00 跑一次，避开签到/成长高峰）。
	// 实际探测由 VisionHook 回调执行（panel.RunVisionProbeOnce，进程内实现）。
	VisionHours []int
	// VisionDisabled 显式关闭视觉探测排程（schedule.vision_enabled=false）。
	VisionDisabled bool

	// VisionHook 视觉能力探测回调（panel.RunVisionProbeOnce：进程内探测并写
	// data/vision_probes.json）。调度器只管时点不管实现；panel 构造晚于 scheduler，
	// 用 SetVisionHook 事后挂载；nil 时到点跳过。
	VisionHook func()
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// runCtx 进程级 ctx（由 Run 记录），供保活重试的可中断等待使用。
	// 未启动 Run 时（单测直接调 RunKeepaliveNow）为 nil → ctxOf 返回 Background。
	runCtx atomic.Pointer[context.Context]
}

// ctxOf 返回进程级 ctx；未启动时为 context.Background()（永不取消）。
func (s *Scheduler) ctxOf() context.Context {
	if p := s.runCtx.Load(); p != nil && *p != nil {
		return *p
	}
	return context.Background()
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	if len(cfg.VisionHours) == 0 {
		cfg.VisionHours = []int{3} // 03:00 视觉探测（避开签到/成长高峰）
	}
	// 保活间隔与重试的默认值（0 表示未配置）。
	//
	// 默认 4h 间隔：把「refreshToken 静默失效」的发现窗口从 24h 压到 4h。
	// 默认重试 [30s, 2min, 10min]：救回瞬时抖动，但不碰确定性失败（12153）。
	// 默认 7 天验证窗口：即使 token 还剩很久，也每周主动验证一次 refreshToken。
	if cfg.KeepaliveInterval < 0 {
		cfg.KeepaliveInterval = 0 // 负数视为禁用（显式）
	}
	if cfg.KeepaliveInterval == 0 {
		cfg.KeepaliveInterval = defaultKeepaliveInterval
	}
	if len(cfg.KeepaliveRetry) == 0 {
		cfg.KeepaliveRetry = defaultKeepaliveRetry
	}
	if cfg.KeepaliveRetryBudget <= 0 {
		cfg.KeepaliveRetryBudget = defaultKeepaliveRetryBudget
	}
	if cfg.KeepaliveVerifyWithin <= 0 {
		cfg.KeepaliveVerifyWithin = defaultKeepaliveVerifyWithin
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
	}
}

// 保活自动化的默认参数（见 Config.Keepalive* 的注释）。
const (
	defaultKeepaliveInterval     = 4 * time.Hour
	defaultKeepaliveVerifyWithin = 7 * 24 * time.Hour
	// defaultKeepaliveRetryBudget 单轮保活的重试总预算。
	// 90 秒 ≈ 前两档（30s、2min）的大部分，够扛瞬时抖动，又不至于让
	// 「多账号同时失败」把一轮保活拖到十几分钟。
	defaultKeepaliveRetryBudget = 90 * time.Second
)

var defaultKeepaliveRetry = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
}

// ExpiringSoonWindow 返回当前快过期路由窗口（读取时与热配置写在 schedMu 下同步）。
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow 热更新快过期路由窗口。窗口变化时清空池内旧快照，避免在下一轮
// 余额刷新覆盖前，继续用旧窗口得出的最早到期顺序选号。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// SetVisionHook 挂载/替换视觉能力探测回调（panel.RunVisionProbeOnce：进程内探测）。
// panel 构造晚于 scheduler，用本方法事后接线；nil 时到点跳过。
func (s *Scheduler) SetVisionHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.VisionHook = fn
	s.schedMu.Unlock()
}

// SetTraeCheckinHook 挂载/替换「Trae 签到」回调。
//
// 为什么用 hook 而不是把 Trae 直接塞进 scheduler 的账号遍历：Trae 的账号池、
// 凭证格式、冷却状态机全都住在另一个进程（traeapi2）里，本 scheduler 对它
// 一无所知。硬要合并就得在 WorkBuddy 的调度器里塞一套 Trae 协议代码——两个
// 上游的失败分类、幂等语义、限速节奏都不一样，合并后必然互相拖累。
//
// hook 的边界很窄：**只在本调度器触发签到之后调一次**，由接线方（panel）
// 去打 Trae 后端。Trae 后端自己的排程仍然独立运行，两者重复调用无害——
// 上游签到是按自然日幂等的，第二次会得到"今天已签到"。
func (s *Scheduler) SetTraeCheckinHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.TraeCheckinHook = fn
	s.schedMu.Unlock()
}

// runTraeCheckin 触发 Trae 签到钩子（未接线时静默跳过）。
//
// 刻意吞掉 panic 与慢响应：Trae 后端是外部依赖，它挂了不该拖垮
// WorkBuddy 的签到主流程（本函数在主流程末尾被调用）。
func (s *Scheduler) runTraeCheckin() {
	s.schedMu.Lock()
	fn := s.cfg.TraeCheckinHook
	s.schedMu.Unlock()
	if fn == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			// 外部注入的回调不该让调度器进程退出；记一行便于排查。
			if r := recover(); r != nil {
				log.Printf("trae checkin hook panic: %v", r)
			}
			close(done)
		}()
		fn()
	}()
	// 超时保护：Trae 后端卡住时不等它（WorkBuddy 侧已签完，无需被拖住）。
	select {
	case <-done:
	case <-time.After(traeCheckinHookTimeout):
		log.Printf("trae checkin hook: 超时 %s（不影响 WorkBuddy 签到结果）", traeCheckinHookTimeout)
	}
}

// traeCheckinHookTimeout 单次 Trae 钩子的等待上限。
const traeCheckinHookTimeout = 2 * time.Minute

// includeDisabledInTasks 返回保号类任务是否应覆盖禁用账号（配置快照，供循环开头取一次）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新「保号类任务是否覆盖禁用账号」
// （schedule.include_disabled_in_tasks）。
// 用独立 setter 而非并入 Reconfigure：后者已有 12 个位置参数，继续追加会让调用点难以
// 校对；本开关语义独立（只影响账号过滤，不影响时点），单独设值更清晰。
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
	taskVision
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	visionHours, visionOff := s.cfg.VisionHours, s.cfg.VisionDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	if !visionOff && len(visionHours) > 0 {
		slots = append(slots, slot{nextFire(now, visionHours), taskVision})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	// 记录进程级 ctx：保活重试的等待据此可中断（关停时不被 12 分钟重试拖住）。
	s.runCtx.Store(&ctx)
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠的风险与
			// 串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			switch k {
			case taskCheckin:
				s.RunCheckinNow()
			case taskTravel:
				s.RunTravelNow()
			case taskActivity:
				s.runActivity(ctx)
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskBlackcat:
				s.RunBlackcatNow()
			case taskGrowth:
				// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
				s.schedMu.Lock()
				hook := s.cfg.GrowthHook
				s.schedMu.Unlock()
				if hook != nil {
					hook()
				}
			case taskVision:
				// 视觉能力探测：回调在 panel 侧异步启动（进程内探测并写
				// data/vision_probes.json），nil 未挂载则跳过。探测较耗时（逐个模型发图），
				// 与成长任务同为异步派发，不阻塞本批其它任务。
				s.schedMu.Lock()
				hook := s.cfg.VisionHook
				s.schedMu.Unlock()
				if hook != nil {
					hook()
				}
			}
		}(k)
	}
	wg.Wait()
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用账号默认跳过——
// 若 schedule.include_disabled_in_tasks 打开则一并执行（禁用只关选号，不停保号）。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow() {
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		// 企业版无签到体系：上游对 POST /v2/billing/meter/daily-checkin 直接
		// 400 code 10001「企业账号不支持该操作」。跳过签到，但**不 continue**——
		// 下方余额查询照常跑（企业额度走 get-enterprise-user-usage 口径，且是
		// credit_floor 判定与面板额度展示的唯一数据源）。
		if !a.IsEnterprise() {
			if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
				// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
				if upstream.IsAlreadyCheckin(err) {
					s.cfg.Pool.NoteCheckinDone(st.UID)
					log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
				} else {
					log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
				// 其余业务错误也继续走余额查询
			} else {
				// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
				// 重复触发时出现），成功也落一行。
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
			}
		}
		// 分桶查余额：配置窗口内的积分单独标记，同时记录最早未来到期批次。
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}
	s.RunStreakBonusNow()
	// WorkBuddy 侧签到（含连登管家）全部结束后，顺带触发一次 Trae 签到。
	// 放在最后：Trae 是外部依赖，不能让它影响 WorkBuddy 本次的签到结果。
	s.runTraeCheckin()
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号默认跳过（schedule.include_disabled_in_tasks 打开时一并上报）；
// 无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上
// code=0 OK，点亮连登）；一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// 企业版跳过活跃上报：/v2/report 本身返回 200（不报错），但它唯一的作用是
		// 点亮 growth 连登天数 / 解锁 first_buddy 领养——这两者对企业号都是 403
		// （「growth system is only available for personal users」），即**零收益**。
		// 该类"静默无效"最易被忽略（日志无异常），故显式跳过，不给上游多发一次请求。
		if a.IsEnterprise() {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切，
		// 无需改动 upstream。此处曾按「D4 门控：global 无活跃体系」跳过 global，实测该
		// 判断不成立——国际版 /v2/report 可用，跳过即国际版账号永远点不亮连登（上游
		// a190252 同口径修复）。checkin/travel 的 global 门控不受影响，仍跳过。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 禁用账号默认跳过；若 schedule.include_disabled_in_tasks 打开，禁用号也会续期 token
// （这是轮换用法下把闲置号保持可用的关键），但**不再对已禁用的号重复计数 12153**——
// 它已经是终态，再计一次只会打出一行「— 禁用」的误导日志。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
//
// ── 2026-10-23 增强（四层防护）────────────────────────────────────────
//
// 事故背景：refreshToken 被上游作废后，因为 accessToken 仍在有效期内，
// 对话照常成功 → 用户完全无感；直到 accessToken 也失效才突然 503。
// 期间最长 24 小时（等下一次整点保活）系统完全不知道账号已死。
//
// 本函数现在的行为：
//  1. **重试**（Layer 3）：非确定性失败（网络抖动/上游瞬时故障）按
//     cfg.KeepaliveRetry 重试；`12153` 是确定性失败，不重试。
//  2. **提前验证**（Layer 4）：即使 accessToken 还剩很久，只要剩余寿命
//     小于 cfg.KeepaliveVerifyWithin（默认 7 天）就主动刷新 —— 保证
//     refreshToken 至少每周被验证一次，不会「静默死掉」。
//  3. **立即告警**（Layer 2）：12153 立即写入账号状态（reason + 错误时间），
//     面板可见，而不是只写一行日志。
//
// 频次由 cfg.KeepaliveInterval 的独立循环提供（Layer 1，见 StartKeepaliveLoop）。
func (s *Scheduler) RunKeepaliveNow() {
	s.runKeepalive(true)
}

// runKeepalive 保活主体。force=true 时不做「剩余寿命」预筛（整点排程语义：
// 无条件刷新所有账号，与改动前逐字一致）；force=false 时只刷「剩余寿命 <
// KeepaliveVerifyWithin」的账号（间隔循环语义：省上游调用，只做有效性验证）。
//
// 为什么区分：整点排程（每天 22:00）是「日常保号」，必须无条件刷新——
// 它同时承担「把闲置号 token 续期」的职责（include_disabled_in_tasks 场景），
// 若加预筛会让「剩余 20 天」的闲置号永远不被续期，反而更容易死。
// 间隔循环（每 4h）是「失效探测」，按需刷新才合理，否则一天白打 6 次上游。
func (s *Scheduler) runKeepalive(force bool) {
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// Layer 4（仅间隔循环）：提前验证 —— 剩余寿命不足 VerifyWithin 才刷。
		// NeedsRefresh(within) 语义是「剩余寿命 < within」，覆盖「7 天内到期」与「已过期」。
		if !force && !a.NeedsRefresh(s.cfg.KeepaliveVerifyWithin) {
			continue
		}

		err := s.refreshWithRetry(a) // Layer 3
		if err != nil {
			var ue *upstream.Error
			isDead := errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead
			if isDead {
				// Layer 2：立即告警 —— 写进 pool 状态，面板立刻可见。
				// 不等 3 次计数：refreshToken 失效是**终态**，掩盖它只会让
				// accessToken 到期时突然暴雷（本次事故正是如此）。
				s.cfg.Pool.MarkSessionDeadAlert(st.UID, err.Error())
				log.Printf("ALERT: keepalive %s: refreshToken 已失效（12153），需重新登录；"+
					"accessToken 到期后将无法使用: %v", logfmt.Label(st.UID, st.Nickname), err)
				if !st.Disabled && s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用",
						logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			} else {
				// 非确定性失败（重试已耗尽）：记错误但不禁用 —— 下次循环还会再试。
				log.Printf("keepalive %s: 刷新失败（已重试 %d 次，非终态，下轮再试）: %v",
					logfmt.Label(st.UID, st.Nickname), len(s.cfg.KeepaliveRetry), err)
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		s.cfg.Pool.ClearSessionDeadAlert(st.UID)
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// refreshWithRetry 带重试的 token 刷新（Layer 3）。
//
// 只重试**非确定性**失败：网络抖动、5xx、超时这类重试有意义；
// 以下情况**不重试**（确定性失败，重试只是白打上游调用）：
//   - `12153`（refreshToken 真失效）
//   - 响应格式不符（如缺 accessToken）—— 换多少次结果都一样
//
// 重试间隔取 cfg.KeepaliveRetry（默认 30s / 2min / 10min）。总耗时上限
// 约 12 分钟，期间该账号的在途名额不受影响（保活不占在途名额）。
//
// 可中断：sleep 期间若 s.ctx 被取消（进程关闭 / 测试清理），立即放弃重试，
// 避免「关停时被重试拖住」以及「测试里无法收尾」。
func (s *Scheduler) refreshWithRetry(a *auth.Auth) error {
	err := s.cfg.Upstream.RefreshToken(a)
	if err == nil {
		return nil
	}
	if !isRetriableRefreshErr(err) {
		return err // 确定性失败，不重试
	}
	ctx := s.ctxOf()
	// 总预算：避免「多账号同时瞬时失败」把一轮保活拖成 N×12 分钟。
	deadline := time.Now().Add(s.cfg.KeepaliveRetryBudget)
	for _, d := range s.cfg.KeepaliveRetry {
		// 预算耗尽 → 放弃重试（剩余账号交给下一轮间隔循环）。
		if time.Now().Add(d).After(deadline) {
			return err
		}
		// 可中断的等待：ctx 取消 → 立即放弃（不拖住关停/测试）。
		if !sleepCtx(ctx, d) {
			return err
		}
		rerr := s.cfg.Upstream.RefreshToken(a)
		if rerr == nil {
			log.Printf("keepalive %s: 重试 %v 后刷新成功", logfmt.Label(a.UID, a.Nickname), d)
			return nil
		}
		err = rerr
		if !isRetriableRefreshErr(rerr) {
			return rerr // 重试途中发现确定性失败，立即停
		}
	}
	return err
}

// isRetriableRefreshErr 判断刷新失败是否值得重试。
//
// 确定性失败（不重试）：
//   - ErrSessionDead（12153）：refreshToken 已失效，重试无意义
//   - 响应格式不符（"no accessToken in response"）：上游行为固定，换多少次都一样
//
// 其余（网络错误、超时、5xx、429）视为瞬时故障，重试有意义。
func isRetriableRefreshErr(err error) bool {
	if err == nil {
		return false
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		return false
	}
	// 响应缺字段是确定性失败：RefreshToken 对该形态返回固定文案。
	// 用子串判断而非错误类型，因为它是 fmt.Errorf 构造的普通错误。
	if strings.Contains(err.Error(), "no accessToken in response") {
		return false
	}
	return true
}

// StartKeepaliveLoop 固定间隔保活循环（Layer 1）。
//
// 与整点排程（KeepaliveHours）**并行运行**：整点排程保留兼容，
// 本循环负责把「refreshToken 静默失效」的发现窗口从 24 小时压到 interval。
//
// interval <= 0 时不启动（等价于关闭该循环）。
// 每次触发复用 RunKeepaliveNow（已含重试/提前验证/告警）。
func (s *Scheduler) StartKeepaliveLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		// 启动后先等一个完整间隔再首次执行：避免与「进程刚启动时的整点排程」
		// 重复刷新（启动时若恰好临近整点，两者会撞在一起）。
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.cfg.KeepaliveDisabled {
					continue
				}
				// force=false：间隔循环只刷「剩余寿命 < VerifyWithin」的账号，
				// 避免一天白打 6 次上游；整点排程仍走 force=true 的无条件刷新。
				s.runKeepalive(false)
			}
		}
	}()
	log.Printf("scheduler: 保活间隔循环已启动，每 %v 检查一次（把静默失效窗口从 24h 压到 %v）",
		interval, interval)
}

// RunBalanceRefreshNow 并发查询余额并更新池内 credits。
// 账号范围：默认跳过禁用账号；schedule.include_disabled_in_tasks 打开时一并刷新
// （轮换用法下据此判断下一个该启用谁）。注意 ReenableIfCredits 对 disabled 是
// no-op，所以本开关**不会**导致禁用号被自动解冻——它只让已有账号的积分保持新鲜。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				return
			}
			s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
		}(a, st.UID)
	}
	wg.Wait()
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}
