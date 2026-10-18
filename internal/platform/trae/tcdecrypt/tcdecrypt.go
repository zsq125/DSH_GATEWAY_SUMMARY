// tcdecrypt.go Trae「tc」加密格式解密 —— 读取本机官方客户端已登录的凭证。
//
// ── 为什么需要这个 ──────────────────────────────────────────────────────
//
// Trae 上游 2026-10 把登录协议从「回调直给 refreshToken」改成了
// 「OAuth2 授权码 + PKCE 交换」（详见 server/login.go 的 auth_from 注释）。
// 官方客户端的登录 URL 带 code_challenge，我们的旧实现不带 → 静默授权
// 走不通（浏览器已登录状态下点「添加账号」报「登录失败/网络错误」）。
//
// 而官方客户端**自己**登录是好的，凭证就落在本机
//   %APPDATA%\TRAE SOLO CN\User\globalStorage\storage.json
// 的 "iCubeAuthInfo://icube.cloudide" 键里（加密）。
//
// 本文件提供一条**不依赖登录协议**的替代路径：直接解密客户端凭证并导入。
// 这样即使上游再改登录协议，只要客户端能登，我们就能拿到凭证。
//
// ── 算法来源 ────────────────────────────────────────────────────────────
//
// 复刻自开源项目 laojichao/trae-local-api 的 src/trae-decrypt.js
//（该项目的 README 明确写着 "auto-decrypt tc encrypted auth"）。
// 算法本身来自 Trae 前端 JS 里硬编码的 salt 数组，不是猜测。
//
// 数据布局：[6B Header][32B RandomBytes][N AES-128-CBC 密文]
// 解密结果：[64B SHA-512 校验][N 明文 JSON]
//
// 密钥派生：
//
//	salt  = SALT_A XOR SALT_B            （64 字节，硬编码）
//	hash1 = SHA512(randomBytes)
//	hash2 = SHA512(hash1 || salt)
//	key   = hash2[0:16]   iv = hash2[16:32]
//
// 明文末尾有 SHA-512 校验，故解密**可自证**：校验不过就报错，
// 不会把错误密钥解出的乱码当成凭证用。
//
// ── 为什么用 Go 而不是继续用 Node 脚本 ─────────────────────────────────
//
// 只用 crypto/sha512 + crypto/aes + encoding/base64 三个标准库包，
// 零外部依赖（不需要 Node/Python/第三方模块），可以编译进单文件 exe。
package tcdecrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ── 硬编码 salt（4 组，各 64 字节）──────────────────────────────────────
//
// 来源：Trae CN 前端 JS。两组按加密类型组合：
//
//	AES（tc 前缀）    → SALT_A XOR SALT_B
//	AES_PRIVATE       → SALT_C XOR SALT_D
//
// 不要改动这些值 —— 它们必须与客户端一致，改一个字节就全解不开。
var saltA = [64]byte{
	82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251,
	124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203,
	84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78,
	8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37,
}

var saltB = [64]byte{
	31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95,
	96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239,
	160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97,
	23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125,
}

var saltC = [64]byte{
	191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176,
	135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156,
	51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6,
	18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209,
}

var saltD = [64]byte{
	246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145,
	50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106,
	175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154,
	86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170,
}

// encType 加密类型（由 6 字节头部判定）。
type encType int

const (
	encUnknown encType = iota
	// encAES 头部 "tc" 0x74 0x63 0x05 0x10 0x00 0x00。
	encAES
	// encAESPrivate 头部 0x12 0x39 0x20 0x20 0x02 0x03。
	encAESPrivate
)

// detectEncType 按头部字节判定加密类型。
func detectEncType(h []byte) encType {
	if len(h) >= 6 {
		if h[0] == 0x74 && h[1] == 0x63 && h[2] == 0x05 &&
			h[3] == 0x10 && h[4] == 0x00 && h[5] == 0x00 {
			return encAES
		}
		if h[0] == 18 && h[1] == 57 && h[2] == 32 &&
			h[3] == 32 && h[4] == 2 && h[5] == 3 {
			return encAESPrivate
		}
	}
	return encUnknown
}

