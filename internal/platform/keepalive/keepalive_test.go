// 保活抽象层的回归测试。
//
// 覆盖四层防护的核心语义：预筛、重试、预算、告警、ctx 取消。
package keepalive

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSubject 测试用的账号桩。
type fakeSubject struct {
	uid    string
	label  string
	// verdicts 依次返回的结果；用完后一直返回最后一个。
	verdicts []Verdict
	calls    int
	needs    bool // NeedsRefresh 的返回值
	saved    int
}

func (f *fakeSubject) UID() string   { return f.uid }
func (f *fakeSubject) Label() string { return f.label }
func (f *fakeSubject) NeedsRefresh(time.Duration) bool { return f.needs }
func (f *fakeSubject) Save() error {
	f.saved++
	return nil
}
func (f *fakeSubject) Refresh() (Verdict, error) {
	v := VerdictTransient
	if f.calls < len(f.verdicts) {
		v = f.verdicts[f.calls]
	} else if len(f.verdicts) > 0 {
		v = f.verdicts[len(f.verdicts)-1]
	}
	f.calls++
	if v == VerdictDead {
		return v, errors.New("12153 invalid")
	}
	if v == VerdictTransient {
		return v, errors.New("transient")
	}
	return v, nil
}

// fakeSink 记录告警调用。
type fakeSink struct {
	marked  map[string]string
	cleared map[string]int
}

func newFakeSink() *fakeSink {
	return &fakeSink{marked: map[string]string{}, cleared: map[string]int{}}
}
func (f *fakeSink) MarkDead(uid, msg string) { f.marked[uid] = msg }
func (f *fakeSink) ClearDead(uid string)     { f.cleared[uid]++ }

// fastPolicy 测试用策略：不重试（避免等待）。
func fastPolicy() Policy {
	return Policy{Retry: []time.Duration{}, RetryBudget: time.Second}
}

// TestRunOnceSuccessSavesAndClears 刷新成功 → 落盘 + 清告警。
func TestRunOnceSuccessSavesAndClears(t *testing.T) {
	sink := newFakeSink()
	r := NewRunner(fastPolicy(), sink)
	s := &fakeSubject{uid: "u1", verdicts: []Verdict{VerdictOK}}

	res := r.RunOnce(context.Background(), []Subject{s}, true)

	if len(res) != 1 || res[0].Verdict != VerdictOK {
		t.Fatalf("期望 1 个 OK 结果，实为 %+v", res)
	}
	if s.saved != 1 {
		t.Errorf("成功应落盘 1 次，实为 %d", s.saved)
	}
	if sink.cleared["u1"] != 1 {
		t.Errorf("成功应清告警，实为 %d", sink.cleared["u1"])
	}
}

// TestRunOnceDeadMarksAlert 凭证已死 → 立即告警（不重试）。
func TestRunOnceDeadMarksAlert(t *testing.T) {
	sink := newFakeSink()
	r := NewRunner(fastPolicy(), sink)
	s := &fakeSubject{uid: "u1", verdicts: []Verdict{VerdictDead}}

	res := r.RunOnce(context.Background(), []Subject{s}, true)

	if res[0].Verdict != VerdictDead {
		t.Fatalf("期望 Dead，实为 %v", res[0].Verdict)
	}
	if _, ok := sink.marked["u1"]; !ok {
		t.Errorf("Dead 必须立即告警（这是四层防护的核心）")
	}
	if s.calls != 1 {
		t.Errorf("Dead 是终态，不该重试，实为 %d 次调用", s.calls)
	}
	if s.saved != 0 {
		t.Errorf("Dead 不该落盘成功凭证")
	}
}

