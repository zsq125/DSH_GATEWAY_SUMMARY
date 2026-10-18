// Package scheduler Trae 定时任务：签到、token 保活、余额后台刷新。
//
// 语义与 WorkBuddy2API 对齐：本地时区整点触发、账号间限速、按天幂等。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/upstream"
)

// 签到 claim 的退避重试参数。用包级变量便于测试覆写。
//
// 9074 处理哲学（2026-10-12 实测复核）：9074「当前参与用户太多」是上游容量闸门，
// 秒级放开——当场带短退避就地重试即可签上（实测同一 token 连续换 did 全部成功，
// 且间隔数秒后原本 9074 的账号自动恢复）。故 CheckinOne 对 9074 做有界重试，
// 不靠换指纹（实测换 machineId/deviceId/重登均无效）。这两个参数即单次 claim 重试退避。
var (
	checkinBackoffBase   = 15 * time.Second
	checkinBackoffJitter = 15 * time.Second
)

// 9074 打散与补签参数（2026-10 实测修复）。
// 9074 是上游签到端点的全局并发闸门，多账号在整点同时发起只会一起被拒，
// 故：①账号间用带抖动的间隔错开到达时刻；②本轮内对仍 9074 的账号再补几轮，
// 趁闸门秒级放开把它签上；③仍失败才交状态机软冷却（冷却已改为分钟级封顶）。
var (
	checkinStagger       = 4 * time.Second
	checkinStaggerJitter = 5 * time.Second
	checkinSweepRounds   = 3
	checkinSweepGap      = 20 * time.Second
	checkinSweepJitter   = 10 * time.Second
)

// Config 调度器依赖。
type Config struct {
	Pool              *pool.Pool
	Upstream          *upstream.Client
	CheckinHours      []int
	KeepaliveHours    []int
	CheckinDisabled   bool
	KeepaliveDisabled bool
	RefreshSkew       time.Duration
	BalanceEnabled    bool
	BalanceInterval   time.Duration
}

// Scheduler 定时调度器。
type Scheduler struct {
	cfg Config
	mu  sync.Mutex
	// guard 批量任务级互斥（见 guard.go）：防止多个触发源并发跑同一任务，
	// 那会让账号自己踩自己（9074 是全局闸门）。
	guard *taskGuard
}

// New 构建调度器。
func New(cfg Config) *Scheduler {
	return &Scheduler{cfg: cfg, guard: newTaskGuard()}
}

// uniqueSortedHours 去重排序合法小时（0-23）。
func uniqueSortedHours(hs ...int) []int {
	m := map[int]bool{}
	for _, h := range hs {
		if h >= 0 && h <= 23 {
			m[h] = true
		}
	}
	out := make([]int, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	sort.Ints(out)
	return out
}

// activeHours 当前生效的排程时点（禁用任务的小时不计入，避免空转唤醒）。
func (s *Scheduler) activeHours() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hs []int
	if !s.cfg.CheckinDisabled {
		hs = append(hs, s.cfg.CheckinHours...)
	}
	if !s.cfg.KeepaliveDisabled {
		hs = append(hs, s.cfg.KeepaliveHours...)
	}
	return uniqueSortedHours(hs...)
}

// Reconfigure 热更新排程（面板改配置立即生效）。
func (s *Scheduler) Reconfigure(checkin, keepalive []int, checkinDisabled, keepaliveDisabled bool) {
	s.mu.Lock()
	s.cfg.CheckinHours = checkin
	s.cfg.KeepaliveHours = keepalive
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.mu.Unlock()
}

