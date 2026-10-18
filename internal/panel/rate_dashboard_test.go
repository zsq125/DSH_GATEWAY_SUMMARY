package panel

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// extractFunc 从 app.js 源里抽取一个顶层函数声明的完整文本（含函数体）。
// 以 "function <name>(" 为起点，到下一个顶层 "function "（或文件结尾）为终点。
// 不按花括号匹配，故函数体内的模板字符串反引号/花括号不会误截断。
func extractFunc(src, name string) string {
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		return ""
	}
	// 从 start+1 开始找下一个 "function "，避免命中自身。
	next := strings.Index(src[start+1:], "function ")
	if next < 0 {
		return src[start:]
	}
	return src[start : start+1+next]
}

// TestAppJSRateDashHelpers 覆盖「模型费率 Dashboard」（需求 36-38）的核心纯函数：
//   - rateCapRaw：能力分四布尔累加
//   - rateScoreModels：三维归一 + 综合分；无费率(Infinity)不得产生 NaN
//   - 分组：可视图(supports_images) / 不可视图
//   - 排序：综合分降序、最优项排首
// 无 node 的环境跳过（与既有 frontend_test 同判）。
func TestAppJSRateDashHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate dash helper test skipped")
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	// 只抽取被测试直接依赖的纯函数（避免连大片代码把无关模板字符串反引号拉进来）。
	// rateIsVision 依赖模块级 VISION_PROBES、rateScoreModels 依赖模块级 RATE_W——
	// 二者都是模块级 const，extractFunc 抽不到，必须在 vm 里手动注入 prelude。
	slice := extractFunc(string(src), "rateIsVision") +
		extractFunc(string(src), "mdRateValue") +
		extractFunc(string(src), "rateCapRaw") +
		extractFunc(string(src), "rateScoreModels")
	if !strings.Contains(slice, "function rateIsVision") ||
		!strings.Contains(slice, "function mdRateValue") ||
		!strings.Contains(slice, "function rateCapRaw") ||
		!strings.Contains(slice, "function rateScoreModels") {
		t.Fatal("rate dash helpers not found in app.js")
	}
	// prelude 在 vm 作用域注入运行时视觉实测结果（模拟 probe_vision.py 输出：
	// glm-5v-turbo 可图）。注意：RATE_W 已随 extractFunc 的 rateCapRaw 区间被一并
	// 抽入 slice（它声明在 rateCapRaw 与 rateScoreModels 之间），此处不可重复声明。
	const prelude = "var VISION_PROBES = Object.create(null); VISION_PROBES['cn:glm-5v-turbo'] = 'vision';\n"

	script := `const fs = require('fs');
const vm = require('vm');
` + "`" + prelude + slice + "`" + `
const ctx = { Number, String, Math, Object, Array, JSON, isNaN, parseFloat, RegExp };
vm.createContext(ctx);
vm.runInContext(` + "`" + prelude + slice + "`" + ` + '\nthis.rateIsVision=rateIsVision; this.mdRateValue=mdRateValue; this.rateCapRaw=rateCapRaw; this.rateScoreModels=rateScoreModels;', ctx);

// 用例 1：可视图/不可视图能力分（分组走 rateIsVision 运行时实测，不再看失真字段）。
// cn:glm-5v-turbo 被 VISION_PROBES 标 vision → 进可视图组；其余未判定 → 不可视图组。
const wb = [
  { id:'cn:glm-5v-turbo', credits:'x0.10', context_length:100000, supports_images:true,  supports_reasoning:true, supports_tool_call:true, is_default:true },
  { id:'cn:b', credits:'x0.20', context_length:200000, supports_images:true,  supports_reasoning:false,supports_tool_call:true, is_default:false },
  { id:'cn:c', credits:'x0.05', context_length:50000,  supports_images:false, supports_reasoning:false,supports_tool_call:false,is_default:false },
  { id:'cn:d', credits:'',       context_length:80000,  supports_images:false, supports_reasoning:false,supports_tool_call:false,is_default:false }, // 无费率
];
const vision = wb.filter(m => ctx.rateIsVision(m));
const noVision = wb.filter(m => !ctx.rateIsVision(m));

const sV = ctx.rateScoreModels(vision);
const sN = ctx.rateScoreModels(noVision);

const out = {
  capA: ctx.rateCapRaw(wb[0]),   // 4
  capC: ctx.rateCapRaw(wb[2]),   // 0
  visionTop: sV[0].id,           // 综合分最高
  visionBestScore: sV[0]._score, // 应接近 1
  noVisionHasNaN: sN.some(m => Number.isNaN(m._score)), // 必须 false
  noVisionDscore: sN.find(m => m.id==='cn:d')._score,   // 无费率→rateScore=0，但 len/cap 仍有效
  noVisionCscore: sN.find(m => m.id==='cn:c')._score,   // 无费率→rateScore=0
};
process.stdout.write(JSON.stringify(out));`

	f, err := os.CreateTemp(t.TempDir(), "rate-dash-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate dash helper node test failed: %v\n%s", err, out)
	}
	body := strings.TrimSpace(string(out))
	// 简单 JSON 键断言（避免引入 json 反序列化依赖过重）。
	must := func(key, want string) {
		re := regexp.MustCompile(regexp.QuoteMeta(key) + `":\s*([^,}\]]+)`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s not found in %s", key, body)
		}
		if strings.TrimSpace(m[1]) != want {
			t.Errorf("%s=%s want %s  (%s)", key, m[1], want, body)
		}
	}
	must("capA", "4")
	must("capC", "0")
	must("visionTop", `"cn:glm-5v-turbo"`) // glm-5v-turbo: 最便宜(0.10)+默认+四能力 → 综合最优
	must("noVisionHasNaN", "false")
	// 不可视图组现为 cn:b(200000)/cn:c(50000)/cn:d(80000)，组内 max length=200000。
	must("noVisionDscore", "0.12") // d: 无费率(rateScore0) + len 80000/200000=0.4*0.3 + cap0 → 0.12
	// cn:c 无费率、len 50000/200000=0.25、cap0 → 0.3*0.25=0.075
	if !strings.Contains(body, `"noVisionCscore":0.075`) {
		t.Errorf("noVisionCscore wrong in %s", body)
	}
}

