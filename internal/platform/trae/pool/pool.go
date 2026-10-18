// Package pool Trae 账号池：健康状态机 + 冷却 + 会话粘性 + state.json 持久化。
//
// 与 WorkBuddy2API 的 pool 互不共享内存：本池只索引 auths/trae-*.json。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"allplatforms/internal/platform/trae/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolPlan CoolKind = iota // 权益不足 → 长冷却
	CoolSoft                 // 429/404 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolPlan:
		return "plan_limit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 对外暴露的账号状态（不含任何 token）。
type Status struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	Credits   int64  `json:"credits"`       // 剩余积分
	Limit     int64  `json:"credits_limit"` // 总量
	Used      int64  `json:"credits_used"`  // 已用
	Packs     int    `json:"packs"`         // 权益包数
	CheckedIn bool   `json:"checked_in"`    // 今日是否已签到
	Cooling   bool   `json:"cooling"`
	Until     string `json:"until,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Disabled  bool   `json:"disabled"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note,omitempty"`

	ExpiresAt  string `json:"expires_at,omitempty"`  // token 过期时刻
	SharedWith int    `json:"shared_with,omitempty"` // 共同管理账号数

	// 运行态指标
	InFlight     int    `json:"in_flight"`
	SuccessCount int64  `json:"success_count,omitempty"`
	ErrTotal     int64  `json:"err_total,omitempty"`
	SuccessRate  string `json:"success_rate,omitempty"`
	LastSuccess  string `json:"last_success,omitempty"`
	LastErr      string `json:"last_err,omitempty"`
	LastErrMsg   string `json:"last_err_msg,omitempty"`
	TokensDelta  int64  `json:"tokens_delta,omitempty"` // 本进程窗口累计

	// 稳态状态机观测（借鉴 WorkBuddy2API）：9074 软冷却 / 连败降权 / 5xx 熔断
	SoftCooling  bool   `json:"soft_cooling,omitempty"`  // 9074 软冷却中（下周期自动重试）
	SoftUntil    string `json:"soft_until,omitempty"`
	SoftStreak   int    `json:"soft_streak,omitempty"`
	Degraded     bool   `json:"degraded,omitempty"`    // 连败降权临时出池
	DegradeUntil string `json:"degrade_until,omitempty"`
	Breaker      bool   `json:"breaker,omitempty"`     // 5xx 熔断暂停
	BreakerUntil string `json:"breaker_until,omitempty"`
}

type entry struct {
	a         *auth.Auth
	credits   int64
	limit     int64
	used      int64
	packs     int
	checkedIn bool
	lastCheck string // 签到状态归属的自然日（Asia/Shanghai）

	disabled bool
	enabled  bool
	reason   string
	until    time.Time

	inFlight     int
	lastUsed     time.Time
	successCount int64
	errTotal     int64
	lastSuccess  time.Time
	lastErr      time.Time
	lastErrMsg   string
	errStreak    int
	tokensDelta  int64

	// 借鉴 WorkBuddy2API 稳态状态机（traeapi2 后加的签到不稳，补这套）：
	//   - softUntil：9074/限流软冷却截止，带指数退避（softStreak 累加，封顶）。
	//   - degradeUntil：连续失败达阈临时出池（连败降权）。
	//   - breakerUntil：上游 5xx 熔断暂停。
	// 三者与 until（权益/错误冷却）、disabled 构成并列「或门」：任一项未到期即不可
	// 选（healthy 已天然汇总，见 healthy 方法）。
	softUntil     time.Time
	softStreak    int
	degradeUntil  time.Time
	consecutiveFails int
	breakerUntil  time.Time
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled || !e.enabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.softUntil.IsZero() && now.Before(e.softUntil) {
		return false
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

type stateEntry struct {
	Credits     int64    `json:"credits"`
	Limit       int64    `json:"credits_limit,omitempty"`
	Used        int64    `json:"credits_used,omitempty"`
	Packs       int      `json:"packs,omitempty"`
	CheckedIn   bool     `json:"checked_in,omitempty"`
	LastCheck   string   `json:"last_check,omitempty"`
	Disabled    bool     `json:"disabled"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Until       string   `json:"until,omitempty"`
	TokensDelta int64    `json:"tokens_delta,omitempty"`
	Note        string   `json:"note,omitempty"`
	SharedWith  []string `json:"shared_with,omitempty"`
}

type stateFile struct {
	Accounts map[string]stateEntry `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu          sync.RWMutex
	byUID       map[string]*entry
	stateFp     string
	affinity    map[string]affinityEntry
	affTTL      time.Duration
	maxInFlight int
	// 三因子权重参数（借鉴 WorkBuddy2API 的成熟选号统计口径）
	idleWeightPerHour float64
	idleWeightMax     float64
}

type affinityEntry struct {
	uid      string
	lastSeen time.Time
}

// New 构建池。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:             map[string]*entry{},
		stateFp:           stateFp,
		affinity:          map[string]affinityEntry{},
		affTTL:            30 * time.Minute,
		maxInFlight:       3,
		idleWeightPerHour: 0.5,
		idleWeightMax:     5.0,
	}
	if stateFp != "" {
		p.load()
	}
	return p
}

