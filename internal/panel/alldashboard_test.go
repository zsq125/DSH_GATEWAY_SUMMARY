// alldashboard_test.go 「All Account」汇总视图的用例。
//
// 用假平台（见 platforms_test.go）验证汇总层的职责：
// 多平台拼接、计数口径、合并动作的失败隔离与异步语义。
// 真平台的账号归一化在各自包的测试里验证。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"allplatforms/internal/platform"
	"allplatforms/internal/platform/workbuddy/pool"
)

// ── 汇总快照 ────────────────────────────────────────────────────────────

func TestAllDashboardMergesAllPlatforms(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	got := decodePlat(t, doPlat(p, "GET", "/panel/api/all/dashboard", ""))

	accounts, _ := got["accounts"].([]any)
	if len(accounts) != 4 {
		t.Fatalf("应合并 4 个账号（2 WB + 2 Trae），实为 %d", len(accounts))
	}

	// 每行都带 platform，前端据此分组
	sources := map[string]int{}
	for _, a := range accounts {
		m := a.(map[string]any)
		sources[m["platform"].(string)]++
	}
	if sources["workbuddy"] != 2 || sources["trae"] != 2 {
		t.Errorf("来源分布不对: %+v", sources)
	}

	c := got["counts"].(map[string]any)
	if c["total"].(float64) != 4 {
		t.Errorf("total 应为 4: %v", c["total"])
	}
	if c["workbuddy"].(float64) != 2 || c["trae"].(float64) != 2 {
		t.Errorf("分平台计数不对: %+v", c)
	}
	if c["checked_in"].(float64) != 1 {
		t.Errorf("checked_in 计数不对: %v", c["checked_in"])
	}
	if c["healthy"].(float64) != 2 || c["cooling"].(float64) != 2 {
		t.Errorf("状态计数不对: %+v", c)
	}
	// 积分合计
	if got["credits"].(float64) != 1400 {
		t.Errorf("积分合计不对: %v", got["credits"])
	}
}

// 各平台元信息必须一并透出（前端据此渲染分区与接入路径）。
func TestAllDashboardExposesPlatformInfos(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	got := decodePlat(t, doPlat(p, "GET", "/panel/api/all/dashboard", ""))
	plats, _ := got["platforms"].([]any)
	if len(plats) != 2 {
		t.Fatalf("应透出 2 个平台元信息: %v", got["platforms"])
	}
	m := plats[0].(map[string]any)
	for _, k := range []string{"kind", "display_name", "models_path", "accounts", "capabilities"} {
		if _, ok := m[k]; !ok {
			t.Errorf("平台元信息缺字段 %q: %+v", k, m)
		}
	}
}

// 单个平台缺席时不该报错，也不该编造它的账号。
func TestAllDashboardSinglePlatform(t *testing.T) {
	p := newRegistryPanel(t, wbFake())
	rec := doPlat(p, "GET", "/panel/api/all/dashboard", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d", rec.Code)
	}
	got := decodePlat(t, rec)
	accs := got["accounts"].([]any)
	if len(accs) != 2 {
		t.Errorf("应只有 WorkBuddy 的 2 个账号: %v", accs)
	}
	for _, a := range accs {
		if a.(map[string]any)["platform"] != "workbuddy" {
			t.Errorf("不该出现其他平台: %+v", a)
		}
	}
}

// 注册表未接线：返回空结构 + 200，不是错误。
func TestAllDashboardNoRegistry(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	rec := doPlat(p, "GET", "/panel/api/all/dashboard", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d", rec.Code)
	}
	got := decodePlat(t, rec)
	if got["accounts"] != nil {
		// nil 会被序列化成 null；关键是没崩、没 500
	}
}

// ── 合并动作 ────────────────────────────────────────────────────────────

