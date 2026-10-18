package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runNode 把一段 JS 跑在 node 里，标准输出作为结果。
func runNode(t *testing.T, script string, args ...string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; Trae 前端测试跳过")
	}
	f, err := os.CreateTemp(t.TempDir(), "trae-fe-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	all := append([]string{f.Name(), "app.js"}, args...)
	out, err := exec.Command(node, all...).CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行失败: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// extractRegion 按标记截出 app.js 的一段（含首尾）。
func extractRegion(src, from, to string) string {
	start := strings.Index(src, from)
	if start < 0 {
		panic("region start not found: " + from)
	}
	end := strings.Index(src, to)
	if end < 0 || end < start {
		panic("region end not found: " + to)
	}
	return src[start:end]
}

// ── 渲染：统计条 ────────────────────────────────────────────────────────

// Trae 概览的统计条口径：积分/权益包是前端按账号列表现算的。
//
// 合并后的变化：数据源从「Trae 后端 overview 的 credits/packs/healthy 字段」
// 改为**统一形态的账号列表**（platform.Account）现算——与 WorkBuddy 侧同一套
// 口径。故夹具字段名也跟着变（status 而非 cooling/enabled 布尔）。
func TestTraeRenderStats(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderTraeStats');
const end = src.indexOf('function traeRowTag');
if (start < 0 || end < 0) throw new Error('renderTraeStats region not found');
const nodes = {};
// 真实 DOM 里 textContent 赋值会强制转字符串；mock 必须同样 coerce，
// 否则数字会留在 mock 里，断言就得按 number 比较——那就测不出真实渲染结果了。
const mk = () => ({ textContent: '', innerHTML: '', value: '' });
const ctx = {
  Date, Number, String, Math, Object, Array,
  $: id => (nodes[id] || (nodes[id] = mk())),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderTraeStats = renderTraeStats;', ctx);

const d = {
  accounts: [
    { uid: 'a', status: 'ok', credits: 700, extra: { packs: 1 }, checked_in: true },
    { uid: 'b', status: 'ok', credits: 300, extra: { packs: 2 }, checked_in: false },
    { uid: 'c', status: 'cooling', credits: 0, checked_in: false },
  ],
};
ctx.renderTraeStats(d);
// 手动 String() 模拟 DOM 的 coerce（mock 不做 implicit conversion）
const s = id => String(nodes[id].textContent);
process.stdout.write(JSON.stringify([
  s('tTotal'), s('tHealthy'), s('tCooling'), s('tDisabled'),
  s('tCredits'), s('tPacks'), s('tChecked'), s('tAccNote'),
]));`
	got := runNode(t, script)
	want := `["3","2","1","0","1,000","3","1 / 3","3 个账号 · 积分合计 1,000"]`
	if got != want {
		t.Fatalf("renderTraeStats=%s want %s", got, want)
	}
}

// 空池也要有明确文案（而不是一排 0 让用户以为读数坏了）。
func TestTraeRenderStatsEmpty(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderTraeStats');
const end = src.indexOf('function traeRowTag');
const nodes = {};
const ctx = {
  Date, Number, String, Math, Object, Array,
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderTraeStats = renderTraeStats;', ctx);
ctx.renderTraeStats({ accounts: [] });
process.stdout.write(JSON.stringify([nodes.tCredits.textContent, nodes.tAccNote.textContent]));`
	got := runNode(t, script)
	want := `["0","Trae 账号池是空的"]`
	if got != want {
		t.Fatalf("renderTraeStats(empty)=%s want %s", got, want)
	}
}

// ── 渲染：状态标签 ──────────────────────────────────────────────────────

// 状态标签直接消费统一形态的 status/label。
//
// 合并前这里要按 disabled/cooling/enabled 三个布尔在前端自己推优先级，
// 与后端 traeStatusClass 是两套并行实现；合并后归一在后端做，前端只做映射。
// 这个用例锁住「前端不再自己推优先级」。
func TestTraeRowTag(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function traeRowTag');
const end = src.indexOf('function renderTraeAccounts');
if (start < 0 || end < 0) throw new Error('traeRowTag region not found');
const ctx = {
  Date, Number, String, Math, Object, Array,
  esc: s => String(s == null ? '' : s),
  dur: s => String(s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.traeRowTag = traeRowTag;', ctx);

const future = new Date(Date.now() + 3600000).toISOString();
const out = {
  disabled: ctx.traeRowTag({ status: 'disabled', label: '已禁用' }),
  cooling: ctx.traeRowTag({ status: 'cooling', label: '冷却中', until: future, reason: 'plan 权益不足' }),
  paused: ctx.traeRowTag({ status: 'paused', label: '已停用' }),
  ok: ctx.traeRowTag({ status: 'ok', label: '可用' }),
};
process.stdout.write(JSON.stringify([
  out.disabled[0], out.disabled[1],
  out.cooling[0], out.cooling[1],
  out.paused[0], out.paused[1],
  out.ok[0], out.ok[1],
]));`
	got := runNode(t, script)
	for _, want := range []string{
		`"off","<span class=\"tag bad\">已禁用</span>"`,
		`"cool","<span class=\"tag warn\">plan 权益不足`,
		`"off","<span class=\"tag\">已停用</span>"`,
		`"","<span class=\"tag ok\">可用</span>"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("traeRowTag 输出缺少 %s\n实际: %s", want, got)
		}
	}
}

// ── 渲染：账号表 ────────────────────────────────────────────────────────

// 账号表：健康色条类名、行内操作按钮的 data 属性、签到标签都要对。
func TestTraeRenderAccounts(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 从 traeRowTag 开始切：renderTraeAccounts 依赖它
const start = src.indexOf('function traeRowTag');
const end = src.indexOf('async function loadTraeOverview');
if (start < 0 || end < 0) throw new Error('region not found');
const nodes = {};
const ctx = {
  Date, Number, String, Math, Object, Array, Intl,
  esc: s => String(s == null ? '' : s),
  ago: () => '刚刚',
  dur: s => String(s),
  fmtLocalDateTime: ms => new Date(ms).toISOString(),
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderTraeAccounts = renderTraeAccounts;', ctx);

ctx.renderTraeAccounts([
  { uid: 'uid-aaaaaaaaaaaaa', nickname: '账号甲', status: 'ok', label: '可用',
    credits: 700, credits_total: 1000, credits_used: 300, checked_in: true,
    success_count: 10, err_total: 1, last_success: new Date().toISOString(),
    expires_at: new Date(Date.now() + 3600000).toISOString(),
    extra: { shared_with: 2 }, note: '主号' },
]);
const html = nodes.tAccBody.innerHTML;
process.stdout.write(JSON.stringify({
  hasName: html.includes('账号甲'),
  hasShortUid: html.includes('uid-aaaaaaa'),
  hasCredits: html.includes('700') && html.includes('1,000') && html.includes('300'),
  hasChecked: html.includes('已签到'),
  hasShared: html.includes('共同管理 2'),
  hasNote: html.includes('主号'),
  hasActions: ['checkin','balance','refresh','disable','remove']
    .every(a => html.includes('data-trae-act="' + a + '"')),
  hasUidAttr: html.includes('data-uid="uid-aaaaaaaaaaaaa"'),
  hasMarkCell: html.includes('class="mark"'),
}));`
	got := runNode(t, script)
	for _, want := range []string{
		`"hasName":true`, `"hasShortUid":true`, `"hasCredits":true`,
		`"hasChecked":true`, `"hasShared":true`, `"hasNote":true`,
		`"hasActions":true`, `"hasUidAttr":true`, `"hasMarkCell":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("账号表缺少 %s\n实际: %s", want, got)
		}
	}
}

// 空池给引导文案；UID 必须截断显示（长串会撑爆表格）。
func TestTraeRenderAccountsEdge(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function traeRowTag');
const end = src.indexOf('async function loadTraeOverview');
const nodes = {};
const ctx = {
  Date, Number, String, Math, Object, Array, Intl,
  esc: s => String(s == null ? '' : s),
  ago: () => '—', dur: s => String(s),
  fmtLocalDateTime: ms => new Date(ms).toISOString(),
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderTraeAccounts = renderTraeAccounts;', ctx);
ctx.renderTraeAccounts([]);
const empty = nodes.tAccBody.innerHTML;
ctx.renderTraeAccounts([{ uid: 'x'.repeat(64), status: 'ok', label: '可用', credits: 0 }]);
const long = nodes.tAccBody.innerHTML;
process.stdout.write(JSON.stringify({
  emptyHint: empty.includes('Trae 账号池是空的'),
  emptyAdd: empty.includes('添加 Trae 账号'),
  // 展示层截断到 14 字符 + 省略号；data-uid 仍保留完整 UID（操作按钮要用它）
  truncated: long.includes('xxxxxxxxxxxxxx…'),
  fullUidKeptInAttr: long.includes('data-uid="' + 'x'.repeat(64) + '"'),
  hasUnchecked: long.includes('未签到'),
}));`
	got := runNode(t, script)
	for _, want := range []string{
		`"emptyHint":true`, `"emptyAdd":true`, `"truncated":true`,
		`"fullUidKeptInAttr":true`, `"hasUnchecked":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("边界渲染缺少 %s\n实际: %s", want, got)
		}
	}
}

// ── 渲染：模型表 ────────────────────────────────────────────────────────

// 模型筛选：空格分词？不——需求只要求关键词匹配，大小写不敏感。
func TestTraeRenderModelsFilter(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderTraeModels');
const end = src.indexOf('async function loadTraeModels');
if (start < 0 || end < 0) throw new Error('renderTraeModels region not found');
const nodes = { tMdQ: { value: '' } };
const ctx = {
  Date, Number, String, Math, Object, Array,
  esc: s => String(s == null ? '' : s),
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderTraeModels = renderTraeModels;', ctx);

const list = [
  { id: 'glm-5.2', name: 'GLM 5.2', owned_by: 'trae-solo' },
  { id: 'kimi-k3', name: 'Kimi K3', owned_by: 'trae-solo' },
  { id: 'DeepSeek-V4-Pro', name: 'DeepSeek V4', owned_by: 'trae-solo' },
];
ctx.renderTraeModels(list);
const allNote = nodes.tMdNote.textContent;
const allRows = (nodes.tMdBody.innerHTML.match(/<tr>/g) || []).length;
// 按 id 搜
nodes.tMdQ.value = 'kimi';
ctx.renderTraeModels(list);
const byId = nodes.tMdBody.innerHTML;
// 按 name 搜（大小写不敏感）
nodes.tMdQ.value = 'deepseek';
ctx.renderTraeModels(list);
const byName = nodes.tMdBody.innerHTML;
// 无匹配
nodes.tMdQ.value = 'zzz';
ctx.renderTraeModels(list);
process.stdout.write(JSON.stringify({
  allNote: allNote,
  allRows: allRows,
  byIdHit: byId.includes('kimi-k3') && !byId.includes('glm-5.2'),
  byNameHit: byName.includes('DeepSeek-V4-Pro'),
  noHitNote: nodes.tMdNote.textContent,
}));`
	got := runNode(t, script)
	for _, want := range []string{
		`"allNote":"3 个"`, `"allRows":3`,
		`"byIdHit":true`, `"byNameHit":true`, `"noHitNote":"0 / 3 个（已筛选）"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("模型筛选缺少 %s\n实际: %s", want, got)
		}
	}
}

func TestTraeRenderModelsEmpty(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderTraeModels');
const end = src.indexOf('async function loadTraeModels');
const nodes = { tMdQ: { value: '' } };
const ctx = {
  Date, Number, String, Math, Object, Array,
  esc: s => String(s == null ? '' : s),
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderTraeModels = renderTraeModels;', ctx);
ctx.renderTraeModels([]);
process.stdout.write(JSON.stringify({
  hint: nodes.tMdBody.innerHTML.includes('没有可用模型'),
}));`
	got := runNode(t, script)
	if !strings.Contains(got, `"hint":true`) {
		t.Errorf("空模型列表应给提示: %s", got)
	}
}

// ── 路由与接线 ──────────────────────────────────────────────────────────

// Trae 视图必须在 TITLES 里（否则 go('trae') 的标题是 undefined，深链 #trae 直接落空）。
func TestTraeViewInRouter(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "trae: 'Trae 账号'") {
		t.Error("TITLES 里缺 Trae 视图标题")
	}
	if !strings.Contains(src, "if (v === 'trae') loadTraeOverview();") {
		t.Error("go('trae') 未触发 loadTraeOverview")
	}
}

// 统一签到：两个入口按钮都必须接到同一个函数。
//
// 合并后的变化：Trae 侧不再是「反代到另一个进程」，而是打本进程的
// /panel/api/platforms/trae/actions/checkin（平台动作统一入口）。
func TestUnifiedCheckinWiring(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		"$('btnUnifiedCheckin').onclick = unifiedCheckinAll;",
		"$('btnTraeCheckin').onclick = unifiedCheckinAll;",
		"'btnUnifiedCheckin', 'btnTraeCheckin'",
		"api('checkin_all', { method: 'POST' })",
		"api('platforms/trae/actions/checkin', { method: 'POST' })",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("统一签到接线缺少 %q", want)
		}
	}
}

// 前端**绝不能**出现任何后端地址/端口硬编码。
//
// 合并前这条用例查的是「不许直连 :7862」；合并后语义更强：
// 全部平台都在同一端口，前端只认 /panel/api/* 这一条路径。
// 顺带锁住「旧的反代前缀 trae/* 已被 platforms/* 取代」。
//
// 注意：注释里提到历史端口（解释"为什么不再需要它"）是允许的，
// 故这里先剥掉注释再检查，避免把说明文字误判成硬编码。
func TestTraeRequestsGoThroughPanelAPI(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	code := stripJSComments(src)

	if strings.Contains(code, ":7862") {
		t.Error("前端代码里不该出现 7862（合并后只有一个端口）")
	}
	if strings.Contains(code, "http://127.0.0.1:786") {
		t.Error("前端代码里不该出现后端地址硬编码")
	}
	// 旧的 /panel/api/trae/* 反代入口必须已被平台统一入口取代。
	if strings.Contains(code, "api('trae/") {
		t.Error("残留旧反代调用 api('trae/...')——应改用 api('platforms/trae/...')")
	}
	// 关键接口都要经平台统一入口。
	for _, p := range []string{
		"api('platforms/trae/overview')", "api('platforms/trae/accounts')",
		"api('platforms/trae/models')",
		"api('platforms/trae/actions/balance'", "api('platforms/trae/actions/keepalive'",
		"api('platforms/trae/login/start'", "api('platforms/trae/import'",
	} {
		if !strings.Contains(src, p) {
			t.Errorf("缺少经平台入口的调用 %q", p)
		}
	}
}

// stripJSComments 剥掉 // 行注释与 /* */ 块注释。
//
// 为什么需要它：源码里的注释会解释历史设计（"合并前这里连 :7862"），
// 那是**有价值的信息**，不该为了迁就一个字符串检查而删掉。
// 正确的做法是让检查只看代码。
func stripJSComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		// 块注释
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				break
			}
			i += 2 + j + 2
			continue
		}
		// 行注释（不处理字符串字面量里的 // —— 本用例只关心 API 调用形态，
		// 那种情形在 app.js 里不存在；若将来出现，检查会偏保守而非漏报）
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '/' {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// 视图 DOM 与 app.js 的引用要一致（少一个 id 就是运行期 null 崩溃）。
func TestTraeViewDOMPresent(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, id := range []string{
		"view-trae", "traeSetup", "traeMain", "traeSetupTitle", "traeSetupDesc",
		"tTotal", "tHealthy", "tCooling", "tDisabled", "tCredits", "tPacks", "tChecked",
		"tAccNote", "tAccBody", "tMdQ", "tMdNote", "tMdBody", "traeBaseUrl",
		"btnTraeBalance", "btnTraeKeepalive", "btnTraeCheckin", "btnTraeAdd", "btnTraeModels",
		"traeNavBadge", "btnUnifiedCheckin",
		"traeLoginVeil", "traeAddTabs", "traeTabLogin", "traeTabImport",
		"traeUrl", "traePoll", "traeLoginErr",
		"traeImportText", "traeImportOk", "traeImportErr",
		"btnTraeOpenUrl", "btnTraeCopyUrl", "btnTraeImportRun", "btnTraeLoginClose",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 id=%q（app.js 引用它，缺失会运行期崩溃）", id)
		}
	}
}

// 导航必须按后端归属分区（用户要能一眼看出点下去打到哪个服务）。
func TestTraeNavGrouped(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		`<div class="ncaption">WorkBuddy</div>`,
		`data-view="trae"`,
		"Trae 账号",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("导航缺少 %q", want)
		}
	}
	if !strings.Contains(html, "id=\"traeNavBadge\"") {
		t.Error("缺少 Trae 连接状态徽标")
	}
}

