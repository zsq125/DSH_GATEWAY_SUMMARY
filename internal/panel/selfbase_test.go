package panel

import (
	"os"
	"strings"
	"testing"
)

// TestSelfBaseURL 视觉探测要「回打本网关」，基址必须由 listen 正确推导。
//
// 2026-10-13 生产事故：面板调探测时基址不对，脚本退回默认 127.0.0.1:7890，
// 而本部署监听 :7863 → 连不上 → 20 个 WorkBuddy 模型在费率页全部显示
// 「未知/未测」。这条用例钉住推导规则，防止同类事故复发。
func TestSelfBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{":7863", "http://127.0.0.1:7863"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863"},
		{"[::]:7863", "http://127.0.0.1:7863"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863"},
		{"192.168.1.7:7863", "http://192.168.1.7:7863"},
		{"", ""},
		{"7863", ""}, // 非法（缺主机分隔符）→ 空串
	}
	for _, c := range cases {
		if got := selfBaseURL(c.in); got != c.want {
			t.Errorf("selfBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRunVisionProbeUsesSelfBaseAndLiveKey 钉住进程内探测的三个要点。
//
// 从「传给脚本的参数」改为「进程内实现后必须仍然成立的三件事」：
//  1. 基址由 selfBaseURL(listen) 推导（那次生产事故的根因）；
//  2. 密钥优先取 livecfg 快照（运行期改过 key 后不取 live 会 401）；
//  3. 只探 WorkBuddy 域（Trae 逐个发图会把 3 个账号打到限流冷却）。
//
// 用源码断言而不是运行断言：探测会真打上游（1 分钟 + 消耗额度），
// 不适合在单测里跑；这里钉的是「实现里必须存在这些决策点」。
func TestRunVisionProbeUsesSelfBaseAndLiveKey(t *testing.T) {
	raw, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	// 只截取 RunVisionProbeOnce 函数体，避免匹配到别处的同名字符串。
	start := strings.Index(src, "func (p *Panel) RunVisionProbeOnce()")
	if start < 0 {
		t.Fatal("未找到 RunVisionProbeOnce")
	}
	end := strings.Index(src[start:], "\n// writeVisionSnapshot")
	if end < 0 {
		t.Fatal("未找到 RunVisionProbeOnce 的结尾标记")
	}
	body := src[start : start+end]

	for _, want := range []struct{ snippet, why string }{
		{"selfBaseURL(p.cfg.Listen)", "基址必须由 listen 推导（那次事故的根因）"},
		{"p.cfg.Live.Load()", "运行期改过 key 时必须取 live 快照，否则探测 401"},
		{`base + "/workbuddy/v1"`, "探测目标是 WorkBuddy 平台路径（正式契约，非根别名）"},
		{"vision.NewRunner", "必须走进程内探测器（而非外部脚本）"},
		{"p.writeVisionSnapshot", "结果必须落盘，面板才能读到"},
		{"context.WithTimeout", "探测必须有总时长上限（否则卡死上游会让 goroutine 永久驻留）"},
	} {
		if !strings.Contains(body, want.snippet) {
			t.Errorf("RunVisionProbeOnce 缺少 %q —— %s", want.snippet, want.why)
		}
	}

	// 反向断言：不该再出现任何外部脚本调用。
	for _, gone := range []string{"exec.Command", "probe_vision.py", "--wb-base", "--wb-only"} {
		if strings.Contains(body, gone) {
			t.Errorf("RunVisionProbeOnce 仍残留外部脚本痕迹 %q —— 已改为进程内实现", gone)
		}
	}
}

// TestVisionSnapshotAtomicWrite 结果文件必须原子写（先 tmp 再 rename）。
//
// 前端会周期性读这个文件；写一半被读到会解析失败，
// 面板上表现为「视觉能力突然全空」——比探测失败更难排查。
func TestVisionSnapshotAtomicWrite(t *testing.T) {
	raw, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "func (p *Panel) writeVisionSnapshot")
	if start < 0 {
		t.Fatal("未找到 writeVisionSnapshot")
	}
	end := strings.Index(src[start:], "\n// selfBaseURL")
	if end < 0 {
		end = len(src) - start
	}
	body := src[start : start+end]

	for _, want := range []struct{ snippet, why string }{
		{"os.WriteFile(tmp", "先写临时文件"},
		{"os.Rename(tmp, path)", "再原子替换（rename 是原子的）"},
		{"os.MkdirAll", "目标目录不存在时要先建（首次部署场景）"},
	} {
		if !strings.Contains(body, want.snippet) {
			t.Errorf("writeVisionSnapshot 缺少 %q —— %s", want.snippet, want.why)
		}
	}
}