func TestAllCheckinHitsAllPlatforms(t *testing.T) {
	wb := wbFake()
	tr := traeFake()
	p := newRegistryPanel(t, wb, tr)

	got := decodePlat(t, doPlat(p, "POST", "/panel/api/all/checkin", ""))
	plats, _ := got["platforms"].(map[string]any)
	if len(plats) != 2 {
		t.Fatalf("应有两个平台的结果: %v", got)
	}
	for _, k := range []string{"workbuddy", "trae"} {
		s := plats[k].(map[string]any)
		if s["ok"] != true {
			t.Errorf("%s 侧应被触发: %+v", k, s)
		}
	}
	s, _ := got["summary"].(string)
	if !strings.Contains(s, "WorkBuddy") || !strings.Contains(s, "Trae") {
		t.Errorf("summary 应含两边: %q", s)
	}

	// 两边都真的收到了动作
	if len(wb.triggerCalls) != 1 || len(tr.triggerCalls) != 1 {
		t.Errorf("动作派发不全: wb=%v tr=%v", wb.triggerCalls, tr.triggerCalls)
	}
}

// 一边失败不掩盖另一边。
func TestAllCheckinFailureIsolation(t *testing.T) {
	wb := wbFake()
	tr := traeFake()
	tr.triggerErr = "上游挂了"
	p := newRegistryPanel(t, wb, tr)

	got := decodePlat(t, doPlat(p, "POST", "/panel/api/all/checkin", ""))
	plats := got["platforms"].(map[string]any)
	wbs := plats["workbuddy"].(map[string]any)
	trs := plats["trae"].(map[string]any)
	if wbs["ok"] != true {
		t.Errorf("Trae 失败不该影响 WorkBuddy: %+v", wbs)
	}
	if trs["ok"] != false || trs["error"] != "上游挂了" {
		t.Errorf("Trae 失败应如实报告: %+v", trs)
	}
	s, _ := got["summary"].(string)
	if !strings.Contains(s, "WorkBuddy") || !strings.Contains(s, "失败") {
		t.Errorf("summary 应说明两边: %q", s)
	}
}

// 不支持该动作的平台：不是错误，summary 里也不提。
func TestAllCheckinUnsupportedNotFailure(t *testing.T) {
	tr := traeFake()
	tr.unsupported = map[platform.Action]bool{platform.ActionCheckin: true}
	p := newRegistryPanel(t, wbFake(), tr)

	got := decodePlat(t, doPlat(p, "POST", "/panel/api/all/checkin", ""))
	plats := got["platforms"].(map[string]any)
	trs := plats["trae"].(map[string]any)
	if trs["supported"] != false {
		t.Errorf("应标 supported=false: %+v", trs)
	}
	s, _ := got["summary"].(string)
	if strings.Contains(s, "Trae") {
		t.Errorf("能力缺口不该出现在 summary: %q", s)
	}
	if !strings.Contains(s, "WorkBuddy") {
		t.Errorf("WorkBuddy 部分应照常: %q", s)
	}
}

// 三个合并动作都要派发到正确的 Action。
func TestAllActionKinds(t *testing.T) {
	cases := []struct {
		path string
		want platform.Action
	}{
		{"/panel/api/all/checkin", platform.ActionCheckin},
		{"/panel/api/all/keepalive", platform.ActionKeepalive},
		{"/panel/api/all/balance", platform.ActionBalance},
	}
	for _, c := range cases {
		tr := traeFake()
		p := newRegistryPanel(t, tr)
		rec := doPlat(p, "POST", c.path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: 应 200，实为 %d", c.path, rec.Code)
		}
		if len(tr.triggerCalls) != 1 || tr.triggerCalls[0] != c.want {
			t.Errorf("%s: 动作派发不对: %v", c.path, tr.triggerCalls)
		}
	}
}

// 注册表未接线时合并动作 501。
func TestAllActionsNoRegistry(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	rec := doPlat(p, "POST", "/panel/api/all/checkin", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("未接线应 501，实为 %d", rec.Code)
	}
}

// ── 鉴权 ────────────────────────────────────────────────────────────────

func TestAllRoutesRequirePanelKey(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/all/dashboard"},
		{"POST", "/panel/api/all/checkin"},
		{"POST", "/panel/api/all/keepalive"},
		{"POST", "/panel/api/all/balance"},
	} {
		rec := doPlatNoAuth(p, c.method, c.path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未带 key 应 401，实为 %d", c.method, c.path, rec.Code)
		}
	}
}

// ── 凭证不泄漏 ──────────────────────────────────────────────────────────

