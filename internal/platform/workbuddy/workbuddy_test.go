// workbuddy_test.go WorkBuddy 平台适配层的用例。
//
// 测的是「既有 pool 代码 ↔ 统一契约」这一层的适配正确性：
// 账号归一化、可选能力实现、动作派发。选号/冷却/协议行为由 pool 与
// upstream 自己的测试覆盖，不在这里重复。
package workbuddy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"allplatforms/internal/platform"
	"allplatforms/internal/platform/workbuddy/auth"
	"allplatforms/internal/platform/workbuddy/pool"
)

// newTestPlatform 造一个带若干账号的 WorkBuddy 平台（无调度器/无 handler）。
func newTestPlatform(t *testing.T, uids ...string) (*Platform, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, Nickname: "n-" + uid})
	}
	return New(Options{Pool: p, Prefix: "/workbuddy"}), p
}

// ── 基本契约 ────────────────────────────────────────────────────────────

func TestPlatformBasics(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	if pl.Kind() != platform.KindWorkBuddy {
		t.Errorf("Kind=%q", pl.Kind())
	}
	if pl.DisplayName() != "WorkBuddy" {
		t.Errorf("DisplayName=%q", pl.DisplayName())
	}
	if pl.ModelsPath() != "/workbuddy/v1" {
		t.Errorf("ModelsPath=%q", pl.ModelsPath())
	}
}

// 前缀缺省时回落 "/workbuddy"（不能是空串，否则 Register 会挂到根路径）。
func TestPlatformDefaultPrefix(t *testing.T) {
	pl := New(Options{Pool: pool.New("")})
	if pl.ModelsPath() != "/workbuddy/v1" {
		t.Errorf("缺省 ModelsPath=%q", pl.ModelsPath())
	}
}

// ── 路由挂载 ────────────────────────────────────────────────────────────

