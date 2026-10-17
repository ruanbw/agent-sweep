package clean

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// R29 回归：`?`、`#` 在 macOS 上是合法的文件名字符，而 modernc 驱动会把
// `file:` 开头的 DSN 当 URI 解析。裸路径拼进去会被截断或改变语义 —— 轻则
// 打不开（用户看到「0 条会话」或删除失败），重则打开到另一个库并修改它。
//
// 这里既逐字节断言 DSN，也真开库验证：只比字符串的话，转义写错了（比如漏了
// `#`，或把 `_pragma` 一起转义掉）仍然可能「通过」。

const (
	dsnFixtureTitle = "带 ? 和 # 的库"
	dsnWriteQuery   = "_pragma=busy_timeout(2000)"
)

// TestOpenDSN 断言写连接 DSN 的转义与参数。
func TestOpenDSN(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "普通路径",
			path: "/Users/me/Library/Application Support/zed/threads.db",
			want: "file:/Users/me/Library/Application%20Support/zed/threads.db?_pragma=busy_timeout(2000)",
		},
		{
			name: "无空格相对路径",
			path: "threads.db",
			want: "file:threads.db?_pragma=busy_timeout(2000)",
		},
		{
			name: "含问号",
			path: "/tmp/a?b.db",
			want: "file:/tmp/a%3Fb.db?_pragma=busy_timeout(2000)",
		},
		{
			name: "含井号",
			path: "/tmp/a#b.db",
			want: "file:/tmp/a%23b.db?_pragma=busy_timeout(2000)",
		},
		{
			name: "含百分号",
			path: "/tmp/100%.db",
			want: "file:/tmp/100%25.db?_pragma=busy_timeout(2000)",
		},
		{
			name: "中文路径",
			path: "/tmp/会话 库.db",
			want: "file:/tmp/%E4%BC%9A%E8%AF%9D%20%E5%BA%93.db?_pragma=busy_timeout(2000)",
		},
		{
			name: "问号井号百分号混合",
			path: "/tmp/a?b#c%.db",
			want: "file:/tmp/a%3Fb%23c%25.db?_pragma=busy_timeout(2000)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openDSN(tt.path)
			if got != tt.want {
				t.Fatalf("openDSN(%q)\n got: %s\nwant: %s", tt.path, got, tt.want)
			}
			// 写连接与只读连接的路径转义必须是同一份实现，否则修了一条调用链
			// 而另一条仍然按裸路径打开。
			if plain := sqliteDSN(tt.path, false); plain != got {
				t.Errorf("sqliteDSN(%q, false) = %s，期望与 openDSN 一致", tt.path, plain)
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
			if u.RawQuery != dsnWriteQuery {
				t.Errorf("query = %q，期望 %q", u.RawQuery, dsnWriteQuery)
			}
		})
	}
}

// seedSQLiteFixture 在 dir 下建一个真实的 SQLite 库，文件名就是 name。
//
// 先在安全文件名上建库再改名：实测 modernc v1.60.1 即使 DSN 不以 `file:`
// 开头，也会把 `?` 之后的内容当 query 解析（`has?q.db` 会落成 `has`），
// 所以带 `?` 的目标名不能直接拿来做建库路径。改名拿到的是同一份字节，
// 目标文件名里 `?`、`#`、`%` 都按字面存在。
func seedSQLiteFixture(t *testing.T, dir, name string) string {
	t.Helper()
	scratch := filepath.Join(dir, "seed.db")
	db, err := sql.Open("sqlite", scratch)
	if err != nil {
		t.Fatalf("创建夹具库 %q：%v", name, err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatalf("建表：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES ('s1', ?)`, dsnFixtureTitle); err != nil {
		t.Fatalf("插行：%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭夹具库：%v", err)
	}
	target := filepath.Join(dir, name)
	if err := os.Rename(scratch, target); err != nil {
		t.Fatalf("重命名夹具库为 %q：%v", name, err)
	}
	return target
}

// TestOpenDSNRealFileWithReservedChars 真的开库：文件名里带 `?`、`#`、`%` 时，
// 用被测 DSN 必须打开到那个真实文件并读到里面的行，而不是另一个库。
func TestOpenDSNRealFileWithReservedChars(t *testing.T) {
	tests := []struct {
		label string
		name  string
	}{
		{"问号", "has?query.db"},
		{"井号", "has#hash.db"},
		{"百分号", "100%done.db"},
		{"问号井号百分号混合", "has?both#and%.db"},
		{"中文与保留字符", "会话 ?#库.db"},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			dir := t.TempDir()
			path := seedSQLiteFixture(t, dir, tt.name)

			// 走生产调用点用的 sqliteDSN(path, false)，而不是只测 openDSN。
			db, err := sql.Open("sqlite", sqliteDSN(path, false))
			if err != nil {
				t.Fatalf("打开 %q：%v", path, err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)

			var title string
			if err := db.QueryRow(`SELECT title FROM sessions WHERE id = 's1'`).Scan(&title); err != nil {
				t.Fatalf("查询 %q：%v（DSN 可能指向了别的文件或打不开）", path, err)
			}
			if title != dsnFixtureTitle {
				t.Fatalf("读到的 title = %q，期望 %q", title, dsnFixtureTitle)
			}

			// 再确认写连接确实可写，而不只是能读。
			if _, err := db.Exec(`UPDATE sessions SET title = ? WHERE id = 's1'`, "改过"); err != nil {
				t.Fatalf("写 %q：%v", path, err)
			}
		})
	}
}

// TestOpenDSNKeepsBusyTimeout 是回归：转义不能顺手把 busy_timeout 弄丢，
// 普通路径下的打开也必须照旧可用。
func TestOpenDSNKeepsBusyTimeout(t *testing.T) {
	dir := t.TempDir()
	path := seedSQLiteFixture(t, dir, "plain.db")

	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("打开普通路径：%v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("读 busy_timeout：%v", err)
	}
	if busyTimeout != 2000 {
		t.Fatalf("PRAGMA busy_timeout = %d，期望 2000", busyTimeout)
	}

	var title string
	if err := db.QueryRow(`SELECT title FROM sessions WHERE id = 's1'`).Scan(&title); err != nil {
		t.Fatalf("读普通库：%v", err)
	}
	if title != dsnFixtureTitle {
		t.Fatalf("读到的 title = %q，期望 %q", title, dsnFixtureTitle)
	}
}

// TestSQLiteDSNKeepsReadOnly 是回归：readonly=true 时仍要带 mode=ro，
// 且路径转义与 busy_timeout 参数不能被覆盖掉。
func TestSQLiteDSNKeepsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := seedSQLiteFixture(t, dir, "plain.db")

	dsn := sqliteDSN(path, true)
	if want := openDSN(path) + "&mode=ro"; dsn != want {
		t.Fatalf("sqliteDSN(%q, true)\n got: %s\nwant: %s", path, dsn, want)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("只读打开：%v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("读 busy_timeout：%v", err)
	}
	if busyTimeout != 2000 {
		t.Fatalf("PRAGMA busy_timeout = %d，期望 2000", busyTimeout)
	}

	if _, err := db.Exec(`DELETE FROM sessions`); err == nil {
		t.Fatal("只读连接不应允许 DELETE")
	}
}
