// 保活自动化四层防护的回归测试（2026-10-23）
//
// 覆盖：Layer 1 间隔循环 / Layer 2 即时告警 / Layer 3 重试 / Layer 4 提前验证
package scheduler

import (
	"context"
	"testing"
	"time"

	"allplatforms/internal/platform/workbuddy/auth"
	"allplatforms/internal/platform/workbuddy/pool"
	"allplatforms/internal/platform/workbuddy/upstream"
)

// TestKeepaliveRetryOnTransientFailure Layer 3：非确定性失败应重试并最终成功。
//
// 场景：上游前 2 次刷新返回 500（瞬时故障），第 3 次成功。
// 期望：保活不放弃，重试后刷新成功、账号不被禁用。
func TestKeepaliveRetryOnTransientFailure(t *testing.T) {
	f := &fakeUpstream{refreshFailTimes: 2, refreshFailStatus: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	// 剩余寿命很短（触发 Layer 4 的预筛），但整点排程走 force=true 不受影响
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:     p,
		Upstream: up,
		// 重试间隔设为极短，避免测试变慢
		KeepaliveRetry: []time.Duration{time.Millisecond, time.Millisecond},
	})

	s.RunKeepaliveNow() // force=true：无条件刷新

	if got := f.refreshCalls.Load(); got < 3 {
		t.Errorf("应重试到成功（≥3 次调用），实为 %d", got)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Errorf("瞬时故障重试成功后不该被禁用: %+v", st)
	}
	if st.SessionDeadAlert {
		t.Errorf("刷新成功应清除告警")
	}
}

// TestKeepaliveSessionDeadAlertsImmediately Layer 2：12153 应立即告警（不等 3 次）。
//
// 这是本次事故的核心修复：refreshToken 失效后，用户必须在 accessToken
// 到期**之前**就看到提示，而不是等突然 503。
func TestKeepaliveSessionDeadAlertsImmediately(t *testing.T) {
	f := &fakeUpstream{refreshSessionDead: true}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRetry: []time.Duration{}})

	s.RunKeepaliveNow()

	st, _ := p.Status("u1")
	// 第一次 12153 就应该有告警（虽然此时还不该禁用 —— 3 次阈值未到）
	if !st.SessionDeadAlert {
		t.Errorf("12153 应立即产生告警（不等 3 次计数），实为 false")
	}
	if st.SessionDeadMsg == "" {
		t.Errorf("告警应带详情信息")
	}
	if st.Disabled {
		t.Errorf("第一次 12153 不该立即禁用（防误杀，需 3 次）: %+v", st)
	}
}

// TestKeepaliveSessionDeadNoRetry Layer 3：12153 是确定性失败，不应重试。
//
// 重试确定性失败只会放大上游调用量而结果不变。
func TestKeepaliveSessionDeadNoRetry(t *testing.T) {
	f := &fakeUpstream{refreshSessionDead: true}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		KeepaliveRetry: []time.Duration{time.Millisecond, time.Millisecond},
	})

	s.RunKeepaliveNow()

	if got := f.refreshCalls.Load(); got != 1 {
		t.Errorf("12153 不该重试，应恰好 1 次调用，实为 %d", got)
	}
}

// TestKeepaliveAlertClearedOnSuccess 刷新成功后告警必须清除。
func TestKeepaliveAlertClearedOnSuccess(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.MarkSessionDeadAlert("u1", "previous failure")

	st, _ := p.Status("u1")
	if !st.SessionDeadAlert {
		t.Fatalf("前置条件：应有告警")
	}

	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRetry: []time.Duration{}})

	s.RunKeepaliveNow()

	st2, _ := p.Status("u1")
	if st2.SessionDeadAlert {
		t.Errorf("刷新成功应清除告警，实为仍存在: %s", st2.SessionDeadMsg)
	}
}

// TestKeepaliveIntervalLoopRuns Layer 1：间隔循环应按间隔触发。
//
// 注意：间隔循环走 runKeepalive(false)，会跳过「剩余寿命充裕」的账号。
// 所以这里必须把 ExpiresAt 设成**即将到期**（now+1h），否则循环会一直跳过、
// 测不出触发次数。这也顺带验证了 Layer 4 的预筛确实在间隔循环里生效。
func TestKeepaliveIntervalLoopRuns(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	// 1 小时后到期 → 剩余寿命 < 默认 7 天窗口 → 间隔循环会刷新
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix()})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRetry: []time.Duration{}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 30ms 间隔，跑 ~120ms 应触发至少 2 次
	s.StartKeepaliveLoop(ctx, 30*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	cancel()

	if got := f.refreshCalls.Load(); got < 1 {
		t.Errorf("间隔循环应至少触发 1 次，实为 %d", got)
	}
}

// TestKeepaliveIntervalZeroDisables 间隔 <= 0 时不启动循环（保持旧行为）。
func TestKeepaliveIntervalZeroDisables(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, KeepaliveRetry: []time.Duration{}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartKeepaliveLoop(ctx, 0) // 不启动
	time.Sleep(80 * time.Millisecond)

	if got := f.refreshCalls.Load(); got != 0 {
		t.Errorf("间隔 0 不该启动循环，实为 %d 次调用", got)
	}
}

// TestKeepaliveVerifyWithinSkipsFreshTokens Layer 4：间隔循环跳过剩余寿命充裕的账号。
//
// 这是「省上游调用」的关键：4h 循环若无条件刷新，一天会白打 6 次上游。
func TestKeepaliveVerifyWithinSkipsFreshTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	// ExpiresAt 远在未来（2286 年）→ 剩余寿命 > 7 天 → 间隔循环应跳过
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:                  p,
		Upstream:              up,
		KeepaliveRetry:        []time.Duration{},
		KeepaliveVerifyWithin: 7 * 24 * time.Hour,
	})

	// force=false（间隔循环语义）：应跳过
	s.runKeepalive(false)
	if got := f.refreshCalls.Load(); got != 0 {
		t.Errorf("剩余寿命充裕时间隔循环应跳过，实为 %d 次调用", got)
	}

	// force=true（整点排程语义）：应无条件刷新（保持旧行为）
	s.RunKeepaliveNow()
	if got := f.refreshCalls.Load(); got != 1 {
		t.Errorf("整点排程应无条件刷新，实为 %d 次调用", got)
	}
}

// TestKeepaliveDefaultsAreSane 默认参数必须合理（防止未来误改）。
func TestKeepaliveDefaultsAreSane(t *testing.T) {
	s := New(Config{})

	if s.cfg.KeepaliveInterval != 4*time.Hour {
		t.Errorf("默认保活间隔应为 4h，实为 %v", s.cfg.KeepaliveInterval)
	}
	if len(s.cfg.KeepaliveRetry) != 3 {
		t.Errorf("默认重试序列应有 3 档，实为 %d", len(s.cfg.KeepaliveRetry))
	}
	if s.cfg.KeepaliveVerifyWithin != 7*24*time.Hour {
		t.Errorf("默认验证窗口应为 7 天，实为 %v", s.cfg.KeepaliveVerifyWithin)
	}
}