// SetWeights 设置闲置补偿参数（非正值回落默认）。
func (p *Pool) SetWeights(perHour, max float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if perHour > 0 {
		p.idleWeightPerHour = perHour
	}
	if max > 0 {
		p.idleWeightMax = max
	}
}

// SetMaxInFlight 单账号在途上限（<=0 不限）。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxInFlight = n
}

func (p *Pool) inFlightFullLocked(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight >= p.maxInFlight
}

// Acquire 占用账号在途名额。
func (p *Pool) Acquire(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.inFlight++
		return true
	}
	return false
}

// Release 释放账号在途名额。
func (p *Pool) Release(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.inFlight > 0 {
		e.inFlight--
	}
}

// RecordSuccess 成功计数：清零连败/熔断/软冷却（成功即健康）。
func (p *Pool) RecordSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.errStreak = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.softUntil = time.Time{}
	}
}

// RecordError 错误计数。
func (p *Pool) RecordError(uid, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.errStreak++
		e.lastErr = time.Now()
		e.lastErrMsg = msg
	}
}

// AddTokens 累计 token 消耗观测。
func (p *Pool) AddTokens(uid string, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && n > 0 {
		e.tokensDelta += n
	}
}

// Add 加入账号；已存在则保留状态、替换凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a
		return
	}
	p.byUID[a.UID] = &entry{a: a, enabled: true}
	p.saveLocked()
}

// SyncToDir 与 auths 目录对齐：新增加入、消失剔除。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a, enabled: true}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
	p.saveLocked()
}

// Remove 从池与 state 中删除账号（凭证文件由调用方删）。
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; !ok {
		return false
	}
	delete(p.byUID, uid)
	p.saveLocked()
	return true
}

// SetEnabled 软开关；不影响 disabled（凭证失效禁用）。
func (p *Pool) SetEnabled(uid string, enabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.enabled = enabled
	if !enabled && reason != "" {
		e.reason = reason
	}
	if enabled && e.reason != "" && !e.disabled && e.until.IsZero() {
		e.reason = ""
	}
	p.saveLocked()
	return true
}

// SetNote 备注。
func (p *Pool) SetNote(uid, note string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.a.Note = note
	e.reason = "" // 保留既有 reason，不同字段
	p.saveLocked()
	return true
}

// SetSharedWith 共同管理账号列表（标记哪些网关共享同一凭证）。
func (p *Pool) SetSharedWith(uid string, shared []string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.a.SharedWith = shared
	p.saveLocked()
	return true
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errStreak = 0
	}
	p.saveLocked()
}

// ---------------------------------------------------------------------------
// 借鉴 WorkBuddy2API 的稳态状态机：9074 软冷却 / 连败降权 / 5xx 熔断。
// 这套是 traeapi2 后加的签到长期不稳的根因补强——9074 不再被当作「放弃」，
// 而是软冷却 + 指数退避，下个整点周期由调度器自动重试（上游限流放开即签上）。
// ---------------------------------------------------------------------------

// 软冷却参数（对齐 WorkBuddy2API 的 CooldownSoftRate 哲学）：基础时长 + 指数退避，
// 封顶。softStreak 每遭一次 9074 自增，退避时长 = base<<min(streak, shiftMax) 并夹到
// [base, cap]；签到成功清零 streak。不写进冷却域 until（二者正交：软冷却只挡选号与
// 自动重试节奏，不动权益冻结语义）。
// 9074 是上游签到端点的**瞬时并发闸门**，秒级/分钟级就会放开，不是账号级故障。
// 原值（base 20min、指数翻倍、封顶 6h、streak 8）会把一次瞬时限流放大成
// 数小时甚至跨天的停车，导致当天再也不会重试——这正是"只有一个成功"的放大环节。
// 改为分钟级短冷却并封顶，保证当天总能再试。
const (
	softCoolBase  = 3 * time.Minute
	softCoolCap   = 30 * time.Minute
	softStreakMax = 3 // 3<< 封顶，最坏 24min
)

