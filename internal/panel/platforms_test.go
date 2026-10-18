// platforms_test.go 多平台层的测试夹具与用例。
//
// 测试策略：用**假平台**（fakePlatform）而不是真的 Trae/WorkBuddy 平台，
// 因为本文件要验证的是「面板的多平台层」这一层——注册表派发、能力探测、
// 路径解析、鉴权、错误语义。真平台的业务逻辑由它们自己的包测（那里有
// 上千行既有测试）。用假平台能把这两层彻底解耦：
// 面板层改坏了会在这里炸，平台层改坏了在那边炸，定位清晰。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"allplatforms/internal/platform"
)

// fakePlatform 可编程的假平台。
type fakePlatform struct {
	kind    platform.Kind
	display string
	prefix  string

	accounts []platform.Account
	status   map[string]any

	// 动作行为
	triggerErr string
	triggerOK  bool
	// 不支持的动作集合（验证「能力缺口不是错误」）
	unsupported map[platform.Action]bool

	// 可选能力
	accountAction func(uid, action string, body []byte) (int, any, error)
	login         bool
	importer      bool
	rawModels     bool

	// 记录调用
	triggerCalls []platform.Action
}

func (f *fakePlatform) Kind() platform.Kind    { return f.kind }
func (f *fakePlatform) DisplayName() string    { return f.display }
func (f *fakePlatform) ModelsPath() string     { return f.prefix + "/v1" }
func (f *fakePlatform) Register(mux *http.ServeMux) {
	mux.HandleFunc(f.prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}
func (f *fakePlatform) Accounts() []platform.Account { return f.accounts }
func (f *fakePlatform) Close() error                 { return nil }

func (f *fakePlatform) Trigger(a platform.Action, uid string) platform.ActionResult {
	f.triggerCalls = append(f.triggerCalls, a)
	if f.unsupported[a] {
		return platform.ActionResult{Platform: f.kind, Supported: false, Error: "unsupported"}
	}
	if f.triggerErr != "" {
		return platform.ActionResult{Platform: f.kind, Supported: true, Error: f.triggerErr}
	}
	return platform.ActionResult{
		Platform: f.kind, Supported: true, OK: f.triggerOK,
		Total: len(f.accounts), Ok: len(f.accounts), Async: true,
	}
}

// Status 实现 platform.StatusProvider（能力探测应发现它）。
func (f *fakePlatform) Status() map[string]any {
	if f.status == nil {
		return map[string]any{"total": len(f.accounts)}
	}
	return f.status
}

// AccountAction 实现 platform.AccountActioner（仅当 accountAction 非 nil 时）。
// 用「非 nil 才具备能力」的形态，让同一类型既能当"有能力"也能当"没能力"，
// 从而直接测出能力探测的分支。
func (f *fakePlatform) AccountAction(uid, action string, body []byte) (int, any, error) {
	if f.accountAction == nil {
		return http.StatusNotImplemented, nil, nil
	}
	return f.accountAction(uid, action, body)
}

// loginPlatform 只实现 LoginProvider 的假平台（单独类型，因为 Go 的
// 接口实现是静态的：要"有条件地实现接口"，只能用不同类型）。
type loginPlatform struct{ *fakePlatform }

func (l *loginPlatform) LoginStart(body []byte) (any, error) {
	return map[string]any{"login_url": "https://example.test/auth", "pending_id": "p1"}, nil
}
func (l *loginPlatform) LoginResult(q map[string][]string) (any, error) {
	id := ""
	if v, ok := q["pending_id"]; ok && len(v) > 0 {
		id = v[0]
	}
	return map[string]any{"pending_id": id, "state": "pending"}, nil
}
func (l *loginPlatform) LoginCancel(body []byte) (any, error) {
	return map[string]any{"canceled": true}, nil
}
func (l *loginPlatform) CallbackPath() string { return l.prefix + "/authorize" }

// importPlatform 只实现 Importer 的假平台。
type importPlatform struct{ *fakePlatform }

func (i *importPlatform) Import(payload string) (any, error) {
	return map[string]any{"imported": true, "payload": payload}, nil
}

// modelPlatform 只实现 RawModelLister 的假平台。
type modelPlatform struct{ *fakePlatform }

func (m *modelPlatform) RawModels() (any, error) {
	return map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "m1"}, map[string]any{"id": "m2"},
	}}, nil
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// newRegistryPanel 造一个带注册表的面板。
func newRegistryPanel(t *testing.T, plats ...platform.Platform) *Panel {
	t.Helper()
	reg := platform.NewRegistry()
	for _, p := range plats {
		if !reg.Register(p) {
			t.Fatalf("注册平台 %s 失败", p.Kind())
		}
	}
	return New(Config{Version: "test", APIKey: "test-key", Platforms: reg})
}

