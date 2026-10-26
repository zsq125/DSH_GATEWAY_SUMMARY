// Trae 保活适配层（对接通用 keepalive 抽象）的回归测试。
//
// 覆盖 2026-10-24 排查发现并修复的三个 Trae 缺陷：
//  1. 只有整点排程 → 补间隔循环（发现窗口 24h → 4h）
//  2. 失败只写日志 → 补即时告警（面板可见）
//  3. 401 直接 Disable → 补 Verdict 分类（可重试 vs 终态）
package scheduler

import (
	"testing"
	"time"

	"allplatforms/internal/platform/keepalive"
)

// TestTraeSubjectNeedsRefreshUsesSkewNotVerifyWithin 钉住关键差异：
// Trae 的刷新窗口是 RefreshSkew（24h），不是 WorkBuddy 的 VerifyWithin（7d）。
//
// 回归背景：接入抽象层时误用 VerifyWithin(168h)，导致「剩余 100 小时的
// token」也被刷新，破坏 TestKeepaliveSkipsFreshToken 的既有约定。
func TestTraeSubjectNeedsRefreshUsesSkewNotVerifyWithin(t *testing.T) {
	s, _, p := newSched(t, 1)
	acct := p.AuthByUID("u0")
	acct.Lock()
	acct.ExpiresAt = time.Now().Add(100 * time.Hour).Unix() // 100h：>24h 但 <168h
	acct.Unlock()

	sub := &traeSubject{a: acct, up: s.cfg.Upstream, skew: 24 * time.Hour}

	// 传 7 天窗口，但内部应取 min(168h, 24h)=24h → 100h 剩余不该刷
	if sub.NeedsRefresh(7 * 24 * time.Hour) {
		t.Errorf("剩余 100h 的 token 不该被判为需要刷新（Trae 窗口是 24h，不是 7 天）")
	}

	// 剩余 10h（<24h）应该刷
	acct.Lock()
	acct.ExpiresAt = time.Now().Add(10 * time.Hour).Unix()
	acct.Unlock()
	if !sub.NeedsRefresh(7 * 24 * time.Hour) {
		t.Errorf("剩余 10h 的 token 应判为需要刷新")
	}
}

// TestTraeKeepaliveAllRespectsWindow 整点保活尊重 24h 窗口（不无条件刷新）。
func TestTraeKeepaliveAllRespectsWindow(t *testing.T) {
	s, f, p := newSched(t, 1)
	acct := p.AuthByUID("u0")
	acct.Lock()
	acct.ExpiresAt = time.Now().Add(100 * time.Hour).Unix()
	acct.Unlock()

	sum := s.KeepaliveAll(time.Now())

	if sum.OK != 1 {
		t.Errorf("应算完成: %+v", sum)
	}
	if f.nExchange.Load() != 0 {
		t.Errorf("未到 24h 窗口不该换 token，实为 %d 次", f.nExchange.Load())
	}
}

// TestTraeKeepaliveDeadMarksAlert 凭证已死（401）→ 立即告警（面板可见）。
//
// 这是修复 Trae 缺陷 2 的核心：原先只 log.Printf，用户看不到。
func TestTraeKeepaliveDeadMarksAlert(t *testing.T) {
	s, f, p := newSched(t, 1)
	f.exchangeOK = false
	f.exchangeDead = true // 模拟 401 凭证失效

	// 把 token 设成即将过期，让预筛放行
	acct := p.AuthByUID("u0")
	acct.Lock()
	acct.ExpiresAt = time.Now().Add(time.Hour).Unix()
	acct.Unlock()

	s.KeepaliveAll(time.Now())

	st, ok := p.Status("u0")
	if !ok {
		t.Fatalf("账号应存在")
	}
	if !st.SessionDeadAlert {
		t.Errorf("凭证失效必须立即产生告警（原先只写日志，面板不可见）")
	}
	if st.SessionDeadMsg == "" {
		t.Errorf("告警应带详情")
	}
}

