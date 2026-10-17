package clean

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// rowsValidateSessionID 是各用例里要被删掉的那条会话。
const rowsValidateSessionID = "s1"

// newRowsValidateDB 在 t.TempDir() 的 home 下建一个两表库：
// sessions 是主表，messages 通过 session_id 挂在它下面。
func newRowsValidateDB(t *testing.T, home string) string {
	t.Helper()
	dbPath := filepath.Join(home, "demo.db")
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, body TEXT)`,
		`INSERT INTO sessions (id, title) VALUES ('s1', '要删'), ('s2', '留着')`,
		`INSERT INTO messages (session_id, body) VALUES ('s1', 'm1'), ('s1', 'm2'), ('s2', 'm3')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath
}

// countRows 用只读连接数某张表的总行数。
func countRows(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + quoteIdent(table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// deleteWithSpec 用给定的 Store 声明跑一次真实删除，返回结果。
func deleteWithSpec(t *testing.T, f *fixture, store catalog.Store) Result {
	t.Helper()
	spec := catalog.Spec{ID: "demo", Stores: []catalog.Store{store}}
	s := session.Session{
		ID:      rowsValidateSessionID,
		UID:     "demo:" + rowsValidateSessionID,
		AgentID: "demo",
		Storage: session.StorageSQLite,
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{s})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	return res
}

// requireDeleteRejected 断言这次删除被拒绝、并且错误点名了写错的位置。
//
// SQLite 层面的失败按既有约定记进 Result.Errors（Delete 的顶层 error 只用于
// journal 写盘失败），所以这里断的是 ItemError 而不是 err。
func requireDeleteRejected(t *testing.T, res Result, wants ...string) {
	t.Helper()
	if len(res.Errors) == 0 {
		t.Fatalf("写错的声明没有产生任何错误：正是 DQS 静默空转的症状")
	}
	var all strings.Builder
	for _, itemErr := range res.Errors {
		all.WriteString(itemErr.Message)
		all.WriteString("\n")
	}
	msg := all.String()
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里没提到 %q，实际为：%s", want, msg)
		}
	}
}

// TestDeleteRejectsUnknownChildColumn 覆盖 Rows 写错列（表存在、列不存在）。
//
// 没有校验时这条声明会静默影响 0 行：主表行被删、messages 原样留着，
// 用户以为删干净了。校验失败必须让整个事务回滚，主表行还在。
func TestDeleteRejectsUnknownChildColumn(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := newRowsValidateDB(t, f.home)

	res := deleteWithSpec(t, f, catalog.Store{
		Kind: "sqlite", DB: "demo.db", Table: "sessions", IDCol: "id",
		Rows: []string{"messages:not_a_column"},
	})
	requireDeleteRejected(t, res, "messages", "not_a_column")

	if got := countRows(t, dbPath, "sessions"); got != 2 {
		t.Fatalf("校验失败应回滚，sessions 行数 = %d，want 2（主表行被先删了）", got)
	}
	if got := countRows(t, dbPath, "messages"); got != 3 {
		t.Fatalf("校验失败应回滚，messages 行数 = %d，want 3", got)
	}
}

// TestDeleteRejectsUnknownIDColumn 覆盖 IDCol 写错列。
//
// 主表自己的删除列写错时同样会静默 0 行 —— 用户勾了一堆会话，返回「成功」，
// 实际上一条都没删。必须在主表 DELETE 执行前就报出来。
func TestDeleteRejectsUnknownIDColumn(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := newRowsValidateDB(t, f.home)

	res := deleteWithSpec(t, f, catalog.Store{
		Kind: "sqlite", DB: "demo.db", Table: "sessions", IDCol: "not_a_column",
	})
	requireDeleteRejected(t, res, "sessions", "not_a_column")

	if got := countRows(t, dbPath, "sessions"); got != 2 {
		t.Fatalf("校验失败应回滚，sessions 行数 = %d，want 2（主表行被先删了）", got)
	}
}

// TestDeleteAcceptsValidColumns 是校验的对照组：正常声明不能被误杀。
func TestDeleteAcceptsValidColumns(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := newRowsValidateDB(t, f.home)

	res := deleteWithSpec(t, f, catalog.Store{
		Kind: "sqlite", DB: "demo.db", Table: "sessions", IDCol: "id",
		Rows: []string{"messages:session_id"},
	})
	if len(res.Errors) != 0 {
		t.Fatalf("正常声明不该报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	if got := countRows(t, dbPath, "sessions"); got != 1 {
		t.Fatalf("sessions 行数 = %d，want 1（s1 已删，s2 保留）", got)
	}
	if got := countRows(t, dbPath, "messages"); got != 1 {
		t.Fatalf("messages 行数 = %d，want 1（s1 的两行已删，s2 的保留）", got)
	}
}
