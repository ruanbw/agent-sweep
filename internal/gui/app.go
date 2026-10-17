// Package gui 是桌面壳：一个 Wails v3 应用 + 一个 embed 进来的纯静态前端。
//
// 为什么不用 wails 的 bindings / 事件系统：前端只是一组 DOM 操作，
// 它需要的东西（列表、删除、撤销）本来就是请求-响应式的。
// 把静态资源和 /api/* 挂在同一个 http.Handler 上，前端连一行生成代码都不用引，
// 换来的好处是「双击 index.html 也能跑演示数据」——UI 可以脱离 Go 单独调。
package gui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed assets
var assetsFS embed.FS

// staticHandler 提供前端静态资源。
//
// 不存在的路径回落到 index.html 而不是 404：前端只有一页，
// 任何「看起来像路由」的路径都应该还是那一页。
func staticHandler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "" {
			clean = "index.html"
		}
		if _, err := fs.Stat(sub, clean); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/index.html"
		}
		// 开发时（go run）不缓存，改完刷新就能看到；打包后也只是几百 KB，不值得加缓存头。
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// Serve 只起 HTTP 服务，不开窗口。用于调试与无窗口环境。
func Serve(addr, version string) error {
	service := NewService(version)
	server := &http.Server{Addr: addr, Handler: service.Handler()}
	fmt.Printf("AgentSweep %s · http://%s\n", version, addr)
	return server.ListenAndServe()
}

// Run 启动桌面应用。必须在主 goroutine 上调用（Cocoa 的要求）。
func Run(version string) error {
	service := NewService(version)

	app := application.New(application.Options{
		Name:        "AgentSweep",
		Description: "扫描并清理本机所有 AI Agent 的会话记录",
		Assets:      application.AssetOptions{Handler: service.Handler()},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyRegular,
		},
	})

	// 自动扫描交给前端发 POST /api/scan（bootstrap 里带着 autoScan 开关），
	// 主进程不抢跑：扫描是异步的，先起窗口再做后台事，用户才不会盯着空白窗口。
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "AgentSweep",
		Width:     1180,
		Height:    760,
		MinWidth:  920,
		MinHeight: 560,
		URL:       "/",
	})

	return app.Run()
}
