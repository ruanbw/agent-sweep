package gui

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guard 是 serve 模式的本机访问护栏。
//
// `agentsweep serve` 是一个调试口：它可以删会话、也能触发一次自我替换。
// 默认只让回环来源访问；即使有人 `--addr 0.0.0.0:8766` 把它暴露出去，
// 这里也会拒绝非本机请求，避免「局域网里谁都能删你的会话」。
//
// 桌面壳（wails://）不经过这层：它的来源是应用自身，不需要也不该受 Host 限制。
func (s *Service) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.guardLocal {
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopbackRemote(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, "serve 只接受本机请求")
			return
		}
		if host := hostOnly(r.Host); host != "" && !isLoopbackName(host) {
			writeError(w, http.StatusForbidden, "serve 只接受本机 Host")
			return
		}
		if isMutating(r.Method) {
			// 浏览器跨站发来的写请求会带 Origin；它必须与当前 Host 同源。
			if origin := r.Header.Get("Origin"); origin != "" {
				parsed, err := url.Parse(origin)
				if err != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
					writeError(w, http.StatusForbidden, "拒绝跨站请求")
					return
				}
			}
			// 表单提交（text/plain / x-www-form-urlencoded）不带预检，是最容易被借用的
			// CSRF 载体；写接口只收 JSON。空 body 的 POST（如 /api/scan）不受影响。
			if r.ContentLength > 0 {
				contentType := r.Header.Get("Content-Type")
				if index := strings.IndexByte(contentType, ';'); index >= 0 {
					contentType = contentType[:index]
				}
				contentType = strings.ToLower(strings.TrimSpace(contentType))
				if contentType != "" && contentType != "application/json" {
					writeError(w, http.StatusUnsupportedMediaType, "请求体必须是 JSON")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

func isLoopbackName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