// DecryptValue 解密单个 base64 编码的 tc 密文，返回明文。
//
// 明文末尾的 SHA-512 校验会一并验证：校验不过返回错误，
// 不会把错误密钥解出的乱码当凭证用（这是算法自带的完整性保障）。
func DecryptValue(base64Value string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(base64Value))
	if err != nil {
		return nil, fmt.Errorf("base64 解码失败: %w", err)
	}
	// 最小长度：6B 头 + 32B 随机数 + 至少一个 AES 块(16B) + 64B 校验
	const minLen = 6 + 32 + 16 + 64
	if len(raw) < minLen {
		return nil, fmt.Errorf("密文过短（%d 字节，至少需 %d）", len(raw), minLen)
	}

	header := raw[0:6]
	randomBytes := raw[6:38]
	encrypted := raw[38:]

	et := detectEncType(header)
	if et == encUnknown {
		return nil, fmt.Errorf("未知的加密类型（头部 % x）", header)
	}

	key, iv := deriveKeyAndIV(randomBytes, et)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造 AES: %w", err)
	}
	if len(encrypted) == 0 || len(encrypted)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("密文长度不是 AES 块大小的正整数倍（%d）", len(encrypted))
	}
	plain := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, encrypted)

	// 去掉 PKCS#7 填充。
	//
	// 这一步是**必须的**，且很容易漏：Node 的 crypto.createDecipheriv 默认
	// 自动去 padding，而 Go 的 cipher.CBCDecrypter.CryptBlocks 只做裸解密、
	// **不去填充**。漏掉它会导致：
	//   - plain 比真实明文多出 N 个填充字节（实测多 6 字节）
	//   - 末尾的 SHA-512 校验必然失败 → 报「密钥不匹配」，让人误以为算法错了
	// 实测踩到过：密钥、IV、AES 解密结果全部正确，只有校验失败，
	// 排查方向一度被带偏到 salt/派生上。
	plain, err = unpadPKCS7(plain, aes.BlockSize)
	if err != nil {
		return nil, err
	}

	if len(plain) < 64 {
		return nil, fmt.Errorf("解密结果过短（%d 字节，缺少校验段）", len(plain))
	}
	storedHash := plain[0:64]
	body := plain[64:]

	sum := sha512.Sum512(body)
	if !equalBytes(storedHash, sum[:]) {
		// 校验失败说明密钥不对或数据损坏 —— 明确报错而不是返回乱码。
		return nil, fmt.Errorf("SHA-512 校验失败（密钥不匹配或数据损坏）")
	}
	return body, nil
}

// unpadPKCS7 去掉 PKCS#7 填充（AES 分组密码的标准填充方式）。
//
// 校验规则（RFC 5652 §6.3）：
//   - 最后一个字节 n 就是填充长度，取值 1..blockSize
//   - 末尾必须有连续 n 个字节都等于 n
//
// 严格校验而不是「只看最后一字节就截断」：填充损坏时应当明确报错，
// 而不是把一个长度恰好合法的错误值当成功（那会静默解出乱码）。
func unpadPKCS7(b []byte, blockSize int) ([]byte, error) {
	n := len(b)
	if n == 0 {
		return nil, fmt.Errorf("解密结果为空")
	}
	pad := int(b[n-1])
	if pad == 0 || pad > blockSize || pad > n {
		return nil, fmt.Errorf("PKCS#7 填充非法（pad=%d, len=%d）", pad, n)
	}
	for i := n - pad; i < n; i++ {
		if int(b[i]) != pad {
			return nil, fmt.Errorf("PKCS#7 填充字节不一致（位置 %d 应为 %d）", i, pad)
		}
	}
	return b[:n-pad], nil
}

// deriveKeyAndIV 按算法派生 AES-128 的 key 与 iv。
func deriveKeyAndIV(randomBytes []byte, et encType) (key, iv []byte) {
	salt := xorSalts(saltA, saltB)
	if et == encAESPrivate {
		salt = xorSalts(saltC, saltD)
	}
	h1 := sha512.Sum512(randomBytes) // SHA512(randomBytes)

	combined := make([]byte, 0, len(h1)+len(salt))
	combined = append(combined, h1[:]...)
	combined = append(combined, salt...)
	h2 := sha512.Sum512(combined) // SHA512(h1 || salt)

	return h2[0:16], h2[16:32]
}

