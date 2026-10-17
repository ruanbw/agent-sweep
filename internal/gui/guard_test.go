package gui

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/update"
)

// 这一组测试覆盖 serve 的访问护栏：它是本机调试口，不能因为绑了 0.0.0.0
// 就变成「局域网里谁都能删会话、触发更新」。
func TestServeGuardRejectsRemoteAddress(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	svc.guardLocal = true
	request := httptest.NewRequest(http.MethodGet, "/api/update", nil)
	request.RemoteAddr = "192.168.1.20:5555"
	request.Host = "127.0.0.1:8766"
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非本机请求应 403，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("错误响应必须是 JSON，实际 %q", recorder.Header().Get("Content-Type"))
	}
}

func TestServeGuardAllowsLoopback(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	svc.guardLocal = true
	request := httptest.NewRequest(http.MethodGet, "/api/update", nil)
	request.RemoteAddr = "127.0.0.1:5555"
	request.Host = "127.0.0.1:8766"
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("本机请求应放行，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestServeGuardRejectsCrossOriginWrite(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	svc.guardLocal = true
	request := httptest.NewRequest(http.MethodPost, "/api/prefs", strings.NewReader(`{"autoScan":false}`))
	request.RemoteAddr = "127.0.0.1:5555"
	request.Host = "127.0.0.1:8766"
	request.Header.Set("Origin", "http://evil.example")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("跨站写请求应 403，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestServeGuardRejectsFormPost(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	svc.guardLocal = true
	request := httptest.NewRequest(http.MethodPost, "/api/prefs", strings.NewReader("autoScan=false"))
	request.RemoteAddr = "127.0.0.1:5555"
	request.Host = "127.0.0.1:8766"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("表单写请求应 415，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestServeGuardAllowsJSONWrite(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	svc.guardLocal = true
	request := httptest.NewRequest(http.MethodPut, "/api/prefs", strings.NewReader(`{"autoScan":false}`))
	request.RemoteAddr = "127.0.0.1:5555"
	request.Host = "127.0.0.1:8766"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:8766")
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("同源 JSON 写请求应放行，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestApplyConflictWhenBusy(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	svc.updateView = UpdateView{State: "available", Current: "0.1.0", Latest: "0.1.1", Available: true, CanApply: true}
	svc.mu.Lock()
	svc.updateBusy = true
	svc.updateOwner = "install"
	svc.mu.Unlock()

	code, _, body := updateRequest(t, svc, http.MethodPost, "/api/update/apply", nil)
	if code != http.StatusConflict {
		t.Fatalf("已有安装时二次 apply 应 409，实际 %d：%s", code, body)
	}
	if !strings.Contains(body, "已经有一个更新") {
		t.Fatalf("409 应该带回人话错误，实际 %s", body)
	}
}

func TestApplyRejectsUnsupportedState(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	svc.updateView = UpdateView{State: "unsupported", Current: "0.1.0", Latest: "0.1.1", Available: true, CanApply: false}
	code, _, body := updateRequest(t, svc, http.MethodPost, "/api/update/apply", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unsupported 状态应 400，实际 %d：%s", code, body)
	}
}

// installFixture 造一个可以完整跑通「检查 → 下载 → 校验 → 替换 → 重启」的环境：
// 假 GitHub、临时 .app、注入的 relaunch/onRestart。
func installFixture(t *testing.T) (*Service, string, string, <-chan string, <-chan string) {
	t.Helper()
	target := writeAppBundle(t, t.TempDir(), "0.1.0")
	archive := makeAppZipBytes(t, "0.1.1")
	sum := sha256.Sum256(archive)
	zipName := "AgentSweep-v0.1.1-darwin-universal.zip"

	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":"v0.1.1","name":"AgentSweep","body":"修好了",
			"html_url":"https://example.invalid/tag","published_at":"2026-10-17T12:00:00Z",
			"assets":[
				{"name":%q,"browser_download_url":%q,"size":%d},
				{"name":"checksums.txt","browser_download_url":%q,"size":100}
			]}`,
			zipName, server.URL+"/download/"+zipName, len(archive), server.URL+"/download/checksums.txt")
	})
	mux.HandleFunc("/download/"+zipName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(archive)))
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), zipName)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)

	svc := NewService("0.1.0")
	svc.appDir = t.TempDir()
	svc.prefs = defaultPrefs()
	svc.updateCache = filepath.Join(svc.appDir, "update.json")
	svc.updateView = UpdateView{State: "idle", Current: "0.1.0", CanApply: true}
	svc.updateTarget = target
	svc.updater = &update.Client{
		HTTP:      server.Client(),
		APIBase:   server.URL,
		Repo:      "owner/repo",
		GOOS:      "darwin",
		GOARCH:    "amd64",
		UserAgent: "AgentSweep/0.1.0",
	}

	relaunched := make(chan string, 1)
	svc.relaunchUpdate = func(appPath string, pid int, workDir string) error {
		relaunched <- appPath
		return nil
	}
	restarted := make(chan string, 1)
	svc.onRestart = func() {
		restarted <- svc.snapshotUpdate().State
	}
	return svc, target, zipName, relaunched, restarted
}

func writeAppBundle(t *testing.T, parent, version string) string {
	t.Helper()
	app := filepath.Join(parent, "AgentSweep.app")
	macOS := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macOS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(appInfoPlist(version)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macOS, "AgentSweep"), []byte("#!/bin/sh\necho agentsweep\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

func appInfoPlist(version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>AgentSweep</string>
<key>CFBundleShortVersionString</key><string>` + version + `</string>
</dict></plist>`
}

func makeAppZipBytes(t *testing.T, version string) []byte {
	t.Helper()
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	add := func(name, body string, mode os.FileMode) {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte(body))
	}
	add("AgentSweep.app/Contents/Info.plist", appInfoPlist(version), 0o644)
	add("AgentSweep.app/Contents/MacOS/AgentSweep", "#!/bin/sh\necho agentsweep\n", 0o755)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestApplyInstallsAndRestarts(t *testing.T) {
	svc, target, zipName, relaunched, restarted := installFixture(t)

	code, view, body := updateRequest(t, svc, http.MethodPost, "/api/update/check", nil)
	if code != http.StatusOK || view.State != "available" {
		t.Fatalf("检查更新失败：%d %s", code, body)
	}
	if view.AssetName != zipName {
		t.Fatalf("选中的资产 = %q", view.AssetName)
	}

	code, _, body = updateRequest(t, svc, http.MethodPost, "/api/update/apply", nil)
	if code != http.StatusOK {
		t.Fatalf("apply 应 200，实际 %d：%s", code, body)
	}

	select {
	case appPath := <-relaunched:
		// Install 会对目标做 EvalSymlinks（/var → /private/var），比较时也解析一次。
		resolved, _ := filepath.EvalSymlinks(target)
		if appPath != target && appPath != resolved {
			t.Fatalf("重启脚本的目标 = %q，期望 %q", appPath, target)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("安装完成后没有调用重启脚本（v0.1.0 的 bug 回归？）")
	}

	select {
	case state := <-restarted:
		if state != "done" {
			t.Fatalf("退出前的状态 = %q，期望 done", state)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("没有调用 onRestart 退出旧进程")
	}

	version, err := updateBundleVersion(target)
	if err != nil || version != "0.1.1" {
		t.Fatalf("替换后的版本 = %q，err = %v", version, err)
	}
	if _, err := os.Stat(svc.updateCache); !os.IsNotExist(err) {
		t.Fatalf("安装成功后应删除检查缓存：%v", err)
	}
}

// updateBundleVersion 只读本地 Info.plist，不依赖 update 包未导出的实现。
func updateBundleVersion(app string) (string, error) {
	data, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return "", err
	}
	pattern := "<key>CFBundleShortVersionString</key><string>"
	index := strings.Index(string(data), pattern)
	if index < 0 {
		return "", fmt.Errorf("缺少 CFBundleShortVersionString")
	}
	rest := string(data)[index+len(pattern):]
	end := strings.IndexByte(rest, '<')
	if end < 0 {
		return "", fmt.Errorf("版本号格式不对")
	}
	return rest[:end], nil
}
