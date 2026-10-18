// Package server Trae 网关 HTTP 层：OpenAI 兼容接口 + 登录回调 +
// /api/* 管理接口（供统一 panel 聚合）。
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/scheduler"
	"allplatforms/internal/platform/trae/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	PoolCfg  PoolOptions
	APIKey   string
	AuthDir  string
	Version  string
	Sched    *scheduler.Scheduler
	Start    time.Time
	// GetCfg 返回当前配置快照（管理接口 /api/config 透出）。
	GetCfg func() map[string]any
	// LoginConf OAuth 装配参数。
	LoginConf LoginSett
}

// PoolOptions 选号与冷却参数（来自 config）。
type PoolOptions struct {
	MaxRotate    int
	PlanCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration
	DefaultModel string
}

// LoginSett 登录闭环配置。
type LoginSett struct {
	ClientID     string
	CallbackPort int
	CallbackPath string
}

const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	loginMu sync.Mutex
	logins  map[string]*pendingLogin
	// bg 跟踪后台任务（首次签到等），供测试收尾等待；见 bgwork.go 的说明。
	bg bgWait
}

// NewHandler 构建 handler 并注册路由。
func NewHandler(cfg Config) *Handler {
	if cfg.PoolCfg.MaxRotate <= 0 {
		cfg.PoolCfg.MaxRotate = 3
	}
	if cfg.PoolCfg.PlanCooldown <= 0 {
		cfg.PoolCfg.PlanCooldown = 12 * time.Hour
	}
	if cfg.PoolCfg.SoftCooldown <= 0 {
		cfg.PoolCfg.SoftCooldown = 60 * time.Second
	}
	if cfg.PoolCfg.ErrThreshold <= 0 {
		cfg.PoolCfg.ErrThreshold = 3
	}
	if cfg.PoolCfg.ErrCooldown <= 0 {
		cfg.PoolCfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.PoolCfg.RefreshSkew <= 0 {
		cfg.PoolCfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.PoolCfg.DefaultModel == "" {
		cfg.PoolCfg.DefaultModel = upstream.DefaultConfigName
	}
	if cfg.LoginConf.CallbackPath == "" {
		cfg.LoginConf.CallbackPath = "/authorize"
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), logins: map[string]*pendingLogin{}}
	h.routes()
	return h
}

func (h *Handler) routes() {
	// OpenAI 兼容
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 观测
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	// Trae 管理接口（统一 panel 经 CORS 直连；同源部署则由 panel 反代）
	h.mux.HandleFunc("GET /api/overview", h.withAuth(h.traeOverview))
	h.mux.HandleFunc("GET /api/accounts", h.withAuth(h.traeAccounts))
	h.mux.HandleFunc("GET /api/models", h.withAuth(h.traeModelsRaw))
	h.mux.HandleFunc("POST /api/checkin_all", h.withAuth(h.traeCheckinAll))
	h.mux.HandleFunc("POST /api/keepalive_all", h.withAuth(h.traeKeepaliveAll))
	h.mux.HandleFunc("POST /api/balance_all", h.withAuth(h.traeBalanceAll))
	h.mux.HandleFunc("POST /api/accounts/{uid}/checkin", h.withAuth(h.traeAccountCheckin))
	h.mux.HandleFunc("POST /api/accounts/{uid}/balance", h.withAuth(h.traeAccountBalance))
	h.mux.HandleFunc("POST /api/accounts/{uid}/refresh", h.withAuth(h.traeAccountRefresh))
	h.mux.HandleFunc("POST /api/accounts/{uid}/disable", h.withAuth(h.traeAccountDisable))
	h.mux.HandleFunc("POST /api/accounts/{uid}/revive", h.withAuth(h.traeAccountRevive))
	h.mux.HandleFunc("POST /api/accounts/{uid}/remove", h.withAuth(h.traeAccountRemove))
	h.mux.HandleFunc("PATCH /api/accounts/{uid}", h.withAuth(h.traeAccountPatch))
	// 登录闭环
	h.mux.HandleFunc("POST /api/login/start", h.withAuth(h.loginStart))
	h.mux.HandleFunc("GET /api/login/result", h.withAuth(h.loginResult))
	h.mux.HandleFunc("POST /api/login/cancel", h.withAuth(h.loginCancel))
	h.mux.HandleFunc("POST /api/import", h.withAuth(h.traeImport))
	h.mux.HandleFunc("GET /authorize", h.authorizeCallback)
}

// ServeHTTP 统一入口：写安全头 + CORS 预检处理后分发。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if handleCORSPreflight(w, r) {
		return
	}
	h.mux.ServeHTTP(w, r)
}

