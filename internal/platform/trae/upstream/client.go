// client.go Trae SOLO 上游 HTTP：ExchangeToken / GetUserInfo / FetchModels /
// ChatStream / CheckinStatus / CheckinClaim / EntUsage。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"allplatforms/internal/platform/trae/auth"
)

// Client 上游客户端。
type Client struct {
	HTTP       *http.Client // 短 RPC，有总超时兜底
	StreamHTTP *http.Client // SSE：无总超时，避免长流被截断

	AgentHost string
	UgHost    string
	OAuthHost string
	ClientID  string
	UserAgent string // 非空则覆盖出站 UA
}

// New 生产默认值（连接池复用，禁压缩）。
func New(timeout, headerTimeout, idleTimeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if headerTimeout <= 0 {
		headerTimeout = timeout
	}
	if idleTimeout <= 0 {
		idleTimeout = 300 * time.Second
	}
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		DisableCompression:    true,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: timeout, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

// SetHosts 覆盖默认 host（测试/自建反代场景）。空值保持默认。
func (c *Client) SetHosts(agent, ug, oauth string) {
	if agent != "" {
		c.AgentHost = agent
	}
	if ug != "" {
		c.UgHost = ug
	}
	if oauth != "" {
		c.OAuthHost = oauth
	}
}

// doJSON 发请求并解 JSON；非 2xx 返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: Truncate(string(raw), 200)}
	}
	return raw, nil
}