// TestRateDashDomPresent 新 Dashboard 区块的所有 id 必须在 index.html 存在，
// 否则 app.js 引用会运行期 null 崩溃（与 TestTraeViewDOMPresent 同口径）。
func TestRateDashDomPresent(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, id := range []string{
		"rateDashBox", "rateDashNote", "rateDashScoring", "btnRateDashReload",
		"wbVisionBody", "wbNoVisionBody", "traeVisionBody", "traeNoVisionBody",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 id=%q（app.js 引用它，缺失会运行期崩溃）", id)
		}
	}
}

// TestRateDashNavDefaultHash 落地页 #all 顶层求值不得崩溃（费率 Dashboard 已拆为
// 独立 #ratedash 子页，不再由 loadAllDashboard 触发，但 #all 仍是默认页，须冒烟保底）。
func TestRateDashSmokeAllHash(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#all' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-all-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	cmd := exec.Command(node, hf.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	cmd.Env = append(os.Environ(), "SMOKE_HASH=#all")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("app.js 顶层求值 #all 崩溃: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SMOKE OK") {
		t.Fatalf("app.js smoke #all 未通过:\n%s", out)
	}
}

// TestAppJSRateDashWiring 费率 Dashboard 已拆为独立 #ratedash 子页：
//   - loadAllDashboard 必须「不再」调用 loadRateDash（否则又嵌回总览页）；
//   - go() 必须挂接 ratedash 分支调用 loadRateDash；
//   - btnRateDashReload 仍绑定 loadRateDash（独立页内手动刷新）。
// 静态引用检查，避免回归。
func TestAppJSRateDashWiring(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "function loadAllDashboard(") {
		t.Error("loadAllDashboard 缺失")
	}
	if strings.Contains(s, "renderAllAccounts();\n    loadRateDash();") {
		t.Error("loadAllDashboard 仍挂接 loadRateDash（费率 Dashboard 应拆为独立页）")
	}
	if !strings.Contains(s, "if (v === 'ratedash') loadRateDash();") {
		t.Error("go() 未挂接 ratedash 分支调用 loadRateDash")
	}
	if !strings.Contains(s, "if ($('btnRateDashReload')) $('btnRateDashReload').onclick = loadRateDash;") {
		t.Error("btnRateDashReload 未绑定 loadRateDash")
	}
	if !strings.Contains(s, "ratedash: '费率 Dashboard'") {
		t.Error("TITLES 缺少 ratedash 条目")
	}
}

// TestRateDashNoInlineScript 新区块不得引入内联 <script>（破坏 CSP）。
func TestRateDashNoInlineScript(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	rest := string(raw)
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		tag := rest[idx:]
		end := strings.Index(tag, ">")
		if end < 0 {
			break
		}
		if !strings.Contains(tag[:end+1], "src=") {
			t.Fatalf("index.html 含内联 <script>（破坏 CSP）: %s", tag[:end+1])
		}
		rest = tag[end:]
	}
}

