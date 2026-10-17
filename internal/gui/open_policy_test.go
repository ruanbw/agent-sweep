package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// 这组测试只碰临时目录：HOME 指到 t.TempDir()，catalog.AppDir() 也跟着落进去，
// 绝不读写真机 ~/Library。openCommand 默认是「调用即失败」的记录器，
// 保证任何路径下的测试都不会真的弹出 Finder / 启动应用。

type recordedOpen struct {
	name string
	args []string
}

func openPolicyFixture(t *testing.T) (*Service, string, string, string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	appDir := catalog.AppDir()
	journalDir := filepath.Join(appDir, "journal")
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(journalDir, "batch-1.json")
	if err := os.WriteFile(journal, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionDir := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(filepath.Join(sessionDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(sessionDir, "s1.jsonl")
	if err := os.WriteFile(sessionFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "nested", "note.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 兄弟目录：用来验证 /demo 不能放行 /demo2。
	siblingFile := filepath.Join(home, "projects", "demo2", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(siblingFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &Service{
		appDir: appDir,
		sessions: []session.Session{
			{
				UID: "demo:s1", AgentID: "demo", Storage: session.StorageFile,
				Paths: []string{sessionFile},
			},
			{
				// 目录型会话：整个目录都是白名单根，目录下的文件才可展开。
				UID: "demo:dir", AgentID: "demo", Storage: session.StorageDir,
				Paths: []string{sessionDir},
			},
		},
		byUID: map[string]session.Session{},
		openCommand: func(name string, args ...string) error {
			t.Fatalf("测试不该真的调用 open：%s %v", name, args)
			return nil
		},
	}
	for _, item := range svc.sessions {
		svc.byUID[item.UID] = item
	}
	return svc, sessionFile, sessionDir, journal
}

func TestAllowedLocalPath(t *testing.T) {
	svc, sessionFile, sessionDir, journal := openPolicyFixture(t)

	link := filepath.Join(sessionDir, "escape")
	outsideTarget := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsideTarget, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("当前平台不允许普通用户创建 symlink：%v", err)
		}
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		path  string
		allow bool
	}{
		{"会话文件", sessionFile, true},
		{"会话目录本身", sessionDir, true},
		{"会话目录下的子文件", filepath.Join(sessionDir, "nested", "note.txt"), true},
		{"应用数据目录本身", svc.appDir, true},
		{"应用数据目录下的 journal", journal, true},

		{"系统文件", "/etc/passwd", false},
		{"应用包", "/Applications/Calculator.app", false},
		{"会话路径的兄弟目录", filepath.Join(filepath.Dir(sessionDir), "demo2", "s1.jsonl"), false},
		{"带 .. 的逃逸", filepath.Join(sessionDir, "..", "demo2", "s1.jsonl"), false},
		{"会话文件再往上跳", filepath.Join(sessionFile, "..", "..", "demo2", "s1.jsonl"), false},
		{".. 但仍在根内", filepath.Join(sessionDir, "nested", "..", "s1.jsonl"), true},
		{"不存在的路径", filepath.Join(sessionDir, "missing.json"), false},
		{"空串", "", false},
		{"相对路径", "s1.jsonl", false},
		{"GitHub URL 不是本地路径", "https://github.com/x", false},
		{"会话目录里的 symlink 指向外部", link, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.allowedLocalPath(tc.path); got != tc.allow {
				t.Fatalf("allowedLocalPath(%q) = %v，want %v", tc.path, got, tc.allow)
			}
		})
	}
}

func TestAllowedExternalURLUnchanged(t *testing.T) {
	cases := []struct {
		url   string
		allow bool
	}{
		{"https://github.com/x", true},
		{"https://www.github.com/org/repo/releases/latest", true},
		{"https://evil.example", false},
		{"http://github.com/x", false},
		{"https://github.com.evil.example/x", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := allowedExternalURL(tc.url); got != tc.allow {
			t.Errorf("allowedExternalURL(%q) = %v，want %v", tc.url, got, tc.allow)
		}
	}
}

func TestOpenHandlerRejectsOutsidePathWithoutExecuting(t *testing.T) {
	svc, sessionFile, sessionDir, _ := openPolicyFixture(t)

	link := filepath.Join(sessionDir, "escape")
	outsideTarget := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsideTarget, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("当前平台不允许普通用户创建 symlink：%v", err)
		}
		t.Fatal(err)
	}

	outside := []string{
		"/etc/passwd",
		"/Applications/Calculator.app",
		filepath.Join(filepath.Dir(sessionDir), "demo2", "s1.jsonl"),
		filepath.Join(sessionDir, "..", "demo2", "s1.jsonl"),
		link,
	}
	for _, path := range outside {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var calls []recordedOpen
			svc.openCommand = func(name string, args ...string) error {
				calls = append(calls, recordedOpen{name: name, args: append([]string(nil), args...)})
				return nil
			}

			recorder := postPath(t, svc, "/api/open", path)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("越界路径应返回 403，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("错误响应不是 JSON：%s", recorder.Body.String())
			}
			if payload["error"] == "" {
				t.Fatal("403 响应应带可读错误信息")
			}
			if len(calls) != 0 {
				t.Fatalf("被拒绝的路径绝不能调用 open：%+v", calls)
			}
		})
	}

	t.Run("允许的会话文件仍然可打开", func(t *testing.T) {
		var calls []recordedOpen
		svc.openCommand = func(name string, args ...string) error {
			calls = append(calls, recordedOpen{name: name, args: append([]string(nil), args...)})
			return nil
		}
		recorder := postPath(t, svc, "/api/open", sessionFile)
		if recorder.Code != http.StatusOK {
			t.Fatalf("会话文件应放行，实际 %d：%s", recorder.Code, recorder.Body.String())
		}
		if len(calls) != 1 || calls[0].name != "open" || len(calls[0].args) != 1 || calls[0].args[0] != sessionFile {
			t.Fatalf("open 调用参数不对：%+v", calls)
		}
	})
}