// Register 必须挂在前缀下，并且裸前缀要重定向到带斜杠的形态。
func TestPlatformRegisterMountsUnderPrefix(t *testing.T) {
	pl := New(Options{
		Pool:    pool.New(""),
		Prefix:  "/workbuddy",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 走到这里说明前缀已被剥掉：既有 handler 看到的是它原本的路径。
			if r.URL.Path != "/v1/models" {
				t.Errorf("StripPrefix 后路径应为 /v1/models，实为 %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	})
	mux := http.NewServeMux()
	pl.Register(mux)

	// 前缀下的子路径应转发到平台 handler
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/workbuddy/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("前缀内请求应 200，实为 %d", rec.Code)
	}

	// 裸前缀应 301 到带斜杠（下游少打一个斜杠时不至于 404）
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/workbuddy", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("裸前缀应 301，实为 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/workbuddy/" {
		t.Errorf("重定向目标=%q", loc)
	}
}

// ── 账号归一化 ──────────────────────────────────────────────────────────

func TestPlatformAccountsNormalized(t *testing.T) {
	pl, p := newTestPlatform(t, "u1", "u2")
	p.SetCredits("u1", 900, 1000)

	accs := pl.Accounts()
	if len(accs) != 2 {
		t.Fatalf("应有 2 个账号，实为 %d", len(accs))
	}
	for _, a := range accs {
		if a.Platform != platform.KindWorkBuddy {
			t.Errorf("Platform=%q", a.Platform)
		}
		if a.Status == "" || a.Label == "" {
			t.Errorf("status/label 不该为空: %+v", a)
		}
	}
	// WorkBuddy 侧刻意不填 Used（只有剩余+总额，相减没有独立信息量）
	for _, a := range accs {
		if a.Used != 0 {
			t.Errorf("WorkBuddy 不该编造已用量: %+v", a)
		}
	}
}

// 状态优先级：disabled > paused > cooling > ok（与账号池视图一致）。
func TestStatusClassification(t *testing.T) {
	cases := []struct {
		name string
		s    pool.Status
		want string
	}{
		{"禁用优先", pool.Status{Disabled: true, Paused: true, Cooling: true}, "disabled"},
		{"暂停优先于冷却", pool.Status{Paused: true, Cooling: true}, "paused"},
		{"冷却", pool.Status{Cooling: true}, "cooling"},
		{"正常", pool.Status{}, "ok"},
	}
	for _, c := range cases {
		if got := wbStatusClass(c.s); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	labels := map[string]string{"disabled": "已禁用", "paused": "已暂停", "cooling": "冷却中", "ok": "可用"}
	for cls, want := range labels {
		var s pool.Status
		switch cls {
		case "disabled":
			s.Disabled = true
		case "paused":
			s.Paused = true
		case "cooling":
			s.Cooling = true
		}
		if got := wbStatusLabel(s); got != want {
			t.Errorf("%s 标签=%q want %q", cls, got, want)
		}
	}
}

// ── 可选能力（能力探测）─────────────────────────────────────────────────

// Status 必须被能力探测发现（缺了它面板会以为平台"什么都没有"）。
func TestPlatformImplementsStatusProvider(t *testing.T) {
	pl, p := newTestPlatform(t, "u1", "u2", "u3")
	p.Disable("u2", "test")

	sp, ok := platform.Platform(pl).(platform.StatusProvider)
	if !ok {
		t.Fatal("WorkBuddy 平台必须实现 StatusProvider")
	}
	st := sp.Status()
	if st["total"] != 3 {
		t.Errorf("total=%v want 3", st["total"])
	}
	if st["disabled"] != 1 {
		t.Errorf("disabled=%v want 1", st["disabled"])
	}
	if _, has := st["paused"]; !has {
		t.Errorf("应含 paused 计数（与账号池视图同口径）: %+v", st)
	}
}

// AccountActioner 必须被能力探测发现。
func TestPlatformImplementsAccountActioner(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	if _, ok := platform.Platform(pl).(platform.AccountActioner); !ok {
		t.Fatal("WorkBuddy 平台必须实现 AccountActioner")
	}
}

// 能力探测的整体结果：status + account_action 应为 true；
// login/import/raw_models 应为 false（WorkBuddy 没有这些能力）。
func TestProbeCapabilities(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	caps := platform.ProbeCapabilities(pl)
	if !caps.Status {
		t.Error("status 能力应为 true")
	}
	if !caps.AccountAction {
		t.Error("account_action 能力应为 true")
	}
	if caps.Login || caps.Import || caps.RawModels {
		t.Errorf("WorkBuddy 不该声称具备 login/import/raw_models: %+v", caps)
	}
}

// ── 单账号动作 ──────────────────────────────────────────────────────────

func TestAccountActionLifecycle(t *testing.T) {
	pl, p := newTestPlatform(t, "u1")

	// disable → 账号进入 disabled
	code, body, err := pl.AccountAction("u1", "disable", nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("disable: code=%d err=%v", code, err)
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Errorf("disable 后应为 disabled: %+v", st)
	}

	// revive → 恢复
	if code, _, err = pl.AccountAction("u1", "revive", nil); err != nil || code != http.StatusOK {
		t.Fatalf("revive: code=%d err=%v", code, err)
	}
	if st, _ := p.Status("u1"); st.Disabled {
		t.Errorf("revive 后不该仍 disabled: %+v", st)
	}

	// pause → paused
	if code, _, err = pl.AccountAction("u1", "pause", nil); err != nil || code != http.StatusOK {
		t.Fatalf("pause: code=%d err=%v", code, err)
	}
	if st, _ := p.Status("u1"); !st.Paused {
		t.Errorf("pause 后应为 paused: %+v", st)
	}

	// resume → 取消暂停
	if code, _, err = pl.AccountAction("u1", "resume", nil); err != nil || code != http.StatusOK {
		t.Fatalf("resume: code=%d err=%v", code, err)
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("resume 后不该仍 paused: %+v", st)
	}

	// remove → 账号消失
	if code, _, err = pl.AccountAction("u1", "remove", nil); err != nil || code != http.StatusOK {
		t.Fatalf("remove: code=%d err=%v", code, err)
	}
	if p.AuthByUID("u1") != nil {
		t.Error("remove 后账号不该还在池里")
	}
	_ = body
}

// 未知账号必须 404（不能静默成功）。
func TestAccountActionUnknownUID(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	code, body, err := pl.AccountAction("nosuch", "disable", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if code != http.StatusNotFound {
		t.Errorf("未知账号应 404，实为 %d (%v)", code, body)
	}
}

// 单号签到/刷新额度必须明确 501 —— **不能**偷偷跑一次全量任务。
//
// 这是本层最重要的语义约束：WorkBuddy 调度器只有全量入口，
// 若把「点了单号」实现成「跑一遍所有账号」，用户完全看不出来。
func TestAccountActionCheckinRefusesInsteadOfRunningAll(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1", "u2")
	code, body, err := pl.AccountAction("u1", "checkin", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if code != http.StatusNotImplemented {
		t.Errorf("单号签到应 501（明确拒绝），实为 %d", code)
	}
	m, _ := body.(map[string]any)
	if m["supported"] != false {
		t.Errorf("应标 supported=false: %v", m)
	}
}

// 未知动作名必须 400。
func TestAccountActionUnknownAction(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	code, _, err := pl.AccountAction("u1", "explode", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if code != http.StatusBadRequest {
		t.Errorf("未知动作应 400，实为 %d", code)
	}
}

// 池未接线时如实报 501（装配错误，不能静默成功）。
func TestAccountActionNoPool(t *testing.T) {
	pl := New(Options{})
	code, _, err := pl.AccountAction("u1", "disable", nil)
	if err == nil {
		t.Fatal("无池时应返回错误")
	}
	if code != http.StatusNotImplemented {
		t.Errorf("无池应 501，实为 %d", code)
	}
}

// ── 批量动作 ────────────────────────────────────────────────────────────

// 无调度器时如实报错（而不是假装成功）。
func TestTriggerWithoutScheduler(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	res := pl.Trigger(platform.ActionCheckin, "")
	if res.OK {
		t.Errorf("无调度器不该报成功: %+v", res)
	}
	if res.Error != "scheduler not available" {
		t.Errorf("error=%q", res.Error)
	}
}

// 未知动作必须标记 supported=false（能力缺口，不是错误）。
func TestTriggerUnknownAction(t *testing.T) {
	pl, _ := newTestPlatform(t, "u1")
	res := pl.Trigger(platform.Action("nonsense"), "")
	if res.Supported {
		t.Errorf("未知动作不该标 supported: %+v", res)
	}
}

// 池未接线时如实报错。
func TestTriggerNoPool(t *testing.T) {
	pl := New(Options{})
	res := pl.Trigger(platform.ActionCheckin, "")
	if res.OK || res.Error == "" {
		t.Errorf("无池应报错: %+v", res)
	}
}