// setSecurityHeaders 面板与 API 统一安全头。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}

// handleCORSPreflight 放行浏览器的 OPTIONS 预检，并给同源 JS 放行 CORS。
func handleCORSPreflight(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Credentials", "false")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Trae-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// withAuth Bearer 鉴权（常量时间比较）；api_key 为空放行。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.verifyKey(r) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// verifyKey 支持 Authorization: Bearer 与 X-Trae-Key 两种携带方式。
func (h *Handler) verifyKey(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	key := ""
	if v := r.Header.Get("X-Trae-Key"); v != "" {
		key = v
	} else if v := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(v), "bearer ") {
		key = strings.TrimSpace(v[len("bearer "):])
	}
	if key == "" {
		if q := r.URL.Query().Get("key"); q != "" {
			key = q
		}
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) == 1
}

// ---------------------------------------------------------------------------
// 观测接口
// ---------------------------------------------------------------------------

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled := h.cfg.Pool.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         healthy > 0,
		"service":    "traeapi2",
		"version":    h.cfg.Version,
		"total":      total,
		"healthy":    healthy,
		"cooling":    cooling,
		"disabled":   disabled,
		"uptime_sec": int(time.Since(h.cfg.Start).Seconds()),
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled := h.cfg.Pool.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "healthy": healthy, "cooling": cooling, "disabled": disabled,
		"accounts": h.cfg.Pool.List(),
	})
}

// ---------------------------------------------------------------------------
// Trae 管理接口（统一 panel 聚合面）
// ---------------------------------------------------------------------------

func (h *Handler) traeOverview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled := h.cfg.Pool.Counts()
	var credits int64
	for _, st := range h.cfg.Pool.List() {
		credits += st.Credits
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":       "trae",
		"version":       h.cfg.Version,
		"uptime_sec":    int(time.Since(h.cfg.Start).Seconds()),
		"auth_required": h.cfg.APIKey != "",
		"total":         total, "healthy": healthy, "cooling": cooling, "disabled": disabled,
		"credits":  credits,
		"accounts": h.cfg.Pool.List(),
	})
}

func (h *Handler) traeAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

func (h *Handler) traeModelsRaw(w http.ResponseWriter, r *http.Request) {
	a := h.cfg.Pool.Pick()
	if a == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_healthy_account", "无可用的 Trae 账号")
		return
	}
	infos, err := h.cfg.Upstream.FetchModels(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		row := map[string]any{
			"id": mi.ID, "name": mi.Name, "object": "model", "owned_by": "trae-solo",
		}
		if mi.Credits != "" {
			row["credits"] = mi.Credits
		}
		// 透出原始 config 整块，供面板解析费率等字段（不依赖上游解析）。
		if len(mi.RawConfig) > 0 {
			row["raw_config"] = json.RawMessage(mi.RawConfig)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (h *Handler) traeCheckinAll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Sched == nil {
		writeErr(w, http.StatusNotImplemented, "no_scheduler", "调度器不可用")
		return
	}
	sum := h.cfg.Sched.CheckinAll(time.Now())
	writeJSON(w, http.StatusOK, sum)
}

func (h *Handler) traeKeepaliveAll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Sched == nil {
		writeErr(w, http.StatusNotImplemented, "no_scheduler", "调度器不可用")
		return
	}
	sum := h.cfg.Sched.KeepaliveAll(time.Now())
	writeJSON(w, http.StatusOK, sum)
}

func (h *Handler) traeBalanceAll(w http.ResponseWriter, r *http.Request) {
	sum := scheduler.RefreshBalances(h.cfg.Upstream, h.cfg.Pool)
	writeJSON(w, http.StatusOK, sum)
}

