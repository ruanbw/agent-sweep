package scan

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// writeDBGlobFixture 在 path 建一个最小会话库并插一行。
func writeDBGlobFixture(t *testing.T, path, id, title string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, updated_at TEXT)`,
		`INSERT INTO sessions (id, title, updated_at) VALUES ('` + id + `', '` + title + `', '2026-10-17T10:00:00Z')`,
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

// TestOneScansSQLiteDBGlob 覆盖「每个项目一个库」的 Agent：DB 声明成 glob
// 时要扫出所有库的会话，并且每条会话记得自己来自哪个库。
func TestOneScansSQLiteDBGlob(t *testing.T) {
	home := t.TempDir()
	dbA := filepath.Join(home, "chats", "a", "s1", "store.db")
	dbB := filepath.Join(home, "chats", "b", "s2", "store.db")
	writeDBGlobFixture(t, dbA, "s1", "项目 A 的会话")
	writeDBGlobFixture(t, dbB, "s2", "项目 B 的会话")

	spec := catalog.Spec{
		ID:    "cursor-agent",
		Name:  "Cursor Agent",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "chats/*/*/store.db",
			Table: "sessions", IDCol: "id", TitleCol: "title", TimeCol: "updated_at",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("扫描不该报错: %s", result.Err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("Sessions = %d，want 2（两个库各一条）", len(result.Sessions))
	}
	dbPathByID := map[string]string{}
	titleByID := map[string]string{}
	for _, item := range result.Sessions {
		dbPathByID[item.ID] = item.DBPath
		titleByID[item.ID] = item.Title
	}
	if dbPathByID["s1"] != dbA {
		t.Errorf("s1.DBPath = %q，want %q", dbPathByID["s1"], dbA)
	}
	if dbPathByID["s2"] != dbB {
		t.Errorf("s2.DBPath = %q，want %q", dbPathByID["s2"], dbB)
	}
	if titleByID["s1"] != "项目 A 的会话" || titleByID["s2"] != "项目 B 的会话" {
		t.Errorf("标题 = %v，want 各自库里的 title", titleByID)
	}
}

// TestOneSkipsNonFileDBGlobMatches 目录不是库，glob 命中也不能当库打开。
func TestOneSkipsNonFileDBGlobMatches(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "dbs", "real.db")
	writeDBGlobFixture(t, dbPath, "s1", "真的库")
	if err := os.MkdirAll(filepath.Join(home, "dbs", "not-a-db"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	spec := catalog.Spec{
		ID:    "demo",
		Name:  "Demo",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "dbs/*", Table: "sessions", IDCol: "id", TitleCol: "title", TimeCol: "updated_at",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("扫描不该报错: %s", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions = %d，want 1（目录被跳过）", len(result.Sessions))
	}
	if result.Sessions[0].DBPath != dbPath {
		t.Errorf("DBPath = %q，want %q", result.Sessions[0].DBPath, dbPath)
	}
}

// TestOneRejectsDBGlobOutsideRoot 带 .. 的 glob 声明不能把扫描带到数据根之外。
func TestOneRejectsDBGlobOutsideRoot(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeDBGlobFixture(t, filepath.Join(base, "outside", "x.db"), "sx", "外部库")

	spec := catalog.Spec{
		ID:    "evil",
		Name:  "Evil",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "../outside/*.db", Table: "sessions", IDCol: "id",
		}},
	}
	result := One(spec)
	if !strings.Contains(result.Err, "越出数据根") {
		t.Fatalf("越界 glob 必须被拒绝，Err = %q", result.Err)
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("越界库不该被打开，却扫出了 %d 条会话", len(result.Sessions))
	}
}
