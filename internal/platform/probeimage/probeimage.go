// probeimage.go 探测用图片的**纯 Go 生成**。
//
// 背景（为什么不能随便用一张图）：上游对「纯色/无效」合成图会返回
// code 11135 / image_invalid / "start a new conversation"，这与
// 「模型不支持图」是**两件完全不同的事**。若用纯色图探测，所有模型都会
// 落进 image_invalid，判定结果全是 unknown——探测就白做了。
//
// 故这里复刻原 Python 探测脚本的做法：生成一张**有真实像素内容**的图
// （渐变底 + 确定性噪点，96x96）。原脚本用 PIL，这里用 Go 标准库
// image/png 实现，口径完全一致：
//   - 同样的尺寸 96x96
//   - 同样的渐变公式
//   - 同样的伪随机噪点（自实现 LCG，seed 固定 → 每次生成同一张图）
//
// 为什么 seed 固定：探测结果要可复现。同一模型两次探测应得同一结论，
// 换图会引入无关变量（万一某张图恰好触发了别的上游校验）。
package probeimage

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"sync"
)

// probeSize 探测图边长（与原脚本一致：96x96 够小，且含足够像素内容）。
const probeSize = 96

// probeSeed 与原脚本同款固定种子（20261012）。
//
// 用自实现 LCG 而不是 math/rand：Go 的 math/rand 算法在版本间会变
//（Go 1.20 起全局源换了实现），跨版本可能生成不同的图；LCG 是自己写的，
// 永远稳定，探测结果不会因为升级 Go 而漂移。
const probeSeed = 20261012

var (
	once   sync.Once
	b64    string
	rawPNG []byte
)

// lcg 线性同余伪随机数（数值取自 Numerical Recipes）。
// 只为复刻原脚本的「确定性噪点」，不需要密码学质量。
type lcg uint32

func (l *lcg) next() int {
	*l = lcg(uint64(*l)*1664525 + 1013904223)
	return int(*l >> 16) // 取高位，低位周期短
}

// randRange 返回 [lo, hi] 闭区间内的整数。
func (l *lcg) randRange(lo, hi int) int {
	return lo + l.next()%(hi-lo+1)
}

// PNG 返回探测图的原始 PNG 字节（进程内只生成一次，缓存复用）。
func PNG() []byte {
	build()
	return rawPNG
}

// Base64 返回探测图的 base64（data URL 里用）。
func Base64() string {
	build()
	return b64
}

// DataURL 返回可直接塞进 image_url.url 的 data URL。
func DataURL() string { return "data:image/png;base64," + Base64() }

// build 生成探测图（sync.Once 保证只跑一次）。
func build() {
	once.Do(func() {
		img := image.NewRGBA(image.Rect(0, 0, probeSize, probeSize))
		rng := lcg(probeSeed)
		for y := 0; y < probeSize; y++ {
			for x := 0; x < probeSize; x++ {
				// 与原脚本同款公式：渐变底 + ±25 噪点。
				//   红：随 x 增大（120→220）
				//   绿：随 y 增大（80→200）
				//   蓝：随 x 减小（200→120）
				noise := func() int { return rng.randRange(-25, 25) }
				r := 120 + 100*x/probeSize + noise()
				g := 80 + 120*y/probeSize + noise()
				b := 200 - 80*x/probeSize + noise()
				img.Set(x, y, color.RGBA{
					R: clamp8(r), G: clamp8(g), B: clamp8(b), A: 255,
				})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			// 编码失败不该发生（纯内存 RGBA）；兜底给一张极小的合法 PNG，
			// 保证调用方永远拿到可用的图，不会因为这里失败而整轮探测崩掉。
			rawPNG = fallbackPNG()
			b64 = base64.StdEncoding.EncodeToString(rawPNG)
			return
		}
		rawPNG = buf.Bytes()
		b64 = base64.StdEncoding.EncodeToString(rawPNG)
	})
}

// clamp8 钳到 [0,255]（渐变+噪点可能越界）。
func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// fallbackPNG 兜底图：8x8 单色 PNG（合法但内容单调）。
//
// 只在 png.Encode 失败时用到（实际不会发生）。它可能导致探测落 unknown，
// 但那是「探测不出结论」而不是「崩溃」，符合探测模块「宁可不猜」的基调。
func fallbackPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(128 + x*8), G: uint8(128 + y*8), B: 160, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
