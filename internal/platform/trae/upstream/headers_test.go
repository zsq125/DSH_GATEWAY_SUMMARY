package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"allplatforms/internal/platform/trae/auth"
)

// TestUgHeadersSetsDeviceID 覆盖签到/积分写接口的设备指纹头形态。
//
// 用 httptest.Server 实际发起一次 CheckinClaim，在服务端捕获请求头，
// 断言小写 x-device-id = "<deviceId>-<uid>"、Authorization 已带上，
// 且不含 PC 客户端不发送的多余头（X-Machine-Id / X-User-Region / X-Uid）。
// 全程只打本地 loopback。
func TestUgHeadersSetsDeviceID(t *testing.T) {
	var got http.Header
	var hit bool
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	})
	c, _ := newTestClient(t, mux)

	if _, err := c.CheckinClaim(testAuth()); err != nil {
		t.Fatalf("claim 失败: %v", err)
	}
	if !hit {
		t.Fatal("claim 请求未到达 mock 上游")
	}
	// 小写 x-device-id = 纯 deviceId（随机 16 位数字，不带 uid 后缀）。
	if v := got.Get("x-device-id"); v != "1234567890123456" {
		t.Errorf("x-device-id 应为 '<deviceId>'，实为 %q", v)
	}
	if v := got.Get("Authorization"); v != "Cloud-IDE-JWT AT" {
		t.Errorf("Authorization 应为 Cloud-IDE-JWT <token>，实为 %q", v)
	}
	// PC 客户端不发送这些头，网关须严格对齐，否则 9074。
	if _, ok := got["X-Machine-Id"]; ok {
		t.Errorf("不应发送 X-Machine-Id，实为 %q", got.Get("X-Machine-Id"))
	}
	if _, ok := got["X-User-Region"]; ok {
		t.Errorf("不应发送 X-User-Region，实为 %q", got.Get("X-User-Region"))
	}
	if _, ok := got["X-Uid"]; ok {
		t.Errorf("不应发送 X-Uid，实为 %q", got.Get("X-Uid"))
	}
}

// TestUgHeadersOmitsDeviceIDWhenEmpty：DeviceID/UID 为空时不设置 x-device-id。
func TestUgHeadersOmitsDeviceIDWhenEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "AT", UID: "u1", MachineID: "", DeviceID: ""}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	UgHeaders(req, a, "")
	if _, ok := req.Header["x-device-id"]; ok {
		t.Errorf("DeviceID 为空时不应设置 x-device-id，实为 %q", req.Header.Get("x-device-id"))
	}
	if req.Header.Get("Authorization") != "Cloud-IDE-JWT AT" {
		t.Errorf("Authorization 实为 %q", req.Header.Get("Authorization"))
	}
}

// TestUgHeadersCustomUA：customUA 非空时覆盖默认 UA，设备指纹头不受影响。
func TestUgHeadersCustomUA(t *testing.T) {
	a := &auth.Auth{AccessToken: "AT", UID: "u1", MachineID: "M1", DeviceID: "D1"}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	UgHeaders(req, a, "MyUA/1.0")
	if req.Header.Get("User-Agent") != "MyUA/1.0" {
		t.Errorf("自定义 UA 未生效: %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("x-device-id") != "D1" {
		t.Errorf("设备指纹头应为 D1，实为 %q", req.Header.Get("x-device-id"))
	}
}

// TestCheckinClaimAlreadySignedIsNilError：10001「今日已签」按幂等成功，
// code 透传但 err 必须为 nil（否则调度器会误判为失败而不标记）。
func TestCheckinClaimAlreadySignedIsNilError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, jsonHandler(`{"code":10001,"message":"今日已签到"}`))
	c, _ := newTestClient(t, mux)
	code, err := c.CheckinClaim(testAuth())
	if err != nil {
		t.Errorf("10001 应按幂等成功返回 nil error，实为 %v", err)
	}
	if code != 10001 || CheckinCodeAlready != 10001 {
		t.Errorf("code 应透传为 10001，实为 %d", code)
	}
}

// TestCheckinClaimBusyReturns9074：9074 服务器繁忙/限流必须返回 error 且 code 透传，
// 这是调度器不误标 checked_in 的前提。
func TestCheckinClaimBusyReturns9074(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, jsonHandler(`{"code":9074,"message":"server busy"}`))
	c, _ := newTestClient(t, mux)
	code, err := c.CheckinClaim(testAuth())
	if err == nil {
		t.Fatal("9074 应报错")
	}
	if code != 9074 {
		t.Errorf("应透传 code=9074，实为 %d", code)
	}
}

// TestCheckinClaimHTTPErrorIsMinusOne：HTTP 层错误 code 固定为 -1。
func TestCheckinClaimHTTPErrorIsMinusOne(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinClaim, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	})
	c, _ := newTestClient(t, mux)
	code, err := c.CheckinClaim(testAuth())
	if err == nil {
		t.Fatal("HTTP 500 应报错")
	}
	if code != -1 {
		t.Errorf("HTTP 层错误 code 应为 -1，实为 %d", code)
	}
}