// xorSalts 两组 salt 逐字节异或。
func xorSalts(a, b [64]byte) []byte {
	out := make([]byte, 64)
	for i := 0; i < 64; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	// 固定时间比较在这里不是必需的（比的是本地文件的校验值，
	// 不是攻击者可探测的密钥），但常量时间比较零成本，顺手用上。
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ── 客户端凭证读取 ──────────────────────────────────────────────────────

// ClientAuth 官方客户端 storage.json 里解出的凭证（字段名对齐客户端）。
//
// 只保留导入所需的字段；客户端 JSON 里还有很多展示用字段（头像、地区等），
// 那些对本程序无意义，刻意不映射（避免"以为要用其实没用"的误读）。
type ClientAuth struct {
	Token          string `json:"token"`
	RefreshToken   string `json:"refreshToken"`
	ExpiredAt      string `json:"expiredAt"`
	UserID         string `json:"userId"`
	Host           string `json:"host"`
	TokenReleaseAt string `json:"tokenReleaseAt"`
	Account        struct {
		Username    string `json:"username"`
		UserTag     string `json:"userTag"`
		StoreRegion string `json:"storeRegion"`
		LoginScope  string `json:"loginScope"`
	} `json:"account"`
}

// StorageKey 客户端存放凭证的键名。
const StorageKey = "iCubeAuthInfo://icube.cloudide"

// ClientDataDirs 官方客户端 userData 目录的候选列表（按优先级）。
//
// 为什么列多个：Trae 有多个产品线（CN / SOLO CN / SOLO 国际版 / SG），
// 目录名各不相同；用户可能装了不止一个。按顺序找，取第一个有凭证的。
//
// 路径规则（Windows）：%APPDATA%\<目录名>\User\globalStorage\storage.json
var ClientDataDirs = []struct {
	Dir  string // %APPDATA% 下的目录名
	Note string // 展示名（面板/日志用）
}{
	{"TRAE SOLO CN", "TRAE SOLO 国内版"},
	{"Trae CN", "Trae 国内版"},
	{"TRAE SOLO", "TRAE SOLO 国际版"},
	{"Trae", "Trae 国际版"},
}

// FindResult 一次查找的结果。
type FindResult struct {
	// Found 是否找到可用凭证。
	Found bool
	// ClientName 命中的客户端展示名（Found=true 时有值）。
	ClientName string
	// StoragePath 命中的 storage.json 路径。
	StoragePath string
	// Auth 解出的凭证（Found=true 时有值）。
	Auth *ClientAuth
	// Tried 尝试过的目录（含失败原因），供面板展示"为什么没找到"。
	Tried []Attempt
}

// Attempt 单个目录的尝试结果。
type Attempt struct {
	Dir    string
	Note   string
	Path   string
	Reason string // 未命中原因（空 = 命中）
}

// FindFromClient 扫描本机官方客户端，返回第一个可解出凭证的。
//
// appDataRoot 传空则用 %APPDATA%（Windows）；测试可传临时目录。
func FindFromClient(appDataRoot string) FindResult {
	var res FindResult
	if appDataRoot == "" {
		appDataRoot = os.Getenv("APPDATA")
	}
	if appDataRoot == "" {
		res.Tried = append(res.Tried, Attempt{Reason: "环境变量 APPDATA 为空，无法定位客户端目录"})
		return res
	}

	for _, c := range ClientDataDirs {
		p := filepath.Join(appDataRoot, c.Dir, "User", "globalStorage", "storage.json")
		att := Attempt{Dir: c.Dir, Note: c.Note, Path: p}

		raw, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				att.Reason = "未安装（storage.json 不存在）"
			} else {
				att.Reason = "读取失败: " + err.Error()
			}
			res.Tried = append(res.Tried, att)
			continue
		}

		auth, err := ParseStorage(raw)
		if err != nil {
			att.Reason = "解密失败: " + err.Error()
			res.Tried = append(res.Tried, att)
			continue
		}

		att.Reason = ""
		res.Tried = append(res.Tried, att)
		res.Found = true
		res.ClientName = c.Note
		res.StoragePath = p
		res.Auth = auth
		return res
	}
	return res
}

// ParseStorage 从 storage.json 原始字节解出凭证。
//
// 兼容两种形态：
//   - 明文 JSON（国际版/SG 直接存明文）→ 直接解析
//   - tc 加密（国内版）→ 先解密再解析
func ParseStorage(raw []byte) (*ClientAuth, error) {
	// 剥 BOM（编辑器可能写入）
	raw = stripBOM(raw)

	var storage map[string]json.RawMessage
	if err := json.Unmarshal(raw, &storage); err != nil {
		return nil, fmt.Errorf("storage.json 不是合法 JSON: %w", err)
	}
	enc, ok := storage[StorageKey]
	if !ok {
		return nil, fmt.Errorf("storage.json 里没有 %q 键（该客户端未登录或版本不符）", StorageKey)
	}

	// json.RawMessage 对字符串会带引号；先解成 string
	var s string
	if err := json.Unmarshal(enc, &s); err != nil {
		return nil, fmt.Errorf("凭证字段不是字符串: %w", err)
	}
	s = strings.TrimSpace(s)

	// 明文形态（以 { 开头）→ 直接解析
	if strings.HasPrefix(s, "{") {
		var a ClientAuth
		if err := json.Unmarshal([]byte(s), &a); err != nil {
			return nil, fmt.Errorf("明文凭证解析失败: %w", err)
		}
		return validateAuth(&a)
	}

	// 加密形态 → 解密
	plain, err := DecryptValue(s)
	if err != nil {
		return nil, err
	}
	var a ClientAuth
	if err := json.Unmarshal(plain, &a); err != nil {
		return nil, fmt.Errorf("解密后的凭证解析失败: %w", err)
	}
	return validateAuth(&a)
}

// validateAuth 校验必需字段（缺 refreshToken 的凭证无法续期，视为无效）。
func validateAuth(a *ClientAuth) (*ClientAuth, error) {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return nil, fmt.Errorf("凭证缺少 refreshToken（无法续期）")
	}
	if strings.TrimSpace(a.UserID) == "" {
		return nil, fmt.Errorf("凭证缺少 userId")
	}
	return a, nil
}

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}