// Run 阻塞式主循环；ctx 取消即退出。全部任务关闭时不空转。
func (s *Scheduler) Run(ctx context.Context) {
	if s.cfg.CheckinDisabled && s.cfg.KeepaliveDisabled {
		log.Printf("[scheduler] 签到与保活均已禁用，等待退出信号")
		<-ctx.Done()
		return
	}
	for {
		d := s.nextDelay()
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

// nextDelay 距下一个执行时点的时长；无有效时点返回 24h。
func (s *Scheduler) nextDelay() time.Duration {
	hours := s.activeHours()
	if len(hours) == 0 {
		return 24 * time.Hour
	}
	now := time.Now()
	for _, h := range hours {
		next := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if next.After(now) {
			return next.Sub(now)
		}
	}
	first := hours[0]
	next := time.Date(now.Year(), now.Month(), now.Day(), first, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	return next.Sub(now)
}

func hourIn(h int, hours []int) bool {
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

// tick 到点执行对应任务。
func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	doCheckin := !s.cfg.CheckinDisabled && hourIn(now.Hour(), s.cfg.CheckinHours)
	doKeepalive := !s.cfg.KeepaliveDisabled && hourIn(now.Hour(), s.cfg.KeepaliveHours)
	s.mu.Unlock()

	if doCheckin {
		log.Printf("[scheduler] %s 签到排程触发", now.Format("15:04"))
		s.CheckinAll(now)
	}
	if doKeepalive {
		log.Printf("[scheduler] %s 保活排程触发", now.Format("15:04"))
		s.KeepaliveAll(now)
	}
}

// Summary 批量任务汇总。
type Summary struct {
	Total   int      `json:"total"`
	OK      int      `json:"ok"`
	Skipped int      `json:"skipped"`
	Failed  int      `json:"failed"`
	Results []Result `json:"results,omitempty"`
}

// Result 单账号结果。
type Result struct {
	UID     string `json:"uid"`
	Name    string `json:"name,omitempty"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped"`
	Msg     string `json:"msg,omitempty"`
	Err     string `json:"error,omitempty"`
}

// CheckinAll 全量签到：今日已签跳过；签到后回查余额刷新池。
// 失败按稳态状态机分流：9074→软冷却退避（下周期重试）；连败→降权；5xx→熔断。
//
// 9074 说明（2026-10-14 直连上游探针实测，修正了原先「全局并发闸门」的误判）：
//
//	9074 的真实含义是「当前参与用户太多，请稍后再试」，它**同时覆盖两种情形**：
//	  ① 该 (账号, deviceId) 组合的设备指纹被风控标记 → 换新 did 立刻可签；
//	  ② 上游真的在限流 → 只能退避等待。
//
// 直连实测（同账号同时刻，只换 did）：
//
//	原 did → 9074；换随机 did → code:0 成功。   ← 情形①
//	另一账号换 did 后仍 9074（多次间隔重试均失败）。 ← 情形②
//
// 因此原实现「只退避、不换 did」只能解决情形②，对情形①会白等 90 秒后失败，
// 表现为「某账号今天怎么都签不上」。现由 CheckinOne 每轮 9074 都换 did 后重试。
//
// 账号间错开到达（下面的 stagger）仍然保留：它防的是情形②的真限流，
// 与「换 did 自愈情形①」是两件互补的事，不是替代关系。
//
// 并发触发修复（2026-10-14 实测确认，见 guard.go）：本函数有 4 个触发源且
// 原先无互斥 —— 两条链并行跑同一账号时，各自的退避重试会互相占住闸门，
// 表现为日志里出现重复的退避序号、最后一起 90 秒超时。
// 现在由 guard 保证同一时刻只有一条 CheckinAll 在跑；后来的触发直接
// 返回「跳过（已在执行）」，不再起第二条链。
func (s *Scheduler) CheckinAll(now time.Time) Summary {
	// 任务级互斥：拿不到就说明已在执行，直接跳过。
	// 跳过语义而非排队：签到按天幂等，重复跑没有增量收益；
	// 且排队会让后到的触发白等 90s，期间闸门仍被前一条链占用。
	if !s.guard.TryAcquire(taskNameCheckin) {
		log.Printf("checkin: 已有签到任务在执行，本次触发跳过（防并发踩踏）")
		return Summary{Total: 0, Skipped: 0,
			Results: []Result{{Msg: "已在执行，本次触发跳过（防 9074 并发踩踏）"}}}
	}
	defer s.guard.Release(taskNameCheckin)

	var sum Summary
	var pending []*auth.Auth // 首轮 9074 的账号，留待补签

	for _, st := range s.cfg.Pool.List() {
		sum.Total++
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			sum.Failed++
			sum.Results = append(sum.Results, Result{UID: st.UID, Err: "no credential"})
			continue
		}
		if s.cfg.Pool.TodayCheckedIn(st.UID) {
			log.Printf("checkin %s: 今日已签到，跳过", st.UID)
			sum.Skipped++
			sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, Skipped: true, Msg: "今日已签到"})
			continue
		}
		credits, msg, code, err := CheckinOne(s.cfg.Upstream, s.cfg.Pool, a)
		if err != nil {
			if code == upstream.CheckinCodeBusy {
				// 9074：不当场判死，收进补签队列，等闸门放开再签。
				pending = append(pending, a)
				log.Printf("checkin %s: 9074 闸门繁忙，转入补签队列", st.UID)
			} else {
				log.Printf("checkin %s: ERR %v", st.UID, err)
				s.classifyCheckinErr(st, err, &sum)
				sum.Failed++
			}
		} else {
			log.Printf("checkin %s: ok %s (credits=%d)", st.UID, msg, credits)
			s.cfg.Pool.SoftCoolClear(st.UID)
			s.cfg.Pool.RecordSuccess(st.UID)
			sum.OK++
			sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, OK: true, Msg: msg})
		}
		// 账号间错开到达时刻（带抖动），防同时撞 9074 闸门，也防风控。
		time.Sleep(checkinStagger + time.Duration(rand.Int63n(int64(checkinStaggerJitter))))
	}

	sum = s.sweepBusy(pending, sum)
	return sum
}

