package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/auth"
)

func newAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, Nickname: "n-" + uid, AccessToken: "AT"}
}

func TestAddAndSync(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.Add(newAuth("u2"))
	if len(p.List()) != 2 {
		t.Fatalf("应有 2 个账号，实为 %d", len(p.List()))
	}
	// SyncToDir 与凭证目录对齐：消失的账号剔除
	p.SyncToDir([]*auth.Auth{newAuth("u1")})
	if len(p.List()) != 1 {
		t.Errorf("SyncToDir 未剔除消失的账号，实为 %d", len(p.List()))
	}
	// Add 已存在的账号：保留状态、替换凭证
	p.SetEnabled("u1", false, "停用")
	p.Add(newAuth("u1"))
	st, _ := p.Status("u1")
	if st.Enabled {
		t.Error("Add 已存在账号不该重置启用状态")
	}
}

func TestHealthyAndPick(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	// 默认启用 → 可选号
	if a := p.Pick(); a == nil || a.UID != "u1" {
		t.Fatalf("应能选到号: %v", a)
	}
	// 冷却中不参与选号
	p.Cooldown("u1", CoolSoft, time.Hour, "429")
	if a := p.Pick(); a != nil {
		t.Error("冷却中的账号不该被选中")
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "429" {
		t.Errorf("冷却状态不对: %+v", st)
	}
	// 禁用不参与选号
	p.Revive("u1")
	p.Disable("u1", "session dead")
	if a := p.Pick(); a != nil {
		t.Error("禁用的账号不该被选中")
	}
	// 解冻恢复
	p.Revive("u1")
	if a := p.Pick(); a == nil {
		t.Error("解冻后应能选号")
	}
}

// 会话粘性：同一 sessionKey 应绑定同一账号，且冷却后自动改绑。
func TestPickAffinity(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.Add(newAuth("u2"))
	first := p.PickAffinity("sess-1", nil)
	if first == nil {
		t.Fatal("应选到号")
	}
	for i := 0; i < 5; i++ {
		if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != first.UID {
			t.Fatalf("粘性失效：%v ≠ %v", got, first)
		}
	}
	// 绑定号冷却 → 自动改绑到另一个健康号
	p.Cooldown(first.UID, CoolSoft, time.Hour, "429")
	alt := p.PickAffinity("sess-1", nil)
	if alt == nil || alt.UID == first.UID {
		t.Errorf("冷却后应改绑: %v", alt)
	}
	// tried 排除：被排除的号不再返回
	tried := map[string]bool{alt.UID: true}
	third := p.PickAffinity("sess-2", tried)
	if third != nil && third.UID == alt.UID {
		t.Error("tried 里的号不该被返回")
	}
}

// 积分更多的号在加权里占优（三因子权重的第一项）。
func TestPickPrefersHigherCredits(t *testing.T) {
	p := New("")
	p.Add(newAuth("poor"))
	p.Add(newAuth("rich"))
	p.SetUsage("poor", 1, 100, 99, 1)
	p.SetUsage("rich", 1000, 1000, 0, 1)
	// 加权随机不是硬排序，跑多轮看倾向
	richHit := 0
	for i := 0; i < 50; i++ {
		if a := p.Pick(); a != nil && a.UID == "rich" {
			richHit++
		}
	}
	if richHit < 35 {
		t.Errorf("高积分号应显著占优，实为 %d/50", richHit)
	}
}

func TestInFlightLimits(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.Add(newAuth("u1"))
	p.Add(newAuth("u2"))
	if !p.Acquire("u1") {
		t.Fatal("首次 Acquire 应成功")
	}
	// u1 已满载 → 选号应改选 u2
	if a := p.Pick(); a == nil || a.UID != "u2" {
		t.Errorf("满载号应被跳过，实为 %v", a)
	}
	p.Release("u1")
	if a := p.Pick(); a == nil {
		t.Error("释放后应能选号")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	// 阈值 3：前两次只累计，第三次触发冷却
	p.NoteError("u1", 3, time.Hour)
	if st, _ := p.Status("u1"); st.Cooling {
		t.Error("第 1 次错误不该冷却")
	}
	p.NoteError("u1", 3, time.Hour)
	if st, _ := p.Status("u1"); st.Cooling {
		t.Error("第 2 次错误不该冷却")
	}
	p.NoteError("u1", 3, time.Hour)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Error("第 3 次错误应触发冷却")
	}
	if st.Reason != "consecutive errors" {
		t.Errorf("冷却原因不对: %q", st.Reason)
	}
	// 成功清零（只是 errStreak，冷却本身要等过期或 Revive）
	p.Revive("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	if st, _ := p.Status("u1"); st.Cooling {
		t.Error("成功清零后不该在第二次错误就冷却")
	}
}

// TestNoteSuccessClearsReason 钉住「成功后清掉 reason 残留」。
//
// ── 回归背景（2026-10-18 用户看到「consecutive errors」）──────────────
//
// 现象：账号在面板上显示 reason="consecutive errors"，看着像有故障，
// 但实际刷额度/对话全部正常，且 until 早已过期 39 小时。
//
// 根因：NoteError 写入 reason + until；冷却到期后 Cooling 按
// now.Before(until) 实时算 → false（账号恢复调度），但 **reason 字符串
// 没人清**（只有手动 Revive 会清）。于是留下「已过期的 until + 残留
// reason」这种半清理状态，重启也会被 load 原样恢复。
//
// 修法：成功是「账号已恢复」的最强信号，NoteSuccess 顺带清 until + reason。
func TestNoteSuccessClearsReason(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))

	// 触发冷却
	p.NoteError("u1", 1, time.Hour)
	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "consecutive errors" {
		t.Fatalf("应先进入冷却，实为 cooling=%v reason=%q", st.Cooling, st.Reason)
	}

	// 一次成功 → reason 必须被清
	p.NoteSuccess("u1")
	st2, _ := p.Status("u1")
	if st2.Reason != "" {
		t.Errorf("成功后应清掉 reason 残留，实为 %q（面板会误显示故障）", st2.Reason)
	}
	if st2.Cooling {
		t.Error("成功后不该仍处于冷却")
	}
}

