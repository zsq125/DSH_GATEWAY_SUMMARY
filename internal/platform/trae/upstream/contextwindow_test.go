package upstream

import "testing"

// 回归：DeepSeek-V4-Flash 必须公告真实窗口（128K），
// 不能沿用 1M 兜底 —— 否则 Trae 会按 1M 累积上下文，
// 聊几轮即被上游拒绝（CONTEXT_WINDOW_EXCEEDED）。
func TestContextWindowForIDFlash(t *testing.T) {
	got := ContextWindowForID("DeepSeek-V4-Flash")
	if got != 128000 {
		t.Errorf("DeepSeek-V4-Flash 应公告 128000，实为 %d", got)
	}
	if got >= DefaultContextWindow {
		t.Errorf("公告值(%d) 必须小于 1M 兜底(%d)，否则仍会溢出", got, DefaultContextWindow)
	}
}

// 未登记的模型继续用 1M 兜底（保持既有行为，不误伤大窗口模型）。
func TestContextWindowForIDFallback(t *testing.T) {
	if got := ContextWindowForID("GLM-4.6"); got != DefaultContextWindow {
		t.Errorf("未登记模型应回退 1M，实为 %d", got)
	}
	if got := ContextWindowForID(""); got != DefaultContextWindow {
		t.Errorf("空 id 应回退 1M，实为 %d", got)
	}
}
