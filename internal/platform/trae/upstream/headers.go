package upstream

import (
	"net/http"

	"allplatforms/internal/platform/trae/auth"
)

// clientUA 出站 User-Agent：Trae/<IdeVersion>。
const clientUA = "Trae/" + IdeVersion

// SOLOHeaders 设置 llm_utils_chat / get_detail_param 所需的上游专属头族。
func SOLOHeaders(req *http.Request, a *auth.Auth, stream bool, customUA string) {
	ua := clientUA
	if customUA != "" {
		ua = customUA
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", ua)

	at := a.JWT() // 读锁快照，防与 RefreshToken 写并发竞态
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "macos")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// UgHeaders 设置签到 / 积分（api.trae.cn）所需头。
// 头部必须严格对齐真实 PC 客户端（Trae SOLO CN 桌面端）的签到请求，
// 否则服务端会以 9074「当前参与用户太多」拒绝。
//
// 实测结论（2026-10-12，单号直连 api.trae.cn 矩阵复现，推翻旧「uid 派生」结论）：
//   - 小写 x-device-id 必须是【非确定性的随机 16 位数字】。用「由 uid 派生的固定值」
//     作为 did，服务端判定为批量伪造设备 → 统一 9074；任意随机 16 位数字 did 一律
//     code:0 签到成功。故 did 必须经 crypto/rand 生成（见 auth.NewCheckinDeviceID），
//     绝不从 uid 确定性派生。
//   - 服务端按 (token, deviceId) 组合去重：某 did 被该账号占用后返回 9095「今日已签」，
//     换一个未占用的 did 即可再签；故 did 不宜长期固定，有被风控标记的风险。
//   - 不发 X-Machine-Id（PC 客户端签到请求均不发送该头）。
//   - body 走 {}（见 CheckinClaim / CheckinStatus），不带 req_source。
//
// 错误码语义（直连实测确认）：
//   - 9074 = 设备字段被风控判定为批量伪造（uid 派生固定 did / 缺失 / 非法格式）；
//   - 9095 = 该 (账号, 设备) 今日已签到（换不同 did 即可绕过，故非硬卡）；
//   - 9004 = 缺少设备字段（裸请求无 x-device-id）。
func UgHeaders(req *http.Request, a *auth.Auth, customUA string) {
	ua := clientUA
	if customUA != "" {
		ua = customUA
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	// 设备指纹：小写 x-device-id = 随机 16 位数字 deviceId（登录/真机下发或随机生成，
	// 绝不由 uid 派生）。见 auth.EnsureCheckinDeviceID 说明。
	if a.DeviceID != "" {
		req.Header.Set("x-device-id", a.DeviceID)
	}
}

// OAuthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func OAuthHeaders(req *http.Request, customUA string) {
	ua := clientUA
	if customUA != "" {
		ua = customUA
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
}