// Config 表单与 CFG_MAP 要对齐（Trae 平台段）。
//
// 合并后的变化：base_url / api_key / timeout_seconds 三项随反代一起删除，
// 改为平台自己的部署参数（凭证目录 / 状态文件 / 回调端口）。
func TestTraeConfigFieldsMapped(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		"trae_enabled: ['trae', 'enabled']",
		"trae_auth_dir: ['trae', 'auth_dir']",
		"trae_state_file: ['trae', 'state_file']",
		"trae_oauth_callback_port: ['trae', 'oauth', 'callback_port']",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("CFG_MAP 缺少 %q", want)
		}
	}
	// 已删除的接线项不该残留（留着就会有人去填一个不存在的配置键）。
	for _, gone := range []string{
		"trae_base_url:", "trae_api_key:", "trae_timeout_seconds:",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("CFG_MAP 残留已删除的接线项 %q", gone)
		}
	}
	// 留空必须照发（清空 = 回落内置默认路径）
	if !strings.Contains(src, "'trae_auth_dir', 'trae_state_file'") {
		t.Error("trae 路径两项应加入 CLEARABLE_CFG")
	}
}

// 默认落地页必须是 All Account Dashboard（需求：「默认页显示 all in 的 dashboard，
// 一登陆就跳到」）。这是「一登录就跳到整个 panel」的落点。
//
// 三处必须一致，缺一处就会闪一下再跳走：
//  1. 路由兜底 hash（app.js setTimeout 里的默认值）
//  2. 导航高亮 class="on"
//  3. 顶栏标题 + 视图 hidden 状态（否则先闪一下账号池再切走）
func TestDefaultViewIsAllDashboard(t *testing.T) {
	rawJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(rawJS)
	if !strings.Contains(js, "location.hash || '#all'") {
		t.Error("路由兜底 hash 应为 #all")
	}
	if !strings.Contains(js, "go(hash in TITLES ? hash : 'all')") {
		t.Error("非法 hash 应回落到 all")
	}

	rawHTML, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(rawHTML)
	// 导航高亮在 all 上
	if !strings.Contains(html, `data-view="all" class="on"`) {
		t.Error("导航高亮 class=\"on\" 应在 all 上")
	}
	if strings.Contains(html, `data-view="accounts" class="on"`) {
		t.Error("accounts 不该再带 class=\"on\"（会和 all 抢高亮）")
	}
	// 顶栏标题
	if !strings.Contains(html, `<h2 id="ttl">All Account</h2>`) {
		t.Error("顶栏初始标题应为 All Account")
	}
	// view-all 不该是 hidden，view-accounts 该是
	iAll := strings.Index(html, `<section class="view" id="view-all"`)
	if iAll < 0 || !strings.HasPrefix(html[iAll:], `<section class="view" id="view-all">`) {
		// view-all 必须是不带 hidden 的（它是默认可见的那块）
		seg := html[iAll : iAll+90]
		if strings.Contains(seg, "hidden") {
			t.Errorf("view-all 不该带 hidden: %s", seg)
		}
	}
	if !strings.Contains(html, `<section class="view" id="view-accounts" hidden>`) {
		t.Error("view-accounts 初始应为 hidden（否则会先闪账号池再切走）")
	}
}

