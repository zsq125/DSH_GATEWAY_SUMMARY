package scheduler

import (
	"context"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/pool"
)

// contextWithTimeout 建一个带超时的 ctx（测试里到处要用，抽出来）。
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// newTestPool 建一个临时状态文件的池（不写仓库目录）。
func newTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	return pool.New(t.TempDir() + "/state.json")
}

// ── 排程计算（纯函数，不依赖上游）────────────────────────────────────────

func TestUniqueSortedHours(t *testing.T) {
	got := uniqueSortedHours(21, 9, 9, 22, 99, -1)
	want := []int{9, 21, 22}
	if len(got) != len(want) {
		t.Fatalf("去重排序结果长度不对: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项期望 %d，实为 %d", i, want[i], got[i])
		}
	}
}

// 全部任务禁用时 activeHours 为空 → nextDelay 回落 24h（不空转忙等）。
func TestActiveHoursEmptyWhenAllDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, KeepaliveDisabled: true})
	if got := s.activeHours(); len(got) != 0 {
		t.Errorf("全禁用时应无有效时点，实为 %v", got)
	}
	if d := s.nextDelay(); d != 24*time.Hour {
		t.Errorf("无时点应回落 24h，实为 %v", d)
	}
}

// nextDelay 必须落在 (0, 24h]：指向「下一个」整点，不能是负数或 0（0 会忙等）。
func TestNextDelayAlwaysForward(t *testing.T) {
	s := New(Config{CheckinHours: []int{0, 6, 12, 18}, KeepaliveHours: []int{22}})
	for i := 0; i < 24; i++ {
		d := s.nextDelay()
		if d <= 0 {
			t.Fatalf("nextDelay 必须为正，实为 %v", d)
		}
		if d > 24*time.Hour {
			t.Fatalf("nextDelay 不该超过 24h，实为 %v", d)
		}
	}
}

// 当前时刻已过全部时点 → nextDelay 指向明天第一个时点（不能是负数）。
func TestNextDelayWrapsToTomorrow(t *testing.T) {
	now := time.Now()
	// 把唯一时点设成刚过去的那个小时
	past := now.Add(-30 * time.Minute).Hour()
	s := New(Config{CheckinHours: []int{past}, KeepaliveDisabled: true})
	d := s.nextDelay()
	if d <= 0 {
		t.Errorf("已过的时点应指向明天，实为 %v", d)
	}
	if d > 24*time.Hour {
		t.Errorf("不该超过 24h，实为 %v", d)
	}
}

func TestHourIn(t *testing.T) {
	if !hourIn(9, []int{9, 21}) {
		t.Error("9 应在 [9,21] 里")
	}
	if hourIn(10, []int{9, 21}) {
		t.Error("10 不该在 [9,21] 里")
	}
	if hourIn(9, nil) {
		t.Error("空列表不该命中")
	}
}

// Reconfigure 改排程后 activeHours 要立刻变（面板改配置不必重启）。
func TestReconfigure(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	s.Reconfigure([]int{23}, nil, false, true)
	got := s.activeHours()
	if len(got) != 1 || got[0] != 23 {
		t.Errorf("Reconfigure 未生效: %v", got)
	}
	// 禁用的任务不该再进 activeHours
	s.Reconfigure([]int{23}, []int{1}, true, true)
	if len(s.activeHours()) != 0 {
		t.Errorf("全禁用后应为空: %v", s.activeHours())
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

// itoa 是汇总消息里的小工具（避免引 strconv 只对一处）。
func TestItoa(t *testing.T) {
	if got := itoa(42); got != "42" {
		t.Errorf("itoa(42) = %q", got)
	}
	if got := itoa(0); got != "0" {
		t.Errorf("itoa(0) = %q", got)
	}
	if got := itoa(-7); got != "-7" {
		t.Errorf("itoa(-7) = %q", got)
	}
}
