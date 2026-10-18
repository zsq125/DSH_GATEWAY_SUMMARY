// visionprobe.go 视觉能力（图输入）运行时探测 —— 移植自 scripts/probe_vision.py。
//
// 为什么移植进主程序（而不是继续用外部 Python 脚本）：
//   1. **空窗期**：脚本只在调度器 03:00 到点或手动点按钮时跑。新部署/换端口后
//      到 03:00 之前，面板的「视觉能力」全是「未知/未测」——用户看到的就是
//      「能力获取不到了」。移植后可在启动时预热，彻底消除空窗。
//   2. **外部依赖**：原方案要求部署包必须带 scripts/ 目录 + 系统装 Python + PIL。
//      少任何一项都静默失效（只记日志不报错），是部署陷阱。
//   3. **跨语言约定**：探测逻辑在 Python、结果文件由 Go 读，格式约定分散两处。
//   4. 主程序已有全部所需能力（上游客户端、账号池、SSE 解析、图片编码），
//      不需要外部脚本。
//
// 判定口径**逐条复刻原脚本**（不重设计），因为原口径是「harness 同款」，
// 改了会让两边对同一模型给出不同结论：
//
//	vision      : HTTP 2xx 且业务信封无拒识信号
//	non-vision  : 命中拒识信号（11133 / "does not support images" ...）
//	unknown     : 图片本身无效（11135）、传输错误、或无明确信号的其它状态码
//
// 关键细节：unknown 必须单独成态，不能并入 non-vision —— 上游对「图片无效」
// 的拒绝与「模型不支持图」是两件事，混起来会让所有模型被误判为不可识图。
package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"allplatforms/internal/platform/probeimage"
)

// State 探测结论三态。
type State string

const (
	StateVision    State = "vision"     // 确认可接收图像
	StateNonVision State = "non-vision" // 确认拒识图像
	StateUnknown   State = "unknown"    // 无明确结论（不猜）
)

// Probe 单个模型的探测结果。
type Probe struct {
	Vision    bool   `json:"vision"`     // 仅 state=vision 时为 true（前端按它分组）
	State     State  `json:"state"`      // vision / non-vision / unknown
	Note      string `json:"note"`       // 人读的判定依据（排障用）
	TestedAt  string `json:"tested_at"`  // RFC3339
	Source    string `json:"source"`     // 固定 "visionprobe"（区别于脚本的 probe_vision.py）
	Model     string `json:"-"`          // 不重复落盘（map 的 key 就是模型名）
}

// Snapshot 一次完整探测的结果（落盘格式与原脚本一致，前端无感）。
type Snapshot struct {
	Version   int               `json:"version"`
	Probes    map[string]Probe  `json:"probes"`
	Exists    bool              `json:"exists"`
	UpdatedAt string            `json:"updated_at"`
}

// ── 拒识/无效信号（逐条复刻原脚本）────────────────────────────────────

// nonVisionMarkers 模型明确不支持图的信号（命中任一即 non-vision）。
var nonVisionMarkers = []string{
	"11133",
	"model_does_not_support_images",
	"does not support images",
	"doesn't support images",
	"not support image",
	"not support images",
	"image input is not supported",
	"images are not supported",
	"this model does not support",
	"model cannot process image",
	"model can't process image",
}

// imageInvalidMarkers 图片**本身**无效（与模型能力无关）→ 必须归 unknown。
//
// 这组信号是原脚本踩出来的：上游对合成图/过期会话会返回 11135，
// 若把它当拒识信号，全部模型都会被误判为「不可识图」。
var imageInvalidMarkers = []string{
	"11135",
	"image_invalid",
	"image is invalid",
	"invalid image data",
	"start a new conversation",
	"replace the image",
	"image data rejected",
}

// ── 探测执行 ────────────────────────────────────────────────────────────

// Target 一个待探测的域：基址 + 鉴权 + 模型清单。
type Target struct {
	Label  string   // 展示名（日志/面板用）
	Base   string   // OpenAI 兼容面基址，如 http://127.0.0.1:7863/workbuddy/v1
	Key    string   // api_key（空 = 不带鉴权头）
	Models []string // 待测模型 id
}

// Config 探测参数。
type Config struct {
	// Interval 模型之间的间隔（原脚本默认 3s）。
	//
	// 为什么必须限速：逐个模型发图，不限速会把上游打到限流——
	// 实测 46 个模型 × 3s 把 3 个 Trae 账号全部打到 consecutive errors 冷却。
	Interval time.Duration
	// Timeout 单个模型请求超时（原脚本默认 60s）。
	Timeout time.Duration
	// Concurrency 并发度。**固定 1**：与上游限流口径一致，不提供调大入口。
	Concurrency int
}

// DefaultConfig 与原脚本一致的默认参数。
func DefaultConfig() Config {
	return Config{Interval: 3 * time.Second, Timeout: 60 * time.Second, Concurrency: 1}
}

// Runner 探测器。
type Runner struct {
	cfg    Config
	client *http.Client
	// now 可注入时间源（测试用）。
	now func() time.Time
}

// NewRunner 构造探测器。cfg 的零值字段回落默认。
func NewRunner(cfg Config) *Runner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Interval < 0 {
		cfg.Interval = 0
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	return &Runner{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		now:    time.Now,
	}
}

