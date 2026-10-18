// allroutes.go 「All Account」统一面板的路由注册。
package panel

// allRoutes 注册汇总面板的接口（All Account 视图的数据源）。
//
// 前端只需要四个接口：一次 GET 拿全部快照，三个 POST 触发合并动作。
// 刻意不提供「按 source 分别操作」的端点——那些能力既有的 /panel/api/* 与
// /panel/api/trae/* 已经覆盖了，重复一套只会让两个入口漂移。
func (p *Panel) allRoutes() {
	p.mux.HandleFunc("GET /panel/api/all/dashboard", p.withAuth(p.allOverview))
	p.mux.HandleFunc("POST /panel/api/all/checkin", p.withAuth(p.allCheckin))
	p.mux.HandleFunc("POST /panel/api/all/keepalive", p.withAuth(p.allKeepalive))
	p.mux.HandleFunc("POST /panel/api/all/balance", p.withAuth(p.allBalance))
}
