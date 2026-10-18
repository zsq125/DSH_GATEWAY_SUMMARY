// paths.go 平台路径前缀与共用的路径工具。
//
// 这些常量是**下游接入契约**的一部分（DSH 等按路径选平台），
// 刻意不做成配置项：改了会静默打断所有已配置的下游。
package main

import (
	"strconv"
	"time"
)

// workbuddyPathPrefix WorkBuddy 平台的路径前缀。
//
// 与 traePathPrefix（见 trae_assembly.go）对称，两者共同构成
// 「一个端口、按路径区分平台」的对外契约：
//
//	http://127.0.0.1:7863/workbuddy/v1  → WorkBuddy
//	http://127.0.0.1:7863/trae/v1       → Trae
const workbuddyPathPrefix = "/workbuddy"

// itoa int → string（cmd 包内共用，避免每个文件各引一次 strconv）。
func itoa(n int) string { return strconv.Itoa(n) }

// parseInt 解析十进制整数；失败返回错误。
func parseInt(s string) (int, error) { return strconv.Atoi(s) }

// nowFunc 时间源出口（Trae handler 的 Start 字段用）。
//
// 保留为函数而不是在各处直接写 time.Now()：装配期需要一个明确的
// 「进程启动时刻」，集中一个出口便于将来注入测试时钟。
func nowFunc() time.Time { return time.Now() }