// ══ All Account 统一 Dashboard ═══════════════════════════════════════════

// All Account 必须是导航里的**第一个分组**（需求：加一级菜单叫 All Account）。
func TestAllAccountNavFirst(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if !strings.Contains(html, `<div class="ncaption">All Account`) {
		t.Error("缺少 All Account 分组标题")
	}
	if !strings.Contains(html, `data-view="all"`) {
		t.Error("缺少 All Account 导航项")
	}
	if !strings.Contains(html, "总览 Dashboard") {
		t.Error("All Account 下的视图名不对")
	}
	iAll := strings.Index(html, `<div class="ncaption">All Account`)
	iWb := strings.Index(html, `<div class="ncaption">WorkBuddy</div>`)
	if iAll < 0 || iWb < 0 || iAll > iWb {
		t.Errorf("All Account 分组应排在 WorkBuddy 之前: all=%d wb=%d", iAll, iWb)
	}
}

// 汇总视图的 DOM 与 app.js 引用必须一致。
func TestAllViewDOMPresent(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, id := range []string{
		"view-all",
		"aTotal", "aHealthy", "aCooling", "aDisabled", "aCredits", "aWb", "aTrae",
		"aActNote", "aSideResult", "aSideWB", "aSideTrae", "aSideHint",
		// 两块独立列表：各一个表体、一个搜索框、一个计数
		"aWbBody", "aQ", "aWbNote", "aWbHint",
		"aTraeBody", "aTq", "aTraeNote", "aTraeHint",
		"btnAllCheckin", "btnAllKeepalive", "btnAllBalance", "btnAllTasks", "btnAllReload",
		"allNavBadge",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 id=%q", id)
		}
	}
	// 旧的单表结构必须已移除（留着一份就等于有两处渲染入口）
	for _, gone := range []string{`id="aSourceFilter"`, `id="aAccBody"`, `id="aAccNote"`} {
		if strings.Contains(html, gone) {
			t.Errorf("旧单表结构残留: %s", gone)
		}
	}
}

