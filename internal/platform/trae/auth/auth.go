// Package auth Trae SOLO 账号凭证：磁盘形态兼容（嵌套 / 扁平）、
// 并发安全读写、原子落盘、目录扫描。
//
// 磁盘形态（两种都认，写回统一用嵌套形）：
//
//	嵌套：{"auth":{"accessToken":..,"refreshToken":..},"account":{"uid":..}}
//	扁平：{"accessToken":..,"uid":..}
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt）。
// 写路径（upstream.RefreshToken）持写锁整段执行 ExchangeToken；
// 读路径持读锁取快照，杜绝与写并发的数据竞争。
type Auth struct {
	mu           sync.RWMutex
	AccessToken  string // Cloud-IDE-JWT 头用
	RefreshToken string // 每次 ExchangeToken 轮换
	ExpiresAt    int64  // Unix 秒
	Domain       string // "trae.cn"
	ApiHost      string // OAuth 端点 host
	MachineID    string // x-machine-id
	DeviceID     string // x-device-id
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 落盘路径

	// 共同管理：同一凭证可被多个面板/网关共享，标记来源便于审计。
	SharedWith []string `json:"shared_with,omitempty"`
	Disabled   bool     `json:"disabled,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// Lock/Unlock 供 upstream 在改写凭证期间加写锁。
func (a *Auth) Lock()   { a.mu.Lock() }
func (a *Auth) Unlock() { a.mu.Unlock() }

// JWT 当前 accessToken 快照。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue 当前 refreshToken 快照。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// NeedsRefresh 报告 token 是否将在 within 内过期或已过期。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.needsRefreshLocked(within)
}

func (a *Auth) needsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// SetTokenLocked 写入换来的新凭证（调用方必须持写锁）。
func (a *Auth) SetTokenLocked(access, refresh string, expiresAt int64) {
	a.AccessToken = access
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if expiresAt > 0 {
		a.ExpiresAt = expiresAt
	}
}

// ExpiresAtUnix 过期时刻快照（0 = 未知）。
func (a *Auth) ExpiresAtUnix() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// parseNested 解析嵌套形。
func parseNested(raw []byte) (*Auth, error) {
	var n struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
			UID          string `json:"uid"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	uid := n.Account.UID
	if uid == "" {
		uid = n.Auth.UID // 某些旧格式 uid 写在 auth.uid，兜底读取，避免丢失
	}
	return &Auth{
		AccessToken:  n.Auth.AccessToken,
		RefreshToken: n.Auth.RefreshToken,
		ExpiresAt:    n.Auth.ExpiresAt,
		Domain:       n.Auth.Domain,
		ApiHost:      n.Auth.ApiHost,
		MachineID:    n.Auth.MachineID,
		DeviceID:     n.Auth.DeviceID,
		UID:          uid,
		EnterpriseID: n.Account.EnterpriseID,
		Nickname:     n.Account.Nickname,
	}, nil
}