// SoftCoolForCheckin 记录一次 9074/限流软冷却（带指数退避）。返回实际冷却截止时刻。
// 连续命中累积 softStreak 拉长退避；success 由 SoftCoolClear 清零。
func (p *Pool) SoftCoolForCheckin(uid string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}
	}
	if e.softStreak < softStreakMax {
		e.softStreak++
	}
	shift := e.softStreak
	if shift > softStreakMax {
		shift = softStreakMax
	}
	d := softCoolBase
	for i := 0; i < shift; i++ {
		d *= 2
	}
	if d > softCoolCap {
		d = softCoolCap
	}
	e.softUntil = time.Now().Add(d)
	e.reason = "checkin soft rate (9074)"
	p.setDirtyLocked()
	return e.softUntil
}

// SoftCoolClear 签到成功时清零软冷却退避计数（允许下轮从 base 重新开始，防越堆越厚）。
func (p *Pool) SoftCoolClear(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.softStreak = 0
	e.softUntil = time.Time{}
	p.setDirtyLocked()
}

// NoteFailures 连败降权（对齐 WorkBuddy2API degrade）：consecutiveFails++，达阈临时
// 出池（degradeUntil），计数清零供下一轮重累计；成功（RecordSuccess/NoteSuccess）清零。
const (
	degradeThreshold      = 3
	defaultDegradeCooldown = 30 * time.Minute
	degradeCooldownMax    = 3 * time.Hour
)

func (p *Pool) NoteFailures(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.consecutiveFails++
	if e.consecutiveFails < degradeThreshold {
		p.setDirtyLocked()
		return
	}
	e.consecutiveFails = 0
	if !e.degradeUntil.IsZero() && time.Now().Before(e.degradeUntil) {
		// 已在降权期：不延长、不翻倍（防重试把降权越堆越厚），计数已清零。
		p.setDirtyLocked()
		return
	}
	d := defaultDegradeCooldown
	if d > degradeCooldownMax {
		d = degradeCooldownMax
	}
	e.degradeUntil = time.Now().Add(d)
	e.reason = "consecutive failures (degrade)"
	p.setDirtyLocked()
}

// NoteBreaker 上游 5xx 熔断：连续 5xx 暂停账号，避免对故障上游狂打。
const breakerThreshold = 3

func (p *Pool) NoteBreaker(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.errStreak++
	if e.errStreak < breakerThreshold {
		p.setDirtyLocked()
		return
	}
	e.errStreak = 0
	e.breakerUntil = time.Now().Add(defaultDegradeCooldown)
	e.reason = "upstream 5xx breaker"
	p.setDirtyLocked()
}

// setDirtyLocked 标记需落盘（兼容已有 saveLocked 的调用点；这里只置旗不写盘，
// 现有 Save/Flush 在变更后显式调 saveLocked，保持既有落盘时序）。
func (p *Pool) setDirtyLocked() {
	p.saveLocked()
}

// Disable 凭证失效禁用（需重登或人工解冻）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// Revive 解冻：清 disabled 与冷却。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.errStreak = 0
	p.saveLocked()
	return true
}

// NoteError 累计错误，达阈值自动冷却。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.errStreak++
	if threshold > 0 && e.errStreak >= threshold {
		e.until = time.Now().Add(d)
		e.reason = "consecutive errors"
		e.errStreak = 0
	}
	p.saveLocked()
}

// NoteSuccess 成功清连败/降权/熔断（不触积分/签到标记）。
//
// ── 必须同时清 reason（2026-10-18 修复）──────────────────────────────
//
// 原先只清计数器与各 Until，**不清 e.reason**。而 reason 只在 Revive
//（手动复活）里被清 —— 于是出现一个很误导的显示：
//
//	NoteError 写入 reason="consecutive errors" + until=now+N
//	→ 冷却到期（until 过去）→ Cooling 按 now.Before(until) 实时算，为 false
//	→ 账号已正常参与调度（实测刷额度/对话都成功）
//	→ 但 reason 字符串仍留在台账里，面板一直显示「consecutive errors」
//
// 用户看到「consecutive errors」会以为账号有故障，实际只是**状态残留**。
// 成功是「这个账号已经恢复正常」的最强信号，此时清掉 reason 才符合语义。
//
// 注意只清 e.reason，不动 disabled：disabled 是人工/熔断的显式停用状态，
// 不该因为一次成功就被悄悄放开（那是 Revive 的职责）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errStreak = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		e.breakerUntil = time.Time{}
		// 成功即代表冷却已无实际约束；连同 until 一起清，避免留下
		// 「已过期的 until + 空 reason」这种半清理状态。
		e.until = time.Time{}
		e.reason = ""
	}
}

