package scheduler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/upstream"
)

// 生产用真退避（数秒级），但测试里会让单测跑上百秒。
// 这里在测试进程启动时把"纯等待"参数归零，CheckinOne 内的重试退避
// 由各用例自行按需归零（见 TestCheckinBusyRetry 等）。
func TestMain(m *testing.M) {
	checkinStagger = 0
	checkinStaggerJitter = 1
	checkinSweepGap = 0
	checkinSweepJitter = 1
	os.Exit(m.Run())
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// fakeUp 起一套可计数的假上游，响应可配置。
type fakeUp struct {
	srv       *httptest.Server
	nStatus   atomic.Int32
	nClaim    atomic.Int32
	nEnt      atomic.Int32
	nExchange atomic.Int32

	checkinChecked bool
	checkinEnable  bool
	claimCode      int
	claimMsg       string
	claimSequence  []int // 非空则按序返回 code（用于模拟 9074→成功）
	claimSeqIdx    int
	entBody        string
	exchangeOK     bool
	// exchangeDead=true 模拟凭证失效（401 / ErrSessionDead），
	// 用于验证保活抽象层的「立即告警」路径（2026-10-24 新增）。
	exchangeDead bool
}

func newFakeUp(t *testing.T) *fakeUp {
	t.Helper()
	f := &fakeUp{
		checkinEnable: true,
		claimMsg:      "ok",
		entBody: `{"user_entitlement_pack_list":[` +
			`{"entitlement_base_info":{"quota":{"credits_limit":1000}},"usage":{"credits_amount":200}}]}`,
		exchangeOK: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(upstream.EpCheckinStatus, func(w http.ResponseWriter, r *http.Request) {
		f.nStatus.Add(1)
		body := `{"checked_in":false,"credits":10,"enable":true}`
		switch {
		case f.checkinChecked:
			body = `{"checked_in":true,"credits":0,"enable":true}`
		case !f.checkinEnable:
			body = `{"checked_in":false,"credits":0,"enable":false}`
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc(upstream.EpCheckinClaim, func(w http.ResponseWriter, r *http.Request) {
		f.nClaim.Add(1)
		code := f.claimCode
		if len(f.claimSequence) > 0 {
			code = f.claimSequence[f.claimSeqIdx]
			if f.claimSeqIdx < len(f.claimSequence)-1 {
				f.claimSeqIdx++
			}
		}
		_, _ = w.Write([]byte(`{"code":` + itoa(int64(code)) + `,"message":"` + f.claimMsg + `"}`))
	})
	mux.HandleFunc(upstream.EpEntUsage, func(w http.ResponseWriter, r *http.Request) {
		f.nEnt.Add(1)
		_, _ = w.Write([]byte(f.entBody))
	})
	mux.HandleFunc(upstream.EpExchange, func(w http.ResponseWriter, r *http.Request) {
		f.nExchange.Add(1)
		// exchangeDead：模拟凭证失效（401 → upstream 分类为 ErrSessionDead）。
		// 与 exchangeOK=false 的区别：两者都返回 401，但 exchangeDead 用于
		// 明确表达「这是终态失效」的测试意图（保活应告警而非静默失败）。
		if f.exchangeDead || !f.exchangeOK {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"Result":{"Token":"NEW","RefreshToken":"NEWRT","TokenExpireAt":1786847930141}}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUp) client() *upstream.Client {
	c := upstream.New(3*time.Second, 3*time.Second, 3*time.Second)
	c.SetHosts(f.srv.URL, f.srv.URL, f.srv.URL)
	return c
}

// newSched 组一套池 + 假上游 + 调度器。
func newSched(t *testing.T, n int) (*Scheduler, *fakeUp, *pool.Pool) {
	t.Helper()
	f := newFakeUp(t)
	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	for i := 0; i < n; i++ {
		uid := "u" + itoa(int64(i))
		p.Add(&auth.Auth{
			AccessToken: "AT", RefreshToken: "RT", UID: uid, Nickname: uid,
			MachineID: "M", DeviceID: "1234567890123456",
			ApiHost: f.srv.URL, FilePath: filepath.Join(dir, "trae-"+uid+".json"),
			ExpiresAt: time.Now().Add(-time.Hour).Unix(), // 强制走刷新路径
		})
	}
	s := New(Config{
		Pool: p, Upstream: f.client(),
		CheckinHours: []int{9}, KeepaliveHours: []int{22},
		RefreshSkew: time.Hour,
	})
	return s, f, p
}

// ── 签到 ────────────────────────────────────────────────────────────────

func TestCheckinAllSuccess(t *testing.T) {
	s, f, p := newSched(t, 2)
	sum := s.CheckinAll(time.Now())
	if sum.Total != 2 || sum.OK != 2 || sum.Failed != 0 {
		t.Errorf("汇总不对: %+v", sum)
	}
	if f.nClaim.Load() != 2 {
		t.Errorf("claim 应调 2 次，实为 %d", f.nClaim.Load())
	}
	for _, uid := range []string{"u0", "u1"} {
		if !p.TodayCheckedIn(uid) {
			t.Errorf("%s 未标记已签到", uid)
		}
		if st, _ := p.Status(uid); st.Credits != 800 {
			t.Errorf("%s 余额未回填: %d", uid, st.Credits)
		}
	}
	if len(sum.Results) != 2 {
		t.Errorf("明细应有 2 条（UI 逐行展示）: %d", len(sum.Results))
	}
}

// 今日已签 → 跳过，不再打 claim（幂等，省一次上游调用）。
func TestCheckinAllIdempotent(t *testing.T) {
	s, f, p := newSched(t, 1)
	p.SetCheckedIn("u0", true)
	sum := s.CheckinAll(time.Now())
	if sum.Skipped != 1 || sum.OK != 0 {
		t.Errorf("应跳过: %+v", sum)
	}
	if f.nClaim.Load() != 0 {
		t.Errorf("已签不该再 claim: %d", f.nClaim.Load())
	}
	if len(sum.Results) == 0 || !sum.Results[0].Skipped {
		t.Errorf("明细应标 skipped: %+v", sum.Results)
	}
}

// enable=false（账号没开通签到）→ 不算失败，也不 claim。
func TestCheckinAllNotEnabled(t *testing.T) {
	s, f, p := newSched(t, 1)
	f.checkinEnable = false
	sum := s.CheckinAll(time.Now())
	if sum.Failed != 0 {
		t.Errorf("未开通签到不该算失败: %+v", sum)
	}
	if f.nClaim.Load() != 0 {
		t.Errorf("未开通不该 claim: %d", f.nClaim.Load())
	}
	if p.TodayCheckedIn("u0") {
		t.Error("未开通不该标记已签到")
	}
}

// claim 返回业务错误（上游「已签到」）→ 按幂等成功处理，不算失败。
func TestCheckinAllClaimIdempotent(t *testing.T) {
	s, f, _ := newSched(t, 1)
	f.claimCode = 10001
	f.claimMsg = "already checked in"
	if sum := s.CheckinAll(time.Now()); sum.Failed != 0 {
		t.Errorf("幂等错误不该算失败: %+v", sum)
	}
}

// 上游不可达 → 记 failed 且带原因（不 panic）。
func TestCheckinAllUpstreamError(t *testing.T) {
	s, f, _ := newSched(t, 1)
	f.srv.Close()
	sum := s.CheckinAll(time.Now())
	if sum.Failed != 1 {
		t.Errorf("上游不可达应记失败: %+v", sum)
	}
	if len(sum.Results) == 0 || sum.Results[0].Err == "" {
		t.Errorf("失败明细应带原因: %+v", sum.Results)
	}
}

// CheckinOne：status 已签 → 记状态 + 查余额 + 返回 0 积分。
func TestCheckinOneAlreadyChecked(t *testing.T) {
	_, f, p := newSched(t, 1)
	f.checkinChecked = true
	credits, msg, _, err := CheckinOne(f.client(), p, p.AuthByUID("u0"))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if credits != 0 {
		t.Errorf("已签不应领到积分: %d", credits)
	}
	if msg == "" {
		t.Error("应给出说明文案")
	}
	if !p.TodayCheckedIn("u0") {
		t.Error("应标记已签到")
	}
}

func TestCheckinOneFresh(t *testing.T) {
	_, f, p := newSched(t, 1)
	if _, _, _, err := CheckinOne(f.client(), p, p.AuthByUID("u0")); err != nil {
		t.Fatalf("签到失败: %v", err)
	}
	if !p.TodayCheckedIn("u0") {
		t.Error("未标记已签到")
	}
}

func TestCheckinOneStatusError(t *testing.T) {
	_, f, p := newSched(t, 1)
	f.srv.Close()
	if _, _, _, err := CheckinOne(f.client(), p, p.AuthByUID("u0")); err == nil {
		t.Error("上游不可达应报错")
	}
}

// 回归：claim 返回 9074（服务器繁忙/限流）是「失败」而非「幂等成功」。
// 旧逻辑把所有 claim 错误都当已签到 → 标记 checked_in 且永不再重试，导致「假签到成功、积分拿不到」。
// 修复后该错误必须：①CheckinOne 返回 err；②不标记今日已签到（留给下个排程周期重试）。
func TestCheckinOneClaimBusy(t *testing.T) {
	s, f, p := newSched(t, 1)
	f.claimCode = 9074
	f.claimMsg = "当前参与用户太多，请稍后再试"

	// 测试里把退避压成 0，避免挂 240s；只验证「最终 9074 仍失败且不标记」。
	oldBase, oldJit := checkinBackoffBase, checkinBackoffJitter
	checkinBackoffBase, checkinBackoffJitter = 0, 0
	defer func() { checkinBackoffBase, checkinBackoffJitter = oldBase, oldJit }()

	// CheckinOne 层：应报错且未标记已签到。
	_, _, _, err := CheckinOne(f.client(), p, p.AuthByUID("u0"))
	if err == nil {
		t.Fatal("9074 应报失败，而非幂等成功")
	}
	if p.TodayCheckedIn("u0") {
		t.Fatal("9074 失败不得标记今日已签到（否则永不再重试）")
	}

	// CheckinAll 层：应计入 failed 且不标记已签到。
	sum := s.CheckinAll(time.Now())
	if sum.Failed != 1 {
		t.Errorf("9074 应记 1 个失败，实为 %+v", sum)
	}
	if p.TodayCheckedIn("u0") {
		t.Error("CheckinAll 后不得标记已签到")
	}
}

// ── 9074 全局并发闸门：多账号必须全部签到成功 ─────────────────────────────
//
// 这是本次缺陷的回归用例。原始现象：3 个账号在整点几乎同时发起，
// 上游 9074 闸门同时只放行少量请求 → 稳定只有 1 个成功、其余 2 个 9074。
// 修复前账号间只隔 800ms 且 9074 当场判死进长冷却，故必然只成功 1 个。
func TestCheckinAllBusyGateAllAccountsRecover(t *testing.T) {
	s, f, p := newSched(t, 3)
	// 首轮每个账号都撞 9074（模拟闸门瞬时打满）。
	f.claimCode = 9074
	f.claimMsg = "当前参与用户太多，请稍后再试"

	oldBase, oldJit := checkinBackoffBase, checkinBackoffJitter
	checkinBackoffBase, checkinBackoffJitter = 0, 0
	defer func() { checkinBackoffBase, checkinBackoffJitter = oldBase, oldJit }()

	// 用 sequence 精确编排：每个账号首轮 9074（共 3 次），之后全部成功。
	f.claimSequence = []int{9074, 9074, 9074, 0, 0, 0, 0, 0, 0}

	sum := s.CheckinAll(time.Now())
	if sum.OK != 3 {
		t.Fatalf("3 个账号都应通过补签签上，实为 OK=%d Failed=%d (%+v)",
			sum.OK, sum.Failed, sum.Results)
	}
	for _, uid := range []string{"u0", "u1", "u2"} {
		if !p.TodayCheckedIn(uid) {
			t.Errorf("%s 未标记已签到（补签机制失效）", uid)
		}
	}
}

// 补签轮次用尽仍 9074 → 交软冷却，但冷却必须是分钟级（当天还能再试），
// 不能是原来的小时/跨天级。
func TestCheckinAllBusyExhaustedCoolsBriefly(t *testing.T) {
	s, f, p := newSched(t, 1)
	f.claimCode = 9074 // 始终 9074，补签必然用尽

	oldBase, oldJit := checkinBackoffBase, checkinBackoffJitter
	checkinBackoffBase, checkinBackoffJitter = 0, 0
	defer func() { checkinBackoffBase, checkinBackoffJitter = oldBase, oldJit }()

	sum := s.CheckinAll(time.Now())
	if sum.OK != 0 {
		t.Fatalf("始终 9074 不该有成功: %+v", sum)
	}
	if p.TodayCheckedIn("u0") {
		t.Error("失败不得标记已签到")
	}
	st, _ := p.Status("u0")
	if !st.SoftCooling {
		t.Fatal("补签用尽后应进入软冷却")
	}
	// 冷却必须短：不得超过 30 分钟（当天还有机会再试）。
	until, err := time.Parse(time.RFC3339, st.SoftUntil)
	if err != nil {
		t.Fatalf("SoftUntil 格式无法解析: %q", st.SoftUntil)
	}
	if d := time.Until(until); d > 30*time.Minute {
		t.Errorf("9074 冷却过长（%v），当天无法再试，会重现\"只有一个成功\"", d)
	}
}

// 模拟真实高峰：前 4 次 9074，第 5 次成功。验证 9074 退避重试能拿到积分并标记已签到。
func TestCheckinOneClaimBusyThenSucceeds(t *testing.T) {
	_, f, p := newSched(t, 1)
	f.claimSequence = []int{9074, 9074, 9074, 9074, 0}
	f.claimMsg = "当前参与用户太多，请稍后再试"

	// 退避压成 0 加速测试。
	oldBase, oldJit := checkinBackoffBase, checkinBackoffJitter
	checkinBackoffBase, checkinBackoffJitter = 0, 0
	defer func() { checkinBackoffBase, checkinBackoffJitter = oldBase, oldJit }()

	if _, _, _, err := CheckinOne(f.client(), p, p.AuthByUID("u0")); err != nil {
		t.Fatalf("重试后应成功，却报错: %v", err)
	}
	if f.nClaim.Load() != 5 {
		t.Errorf("应触发 5 次 claim（4 次 9074 + 1 次成功），实为 %d", f.nClaim.Load())
	}
	if !p.TodayCheckedIn("u0") {
		t.Error("成功后应标记已签到")
	}
}

// ── 保活 ────────────────────────────────────────────────────────────────

func TestKeepaliveAllRefreshes(t *testing.T) {
	s, f, p := newSched(t, 2)
	sum := s.KeepaliveAll(time.Now())
	if sum.OK != 2 || sum.Failed != 0 {
		t.Errorf("保活应全成功: %+v", sum)
	}
	if f.nExchange.Load() != 2 {
		t.Errorf("应换 token 2 次，实为 %d", f.nExchange.Load())
	}
	for _, uid := range []string{"u0", "u1"} {
		if got := p.AuthByUID(uid).JWT(); got != "NEW" {
			t.Errorf("%s token 未刷新: %q", uid, got)
		}
	}
}

// 未到刷新窗口 → 不调 ExchangeToken，但仍算「保活完成」。
func TestKeepaliveSkipsFreshToken(t *testing.T) {
	s, f, p := newSched(t, 1)
	acct := p.AuthByUID("u0")
	acct.Lock()
	acct.ExpiresAt = time.Now().Add(100 * time.Hour).Unix()
	acct.Unlock()

	if sum := s.KeepaliveAll(time.Now()); sum.OK != 1 {
		t.Errorf("应算完成: %+v", sum)
	}
	if f.nExchange.Load() != 0 {
		t.Errorf("未到窗口不该换 token: %d", f.nExchange.Load())
	}
}

func TestKeepaliveRefreshFails(t *testing.T) {
	s, f, _ := newSched(t, 1)
	f.exchangeOK = false
	sum := s.KeepaliveAll(time.Now())
	if sum.Failed != 1 {
		t.Errorf("应记失败: %+v", sum)
	}
	if len(sum.Results) == 0 || sum.Results[0].Err == "" {
		t.Errorf("应带原因: %+v", sum.Results)
	}
}

// ── 余额刷新 ────────────────────────────────────────────────────────────

func TestRefreshBalances(t *testing.T) {
	_, f, p := newSched(t, 2)
	sum := RefreshBalances(f.client(), p)
	if sum.OK != 2 || sum.Failed != 0 {
		t.Errorf("应全成功: %+v", sum)
	}
	if f.nEnt.Load() != 2 {
		t.Errorf("应查 2 次，实为 %d", f.nEnt.Load())
	}
	st, _ := p.Status("u0")
	if st.Credits != 800 || st.Packs != 1 {
		t.Errorf("额度未入池: %+v", st)
	}
	if len(sum.Results) == 0 || sum.Results[0].Msg == "" {
		t.Errorf("明细应带积分摘要: %+v", sum.Results)
	}
}

// 额度查询失败 → 记 failed，且不覆盖池里的旧值（避免一次抖动把余额清零）。
func TestRefreshBalancesErrorKeepsOldValue(t *testing.T) {
	_, f, p := newSched(t, 1)
	p.SetUsage("u0", 555, 1000, 445, 1)
	f.srv.Close()
	if sum := RefreshBalances(f.client(), p); sum.Failed != 1 {
		t.Errorf("应记失败: %+v", sum)
	}
	if st, _ := p.Status("u0"); st.Credits != 555 {
		t.Errorf("失败时不该覆盖旧值: %d", st.Credits)
	}
}

func TestRefreshBalancesEmptyPool(t *testing.T) {
	_, f, p := newSched(t, 0)
	if sum := RefreshBalances(f.client(), p); sum.Total != 0 {
		t.Errorf("空池应全 0: %+v", sum)
	}
}

// ── 余额后台刷新循环 ────────────────────────────────────────────────────

func TestStartBalanceRefreshRuns(t *testing.T) {
	s, f, _ := newSched(t, 1)
	ctx, cancel := contextWithTimeout(900 * time.Millisecond)
	defer cancel()
	s.StartBalanceRefresh(ctx, 30*time.Millisecond)

	// 轮询等而不是死睡固定时长：CI 机器负载高时调度器启动会滞后，
	// 固定 sleep + 紧阈值会随机假失败。这里只要求「循环确实跑起来了」。
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if f.nEnt.Load() >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("周期刷新未启动（跑了 %d 次，应 ≥2）", f.nEnt.Load())
}

// interval<=0 不启动（不空转打上游）。
func TestStartBalanceRefreshDisabled(t *testing.T) {
	s, f, _ := newSched(t, 1)
	ctx, cancel := contextWithTimeout(200 * time.Millisecond)
	defer cancel()
	s.StartBalanceRefresh(ctx, 0)
	time.Sleep(120 * time.Millisecond)
	if f.nEnt.Load() != 0 {
		t.Errorf("interval=0 不该启动: %d", f.nEnt.Load())
	}
}

// ── Run 主循环 ──────────────────────────────────────────────────────────

// Run 收到取消信号就退出，否则进程关不掉。
func TestRunExitsOnContextCancel(t *testing.T) {
	s, _, _ := newSched(t, 0)
	ctx, cancel := contextWithTimeout(150 * time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
}

// 全部任务禁用 → Run 阻塞等待退出，不空转。
func TestRunBlocksWhenAllDisabled(t *testing.T) {
	s, _, _ := newSched(t, 0)
	s.Reconfigure(nil, nil, true, true)
	ctx, cancel := contextWithTimeout(150 * time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
}

// tick 到点要按各自时点派发（签到 9 点 / 保活 22 点）。
func TestTickDispatchesByHour(t *testing.T) {
	s, f, _ := newSched(t, 1)

	f.nClaim.Store(0)
	f.nExchange.Store(0)
	s.tick(time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local))
	if f.nClaim.Load() == 0 {
		t.Error("9 点该触发签到")
	}

	f.nClaim.Store(0)
	f.nExchange.Store(0)
	s.tick(time.Date(2026, 1, 1, 22, 0, 0, 0, time.Local))
	if f.nExchange.Load() == 0 {
		t.Error("22 点该触发保活")
	}

	f.nClaim.Store(0)
	f.nExchange.Store(0)
	s.tick(time.Date(2026, 1, 1, 15, 0, 0, 0, time.Local))
	if f.nClaim.Load() != 0 || f.nExchange.Load() != 0 {
		t.Error("15 点不该触发任何任务")
	}
}

// 任务被禁用时 tick 不派发。
func TestTickRespectsDisabled(t *testing.T) {
	s, f, _ := newSched(t, 1)
	s.Reconfigure([]int{9}, nil, true, false) // 关签到，保活无时点
	f.nClaim.Store(0)
	s.tick(time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local))
	if f.nClaim.Load() != 0 {
		t.Error("禁用后不该触发签到")
	}
}
