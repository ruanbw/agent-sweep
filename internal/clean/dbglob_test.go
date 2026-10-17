package clean

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// writeCleanDBFixture 在 path 建一个最小会话库并插一行。
func writeCleanDBFixture(t *testing.T, path, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT)`,
		`INSERT INTO sessions (id, title) VALUES ('` + id + `', '标题')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// sessionRowsInDB 数某个库里 sessions 表的行数。
func sessionRowsInDB(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	return count
}

// TestDeleteSQLiteUsesSessionDBPath 覆盖多库声明：删除只碰会话自己记录的那个
// 库，journal 里记的也是那个库的路径。
func TestDeleteSQLiteUsesSessionDBPath(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbA := filepath.Join(f.home, "chats", "a", "store.db")
	dbB := filepath.Join(f.home, "chats", "b", "store.db")
	writeCleanDBFixture(t, dbA, "s1")
	writeCleanDBFixture(t, dbB, "s2")

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "chats/*/store.db", Table: "sessions", IDCol: "id",
		}},
	}
	itemA := session.Session{
		ID: "s1", UID: "demo:s1", AgentID: "demo",
		Storage: session.StorageSQLite, DBPath: dbA,
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{itemA})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除不该报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d，want 1", res.Count)
	}
	if got := sessionRowsInDB(t, dbA); got != 0 {
		t.Fatalf("目标库 s1 应已删除，剩 %d 行", got)
	}
	if got := sessionRowsInDB(t, dbB); got != 1 {
		t.Fatalf("另一个库不该被动过，剩 %d 行（want 1）", got)
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 {
		t.Fatalf("journal sqlite = %d，want 1（只有目标库）", len(j.SQLite))
	}
	if j.SQLite[0].DB != dbA {
		t.Fatalf("journal DB = %q，want %q", j.SQLite[0].DB, dbA)
	}
	if len(j.SQLite[0].Deletes) != 1 || len(j.SQLite[0].Deletes[0].IDs) != 1 ||
		j.SQLite[0].Deletes[0].IDs[0] != "s1" {
		t.Fatalf("journal 删除记录 = %+v，want 只删 s1", j.SQLite[0].Deletes)
	}

	// 再删另一个库里的会话同样成功。
	itemB := session.Session{
		ID: "s2", UID: "demo:s2", AgentID: "demo",
		Storage: session.StorageSQLite, DBPath: dbB,
	}
	res, err = f.cleaner.Delete(spec, f.home, []session.Session{itemB})
	if err != nil {
		t.Fatalf("第二次 Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("第二次删除 Count = %d, Errors = %+v", res.Count, res.Errors)
	}
	if got := sessionRowsInDB(t, dbB); got != 0 {
		t.Fatalf("第二个库应已删除，剩 %d 行", got)
	}
}

// TestDeleteSQLiteLegacySessionWithoutDBPath 是兼容性对照：旧内存/旧 journal
// 的会话没有 DBPath，仍按 Store 声明的单路径删除。
func TestDeleteSQLiteLegacySessionWithoutDBPath(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := filepath.Join(f.home, "store.db")
	writeCleanDBFixture(t, dbPath, "s1")

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "store.db", Table: "sessions", IDCol: "id",
		}},
	}
	item := session.Session{
		ID: "s1", UID: "demo:s1", AgentID: "demo", Storage: session.StorageSQLite,
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{item})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("旧会话删除 Count = %d, Errors = %+v", res.Count, res.Errors)
	}
	if got := sessionRowsInDB(t, dbPath); got != 0 {
		t.Fatalf("旧路径下应已删除，剩 %d 行", got)
	}
}

// TestDeleteSQLiteMultiDBBatchAndUndo 一次批次里删两个不同库的会话：
// journal 必须每个库各记一条，Undo 要能把两个库分别从各自备份还原。
func TestDeleteSQLiteMultiDBBatchAndUndo(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbA := filepath.Join(f.home, "chats", "a", "store.db")
	dbB := filepath.Join(f.home, "chats", "b", "store.db")
	writeCleanDBFixture(t, dbA, "s1")
	writeCleanDBFixture(t, dbB, "s2")

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "chats/*/store.db", Table: "sessions", IDCol: "id",
		}},
	}
	items := []session.Session{
		{ID: "s1", UID: "demo:s1", AgentID: "demo", Storage: session.StorageSQLite, DBPath: dbA},
		{ID: "s2", UID: "demo:s2", AgentID: "demo", Storage: session.StorageSQLite, DBPath: dbB},
	}
	res, err := f.cleaner.Delete(spec, f.home, items)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 2 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}
	if sessionRowsInDB(t, dbA) != 0 || sessionRowsInDB(t, dbB) != 0 {
		t.Fatal("两个库的目标行都应被删除")
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 2 {
		t.Fatalf("journal sqlite = %d，want 2（每个库一条）", len(j.SQLite))
	}
	seen := map[string]bool{j.SQLite[0].DB: true, j.SQLite[1].DB: true}
	if !seen[dbA] || !seen[dbB] {
		t.Fatalf("journal 里的库路径 = %v，want 含 %s 与 %s", seen, dbA, dbB)
	}

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("Undo errors: %+v", undo.Errors)
	}
	if got := sessionRowsInDB(t, dbA); got != 1 {
		t.Fatalf("撤销后库 A 行数 = %d，want 1", got)
	}
	if got := sessionRowsInDB(t, dbB); got != 1 {
		t.Fatalf("撤销后库 B 行数 = %d，want 1", got)
	}
}