// 路由与接口接线。
func TestAllViewWiring(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		"all: 'All Account'",
		"if (v === 'all') loadAllDashboard();",
		"api('all/dashboard')",
		"api('all/' + kind, { method: 'POST' })",
		"$('btnAllCheckin').onclick",
		"$('btnAllKeepalive').onclick",
		"$('btnAllBalance').onclick",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("接线缺少 %q", want)
		}
	}
}

// 三个动作入口都打到同一个 allAction（不能各写一套调用）。
func TestAllActionUnified(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, line := range []string{
		"$('btnAllCheckin').onclick = () => allAction('checkin', 'btnAllCheckin', '签到');",
		"$('btnAllKeepalive').onclick = () => allAction('keepalive', 'btnAllKeepalive', '保活');",
		"$('btnAllBalance').onclick = () => allAction('balance', 'btnAllBalance', '刷新余额');",
	} {
		if !strings.Contains(src, line) {
			t.Errorf("缺少统一接线: %q", line)
		}
	}
}

// 状态标签与 Trae 视图同色标。
func TestAllStatusTagConsistency(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function allStatusTag');
const end = src.indexOf('function renderAllAccounts');
if (start < 0 || end < 0) throw new Error('allStatusTag region not found');
const ctx = { Date, Number, String, Math, Object, Array, esc: s => String(s == null ? '' : s) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.allStatusTag = allStatusTag;', ctx);
process.stdout.write(JSON.stringify([
  ctx.allStatusTag({ status: 'ok', label: '可用' }),
  ctx.allStatusTag({ status: 'cooling', label: '冷却中', reason: '429' }),
  ctx.allStatusTag({ status: 'disabled', label: '已禁用' }),
  ctx.allStatusTag({ status: 'paused', label: '已暂停' }),
]));`
	got := runNode(t, script)
	// 注意 JSON.stringify 会把 HTML 里的双引号转义成 \"，
	// 所以这里匹配的是 class=\\"tag ok\\" 这种形态（Go 原始字符串里写作 \ 反斜杠）。
	for _, want := range []string{
		`class=\"tag ok\"`, `class=\"tag warn\"`, `class=\"tag bad\"`, `已暂停`, `429`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("状态标签缺少 %s\n实际: %s", want, got)
		}
	}
}

