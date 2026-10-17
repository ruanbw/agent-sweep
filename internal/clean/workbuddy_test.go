package clean

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

const (
	// WorkBuddy 的会话 ID 就是 <session-id>.jsonl 的文件名，也是 workbuddy.db
	// sessions 表的主键，两边必须一致才能对上。
	workbuddyTargetID = "6b1f0f0e-9d0a-4c2b-8e5a-3f7c1d2e4a01"
	workbuddyOtherID  = "6b1f0f0e-9d0a-4c2b-8e5a-3f7c1d2e4a02"
)

// seedWorkBuddyDB 按真实 schema 建索引库并插两行。
//
// 留一行「不该被删」的，是为了区分「按 ID 精确裁行」和「把表清空」——
// 后者也能让目标 id 消失，但会连累别的会话。
func seedWorkBuddyDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, updated_at TEXT, cwd TEXT)`,
		`INSERT INTO sessions (id, title, updated_at, cwd) VALUES
			('` + workbuddyTargetID + `', '要删的会话', '2026-10-17 10:00:00', '/Users/me/proj'),
			('` + workbuddyOtherID + `', '保留的会话', '2026-10-17 11:00:00', '/Users/me/proj')`,
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

// workbuddySpec 从内置目录取声明，而不是在测试里另抄一份配置。
func workbuddySpec(t *testing.T) catalog.Spec {
	t.Helper()
	for _, spec := range catalog.Builtin() {
		if spec.ID == "workbuddy" {
			return spec
		}
	}
	t.Fatalf("内置目录里没有 workbuddy，测试需随声明更新")
	return catalog.Spec{}
}

// countWorkBuddySessions 用只读连接数 sessions 表里指定 id 的行数。
func countWorkBuddySessions(t *testing.T, dbPath, id string) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count id=%s: %v", id, err)
	}
	return n
}

// TestWorkBuddyIndexTrimmedOnDelete 回归「索引登记成 Store」导致的会话复活。
//
// workbuddy.db 的 sessions 是三列都齐全的 SQLite 表，登记在 Stores 里扫描器
// 会把它当成一类独立的「会话」；可删除流程里的 SQLite 只处理用户选中的行
// （Storage == sqlite），删 jsonl 时根本不会碰它。于是正文没了、索引行还在，
// 下一轮扫描这条会话又出现，用户被迫删第二次 —— 第二次删的还是空壳。
//
// 登记成 Index 才会在正文删除后按已删 ID 裁行；这条用例跑完整删除流程，
// 断言 jsonl 与索引行一起消失，且只裁目标 ID。
func TestWorkBuddyIndexTrimmedOnDelete(t *testing.T) {
	// newFixture 内部已把 HOME 指到 t.TempDir()：本用例走 permanent，但同包
	// trash 用例有过漏进真实废纸篓的事故，隔离 HOME 是硬规矩。
	f := newFixture(t, ModePermanent)

	jsonlPath := filepath.Join(f.home, "projects", "p1", workbuddyTargetID+".jsonl")
	writeTestFile(t, jsonlPath, `{"type":"user","message":{"content":"hello"}}`+"\n")
	dbPath := filepath.Join(f.home, "workbuddy.db")
	seedWorkBuddyDB(t, dbPath)

	spec := workbuddySpec(t)
	s := session.Session{
		ID:      workbuddyTargetID,
		UID:     spec.ID + ":" + workbuddyTargetID,
		AgentID: spec.ID,
		Storage: session.StorageFile,
		Paths:   []string{jsonlPath},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{s})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	if fsx.Exists(jsonlPath) {
		t.Fatalf("正文仍在原位置: %s", jsonlPath)
	}

	// 核心断言：旧声明下这里会剩 1 行，正是「删完又复活」的根因。
	if got := countWorkBuddySessions(t, dbPath, workbuddyTargetID); got != 0 {
		t.Fatalf("索引行未裁掉（会话会复活），id=%s 还剩 %d 行", workbuddyTargetID, got)
	}
	if got := countWorkBuddySessions(t, dbPath, workbuddyOtherID); got != 1 {
		t.Fatalf("别的会话被误删，id=%s 剩 %d 行，want 1", workbuddyOtherID, got)
	}

	// 裁掉的索引必须进 journal：否则撤销只恢复 jsonl，索引里那条仍然空着。
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 {
		t.Fatalf("journal sqlite = %d, want 1", len(j.SQLite))
	}
	if len(j.SQLite[0].Deletes) != 1 {
		t.Fatalf("索引删除记录 = %d, want 1", len(j.SQLite[0].Deletes))
	}
	if j.SQLite[0].Deletes[0].Table != "sessions" {
		t.Fatalf("索引删除记录指向 %q，want sessions", j.SQLite[0].Deletes[0].Table)
	}
}
