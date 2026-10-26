// 保活自动化配置解析的回归测试（2026-10-23）。
//
// 核心保护：配置层的「空值」必须解析成**具体默认值**，不能是 0。
// 0 在调度器层是「禁用」，若空值解析成 0，间隔循环会被静默关掉
// （实测踩过：config 里没写 keepalive_interval → 循环根本没启动）。
package main

import (
	"testing"
	"time"
)

// TestKeepaliveConfigDefaultsWhenEmpty 空配置必须得到具体默认值，不能是 0。
func TestKeepaliveConfigDefaultsWhenEmpty(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.KeepaliveIntervalDur != 4*time.Hour {
		t.Errorf("空配置的 KeepaliveIntervalDur 应为 4h，实为 %v（0 会导致间隔循环被静默关闭）",
			c.KeepaliveIntervalDur)
	}
	if c.KeepaliveVerifyWithinDur != 7*24*time.Hour {
		t.Errorf("空配置的 KeepaliveVerifyWithinDur 应为 168h，实为 %v", c.KeepaliveVerifyWithinDur)
	}
}

// TestKeepaliveConfigExplicitZeroDisables 显式 "0" 才是禁用。
func TestKeepaliveConfigExplicitZeroDisables(t *testing.T) {
	c := Default()
	c.Schedule.KeepaliveInterval = "0"
	c.Schedule.KeepaliveVerifyWithin = "0"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.KeepaliveIntervalDur != 0 {
		t.Errorf("显式 0 应解析为 0（禁用），实为 %v", c.KeepaliveIntervalDur)
	}
	if c.KeepaliveVerifyWithinDur != 0 {
		t.Errorf("显式 0 应解析为 0，实为 %v", c.KeepaliveVerifyWithinDur)
	}
}

// TestKeepaliveConfigParsesDurations 合法 Duration 字符串应被正确解析。
func TestKeepaliveConfigParsesDurations(t *testing.T) {
	c := Default()
	c.Schedule.KeepaliveInterval = "2h"
	c.Schedule.KeepaliveVerifyWithin = "72h"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.KeepaliveIntervalDur != 2*time.Hour {
		t.Errorf("keepalive_interval=2h 解析为 %v", c.KeepaliveIntervalDur)
	}
	if c.KeepaliveVerifyWithinDur != 72*time.Hour {
		t.Errorf("keepalive_verify_within=72h 解析为 %v", c.KeepaliveVerifyWithinDur)
	}
}

// TestKeepaliveConfigRetryParsing 重试间隔数组应正确解析为 Duration。
func TestKeepaliveConfigRetryParsing(t *testing.T) {
	c := Default()
	c.Schedule.KeepaliveRetry = []int{10, 60}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.KeepaliveRetryDurs) != 2 {
		t.Fatalf("应有 2 档重试，实为 %d", len(c.KeepaliveRetryDurs))
	}
	if c.KeepaliveRetryDurs[0] != 10*time.Second || c.KeepaliveRetryDurs[1] != 60*time.Second {
		t.Errorf("重试间隔解析错误: %v", c.KeepaliveRetryDurs)
	}
}

// TestKeepaliveConfigInvalidDurationFailsFast 非法 Duration 必须报错（拼写错误不能静默）。
func TestKeepaliveConfigInvalidDurationFailsFast(t *testing.T) {
	c := Default()
	c.Schedule.KeepaliveInterval = "4hours" // 非法
	if err := c.normalize(); err == nil {
		t.Errorf("非法 keepalive_interval 应报错，实际通过了")
	}
}