// TestRunOnceTransientRetriesThenSucceeds 瞬时失败重试后成功。
func TestRunOnceTransientRetriesThenSucceeds(t *testing.T) {
	sink := newFakeSink()
	r := NewRunner(Policy{
		Retry:       []time.Duration{time.Millisecond, time.Millisecond},
		RetryBudget: time.Second,
	}, sink)
	s := &fakeSubject{uid: "u1", verdicts: []Verdict{VerdictTransient, VerdictTransient, VerdictOK}}

	res := r.RunOnce(context.Background(), []Subject{s}, true)

	if res[0].Verdict != VerdictOK {
		t.Errorf("重试后应成功，实为 %v", res[0].Verdict)
	}
	if s.calls != 3 {
		t.Errorf("应调用 3 次（1 次 + 2 次重试），实为 %d", s.calls)
	}
}

// TestRunOnceTransientExhaustedNoAlert 瞬时失败重试耗尽 → 不告警（非终态）。
func TestRunOnceTransientExhaustedNoAlert(t *testing.T) {
	sink := newFakeSink()
	r := NewRunner(Policy{
		Retry:       []time.Duration{time.Millisecond},
		RetryBudget: time.Second,
	}, sink)
	s := &fakeSubject{uid: "u1", verdicts: []Verdict{VerdictTransient}}

	res := r.RunOnce(context.Background(), []Subject{s}, true)

	if res[0].Verdict != VerdictTransient {
		t.Errorf("应为 Transient，实为 %v", res[0].Verdict)
	}
	if len(sink.marked) != 0 {
		t.Errorf("瞬时失败不该告警（否则每轮抖动都报警）: %v", sink.marked)
	}
}

// TestRunOnceBudgetStopsRetry 预算耗尽后停止重试。
//
// 防止「多账号同时失败」把一轮保活拖成 N×12 分钟。
func TestRunOnceBudgetStopsRetry(t *testing.T) {
	r := NewRunner(Policy{
		Retry:       []time.Duration{time.Hour}, // 远超预算
		RetryBudget: 10 * time.Millisecond,      // 极小预算
	}, nil)
	s := &fakeSubject{uid: "u1", verdicts: []Verdict{VerdictTransient}}

	start := time.Now()
	r.RunOnce(context.Background(), []Subject{s}, true)
	elapsed := time.Since(start)

	if s.calls != 1 {
		t.Errorf("预算不足时不该重试，实为 %d 次调用", s.calls)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("预算应阻止长等待，实际耗时 %v", elapsed)
	}
}

// TestRunOnceForceSkipsPrescreen force=true 绕过预筛（整点排程语义）。
func TestRunOnceForceSkipsPrescreen(t *testing.T) {
	r := NewRunner(fastPolicy(), nil)
	s := &fakeSubject{uid: "u1", needs: false, verdicts: []Verdict{VerdictOK}}

	r.RunOnce(context.Background(), []Subject{s}, true)
	if s.calls != 1 {
		t.Errorf("force=true 应无条件刷新，实为 %d 次调用", s.calls)
	}
}

// TestRunOnceNonForceAppliesPrescreen force=false 应用预筛（间隔循环语义）。
func TestRunOnceNonForceAppliesPrescreen(t *testing.T) {
	r := NewRunner(fastPolicy(), nil)
	s := &fakeSubject{uid: "u1", needs: false, verdicts: []Verdict{VerdictOK}}

	res := r.RunOnce(context.Background(), []Subject{s}, false)
	if s.calls != 0 {
		t.Errorf("剩余寿命充裕时应跳过，实为 %d 次调用", s.calls)
	}
	if !res[0].Skipped {
		t.Errorf("应标记为 Skipped")
	}
}

// TestRunOnceNonForceRefreshesWhenNeeded 需要刷新时预筛放行。
func TestRunOnceNonForceRefreshesWhenNeeded(t *testing.T) {
	r := NewRunner(fastPolicy(), nil)
	s := &fakeSubject{uid: "u1", needs: true, verdicts: []Verdict{VerdictOK}}

	r.RunOnce(context.Background(), []Subject{s}, false)
	if s.calls != 1 {
		t.Errorf("需要刷新时应执行，实为 %d 次调用", s.calls)
	}
}