// TestNoteSuccessDoesNotClearDisabled 成功不能悄悄放开显式停用的账号。
//
// disabled 是人工/熔断的显式状态，只应由 Revive 放开；
// NoteSuccess 只负责清「连败/冷却」这类运行时残留。
func TestNoteSuccessDoesNotClearDisabled(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.Disable("u1", "人工停用")

	p.NoteSuccess("u1")

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Error("NoteSuccess 不该清 disabled（那是 Revive 的职责）")
	}
}

// TestLoadClearsExpiredCooldownReason 钉住「加载时自愈已过期的冷却残留」。
//
// 历史台账里可能存着「until 已过去 + reason 还在」的半清理状态。
// 重启后 load 原样恢复会让面板一直显示故障原因，故加载时顺手清掉。
func TestLoadClearsExpiredCooldownReason(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "state.json")

	// 造一份「冷却已过期但 reason 残留」的台账
	past := time.Now().Add(-39 * time.Hour).Format(time.RFC3339)
	raw := `{"accounts":{"u1":{"credits":100,"reason":"consecutive errors","until":"` + past + `"}}}`
	if err := os.WriteFile(sf, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(sf)
	p.Add(newAuth("u1"))

	st, _ := p.Status("u1")
	if st.Reason != "" {
		t.Errorf("已过期的冷却残留应在加载时清掉，实为 %q", st.Reason)
	}
	if st.Cooling {
		t.Error("已过期的冷却不该仍算 cooling")
	}
}

// TestLoadKeepsActiveCooldownReason 未过期的冷却**必须保留** reason。
//
// 自愈只针对「已过期」的残留；仍在冷却期内的账号要如实显示原因，
// 否则用户会误以为账号可用却被调度器跳过。
func TestLoadKeepsActiveCooldownReason(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "state.json")

	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	raw := `{"accounts":{"u1":{"credits":100,"reason":"consecutive errors","until":"` + future + `"}}}`
	if err := os.WriteFile(sf, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(sf)
	p.Add(newAuth("u1"))

	st, _ := p.Status("u1")
	if st.Reason != "consecutive errors" {
		t.Errorf("未过期的冷却应保留 reason，实为 %q", st.Reason)
	}
	if !st.Cooling {
		t.Error("未过期的冷却应仍算 cooling")
	}
}