// sweepBusy 对首轮 9074 的账号做有界补签：轮间隔开 + 抖动，逐轮重试，
// 直到签上或轮次用尽。闸门秒级放开，错开后基本都能签上。
func (s *Scheduler) sweepBusy(pending []*auth.Auth, sum Summary) Summary {
	for round := 1; round <= checkinSweepRounds && len(pending) > 0; round++ {
		time.Sleep(checkinSweepGap + time.Duration(rand.Int63n(int64(checkinSweepJitter))))
		var still []*auth.Auth
		for _, a := range pending {
			if s.cfg.Pool.TodayCheckedIn(a.UID) {
				continue // 已被其他路径签上
			}
			credits, msg, code, err := CheckinOne(s.cfg.Upstream, s.cfg.Pool, a)
			if err == nil {
				log.Printf("checkin %s: 补签第%d轮 ok %s (credits=%d)", a.UID, round, msg, credits)
				s.cfg.Pool.SoftCoolClear(a.UID)
				s.cfg.Pool.RecordSuccess(a.UID)
				sum.OK++
				sum.Results = append(sum.Results, Result{UID: a.UID, OK: true, Msg: msg + fmt.Sprintf(" (补签第%d轮)", round)})
				continue
			}
			if code == upstream.CheckinCodeBusy {
				still = append(still, a)
				continue
			}
			log.Printf("checkin %s: 补签第%d轮 ERR %v", a.UID, round, err)
			st, _ := s.cfg.Pool.Status(a.UID)
			s.classifyCheckinErr(st, err, &sum)
			sum.Failed++
		}
		pending = still
	}

	// 补签轮次用尽仍 9074：交状态机软冷却（分钟级封顶，当天还能再试）。
	for _, a := range pending {
		log.Printf("checkin %s: ERR %v", a.UID, errors.New("签到失败(code=9074): 补签轮次用尽"))
		st, _ := s.cfg.Pool.Status(a.UID)
		until := s.cfg.Pool.SoftCoolForCheckin(a.UID)
		log.Printf("checkin %s: 9074 软冷却至 %s，稍后自动重试", a.UID, until.Format("15:04"))
		sum.Results = append(sum.Results, Result{UID: a.UID, Name: st.Nickname, Skipped: true,
			Msg: "9074 软冷却，稍后重试"})
		sum.Failed++
	}
	return sum
}

// classifyCheckinErr 非 9074 失败的稳态分流：5xx→熔断；其他→连败降权。
func (s *Scheduler) classifyCheckinErr(st pool.Status, err error, sum *Summary) {
	switch {
	case isUpstreamServer(err):
		s.cfg.Pool.NoteBreaker(st.UID)
		sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, Err: err.Error()})
	default:
		s.cfg.Pool.NoteFailures(st.UID)
		sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, Err: err.Error()})
	}
}

