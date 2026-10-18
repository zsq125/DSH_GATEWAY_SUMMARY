// capabilities.go 平台的**可选能力**接口。
//
// 为什么用「可选接口」而不是把一切都塞进 Platform 主接口：
// 各平台的能力集合本来就不一样——Trae 有浏览器 OAuth 登录闭环，WorkBuddy 没有；
// WorkBuddy 有成长任务队列，Trae 没有。若把这些都做成主接口方法，每个平台
// 都得实现一堆自己用不上的空方法（`return nil`），新平台作者也看不出
// 「到底哪些是必须的」。
//
// 做法：主接口只保留所有平台**都有**的四件事（身份/路由/账号/动作）；
// 额外能力按能力单独定义接口，平台按需实现。面板启动时用类型断言探测，
// 探测到就挂对应路由，探测不到就返回 501 + 明确提示。
//
// 扩展新平台时：实现 Platform + 需要的能力接口，注册一行即可，
// 面板与聚合逻辑零改动。
package platform

import "net/http"

// ── 必需能力的补充 ───────────────────────────────────────────────────────

// StatusProvider 平台自身健康/计数快照（面板导航与概览用）。
//
// 返回 map 的约定键：total / healthy / cooling / disabled（缺省即可），
// 外加平台自有字段。刻意用 map 而非结构体：各平台计数维度不同
// （WorkBuddy 有 in_flight_full / realm 分档，Trae 有 soft_cooling），
// 定死结构体会逼出大量零值字段。
type StatusProvider interface {
	Status() map[string]any
}

// ── 可选能力 ─────────────────────────────────────────────────────────────

// AccountActioner 单账号动作（签到/刷新额度/禁用/解冻/移除/改备注…）。
//
// 走这一个方法而不是给每个动作定义方法：动作集合是可枚举的有限集，
// 面板用同一个路由 /panel/api/platforms/{kind}/accounts/{uid}/{action}
// 覆盖全部，避免为 6 个动作写 6 套几乎一样的 handler。
//
// 返回 (httpStatus, body, error)：status 让平台自己决定语义
// （404 账号不存在 / 502 上游失败 / 200 成功），面板原样透出。
type AccountActioner interface {
	AccountAction(uid, action string, body []byte) (status int, resp any, err error)
}

// LoginProvider 浏览器 OAuth 登录闭环（start / result / cancel）。
//
// 只有需要「网页登录换凭证」的平台才实现（Trae 是；WorkBuddy 走 cockpit 导入）。
type LoginProvider interface {
	// LoginStart 开始一次登录，返回前端渲染所需的引导信息
	// （授权 URL、pending_id、设备码等），具体字段由平台决定。
	LoginStart(body []byte) (any, error)
	// LoginResult 轮询登录结果（成功/失败/进行中）。
	LoginResult(query map[string][]string) (any, error)
	// LoginCancel 取消进行中的登录。
	LoginCancel(body []byte) (any, error)
	// CallbackPath 该平台 OAuth 回调的对外路径（面板展示引导文案用）。
	CallbackPath() string
}

// Importer 粘贴凭证 / 回调链接直接导入账号。
type Importer interface {
	Import(payload string) (any, error)
}

// ClientImporter 从**本机已安装的官方客户端**读取并导入凭证。
//
// 为什么需要它（2026-10 上游协议变更）：Trae 把登录协议从「回调直给
// refreshToken」改成「OAuth2 授权码 + PKCE 交换」，旧的一键登录 URL 不再
// 被上游接受（浏览器已登录时点「添加账号」会报「登录失败/网络错误」）。
//
// 但官方客户端**自己**登录是好的，凭证就落在本机磁盘上（加密）。
// 本能力让面板直接解密并导入，绕开登录协议本身 ——
// 这样即使上游再改登录协议，只要客户端能登，网关就能拿到凭证。
//
// 与 Importer 的区别：Importer 处理**用户粘贴的文本**（用户得先去别处复制），
// ClientImporter 是**零输入**的（自己找本机客户端、自己解密）。
type ClientImporter interface {
	// ImportFromClient 扫描本机客户端，解出凭证并导入。
	// 返回导入结果 + 探测过程（前端据此显示"找了哪些目录、为什么没找到"）。
	ImportFromClient() (any, error)
	// ProbeClient 只探测不导入（前端展示"本机装了哪个客户端、能否读取"）。
	ProbeClient() any
}

