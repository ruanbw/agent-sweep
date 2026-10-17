// 这个测试只关心「发现」这一层的判断力，不启动 GUI、不碰真实 HOME：
// 每个用例把 HOME 指到 t.TempDir()，在临时目录里伪造 Agent 的落盘结构，
// 这样既不会扫到开发机上的真实会话，也能把「无关目录」「已知根」隔离干净。
package probe

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// setupHome 把 HOME 换成临时目录。os.UserHomeDir 在 darwin/linux 上读 $HOME，
// catalog.Expand 的 `~` 展开也会跟着走同一个变量，所以已知根的测试是自洽的。
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// 一条最小的消息流：只要文件头有 role/content/messages 就足以让 probe 认出
// 它是会话正文，不需要完整的 Agent 协议。
const sessionJSONL = `{"type":"message","role":"user","content":"hello"}
{"type":"message","role":"assistant","content":"hi"}
`

// makeSQLiteFixture 造一个「表名和列名都像会话库」的 SQLite 文件。
// 列名故意覆盖 probe 的常见候选，用来验证 Suggested 里的映射。
func makeSQLiteFixture(t *testing.T, path string, rows int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		title TEXT,
		updated_at INTEGER,
		cwd TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO sessions (id, title, updated_at, cwd) VALUES (?, ?, ?, ?)`,
			"session-"+strings.Repeat("x", i+1), "标题", 1700000000000+i, "/tmp/project"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func candidateByRoot(t *testing.T, candidates []Candidate, root string) *Candidate {
	t.Helper()
	for i := range candidates {
		for _, got := range candidates[i].Suggested.Roots {
			if got == root {
				return &candidates[i]
			}
		}
	}
	return nil
}

func TestDiscoverJSONLSessionDir(t *testing.T) {
	home := setupHome(t)
	dir := filepath.Join(home, "Library", "Application Support", "JsonlAgent", "sessions")
	writeFile(t, filepath.Join(dir, "session-1.jsonl"), sessionJSONL)
	writeFile(t, filepath.Join(dir, "session-2.jsonl"), sessionJSONL)

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("期望只发现 1 个候选，实际 %d 个：%+v", len(candidates), candidates)
	}
	candidate := candidateByRoot(t, candidates, "~/Library/Application Support/JsonlAgent")
	if candidate == nil {
		t.Fatalf("没有发现 JsonlAgent：%+v", candidates)
	}
	if candidate.Confidence < minConfidence {
		t.Errorf("置信度 %.2f 低于门槛 %.2f", candidate.Confidence, minConfidence)
	}
	if candidate.SessionCount != 2 {
		t.Errorf("SessionCount = %d，期望 2", candidate.SessionCount)
	}
	if candidate.Bytes <= 0 {
		t.Errorf("Bytes 应该大于 0，实际 %d", candidate.Bytes)
	}
	if len(candidate.Evidence) == 0 {
		t.Error("Evidence 不应该为空")
	}
	if candidate.Suggested.ID != "jsonlagent" {
		t.Errorf("ID = %q，期望 jsonlagent", candidate.Suggested.ID)
	}
	if candidate.Suggested.Name != "JsonlAgent" {
		t.Errorf("Name = %q，期望 JsonlAgent", candidate.Suggested.Name)
	}
	if candidate.Suggested.Group != catalog.GroupOther {
		t.Errorf("Group = %q，期望 other", candidate.Suggested.Group)
	}
	if !candidate.Suggested.Custom {
		t.Error("Suggested.Custom 应该为 true")
	}
	if len(candidate.Suggested.Stores) != 1 {
		t.Fatalf("Stores 数量 = %d，期望 1", len(candidate.Suggested.Stores))
	}
	store := candidate.Suggested.Stores[0]
	if store.Kind != "jsonl" {
		t.Errorf("Store.Kind = %q，期望 jsonl", store.Kind)
	}
	if store.Glob != "sessions/*.jsonl" {
		t.Errorf("Store.Glob = %q，期望 sessions/*.jsonl", store.Glob)
	}
}

func TestDiscoverSQLiteSessionDB(t *testing.T) {
	home := setupHome(t)
	dbPath := filepath.Join(home, "Library", "Application Support", "SqliteAgent", "agent.db")
	makeSQLiteFixture(t, dbPath, 3)

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	candidate := candidateByRoot(t, candidates, "~/Library/Application Support/SqliteAgent")
	if candidate == nil {
		t.Fatalf("没有发现 SqliteAgent：%+v", candidates)
	}
	if candidate.Confidence < minConfidence {
		t.Errorf("置信度 %.2f 低于门槛 %.2f", candidate.Confidence, minConfidence)
	}
	if candidate.SessionCount != 3 {
		t.Errorf("SessionCount = %d，期望 3", candidate.SessionCount)
	}
	if len(candidate.Suggested.Stores) != 1 {
		t.Fatalf("Stores 数量 = %d，期望 1", len(candidate.Suggested.Stores))
	}
	store := candidate.Suggested.Stores[0]
	if store.Kind != "sqlite" {
		t.Fatalf("Store.Kind = %q，期望 sqlite", store.Kind)
	}
	if store.DB != "agent.db" {
		t.Errorf("Store.DB = %q，期望相对 Roots 的 agent.db", store.DB)
	}
	if store.Table != "sessions" {
		t.Errorf("Store.Table = %q，期望 sessions", store.Table)
	}
	if store.IDCol != "id" {
		t.Errorf("Store.IDCol = %q，期望 id", store.IDCol)
	}
	if store.TitleCol != "title" {
		t.Errorf("Store.TitleCol = %q，期望 title", store.TitleCol)
	}
	if store.TimeCol != "updated_at" {
		t.Errorf("Store.TimeCol = %q，期望 updated_at", store.TimeCol)
	}
}

func TestDiscoverIgnoresUnrelatedDirectory(t *testing.T) {
	home := setupHome(t)
	dir := filepath.Join(home, "Library", "Application Support", "UnrelatedApp")
	writeFile(t, filepath.Join(dir, "notes.txt"), "只是普通文档")
	writeFile(t, filepath.Join(dir, "config.json"), `{"hello":"world"}`)

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("无关目录不应被识别，实际发现 %d 个：%+v", len(candidates), candidates)
	}
	for _, candidate := range candidates {
		if strings.Contains(candidate.Path, "UnrelatedApp") {
			t.Errorf("无关目录出现在了结果里：%s", candidate.Path)
		}
	}
}

func TestDiscoverSkipsKnownRoots(t *testing.T) {
	home := setupHome(t)
	writeFile(t, filepath.Join(home, ".knownagent", "sessions", "s1.jsonl"), sessionJSONL)

	// 先证明这个夹具本身是「可被发现」的，否则后面的「跳过」断言可能是假阳性。
	before, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover(nil): %v", err)
	}
	if candidateByRoot(t, before, "~/.knownagent") == nil {
		t.Fatalf("夹具没有被识别，无法验证已知根跳过逻辑：%+v", before)
	}

	known := []catalog.Spec{{
		ID:    "knownagent",
		Name:  "Known Agent",
		Roots: []string{"~/.knownagent"},
	}}
	after, err := Discover(known)
	if err != nil {
		t.Fatalf("Discover(known): %v", err)
	}
	if candidateByRoot(t, after, "~/.knownagent") != nil {
		t.Fatalf("已知 Agent 的根不应被扫出：%+v", after)
	}
	for _, candidate := range after {
		if strings.Contains(candidate.Path, ".knownagent") {
			t.Errorf("已知根的路径出现在结果里：%s", candidate.Path)
		}
	}
}

// 浏览器缓存和 node_modules 里经常有长得像会话的 JSONL（下载记录、扩展缓存），
// 但它们不是 Agent 的会话正文。这里把同样的内容放进被排除的路径，
// 确认「必须排除」不是只写在注释里。
func TestDiscoverExcludesCacheAndDependencyNoise(t *testing.T) {
	home := setupHome(t)
	writeFile(t, filepath.Join(home, "Library", "Caches", "Google", "Chrome", "Default", "sessions", "s1.jsonl"), sessionJSONL)
	writeFile(t, filepath.Join(home, "Library", "Application Support", "NoiseApp", "node_modules", "pkg", "sessions", "s2.jsonl"), sessionJSONL)

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("被排除的目录不应产生候选，实际 %d 个：%+v", len(candidates), candidates)
	}
}

// 真实世界里的噪音源：一个父目录下几十个 UUID 子目录，每个装一个 session.db。
// 它们必须合并成一条候选，否则发现页会被同一批数据刷满 20 条。
func TestDiscoverMergesSiblingSessionDirs(t *testing.T) {
	home := setupHome(t)
	base := filepath.Join(home, "Library", "Application Support", "Code", "agentSessionData")
	uuids := []string{
		"017d2395-2c7e-47a6-bdfb-4d37db894527",
		"0541305a-e068-44c7-8a57-d6304ff32d52",
		"0a6d2222-0ee0-437d-83fc-428dc4fd48d6",
		"0bb2fbab-3d9a-4cf1-b858-05d8f349a09e",
		"0c493bf1-8473-4d82-aaf3-a4c44cc25aea",
	}
	for _, uuid := range uuids {
		dbPath := filepath.Join(base, uuid, "session.db")
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE session_metadata (
			id TEXT PRIMARY KEY, title TEXT, updated_at INTEGER
		)`); err != nil {
			t.Fatalf("create table: %v", err)
		}
		for i := 0; i < 2; i++ {
			if _, err := db.Exec(`INSERT INTO session_metadata (id, title, updated_at) VALUES (?, ?, ?)`,
				uuid+"-"+string(rune('a'+i)), "标题", 1700000000000+i); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		db.Close()
	}

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("同一父目录下的会话单元应合并为 1 条，实际 %d 条：%+v", len(candidates), candidates)
	}
	candidate := candidates[0]
	if candidate.Path != base {
		t.Errorf("Path = %q，期望父目录 %q", candidate.Path, base)
	}
	if candidate.Name != "AgentSessionData" {
		t.Errorf("Name = %q，期望 AgentSessionData", candidate.Name)
	}
	if candidate.SessionCount != len(uuids) {
		t.Errorf("SessionCount = %d，期望 %d（一个子目录 = 一条会话）", candidate.SessionCount, len(uuids))
	}
	joined := strings.Join(candidate.Evidence, " | ")
	for _, want := range []string{"5 个形如 uuid 的子目录", "session.db", "session_metadata"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Evidence 缺少 %q：%v", want, candidate.Evidence)
		}
	}
	if len(candidate.Suggested.Stores) != 1 {
		t.Fatalf("Stores 数量 = %d，期望 1", len(candidate.Suggested.Stores))
	}
	store := candidate.Suggested.Stores[0]
	if store.Kind != "dir" {
		t.Errorf("Store.Kind = %q，期望 dir（多库无法用单条 sqlite Store 表达）", store.Kind)
	}
	if store.Glob != "*" {
		t.Errorf("Store.Glob = %q，期望 *（Roots 就是父目录）", store.Glob)
	}
	if candidate.Suggested.Roots[0] != "~/Library/Application Support/Code/agentSessionData" {
		t.Errorf("Roots = %v，期望指向父目录 agentSessionData", candidate.Suggested.Roots)
	}
}