// 汇总统计条：分项求和。
func TestAllRenderStats(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderAllStats');
const end = src.indexOf('/* 状态标签');
if (start < 0 || end < 0) throw new Error('renderAllStats region not found');
const nodes = {};
const ctx = {
  Date, Number, String, Math, Object, Array,
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderAllStats = renderAllStats;', ctx);
ctx.renderAllStats({ counts: { total: 5, healthy: 3, cooling: 1, disabled: 1, workbuddy: 4, trae: 1 }, credits: 12345 });
const s = id => String(nodes[id].textContent);
process.stdout.write(JSON.stringify([s('aTotal'), s('aHealthy'), s('aCooling'), s('aDisabled'), s('aCredits'), s('aWb'), s('aTrae')]));`
	got := runNode(t, script)
	want := `["5","3","1","1","12,345","4","1"]`
	if got != want {
		t.Fatalf("renderAllStats=%s want %s", got, want)
	}
}

// 能力缺口 / 真实失败必须区分开。
//
// 合并后的变化：响应形状从固定两键 {workbuddy, trae} 改为
// {platforms: {kind: SideResult}}，故判定改为遍历 map。
func TestSideHasFailureSemantics(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function sideHasFailure');
const end = src.indexOf('/* renderAllSideResults');
if (start < 0 || end < 0) throw new Error('sideHasFailure region not found');
const ctx = { Date, Number, String, Math, Object, Array, Object };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.sideHasFailure = sideHasFailure;', ctx);
process.stdout.write(JSON.stringify({
  bothOk: ctx.sideHasFailure({ platforms: { workbuddy: { ok: true }, trae: { ok: true } } }),
  unsupported: ctx.sideHasFailure({ platforms: { workbuddy: { ok: true }, trae: { supported: false, ok: false } } }),
  wbFail: ctx.sideHasFailure({ platforms: { workbuddy: { ok: false }, trae: { ok: true } } }),
  traeFail: ctx.sideHasFailure({ platforms: { workbuddy: { ok: true }, trae: { ok: false } } }),
  empty: ctx.sideHasFailure({ platforms: {} }),
}));`
	got := runNode(t, script)
	for _, want := range []string{
		`"bothOk":false`, `"unsupported":false`, `"wbFail":true`, `"traeFail":true`,
		`"empty":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("失败判定缺少 %s\n实际: %s", want, got)
		}
	}
}

// 两块列表：上 WorkBuddy、下 Trae，各用各的列与搜索框。
//
// 切片从 allStatusTag 起（renderWbTable 依赖它），到「合并动作」注释止。
func TestAllRenderTwoTables(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function allStatusTag');
const end = src.indexOf('/* ── 合并动作');
if (start < 0 || end < 0) throw new Error('render region not found');
const nodes = {
  aQ: { value: '' }, aTq: { value: '' },
  aWbBody: { innerHTML: '' }, aTraeBody: { innerHTML: '' },
  aWbNote: { textContent: '' }, aTraeNote: { textContent: '' },
};
const ctx = {
  Date, Number, String, Math, Object, Array, Intl,
  esc: s => String(s == null ? '' : s),
  ago: () => '刚刚',
  fmtLocalDateTime: ms => new Date(ms).toISOString().slice(0, 16).replace('T', ' '),
  allData: null,
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.renderAllAccounts = renderAllAccounts; this.setAll = d => { allData = d; };', ctx);

// 注意这里用 credits_total（前端消费的字段名）而不是 Trae 后端的 credits_limit：
// 平台抽象层已把后者归一到 Account.Limit，序列化出来就是 credits_total。
// 用错名字会测出一个"看起来是 bug、其实是测试数据不对"的假阳性。
//
// 来源字段也从 source 改名为 platform（抽象层统一定义）。
ctx.setAll({ platforms: [{ kind: 'workbuddy' }, { kind: 'trae' }], accounts: [
  { platform: 'workbuddy', uid: 'wb-1', nickname: 'WB 甲', status: 'ok', label: '可用', credits: 900, credits_total: 1000, realm: 'cn', checked_in: true },
  { platform: 'workbuddy', uid: 'wb-2', nickname: 'WB 乙', status: 'ok', label: '可用', credits: 500, credits_total: 800, realm: 'global' },
  { platform: 'trae', uid: 'tr-1', nickname: 'Trae 甲', status: 'ok', label: '可用', credits: 348, credits_total: 1000, credits_used: 652, checked_in: true,
    expires_at: new Date(Date.now() + 3600000).toISOString(), note: '主号' },
]});
ctx.renderAllAccounts();
const wbAll = nodes.aWbBody.innerHTML;
const trAll = nodes.aTraeBody.innerHTML;

// 各自只出现自己那侧的账号（不串行）
const out = {
  wbHasBoth: wbAll.includes('WB 甲') && wbAll.includes('WB 乙'),
  wbNoTrae: !wbAll.includes('Trae 甲'),
  traeHasOwn: trAll.includes('Trae 甲'),
  traeNoWb: !trAll.includes('WB 甲'),
  wbRealm: wbAll.includes('>cn<') && wbAll.includes('>global<'),
  wbNote: nodes.aWbNote.textContent,
  traeNote: nodes.aTraeNote.textContent,
  traeExpiry: trAll.includes('主号'),
  traeUsedCol: trAll.includes('348') && trAll.includes('1,000') && trAll.includes('652'),
};
// WorkBuddy 侧搜索只筛自己那块
nodes.aQ.value = '甲';
ctx.renderAllAccounts();
out.wbFiltered = nodes.aWbBody.innerHTML.includes('WB 甲') && !nodes.aWbBody.innerHTML.includes('WB 乙');
out.traeUnaffected = nodes.aTraeBody.innerHTML.includes('Trae 甲');
nodes.aQ.value = '';
// Trae 侧搜索同理
nodes.aTq.value = '不存在';
ctx.renderAllAccounts();
out.traeFiltered = nodes.aTraeBody.innerHTML.includes('没有匹配');
out.wbStillAll = nodes.aWbBody.innerHTML.includes('WB 乙');

process.stdout.write(JSON.stringify(out));`
	got := runNode(t, script)
	for _, want := range []string{
		`"wbHasBoth":true`, `"wbNoTrae":true`, `"traeHasOwn":true`, `"traeNoWb":true`,
		`"wbRealm":true`, `"wbNote":"2 个账号"`, `"traeNote":"1 个账号"`,
		`"traeUsedCol":true`, `"traeExpiry":true`,
		`"wbFiltered":true`, `"traeUnaffected":true`,
		`"traeFiltered":true`, `"wbStillAll":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("两块列表缺少 %s\n实际: %s", want, got)
		}
	}
}

// Trae 平台未注册时给引导文案，而不是一张空表（用户会以为账号丢了）。
func TestAllTraeTableOfflineHint(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function allStatusTag');
const end = src.indexOf('/* ── 合并动作');
const nodes = {
  aQ: { value: '' }, aTq: { value: '' },
  aWbBody: { innerHTML: '' }, aTraeBody: { innerHTML: '' },
  aWbNote: { textContent: '' }, aTraeNote: { textContent: '' },
};
const ctx = {
  Date, Number, String, Math, Object, Array,
  esc: s => String(s == null ? '' : s), ago: () => '—',
  fmtLocalDateTime: () => '—',
  allData: { platforms: [], accounts: [] },
  $: id => (nodes[id] || (nodes[id] = { textContent: '', innerHTML: '', value: '' })),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderAllAccounts = renderAllAccounts;', ctx);
ctx.renderAllAccounts();
process.stdout.write(JSON.stringify({ hint: nodes.aTraeBody.innerHTML.includes('未启用') }));`
	got := runNode(t, script)
	if !strings.Contains(got, `"hint":true`) {
		t.Errorf("Trae 未启用应给引导文案: %s", got)
	}
}
