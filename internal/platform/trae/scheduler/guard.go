// guard.go 任务级互斥：防止同一批量任务被多个触发源并发跑。
//
// ── 为什么需要这个文件（真实根因，2026-10-14 实测确认）───────────────
//
// 现象：Trae 3 个账号，整点自动签到稳定只有 1 个成功，另外 2 个 9074 失败。
// 但**手动在面板点签到**时 3 个全成功——所以问题一直没被发现。
//
// 根因：9074 是上游签到端点的**全局并发闸门**（不是单账号限流），
// 同一时刻只放行少量请求。而本调度器有 **4 个触发源**可以同时进入
// CheckinAll，且**完全没有互斥**：
//
//	① 定时调度器 tick（CheckinHours 到点）
//	② Trae 自有 HTTP 接口 POST /api/checkin_all
//	③ 面板平台动作 /panel/api/platforms/trae/actions/checkin
//	④ 面板的 WorkBuddy 签到 hook（WorkBuddy 签到跑完顺带触发 Trae）
//
// 实测日志铁证（同一账号出现**重复的退避序号**，说明有两条链在并行跑）：
//
//	06:11:01 checkin 3454...: 9074 退避重试 (1/5)   ┐ 两条独立重试链
//	06:11:08 checkin 3454...: 9074 退避重试 (1/5)   ┘
//	06:11:35 checkin 3454...: 9074 退避重试 (2/5)   ┐
//	06:11:47 checkin 3454...: 9074 退避重试 (2/5)   ┘
//	...
//	06:13:21 checkin 3454...: 9074 退避重试 (5/5)   ┐ 两条链同时到 5/5
//	06:13:21 checkin 3454...: 9074 退避重试 (5/5)   ┘
//	06:13:26 checkin 3454...: 9074 退避重试 (1/5)   ← 补签又起一条新链
//
// 于是形成**自我踩踏**：A 链在退避等待时，B 链的请求正好占住闸门，
// A 醒来重试又被 B 占着 → 两条链互相把对方挤掉，最后一起 90 秒超时失败。
//
// ── 为什么手动测试看不出来 ──────────────────────────────────────────
//
// 手动点「全量签到」时，没有定时任务在并发抢闸门；而且 CheckinAll 里有
// sweepBusy 补签（等 20~30s 后重试），此时首轮赢家已经签完不占闸门，
// 补签轻松成功 → 3 个全绿。**这个补签机制本身是对的，但它掩盖了
// 「并发触发导致首轮全输」这个真问题。**
//
// ── 修法 ───────────────────────────────────────────────────────────
//
// 给每个批量任务加**任务级互斥**：已有同任务在跑时，后来的触发直接返回
// 「跳过（已在执行）」，不再起第二条链。
//
// 为什么不是「排队等待」而是「直接跳过」：
//   - 排队会让后到的触发白等 90 秒，且它等待期间闸门仍被前一条链占用，
//     排到了也是重复劳动——签到是按天幂等的，跑两遍没有任何增量收益；
//   - 面板的交互契约是 fire-and-forget（点完就走），让它阻塞反而破坏体验；
//   - 「跳过」语义对调用方最诚实：如实告诉它「这次没跑，因为已经在跑了」，
//     而不是假装成功（那正是本 bug 被掩盖的方式）。
package scheduler

import "sync"

// taskGuard 任务级互斥：TryAcquire 拿不到锁说明同任务已在执行。
//
// 用 TryLock 而不是 Lock：调用方要的是「拿不到就跳过」的语义，
// 阻塞等待会把「重复触发」变成「串行重复执行」，问题更隐蔽。
type taskGuard struct {
	mu sync.Mutex
	// running 各任务是否在执行（key 为任务名）。
	// 用 map 而不是多个具名 Mutex：任务清单会增长（将来加任务体系），
	// 具名字段每加一个任务都要改结构体。
	running map[string]bool
}

func newTaskGuard() *taskGuard {
	return &taskGuard{running: map[string]bool{}}
}

// TryAcquire 尝试开始一个任务；返回 false 表示该任务已在执行。
// 调用方必须仅在返回 true 时执行任务，并在结束时 Release。
func (g *taskGuard) TryAcquire(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[name] {
		return false
	}
	g.running[name] = true
	return true
}

// Release 标记任务结束。
func (g *taskGuard) Release(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.running, name)
}

// IsRunning 该任务是否在执行（观测用，不改变状态）。
func (g *taskGuard) IsRunning(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[name]
}

// 任务名常量（互斥键）。用常量而非字面量：拼错字面量会静默地让互斥失效。
const (
	taskNameCheckin   = "checkin"
	taskNameKeepalive = "keepalive"
	taskNameBalance   = "balance"
)