// TestRunOnceContextCancelStopsEarly ctx 取消 → 立即停止。
func TestRunOnceContextCancelStopsEarly(t *testing.T) {
	r := NewRunner(fastPolicy(), nil)
	subjects := []Subject{
		&fakeSubject{uid: "u1", verdicts: []Verdict{VerdictOK}},
		&fakeSubject{uid: "u2", verdicts: []Verdict{VerdictOK}},
		&fakeSubject{uid: "u3", verdicts: []Verdict{VerdictOK}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	res := r.RunOnce(ctx, subjects, true)
	if len(res) != 0 {
		t.Errorf("ctx 已取消应不处理任何账号，实为 %d", len(res))
	}
}

// TestPolicyNormalize 零值策略应补成默认值。
func TestPolicyNormalize(t *testing.T) {
	p := Policy{}.normalize()

	if p.Interval != DefaultInterval {
		t.Errorf("默认间隔应为 %v，实为 %v", DefaultInterval, p.Interval)
	}
	if len(p.Retry) != len(DefaultRetry) {
		t.Errorf("默认重试档数应为 %d，实为 %d", len(DefaultRetry), len(p.Retry))
	}
	if p.RetryBudget != DefaultRetryBudget {
		t.Errorf("默认预算应为 %v，实为 %v", DefaultRetryBudget, p.RetryBudget)
	}
	if p.VerifyWithin != DefaultVerifyWithin {
		t.Errorf("默认验证窗口应为 %v，实为 %v", DefaultVerifyWithin, p.VerifyWithin)
	}
}

// TestPolicyEmptyRetryDisables 空切片 [] 表示不重试（与 nil 区分）。
func TestPolicyEmptyRetryDisables(t *testing.T) {
	p := Policy{Retry: []time.Duration{}}.normalize()
	if len(p.Retry) != 0 {
		t.Errorf("空切片应保持不重试，实为 %d 档", len(p.Retry))
	}
}

// TestVerdictString 分类的字符串形式（日志与断言用）。
func TestVerdictString(t *testing.T) {
	cases := map[Verdict]string{
		VerdictOK:        "ok",
		VerdictDead:      "dead",
		VerdictTransient: "transient",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Errorf("%d.String()=%q want %q", v, got, want)
		}
	}
}

// TestStartLoopIntervalZeroDisabled 间隔 <= 0 不启动。
func TestStartLoopIntervalZeroDisabled(t *testing.T) {
	r := NewRunner(Policy{Interval: -1, Retry: []time.Duration{}}, nil)
	done := make(chan struct{})
	go func() {
		r.StartLoop(context.Background(), func() []Subject { return nil })
		close(done)
	}()
	select {
	case <-done:
		// 立即返回 = 未启动，正确
	case <-time.After(200 * time.Millisecond):
		t.Errorf("间隔 <= 0 应立即返回，不该进入循环")
	}
}

// TestStartLoopTicks 间隔循环应按周期执行。
func TestStartLoopTicks(t *testing.T) {
	r := NewRunner(Policy{
		Interval: 20 * time.Millisecond,
		Retry:    []time.Duration{},
	}, nil)
	s := &fakeSubject{uid: "u1", needs: true, verdicts: []Verdict{VerdictOK}}

	ctx, cancel := context.WithCancel(context.Background())
	go r.StartLoop(ctx, func() []Subject { return []Subject{s} })
	time.Sleep(100 * time.Millisecond)
	cancel()

	if s.calls < 2 {
		t.Errorf("100ms 内应至少触发 2 次，实为 %d", s.calls)
	}
}

// TestAsDeadIsDeadErr 死亡标记的辅助函数。
func TestAsDeadIsDeadErr(t *testing.T) {
	base := errors.New("12153 invalid")
	marked := AsDead(base)

	if !IsDeadErr(marked) {
		t.Errorf("AsDead 标记后应被 IsDeadErr 识别")
	}
	if IsDeadErr(base) {
		t.Errorf("未标记的错误不该被识别为 dead")
	}
	if !errors.Is(marked, base) {
		t.Errorf("应保留原始错误（errors.Join 语义）")
	}
}
