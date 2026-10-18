// guard_test.go 任务级互斥的用例。
//
// 钉住的是一个**实测踩出来的真实根因**（2026-10-14）：
// Trae 3 个账号自动签到稳定只 1 个成功，而手动点却 3 个全成功。
// 根因是 CheckinAll 有 4 个触发源且无互斥 → 两条链并行跑同一账号，
// 各自的 9074 退避重试互相占住上游闸门（自我踩踏）→ 一起超时失败。
// 补签机制在手动场景下掩盖了这个问题。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/upstream"
)

// ── taskGuard 本身 ──────────────────────────────────────────────────────

func TestTaskGuardTryAcquire(t *testing.T) {
	g := newTaskGuard()

	if !g.TryAcquire("a") {
		t.Fatal("首次获取应成功")
	}
	if g.TryAcquire("a") {
		t.Error("同任务重复获取应失败（这正是防并发踩踏的核心）")
	}
	if !g.IsRunning("a") {
		t.Error("IsRunning 应为 true")
	}
	// 不同任务互不影响
	if !g.TryAcquire("b") {
		t.Error("不同任务应可并行获取")
	}

	g.Release("a")
	if g.IsRunning("a") {
		t.Error("Release 后不该仍在执行")
	}
	if !g.TryAcquire("a") {
		t.Error("Release 后应可再次获取")
	}
}

func TestTaskGuardReleaseIdempotent(t *testing.T) {
	g := newTaskGuard()
	g.TryAcquire("a")
	g.Release("a")
	g.Release("a") // 重复 Release 不该 panic，也不该破坏状态
	if g.IsRunning("a") {
		t.Error("重复 Release 后状态应仍是未执行")
	}
	if !g.TryAcquire("a") {
		t.Error("应可再次获取")
	}
}

// 并发争抢：只有 1 个能拿到。
func TestTaskGuardConcurrent(t *testing.T) {
	g := newTaskGuard()
	var won atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if g.TryAcquire("x") {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won.Load() != 1 {
		t.Errorf("并发 50 次争抢应恰好 1 个成功，实为 %d", won.Load())
	}
}

// ── 批量任务互斥（真实场景）────────────────────────────────────────────

// newCheckinSched 造一个调度器 + 假上游（可控制 9074 响应与延迟）。
func newCheckinSched(t *testing.T, handler http.HandlerFunc) (*Scheduler, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{
			UID: uid, AccessToken: "AT-" + uid, RefreshToken: "RT-" + uid,
			MachineID: "M", DeviceID: "1234567890123456",
		})
	}
	up := upstream.New(time.Second, time.Second, time.Second)
	up.SetHosts(srv.URL, srv.URL, srv.URL)
	return New(Config{Pool: p, Upstream: up, RefreshSkew: time.Hour}), p
}

// 核心用例：**第二次触发必须被跳过**，而不是起第二条链。
//
// 这是本 bug 的根因所在：原实现允许多条链并行跑同一账号，
// 各自的退避重试互相占住闸门，最后一起失败。
func TestCheckinAllSecondTriggerIsSkipped(t *testing.T) {
	var claimCalls atomic.Int32
	release := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true}`))
		case strings.Contains(r.URL.Path, "checkin_credits/claim"):
			claimCalls.Add(1)
			<-release // 卡住第一次触发，模拟它正在跑
			_, _ = w.Write([]byte(`{"code":0}`))
		case strings.Contains(r.URL.Path, "ide_user_ent_usage"):
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
	s, _ := newCheckinSched(t, handler)

	// 第一次触发：在 goroutine 里跑（会被 claim 卡住）
	done := make(chan Summary, 1)
	go func() { done <- s.CheckinAll(time.Now()) }()

	// 等它真的进入执行（claim 被调用）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if claimCalls.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if claimCalls.Load() == 0 {
		t.Fatal("第一次触发没有进入 claim")
	}

	// 第二次触发：必须被跳过
	sum2 := s.CheckinAll(time.Now())
	if len(sum2.Results) == 0 {
		t.Fatal("跳过时应返回说明性 Result")
	}
	if !strings.Contains(sum2.Results[0].Msg, "已在执行") {
		t.Errorf("跳过说明文案不对: %q", sum2.Results[0].Msg)
	}
	if sum2.Total != 0 {
		t.Errorf("跳过的触发不该处理任何账号，Total=%d", sum2.Total)
	}

	// 第三次、第四次同样跳过（多个触发源同时打来的场景）
	for i := 0; i < 3; i++ {
		if got := s.CheckinAll(time.Now()); got.Total != 0 {
			t.Errorf("第 %d 次重复触发应跳过，实为 Total=%d", i+3, got.Total)
		}
	}

	close(release)
	<-done // 等第一次跑完

	// 第一次跑完后，应能再次触发（互斥已释放）
	if !s.guard.TryAcquire(taskNameCheckin) {
		t.Error("第一次跑完后互斥应已释放")
	} else {
		s.guard.Release(taskNameCheckin)
	}
}

// 互斥必须覆盖「整个任务生命周期」，包括补签轮次。
//
// 若只在首轮加锁、补签前解锁，两条链仍会在补签阶段互相踩。
//
// 测法说明：不用「循环采样」——任务被压到毫秒级后会瞬间结束，
// 采样循环根本抓不到执行窗口（实测踩到，日志证明互斥是好的但断言失败）。
// 改为在**补签阶段的请求处理函数里**断言互斥仍被持有：
// 那一刻任务一定还在跑，且已越过首轮。
func TestCheckinGuardHeldThroughSweep(t *testing.T) {
	var s *Scheduler
	var heldDuringSweep atomic.Bool
	var sawSweep atomic.Bool
	// 首轮全 9074 → 必然进补签；在补签的 claim 请求里检查互斥状态。
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true}`))
		case strings.Contains(r.URL.Path, "checkin_credits/claim"):
			if sawSweep.Load() {
				// 已在补签阶段：此刻互斥必须仍被持有
				if s.guard.IsRunning(taskNameCheckin) {
					heldDuringSweep.Store(true)
				}
			}
			sawSweep.Store(true)
			_, _ = w.Write([]byte(`{"code":9074,"message":"busy"}`))
		case strings.Contains(r.URL.Path, "ide_user_ent_usage"):
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
	s, _ = newCheckinSched(t, handler)

	// 压到最小，让测试快
	oldBase, oldJitter := checkinBackoffBase, checkinBackoffJitter
	oldStagger, oldStaggerJitter := checkinStagger, checkinStaggerJitter
	oldGap, oldGapJitter := checkinSweepGap, checkinSweepJitter
	oldRounds := checkinSweepRounds
	defer func() {
		checkinBackoffBase, checkinBackoffJitter = oldBase, oldJitter
		checkinStagger, checkinStaggerJitter = oldStagger, oldStaggerJitter
		checkinSweepGap, checkinSweepJitter = oldGap, oldGapJitter
		checkinSweepRounds = oldRounds
	}()
	checkinBackoffBase, checkinBackoffJitter = time.Millisecond, time.Nanosecond
	checkinStagger, checkinStaggerJitter = 0, time.Nanosecond
	checkinSweepGap, checkinSweepJitter = time.Millisecond, time.Nanosecond
	checkinSweepRounds = 2

	s.CheckinAll(time.Now())

	if !sawSweep.Load() {
		t.Fatal("未进入补签阶段（夹具没构造出 9074 全败场景）")
	}
	if !heldDuringSweep.Load() {
		t.Error("补签阶段互斥应仍被持有 —— 否则两条链会在补签时互相踩")
	}
	if s.guard.IsRunning(taskNameCheckin) {
		t.Error("任务结束后互斥应已释放")
	}
}