// claimOnce 单账号单次签到尝试（不爆发重试）：刷 token → 查状态 → 单次
// claim。返回积分、说明、业务码与错误。业务码：0=成功；
// CheckinCodeAlready(10001)=今日已签（幂等成功）；CheckinCodeBusy(9074)=
// 服务端容量闸门/限流；其余非 0 为失败。
func claimOnce(up *upstream.Client, p *pool.Pool, a *auth.Auth) (int64, string, int, error) {
	if a.RefreshTokenValue() != "" {
		if _, err := up.RefreshTokenIfNeeded(a, time.Hour); err == nil {
			_ = a.SaveAtomic()
		}
	}
	checked, _, enable, serr := up.CheckinStatus(a)
	if serr != nil {
		return 0, "", 0, serr
	}
	if checked {
		p.SetCheckedIn(a.UID, true)
		_ = refreshCredits(up, p, a)
		return 0, "今日已签到（上游确认）", upstream.CheckinCodeAlready, nil
	}
	if !enable {
		// 签到未开启也补一次额度。
		//
		// 原先这里直接 return，导致「没开签到的账号」在台账里永远没有 credits
		// —— 面板显示积分 0，看着像账号没额度，其实只是没查。
		// 与 checkinQuietImpl 同款缺陷（2026-10-14 一并修掉）：
		// 额度是独立于签到状态的信息，任何分支拿到都该回写。
		_ = refreshCredits(up, p, a)
		return 0, "签到未开启", 0, nil
	}
	code, cerr := up.CheckinClaim(a)
	if cerr == nil {
		p.SetCheckedIn(a.UID, true)
		credits := refreshCredits(up, p, a)
		if credits > 0 {
			return credits, "签到成功", 0, nil
		}
		return 0, "签到成功", 0, nil
	}
	if code == upstream.CheckinCodeAlready {
		p.SetCheckedIn(a.UID, true)
		_ = refreshCredits(up, p, a)
		return 0, "今日已签到（上游确认）", upstream.CheckinCodeAlready, nil
	}
	return 0, "", code, cerr
}

// CheckinOne 单账号签到：claimOnce 单次尝试（含刷 token→查状态→claim）。
//
// ── 9074 的真实语义（2026-10-14 直连上游探针实测，**推翻了原先的「并发闸门」结论**）
//
// 直连 api.trae.cn 实测（同一账号、同一时刻、只换 did）：
//
//	did=7383620021578383（凭证里的原值） → {"code":9074,"message":"当前参与用户太多，请稍后再试"}
//	did=2844462291979032（随机新值）      → {"code":0,"message":"success"}   ← 立刻成功
//
// 所以 9074 里**包含**「设备指纹被上游风控标记」这一情形，且换 did 能当场自愈。
// 原实现只做退避重试（不换 did），于是一个被标记的 did 会被反复重试到 90 秒超时，
// 表现为「这个账号今天怎么都签不上」，而换 did 的账号（或已签的账号）却正常。
//
// 实测还发现 9074 **同时**可能是真的服务端限流（另一账号换 did 后仍持续 9074），
// 故修复必须两件事都做：
//  1. 每次 9074 都换一个全新随机 did 再试（自愈被标记的设备指纹）；
//  2. 仍失败则退避重试（应对真实限流）。
//
// 10001「今日已签」按幂等成功；5xx/其他失败不重试、直接透传给上层分流。
// 返回 (积分, 说明, 业务code, 错误)：成功/幂等成功 err==nil；其余 err 非 nil 且 code 透传。
func CheckinOne(up *upstream.Client, p *pool.Pool, a *auth.Auth) (int64, string, int, error) {
	const maxBusyRetries = 5
	backoff := checkinBackoffBase
	var lastCode int
	var lastErr error
	for attempt := 0; attempt <= maxBusyRetries; attempt++ {
		credits, msg, code, cerr := claimOnce(up, p, a)
		if cerr == nil {
			return credits, msg, code, nil
		}
		if code == upstream.CheckinCodeAlready {
			return 0, "今日已签到（上游确认）", code, nil
		}
		lastCode, lastErr = code, cerr
		if code != upstream.CheckinCodeBusy {
			// 非限流类错误（5xx/网络/其他业务码）不重试，直接透传。
			break
		}
		if attempt == maxBusyRetries {
			break
		}
		// 9074：先换 did 自愈（上游对「被标记的设备指纹」的放行路径），再退避重试。
		//
		// 为什么每次都换而不是只换一次：上游按 (token, deviceId) 组合判定，
		// 换一个新 did 就是一次全新的设备指纹；若新 did 也被拒（真限流），
		// 下一轮再换一个即可。did 是纯随机 16 位数字，成本为零。
		if _, rerr := a.ResetDeviceID(); rerr != nil {
			log.Printf("checkin %s: 换 did 失败（继续用原 did 重试）: %v", a.UID, rerr)
		} else {
			log.Printf("checkin %s: 9074 换新 did 后重试 (%d/%d)", a.UID, attempt+1, maxBusyRetries)
		}
		jitterNanos := checkinBackoffJitter
		if jitterNanos < time.Nanosecond {
			jitterNanos = time.Nanosecond // 避免 rand.Int63n(0) panic（测试把退避压成 0 时）
		}
		jitter := time.Duration(rand.Int63n(int64(jitterNanos)))
		time.Sleep(backoff + jitter)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return 0, "", lastCode, fmt.Errorf("签到失败(code=%d): %w", lastCode, lastErr)
}

// isUpstreamServer 判上游 5xx（熔断触发条件）。
func isUpstreamServer(err error) bool {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return ue.Kind == upstream.ErrServer
	}
	return false
}