func TestOpenHandlerKeepsURLPolicy(t *testing.T) {
	svc, _, _, _ := openPolicyFixture(t)

	cases := []struct {
		url    string
		status int
		allow  bool
	}{
		{"https://github.com/x", http.StatusOK, true},
		{"https://evil.example", http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			var calls []recordedOpen
			svc.openCommand = func(name string, args ...string) error {
				calls = append(calls, recordedOpen{name: name, args: append([]string(nil), args...)})
				return nil
			}
			recorder := postPath(t, svc, "/api/open", tc.url)
			if recorder.Code != tc.status {
				t.Fatalf("%s 返回 %d，want %d：%s", tc.url, recorder.Code, tc.status, recorder.Body.String())
			}
			if !tc.allow {
				if len(calls) != 0 {
					t.Fatalf("被拒绝的 URL 绝不能调用 open：%+v", calls)
				}
				return
			}
			if len(calls) != 1 || calls[0].name != "open" || len(calls[0].args) != 1 || calls[0].args[0] != tc.url {
				t.Fatalf("open 调用参数不对：%+v", calls)
			}
		})
	}
}

func TestRevealHandlerRejectsOutsidePathWithoutExecuting(t *testing.T) {
	svc, sessionFile, _, _ := openPolicyFixture(t)

	for _, path := range []string{"/etc/passwd", "/Applications/Calculator.app"} {
		var calls []recordedOpen
		svc.openCommand = func(name string, args ...string) error {
			calls = append(calls, recordedOpen{name: name, args: append([]string(nil), args...)})
			return nil
		}
		recorder := postPath(t, svc, "/api/reveal", path)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("reveal %s 应返回 403，实际 %d：%s", path, recorder.Code, recorder.Body.String())
		}
		if len(calls) != 0 {
			t.Fatalf("被拒绝的路径绝不能调用 open：%+v", calls)
		}
	}

	var calls []recordedOpen
	svc.openCommand = func(name string, args ...string) error {
		calls = append(calls, recordedOpen{name: name, args: append([]string(nil), args...)})
		return nil
	}
	recorder := postPath(t, svc, "/api/reveal", sessionFile)
	if recorder.Code != http.StatusOK {
		t.Fatalf("会话文件 reveal 应放行，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if len(calls) != 1 || calls[0].name != "open" || len(calls[0].args) != 2 ||
		calls[0].args[0] != "-R" || calls[0].args[1] != sessionFile {
		t.Fatalf("reveal 调用参数不对：%+v", calls)
	}
}

func postPath(t *testing.T, svc *Service, endpoint, path string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	return recorder
}
