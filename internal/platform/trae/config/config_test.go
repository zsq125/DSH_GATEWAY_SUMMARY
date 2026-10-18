package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsApplied(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载默认配置失败: %v", err)
	}
	if c.Listen != ":7862" {
		t.Errorf("默认监听应为 :7862，实为 %q", c.Listen)
	}
	if c.Pool.DefaultModel != "glm-5.2" {
		t.Errorf("默认模型不对: %q", c.Pool.DefaultModel)
	}
	if c.PlanCooldownDur != 12*time.Hour {
		t.Errorf("权益冷却默认应为 12h，实为 %v", c.PlanCooldownDur)
	}
	if c.SoftCooldownDur != 60*time.Second {
		t.Errorf("限流冷却默认应为 60s，实为 %v", c.SoftCooldownDur)
	}
	if c.ErrCooldownDur != 10*time.Minute {
		t.Errorf("错误冷却默认应为 10m，实为 %v", c.ErrCooldownDur)
	}
	if c.RefreshSkewDur != 24*time.Hour {
		t.Errorf("预刷新窗口默认应为 24h，实为 %v", c.RefreshSkewDur)
	}
	// 排程缺省
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("签到排程缺省不对: %v", c.Schedule.CheckinHours)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Error("开关缺省应为启用")
	}
	// header/idle 超时回落
	if c.Upstream.HeaderTimeoutSeconds != c.Upstream.TimeoutSeconds {
		t.Errorf("header 超时应回落 timeout: %d", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle 超时默认应为 300，实为 %d", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestWriteDefaultThenLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	key, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if key == "" {
		t.Error("应生成随机 api_key")
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if c.APIKey != key {
		t.Errorf("api_key 不一致: %q ≠ %q", c.APIKey, key)
	}
	// 已存在时 O_EXCL 拒绝覆盖，绝不悄悄改写用户配置
	if _, err := WriteDefault(path); err == nil {
		t.Error("重复生成应被拒绝")
	}
}

// 非法小时必须报错（静默钳制会让用户以为关掉了签到，实际照常跑）。
// 首次运行（文件不存在）必须把 fs.ErrNotExist 抛给调用方——
// app.Run 靠它决定「生成默认配置」。曾经因为这里吞掉错误并静默回落默认值，
// 导致首次启动永远不生成 config.json（用户以为功能坏了，实际是没写盘）。
func TestLoadMissingFileReportsNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.json")
	_, err := Load(missing)
	if err == nil {
		t.Fatal("文件不存在时 Load 应报错")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("应可用 errors.Is 判定为 ErrNotExist，实际: %v", err)
	}
	// 空 path 才是「不读文件，纯默认值 + env」
	if _, err := Load(""); err != nil {
		t.Errorf("空 path 不该报错: %v", err)
	}
}

// 目录而非文件 → 明确报错（否则 os.ReadFile 会给一个含糊的 EISDIR）。
func TestLoadDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Error("传入目录应报错")
	}
}

func TestRejectsInvalidHours(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"schedule":{"checkin_hours":[25]}}`), 0o600)
	if _, err := Load(bad); err == nil {
		t.Error("非法小时应报错")
	}
	// 合法小时照常通过
	good := filepath.Join(dir, "good.json")
	_ = os.WriteFile(good, []byte(`{"schedule":{"checkin_hours":[0,23]}}`), 0o600)
	if _, err := Load(good); err != nil {
		t.Errorf("合法小时不该报错: %v", err)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("TRA2A_LISTEN", ":9999")
	t.Setenv("TRA2A_API_KEY", "env-key")
	t.Setenv("TRA2A_DEFAULT_MODEL", "kimi-k3")
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if c.Listen != ":9999" {
		t.Errorf("LISTEN 未覆盖: %q", c.Listen)
	}
	if c.APIKey != "env-key" {
		t.Errorf("API_KEY 未覆盖: %q", c.APIKey)
	}
	if c.Pool.DefaultModel != "kimi-k3" {
		t.Errorf("DEFAULT_MODEL 未覆盖: %q", c.Pool.DefaultModel)
	}
}

// 裸端口号应自动补冒号（":7862" 与 "7862" 都可用）。
func TestListenNormalized(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	c.Listen = "7862"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize 失败: %v", err)
	}
	if c.Listen != ":7862" {
		t.Errorf("监听地址未归一: %q", c.Listen)
	}
}

// 面板展示用的脱敏：密钥不能整串吐出去。
func TestRedacted(t *testing.T) {
	c, _ := Load("")
	c.APIKey = "sk-abcdefghijklmnop"
	c.Upstream.APIKey = "upstream-secret-key"
	m := c.Redacted()
	if s, _ := m["api_key"].(string); s == c.APIKey || s == "" {
		t.Errorf("api_key 未脱敏: %q", s)
	}
	if s, _ := m["upstream_api_key"].(string); s == c.Upstream.APIKey {
		t.Errorf("upstream_api_key 未脱敏: %q", s)
	}
}
