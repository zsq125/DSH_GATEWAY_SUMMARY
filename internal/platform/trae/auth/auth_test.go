package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 嵌套形与扁平形都要能读（前者是登录脚本产出，后者是手建）。
func TestParseNestedAndFlat(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"AT","refreshToken":"RT","expiresAt":123,
		"domain":"trae.cn","apiHost":"https://api.trae.com.cn","machineId":"M","deviceId":"D"},
		"account":{"uid":"u1","nickname":"昵称"}}`)
	a, err := Parse(nested)
	if err != nil {
		t.Fatalf("嵌套形解析失败: %v", err)
	}
	if a.AccessToken != "AT" || a.RefreshToken != "RT" || a.UID != "u1" {
		t.Errorf("嵌套形字段不对: %+v", a)
	}
	if a.Nickname != "昵称" || a.MachineID != "M" || a.DeviceID != "D" {
		t.Errorf("嵌套形附加字段不对: %+v", a)
	}

	flat := []byte(`{"accessToken":"AT2","uid":"u2"}`)
	b, err := Parse(flat)
	if err != nil {
		t.Fatalf("扁平形解析失败: %v", err)
	}
	if b.AccessToken != "AT2" || b.UID != "u2" {
		t.Errorf("扁平形字段不对: %+v", b)
	}
}

func TestParseRejectsMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u"}`)); err == nil {
		t.Error("缺 accessToken 应报错")
	}
	if _, err := Parse(nil); err == nil {
		t.Error("空内容应报错")
	}
	if _, err := Parse([]byte(`{`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// 共同管理标记要能被读出（面板要展示同一凭证被几个网关共享）。
func TestParseSharedWith(t *testing.T) {
	raw := []byte(`{"accessToken":"AT","uid":"u1","shared_with":["gw-a","gw-b"],"note":"备注"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(a.SharedWith) != 2 {
		t.Errorf("shared_with 未读出: %#v", a.SharedWith)
	}
	if a.Note != "备注" {
		t.Errorf("note 未读出: %q", a.Note)
	}
}

// ExpiresAtUnix 是面板「凭证到期」列的数据源：0 = 未知（不是过期）。
func TestExpiresAtUnix(t *testing.T) {
	a := &Auth{}
	if got := a.ExpiresAtUnix(); got != 0 {
		t.Errorf("未设过期时间应为 0: %d", got)
	}
	a.Lock()
	a.SetTokenLocked("AT", "RT", 1786847930)
	a.Unlock()
	if got := a.ExpiresAtUnix(); got != 1786847930 {
		t.Errorf("过期时刻不对: %d", got)
	}
	// refreshToken 为空时不覆盖它（凭据没有轮换能力就别写空值）
	a2 := &Auth{RefreshToken: "keep"}
	a2.Lock()
	a2.SetTokenLocked("AT", "", 1)
	a2.Unlock()
	if a2.RefreshTokenValue() != "keep" {
		t.Errorf("空 refresh 不该覆盖旧值: %q", a2.RefreshTokenValue())
	}
	if a2.ExpiresAtUnix() != 1 {
		t.Errorf("过期时间仍应更新: %d", a2.ExpiresAtUnix())
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{}
	// 无过期信息 → 一律需要刷新（老凭证文件缺 expiresAt）
	if !a.NeedsRefresh(time.Hour) {
		t.Error("无 expiresAt 应判为需要刷新")
	}
	a.ExpiresAt = time.Now().Add(3 * time.Hour).Unix()
	if a.NeedsRefresh(time.Hour) {
		t.Error("3 小时后过期、skew 1 小时 → 不该刷新")
	}
	if !a.NeedsRefresh(5 * time.Hour) {
		t.Error("skew 5 小时 → 应刷新")
	}
	a.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if !a.NeedsRefresh(0) {
		t.Error("已过期 → 应刷新")
	}
}

// 并发安全：读写 token 必须走锁，-race 下不能报数据竞争。
func TestConcurrentTokenAccess(t *testing.T) {
	a := &Auth{AccessToken: "old", RefreshToken: "rt"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.JWT()
			_ = a.RefreshTokenValue()
			_ = a.NeedsRefresh(time.Hour)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Lock()
		a.SetTokenLocked("new", "rt2", time.Now().Unix())
		a.Unlock()
	}()
	wg.Wait()
	if got := a.JWT(); got != "new" {
		t.Errorf("写入未生效: %q", got)
	}
}

func TestSaveAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{
		AccessToken: "AT", RefreshToken: "RT", ExpiresAt: 123,
		Domain: "trae.cn", ApiHost: "https://api.trae.com.cn",
		MachineID: "M", DeviceID: "D",
		UID: "u1", Nickname: "昵称", SharedWith: []string{"gw-a"},
		FilePath: filepath.Join(dir, "trae-u1.json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	// 权限必须是 0600（凭证文件）。Windows 没有 POSIX 权限位语义
	//（Go 的 0600 在 Windows 上不生效），只在 Unix 系断言。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(a.FilePath)
		if err != nil {
			t.Fatalf("读回失败: %v", err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("权限应为 0600，实为 %v", st.Mode().Perm())
		}
	}
	back, err := Parse(mustRead(t, a.FilePath))
	if err != nil {
		t.Fatalf("回读解析失败: %v", err)
	}
	if back.AccessToken != "AT" || back.UID != "u1" || back.Nickname != "昵称" {
		t.Errorf("往返不一致: %+v", back)
	}
	if len(back.SharedWith) != 1 || back.SharedWith[0] != "gw-a" {
		t.Errorf("shared_with 往返丢失: %#v", back.SharedWith)
	}
	// 写回格式必须是嵌套形（登录脚本可读）
	var probe map[string]json.RawMessage
	raw := mustRead(t, a.FilePath)
	_ = json.Unmarshal(raw, &probe)
	if _, has := probe["auth"]; !has {
		t.Error("落盘应为嵌套形（含 auth 段）")
	}
}

func TestFilePathFor(t *testing.T) {
	got := FilePathFor("/x/auths", "u1")
	if !strings.HasSuffix(got, filepath.Join("auths", "trae-u1.json")) {
		t.Errorf("路径不对: %q", got)
	}
}

// LoadDir 只扫 trae-*.json，且解析失败的文件静默跳过（不该让整个池加载失败）。
func TestLoadDirSkipsBadFiles(t *testing.T) {
	dir := t.TempDir()
	good := &Auth{AccessToken: "AT", UID: "u1"}
	good.FilePath = filepath.Join(dir, "trae-u1.json")
	_ = good.SaveAtomic()
	// 坏文件 + 非目标前缀的文件
	_ = os.WriteFile(filepath.Join(dir, "trae-broken.json"), []byte(`{"uid":"x"}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"accessToken":"A","uid":"u9"}`), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir 失败: %v", err)
	}
	if len(list) != 1 || list[0].UID != "u1" {
		t.Errorf("应只加载 1 个有效账号，实为 %d", len(list))
	}
}

func TestMaskToken(t *testing.T) {
	if got := MaskToken("abcdef", 3); !strings.HasPrefix(got, "abc") || !strings.Contains(got, "6") {
		t.Errorf("脱敏格式不对: %q", got)
	}
	if got := MaskToken("ab", 10); got != "ab" {
		t.Errorf("短串不该被截断: %q", got)
	}
}

// device id 必须是 16 位纯数字：上游签到按此格式识别设备，非数字形态会被判异常。
func TestNewCheckinDeviceID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id, err := NewCheckinDeviceID()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if len(id) != 16 {
			t.Errorf("长度应为 16，实为 %d (%q)", len(id), id)
		}
		for _, c := range id {
			if c < '0' || c > '9' {
				t.Errorf("应全为数字: %q", id)
				break
			}
		}
		seen[id] = true
	}
	if len(seen) < 15 {
		t.Errorf("随机性不足：20 次只生成 %d 个不同的 id", len(seen))
	}
}

func TestNewHexID(t *testing.T) {
	id, err := NewHexID(16)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("16 字节应为 32 hex 字符，实为 %d", len(id))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return raw
}