func (h *Handler) withAccount(w http.ResponseWriter, r *http.Request, fn func(a *auth.Auth) (int, any)) {
	uid := r.PathValue("uid")
	if h.isValidUID(uid) == false {
		writeErr(w, http.StatusBadRequest, "invalid_uid", "非法 uid")
		return
	}
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	code, body := fn(a)
	writeJSON(w, code, body)
}

func (h *Handler) isValidUID(uid string) bool {
	if uid == "" || strings.ContainsAny(uid, `/\..`) || strings.Contains(uid, "..") {
		return false
	}
	return true
}

func (h *Handler) traeAccountCheckin(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		credits, msg, _, err := scheduler.CheckinOne(h.cfg.Upstream, h.cfg.Pool, a)
		if err != nil {
			return http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "uid": a.UID}
		}
		return http.StatusOK, map[string]any{"ok": true, "uid": a.UID, "msg": msg, "credits": credits}
	})
}

func (h *Handler) traeAccountBalance(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		remain, limit, used, packs, err := h.cfg.Upstream.EntUsage(a)
		if err != nil {
			return http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()}
		}
		h.cfg.Pool.SetUsage(a.UID, remain, limit, used, packs)
		return http.StatusOK, map[string]any{"ok": true, "uid": a.UID, "credits": remain, "limit": limit, "used": used, "packs": packs}
	})
}

func (h *Handler) traeAccountRefresh(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		if err := h.cfg.Upstream.RefreshToken(a); err != nil {
			return http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()}
		}
		if err := a.SaveAtomic(); err != nil {
			return http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()}
		}
		h.cfg.Pool.Revive(a.UID)
		return http.StatusOK, map[string]any{"ok": true, "uid": a.UID, "msg": "token 已刷新并落盘"}
	})
}

func (h *Handler) traeAccountDisable(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		h.cfg.Pool.Disable(a.UID, "手动禁用")
		return http.StatusOK, map[string]any{"ok": true, "uid": a.UID}
	})
}

func (h *Handler) traeAccountRevive(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		if !h.cfg.Pool.Revive(a.UID) {
			return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}
		}
		h.cfg.Pool.SetEnabled(a.UID, true, "")
		return http.StatusOK, map[string]any{"ok": true, "uid": a.UID}
	})
}

func (h *Handler) traeAccountRemove(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		if !h.cfg.Pool.Remove(a.UID) {
			return http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"}
		}
		_ = removeFile(a.FilePath)
		return http.StatusOK, map[string]any{"ok": true, "uid": a.UID}
	})
}

func (h *Handler) traeAccountPatch(w http.ResponseWriter, r *http.Request) {
	h.withAccount(w, r, func(a *auth.Auth) (int, any) {
		var req struct {
			Enabled    *bool    `json:"enabled"`
			Note       *string  `json:"note"`
			Nickname   *string  `json:"nickname"`
			SharedWith []string `json:"shared_with"`
		}
		if err := decodeJSON(r, &req); err != nil {
			return http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}
		}
		if req.Enabled != nil {
			h.cfg.Pool.SetEnabled(a.UID, *req.Enabled, "")
		}
		if req.Note != nil {
			h.cfg.Pool.SetNote(a.UID, *req.Note)
		}
		if req.Nickname != nil {
			a.Nickname = *req.Nickname
			_ = a.SaveAtomic()
		}
		if req.SharedWith != nil {
			h.cfg.Pool.SetSharedWith(a.UID, req.SharedWith)
		}
		_ = a.SaveAtomic()
		st, _ := h.cfg.Pool.Status(a.UID)
		return http.StatusOK, map[string]any{"ok": true, "account": st}
	})
}

// traeImport 粘贴凭证 / 回调链接直接导入。
func (h *Handler) traeImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Payload string `json:"payload"` // JSON 凭证 或 回调链接
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	res, err := h.importCredential(strings.TrimSpace(req.Payload))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "import_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// OpenAI 兼容
// ---------------------------------------------------------------------------

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			out = append(out, map[string]any{
				"id": mi.ID, "object": "model", "owned_by": "trae-solo",
				// 按模型真实窗口公告：命中覆盖表用真实值（如 DeepSeek-V4-Flash=128K），
				// 否则回退 1M 兜底。此前一律 1M 会让小窗口模型在 Trae 侧撑爆。
				"created": 1753600000, "context_length": upstream.ContextWindowForID(mi.ID),
			})
		}
		return out
	}
	return staticModels
}