// 文件在两层面板下：billion-context/sessions/openai/*.json。
// 建议必须从「文件所在目录」反推成 json + sessions/*/*.json，
// 而不是看到 sessions/ 就给 dir（那会把整个 openai 目录当成 1 条会话）。
func TestDiscoverJSONFilesTwoLevelsDeep(t *testing.T) {
	home := setupHome(t)
	dir := filepath.Join(home, ".local", "share", "billion-context", "sessions", "openai")
	for i := 0; i < 42; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("session-%02d.json", i)),
			`{"messages":[{"role":"user","content":"hi"}]}`)
	}

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	candidate := candidateByRoot(t, candidates, "~/.local/share/billion-context")
	if candidate == nil {
		t.Fatalf("没有发现 billion-context：%+v", candidates)
	}
	if candidate.SessionCount != 42 {
		t.Errorf("SessionCount = %d，期望 42", candidate.SessionCount)
	}
	if len(candidate.Suggested.Stores) != 1 {
		t.Fatalf("Stores 数量 = %d，期望 1", len(candidate.Suggested.Stores))
	}
	store := candidate.Suggested.Stores[0]
	if store.Kind != "json" {
		t.Errorf("Store.Kind = %q，期望 json", store.Kind)
	}
	if store.Glob != "sessions/*/*.json" {
		t.Errorf("Store.Glob = %q，期望 sessions/*/*.json", store.Glob)
	}
}