func TestSetUsageAndAutoUnfreeze(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	// 权益不足冷却
	p.Cooldown("u1", CoolPlan, 12*time.Hour, "plan 权益不足")
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatal("应处于冷却")
	}
	// 余额回血 → 自动解冻（这是签到回血后能立刻恢复选号的关键）
	p.SetUsage("u1", 500, 1000, 500, 2)
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Error("余额恢复后应自动解冻")
	}
	if st.Credits != 500 || st.Limit != 1000 || st.Used != 500 || st.Packs != 2 {
		t.Errorf("额度三元组不对: %+v", st)
	}
}

// 签到状态按 Asia/Shanghai 自然日归档，跨日自动失效。
func TestCheckedInPerDay(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	if p.TodayCheckedIn("u1") {
		t.Error("初始不该是已签到")
	}
	p.SetCheckedIn("u1", true)
	if !p.TodayCheckedIn("u1") {
		t.Error("标记后应为已签到")
	}
	if st, _ := p.Status("u1"); !st.CheckedIn {
		t.Error("状态快照应带出已签到")
	}
}

func TestRemoveAndCounts(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.Add(newAuth("u2"))
	p.Disable("u2", "dead")
	total, healthy, _, disabled := p.Counts()
	if total != 2 || healthy != 1 || disabled != 1 {
		t.Errorf("计数不对: total=%d healthy=%d disabled=%d", total, healthy, disabled)
	}
	if !p.Remove("u1") {
		t.Error("Remove 应返回 true")
	}
	if p.Remove("nope") {
		t.Error("不存在的账号 Remove 应返回 false")
	}
}

// 状态持久化：重启后冷却/禁用/积分都要还在。
func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p1 := New(fp)
	p1.Add(newAuth("u1"))
	p1.SetUsage("u1", 777, 1000, 223, 3)
	p1.Cooldown("u1", CoolPlan, 12*time.Hour, "plan 权益不足")
	p1.SetCheckedIn("u1", true)
	p1.Flush()

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("状态文件未落盘: %v", err)
	}
	p2 := New(fp)
	p2.Add(newAuth("u1")) // 重新挂上凭证（启动时由 SyncToDir 完成）
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("状态未恢复")
	}
	if st.Credits != 777 {
		t.Errorf("积分未恢复: %d", st.Credits)
	}
	if !st.Cooling || st.Reason != "plan 权益不足" {
		t.Errorf("冷却未恢复: %+v", st)
	}
	if !st.CheckedIn {
		t.Error("签到状态未恢复")
	}
}

// 禁用状态也要持久化：session 失效是跨重启的事实，不该被重置。
func TestStatePersistsDisabled(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p1 := New(fp)
	p1.Add(newAuth("u1"))
	p1.Disable("u1", "session dead")
	p1.Flush()

	p2 := New(fp)
	p2.Add(newAuth("u1"))
	st, _ := p2.Status("u1")
	if !st.Disabled {
		t.Error("禁用状态未持久化")
	}
	if st.Reason != "session dead" {
		t.Errorf("禁用原因未持久化: %q", st.Reason)
	}
}

func TestStatusOfUnknownUID(t *testing.T) {
	p := New("")
	if _, ok := p.Status("nope"); ok {
		t.Error("未知 uid 应返回 false")
	}
	if p.AuthByUID("nope") != nil {
		t.Error("未知 uid 的凭证应为 nil")
	}
}

// 成功率渲染（面板展示用）不应出现 NaN 或过长小数。
func TestSuccessRateFormat(t *testing.T) {
	p := New("")
	p.Add(newAuth("u1"))
	p.RecordSuccess("u1")
	p.RecordError("u1", "boom")
	st, _ := p.Status("u1")
	if st.SuccessRate != "50%" {
		t.Errorf("成功率渲染不对: %q", st.SuccessRate)
	}
	// 无请求时不显示比率
	p2 := New("")
	p2.Add(newAuth("u2"))
	st2, _ := p2.Status("u2")
	if st2.SuccessRate != "" {
		t.Errorf("无请求时成功率应为空: %q", st2.SuccessRate)
	}
}
