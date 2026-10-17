package clean

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// SQLite 型会话的正文常常在库外面（Antigravity 的 brain/<id>）。
// 只删行不删目录，磁盘上会留一堆再也访问不到的正文 —— 这正是「删了但没释放」。
func TestSQLiteSessionRemovesSidecarDirs(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "conv.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES ('c1','标题')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	sidecar := filepath.Join(home, "brain", "c1")
	if err := os.MkdirAll(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "context.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := catalog.Spec{
		ID: "antigravity", Name: "Antigravity",
		Roots:  []string{home},
		Stores: []catalog.Store{{Kind: "sqlite", DB: "conv.db", Table: "conversation_summaries", IDCol: "conversation_id"}},
	}
	cleaner := New(Options{
		Mode:       ModePermanent,
		BackupDir:  filepath.Join(t.TempDir(), "backups"),
		JournalDir: filepath.Join(t.TempDir(), "journal"),
		Now:        func() time.Time { return time.Unix(1792119045, 0) },
	})
	result, err := cleaner.Delete(spec, home, []session.Session{{
		ID: "c1", UID: "antigravity:c1", AgentID: "antigravity",
		Storage: session.StorageSQLite, Paths: []string{sidecar},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 {
		t.Fatalf("Count = %d, want 1", result.Count)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatal("旁挂目录应随会话一起删除")
	}
	if result.Freed <= 0 {
		t.Fatalf("旁挂目录的体积应计入 Freed，实际 %d", result.Freed)
	}
}
