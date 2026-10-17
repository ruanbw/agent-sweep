package gui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/update"
)

// 更新接口的测试同样不碰网络：httptest 假扮 GitHub，目标目录用临时目录。
// 这里只测 HTTP 状态机；「下载校验替换」由 internal/update 自己的测试覆盖。

func updateFixture(t *testing.T, tag string) *Service {
	t.Helper()
	svc := NewService("0.1.0")
	svc.appDir = t.TempDir()
	svc.prefs = defaultPrefs()
	svc.updateCache = filepath.Join(svc.appDir, "update.json")
	svc.updateView = UpdateView{State: "idle", Current: "0.1.0", CanApply: true}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"tag_name": %q,
			"name": "AgentSweep",
			"body": "修了三个 bug",
			"html_url": "https://github.com/ruanbw/agent-sweep/releases/tag/%s",
			"published_at": "2026-10-17T12:00:00Z",
			"assets": [
				{"name": "AgentSweep-%s-darwin-universal.zip", "browser_download_url": %q, "size": 12345678},
				{"name": "checksums.txt", "browser_download_url": %q, "size": 100}
			]
		}`, tag, tag, tag, "https://example.invalid/app.zip", "https://example.invalid/checksums.txt")
	}))
	t.Cleanup(server.Close)

	svc.updater = &update.Client{
		HTTP:      server.Client(),
		APIBase:   server.URL,
		Repo:      "owner/repo",
		GOOS:      "darwin",
		GOARCH:    "amd64",
		UserAgent: "AgentSweep/0.1.0",
	}
	return svc
}

func updateRequest(t *testing.T, svc *Service, method, path string, body any) (int, UpdateView, string) {
	t.Helper()
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = data
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	var view UpdateView
	_ = json.Unmarshal(recorder.Body.Bytes(), &view)
	return recorder.Code, view, recorder.Body.String()
}

func TestUpdateEndpointInitialState(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	code, view, body := updateRequest(t, svc, http.MethodGet, "/api/update", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/update 返回 %d：%s", code, body)
	}
	if view.State != "idle" || view.Current != "0.1.0" {
		t.Fatalf("初始状态 = %+v", view)
	}
	if !view.AutoCheck {
		t.Fatal("默认应开启自动检查")
	}
}

func TestUpdateCheckFindsNewVersion(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	code, view, body := updateRequest(t, svc, http.MethodPost, "/api/update/check", nil)
	if code != http.StatusOK {
		t.Fatalf("检查更新返回 %d：%s", code, body)
	}
	if view.State != "available" || !view.Available {
		t.Fatalf("应发现新版本，实际 %+v", view)
	}
	if view.Latest != "0.1.1" || view.Current != "0.1.0" {
		t.Fatalf("版本字段 = %+v", view)
	}
	if view.AssetName != "AgentSweep-v0.1.1-darwin-universal.zip" {
		t.Fatalf("选中的包 = %q", view.AssetName)
	}
	if view.PageURL == "" || view.CheckedAt == "" {
		t.Fatalf("发布页/检查时间为空：%+v", view)
	}

	// 检查结果要落盘，下次启动先看缓存再决定联不联网。
	if _, err := os.Stat(svc.updateCache); err != nil {
		t.Fatalf("检查结果没有缓存：%v", err)
	}
}

func TestUpdateCheckUpToDate(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	code, view, body := updateRequest(t, svc, http.MethodPost, "/api/update/check", nil)
	if code != http.StatusOK {
		t.Fatalf("检查更新返回 %d：%s", code, body)
	}
	if view.State != "uptodate" || view.Available {
		t.Fatalf("同版本应显示已是最新，实际 %+v", view)
	}
}

func TestUpdateCheckErrorKeepsKnownVersion(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	svc.updateView = UpdateView{
		State: "available", Current: "0.1.0", Latest: "0.1.2", Available: true,
		Notes: "旧说明", PageURL: "https://example.invalid/tag",
	}
	// 把客户端指向一个必然失败的地址。
	svc.updater = &update.Client{
		HTTP:      &http.Client{Transport: failingTransport{}},
		APIBase:   "http://127.0.0.1:1",
		Repo:      "owner/repo",
		GOOS:      "darwin",
		GOARCH:    "amd64",
		UserAgent: "AgentSweep/0.1.0",
	}
	code, view, body := updateRequest(t, svc, http.MethodPost, "/api/update/check", nil)
	if code != http.StatusOK {
		t.Fatalf("检查失败也应返回 200 状态对象，实际 %d：%s", code, body)
	}
	if view.State != "error" || view.Error == "" {
		t.Fatalf("应进入 error 状态，实际 %+v", view)
	}
	if view.Latest != "0.1.2" || !view.Available {
		t.Fatalf("失败不该抹掉已知的新版本：%+v", view)
	}
}

func TestUpdateApplyRejectsNonBundle(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	svc.updateView = UpdateView{State: "available", Current: "0.1.0", Latest: "0.1.1", Available: true, CanApply: false}
	code, _, body := updateRequest(t, svc, http.MethodPost, "/api/update/apply", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("非 .app 运行时申请安装应 400，实际 %d：%s", code, body)
	}
}

func TestUpdateApplyRejectsWhenUpToDate(t *testing.T) {
	svc := updateFixture(t, "v0.1.0")
	code, _, body := updateRequest(t, svc, http.MethodPost, "/api/update/apply", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("没有更新时安装应 400，实际 %d：%s", code, body)
	}
}

func TestPrefsAutoCheckUpdatesRoundTrip(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	code, _, body := updateRequest(t, svc, http.MethodPut, "/api/prefs", map[string]any{"autoCheckUpdates": false})
	if code != http.StatusOK {
		t.Fatalf("PUT /api/prefs 返回 %d：%s", code, body)
	}
	if svc.prefs.AutoCheckUpdates {
		t.Fatal("关闭自动检查没有生效")
	}
	// 再打开。
	code, _, body = updateRequest(t, svc, http.MethodPut, "/api/prefs", map[string]any{"autoCheckUpdates": true})
	if code != http.StatusOK {
		t.Fatalf("PUT /api/prefs 返回 %d：%s", code, body)
	}
	if !svc.prefs.AutoCheckUpdates {
		t.Fatal("开启自动检查没有生效")
	}
	data, err := os.ReadFile(filepath.Join(svc.appDir, "prefs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"autoCheckUpdates": true`)) {
		t.Fatalf("prefs.json 没有持久化新字段：%s", data)
	}
}

func TestUpdateCacheRestoresState(t *testing.T) {
	svc := updateFixture(t, "v0.1.1")
	cache := `{"checkedAt":"2026-10-17T12:00:00Z","tag":"v0.1.1","version":"0.1.1",
		"notes":"说明","pageUrl":"https://example.invalid/tag","assetName":"AgentSweep-v0.1.1-darwin-universal.zip",
		"assetSize":10,"publishedAt":"2026-10-17T12:00:00Z"}`
	if err := os.WriteFile(svc.updateCache, []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.initUpdate() // 测试二进制不在 .app 里，因此 CanApply=false
	if !svc.updateView.Available || svc.updateView.Latest != "0.1.1" {
		t.Fatalf("缓存没有恢复出新版本：%+v", svc.updateView)
	}
	if svc.updateView.State != "unsupported" {
		t.Fatalf("无法自动安装时状态应为 unsupported，实际 %q", svc.updateView.State)
	}
}

// failingTransport 让所有请求立即失败，用来模拟离线。
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("离线")
}
