package scan

import (
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// TestStatSkipNotice 钉住「哪些 Stat 失败该静默、哪些该提示」的分类。
func TestStatSkipNotice(t *testing.T) {
	const path = "/Users/me/.agent/projects/sess.jsonl"

	t.Run("ENOENT 静默", func(t *testing.T) {
		err := &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
		if _, ok := statSkipNotice(path, err); ok {
			t.Fatal("glob 与 Stat 之间被删是正常竞态，不该产生提示")
		}
	})

	t.Run("权限错误提示", func(t *testing.T) {
		err := &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
		notice, ok := statSkipNotice(path, err)
		if !ok {
			t.Fatal("权限错误意味着真实的信息丢失，必须提示")
		}
		if !strings.Contains(notice.text, path) {
			t.Errorf("提示应带上首例路径，实际：%s", notice.text)
		}
		if notice.key == "" {
			t.Error("提示必须有 key，否则无法去重")
		}
	})

	t.Run("其它 IO 错误同样提示", func(t *testing.T) {
		err := errors.New("iCloud 文件尚未下载")
		notice, ok := statSkipNotice(path, err)
		if !ok {
			t.Fatal("非 ENOENT 的 IO 错误也意味着读不到文件，必须提示")
		}
		if !strings.Contains(notice.text, path) {
			t.Errorf("提示应带上首例路径，实际：%s", notice.text)
		}
	})

	t.Run("同一类错误聚合", func(t *testing.T) {
		err1 := &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
		err2 := &fs.PathError{Op: "stat", Path: "/Users/me/.agent/other/a.jsonl", Err: fs.ErrPermission}
		n1, _ := statSkipNotice(path, err1)
		n2, _ := statSkipNotice("/Users/me/.agent/other/a.jsonl", err2)
		if n1.key != n2.key {
			t.Errorf("同类错误应共用 key 以便聚合: %q vs %q", n1.key, n2.key)
		}
	})
}

// TestOneReportsRowsSkippedInSQLite 回归 data #10/#11 的 SQLite 一半：
// NULL id 的行一直被静默 continue，用户只会看到「少了几个会话」。
func TestOneReportsRowsSkippedInSQLite(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "s.db")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT, title TEXT)`,
		`INSERT INTO sessions (id, title) VALUES ('s1', '正常一行'), (NULL, '没有 ID')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	spec := catalog.Spec{
		ID:    "demo",
		Name:  "Demo",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "s.db", Table: "sessions", IDCol: "id",
		}},
	}
	result := One(spec)

	if len(result.Sessions) != 1 {
		t.Fatalf("正常行应被扫出，Sessions = %d", len(result.Sessions))
	}
	if result.Sessions[0].ID != "s1" {
		t.Fatalf("扫出的 ID = %q，want s1", result.Sessions[0].ID)
	}
	if !strings.Contains(result.Err, "缺少可用 ID") {
		t.Fatalf("NULL id 被跳过必须可见，Err = %q", result.Err)
	}
	if !strings.Contains(result.Err, "sessions") {
		t.Errorf("提示应点名是哪张表，Err = %q", result.Err)
	}
	if !strings.Contains(result.Err, "警告：") {
		t.Errorf("走 notices 通道的提示应带「警告：」前缀，Err = %q", result.Err)
	}
}
