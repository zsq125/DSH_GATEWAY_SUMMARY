// exports.go 对既有 trae scheduler 的**纯增量**补充：把包级函数
// RefreshBalances 暴露成 Scheduler 方法，供平台适配层（platform.Platform）
// 以统一形态调用。
//
// 为什么单独一个文件而不是改 scheduler.go：scheduler.go 是从 traeapi2
// 原样移植的，保持「一行未改」能把合并带来的回归面压到最小。这里只加
// 一个薄包装，不触碰任何既有逻辑。
package scheduler

import "log"

// BalanceRefreshNow 全量刷新余额（同步，逐号轻限速）。
//
// 注意调用方语义：本方法是**同步阻塞**的（逐号打上游 + 每号 200ms 限速），
// 平台适配层必须放到 goroutine 里异步触发，否则会把面板按钮卡住。
//
// 与 CheckinAll/KeepaliveAll 同款任务级互斥（见 guard.go）：并发触发
// 会重复打上游（余额查询本身幂等，但白耗额度与风控暴露面）。
func (s *Scheduler) BalanceRefreshNow() Summary {
	if s == nil {
		return Summary{}
	}
	if !s.guard.TryAcquire(taskNameBalance) {
		log.Printf("balance: 已有余额刷新在执行，本次触发跳过")
		return Summary{Results: []Result{{Msg: "已在执行，本次触发跳过"}}}
	}
	defer s.guard.Release(taskNameBalance)

	return RefreshBalances(s.cfg.Upstream, s.cfg.Pool)
}
