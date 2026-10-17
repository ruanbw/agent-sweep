package clean

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// anythingLLMFixture 按真实库的列建一份 AnythingLLM 数据库。
//
// 真实 workspace_threads 的列是 id/name/slug/workspace_id/user_id/createdAt/
// lastUpdatedAt，没有 thread_id。这里刻意不补这列：一旦为了「让删除跑通」把
// 库掰成声明想要的样子，用例就再也拦不住「声明指向真库不存在的列」这类错误。
func seedAnythingLLMDB(t *testing.T, dbPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE workspace_chats (id INTEGER PRIMARY KEY, prompt TEXT, createdAt DATETIME)`,
		`CREATE TABLE workspace_threads (
			id INTEGER PRIMARY KEY,
			name TEXT,
			workspace_id INTEGER,
			createdAt DATETIME,
			lastUpdatedAt DATETIME
		)`,
		`INSERT INTO workspace_chats (id, prompt, createdAt)
			VALUES (1, '要删的这一轮', '2026-10-17 10:00:00'), (2, '留着的一轮', '2026-10-17 11:00:00')`,
		`INSERT INTO workspace_threads (id, name, workspace_id, createdAt, lastUpdatedAt)
			VALUES (7, '示例线程', 1, '2026-10-17 10:00:00', '2026-10-17 10:00:00')`,
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

// anythingLLMSpecStore 从内置目录取声明，而不是在测试里另抄一份。
//
// 抄一份的话，改 builtin.go 却忘了改测试时用例照样绿 —— 它验证的就变成了
// 测试自己的副本，而不是用户真正跑的配置。
func anythingLLMSpecStore(t *testing.T) (catalog.Spec, catalog.Store) {
	t.Helper()
	for _, spec := range catalog.Builtin() {
		if spec.ID != "anythingllm" {
			continue
		}
		for _, store := range spec.Stores {
			if store.Kind == "sqlite" && store.Table == "workspace_chats" {
				return spec, store
			}
		}
		t.Fatalf("anythingllm 下找不到 workspace_chats 的 SQLite 存储，测试需随声明更新")
	}
	t.Fatalf("内置目录里没有 anythingllm")
	return catalog.Spec{}, catalog.Store{}
}

// assertRowsResolve 校验 Rows 里的每个 `table:fkcol` 都能在真实 schema 里解析到。
//
// 这是本用例的核心断言，因为「列不存在」不会以报错的形式暴露：execDelete 用
// quoteIdent 给列名加双引号，而 SQLite 默认开着「双引号字符串」兼容行为 ——
// 解析不到的 `"thread_id"` 会被降级成字符串字面量，`WHERE 'thread_id' IN ('1')`
// 恒假，DELETE 静默影响 0 行、事务照样提交。用户看到的是「删除成功」，账本里却
// 多了一条删过不存在的子表的记录。所以只能正面验证声明本身在真库上成立。
func assertRowsResolve(t *testing.T, dbPath string, rows []string) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()

	for _, declaration := range rows {
		table, col, ok := strings.Cut(declaration, ":")
		if !ok {
			t.Errorf("Rows 声明 %q 不是 table:column 形态", declaration)
			continue
		}
		cols, err := tableColumns(db, table)
		if err != nil {
			t.Errorf("读取表 %s 的列失败: %v", table, err)
			continue
		}
		if len(cols) == 0 {
			t.Errorf("Rows 声明 %q 指向的表在真实 schema 里不存在", declaration)
			continue
		}
		if !cols[col] {
			t.Errorf("Rows 声明 %q 的列在真实 schema 里不存在", declaration)
		}
	}
}

// tableColumns 返回一张表的列名集合；表不存在时返回空集合。
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + quoteIdent(table) + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// TestAnythingLLMDeleteWithRealSchema 回归 AnythingLLM 的子表声明。
//
// 旧声明 `workspace_threads:thread_id` 有两个问题：thread_id 这列在真库里不存在；
// 而且方向也反了 —— Rows 的语义是「子表里以会话 ID 为外键的列」，thread_id（若
// 存在）长在 workspace_chats 上指向 workspace_threads，不是 workspace_chats 的
// 子表外键。删掉这一项之后，删除应当只碰主表。
func TestAnythingLLMDeleteWithRealSchema(t *testing.T) {
	// 即使本用例走 permanent，也把 HOME 指到临时目录：同包 trash 用例出过一次
	// 把目录漏进真实 ~/Trash 的事故，隔离 HOME 是这里的硬规矩。
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)

	root := t.TempDir()
	cleaner := New(Options{
		Mode:       ModePermanent,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: filepath.Join(root, "journal"),
		Now:        func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) },
	})

	spec, store := anythingLLMSpecStore(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, store.DB)
	seedAnythingLLMDB(t, dbPath)

	// 先验证声明在真实 schema 上成立：旧代码在这里就会失败，错误信息直接
	// 点名哪个 table:column 在真库里不存在。
	assertRowsResolve(t, dbPath, store.Rows)

	s := session.Session{
		ID:      "1",
		UID:     "anythingllm:1",
		AgentID: spec.ID,
		Storage: session.StorageSQLite,
	}
	res, err := cleaner.Delete(spec, home, []session.Session{s})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除报错，行未落账: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}

	if got := countAnythingLLMChats(t, dbPath, 1); got != 0 {
		t.Fatalf("id=1 应已删除，实际还剩 %d 行", got)
	}
	if got := countAnythingLLMChats(t, dbPath, 2); got != 1 {
		t.Fatalf("id=2 不该被连带删除，实际剩 %d 行", got)
	}

	// 账本只该记主表。多出的那条即使影响 0 行也是有害的：撤销会照着它去
	// 恢复一个根本不存在的列，用户以为可撤销，实际恢复不了。
	j, err := cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 {
		t.Fatalf("journal sqlite = %d, want 1", len(j.SQLite))
	}
	if len(j.SQLite[0].Deletes) != 1 {
		t.Fatalf("删除记录 = %d, want 1（只应删主表）", len(j.SQLite[0].Deletes))
	}
	if j.SQLite[0].Deletes[0].Table != "workspace_chats" {
		t.Fatalf("删除记录指向 %q，want workspace_chats", j.SQLite[0].Deletes[0].Table)
	}
}

// countAnythingLLMChats 用只读连接数指定 id 的行数。
func countAnythingLLMChats(t *testing.T, dbPath string, id int) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workspace_chats WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count id=%d: %v", id, err)
	}
	return n
}