// TestRateDashWeightControls 钉住 2026-10-13 的三条需求：
//  ① 权重改完有「保存」按钮（btnRateSave）；
//  ② 保存旁边有「排序」按钮（btnRateSort）；
//  ③ 最上面有「获取最新数据」按钮（btnRateFetch）；
//  ④ 「排序」初始必须 disabled —— 只在有数据时才能点。
//
// 背景：旧实现把权重放在配置表单之外，collectConfig() 读 cfgForm.elements 取不到，
// 权重永远存不进去；且 loadRatedashWeights 读 c.ratedash（实际在 c.config.ratedash），
// 永远读不回来。两条一起导致「配置了权重等于没配」。
func TestRateDashWeightControls(t *testing.T) {
	htmlRaw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlRaw)
	for _, id := range []string{"btnRateFetch", "btnRateSave", "btnRateSort"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 id=%q", id)
		}
	}
	if !strings.Contains(html, `id="btnRateSort" disabled`) {
		t.Error("「排序」必须初始 disabled（无数据时不可排序）")
	}

	jsRaw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsRaw)
	// 保存必须发到 config 且带 ratedash 三键。
	if !strings.Contains(js, "ratedash: { rate_w: RATE_W.rate") {
		t.Error("「保存」未把 ratedash 权重写回 config")
	}
	// 读配置必须走后端真实结构 c.config.ratedash（少一层就永远读不到）。
	if !strings.Contains(js, "c.config && c.config.ratedash") {
		t.Error("loadRatedashWeights 未按 c.config.ratedash 读取（少一层会永远回落默认）")
	}
	// 排序按钮必须由数据可用性驱动。
	if !strings.Contains(js, "function rateSortReady()") || !strings.Contains(js, "function rateUpdateSortBtn()") {
		t.Error("缺少 rateSortReady/rateUpdateSortBtn（排序的数据门控）")
	}
	// 三个按钮都必须接线。
	for _, bind := range []string{"$('btnRateFetch')", "$('btnRateSave')", "$('btnRateSort')"} {
		if !strings.Contains(js, "if ("+bind+")") {
			t.Errorf("%s 未接线", bind)
		}
	}
}

// TestGoUnknownViewFallsBack go() 收到未知视图时必须回落 all，
// 不能把所有 .view 隐藏——那正是「点一下所有控件消失」的表现。
func TestGoUnknownViewFallsBack(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if !strings.Contains(js, "if (!(v in TITLES)) v = 'all';") {
		t.Error("go() 缺少未知视图回落 all 的防御")
	}
	if !strings.Contains(js, "if (!matched)") {
		t.Error("go() 缺少『没有匹配到 section 时兜底显示 all』的防御")
	}
}

// TestRateDashTraeColumnsMatchHeader 钉住 Trae 表格列错位修复（2026-10-13 截图缺陷）。
//
// 现象：Trae「不可视图」表头是 7 列（费率/上下长度/输出上限/能力/档位/综合分），
// 但 rateRowTrae 只输出 3 格 → 费率被渲染到「上下长度」列下面。
// 根因：rateRenderGroup 用布尔 ranked 决定表头，Trae 也被传了 true。
// 修法：表头改为调用方显式传入 RATE_HEAD_WB / RATE_HEAD_TRAE。
func TestRateDashTraeColumnsMatchHeader(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if !strings.Contains(js, `RATE_HEAD_TRAE = '<th>模型</th><th class="num">积分消耗费率</th>'`) {
		t.Error("Trae 表头必须是『模型 + 积分消耗费率』两列")
	}
	if !strings.Contains(js, "const RATE_HEAD_WB") {
		t.Error("缺少 WB 表头常量 RATE_HEAD_WB")
	}
	// Trae 行不得再输出无用的 owned_by（全是 trae-solo）。
	// 只看 rateRowTrae 函数体：Trae「模型与档位」视图另有一处 owned_by，与本缺陷无关。
	if strings.Contains(extractFunc(js, "rateRowTrae"), "owned_by") {
		t.Error("rateRowTrae 仍在渲染 owned_by（应去掉的 trae-solo 列）")
	}
	// 未知/未测组必须真的被渲染（wf 快照里有 37 个未判定模型）。
	for _, id := range []string{"'wbUnknownBody'", "'traeUnknownBody'"} {
		if !strings.Contains(js, id) {
			t.Errorf("未渲染 %s（未知/未测组缺失）", id)
		}
	}
	htmlRaw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlRaw)
	for _, id := range []string{"wbUnknownBody", "traeUnknownBody"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 id=%q", id)
		}
	}
}
