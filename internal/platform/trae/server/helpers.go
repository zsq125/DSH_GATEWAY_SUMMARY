package server

import (
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"allplatforms/internal/platform/trae/auth"
	"allplatforms/internal/platform/trae/pool"
	"allplatforms/internal/platform/trae/upstream"
)

// logf 统一日志前缀，便于 grep。
func logf(format string, args ...any) {
	log.Printf("[trae] "+format, args...)
}

// logChat 一行表格化聊天日志（时间/uid/模型/耗时/token）。
func logChat(start time.Time, uid, model string, status, tokens int64) {
	log.Printf("chat | uid=%s model=%-24s status=%d %6dms tokens=%d",
		uid, truncateS(model, 24), status, time.Since(start).Milliseconds(), tokens)
}

func truncateS(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// renderAuthPage 登录回调结果页（浏览器直接跳转用，非 JSON）。
func renderAuthPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	ok := status == http.StatusOK
	color := "#cf3b34"
	if ok {
		color = "#0f9d63"
	}
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<title>%s</title><style>
body{font:15px/1.7 system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;
display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f4f5f8;color:#14171f}
.card{background:#fff;border:1px solid #e2e5ec;border-radius:12px;padding:32px 40px;max-width:520px;
box-shadow:0 1px 2px rgba(16,20,32,.06),0 8px 24px -14px rgba(16,20,32,.14)}
h1{font-size:18px;margin:0 0 10px;color:%s}p{color:#545c6e;margin:0;font-size:14px}
</style></head><body><div class="card"><h1>%s</h1><p>%s</p></div></body></html>`,
		html.EscapeString(title), color, html.EscapeString(title), html.EscapeString(detail))
}

// removeFile 删除文件（不存在视为成功）。
func removeFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

// checkinQuietImpl 静默签到（新账号首次入账）。
//
// ── 额度必须**所有分支**都回写（2026-10-14 修复）──────────────────────
//
// 原先只在「本次签到成功」分支调 EntUsage 写额度，「今日已签到」分支直接
// return，导致一个很隐蔽的缺陷：
//
//	新账号在官方客户端登录时通常**已经签过到**，于是导入时走 checked=true
//	分支 → 只写 checked_in，**不写 credits** → 台账 credits 为 0
//	→ 面板显示「积分 0」，要等后台周期刷新（30 分钟）或手动刷才出现真实值。
//
// 用户感知就是「新导入的账号没有积分」（而其他老账号有）——看着像数据丢失，
// 实际只是没查。所以现在无论走哪个分支，只要拿到额度就回写。
func checkinQuietImpl(up *upstream.Client, p *pool.Pool, a *auth.Auth) (int64, string, error) {
	if _, err := up.RefreshTokenIfNeeded(a, time.Hour); err == nil {
		_ = a.SaveAtomic()
	}
	checked, _, enable, err := up.CheckinStatus(a)
	if err != nil {
		return 0, "", err
	}
	if checked {
		p.SetCheckedIn(a.UID, true)
		// 已签到也要查额度：否则新导入账号的积分要等后台刷新才显示。
		remain := refreshUsageQuiet(up, p, a)
		return remain, "今日已签到", nil
	}
	if !enable {
		// 签到未开启同样补一次额度（账号可能只是没开签到，额度仍应可见）。
		remain := refreshUsageQuiet(up, p, a)
		return remain, "签到未开启", nil
	}
	code, cerr := up.CheckinClaim(a)
	if cerr != nil {
		// 仅 10001（今日已签）按幂等成功；其他失败不标记已签到，交由重试。
		if code == upstream.CheckinCodeAlready {
			p.SetCheckedIn(a.UID, true)
			remain := refreshUsageQuiet(up, p, a)
			return remain, "今日已签到", nil
		}
		return 0, cerr.Error(), nil
	}
	p.SetCheckedIn(a.UID, true)
	remain := refreshUsageQuiet(up, p, a)
	return remain, "签到成功", nil
}

// refreshUsageQuiet 查一次额度并回写台账；失败不影响签到结果。
//
// 单独抽出来是因为现在有 4 个分支都要用它（原先只在成功分支内联调用，
// 导致其余分支漏写）。失败**静默**：额度查不到不该让签到算失败
//（签到本身已经成功），下次后台刷新会补上。
func refreshUsageQuiet(up *upstream.Client, p *pool.Pool, a *auth.Auth) int64 {
	remain, limit, used, packs, err := up.EntUsage(a)
	if err != nil {
		logf("usage %s: 查额度失败（不影响签到）: %v", a.UID, err)
		return 0
	}
	p.SetUsage(a.UID, remain, limit, used, packs)
	return remain
}