// 一目录一会话：Cline 的 tasks/<id>/api_conversation_history.json。
// 这种形态才该用 dir，Glob 指到会话目录层 tasks/*。
func TestDiscoverDirPerSessionJSON(t *testing.T) {
	home := setupHome(t)
	base := filepath.Join(home, "Library", "Application Support", "ClineLike", "tasks")
	for i := 0; i < 3; i++ {
		writeFile(t,
			filepath.Join(base, "task-"+string(rune('a'+i)), "api_conversation_history.json"),
			`{"messages":[{"role":"user","content":"hi"}]}`)
	}

	candidates, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	candidate := candidateByRoot(t, candidates, "~/Library/Application Support/ClineLike")
	if candidate == nil {
		t.Fatalf("没有发现 ClineLike：%+v", candidates)
	}
	if candidate.SessionCount != 3 {
		t.Errorf("SessionCount = %d，期望 3", candidate.SessionCount)
	}
	if len(candidate.Suggested.Stores) != 1 {
		t.Fatalf("Stores 数量 = %d，期望 1", len(candidate.Suggested.Stores))
	}
	store := candidate.Suggested.Stores[0]
	if store.Kind != "dir" {
		t.Errorf("Store.Kind = %q，期望 dir", store.Kind)
	}
	if store.Glob != "tasks/*" {
		t.Errorf("Store.Glob = %q，期望 tasks/*", store.Glob)
	}
}
