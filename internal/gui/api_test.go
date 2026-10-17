package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/clean"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// 这组测试走完整的 HTTP 链路：请求体 → 分组 → clean → 内存状态更新 → JSON 响应。
//
// 用临时目录当 Agent 数据根，**绝不碰真实用户数据** —— 一个测试如果会删真实会话，
// 它迟早会在某次 `go test ./...` 里删掉开发者的东西。
// 删除模式用 permanent：trash 模式会往真实 ~/.Trash 里写，那是测试不该有的副作用。

func fixtureService(t *testing.T, mode string) (*Service, catalog.Spec, string) {
	t.Helper()
	home := t.TempDir()
	project := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "s1.jsonl"),
		[]byte(`{"message":{"role":"user","content":"第一条"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "s2.jsonl"),
		[]byte(`{"message":{"role":"user","content":"第二条"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 索引：s1 对应两行，s2 一行。删 s1 后这两行必须消失、s2 那行必须留着。
	history := `{"sessionId":"s1","display":"第一条"}` + "\n" +
		`{"sessionId":"s1","display":"第一条（续）"}` + "\n" +
		`{"sessionId":"s2","display":"第二条"}` + "\n"
	if err := os.WriteFile(filepath.Join(home, "history.jsonl"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := catalog.Spec{
		ID: "demo", Name: "Demo", Group: catalog.GroupCLI,
		Roots:  []string{home},
		Stores: []catalog.Store{{Kind: "jsonl", Glob: "projects/*/*.jsonl"}},
		Indexs: []catalog.Index{{Kind: "jsonl-lines", Glob: "history.jsonl", Key: "sessionId"}},
	}

	svc := NewService("test")
	svc.prefs.DeleteMode = mode
	svc.specs = []catalog.Spec{spec}
	svc.agents = []AgentView{{AgentID: "demo", Name: "Demo", Installed: true, SessionCount: 2}}
	svc.sessions = []session.Session{
		{ID: "s1", UID: "demo:s1", AgentID: "demo", Title: "第一条",
			Paths: []string{filepath.Join(project, "s1.jsonl")}, Storage: session.StorageFile, Bytes: 48},
		{ID: "s2", UID: "demo:s2", AgentID: "demo", Title: "第二条",
			Paths: []string{filepath.Join(project, "s2.jsonl")}, Storage: session.StorageFile, Bytes: 48},
	}
	svc.byUID = map[string]session.Session{}
	for _, item := range svc.sessions {
		svc.byUID[item.UID] = item
	}
	svc.appDir = t.TempDir()
	svc.icons = newIconStore(filepath.Join(svc.appDir, "icons"))
	svc.cleaner = clean.New(clean.Options{
		Mode:       mode,
		BackupDir:  filepath.Join(svc.appDir, "backups"),
		JournalDir: filepath.Join(svc.appDir, "journal"),
	})
	return svc, spec, home
}

func postJSON(t *testing.T, svc *Service, path string, body any) map[string]any {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s 返回 %d：%s", path, recorder.Code, recorder.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%s", recorder.Body.String())
	}
	return out
}

func TestDeleteTrimsIndexAndUndoRestores(t *testing.T) {
	// 废纸篓根目录由 HOME 推导，指到临时目录，避免往开发机真实的 ~/.Trash 里扔东西。
	t.Setenv("HOME", t.TempDir())
	svc, _, home := fixtureService(t, "trash")
	sessionFile := filepath.Join(home, "projects", "demo", "s1.jsonl")

	result := postJSON(t, svc, "/api/delete", map[string]any{"uids": []string{"demo:s1"}})
	if result["count"].(float64) != 1 {
		t.Fatalf("应删 1 条，实际 %v", result["count"])
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatal("会话文件应已被删除")
	}
	history, err := os.ReadFile(filepath.Join(home, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(history, []byte(`"s1"`)) {
		t.Fatalf("索引里还留着 s1：%s", history)
	}
	if !bytes.Contains(history, []byte(`"s2"`)) {
		t.Fatalf("不该删掉 s2 的索引行：%s", history)
	}

	// 内存状态同步：删完列表里不该再有它。
	if len(svc.sessions) != 1 || svc.sessions[0].UID != "demo:s2" {
		t.Fatalf("内存状态没同步：%+v", svc.sessions)
	}

	batchIDs, _ := result["batchIds"].([]any)
	undo := postJSON(t, svc, "/api/undo", map[string]any{"batchIds": batchIDs})
	// Restored 统计的是「恢复动作数」（正文文件 + 索引备份各算一次），不是会话数。
	if undo["restored"].(float64) < 1 {
		t.Fatalf("撤销应至少恢复 1 个动作，实际 %v", undo["restored"])
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("撤销后会话文件应回来：%v", err)
	}
}

func TestDeleteReportsPartialFailure(t *testing.T) {
	svc, _, _ := fixtureService(t, "permanent")
	// 把 s2 的路径改到 Agent 数据根之外：清理器必须拒绝，并且**保留**它在内存列表里。
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("do not delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	item := svc.byUID["demo:s2"]
	item.Paths = []string{outside}
	svc.byUID["demo:s2"] = item
	for i := range svc.sessions {
		if svc.sessions[i].UID == "demo:s2" {
			svc.sessions[i].Paths = []string{outside}
		}
	}

	result := postJSON(t, svc, "/api/delete", map[string]any{"uids": []string{"demo:s2"}})
	errorsList, _ := result["errors"].([]any)
	if len(errorsList) == 0 {
		t.Fatal("越界路径应回报错误")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("越界文件绝不能被删")
	}
	found := false
	for _, item := range svc.sessions {
		if item.UID == "demo:s2" {
			found = true
		}
	}
	if !found {
		t.Fatal("删除失败的会话不该从内存里消失")
	}
}

func TestPrefsPartialPatchKeepsOtherFlags(t *testing.T) {
	svc, _, _ := fixtureService(t, "permanent")
	svc.prefs = Prefs{DeleteMode: "trash", ConfirmBeforeDelete: true, IncludeSnapshots: true, AutoScan: true}

	// 只关掉一个开关，其它三个必须原样保留 —— 这是「逐开关发请求」的契约。
	request := httptest.NewRequest(http.MethodPut, "/api/prefs",
		bytes.NewReader([]byte(`{"confirmBeforeDelete":false}`)))
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("返回 %d：%s", recorder.Code, recorder.Body.String())
	}
	if svc.prefs.ConfirmBeforeDelete {
		t.Fatal("confirmBeforeDelete 应为 false")
	}
	if svc.prefs.DeleteMode != "trash" || !svc.prefs.IncludeSnapshots || !svc.prefs.AutoScan {
		t.Fatalf("未提及的字段被改掉了：%+v", svc.prefs)
	}
}

func TestBootstrapShape(t *testing.T) {
	svc, _, _ := fixtureService(t, "permanent")
	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("返回 %d", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "prefs", "scan", "agents", "sessions", "batches", "home"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("bootstrap 缺少字段 %q", key)
		}
	}
}