// SetCredits 更新积分。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// SetUsage 更新额度三元组。余额恢复且处于权益冷却时自动解冻。
func (p *Pool) SetUsage(uid string, remain, limit, used int64, packs int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		e.limit = limit
		e.used = used
		e.packs = packs
		// 签到回血越过 0 → 解冻权益冷却（disabled 不动，那是凭证失效）
		if remain > 0 && !e.disabled && e.until.IsZero() == false && strings.Contains(e.reason, "plan") {
			e.until = time.Time{}
			e.reason = ""
		}
	}
	p.saveLocked()
}

// SetCheckedIn 标记今日已签到（按 Asia/Shanghai 自然日）。
func (p *Pool) SetCheckedIn(uid string, checked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.checkedIn = checked
		loc, _ := time.LoadLocation("Asia/Shanghai")
		if loc == nil {
			loc = time.Local
		}
		e.lastCheck = time.Now().In(loc).Format("2006-01-02")
	}
	p.saveLocked()
}

// AuthByUID 取完整凭证。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// TodayCheckedIn 今日是否已签到（跨自然日自动失效）。
func (p *Pool) TodayCheckedIn(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.Local
	}
	today := time.Now().In(loc).Format("2006-01-02")
	return e.checkedIn && e.lastCheck == today
}

// PickAffinity 按会话粘性选号，降级时走加权择优并绑定。
func (p *Pool) PickAffinity(sessionKey string, tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	if sessionKey != "" {
		if aff, ok := p.affinity[sessionKey]; ok {
			if tried == nil || !tried[aff.uid] {
				if e, exists := p.byUID[aff.uid]; exists && e.healthy(now) && !p.inFlightFullLocked(e) {
					aff.lastSeen = now
					p.affinity[sessionKey] = aff
					e.lastUsed = now
					return e.a
				}
			}
		}
	}
	best := p.pickBestLocked(tried, now)
	if best == nil {
		return nil
	}
	if sessionKey != "" {
		p.affinity[sessionKey] = affinityEntry{uid: best.a.UID, lastSeen: now}
		if len(p.affinity) > 500 { // 惰性清理过期绑定
			for k, v := range p.affinity {
				if now.Sub(v.lastSeen) > p.affTTL {
					delete(p.affinity, k)
				}
			}
		}
	}
	return best.a
}

// Pick 无粘性择优。
func (p *Pool) Pick() *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.pickBestLocked(nil, time.Now()); e != nil {
		return e.a
	}
	return nil
}

const minPickGap = 100 * time.Millisecond

// weightOf 三因子权重：积分占比 ×10 + 闲置补偿 + 成功率 ×3。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	if maxCredits > 0 && e.credits > 0 {
		w += (float64(e.credits) / float64(maxCredits)) * 10.0
	}
	if !e.lastUsed.IsZero() {
		if idleH := now.Sub(e.lastUsed).Hours(); idleH > 0 {
			idleW := idleH * p.idleWeightPerHour
			if idleW > p.idleWeightMax {
				idleW = p.idleWeightMax
			}
			w += idleW
		}
	}
	if total := e.successCount + e.errTotal; total > 0 {
		w += (float64(e.successCount) / float64(total)) * 3.0
	}
	return w
}

// pickBestLocked 健康 → 未满载 → Top5 加权 → 防惊群窗口。
func (p *Pool) pickBestLocked(tried map[string]bool, now time.Time) *entry {
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		cands = append(cands, e)
	}
	var relaxed bool
	var full []*entry
	for _, e := range cands {
		if p.inFlightFullLocked(e) {
			full = append(full, e)
		}
	}
	if len(cands) > len(full) {
		var keep []*entry
		for _, e := range cands {
			if p.inFlightFullLocked(e) {
				continue
			}
			keep = append(keep, e)
		}
		cands = keep
	} else if len(cands) == 0 {
		relaxed = true
	}
	_ = relaxed
	if len(cands) == 0 {
		return nil
	}
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ws = append(ws, weighted{e: e, w: p.weightOf(e, maxCredits, now)})
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	top := ws
	if len(top) > 5 {
		top = top[:5]
	}
	var eligible []*entry
	for _, it := range top {
		if now.Sub(it.e.lastUsed) >= minPickGap {
			eligible = append(eligible, it.e)
		}
	}
	selected := top[0].e
	if len(eligible) > 0 {
		selected = eligible[0]
	} else {
		for _, it := range top[1:] {
			if it.e.lastUsed.Before(selected.lastUsed) {
				selected = it.e
			}
		}
	}
	selected.lastUsed = now
	return selected
}