// Run 依次探测各 Target，返回合并后的快照。
//
// 合并语义（与原脚本的 merge_previous 一致）：**只保留本次出现的键**。
// 旧记录会被自然清掉——这是刻意的，避免「模型已下线但旧判定永不过期」。
//
// ctx 传 nil 视为 context.Background()：本方法在库内被调用，而
// `ctx.Err()` 对 nil 接口会 panic（实测踩到）。调用方漏传 ctx 时应退化为
// 「不取消」而不是崩掉整个探测。
func (r *Runner) Run(ctx context.Context, targets []Target) Snapshot {
	if ctx == nil {
		ctx = context.Background()
	}
	out := Snapshot{Version: 1, Probes: map[string]Probe{}, Exists: true}
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		r.runTarget(ctx, t, out.Probes)
	}
	out.UpdatedAt = r.now().Format(time.RFC3339)
	return out
}

// runTarget 探测一个域的所有模型。
func (r *Runner) runTarget(ctx context.Context, t Target, into map[string]Probe) {
	models := t.Models
	if len(models) == 0 {
		models = r.fetchModels(ctx, t)
	}
	if len(models) == 0 {
		return
	}
	for i, m := range models {
		if ctx.Err() != nil {
			return
		}
		if i > 0 && r.cfg.Interval > 0 {
			// 限速：可被 ctx 取消（关停/超时时不必等完整个间隔）。
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.cfg.Interval):
			}
		}
		state, note := r.probeOne(ctx, t, m)
		into[m] = Probe{
			Vision:   state == StateVision,
			State:    state,
			Note:     note,
			TestedAt: r.now().Format(time.RFC3339),
			Source:   "visionprobe",
			Model:    m,
		}
	}
}

// fetchModels 从 /v1/models 取模型 id 清单（Target.Models 为空时才用）。
func (r *Runner) fetchModels(ctx context.Context, t Target) []string {
	url := strings.TrimRight(t.Base, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	if t.Key != "" {
		req.Header.Set("Authorization", "Bearer "+t.Key)
		req.Header.Set("x-api-key", t.Key)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if id := strings.TrimSpace(d.ID); id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// probeOne 对一个模型发一条带图请求并判定。
func (r *Runner) probeOne(ctx context.Context, t Target, model string) (State, string) {
	status, body, transportErr := r.chatWithImage(ctx, t, model)
	return Classify(status, body, transportErr)
}

// chatWithImage 发一条带图的最小请求。
//
// max_tokens=1：探测只关心「上游接不接受这张图」，不需要模型真的回答。
// 用 1 能把成本与耗时压到最低（原脚本同款）。
func (r *Runner) chatWithImage(ctx context.Context, t Target, model string) (int, string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "image_url", "image_url": map[string]string{"url": probeimage.DataURL()}},
				{"type": "text", "text": "hi"},
			},
		}},
		"max_tokens": 1,
		"stream":     false,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	url := strings.TrimRight(t.Base, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if t.Key != "" {
		// 空 key 时**不要**发空鉴权头（网关会 400）——原脚本同款处理。
		req.Header.Set("Authorization", "Bearer "+t.Key)
		req.Header.Set("x-api-key", t.Key)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), nil
}

// Classify 三态判定（**逐条复刻原 Python 脚本的 classify**）。
//
// 顺序不能换：
//  1. 先查「图片本身无效」——它必须在拒识判定**之前**，否则 11135 里若
//     恰好含拒识子串就会误判成 non-vision；
//  2. 再看 2xx：即便成功也可能带业务信封拒识（网关透传上游错误体）；
//  3. 最后看非 2xx 的拒识信号；都没有则 unknown（不猜）。
func Classify(status int, body string, transportErr error) (State, string) {
	if transportErr != nil {
		return StateUnknown, "transport error: " + truncate(transportErr.Error(), 160)
	}
	text := strings.ToLower(body)

	if hit := firstMarker(text, imageInvalidMarkers); hit != "" {
		return StateUnknown, "图片本身无效/需新会话（非能力判定，不猜）: " + hit
	}
	if status >= 200 && status < 300 {
		if hit := firstMarker(text, nonVisionMarkers); hit != "" {
			return StateNonVision, "业务信封拒识 (2xx, marker=" + hit + ")"
		}
		return StateVision, fmt.Sprintf("HTTP %d 正常接收", status)
	}
	if hit := firstMarker(text, nonVisionMarkers); hit != "" {
		return StateNonVision, fmt.Sprintf("HTTP %d 拒识（图片不支持）: %s", status, hit)
	}
	if status != 0 {
		return StateUnknown, fmt.Sprintf("HTTP %d 但无明确拒识信号（不猜）", status)
	}
	return StateUnknown, "无状态码且无拒识信号（不猜）"
}

// firstMarker 返回第一个命中的标记（空 = 无命中）。
//
// 返回命中的具体标记而不是 bool：判定依据要写进 note 里，
// 排障时能直接看出「是哪个信号触发的判定」，不用回去翻代码。
func firstMarker(lowerText string, markers []string) string {
	for _, m := range markers {
		if strings.Contains(lowerText, m) {
			return m
		}
	}
	return ""
}

// truncate 截断字符串（避免 note 过长撑爆落盘文件）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── 统计（日志/面板用）─────────────────────────────────────────────────

// Counts 统计快照里三态各多少。
func (s Snapshot) Counts() (vision, nonVision, unknown int) {
	for _, p := range s.Probes {
		switch p.State {
		case StateVision:
			vision++
		case StateNonVision:
			nonVision++
		default:
			unknown++
		}
	}
	return
}

// Describe 一行摘要（日志用）。
func (s Snapshot) Describe() string {
	v, n, u := s.Counts()
	return fmt.Sprintf("vision=%d non-vision=%d unknown=%d (共 %d 个模型)", v, n, u, len(s.Probes))
}

// 确保 sync 引用（build 里用 once）。
var _ = sync.Once{}