// wbFake 造一个假 WorkBuddy 平台（带两个账号）。
func wbFake() *fakePlatform {
	return &fakePlatform{
		kind: platform.KindWorkBuddy, display: "WorkBuddy", prefix: "/workbuddy",
		triggerOK: true,
		accounts: []platform.Account{
			{Platform: platform.KindWorkBuddy, UID: "wb-1", Status: "ok", Label: "可用", Credits: 900, Limit: 1000},
			{Platform: platform.KindWorkBuddy, UID: "wb-2", Status: "cooling", Label: "冷却中", Credits: 0},
		},
	}
}

// traeFake 造一个假 Trae 平台（带两个账号，支持登录/导入/模型）。
func traeFake() *fakePlatform {
	return &fakePlatform{
		kind: platform.KindTrae, display: "Trae", prefix: "/trae",
		triggerOK: true,
		accounts: []platform.Account{
			{Platform: platform.KindTrae, UID: "tr-1", Status: "ok", Label: "可用", Credits: 500, Limit: 1000, Used: 500, CheckedIn: true},
			{Platform: platform.KindTrae, UID: "tr-2", Status: "cooling", Label: "冷却中", Credits: 0},
		},
	}
}

func doPlat(p *Panel, method, path, body string) *httptest.ResponseRecorder {
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func decodePlat(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("非法 JSON: %v (%s)", err, rec.Body.String())
	}
	return m
}

// ── 平台列表 ────────────────────────────────────────────────────────────

func TestPlatformsList(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	got := decodePlat(t, doPlat(p, "GET", "/panel/api/platforms", ""))

	plats, _ := got["platforms"].([]any)
	if len(plats) != 2 {
		t.Fatalf("应列出 2 个平台，实为 %d", len(plats))
	}
	// 顺序必须稳定（注册顺序）：WorkBuddy 在前。
	first := plats[0].(map[string]any)
	if first["kind"] != "workbuddy" {
		t.Errorf("注册顺序未保持: %v", first["kind"])
	}
	if first["models_path"] != "/workbuddy/v1" {
		t.Errorf("接入路径不对: %v", first["models_path"])
	}
	if first["accounts"] != float64(2) {
		t.Errorf("账号数不对: %v", first["accounts"])
	}
}

// 能力集必须如实反映实现了哪些接口（前端据此渲染按钮）。
func TestPlatformsCapabilities(t *testing.T) {
	wb := wbFake()
	tr := traeFake()
	// Trae 假平台实现全部可选能力。
	var trFull platform.Platform = &fullTraePlatform{tr}
	p := newRegistryPanel(t, wb, trFull)

	got := decodePlat(t, doPlat(p, "GET", "/panel/api/platforms", ""))
	plats := got["platforms"].([]any)

	byKind := map[string]map[string]any{}
	for _, x := range plats {
		m := x.(map[string]any)
		byKind[m["kind"].(string)] = m
	}
	wbCaps := byKind["workbuddy"]["capabilities"].(map[string]any)
	// 假 WorkBuddy 只实现了 StatusProvider：其余能力必须是 false。
	if wbCaps["status"] != true {
		t.Errorf("WorkBuddy 应具备 status 能力: %+v", wbCaps)
	}
	if wbCaps["login"] != false || wbCaps["import"] != false {
		t.Errorf("WorkBuddy 不该声称具备 login/import: %+v", wbCaps)
	}
	trCaps := byKind["trae"]["capabilities"].(map[string]any)
	for _, k := range []string{"status", "login", "import", "raw_models"} {
		if trCaps[k] != true {
			t.Errorf("Trae 应具备 %s 能力: %+v", k, trCaps)
		}
	}
}

// fullTraePlatform 实现全部可选能力的假平台。
type fullTraePlatform struct{ *fakePlatform }

func (f *fullTraePlatform) LoginStart(body []byte) (any, error) {
	return map[string]any{"login_url": "https://example.test/auth", "pending_id": "p1"}, nil
}
func (f *fullTraePlatform) LoginResult(q map[string][]string) (any, error) {
	id := ""
	if v, ok := q["pending_id"]; ok && len(v) > 0 {
		id = v[0]
	}
	return map[string]any{"pending_id": id, "state": "pending"}, nil
}
func (f *fullTraePlatform) LoginCancel(body []byte) (any, error) {
	return map[string]any{"canceled": true}, nil
}
func (f *fullTraePlatform) CallbackPath() string { return f.prefix + "/authorize" }
func (f *fullTraePlatform) Import(payload string) (any, error) {
	return map[string]any{"imported": true, "payload": payload}, nil
}
func (f *fullTraePlatform) RawModels() (any, error) {
	return map[string]any{"object": "list", "data": []any{map[string]any{"id": "m1"}}}, nil
}
func (f *fullTraePlatform) AccountAction(uid, action string, body []byte) (int, any, error) {
	return http.StatusOK, map[string]any{"ok": true, "uid": uid, "action": action}, nil
}