// KeepaliveAll 全量 token 保活：到期窗口内主动刷新并原子落盘。
//
// 与 CheckinAll 同款任务级互斥（见 guard.go）：保活也要逐号打上游
//（每号 500ms 限速），并发触发同样会互相挤，只是后果比签到轻
//（不涉及 9074 闸门，最多白打请求）。
func (s *Scheduler) KeepaliveAll(now time.Time) Summary {
	if !s.guard.TryAcquire(taskNameKeepalive) {
		log.Printf("keepalive: 已有保活任务在执行，本次触发跳过")
		return Summary{Results: []Result{{Msg: "已在执行，本次触发跳过"}}}
	}
	defer s.guard.Release(taskNameKeepalive)

	var sum Summary
	for _, st := range s.cfg.Pool.List() {
		sum.Total++
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			sum.Failed++
			continue
		}
		refreshed, err := s.cfg.Upstream.RefreshTokenIfNeeded(a, s.cfg.RefreshSkew)
		if err != nil {
			log.Printf("keepalive %s: ERR %v", st.UID, err)
			sum.Failed++
			sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, Err: err.Error()})
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if refreshed {
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("keepalive %s: save ERR %v", st.UID, serr)
			} else {
				log.Printf("keepalive %s: token 已刷新并落盘", st.UID)
			}
		}
		sum.OK++
		sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, OK: true, Msg: "保活完成"})
		time.Sleep(500 * time.Millisecond)
	}
	return sum
}

// RefreshBalances 全量刷新余额（顺序 + 轻限速），返回汇总。
func RefreshBalances(up *upstream.Client, p *pool.Pool) Summary {
	var sum Summary
	for _, st := range p.List() {
		sum.Total++
		a := p.AuthByUID(st.UID)
		if a == nil {
			sum.Failed++
			continue
		}
		remain, limit, used, packs, err := up.EntUsage(a)
		if err != nil {
			log.Printf("[balance] %s: ERR %v", st.UID, err)
			sum.Failed++
			sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, Err: err.Error()})
		} else {
			p.SetUsage(st.UID, remain, limit, used, packs)
			sum.OK++
			sum.Results = append(sum.Results, Result{UID: st.UID, Name: st.Nickname, OK: true, Msg: "credits=" + itoa(remain)})
		}
		time.Sleep(200 * time.Millisecond) // 轻限速，避免短时间打爆 ug 端点
	}
	return sum
}

// StartBalanceRefresh 启动余额后台周期刷新协程；interval<=0 不启动。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		log.Printf("[balance] 已启用周期刷新：每 %s", interval)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RefreshBalances(s.cfg.Upstream, s.cfg.Pool)
			}
		}
	}()
}

// refreshCredits 查余额并写入池，返回剩余积分。
func refreshCredits(up *upstream.Client, p *pool.Pool, a *auth.Auth) int64 {
	remain, limit, used, packs, err := up.EntUsage(a)
	if err != nil {
		return 0
	}
	p.SetUsage(a.UID, remain, limit, used, packs)
	return remain
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