// TestTraeKeepaliveAlertClearedOnSuccess 刷新成功 → 清告警。
func TestTraeKeepaliveAlertClearedOnSuccess(t *testing.T) {
	s, _, p := newSched(t, 1)
	p.MarkSessionDeadAlert("u0", "previous failure")

	st, _ := p.Status("u0")
	if !st.SessionDeadAlert {
		t.Fatalf("前置：应有告警")
	}

	acct := p.AuthByUID("u0")
	acct.Lock()
	acct.ExpiresAt = time.Now().Add(time.Hour).Unix()
	acct.Unlock()

	s.KeepaliveAll(time.Now())

	st2, _ := p.Status("u0")
	if st2.SessionDeadAlert {
		t.Errorf("刷新成功应清告警，实为仍存在: %q", st2.SessionDeadMsg)
	}
}

// TestTraeBuildSubjectsSkipsDisabled 默认跳过禁用账号（与既有行为一致）。
func TestTraeBuildSubjectsSkipsDisabled(t *testing.T) {
	s, _, p := newSched(t, 2)
	p.Disable("u1", "test disable")

	subs := s.buildSubjects()
	if len(subs) != 1 {
		t.Errorf("默认应跳过禁用账号（剩 1 个），实为 %d", len(subs))
	}
	if len(subs) > 0 && subs[0].UID() != "u0" {
		t.Errorf("应只剩 u0，实为 %s", subs[0].UID())
	}
}

// TestTraeBuildSubjectsIncludesDisabledWhenConfigured 开关打开时覆盖禁用号。
func TestTraeBuildSubjectsIncludesDisabledWhenConfigured(t *testing.T) {
	s, _, p := newSched(t, 2)
	p.Disable("u1", "test disable")

	s.cfg.IncludeDisabledInTasks = true
	subs := s.buildSubjects()
	if len(subs) != 2 {
		t.Errorf("开关打开时应包含禁用号（2 个），实为 %d", len(subs))
	}
}

// TestTraeRunnerPolicyUsesDefaults 策略零值应回落抽象层默认值。
func TestTraeRunnerPolicyUsesDefaults(t *testing.T) {
	s, _, _ := newSched(t, 1)
	r := s.newKeepaliveRunner()
	p := r.Policy()

	if p.Interval != keepalive.DefaultInterval {
		t.Errorf("默认间隔应为 %v，实为 %v", keepalive.DefaultInterval, p.Interval)
	}
	if p.VerifyWithin != keepalive.DefaultVerifyWithin {
		t.Errorf("默认验证窗口应为 %v，实为 %v", keepalive.DefaultVerifyWithin, p.VerifyWithin)
	}
	if len(p.Retry) != len(keepalive.DefaultRetry) {
		t.Errorf("默认重试档数应为 %d，实为 %d", len(keepalive.DefaultRetry), len(p.Retry))
	}
}

// TestTraeKeepaliveSummaryCounts 汇总口径：跳过不算失败。
func TestTraeKeepaliveSummaryCounts(t *testing.T) {
	s, _, p := newSched(t, 2)

	// 两个账号都设成剩余充裕 → 都会被跳过
	for _, uid := range []string{"u0", "u1"} {
		a := p.AuthByUID(uid)
		if a != nil {
			a.Lock()
			a.ExpiresAt = time.Now().Add(200 * time.Hour).Unix()
			a.Unlock()
		}
	}

	sum := s.KeepaliveAll(time.Now())
	if sum.Total != 2 {
		t.Errorf("Total 应为 2，实为 %d", sum.Total)
	}
	if sum.Failed != 0 {
		t.Errorf("跳过不算失败，Failed 应为 0，实为 %d", sum.Failed)
	}
	if sum.OK != 2 {
		t.Errorf("跳过的应计为 OK，实为 %d", sum.OK)
	}
}