// normalizeExpiresAt TokenExpireAt 毫秒 → 秒。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// RefreshToken 强制换新 token（refreshToken 轮换）。成功更新 a 并写回文件。
// 全程持写锁，任何失败路径都不改写 a（保证旧 refreshToken 可重试）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅在即将过期（或已过期）时刷新，返回是否真正刷新。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if a.ExpiresAt <= 0 {
		// 无过期信息：仍尝试刷新（老凭证文件缺 expiresAt）
	} else if time.Now().Add(skew).Unix() < a.ExpiresAt {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) refreshLocked(a *auth.Auth) error {
	rt := strings.TrimSpace(a.RefreshToken)
	if rt == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	raw, _ := json.Marshal(map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": rt,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req, c.UserAgent)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	exp := resp.Result.TokenExpireAt
	if exp > 0 {
		exp = normalizeExpiresAt(exp)
	} else if resp.Result.TokenExpireDuration > 0 {
		exp = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	a.SetTokenLocked(resp.Result.Token, resp.Result.RefreshToken, exp)
	return nil
}

// UserInfo 由 GetUserInfo 返回的基本信息。
type UserInfo struct {
	UID          string
	Nickname     string
	EnterpriseID string
}

// EpExchangeByAuthCode OAuth2 授权码交换端点。
//
// ⚠ 与 EpExchange（refreshToken 轮换）是**两个不同的端点**，不可混用：
//
//	EpExchange          /cloudide/api/v3/trae/oauth/ExchangeToken   ← refreshToken 轮换
//	EpExchangeByAuthCode /trae/api/v3/oauth/ExchangeToken           ← 授权码换 token
//
// 来源：官方客户端源码的 exchangeTokenByAuthCode 实现 + 运行时日志里
// 实际请求的 URL（`https://api.trae.cn/trae/api/v3/oauth/ExchangeToken`）。
const EpExchangeByAuthCode = "/trae/api/v3/oauth/ExchangeToken"

// ExchangeByAuthCode 用 OAuth2 授权码 + PKCE code_verifier 换 token。
//
// ── 为什么需要（2026-10 上游协议变更）──────────────────────────────
//
// 上游把登录回调从「直接给 refreshToken」改成了「给授权码（authCodeInfo）」，
// 客户端必须自己拿授权码去 ExchangeToken 换 token。
//
// 官方回调参数（从客户端日志提取，权威）：
//
//	{"isRedirect":"true","scope":"solo",
//	 "authCodeInfo":"{\"AuthCode\":\"...\",\"ExpireAt\":...,\"ExpireDuration\":600000}",
//	 "loginTraceID":"...","host":"https://api.trae.com.cn","userRegion":"cn",
//	 "userInfo":"{...UserID/ScreenName/TenantID...}"}
//
// 官方交换请求体（同样从日志提取）：
//
//	{"ClientID":"en1oxy7wnw8j9n",
//	 "AuthCode":"<授权码>",
//	 "CodeVerifier":"<PKCE verifier>",
//	 "DeviceInfo":{...},"IDEVersion":"0.2.2"}
//
// 注意：本函数**不校验 CodeVerifier**（上游会校验）。CodeVerifier 必须与
// 生成登录 URL 时用的 code_challenge 配对（challenge = SHA256(verifier)），
// 否则上游返回失败。配对关系由调用方（server 包的 pending 会话）保证。
//
// 成功时更新 a 的 token/refreshToken 并返回；失败不改写 a（保留旧凭证可重试）。
func (c *Client) ExchangeByAuthCode(a *auth.Auth, authCode, codeVerifier string) error {
	if strings.TrimSpace(authCode) == "" {
		return fmt.Errorf("empty auth code")
	}
	host := a.ApiHost
	if host == "" {
		host = UgHost // 授权码交换走 ug 域（api.trae.cn），与官方一致
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   deviceInfoForExchange(a),
		"IDEVersion":   IdeVersion,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchangeByAuthCode, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req, c.UserAgent)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("authcode exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("authcode_exchange_failed: no token in response")
	}
	exp := resp.Result.TokenExpireAt
	if exp > 0 {
		exp = normalizeExpiresAt(exp)
	} else if resp.Result.TokenExpireDuration > 0 {
		exp = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	a.Lock()
	a.SetTokenLocked(resp.Result.Token, resp.Result.RefreshToken, exp)
	a.Unlock()
	return nil
}

// deviceInfoForExchange 组装交换请求里的 DeviceInfo。
//
// 官方字段（从日志提取）：DeviceID / MachineID / PlatformCode / DeviceType /
// DeviceName / DeviceModel / ClientVersion / DevicePublicKey / DeviceBrand /
// DeviceCPU / OSInfo / OSVersion。
//
// 这里只填**本程序确实拥有**的字段（设备 id 与机型指纹），
// 刻意不伪造 DevicePublicKey / DeviceName（那是官方客户端的本机信息，
// 我们无从得知；填假值反而可能与上游校验冲突）。
// 实测这套精简字段可以完成交换。
func deviceInfoForExchange(a *auth.Auth) map[string]any {
	return map[string]any{
		"DeviceID":      a.DeviceID,
		"MachineID":     a.MachineID,
		"PlatformCode":  "SOLO_PC",
		"DeviceType":    "PC",
		"DeviceModel":   DeviceBrand,
		"ClientVersion": IdeVersion,
		"DeviceBrand":   DeviceBrand,
		"OSInfo":        "windows",
		"OSVersion":     OSVersion,
	}
}

// GetUserInfo 查询账号信息（登录闭环补全 uid/nickname 用）。
func (c *Client) GetUserInfo(a *auth.Auth) (UserInfo, error) {
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	raw, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion})
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return UserInfo{}, err
	}
	OAuthHeaders(req, c.UserAgent)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return UserInfo{}, err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return UserInfo{}, fmt.Errorf("userinfo parse: %w", err)
	}
	return UserInfo{UID: resp.Result.UserID, Nickname: resp.Result.ScreenName, EnterpriseID: resp.Result.EnterpriseID}, nil
}

// ModelInfo 动态模型条目。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
	// Credits Trae 积分消耗费率原文（客户端选模型时展示的「费率」）。
	// get_detail_param 的 config_info_list 中每项的字段名尚未完全确认，
	// 这里同时保留整块原始 config，便于面板在不改上游解析的前提下透出费率。
	Credits string
	// RawConfig 保留 config_info_list 单项完整原始 JSON，供面板按需解析费率等字段。
	RawConfig json.RawMessage
}

