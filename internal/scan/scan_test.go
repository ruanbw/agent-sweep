package scan

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// 夹具原则：每个用例都造出**真实布局的最小版本**，而不是造一个方便解析的假文件。
// 扫描器最容易错的地方是「路径对不对、旁挂文件带没带上」，这两件事只有真实布局能测出来。

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLSession(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "-Users-me-proj")
	writeFile(t, filepath.Join(project, "abc-123.jsonl"), `{"type":"user","cwd":"/Users/me/proj","gitBranch":"main","message":{"role":"user","content":"重构一下 registry"}}
{"type":"assistant","message":{"content":"好的"}}
`)
	// 旁挂目录：删除时必须跟会话一起走，否则磁盘上会剩一堆孤儿快照。
	if err := os.MkdirAll(filepath.Join(root, "file-history", "abc-123"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "file-history", "abc-123", "v1.bak"), "snapshot")

	spec := catalog.Spec{
		ID: "claude-code", Name: "Claude Code", Group: catalog.GroupCLI,
		Roots: []string{root},
		Stores: []catalog.Store{{
			Kind: "jsonl", Glob: "projects/*/*.jsonl",
			Assoc: []string{"/file-history/{id}"},
		}},
	}

	result := One(spec)
	if !result.Installed {
		t.Fatal("数据根存在，应判定为已安装")
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d", len(result.Sessions))
	}
	item := result.Sessions[0]
	if item.Title != "重构一下 registry" {
		t.Errorf("标题 = %q", item.Title)
	}
	if item.Project != "/Users/me/proj" {
		t.Errorf("项目 = %q", item.Project)
	}
	if len(item.Paths) != 2 {
		t.Fatalf("期望正文 + 快照两条路径，实际 %v", item.Paths)
	}
	if item.Bytes <= int64(len("snapshot")) {
		t.Errorf("体积应把旁挂快照算进去，实际 %d", item.Bytes)
	}
}

func TestDeduplicatesAcrossStores(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "projects", "p", "same.jsonl"), `{"message":{"role":"user","content":"hi"}}`)
	writeFile(t, filepath.Join(root, "sessions", "same.jsonl"), `{"message":{"role":"user","content":"hi"}}`)

	spec := catalog.Spec{
		ID: "x", Name: "X",
		Roots: []string{root},
		Stores: []catalog.Store{
			{Kind: "jsonl", Glob: "projects/*/*.jsonl"},
			{Kind: "jsonl", Glob: "sessions/*.jsonl"},
		},
	}
	result := One(spec)
	if len(result.Sessions) != 1 {
		t.Fatalf("同一 sessionId 在两个 Store 里只应出现一次，实际 %d", len(result.Sessions))
	}
}

func TestSQLiteSessions(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "threads.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, summary TEXT, updated_at INTEGER, folder_paths TEXT)`); err != nil {
		t.Fatal(err)
	}
	// 毫秒时间戳：各家最常见的一种。
	if _, err := db.Exec(`INSERT INTO threads VALUES ('t1','修 bug',1792119045599,'/Users/me/proj')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	spec := catalog.Spec{
		ID: "zed", Name: "Zed",
		Roots: []string{root},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "threads.db", Table: "threads",
			IDCol: "id", TitleCol: "summary", TimeCol: "updated_at", CWDCol: "folder_paths",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatal(result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 行，实际 %d", len(result.Sessions))
	}
	item := result.Sessions[0]
	if item.Title != "修 bug" || item.Project != "/Users/me/proj" {
		t.Errorf("读出来的字段不对：%+v", item)
	}
	if item.UpdatedAt.Year() < 2020 {
		t.Errorf("时间戳没按毫秒解析：%v", item.UpdatedAt)
	}
}

func TestMissingRootIsNotInstalled(t *testing.T) {
	spec := catalog.Spec{ID: "ghost", Name: "Ghost", Roots: []string{filepath.Join(t.TempDir(), "nope")}}
	result := One(spec)
	if result.Installed {
		t.Fatal("数据根不存在时应报未安装")
	}
	if len(result.Sessions) != 0 {
		t.Fatal("未安装不应返回会话")
	}
}