// Counts 返回概览计数。
func (p *Pool) Counts() (total, healthy, cooling, disabled int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.until.IsZero() && now.Before(e.until):
			cooling++
		case e.healthy(now):
			healthy++
		}
	}
	return
}

// Status 单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// List 全量状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	var nick, note string
	var shared int
	if e.a != nil {
		nick = e.a.Nickname
		note = e.a.Note
		shared = len(e.a.SharedWith)
	}
	rate := ""
	if total := e.successCount + e.errTotal; total > 0 {
		rate = fmtRate(float64(e.successCount) / float64(total))
	}
	var exp string
	if e.a != nil {
		if unix := e.a.ExpiresAtUnix(); unix > 0 {
			exp = time.Unix(unix, 0).Format(time.RFC3339)
		}
	}
	return Status{
		UID: uid, Nickname: nick, Credits: e.credits, Limit: e.limit, Used: e.used,
		Packs: e.packs, CheckedIn: p.todayCheckedInLocked(e), Note: note,
		Cooling: !e.until.IsZero() && now.Before(e.until), Until: fmtTime(e.until), Reason: e.reason,
		Disabled: e.disabled, Enabled: e.enabled, ExpiresAt: exp, SharedWith: shared,
		InFlight: e.inFlight, SuccessCount: e.successCount, ErrTotal: e.errTotal,
		SuccessRate: rate, LastSuccess: fmtTime(e.lastSuccess),
		LastErr: fmtTime(e.lastErr), LastErrMsg: e.lastErrMsg, TokensDelta: e.tokensDelta,
		SoftCooling: !e.softUntil.IsZero() && now.Before(e.softUntil),
		SoftUntil:   fmtTime(e.softUntil),
		SoftStreak:  e.softStreak,
		Degraded:    !e.degradeUntil.IsZero() && now.Before(e.degradeUntil),
		DegradeUntil: fmtTime(e.degradeUntil),
		Breaker:     !e.breakerUntil.IsZero() && now.Before(e.breakerUntil),
		BreakerUntil: fmtTime(e.breakerUntil),
	}
}

func (p *Pool) todayCheckedInLocked(e *entry) bool {
	if !e.checkedIn {
		return false
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.Local
	}
	return e.lastCheck == time.Now().In(loc).Format("2006-01-02")
}

// fmtRate 把 0-1 的成功率渲染成百分比字符串（一位小数，去尾零）。
func fmtRate(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(v*100, 'f', 1, 64), "0"), ".") + "%"
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		enabled := true
		if s.Enabled != nil {
			enabled = *s.Enabled
		}
		e := &entry{
			a:       &auth.Auth{UID: uid}, // placeholder，SyncToDir/Add 时替换
			credits: s.Credits, limit: s.Limit, used: s.Used, packs: s.Packs,
			checkedIn: s.CheckedIn, lastCheck: s.LastCheck,
			disabled: s.Disabled, enabled: enabled, reason: s.Reason,
			tokensDelta: s.TokensDelta,
		}
		if s.Until != "" {
			if t, terr := time.Parse(time.RFC3339, s.Until); terr == nil {
				e.until = t
			}
		}
		// 冷却已过期的残留自愈（2026-10-18 修复）。
		//
		// 台账里可能存着「until 已过去、但 reason 还留着」的半清理状态
		//（历史版本 NoteSuccess 不清 reason 留下的）。加载时如果发现
		// until 已过期，就把 until 与 reason 一并清掉 —— 否则面板会一直
		// 显示「consecutive errors」，让用户以为账号有故障，实际账号早已
		// 正常参与调度（Cooling 是按 now.Before(until) 实时算的）。
		//
		// 只清冷却类残留：不动 disabled（那是显式停用状态）。
		if !e.until.IsZero() && time.Now().After(e.until) {
			e.until = time.Time{}
			e.reason = ""
		}
		a := &auth.Auth{UID: uid, Note: s.Note, SharedWith: s.SharedWith}
		e.a = a
		p.byUID[uid] = e
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]stateEntry{}}
	for uid, e := range p.byUID {
		se := stateEntry{
			Credits: e.credits, Limit: e.limit, Used: e.used, Packs: e.packs,
			CheckedIn: e.checkedIn, LastCheck: e.lastCheck,
			Disabled: e.disabled, Reason: e.reason, Until: fmtTime(e.until),
			TokensDelta: e.tokensDelta,
		}
		if e.a != nil {
			se.Note = e.a.Note
			se.SharedWith = e.a.SharedWith
		}
		if !e.enabled {
			f := false
			se.Enabled = &f
		}
		sf.Accounts[uid] = se
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}

// Flush 显式落盘（停机前调用）。
func (p *Pool) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}