// ── 平台解析与安全边界 ──────────────────────────────────────────────────

// 未注册的 kind 必须 404，而不是静默走到空实现。
func TestPlatformUnknownKind404(t *testing.T) {
	p := newRegistryPanel(t, wbFake())
	rec := doPlat(p, "GET", "/panel/api/platforms/nosuch/accounts", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知平台应 404，实为 %d: %s", rec.Code, rec.Body.String())
	}
}

// 非法 kind（路径穿越形状）必须 400，且在注册表查询之前就被拒。
func TestPlatformInvalidKindRejected(t *testing.T) {
	p := newRegistryPanel(t, wbFake())
	// %2e%2e 会被解码成 ".."，ValidKind 应拒绝（它含 '.'）。
	rec := doPlat(p, "GET", "/panel/api/platforms/..%2fetc/accounts", "")
	if rec.Code == http.StatusOK {
		t.Errorf("非法 kind 应被拒，实为 %d", rec.Code)
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("非法 kind 应 400/404，实为 %d", rec.Code)
	}
}

// 注册表未接线时全部接口 501（测试裸用/装配遗漏场景）。
func TestPlatformsRegistryNotConfigured(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	rec := doPlat(p, "GET", "/panel/api/platforms", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("未接线应 501，实为 %d", rec.Code)
	}
}

// ── 账号与概览 ──────────────────────────────────────────────────────────

func TestPlatformAccountsUnifiedShape(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	got := decodePlat(t, doPlat(p, "GET", "/panel/api/platforms/trae/accounts", ""))
	if got["platform"] != "trae" {
		t.Errorf("platform 字段不对: %v", got["platform"])
	}
	accs, _ := got["accounts"].([]any)
	if len(accs) != 2 {
		t.Fatalf("应有 2 个账号: %v", got)
	}
	a := accs[0].(map[string]any)
	// 统一形态：platform/uid/status/label/credits 都必须有。
	for _, k := range []string{"platform", "uid", "status", "label", "credits"} {
		if _, ok := a[k]; !ok {
			t.Errorf("统一形态缺字段 %q: %+v", k, a)
		}
	}
}

func TestPlatformOverviewIncludesStatusAndCaps(t *testing.T) {
	p := newRegistryPanel(t, wbFake())
	got := decodePlat(t, doPlat(p, "GET", "/panel/api/platforms/workbuddy/overview", ""))
	if got["display_name"] != "WorkBuddy" {
		t.Errorf("display_name 不对: %v", got["display_name"])
	}
	if _, ok := got["status"]; !ok {
		t.Errorf("应带 status（平台实现了 StatusProvider）: %v", got)
	}
	if _, ok := got["capabilities"]; !ok {
		t.Errorf("应带 capabilities: %v", got)
	}
}

// ── 动作 ────────────────────────────────────────────────────────────────

func TestPlatformActionAll(t *testing.T) {
	tr := traeFake()
	p := newRegistryPanel(t, tr)
	got := decodePlat(t, doPlat(p, "POST", "/panel/api/platforms/trae/actions/checkin", ""))
	if got["ok"] != true {
		t.Errorf("动作应成功: %v", got)
	}
	if len(tr.triggerCalls) != 1 || tr.triggerCalls[0] != platform.ActionCheckin {
		t.Errorf("动作未正确派发: %v", tr.triggerCalls)
	}
}

// 能力缺口必须 501（前端显示「暂不支持」），不是 500/报错。
func TestPlatformActionUnsupportedIs501(t *testing.T) {
	tr := traeFake()
	tr.unsupported = map[platform.Action]bool{platform.ActionCheckin: true}
	p := newRegistryPanel(t, tr)
	rec := doPlat(p, "POST", "/panel/api/platforms/trae/actions/checkin", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("不支持的动作应 501，实为 %d", rec.Code)
	}
	got := decodePlat(t, rec)
	if got["supported"] != false {
		t.Errorf("应标 supported=false: %v", got)
	}
}

// 非法动作名必须 400（白名单，防任意路径片段透传）。
func TestPlatformActionInvalidName(t *testing.T) {
	p := newRegistryPanel(t, traeFake())
	rec := doPlat(p, "POST", "/panel/api/platforms/trae/actions/delete_everything", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法动作应 400，实为 %d", rec.Code)
	}
}

