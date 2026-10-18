package pool

import (
	"testing"
	"time"
)

// 这些入口由 app.go 装配层调用（该层是进程启动代码，不适合单测），
// 但它们本身是稳定的公开 API，值得直接锁住行为，防止被误改。

func TestSetWeights(t *testing.T) {
	p := New("")
	p.SetWeights(2.0, 8.0)
	// 直接读字段而不是再拿一次锁：sync.RWMutex 不可重入，
	// 这里 p.mu.RLock() 会与 SetWeights 的写锁互等，直接死锁
	// （本包没有导出型 Getter，单测就按"无并发"的前提直读）。
	if p.idleWeightPerHour != 2.0 || p.idleWeightMax != 8.0 {
		t.Errorf("权重未生效: %v / %v", p.idleWeightPerHour, p.idleWeightMax)
	}
	// 非正值应被忽略（不让脏配置把权重打成 0）
	p.SetWeights(-1, -1)
	if p.idleWeightPerHour != 2.0 || p.idleWeightMax != 8.0 {
		t.Errorf("非法值不该覆盖: %v / %v", p.idleWeightPerHour, p.idleWeightMax)
	}
}

func TestSetNote(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	if !p.SetNote("u1", "轮换中") {
		t.Error("SetNote 应返回 true")
	}
	if st, _ := p.Status("u1"); st.Note != "轮换中" {
		t.Errorf("备注未生效: %q", st.Note)
	}
	// 清空备注也要能写回
	p.SetNote("u1", "")
	if st, _ := p.Status("u1"); st.Note != "" {
		t.Errorf("备注未清空: %q", st.Note)
	}
	if p.SetNote("nope", "x") {
		t.Error("未知账号应返回 false")
	}
}

// 共同管理账号标记：面板靠 shared_with 展示「几个网关共享这一份凭证」。
func TestSetSharedWith(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	if !p.SetSharedWith("u1", []string{"gw-a", "gw-b"}) {
		t.Error("SetSharedWith 应返回 true")
	}
	st, _ := p.Status("u1")
	if st.SharedWith != 2 {
		t.Errorf("共同管理数不对: %d", st.SharedWith)
	}
	if p.SetSharedWith("nope", nil) {
		t.Error("未知账号应返回 false")
	}
	// 清空
	p.SetSharedWith("u1", nil)
	if st, _ := p.Status("u1"); st.SharedWith != 0 {
		t.Errorf("共同管理未清空: %d", st.SharedWith)
	}
}

func TestSetCredits(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.SetCredits("u1", 888)
	if st, _ := p.Status("u1"); st.Credits != 888 {
		t.Errorf("积分未生效: %d", st.Credits)
	}
	// 未知账号静默忽略
	p.SetCredits("nope", 1)
}

func TestAddTokens(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.AddTokens("u1", 100)
	p.AddTokens("u1", 50)
	if st, _ := p.Status("u1"); st.TokensDelta != 150 {
		t.Errorf("token 累计不对: %d", st.TokensDelta)
	}
	// 负数与未知账号不该把计数弄坏
	p.AddTokens("u1", -10)
	p.AddTokens("nope", 5)
	if st, _ := p.Status("u1"); st.TokensDelta != 150 {
		t.Errorf("负数不该计入: %d", st.TokensDelta)
	}
}

func TestCoolKindString(t *testing.T) {
	want := map[CoolKind]string{
		CoolPlan: "plan_limit",
		CoolSoft: "soft_rate",
		CoolErr:  "error_threshold",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("CoolKind(%d) = %q，期望 %q", int(k), k.String(), s)
		}
	}
	if CoolKind(99).String() != "unknown" {
		t.Errorf("未知类型应返回 unknown: %q", CoolKind(99).String())
	}
}

// 连续错误冷却的时长由调用方给定，池不自己决定。
func TestCooldownDurations(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.Cooldown("u1", CoolErr, 90*time.Second, "手动")
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("应处于冷却")
	}
	if d := time.Until(parseTime(t, st.Until)); d <= 0 || d > 90*time.Second {
		t.Errorf("冷却时长不对: %v", d)
	}
}

func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("时间格式不对 %q: %v", s, err)
	}
	return v
}
