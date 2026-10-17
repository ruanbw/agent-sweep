package clean

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// useSharedHostHome 把共享索引的宿主数据根指向临时目录；用例结束自动复原。
func useSharedHostHome(t *testing.T, hostHome string) {
	t.Helper()
	original := sharedHostHome
	sharedHostHome = func(hostID string) string {
		if hostID == "vscode" {
			return hostHome
		}
		return ""
	}
	t.Cleanup(func() { sharedHostHome = original })
}

// makeSharedIndexDB 按实机 schema 造一个 agent-host.db：
// 三张表各有目标会话与一条无关会话。
func makeSharedIndexDB(t *testing.T, dbPath, targetID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	targetURI := "vscode-chat://claude-code/" + targetID
	unrelatedURI := "vscode-chat://claude-code/other-999"
	statements := []string{
		`CREATE TABLE sessions (session_uri TEXT PRIMARY KEY, provider TEXT, start_time TEXT)`,
		`CREATE TABLE sessions_v2 (session_uri TEXT PRIMARY KEY, provider TEXT, payload TEXT)`,
		`CREATE TABLE session_chat_catalogs (session_uri TEXT PRIMARY KEY, revision INTEGER)`,
		`CREATE TABLE session_chats (session_uri TEXT, chat_uri TEXT, chat_order INTEGER,
			FOREIGN KEY (session_uri) REFERENCES session_chat_catalogs(session_uri) ON DELETE CASCADE)`,
		`INSERT INTO sessions (session_uri, provider) VALUES ('` + targetURI + `', 'claude'), ('` + unrelatedURI + `', 'claude')`,
		`INSERT INTO sessions_v2 (session_uri, provider) VALUES ('` + targetURI + `', 'claude'), ('` + unrelatedURI + `', 'claude')`,
		`INSERT INTO session_chat_catalogs (session_uri, revision) VALUES ('` + targetURI + `', 0), ('` + unrelatedURI + `', 0)`,
		`INSERT INTO session_chats (session_uri, chat_uri, chat_order) VALUES ('` + targetURI + `', 'chat-1', 0), ('` + unrelatedURI + `', 'chat-2', 0)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// countSharedRows 数某张表里指定 session_uri 的行数。
func countSharedRows(t *testing.T, dbPath, table, uri string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()
	var count int
	query := "SELECT COUNT(*) FROM " + quoteIdent(table) + " WHERE " + quoteIdent("session_uri") + " = ?"
	if err := db.QueryRow(query, uri).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// TestDeleteTrimsSharedVSCodeIndex 覆盖跨 Spec 索引：删 claude-code 的正文时，
// VS Code globalStorage/agent-host.db 里三张表都要联动裁掉该会话。
func TestDeleteTrimsSharedVSCodeIndex(t *testing.T) {
	f := newFixture(t, ModePermanent)
	hostHome := t.TempDir()
	dbPath := filepath.Join(hostHome, "globalStorage", "agent-host.db")
	const sessionID = "abc-123"
	makeSharedIndexDB(t, dbPath, sessionID)
	useSharedHostHome(t, hostHome)

	spec, ok := catalog.Find("claude-code")
	if !ok {
		t.Fatal("内置目录里没有 claude-code")
	}
	body := filepath.Join(f.home, "sessions", sessionID+".jsonl")
	writeTestFile(t, body, `{"type":"user"}`+"\n")

	res, err := f.cleaner.Delete(spec, f.home, []session.Session{{
		ID: sessionID, UID: spec.ID + ":" + sessionID, AgentID: spec.ID,
		Storage: session.StorageFile, Paths: []string{body},
	}})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除不该报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d，want 1", res.Count)
	}

	targetURI := "vscode-chat://claude-code/" + sessionID
	unrelatedURI := "vscode-chat://claude-code/other-999"
	for _, table := range []string{"sessions", "sessions_v2", "session_chat_catalogs", "session_chats"} {
		if got := countSharedRows(t, dbPath, table, targetURI); got != 0 {
			t.Errorf("%s 里目标会话应被裁掉，还剩 %d 行", table, got)
		}
		if got := countSharedRows(t, dbPath, table, unrelatedURI); got != 1 {
			t.Errorf("%s 里的无关会话不该被动，剩 %d 行", table, got)
		}
	}

	// journal 必须记下这个库的备份，否则撤销无法还原三张表。
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 {
		t.Fatalf("journal sqlite = %d，want 1（agent-host.db）", len(j.SQLite))
	}
	if j.SQLite[0].DB != dbPath {
		t.Fatalf("journal DB = %q，want %q", j.SQLite[0].DB, dbPath)
	}
	if len(j.SQLite[0].Deletes) != 4 {
		t.Fatalf("journal 删除记录 = %d 张表，want 4", len(j.SQLite[0].Deletes))
	}
	if !fileExists(j.SQLite[0].Backup) {
		t.Fatalf("共享索引备份不存在: %s", j.SQLite[0].Backup)
	}

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("Undo errors: %+v", undo.Errors)
	}
	for _, table := range []string{"sessions", "sessions_v2", "session_chat_catalogs", "session_chats"} {
		if got := countSharedRows(t, dbPath, table, targetURI); got != 1 {
			t.Errorf("撤销后 %s 里目标会话应回来，实际 %d 行", table, got)
		}
	}
}

// TestDeleteSkipsSharedIndexForOtherAgents 不在这份清单里的 Agent 不能碰
// agent-host.db —— 否则会误删别家登记的会话。
func TestDeleteSkipsSharedIndexForOtherAgents(t *testing.T) {
	f := newFixture(t, ModePermanent)
	hostHome := t.TempDir()
	dbPath := filepath.Join(hostHome, "globalStorage", "agent-host.db")
	const sessionID = "xyz-456"
	makeSharedIndexDB(t, dbPath, sessionID)
	useSharedHostHome(t, hostHome)

	spec := catalog.Spec{ID: "workbuddy"}
	body := filepath.Join(f.home, "sessions", sessionID+".jsonl")
	writeTestFile(t, body, `{"type":"user"}`+"\n")

	res, err := f.cleaner.Delete(spec, f.home, []session.Session{{
		ID: sessionID, UID: spec.ID + ":" + sessionID, AgentID: spec.ID,
		Storage: session.StorageFile, Paths: []string{body},
	}})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}

	uri := "vscode-chat://claude-code/" + sessionID
	for _, table := range []string{"sessions", "sessions_v2", "session_chat_catalogs", "session_chats"} {
		if got := countSharedRows(t, dbPath, table, uri); got != 1 {
			t.Errorf("%s 的行不该被 workbuddy 的删除动过，剩 %d 行", table, got)
		}
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 0 {
		t.Fatalf("不该为共享索引写 journal，got %d 条", len(j.SQLite))
	}
}

// fileExists 是 os.Stat 的薄包装，测试里读起来短一点。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
