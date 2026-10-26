// Trae 平台对接通用保活抽象层（internal/platform/keepalive）。
//
// ── 为什么需要（2026-10-24）──────────────────────────────────────────
//
// 排查发现 Trae 的保活与 WorkBuddy 有**完全相同的三个缺陷**：
//
//	① 只有整点排程（每天 22:00 一次）→ refreshToken 静默失效最长 24h 才发现
//	② 失败只 log.Printf → 面板不可见，用户直到 accessToken 到期突然失败才知道
//	③ 401 直接 Disable（handler.go:677）→ refreshToken 还有效的账号被误杀
//
// 与 WorkBuddy 不同的是 Trae **连 3 次计数都没有**（一次 401 就永久禁用），
// 误杀风险更高。本文件把 keepalive 抽象层接进来，补齐同样的四层防护。
package scheduler

import (
	"errors"
	"time"

	"allplatforms/internal/platform/keepalive"
	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/upstream"
)

// traeSubject 把一个 Trae 账号适配成 keepalive.Subject。
type traeSubject struct {
	a  *auth.Auth
	up *upstream.Client
	// skew 整点排程用的刷新窗口（Trae 侧配置，默认 24h）。
	skew time.Duration
}

func (s *traeSubject) UID() string { return s.a.UID }
func (s *traeSubject) Label() string {
	if s.a.Nickname != "" {
		return s.a.Nickname + "(" + s.a.UID + ")"
	}
	return s.a.UID
}

// NeedsRefresh 供间隔循环预筛：剩余寿命不足 within 才值得刷。
//
// ⚠ 与 WorkBuddy 的关键差异（易错点）：Trae 的既有语义是按
// **RefreshSkew（默认 24h）** 决定是否换 token，而不是 WorkBuddy 的
// VerifyWithin（7 天）。用 7 天会让「剩余 100 小时的 token」也被刷新，
// 破坏 TestKeepaliveSkipsFreshToken 钉住的既有约定。
//
// 所以这里取 **min(within, skew)**：
//   - 整点排程传 within=VerifyWithin(7d)，但实际用 skew(24h) → 保持原语义
//   - 间隔循环同理，仍是 24h 窗口
//
// 即：Trae 的「提前验证」窗口等于它自己的 RefreshSkew，不额外放宽。
func (s *traeSubject) NeedsRefresh(within time.Duration) bool {
	w := within
	if s.skew > 0 && s.skew < w {
		w = s.skew
	}
	return s.a.NeedsRefresh(w)
}

// Refresh 发一次刷新，并把上游错误映射成 keepalive.Verdict。
//
// 映射规则（与 WorkBuddy 同构）：
//
//	ErrSessionDead（401 / invalid_grant）→ VerdictDead    终态，告警要求重登
//	其他错误（网络/5xx/超时）           → VerdictTransient 可重试
//	成功                                → VerdictOK
func (s *traeSubject) Refresh() (keepalive.Verdict, error) {
	// 用 RefreshTokenIfNeeded + skew=0 强制刷新：skew=0 时
	// 「now + 0 < ExpiresAt」对未过期 token 仍成立，会提前返回 false。
	// 所以这里直接调底层强制刷新路径。
	if err := s.forceRefresh(); err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			return keepalive.VerdictDead, err
		}
		return keepalive.VerdictTransient, err
	}
	return keepalive.VerdictOK, nil
}

// forceRefresh 无条件刷新一次 token。
//
// 为什么不用 RefreshTokenIfNeeded：它的 skew 语义是「剩余寿命 < skew 才刷」，
// 传 0 会对未过期 token 直接返回 (false, nil)，拿不到「强制刷新」的效果。
// 这里用 NeedsRefresh(极大值) 判定恒真来复用同一条刷新路径。
func (s *traeSubject) forceRefresh() error {
	// 传一个远超任何 token 寿命的窗口 → NeedsRefresh 恒为真 → 必然执行刷新。
	const always = 100 * 365 * 24 * time.Hour
	_, err := s.up.RefreshTokenIfNeeded(s.a, always)
	return err
}

func (s *traeSubject) Save() error { return s.a.SaveAtomic() }

// traeAlertSink 把 keepalive 的告警写进 Trae 账号池状态（面板可见）。
type traeAlertSink struct {
	p *pool.Pool
}

func (s *traeAlertSink) MarkDead(uid, msg string) {
	s.p.MarkSessionDeadAlert(uid, msg)
}

func (s *traeAlertSink) ClearDead(uid string) {
	s.p.ClearSessionDeadAlert(uid)
}

// buildSubjects 把当前账号池转成 keepalive.Subject 列表。
//
// includeDisabled=false 时跳过已禁用账号（与既有保活行为一致）。
func (s *Scheduler) buildSubjects() []keepalive.Subject {
	includeDisabled := s.cfg.IncludeDisabledInTasks
	out := make([]keepalive.Subject, 0, len(s.cfg.Pool.List()))
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		out = append(out, &traeSubject{a: a, up: s.cfg.Upstream, skew: s.cfg.RefreshSkew})
	}
	return out
}

// newKeepaliveRunner 构建保活编排器（含告警 sink）。
func (s *Scheduler) newKeepaliveRunner() *keepalive.Runner {
	return keepalive.NewRunner(keepalive.Policy{
		Interval:     s.cfg.KeepaliveInterval,
		Retry:        s.cfg.KeepaliveRetry,
		RetryBudget:  s.cfg.KeepaliveRetryBudget,
		VerifyWithin: s.cfg.KeepaliveVerifyWithin,
	}, &traeAlertSink{p: s.cfg.Pool})
}

// runKeepaliveViaRunner 用抽象层执行一轮保活，并把结果折算成 Summary。
//
// force=true：整点排程语义（无条件刷，含闲置号续期）
// force=false：间隔循环语义（按需刷，省上游调用）
func (s *Scheduler) runKeepaliveViaRunner(force bool) Summary {
	subjects := s.buildSubjects()
	results := s.newKeepaliveRunner().RunOnce(s.ctxOf(), subjects, force)

	var sum Summary
	for _, r := range results {
		sum.Total++
		if r.Skipped {
			sum.OK++ // 跳过不算失败（剩余寿命充裕）
			continue
		}
		if r.Verdict == keepalive.VerdictOK {
			sum.OK++
			sum.Results = append(sum.Results, Result{UID: r.UID, OK: true, Msg: "保活完成"})
			continue
		}
		sum.Failed++
		errMsg := ""
		if r.Err != nil {
			errMsg = r.Err.Error()
		}
		sum.Results = append(sum.Results, Result{UID: r.UID, Err: errMsg})
	}
	return sum
}