// KeepaliveAll 同样要互斥。
func TestKeepaliveAllSecondTriggerIsSkipped(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte(`{"Result":{"Token":"AT","RefreshToken":"RT","TokenExpireAt":9999999999999}}`))
	}
	s, _ := newCheckinSched(t, handler)

	done := make(chan Summary, 1)
	go func() { done <- s.KeepaliveAll(time.Now()) }()
	<-entered

	sum := s.KeepaliveAll(time.Now())
	if len(sum.Results) == 0 || !strings.Contains(sum.Results[0].Msg, "已在执行") {
		t.Errorf("保活重复触发应被跳过: %+v", sum)
	}
	close(release)
	<-done
}

// BalanceRefreshNow 同样要互斥。
func TestBalanceRefreshSecondTriggerIsSkipped(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[]}`))
	}
	s, _ := newCheckinSched(t, handler)

	done := make(chan Summary, 1)
	go func() { done <- s.BalanceRefreshNow() }()
	<-entered

	sum := s.BalanceRefreshNow()
	if len(sum.Results) == 0 || !strings.Contains(sum.Results[0].Msg, "已在执行") {
		t.Errorf("余额刷新重复触发应被跳过: %+v", sum)
	}
	close(release)
	<-done
}

// 任务之间互不阻塞（签到在跑不该挡住保活）。
func TestDifferentTasksDoNotBlockEachOther(t *testing.T) {
	s, _ := newCheckinSched(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	if !s.guard.TryAcquire(taskNameCheckin) {
		t.Fatal("获取签到互斥失败")
	}
	defer s.guard.Release(taskNameCheckin)

	// 保活是另一个任务，应能独立获取
	if !s.guard.TryAcquire(taskNameKeepalive) {
		t.Error("签到在跑不该阻塞保活")
	} else {
		s.guard.Release(taskNameKeepalive)
	}
}

// 定时 tick 与手动触发并发时，手动那次应被跳过（真实场景）。
//
// 这正是线上现象：整点 tick 在跑，用户/其他 hook 又触发一次。
func TestTickAndManualTriggerDoNotOverlap(t *testing.T) {
	var claims atomic.Int32
	release := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true}`))
		case strings.Contains(r.URL.Path, "checkin_credits/claim"):
			claims.Add(1)
			<-release
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
	s, _ := newCheckinSched(t, handler)

	// 模拟定时 tick
	go s.CheckinAll(time.Now())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && claims.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// 手动触发（模拟面板按钮 / WorkBuddy hook）
	manual := s.CheckinAll(time.Now())
	if manual.Total != 0 {
		t.Errorf("定时任务在跑时手动触发应被跳过，实为 Total=%d", manual.Total)
	}
	close(release)
}

// 确保 context 引用（本文件未直接用，但保留 import 以便将来扩展）。
var _ = context.Background
