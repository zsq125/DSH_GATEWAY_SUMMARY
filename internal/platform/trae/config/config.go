// Package config traeapi2 配置加载（JSON + TRA2A_* 环境变量覆盖）。
//
// 与 Gateway 共享 key（更省事），也支持通过 TRA2A_SHARED_KEY 与 Trae 后端
// 共用同一管理密钥。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7862"
	APIKey    string `json:"api_key"`    // 网关鉴权（/v1/* 与写接口）
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// Upstream 凭据：第三方 Trae 网关（apis）
	Upstream struct {
		BaseURL string `json:"base_url"` // 如 https://api.example.com
		APIKey  string `json:"api_key"`  // upstream api key
		// Upstream Base：理论上 https://solo-api-sg-...  (含模型*)
		TimeoutSeconds       int    `json:"timeout_seconds"`        // 短 RPC 总时长上限，默认 120
		HeaderTimeoutSeconds int    `json:"header_timeout_seconds"` // SSE 首字节前上限，<=0 回落 TimeoutSeconds
		IdleTimeoutSeconds   int    `json:"idle_timeout_seconds"`   // SSE 流中空闲上限，<=0 回落 300
		UserAgent            string `json:"user_agent"`             // 出站 UA 覆盖
	} `json:"upstream"`

	OAuth struct {
		BaseURL      string `json:"base_url"`      // OAuth base
		ClientID     string `json:"client_id"`     // client id
		CallbackURL  string `json:"callback_url"`  // 回调地址
		CallbackPort int    `json:"callback_port"` // 本地 OAuth 回调监听端口，0 = 不监听
	} `json:"oauth"`

	Features struct {
		AutoRefresh bool `json:"auto_refresh"`
	} `json:"features"`

	Pool struct {
		MaxInFlight       int     `json:"max_in_flight"` // 单账号最大在途请求，0 = 不限
		PlanCooldown      string  `json:"plan_cooldown"` // 权益不足冷却，默认 "12h"
		SoftCooldown      string  `json:"soft_cooldown"` // 429 冷却，默认 "60s"
		ErrThreshold      int     `json:"err_threshold"` // 连续错误阈值，默认 3
		ErrCooldown       string  `json:"err_cooldown"`  // 错误冷却，默认 "10m"
		RefreshSkew       string  `json:"refresh_skew"`  // token 预刷新窗口，默认 "24h"
		DefaultModel      string  `json:"default_model"` // 缺省 glm-5.2
		IdleWeightPerHour float64 `json:"idle_weight_per_hour"`
		IdleWeightMax     float64 `json:"idle_weight_max"`
		MaxRotate         int     `json:"max_rotate"` // 单请求最多换号次数，默认 3
	} `json:"pool"`

	Schedule struct {
		CheckinHours          []int `json:"checkin_hours"`   // [9,21]
		KeepaliveHours        []int `json:"keepalive_hours"` // [22]
		CheckinEnabled        bool  `json:"checkin_enabled"`
		KeepaliveEnabled      bool  `json:"keepalive_enabled"`
		BalanceRefreshEnabled bool  `json:"balance_refresh_enabled"`
		BalanceRefreshMinutes int   `json:"balance_refresh_minutes"` // 默认 30
	} `json:"schedule"`

	Logging struct {
		Level string `json:"level"`
	} `json:"logging"`

	// 解析后
	PlanCooldownDur        time.Duration `json:"-"`
	SoftCooldownDur        time.Duration `json:"-"`
	ErrCooldownDur         time.Duration `json:"-"`
	RefreshSkewDur         time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7862",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Upstream.TimeoutSeconds = 120
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Pool.MaxInFlight = 3
	c.Pool.PlanCooldown = "12h"
	c.Pool.SoftCooldown = "60s"
	c.Pool.ErrThreshold = 3
	c.Pool.ErrCooldown = "10m"
	c.Pool.RefreshSkew = "24h"
	c.Pool.DefaultModel = "glm-5.2"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.MaxRotate = 3
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.CheckinEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 30
	c.Logging.Level = "info"
	return c
}

