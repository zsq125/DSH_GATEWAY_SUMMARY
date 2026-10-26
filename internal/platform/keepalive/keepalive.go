// Package keepalive 提供**跨平台**的 token 保活编排。
//
// ── 为什么需要这个包（2026-10-24）─────────────────────────────────────
//
// WorkBuddy 与 Trae 两个平台各自维护了一套保活代码，且都踩了同一组坑：
//
//	① 保活只在整点跑（每天 1 次）→ refreshToken 静默失效最长 24h 才发现
//	② 失败只写日志 → 面板不可见，用户直到 accessToken 到期突然 503 才知道
//	③ 瞬时抖动没有重试 → 一次网络抖动就让本轮保活放弃
//	④ 401 直接判死 → refreshToken 其实还有效的账号被永久禁用（误杀）
//
// 平台间唯一真正不同的是「怎么发一次刷新请求」和「怎么判定凭证已死」。
// 其余（何时刷、失败怎么办、如何告警）**完全一致**，因此抽到本包，
// 让新平台接入时不必再踩一遍同样的坑。
//
// ── 分层设计 ─────────────────────────────────────────────────────────
//
//	Refresher      平台实现：发一次刷新请求（唯一的平台相关部分）
//	Policy         配置：间隔 / 重试 / 预算 / 提前验证窗口
//	Runner         编排：遍历账号、预筛、重试、告警、落盘
//
// 本包**不依赖**任何具体平台的 auth 类型：账号以字符串 UID + 回调函数表达，
// 平台用自己的类型实现 Refresher 即可。
package keepalive

import (
	"context"
	"errors"
	"log"
	"time"
)

// Verdict 一次刷新的结果分类。平台负责把上游错误映射到这里。
type Verdict int

const (
	// VerdictOK 刷新成功（凭证已更新）。
	VerdictOK Verdict = iota
	// VerdictDead 凭证已确认失效（如 12153 / invalid_grant）。
	//
	// 这是**终态**：重试无意义，必须告警要求人工重新登录。
	VerdictDead
	// VerdictTransient 瞬时失败（网络抖动 / 5xx / 超时）。
	//
	// 值得重试；重试耗尽后仍属"非终态"，下一轮还会再试。
	VerdictTransient
)

// String 便于日志与测试断言。
func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictDead:
		return "dead"
	case VerdictTransient:
		return "transient"
	}
	return "unknown"
}

// Subject 待保活的一个账号。
//
// 用接口而非具体类型：本包不依赖各平台的 auth 结构，平台只需提供
// 这三件事就能接入保活编排。
type Subject interface {
	// UID 稳定标识（日志与告警用）。
	UID() string
	// Label 人类可读名（日志用；为空时回落 UID）。
	Label() string
	// Refresh 发一次刷新请求，并把上游错误映射成 Verdict。
	//
	// 实现方应在成功时**自行更新凭证字段**；本包只负责调用时机与重试，
	// 落盘由 Save 回调统一处理。
	Refresh() (Verdict, error)
	// Save 凭证落盘（刷新成功后调用）。返回 error 只记日志，不影响判定。
	Save() error
	// NeedsRefresh 报告凭证是否「值得现在刷一次」。
	//
	// 用于间隔循环的预筛：剩余寿命还很充裕时跳过，避免白打上游。
	// 整点排程会绕过此判定（见 Runner.RunOnce 的 force 参数）。
	NeedsRefresh(within time.Duration) bool
}

// AlertSink 告警接收方（由平台实现，通常写到自己的账号状态里供面板展示）。
type AlertSink interface {
	// MarkDead 标记「该账号凭证已确认失效，需重新登录」。
	MarkDead(uid, msg string)
	// ClearDead 清除告警（刷新成功后调用）。
	ClearDead(uid string)
}

// Policy 保活策略。零值全部回落默认，老调用方无需配置即可获得新行为。
type Policy struct {
	// Interval 固定间隔循环周期。<=0 用默认 4h；显式负值表示禁用循环。
	Interval time.Duration
	// Retry 重试间隔序列。nil 用默认 [30s,2min,10min]；空切片 [] 表示不重试。
	//
	// 注意：nil 与空切片语义不同（nil=默认，[]=禁用），由调用方显式区分。
	Retry []time.Duration
	// RetryBudget 单轮保活的重试总预算。<=0 用默认 90s。
	//
	// 防止「多个账号同时瞬时失败」把一轮保活拖成 N×12 分钟
	// （手动触发时面板请求也会挂住）。
	RetryBudget time.Duration
	// VerifyWithin 提前验证窗口。<=0 用默认 7 天。
	//
	// 即使凭证剩余寿命还很充裕，只要小于该值就主动刷新 ——
	// 保证根凭证（refreshToken）至少每周被验证一次，不会静默死掉。
	VerifyWithin time.Duration
}

// 默认值。与 WorkBuddy v4 的取值一致（有测试钉住）。
const (
	DefaultInterval     = 4 * time.Hour
	DefaultRetryBudget  = 90 * time.Second
	DefaultVerifyWithin = 7 * 24 * time.Hour
)

// DefaultRetry 默认重试序列：30 秒 → 2 分钟 → 10 分钟。
var DefaultRetry = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
}

// normalize 把零值补成默认值。nil 的 Retry 用默认，空切片保持"不重试"。
func (p Policy) normalize() Policy {
	if p.Interval == 0 {
		p.Interval = DefaultInterval
	}
	if p.Retry == nil {
		p.Retry = DefaultRetry
	}
	if p.RetryBudget <= 0 {
		p.RetryBudget = DefaultRetryBudget
	}
	if p.VerifyWithin <= 0 {
		p.VerifyWithin = DefaultVerifyWithin
	}
	return p
}