// 单账号动作：uid 白名单必须生效（路径穿越挡住）。
func TestPlatformAccountActionUIDGuard(t *testing.T) {
	tr := traeFake()
	tr.accountAction = func(uid, action string, body []byte) (int, any, error) {
		return http.StatusOK, map[string]any{"ok": true, "uid": uid}, nil
	}
	p := newRegistryPanel(t, tr)

	// 非法 uid（含 ..）必须 400，且不能到达平台。
	rec := doPlat(p, "POST", "/panel/api/platforms/trae/accounts/..%2f..%2fetc/checkin", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 uid 应 400，实为 %d", rec.Code)
	}
	// 合法 uid 放行。
	rec = doPlat(p, "POST", "/panel/api/platforms/trae/accounts/abc-123/checkin", "")
	if rec.Code != http.StatusOK {
		t.Errorf("合法 uid 应放行，实为 %d: %s", rec.Code, rec.Body.String())
	}
}

// 未实现 AccountActioner 的平台必须 501。
func TestPlatformAccountActionUnsupported(t *testing.T) {
	p := newRegistryPanel(t, wbFake()) // 假 WorkBuddy 未实现 AccountActioner
	rec := doPlat(p, "POST", "/panel/api/platforms/workbuddy/accounts/u1/checkin", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("不支持单账号动作应 501，实为 %d", rec.Code)
	}
}

// ── 登录 / 导入 / 模型 ──────────────────────────────────────────────────

func TestPlatformLoginFlow(t *testing.T) {
	tr := &fullTraePlatform{traeFake()}
	p := newRegistryPanel(t, tr)

	got := decodePlat(t, doPlat(p, "POST", "/panel/api/platforms/trae/login/start", "{}"))
	if got["pending_id"] != "p1" {
		t.Errorf("login/start 应返回 pending_id: %v", got)
	}

	got = decodePlat(t, doPlat(p, "GET", "/panel/api/platforms/trae/login/result?pending_id=abc", ""))
	if got["pending_id"] != "abc" {
		t.Errorf("login/result 未带 query: %v", got)
	}
	if got["state"] != "pending" {
		t.Errorf("state 不对: %v", got)
	}

	got = decodePlat(t, doPlat(p, "POST", "/panel/api/platforms/trae/login/cancel", `{"pending_id":"p1"}`))
	if got["canceled"] != true {
		t.Errorf("login/cancel 不对: %v", got)
	}
}

// 不支持登录的平台必须 501。
func TestPlatformLoginUnsupported(t *testing.T) {
	p := newRegistryPanel(t, wbFake())
	rec := doPlat(p, "POST", "/panel/api/platforms/workbuddy/login/start", "{}")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("不支持登录应 501，实为 %d", rec.Code)
	}
}

func TestPlatformImport(t *testing.T) {
	tr := &fullTraePlatform{traeFake()}
	p := newRegistryPanel(t, tr)
	got := decodePlat(t, doPlat(p, "POST", "/panel/api/platforms/trae/import", `{"payload":"cred-json"}`))
	if got["imported"] != true {
		t.Errorf("导入失败: %v", got)
	}
	if got["payload"] != "cred-json" {
		t.Errorf("payload 未正确搬运: %v", got)
	}
}

func TestPlatformRawModels(t *testing.T) {
	tr := &fullTraePlatform{traeFake()}
	p := newRegistryPanel(t, tr)
	got := decodePlat(t, doPlat(p, "GET", "/panel/api/platforms/trae/models", ""))
	data, _ := got["data"].([]any)
	if len(data) != 1 {
		t.Errorf("模型列表不对: %v", got)
	}
}

func TestPlatformRawModelsUnsupported(t *testing.T) {
	p := newRegistryPanel(t, wbFake())
	rec := doPlat(p, "GET", "/panel/api/platforms/workbuddy/models", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("不支持模型目录应 501，实为 %d", rec.Code)
	}
}

// ── 鉴权与安全头 ────────────────────────────────────────────────────────

func TestPlatformRoutesRequirePanelKey(t *testing.T) {
	p := newRegistryPanel(t, wbFake(), traeFake())
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/platforms"},
		{"GET", "/panel/api/platforms/trae/accounts"},
		{"POST", "/panel/api/platforms/trae/actions/checkin"},
		{"POST", "/panel/api/platforms/trae/accounts/u1/checkin"},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未带 key 应 401，实为 %d", c.method, c.path, rec.Code)
		}
	}
}

func TestPlatformRoutesCarrySecurityHeaders(t *testing.T) {
	p := newRegistryPanel(t, traeFake())
	rec := doPlat(p, "GET", "/panel/api/platforms", "")
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("缺少 CSP")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options=%q", rec.Header().Get("X-Content-Type-Options"))
	}
}