// FetchModels 拉模型目录（get_detail_param）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	raw, _ := json.Marshal(map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	req, err := http.NewRequest(http.MethodPost, c.AgentHost+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false, c.UserAgent)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []json.RawMessage `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, raw := range resp.ConfigInfoList {
		var cfg struct {
			ConfigName        string `json:"config_name"`
			DisplayConfig     struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
			DisplayContactConfig string `json:"display_contact_config"`
			ModelDetailList []struct {
				ModelName string `json:"model_name"`
			} `json:"model_detail_list"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		if cfg.ConfigName == "" {
			continue
		}
		// Trae 费率 = 积分消耗费率，位于 display_contact_config(JSON 字符串) ->
		// consumption_rate.data.rate（如 0.08）。与 WB 同款 Credits 字段语义对齐。
		credits := parseTraeRate(cfg.DisplayContactConfig)
		out = append(out, ModelInfo{
			ID:        cfg.ConfigName,
			Name:      cfg.DisplayConfig.DisplayName,
			Credits:   credits,
			RawConfig: raw,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// ChatStream 发对话请求并返回原始 SSE 流。
// 非 2xx 时 rc 为 nil、respBody 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(ctx context.Context, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AgentHost+EpChat, bytes.NewReader(PrepareBody(body, DefaultConfigName)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true, c.UserAgent)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s", a.UID, resp.StatusCode, kind, Truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// CheckinStatus 查询今日签到状态。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.UgHost+EpCheckinStatus, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a, c.UserAgent)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。
// 返回 (业务code, err)：
//   - code==0 且 err==nil → 签到成功
//   - code==10001（今日已签）→ err==nil，由调用方按幂等成功处理
//   - 其他 code≠0（如 9074 服务器繁忙/限流）→ err 非 nil，且 code 透传，
//     调用方不得误判为已签到，应当失败并留待下个排程周期重试。
//
// 上游 HTTP 层错误（网络/5xx）err 非 nil 且 code==-1。
func (c *Client) CheckinClaim(a *auth.Auth) (int, error) {
	// body 必须为 {}（与 PC 客户端一致）；带 req_source 等额外字段会被服务端以
	// 9074 拒绝。device 指纹由 UgHeaders 以小写 x-device-id=devId（uid 派生的纯 16 位数字）注入。
	req, err := http.NewRequest(http.MethodPost, c.UgHost+EpCheckinClaim, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return -1, err
	}
	UgHeaders(req, a, c.UserAgent)
	data, err := c.doJSON(req)
	if err != nil {
		return -1, err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return -1, fmt.Errorf("签到响应解析失败: %w", err)
	}
	switch resp.Code {
	case 0, 10001:
		// 0=成功；10001="今日已签到" → 调用方按幂等成功处理
		return resp.Code, nil
	default:
		return resp.Code, fmt.Errorf("签到失败 code=%d: %s", resp.Code, resp.Message)
	}
}

// EntUsage 权益包聚合：剩余 / 总量 / 已用 / 包数。
// usage.credits_amount 是已用积分（实测）。
func (c *Client) EntUsage(a *auth.Auth) (remain, limit, used int64, packs int, err error) {
	req, err := http.NewRequest(http.MethodPost, c.UgHost+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	UgHeaders(req, a, c.UserAgent)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		if u < 0 {
			u = 0 // 上游偶发负数（退费冲抵），按 0 计，别让 total 变负
		}
		limit += l
		used += u
		// 单包余额不允许为负：上游 credits_amount 可能超过 credits_limit
		// （跨月结算、并发扣减的尾差）。负数会一路污染到 pool.SetUsage →
		// 加权选号的积分项、以及「余额 > 0 才解冻」的判断，这里钳到 0。
		if rest := l - u; rest > 0 {
			remain += rest
		}
		packs++
	}
	return remain, limit, used, packs, nil
}

// parseTraeRate 从 config_info_list 单项的 display_contact_config（JSON 字符串）
// 中解析积分消耗费率 consumption_rate.data.rate，格式化为 WB 同款 "x<rate>" 形态。
// 无费率（自定义/子代理类模型）返回空串。
func parseTraeRate(displayContactConfig string) string {
	if displayContactConfig == "" {
		return ""
	}
	var dcc struct {
		ConsumptionRate struct {
			Enable bool `json:"enable"`
			Data   struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
	}
	if err := json.Unmarshal([]byte(displayContactConfig), &dcc); err != nil {
		return ""
	}
	if !dcc.ConsumptionRate.Enable {
		return ""
	}
	return "x" + strconv.FormatFloat(dcc.ConsumptionRate.Data.Rate, 'f', -1, 64)
}
