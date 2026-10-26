// session_dead_alert 生命周期的回归测试（2026-10-23 自查发现的问题）。
//
// 自查发现的三个缺陷：
//  1. 前端不显示 session_dead_alert → 告警在 UI 上不可见（Layer 2 实际失效）
//  2. ReviveDisabled / Revive 不清告警 → 手工复活后面板仍提示「需重新登录」
//  3. NoteSuccess 不清告警 → 对话成功的账号仍显示「需重新登录」，自相矛盾
package pool

import (
	"testing"
	"time"

	"allplatforms/internal/platform/workbuddy/auth"
)

func poolWithOne() *Pool {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return p
}

// TestReviveDisabledClearsSessionDeadAlert 手工复活必须清告警。
//
// 场景：refreshToken 失效 → 告警 → 用户重新登录/手工复活 → 告警应消失。
// 若不清，面板会一直显示「需重新登录」，与实际状态矛盾。
func TestReviveDisabledClearsSessionDeadAlert(t *testing.T) {
	p := poolWithOne()
	p.MarkSessionDeadAlert("u1", "12153 refresh token is invalid")
	p.Disable("u1", "12153 session dead")

	st, _ := p.Status("u1")
	if !st.SessionDeadAlert {
		t.Fatalf("前置：应有告警")
	}

	p.ReviveDisabled("u1")

	st2, _ := p.Status("u1")
	if st2.SessionDeadAlert {
		t.Errorf("ReviveDisabled 应清告警，实为仍存在: %q", st2.SessionDeadMsg)
	}
	if st2.Disabled {
		t.Errorf("ReviveDisabled 应解除禁用")
	}
}

// TestReviveClearsSessionDeadAlert 全量解冻（Revive）必须清告警。
func TestReviveClearsSessionDeadAlert(t *testing.T) {
	p := poolWithOne()
	p.MarkSessionDeadAlert("u1", "12153 invalid")

	st, _ := p.Status("u1")
	if !st.SessionDeadAlert {
		t.Fatalf("前置：应有告警")
	}

	if ok := p.Revive("u1"); !ok {
		t.Fatalf("Revive 应成功")
	}

	st2, _ := p.Status("u1")
	if st2.SessionDeadAlert {
		t.Errorf("Revive 应清告警，实为仍存在: %q", st2.SessionDeadMsg)
	}
}

// TestNoteSuccessClearsSessionDeadAlert 对话成功必须清告警。
//
// 理由：对话成功 = accessToken 确实有效 = 账号可用。
// 若此时仍显示「refreshToken 已失效」，是自相矛盾的状态。
func TestNoteSuccessClearsSessionDeadAlert(t *testing.T) {
	p := poolWithOne()
	p.MarkSessionDeadAlert("u1", "12153 invalid")

	st, _ := p.Status("u1")
	if !st.SessionDeadAlert {
		t.Fatalf("前置：应有告警")
	}

	p.NoteSuccess("u1")

	st2, _ := p.Status("u1")
	if st2.SessionDeadAlert {
		t.Errorf("对话成功应清告警，实为仍存在: %q", st2.SessionDeadMsg)
	}
}

// TestMarkSessionDeadAlertDoesNotDisable 告警本身不禁用账号。
//
// 关键设计：告警是**预警**，禁用仍由 3 次计数决定。
// 混为一谈会把「瞬时 12153」误判成终态（P0-1 教训：13 个号被误杀）。
func TestMarkSessionDeadAlertDoesNotDisable(t *testing.T) {
	p := poolWithOne()
	p.MarkSessionDeadAlert("u1", "12153 invalid")

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Errorf("告警不该直接禁用账号（需 3 次计数）: %+v", st)
	}
	if !st.SessionDeadAlert {
		t.Errorf("应设置告警标记")
	}
}

// TestSessionDeadAlertPersistsAcrossReload 告警应持久化（重启后不丢）。
//
// 理由：告警是「需人工处理」的状态。若重启即消失，用户可能错过窗口
// （accessToken 到期后账号彻底不可用）。
func TestSessionDeadAlertPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	stateFile := dir + "/state.json"

	p := New(stateFile)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.MarkSessionDeadAlert("u1", "12153 invalid")
	p.Flush() // 落盘

	// 新池从同一文件加载
	p2 := New(stateFile)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p2.load() // 包内方法，测试可见

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatalf("账号应存在")
	}
	if !st.SessionDeadAlert {
		t.Errorf("告警应持久化到 state.json，重启后仍存在")
	}
	if st.SessionDeadMsg == "" {
		t.Errorf("告警详情应持久化")
	}
}

// TestMarkSessionDeadAlertTruncatesLongMsg 上游 HTML 错误页必须截断。
//
// 401 页面是整段 HTML（数 KB），原样存进 state.json 会让文件膨胀。
func TestMarkSessionDeadAlertTruncatesLongMsg(t *testing.T) {
	p := poolWithOne()
	long := "<html><head><title>401 Authorization Required</title></head><body>" +
		"<center><h1>401 Authorization Required</h1></center><hr><center>openresty</center>" +
		"<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a></em></p></body></html>"
	p.MarkSessionDeadAlert("u1", long)

	st, _ := p.Status("u1")
	if len(st.SessionDeadMsg) > 210 {
		t.Errorf("告警信息应截断到 ~200 字符，实为 %d", len(st.SessionDeadMsg))
	}
	if st.SessionDeadMsg == "" {
		t.Errorf("截断后不该为空")
	}
}

// TestSessionDeadAlertZeroValueIsFalse 零值状态不应误报告警。
func TestSessionDeadAlertZeroValueIsFalse(t *testing.T) {
	p := poolWithOne()
	st, _ := p.Status("u1")
	if st.SessionDeadAlert {
		t.Errorf("新账号不该有告警")
	}
	if st.SessionDeadMsg != "" {
		t.Errorf("新账号不该有告警详情")
	}
}

// TestClearSessionDeadAlertIdempotent 重复清除应安全（无 panic、无副作用）。
func TestClearSessionDeadAlertIdempotent(t *testing.T) {
	p := poolWithOne()
	p.MarkSessionDeadAlert("u1", "x")
	p.ClearSessionDeadAlert("u1")
	p.ClearSessionDeadAlert("u1") // 二次清除

	st, _ := p.Status("u1")
	if st.SessionDeadAlert {
		t.Errorf("应已清除")
	}
	// 不存在的账号也安全
	p.ClearSessionDeadAlert("nonexistent")
	_ = time.Now()
}
