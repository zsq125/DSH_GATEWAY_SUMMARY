// bgwork.go 后台任务的**可等待**封装。
//
// 修的是一个既有缺陷（在合并前就存在，只是被 flaky 测试掩盖）：
//
//	finishImport 导入成功后起了一个「首次签到」的 goroutine（login.go:247），
//	但它**没有被任何东西跟踪**。测试里账号的 FilePath 落在 t.TempDir() 下，
//	而这个 goroutine 会去写那个文件（签到回查余额会 SetCredits → 落盘）。
//	于是出现竞态：`go test` 收尾调 RemoveAll 删临时目录时，goroutine 可能
//	正好在写文件 → "The directory is not empty" → 用例随机失败。
//
// 实测频率约 1/6，且**在原始 traeapi2 仓库里同样复现**（不是合并引入的）。
//
// 修法：给 Handler 加一个 WaitGroup 跟踪这类后台任务，并暴露 WaitBackground。
// 生产路径不受影响（进程常驻，不调 WaitBackground）；测试在 cleanup 里
// 等一次即可彻底消除竞态。
//
// 为什么用「显式等待」而不是「改成同步」：首次签到要打上游、逐号限速，
// 同步会让登录回调的响应挂住数秒——那正是当初写成 goroutine 的原因。
package server

import "sync"

// bgWG 跟踪 handler 起的后台任务（见文件头说明）。
// 零值可用；Handler 构造时无需初始化。
type bgWait struct{ wg sync.WaitGroup }

// Go 起一个被跟踪的后台任务。
func (b *bgWait) Go(fn func()) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		fn()
	}()
}

// Wait 等所有被跟踪的后台任务结束。
func (b *bgWait) Wait() { b.wg.Wait() }

// WaitBackground 等待本 handler 起的后台任务结束（供测试收尾调用）。
//
// 生产代码不需要调用它：进程退出时这些任务随进程一起消失。
// 测试必须在 t.TempDir() 清理之前调用，否则会与文件写入竞态。
func (h *Handler) WaitBackground() { h.bg.Wait() }
