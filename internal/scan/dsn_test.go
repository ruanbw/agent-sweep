package scan

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// R8 回归：`?`、`#` 在 macOS 上是合法的文件名字符，裸路径拼进 `file:` URI 会被
// 截断或改变语义。这里既断言 DSN 字符串本身，也真开库验证 —— 只比字符串的话，
// 转义错了（比如少转 `#`，或把 mode=ro 也当成路径转义掉）仍然会「通过」。

const wantQuery = "mode=ro&_pragma=query_only(1)"

const fixtureSummary = "带 ? 和 # 的库"

func TestSQLiteDSN(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "普通路径",
			path: "/Users/me/Library/Application Support/zed/threads.db",
			want: "file:/Users/me/Library/Application%20Support/zed/threads.db?mode=ro&_pragma=query_only(1)",
		},
		{
			name: "无空格相对路径",
			path: "threads.db",
			want: "file:threads.db?mode=ro&_pragma=query_only(1)",
		},
		{
			name: "含问号",
			path: "/tmp/谁的库?1.db",
			want: "file:/tmp/%E8%B0%81%E7%9A%84%E5%BA%93%3F1.db?mode=ro&_pragma=query_only(1)",
		},
		{
			name: "含井号",
			path: "/tmp/a#b.db",
			want: "file:/tmp/a%23b.db?mode=ro&_pragma=query_only(1)",
		},
		{
			name: "含百分号",
			path: "/tmp/100%.db",
			want: "file:/tmp/100%25.db?mode=ro&_pragma=query_only(1)",
		},
		{
			name: "中文路径",
			path: "/tmp/会话 库.db",
			want: "file:/tmp/%E4%BC%9A%E8%AF%9D%20%E5%BA%93.db?mode=ro&_pragma=query_only(1)",
		},
		{
			name: "问号井号百分号混合",
			path: "/tmp/a?b#c%.db",
			want: "file:/tmp/a%3Fb%23c%25.db?mode=ro&_pragma=query_only(1)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqliteDSN(tt.path)
			if got != tt.want {
				t.Fatalf("sqliteDSN(%q)\n got: %s\nwant: %s", tt.path, got, tt.want)
			}

			// 独立的结构断言：整条 DSN 解析回来，path 必须和原路径逐字节一致。
			// 这样即使上面把期望值抄错了，转义的完整性仍然会被检查一遍。
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("DSN 不是合法 URL：%v", err)
			}
			if u.Scheme != "file" {
				t.Errorf("scheme = %q，期望 file", u.Scheme)
			}
			decoded := u.Path
			if decoded == "" {
				decoded = u.Opaque // 相对路径会被解析进 Opaque
			}
			if want := filepath.ToSlash(tt.path); decoded != want {
				t.Errorf("解析回来的路径 = %q，期望 %q", decoded, want)
			}
			if u.RawQuery != wantQuery {
				t.Errorf("query = %q，期望 %q", u.RawQuery, wantQuery)
			}
		})
	}
}

// seedSQLiteFixture 在 dir 下造一个真实 SQLite 库，再把字节写到 name 指定的
// 目标文件，返回目标路径。
//
// 两个细节都是必需的：
//   - 创建阶段先落到普通文件名再复制，因为目标名里就带 `?`/`#`；
//   - 创建时用裸文件名（不以 `file:` 开头）—— SQLite 只对 `file:` 前缀做 URI
//     解析，裸名按字面路径处理，所以连 dir 里有 `?`/`#` 也不会被截断。
//
// 被测的 sqliteDSN 本身不能用来建表：它是 mode=ro。
func seedSQLiteFixture(t *testing.T, dir, name string) string {
	t.Helper()
	scratch := filepath.Join(dir, "seed.db")
	db, err := sql.Open("sqlite", scratch)
	if err != nil {
		t.Fatalf("创建夹具库：%v", err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, summary TEXT, updated_at INTEGER, folder_paths TEXT)`); err != nil {
		t.Fatalf("建表：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES ('t1', ?, 1792119045599, '/Users/me/proj')`, fixtureSummary); err != nil {
		t.Fatalf("插行：%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭夹具库：%v", err)
	}

	data, err := os.ReadFile(scratch)
	if err != nil {
		t.Fatalf("读取夹具库：%v", err)
	}
	if err := os.Remove(scratch); err != nil {
		t.Fatalf("清理临时库：%v", err)
	}
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatalf("写入目标库 %q：%v", name, err)
	}
	return target
}

// TestSQLiteDSNOpensReservedCharacterFile 是真的开库验证：`?`、`#`、`%` 都在
// 文件名里时，用 sqliteDSN 的结果必须打开到那个真实文件并读到里面的行。
func TestSQLiteDSNOpensReservedCharacterFile(t *testing.T) {
	tests := []struct {
		label string
		name  string
	}{
		{"问号", "has?query.db"},
		{"井号", "has#hash.db"},
		{"混合", "has?both#and%.db"},
		{"中文", "会话 ?#库.db"},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			dir := t.TempDir()
			path := seedSQLiteFixture(t, dir, tt.name)

			db, err := sql.Open("sqlite", sqliteDSN(path))
			if err != nil {
				t.Fatalf("打开 %q：%v", path, err)
			}
			defer db.Close()

			var summary string
			if err := db.QueryRow(`SELECT summary FROM threads WHERE id = 't1'`).Scan(&summary); err != nil {
				t.Fatalf("查询 %q：%v（DSN 可能指向了别的文件或打不开）", path, err)
			}
			if summary != fixtureSummary {
				t.Fatalf("读到的 summary = %q，期望 %q", summary, fixtureSummary)
			}
		})
	}
}

// TestSQLiteDSNKeepsReadOnly 是回归：转义不能顺手把 mode=ro / query_only 弄丢。
func TestSQLiteDSNKeepsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := seedSQLiteFixture(t, dir, "plain.db")

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatalf("打开普通路径：%v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var queryOnly int
	if err := db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatalf("读 query_only：%v", err)
	}
	if queryOnly != 1 {
		t.Fatalf("PRAGMA query_only = %d，期望 1", queryOnly)
	}

	if _, err := db.Exec("DELETE FROM threads"); err == nil {
		t.Fatal("只读连接不应允许 DELETE")
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES ('t2','不该写进去',0,'/tmp')`); err == nil {
		t.Fatal("只读连接不应允许 INSERT")
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&count); err != nil {
		t.Fatalf("计数：%v", err)
	}
	if count != 1 {
		t.Fatalf("行数 = %d，期望 1（写入不应生效）", count)
	}
}

// TestScanSQLiteReservedCharacterPath 走完整的 scanSQLite 路径：证明修的是
// 调用点，而不只是新加的辅助函数。
func TestScanSQLiteReservedCharacterPath(t *testing.T) {
	dir := t.TempDir()
	seedSQLiteFixture(t, dir, "threads?#.db")

	spec := catalog.Spec{
		ID: "weird", Name: "Weird",
		Roots: []string{dir},
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "threads?#.db", Table: "threads",
			IDCol: "id", TitleCol: "summary", TimeCol: "updated_at", CWDCol: "folder_paths",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("扫描报错：%s", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d", len(result.Sessions))
	}
	if got := result.Sessions[0]; got.Title != fixtureSummary || got.Project != "/Users/me/proj" {
		t.Fatalf("读出来的字段不对：%+v", got)
	}
}
