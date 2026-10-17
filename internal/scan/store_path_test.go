package scan

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// writeSessionsDB 在 dbPath 建一个最小的会话表，插一行 id='secret-row'。
func writeSessionsDB(t *testing.T, dbPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT, title TEXT)`,
		`INSERT INTO sessions (id, title) VALUES ('secret-row', '不该被读到')`,
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

// TestOneRejectsSQLitePathOutsideRoot 回归 data #19：store.DB 里的 `..` 会被
// filepath.Join Clean 掉，旧实现会以只读方式打开数据根之外的任意 SQLite。
func TestOneRejectsSQLitePathOutsideRoot(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// 数据根外面放一个真实库：如果路径校验失效，它的行会出现在扫描结果里。
	writeSessionsDB(t, filepath.Join(base, "escape.db"))

	spec := catalog.Spec{
		ID:    "evil",
		Name:  "Evil",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "../escape.db", Table: "sessions", IDCol: "id",
		}},
	}
	result := One(spec)

	if !strings.Contains(result.Err, "越出数据根") {
		t.Fatalf("越界路径必须被拒绝并说明原因，Err = %q", result.Err)
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("越界库不该被打开，却扫出了 %d 条会话", len(result.Sessions))
	}
}

// TestOneRejectsAbsoluteSQLitePath 绝对路径同样拒绝，不因为「它指向一个真实
// 存在的库」而放行。
func TestOneRejectsAbsoluteSQLitePath(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "abs.db")
	writeSessionsDB(t, outside)

	spec := catalog.Spec{
		ID:    "evil-abs",
		Name:  "Evil Abs",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: outside, Table: "sessions", IDCol: "id",
		}},
	}
	result := One(spec)

	if !strings.Contains(result.Err, "绝对路径") {
		t.Fatalf("绝对路径必须被拒绝，Err = %q", result.Err)
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("绝对路径库不该被打开，却扫出了 %d 条会话", len(result.Sessions))
	}
}

// TestOneAcceptsSQLitePathInsideRoot 是防误杀的对照组：数据根内的相对路径
// 必须照常扫出行。
func TestOneAcceptsSQLitePathInsideRoot(t *testing.T) {
	home := t.TempDir()
	writeSessionsDB(t, filepath.Join(home, "sub", "ok.db"))

	spec := catalog.Spec{
		ID:    "ok",
		Name:  "OK",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "sub/ok.db", Table: "sessions", IDCol: "id", TitleCol: "title",
		}},
	}
	result := One(spec)

	if result.Err != "" {
		t.Fatalf("数据根内的库不该报错，Err = %q", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions = %d，want 1", len(result.Sessions))
	}
	if result.Sessions[0].ID != "secret-row" {
		t.Fatalf("扫出的会话 ID = %q，want secret-row", result.Sessions[0].ID)
	}
}