// parseFlat 解析扁平形。
func parseFlat(raw []byte) (*Auth, error) {
	var f struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"`
		Domain       string   `json:"domain"`
		ApiHost      string   `json:"apiHost"`
		MachineID    string   `json:"machineId"`
		DeviceID     string   `json:"deviceId"`
		UID          string   `json:"uid"`
		EnterpriseID string   `json:"enterpriseId"`
		Nickname     string   `json:"nickname"`
		SharedWith   []string `json:"shared_with"`
		Note         string   `json:"note"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiresAt:    f.ExpiresAt,
		Domain:       f.Domain,
		ApiHost:      f.ApiHost,
		MachineID:    f.MachineID,
		DeviceID:     f.DeviceID,
		UID:          f.UID,
		EnterpriseID: f.EnterpriseID,
		Nickname:     f.Nickname,
		SharedWith:   f.SharedWith,
		Note:         f.Note,
	}, nil
}

// Parse 兼容两种磁盘形态。缺 accessToken 视为无效。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// 剥离 UTF-8 BOM（部分编辑器/备份会在文件头写入 EF BB BF），否则
	// json.Unmarshal 首字符报错导致整组账号静默跳过、网关无任何账号可用。
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var (
		a   *Auth
		err error
	)
	if _, nested := probe["auth"]; nested {
		a, err = parseNested(raw)
	} else {
		a, err = parseFlat(raw)
	}
	if err != nil {
		return nil, err
	}
	// 附加字段（嵌套形里写在 auth 之外的 shared_with / note）补充读取。
	var extra struct {
		SharedWith []string `json:"shared_with"`
		Note       string   `json:"note"`
		Disabled   bool     `json:"disabled"`
	}
	if jerr := json.Unmarshal(raw, &extra); jerr == nil {
		if len(a.SharedWith) == 0 {
			a.SharedWith = extra.SharedWith
		}
		if a.Note == "" {
			a.Note = extra.Note
		}
		a.Disabled = extra.Disabled
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename，0600）。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
			"uid":          a.UID, // 双写：与 account.uid 同步，防任何读取方单边丢失
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	if len(a.SharedWith) > 0 {
		doc["shared_with"] = a.SharedWith
	}
	if a.Note != "" {
		doc["note"] = a.Note
	}
	if a.Disabled {
		doc["disabled"] = true
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(a.FilePath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// FilePathFor 凭证落盘路径：authDir/trae-<uid>.json。
func FilePathFor(authDir, uid string) string {
	return filepath.Join(authDir, "trae-"+uid+".json")
}

// MaskToken 脱敏：保留前 n 字符 + 长度标注。
func MaskToken(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(" + strconv.Itoa(len(s)) + ")"
}

// LoadDir 扫描 dir 下 trae-*.json；解析失败静默跳过。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := make([]*Auth, 0, len(files))
	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		a, perr := Parse(raw)
		if perr != nil {
			continue
		}
		a.FilePath = f
		// 设备 id 自愈：仅在缺失/非法，或仍为旧「uid 派生」被风控值（历史 9074 元凶）
		// 时重新生成随机 16 位 did；否则保留登录/真机下发的既有 did（不覆盖，避免破坏
		// 已在服务端登记的设备指纹）。
		if !is16Digit(a.DeviceID) || a.DeviceID == deriveLegacyDeviceID(a.UID) {
			if _, eerr := a.ResetDeviceID(); eerr != nil {
				log.Printf("[auth] %s 分配 deviceId 失败: %v", f, eerr)
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// NewHexID 生成 n 字节随机十六进制串（hex 长度 2n）。
func NewHexID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewCheckinDeviceID 生成签到兼容的 device id：16 位纯数字。
//
// 上游签到对 device id 有格式约定（与真实客户端注册的一致），非数字形态可能
// 被判为异常设备；这里用 crypto/rand 取均匀整数，保证与其他账号互异。
func NewCheckinDeviceID() (string, error) {
	var sb strings.Builder
	for i := 0; i < 16; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		sb.WriteString(n.String())
	}
	return sb.String(), nil
}

// EnsureCheckinDeviceID 保证账号持有「持久化」的签到设备 id（16 位纯数字）。
//
// 实测结论（2026-10-12 直连 api.trae.cn 单号矩阵验证，推翻旧「uid 派生」结论）：
//   - 签到成功的关键不是 did 是否「按 uid 互不相同」，而是 did 必须是【非确定性
//     的随机 16 位数字】。用「由 uid 确定性派生的固定值」作为 did，服务端会判定为
//     批量伪造设备 → 统一返回 9074「当前参与用户太多」；而任意随机 16 位数字 did
//     一律 code:0 签到成功。
//   - 服务端按 (token, deviceId) 组合去重：某 did 被该账号占用后返回 9095「今日已签」，
//     换一个未占用的 did 即可再签（故 did 不宜长期固定，否则有被风控标记的风险）。
//   - 因此策略改为：尊重并保留登录/真机下发的既有 did；仅在缺失或不是 16 位纯数字
//     时才用 crypto/rand 生成一个随机 did（不再由 uid 派生），并原子落盘。
//
// 返回最终使用的 deviceId。
func (a *Auth) EnsureCheckinDeviceID() (string, error) {
	a.mu.Lock()
	// 已持有合法 16 位纯数字 did：直接沿用，不覆盖（避免破坏真机/随机设备指纹）。
	if is16Digit(a.DeviceID) {
		dev := a.DeviceID
		a.mu.Unlock()
		return dev, nil
	}
	return a.resetDeviceIDLocked()
}

// resetDeviceIDLocked 在持锁状态下强制重新生成随机 16 位 did 并原子落盘。
// 调用方须已持 a.mu（或单线程上下文）。用于 LoadDir 自愈被风控的旧派生 did，
// 以及 did 缺失/非法时的补齐。
// 注意：SaveAtomic 内部会再次加写锁，故本函数在调用 SaveAtomic 前先解锁，
// 且调用后【不再重新加锁】即返回（调用方 ResetDeviceID 拿到的是已解锁状态）。
func (a *Auth) resetDeviceIDLocked() (string, error) {
	dev, err := NewCheckinDeviceID()
	if err != nil {
		a.mu.Unlock()
		return "", err
	}
	a.DeviceID = dev
	a.mu.Unlock()
	if a.FilePath != "" {
		if serr := a.SaveAtomic(); serr != nil {
			return dev, serr
		}
	}
	return dev, nil
}

// ResetDeviceID 强制重新生成随机 16 位 did（用于被风控标记的设备指纹自愈）。
func (a *Auth) ResetDeviceID() (string, error) {
	a.mu.Lock()
	return a.resetDeviceIDLocked()
}

// is16Digit 判 s 是否为 16 位纯数字串。
func is16Digit(s string) bool {
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

// deriveLegacyDeviceID 复刻旧版「由 uid 确定性派生」算法产出的 did，仅用于在
// LoadDir 中识别并治愈被风控标记的历史值（该固定派生值已被上游判为批量伪造设备 →
// 9074）。新代码不再使用此算法生成 did，仅作迁移期识别。
func deriveLegacyDeviceID(uid string) string {
	sum := sha256.Sum256([]byte("traeapi2-device-" + uid))
	mod := new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil)
	rem := new(big.Int).Mod(new(big.Int).SetBytes(sum[:16]), mod)
	return fmt.Sprintf("%016d", rem)
}

// EnsureMachineID 保证账号持有「独立、持久化」的设备指纹 machineId。
//
// 实测结论：Trae 签到按 (token, deviceId) 组合去重（见 EnsureCheckinDeviceID），
// machineId 已不参与签到鉴权；本函数保留仅为兼容 chat 等路径的设备指纹维度。
// 派生值由 uid 决定，确定性、跨重启稳定、按号互异。
func (a *Auth) EnsureMachineID() (string, error) {
	a.mu.Lock()
	// 由 uid 派生 32-hex machineId：确定性、跨重启稳定、按号互异。
	sum := sha256.Sum256([]byte("traeapi2-machine-" + a.UID))
	mid := hex.EncodeToString(sum[:16]) // 32 hex 字符，与真实客户端 machineId 形态一致
	if a.MachineID == mid {
		a.mu.Unlock()
		return mid, nil
	}
	a.MachineID = mid
	a.mu.Unlock()
	if a.FilePath != "" {
		if err := a.SaveAtomic(); err != nil {
			return mid, err
		}
	}
	return mid, nil
}