var (
	dynMu      sync.RWMutex
	dynModels  []upstream.ModelInfo
	dynFetched time.Time
	dynFail    time.Time
)

const (
	dynTTL          = time.Hour
	dynFailCooldown = 5 * time.Minute
)

func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynMu.RLock()
	if len(dynModels) > 0 && time.Since(dynFetched) < dynTTL {
		out := dynModels
		dynMu.RUnlock()
		return out
	}
	if !dynFail.IsZero() && time.Since(dynFail) < dynFailCooldown {
		dynMu.RUnlock()
		return nil
	}
	dynMu.RUnlock()

	a := h.cfg.Pool.Pick()
	if a == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(a)
	if err != nil || len(infos) == 0 {
		dynMu.Lock()
		dynFail = time.Now()
		dynMu.Unlock()
		return nil
	}
	dynMu.Lock()
	dynModels = infos
	dynFetched = time.Now()
	dynFail = time.Time{}
	dynMu.Unlock()
	return infos
}

// staticModels 动态拉取失败时的兜底目录（实测 config_name 集合）。
// context_length 统一公告 DefaultContextWindow（1M 兜底），避免下游被错误引导早缩。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "glm-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "glm-5-turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "DeepSeek-V4-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "DeepSeek-V4-Flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.ContextWindowForID("DeepSeek-V4-Flash")},
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "Doubao-Seed-2.0-Code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
	{"id": "qwen-3.7-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": upstream.DefaultContextWindow},
}

// mapModel 归一化模型名 → config_name。
// 未知模型直接原样转发（上游自有 400 兜底，网关不擅自拦截）。
func (h *Handler) mapModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		return h.cfg.PoolCfg.DefaultModel
	}
	return model
}

