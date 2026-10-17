package probe

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// makeQualitySQLite 在 t.TempDir() 里建一个库并执行给定语句，返回路径。
func makeQualitySQLite(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
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
	return path
}

// probeQualityFixture 用给足预算的 deadline 探一次。
func probeQualityFixture(t *testing.T, path string) *sqliteHit {
	t.Helper()
	return probeSQLite(path, time.Now().Add(2*time.Second))
}

// TestProbeSQLiteRejectsTableWithoutDisplayColumns 回归 data #13/#25：
// 表名像会话表、也有 id，但没有任何展示/时间/路径列 —— 普通应用的
// messages(id, body) 就是这种形状，不该被推荐成 Agent 适配器。
func TestProbeSQLiteRejectsTableWithoutDisplayColumns(t *testing.T) {
	path := makeQualitySQLite(t,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT)`,
		`INSERT INTO messages (body) VALUES ('普通应用的一条消息')`,
	)
	if hit := probeQualityFixture(t, path); hit != nil {
		t.Fatalf("只有 id 的 messages 表不该成为候选（草稿没有标题列可用）：%+v", hit)
	}
}

// TestProbeSQLiteAcceptsUsableSessionTable 是防误杀的对照组：id + title +
// updated_at 且有数据，三个条件都满足时必须给出正确的列映射。
func TestProbeSQLiteAcceptsUsableSessionTable(t *testing.T) {
	path := makeQualitySQLite(t,
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY, title TEXT, updated_at TEXT)`,
		`INSERT INTO sessions (id, title, updated_at) VALUES (1, '一条会话', '2026-10-17T10:00:00Z')`,
	)
	hit := probeQualityFixture(t, path)
	if hit == nil {
		t.Fatal("可用的会话表必须被接受")
	}
	if hit.table != "sessions" {
		t.Errorf("table = %q，want sessions", hit.table)
	}
	if hit.idCol != "id" {
		t.Errorf("idCol = %q，want id", hit.idCol)
	}
	if hit.titleCol != "title" {
		t.Errorf("titleCol = %q，want title", hit.titleCol)
	}
	if hit.timeCol != "updated_at" {
		t.Errorf("timeCol = %q，want updated_at", hit.timeCol)
	}
	if hit.rows != 1 {
		t.Errorf("rows = %d，want 1", hit.rows)
	}
}

// TestProbeSQLiteRejectsEmptyTable 零行的表不推荐：它没有任何会话，
// 选它只会给发现页添噪声；数行失败也归到同一分支。
func TestProbeSQLiteRejectsEmptyTable(t *testing.T) {
	path := makeQualitySQLite(t,
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY, title TEXT)`,
	)
	if hit := probeQualityFixture(t, path); hit != nil {
		t.Fatalf("空表不该成为候选：%+v", hit)
	}
}

// TestProbeSQLiteIgnoresNonSessionTable 顺手钉住既有行为：表名不在会话词表里
// 的表，连列检查都走不到。
func TestProbeSQLiteIgnoresNonSessionTable(t *testing.T) {
	path := makeQualitySQLite(t,
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`,
		`INSERT INTO settings (key, value) VALUES ('theme', 'dark')`,
	)
	if hit := probeQualityFixture(t, path); hit != nil {
		t.Fatalf("settings 表不该成为候选：%+v", hit)
	}
}