// RawModelLister 该平台上游模型目录的原始数据（面板「模型」页用）。
//
// 与 OpenAI 兼容面 /v1/models 的区别：那个是给下游客户端用的规范化列表，
// 这个是**上游原始条目**（含费率、raw_config 等），只给面板看。
type RawModelLister interface {
	RawModels() (any, error)
}

// ── 能力探测辅助 ─────────────────────────────────────────────────────────

// Capabilities 描述一个平台实际实现了哪些可选能力（面板启动时探测一次）。
//
// 探测结果要透出给前端：前端据此决定渲染哪些按钮/卡片，
// 而不是「点了才发现 501」。
type Capabilities struct {
	Status        bool `json:"status"`
	AccountAction bool `json:"account_action"`
	Login         bool `json:"login"`
	Import        bool `json:"import"`
	RawModels     bool `json:"raw_models"`
	// ClientImport 能否从本机官方客户端读取凭证（零输入导入）。
	ClientImport bool `json:"client_import"`
}

// ProbeCapabilities 对一个平台做能力探测。
//
// 用类型断言而不是在 Platform 里加一个 Capabilities() 方法：
// 断言让「实现了接口 = 具备能力」成为唯一事实来源，不可能出现
// 「声称支持但没实现」的漂移（那正是最容易被误配的地方）。
func ProbeCapabilities(p Platform) Capabilities {
	var c Capabilities
	if p == nil {
		return c
	}
	_, c.Status = p.(StatusProvider)
	_, c.AccountAction = p.(AccountActioner)
	_, c.Login = p.(LoginProvider)
	_, c.Import = p.(Importer)
	_, c.RawModels = p.(RawModelLister)
	_, c.ClientImport = p.(ClientImporter)
	return c
}

// Capabilities 返回该平台的能力集。
func (r *Registry) Capabilities(k Kind) (Capabilities, bool) {
	p := r.Get(k)
	if p == nil {
		return Capabilities{}, false
	}
	return ProbeCapabilities(p), true
}

// PlatformInfo 注册表对外暴露的单个平台元信息（面板导航数据源）。
type PlatformInfo struct {
	Kind        Kind         `json:"kind"`
	DisplayName string       `json:"display_name"`
	// ModelsPath 下游（DSH 等）接入该平台应使用的 baseURL 路径。
	ModelsPath string `json:"models_path"`
	// Accounts 该平台账号数。
	Accounts int `json:"accounts"`
	// Caps 可选能力集。
	Caps Capabilities `json:"capabilities"`
	// Status 平台自有计数（实现了 StatusProvider 才有值）。
	Status map[string]any `json:"status,omitempty"`
}

// Infos 返回全部平台的元信息（按注册顺序）。
func (r *Registry) Infos() []PlatformInfo {
	out := make([]PlatformInfo, 0, len(r.order))
	for _, p := range r.All() {
		info := PlatformInfo{
			Kind:        p.Kind(),
			DisplayName: p.DisplayName(),
			ModelsPath:  p.ModelsPath(),
			Accounts:    len(p.Accounts()),
			Caps:        ProbeCapabilities(p),
		}
		if sp, ok := p.(StatusProvider); ok {
			info.Status = sp.Status()
		}
		out = append(out, info)
	}
	return out
}

// ServeMuxProvider 可选能力：平台想自己挂额外路由时实现。
//
// 存在的意义：给平台留一个「逃生门」。若某个新平台需要的能力
// 上面几个接口都盖不住，它可以自己挂路由，而不用改抽象层——
// 抽象层不该成为新平台的瓶颈。
type ServeMuxProvider interface {
	RegisterExtra(mux *http.ServeMux)
}