// normalize 补齐缺省并校验。
func (c *Config) normalize() error {
	var err error
	if c.Listen == "" {
		c.Listen = ":7862"
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.Pool.PlanCooldown == "" {
		c.Pool.PlanCooldown = "12h"
	}
	if c.Pool.SoftCooldown == "" {
		c.Pool.SoftCooldown = "60s"
	}
	if c.Pool.ErrCooldown == "" {
		c.Pool.ErrCooldown = "10m"
	}
	if c.Pool.RefreshSkew == "" {
		c.Pool.RefreshSkew = "24h"
	}
	if c.Pool.DefaultModel == "" {
		c.Pool.DefaultModel = "glm-5.2"
	}
	if c.Pool.ErrThreshold <= 0 {
		c.Pool.ErrThreshold = 3
	}
	if c.Pool.MaxRotate <= 0 {
		c.Pool.MaxRotate = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.PlanCooldownDur, err = time.ParseDuration(c.Pool.PlanCooldown); err != nil {
		return fmt.Errorf("pool.plan_cooldown: %w", err)
	}
	if c.SoftCooldownDur, err = time.ParseDuration(c.Pool.SoftCooldown); err != nil {
		return fmt.Errorf("pool.soft_cooldown: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Pool.ErrCooldown); err != nil {
		return fmt.Errorf("pool.err_cooldown: %w", err)
	}
	if c.RefreshSkewDur, err = time.ParseDuration(c.Pool.RefreshSkew); err != nil {
		return fmt.Errorf("pool.refresh_skew: %w", err)
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	for _, h := range append(append([]int{}, c.Schedule.CheckinHours...), c.Schedule.KeepaliveHours...) {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule hours: %d 不是合法小时（0-23）", h)
		}
	}
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 30
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return nil
}

// Load 读配置文件 + 环境变量覆盖。
//
// path 指向的文件不存在时返回 fs.ErrNotExist 包装的错误（而不是静默回落
// 默认值）——调用方要靠它判断「首次运行」并触发 WriteDefault 生成配置。
// 显式传空 path 才是「不读文件，纯 env + 默认值」。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件", path)
		}
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, c); err != nil {
				return nil, fmt.Errorf("parse config: %w", err)
			}
		case errors.Is(err, fs.ErrNotExist):
			// 首次运行：把错误抛给调用方，由它决定生成默认配置。
			return nil, fmt.Errorf("config %s 不存在: %w", path, err)
		default:
			return nil, fmt.Errorf("read config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// WriteDefault 首次运行生成推荐配置（含随机 api_key）。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize()
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("TRA2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TRA2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("TRA2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("TRA2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("TRA2A_UPSTREAM_BASE_URL"); v != "" {
		c.Upstream.BaseURL = v
	}
	if v := os.Getenv("TRA2A_UPSTREAM_API_KEY"); v != "" {
		c.Upstream.APIKey = v
	}
	if v := os.Getenv("TRA2A_OAUTH_BASE_URL"); v != "" {
		c.OAuth.BaseURL = v
	}
	if v := os.Getenv("TRA2A_CLIENT_ID"); v != "" {
		c.OAuth.ClientID = v
	}
	if v := os.Getenv("TRA2A_CALLBACK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.OAuth.CallbackPort = n
		}
	}
	if v := os.Getenv("TRA2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("TRA2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("TRA2A_DEFAULT_MODEL"); v != "" {
		c.Pool.DefaultModel = v
	}
	if v := os.Getenv("TRA2A_AUTO_REFRESH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.AutoRefresh = b
		}
	}
	if v := os.Getenv("TRA2A_LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
}

// MergeEnv 返回遮蔽后的敏感字段（供日志/面板展示）。
func (c *Config) Redacted() map[string]any {
	mask := func(s string) string {
		if s == "" {
			return ""
		}
		if len(s) <= 8 {
			return "***"
		}
		return s[:4] + "…(" + strconv.Itoa(len(s)) + ")"
	}
	return map[string]any{
		"api_key":          mask(c.APIKey),
		"upstream_api_key": mask(c.Upstream.APIKey),
	}
}

// Normalize 导出 normalize：统一配置装配时，Trae 的配置段由 cmd/server
// 从顶层 config.json 解析出来，需要在这里补默认值并校验。
//
// 为什么导出而不是让调用方走 Load("")：Load 会读文件 + 应用 TRA2A_* env，
// 而统一配置场景下配置已经解析完了，只需要「补默认 + 校验」这一步。
// 导出它保证统一配置与独立进程**走同一套归一逻辑**，不会出现两套默认值。
func (c *Config) Normalize() error { return c.normalize() }

// ApplyEnv 导出环境变量覆盖（TRA2A_*）。
//
// 用途：统一进程启动时，让既有的 TRA2A_* 环境变量仍然生效——
// 部署脚本/容器编排里已经设了这些变量的场景不必改写。
func (c *Config) ApplyEnv() { applyEnv(c) }