// extractSessionKey 会话粘性键：头 → body.user → 首轮内容指纹。
func extractSessionKey(r *http.Request, body []byte) string {
	if sid := r.Header.Get("X-Session-ID"); sid != "" {
		return sid
	}
	if cid := r.Header.Get("X-Conversation-ID"); cid != "" {
		return cid
	}
	var req struct {
		User     string `json:"user"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err == nil {
		if req.User != "" {
			return req.User
		}
		if len(req.Messages) > 0 {
			hh := sha256.New()
			if req.Messages[0].Role == "system" {
				hh.Write([]byte("sys:"))
				hh.Write(req.Messages[0].Content)
			}
			for _, m := range req.Messages {
				if m.Role != "system" {
					hh.Write([]byte(m.Role + ":"))
					hh.Write(m.Content)
					break
				}
			}
			return "fp:" + hex.EncodeToString(hh.Sum(nil)[:8])
		}
	}
	return ""
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	configName := h.mapModel(peek.Model)
	body = setModelInBody(body, configName)
	sessionKey := extractSessionKey(r, body)

	start := time.Now()
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	acquire := func(uid string) {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
		heldUID = uid
		h.cfg.Pool.Acquire(uid)
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.PoolCfg.MaxRotate; i++ {
		a := h.cfg.Pool.PickAffinity(sessionKey, tried)
		if a == nil {
			break
		}
		tried[a.UID] = true
		acquire(a.UID)

		refreshed, rerr := h.cfg.Upstream.RefreshTokenIfNeeded(a, h.cfg.PoolCfg.RefreshSkew)
		if rerr != nil {
			lastErr = rerr
			record.refreshFail(h.cfg, a, rerr)
			continue
		}
		if refreshed {
			_ = a.SaveAtomic()
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(r.Context(), a, body)
		if terr != nil {
			lastErr = terr
			record.transportFail(h.cfg, a, terr)
			continue
		}
		if status >= 400 {
			lastErr = fmt.Errorf("upstream %d: %s", status, string(respBody))
			if !h.classifyAndCool(a, status, respBody) {
				continue
			}
			continue
		}

		if peek.Stream {
			h.cfg.Pool.RecordSuccess(a.UID)
			h.cfg.Pool.NoteSuccess(a.UID)
			logChat(start, a.UID, configName, http.StatusOK, 0)
			_ = upstream.Stream(w, rc, configName, func(se *upstream.SOLOStreamError) {
				h.handleStreamError(a.UID, se)
			})
			_ = rc.Close()
			return
		}
		resp, aerr := upstream.Aggregate(rc, configName)
		_ = rc.Close()
		if aerr != nil {
			lastErr = aerr
			var se *upstream.SOLOStreamError
			if errors.As(aerr, &se) {
				h.handleStreamError(a.UID, se)
				continue
			}
			h.cfg.Pool.RecordError(a.UID, upstream.Truncate(aerr.Error(), 200))
			h.cfg.Pool.NoteError(a.UID, h.cfg.PoolCfg.ErrThreshold, h.cfg.PoolCfg.ErrCooldown)
			continue
		}
		h.cfg.Pool.RecordSuccess(a.UID)
		h.cfg.Pool.NoteSuccess(a.UID)
		if ToTokenCounter(resp) > 0 {
			h.cfg.Pool.AddTokens(a.UID, ToTokenCounter(resp))
		}
		logChat(start, a.UID, configName, http.StatusOK, ToTokenCounter(resp))
		writeJSON(w, http.StatusOK, resp)
		return
	}

	logChat(start, "<none>", configName, http.StatusServiceUnavailable, 0)
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeErr(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// classifyAndCool 按状态码分类并冷却账号；返回是否需要轮转下一账号（恒 true）。
func (h *Handler) classifyAndCool(a *auth.Auth, status int, respBody []byte) bool {
	kind := upstream.Classify(status, string(respBody))
	h.cfg.Pool.RecordError(a.UID, upstream.Truncate(string(respBody), 200))
	switch kind {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.Cooldown(a.UID, pool.CoolPlan, h.cfg.PoolCfg.PlanCooldown, "plan 权益不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(a.UID, pool.CoolSoft, h.cfg.PoolCfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(a.UID, "session dead")
	case upstream.ErrNotFound:
		h.cfg.Pool.Cooldown(a.UID, pool.CoolSoft, h.cfg.PoolCfg.SoftCooldown, "upstream 404")
	default:
		h.cfg.Pool.NoteError(a.UID, h.cfg.PoolCfg.ErrThreshold, h.cfg.PoolCfg.ErrCooldown)
	}
	return true
}

// handleStreamError 流内业务错误 → 冷却状态机。
func (h *Handler) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	switch se.Kind() {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.Cooldown(uid, pool.CoolPlan, h.cfg.PoolCfg.PlanCooldown, "plan 权益不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.PoolCfg.SoftCooldown, "429 rate limit")
	default:
		h.cfg.Pool.NoteError(uid, h.cfg.PoolCfg.ErrThreshold, h.cfg.PoolCfg.ErrCooldown)
	}
	h.cfg.Pool.RecordError(uid, upstream.Truncate(se.Error(), 200))
}

// failRecorder 失败处置辅助（保持 chat 主流程可读）。
type failRecorder struct{}

var record = failRecorder{}

func (failRecorder) refreshFail(cfg Config, a *auth.Auth, err error) {
	cfg.Pool.RecordError(a.UID, upstream.Truncate(err.Error(), 200))
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		cfg.Pool.Disable(a.UID, "refresh session dead")
		return
	}
	cfg.Pool.Cooldown(a.UID, pool.CoolErr, cfg.PoolCfg.ErrCooldown, "refresh: "+upstream.Truncate(err.Error(), 80))
}

func (failRecorder) transportFail(cfg Config, a *auth.Auth, err error) {
	cfg.Pool.RecordError(a.UID, upstream.Truncate(err.Error(), 200))
	cfg.Pool.NoteError(a.UID, cfg.PoolCfg.ErrThreshold, cfg.PoolCfg.ErrCooldown)
}

// setModelInBody 替换请求体里的 model 字段。
func setModelInBody(body []byte, model string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

func decodeJSON(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBodyBytes {
		return errors.New("request too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil // 空 body 视为全缺省，交由字段指针判空
	}
	return json.Unmarshal(raw, v)
}

// ToTokenCounter 从聚合响应里取 total_tokens。
func ToTokenCounter(resp map[string]any) int64 {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return 0
	}
	switch v := u["total_tokens"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
