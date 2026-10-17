package clean

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// TestSQLiteFootprint 覆盖主库 + WAL + SHM 的足迹统计。
func TestSQLiteFootprint(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent.db")

	if got := sqliteFootprint(dbPath); got != 0 {
		t.Fatalf("文件不存在时 footprint = %d，want 0", got)
	}

	if err := os.WriteFile(dbPath, []byte("1234567890"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := sqliteFootprint(dbPath); got != 10 {
		t.Fatalf("只有主库时 footprint = %d，want 10", got)
	}

	if err := os.WriteFile(dbPath+"-wal", []byte("12345"), 0o644); err != nil {
		t.Fatalf("WriteFile wal: %v", err)
	}
	if err := os.WriteFile(dbPath+"-shm", []byte("123"), 0o644); err != nil {
		t.Fatalf("WriteFile shm: %v", err)
	}
	if got := sqliteFootprint(dbPath); got != 18 {
		t.Fatalf("主库+WAL+SHM 时 footprint = %d，want 18", got)
	}
}

// TestFreedFromFootprint 钉住「只报正数」。
func TestFreedFromFootprint(t *testing.T) {
	cases := []struct {
		before, after, want int64
	}{
		{100, 40, 60},
		{40, 40, 0},
		{40, 100, 0}, // 并发写入导致变大：不报负数
	}
	for _, tc := range cases {
		if got := freedFromFootprint(tc.before, tc.after); got != tc.want {
			t.Errorf("freedFromFootprint(%d,%d) = %d，want %d", tc.before, tc.after, got, tc.want)
		}
	}
}

// TestSQLiteDeleteReportsNonNegativeFreed 确认 Delete 的 Freed 在 SQLite 路径上
// 不会出现负数，且删掉大量行后 VACUUM 缩库能被计入。
func TestSQLiteDeleteReportsNonNegativeFreed(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := filepath.Join(f.home, "bulk.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT)`); err != nil {
		db.Close()
		t.Fatalf("create: %v", err)
	}
	// 填充到能看到 VACUUM 缩库的体积；删掉其中大部分。
	const total, keep = 200, 20
	payload := strings.Repeat("x", 4096)
	items := make([]session.Session, 0, total-keep)
	for i := 0; i < total; i++ {
		id := "s" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i)
		if _, err := db.Exec(`INSERT INTO sessions (id, title) VALUES (?, ?)`, id, payload); err != nil {
			db.Close()
			t.Fatalf("insert: %v", err)
		}
		if i < keep {
			continue
		}
		items = append(items, session.Session{
			ID: id, UID: "demo:" + id, AgentID: "demo",
			Storage: session.StorageSQLite, DBPath: dbPath,
		})
	}
	db.Close()

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "bulk.db", Table: "sessions", IDCol: "id", TitleCol: "title",
		}},
	}
	res, err := f.cleaner.Delete(spec, f.home, items)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除不该报错: %+v", res.Errors)
	}
	if res.Count != len(items) {
		t.Fatalf("Count = %d，want %d", res.Count, len(items))
	}
	if res.Freed < 0 {
		t.Fatalf("Freed = %d，不能为负", res.Freed)
	}
	if res.Freed == 0 {
		t.Fatalf("删掉 %d 大行后 VACUUM 应缩库，Freed 不该是 0", len(items))
	}
}

// itoa 是给上面构造零填充 ID 用的最小实现（避免额外 import strconv 与现有 helper 冲突）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestPermanentPartialFailureAccounting 钉住 Freed/Count 的不同口径：
// 一条会话两根路径，第一根删成功、第二根因中间层链接失败 → Count=0、
// Freed 只含第一根的字节数。
func TestPermanentPartialFailureAccounting(t *testing.T) {
	f := newFixture(t, ModePermanent)
	content := "12345678"
	good := filepath.Join(f.home, "sessions", "a.jsonl")
	writeTestFile(t, good, content)

	// 中间层符号链接（指向 home 内部）：checkPathInHome 会放行，但 fd 相对
	// 删除拒绝中间层链接，正好制造「第二条路径失败」。
	realDir := filepath.Join(f.home, "real-dir")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	linkDir := filepath.Join(f.home, "link-dir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("当前文件系统不支持符号链接: %v", err)
	}
	bad := filepath.Join(linkDir, "b.jsonl")
	if err := os.WriteFile(filepath.Join(realDir, "b.jsonl"), []byte("bad"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	item := fileSession("a", good, bad)
	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{item})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("部分成功不应计入 Count，got %d", res.Count)
	}
	if len(res.Errors) == 0 {
		t.Fatal("失败的路径必须记 ItemError")
	}
	if res.Freed != int64(len(content)) {
		t.Fatalf("Freed = %d，want 只含第一条路径的 %d 字节", res.Freed, len(content))
	}
	if fsxExists(good) {
		t.Fatalf("成功的路径应已删除: %s", good)
	}
	if !fsxExists(bad) {
		t.Fatalf("失败路径对应的真实文件不该被删: %s", bad)
	}
}