// 响应里绝不能出现任何平台凭据。
func TestAllDashboardNoCredentialLeak(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	rec := doPlat(p, "GET", "/panel/api/all/dashboard", "")
	body := rec.Body.String()
	for _, secret := range []string{"test-key", "accessToken", "refreshToken", "secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("响应泄漏了 %q: %s", secret, body)
		}
	}
}

// ── 状态分类 ────────────────────────────────────────────────────────────

func TestWBStatusClassification(t *testing.T) {
	cases := []struct {
		name string
		s    pool.Status
		want string
	}{
		{"禁用优先于暂停与冷却", pool.Status{Disabled: true, Paused: true, Cooling: true}, "disabled"},
		{"暂停优先于冷却", pool.Status{Paused: true, Cooling: true}, "paused"},
		{"冷却", pool.Status{Cooling: true}, "cooling"},
		{"正常", pool.Status{}, "ok"},
	}
	for _, c := range cases {
		if got := wbStatusClass(c.s); got != c.want {
			t.Errorf("%s: wbStatusClass = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestWBStatusLabel(t *testing.T) {
	if got := wbStatusLabel(pool.Status{}); got != "可用" {
		t.Errorf("正常标签: %q", got)
	}
	if got := wbStatusLabel(pool.Status{Disabled: true}); got != "已禁用" {
		t.Errorf("禁用标签: %q", got)
	}
	if got := wbStatusLabel(pool.Status{Paused: true}); got != "已暂停" {
		t.Errorf("暂停标签: %q", got)
	}
}

// buildSummaryMulti 的三种呈现（成功 / 不支持 / 失败）不能混淆。
func TestBuildSummarySemantics(t *testing.T) {
	okSides := map[string]SideResult{
		"workbuddy": {Platform: "workbuddy", Enabled: true, Supported: true, OK: true, Ok: 4},
		"trae":      {Platform: "trae", Enabled: true, Supported: true, OK: true, Ok: 3},
	}
	if s := buildSummaryMulti("签到", okSides); !strings.Contains(s, "WorkBuddy 签到 4") || !strings.Contains(s, "Trae 签到 3") {
		t.Errorf("两边成功: %q", s)
	}

	unsupported := map[string]SideResult{
		"workbuddy": {Platform: "workbuddy", Enabled: true, Supported: true, OK: true, Ok: 4},
		"trae":      {Platform: "trae", Enabled: true, Supported: false},
	}
	if s := buildSummaryMulti("签到", unsupported); strings.Contains(s, "Trae") {
		t.Errorf("能力缺口不该提 Trae: %q", s)
	}

	bad := map[string]SideResult{
		"workbuddy": {Platform: "workbuddy", Enabled: true, Supported: true, Error: "无调度器"},
		"trae":      {Platform: "trae", Enabled: true, Supported: true, OK: true, Ok: 3},
	}
	if s := buildSummaryMulti("签到", bad); !strings.Contains(s, "WorkBuddy 签到失败") {
		t.Errorf("WorkBuddy 失败要明说: %q", s)
	}
	if s := buildSummaryMulti("签到", bad); !strings.Contains(s, "Trae 签到 3") {
		t.Errorf("Trae 成功要照常说: %q", s)
	}
}

// summary 的顺序必须稳定（map 迭代随机会让文案抖动）。
func TestBuildSummaryOrderIsStable(t *testing.T) {
	sides := map[string]SideResult{
		"workbuddy": {Platform: "workbuddy", Enabled: true, Supported: true, OK: true, Ok: 1},
		"trae":      {Platform: "trae", Enabled: true, Supported: true, OK: true, Ok: 2},
	}
	first := buildSummaryMulti("签到", sides)
	for i := 0; i < 20; i++ {
		if got := buildSummaryMulti("签到", sides); got != first {
			t.Fatalf("summary 顺序不稳定:\n  %q\n  %q", first, got)
		}
	}
	// 字母序：trae 在 workbuddy 之前
	if strings.Index(first, "Trae") > strings.Index(first, "WorkBuddy") {
		t.Errorf("顺序应为字母序: %q", first)
	}
}

// doPlatNoAuth 发一个不带鉴权头的请求。
func doPlatNoAuth(p *Panel, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// 确保 json 引用（decodePlat 用到，但本文件也有直接 Unmarshal 的场景）。
var _ = json.Unmarshal