// Runner 保活编排器。平台持有一个实例即可获得全部四层防护。
type Runner struct {
	policy Policy
	sink   AlertSink
	// sleep 可注入（测试用），默认 sleepCtx。
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewRunner 构建编排器。sink 可为 nil（不产生告警，仅记日志）。
func NewRunner(policy Policy, sink AlertSink) *Runner {
	return &Runner{
		policy: policy.normalize(),
		sink:   sink,
		sleep:  sleepCtx,
	}
}

// Policy 返回归一化后的策略（供平台自省/测试）。
func (r *Runner) Policy() Policy { return r.policy }

// Result 单个账号的保活结果。
type Result struct {
	UID     string
	Verdict Verdict
	Err     error
	// Skipped 为 true 表示因「剩余寿命充裕」被预筛跳过（不是失败）。
	Skipped bool
}

// RunOnce 对一组账号执行一轮保活。
//
// force=true：跳过 NeedsRefresh 预筛，无条件尝试刷新（整点排程语义）。
// force=false：只刷 NeedsRefresh(VerifyWithin) 为真的账号（间隔循环语义，
// 省上游调用）。
//
// ctx 取消时立即放弃剩余账号与重试等待（关停不被重试拖住）。
//
// 返回值顺序与入参一致，便于平台汇总。
func (r *Runner) RunOnce(ctx context.Context, subjects []Subject, force bool) []Result {
	out := make([]Result, 0, len(subjects))
	budgetEnd := time.Now().Add(r.policy.RetryBudget)

	for _, s := range subjects {
		if ctx.Err() != nil {
			return out
		}
		if s == nil {
			continue
		}
		// 预筛：间隔循环只刷「值得刷」的账号。
		if !force && !s.NeedsRefresh(r.policy.VerifyWithin) {
			out = append(out, Result{UID: s.UID(), Skipped: true})
			continue
		}

		verdict, err := r.refreshWithRetry(ctx, s, budgetEnd)
		switch verdict {
		case VerdictOK:
			if serr := s.Save(); serr != nil {
				log.Printf("keepalive %s: 落盘失败: %v", labelOf(s), serr)
			}
			if r.sink != nil {
				r.sink.ClearDead(s.UID())
			}
		case VerdictDead:
			// 终态：立即告警，不等重试/计数。
			//
			// 这是本次抽象的核心价值之一：refreshToken 失效是**终态**，
			// 掩盖它只会让 accessToken 到期时突然暴雷（用户措手不及）。
			if r.sink != nil {
				msg := "凭证已失效，需重新登录"
				if err != nil {
					msg = err.Error()
				}
				r.sink.MarkDead(s.UID(), msg)
			}
			log.Printf("ALERT: keepalive %s: 凭证已失效（需重新登录）: %v", labelOf(s), err)
		case VerdictTransient:
			// 非终态：不告警、不禁用，下一轮循环还会再试。
			log.Printf("keepalive %s: 瞬时失败（非终态，下轮再试）: %v", labelOf(s), err)
		}
		out = append(out, Result{UID: s.UID(), Verdict: verdict, Err: err})
	}
	return out
}

// refreshWithRetry 带重试与预算的刷新（Layer 3）。
func (r *Runner) refreshWithRetry(ctx context.Context, s Subject, budgetEnd time.Time) (Verdict, error) {
	verdict, err := s.Refresh()
	if verdict != VerdictTransient {
		return verdict, err // OK 或 Dead：无需重试
	}
	for _, d := range r.policy.Retry {
		// 预算耗尽 → 放弃重试，把机会留给下一轮。
		if time.Now().Add(d).After(budgetEnd) {
			return verdict, err
		}
		if !r.sleep(ctx, d) {
			return verdict, err // ctx 取消
		}
		verdict, err = s.Refresh()
		if verdict != VerdictTransient {
			return verdict, err
		}
	}
	return verdict, err
}

// StartLoop 固定间隔保活循环（Layer 1）。阻塞直到 ctx 取消。
//
// 调用方应 `go runner.StartLoop(ctx, subjectsFn)`。
//
// subjectsFn 每轮重新取账号列表（支持运行期增删账号）。
// 返回的通道在首次 tick 前不产出；ctx 取消后关闭。
func (r *Runner) StartLoop(ctx context.Context, subjectsFn func() []Subject) {
	if r.policy.Interval <= 0 {
		return // 禁用
	}
	log.Printf("keepalive: 间隔循环已启动，每 %v 检查一次（把静默失效窗口从 24h 压到 %v）",
		r.policy.Interval, r.policy.Interval)
	ticker := time.NewTicker(r.policy.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx, subjectsFn(), false)
		}
	}
}

// labelOf 取日志标签（Label 为空时回落 UID）。
func labelOf(s Subject) string {
	if l := s.Label(); l != "" {
		return l
	}
	return s.UID()
}

// sleepCtx 等待 d，返回 false 表示 ctx 已取消。
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

// IsDead 便捷判定：平台把上游错误映射成 Verdict 时可用。
//
// 用法：
//
//	var ue *upstream.Error
//	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
//	    return keepalive.VerdictDead, err
//	}
var ErrDeadSentinel = errors.New("credential dead")

// AsDead 把「凭证已死」的错误标记为可识别。
func AsDead(err error) error {
	if err == nil {
		return ErrDeadSentinel
	}
	return errors.Join(ErrDeadSentinel, err)
}

// IsDeadErr 报告错误是否被标记为「凭证已死」。
func IsDeadErr(err error) bool {
	return errors.Is(err, ErrDeadSentinel)
}
