// deviceid_rotation_test.go 9074 换 did 自愈的用例。
//
// 钉住的根因（2026-10-14 直连上游探针实测）：
//
//	同一账号、同一时刻，只换 x-device-id：
//	  did=原值   → {"code":9074,"message":"当前参与用户太多，请稍后再试"}
//	  did=随机新值 → {"code":0,"message":"success"}
//
// 原实现遇到 9074 只做退避重试、不换 did，于是一个被风控标记的 did
// 会被反复重试到 90 秒超时 → 「这个账号今天怎么都签不上」。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/upstream"
)

// 关键用例：9074 必须触发换 did，且换完能签上。
//
// 假上游按 did 判定：只放行「不是原 did」的请求 —— 精确复刻实测现象。
func TestCheckinOneRotatesDeviceIDOn9074(t *testing.T) {
	const originalDID = "1111111111111111"

	var sawOtherDID atomic.Bool
	var claimsWithOriginal atomic.Int32

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true}`))
		case strings.Contains(r.URL.Path, "checkin_credits/claim"):
			did := r.Header.Get("x-device-id")
			if did == originalDID {
				claimsWithOriginal.Add(1)
				// 原 did 一律 9074（复刻「设备指纹被标记」）
				_, _ = w.Write([]byte(`{"code":9074,"message":"当前参与用户太多，请稍后再试"}`))
				return
			}
			sawOtherDID.Store(true)
			// 新 did 一律成功（复刻实测：换 did 立刻 code:0）
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.Contains(r.URL.Path, "ide_user_ent_usage"):
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{
		UID: "u1", AccessToken: "AT", RefreshToken: "RT",
		DeviceID: originalDID, MachineID: "M",
	}
	p.Add(a)
	up := upstream.New(time.Second, time.Second, time.Second)
	up.SetHosts(srv.URL, srv.URL, srv.URL)

	// 把退避压到最小，让测试快
	oldBase, oldJitter := checkinBackoffBase, checkinBackoffJitter
	defer func() { checkinBackoffBase, checkinBackoffJitter = oldBase, oldJitter }()
	checkinBackoffBase, checkinBackoffJitter = time.Millisecond, time.Nanosecond

	_, _, code, err := CheckinOne(up, p, a)
	if err != nil {
		t.Fatalf("换 did 后应签到成功，实为 err=%v code=%d", err, code)
	}
	if !sawOtherDID.Load() {
		t.Error("9074 后必须换 did 重试 —— 这是上游给出的自愈路径")
	}
	if claimsWithOriginal.Load() == 0 {
		t.Error("应先用原 did 尝试一次（不该跳过首次尝试）")
	}
	if a.DeviceID == originalDID {
		t.Error("账号的 DeviceID 应已更新为新随机值（且落盘）")
	}
	if !is16Digits(a.DeviceID) {
		t.Errorf("新 did 必须是 16 位纯数字，实为 %q", a.DeviceID)
	}
}

// 每次 9074 都要换新 did（而不是只换一次）。
//
// 理由：上游按 (token, deviceId) 组合判定。若新 did 也恰好被拒，
// 下一轮必须再换一个才有机会；固定一个 did 重试等于没换。
func TestCheckinOneRotatesEveryRound(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true}`))
		case strings.Contains(r.URL.Path, "checkin_credits/claim"):
			did := r.Header.Get("x-device-id")
			mu.Lock()
			seen[did]++
			n := len(seen)
			mu.Unlock()
			// 前 3 个 did 都 9074，第 4 个才成功
			if n < 4 {
				_, _ = w.Write([]byte(`{"code":9074,"message":"busy"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.Contains(r.URL.Path, "ide_user_ent_usage"):
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "AT", RefreshToken: "RT", DeviceID: "2222222222222222"}
	p.Add(a)
	up := upstream.New(time.Second, time.Second, time.Second)
	up.SetHosts(srv.URL, srv.URL, srv.URL)

	oldBase, oldJitter := checkinBackoffBase, checkinBackoffJitter
	defer func() { checkinBackoffBase, checkinBackoffJitter = oldBase, oldJitter }()
	checkinBackoffBase, checkinBackoffJitter = time.Millisecond, time.Nanosecond

	_, _, _, err := CheckinOne(up, p, a)
	if err != nil {
		t.Fatalf("应在若干轮后签上，实为 %v", err)
	}

	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n < 4 {
		t.Errorf("应尝试了至少 4 个不同的 did（每轮换新），实为 %d 个: %v", n, seen)
	}
}

// 非 9074 错误（如 5xx）不该换 did —— 那与设备指纹无关。
func TestCheckinOneDoesNotRotateOnOtherErrors(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":false,"enable":true}`))
		case strings.Contains(r.URL.Path, "checkin_credits/claim"):
			_, _ = w.Write([]byte(`{"code":5000,"message":"internal"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	p := pool.New("")
	const did = "3333333333333333"
	a := &auth.Auth{UID: "u1", AccessToken: "AT", RefreshToken: "RT", DeviceID: did}
	p.Add(a)
	up := upstream.New(time.Second, time.Second, time.Second)
	up.SetHosts(srv.URL, srv.URL, srv.URL)

	_, _, _, err := CheckinOne(up, p, a)
	if err == nil {
		t.Fatal("非 9074 错误应透传失败")
	}
	if a.DeviceID != did {
		t.Errorf("非 9074 错误不该换 did，实为 %q", a.DeviceID)
	}
}

// is16Digits 测试辅助（与 auth 包内的 is16Digit 同口径）。
func is16Digits(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
