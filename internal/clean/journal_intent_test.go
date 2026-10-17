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

// setJournalHook 替换「动作前」钩子，用例结束自动复原。
func setJournalHook(t *testing.T, fn func()) {
	t.Helper()
	original := journalBeforeActionHook
	journalBeforeActionHook = fn
	t.Cleanup(func() { journalBeforeActionHook = original })
}

// latestJournalBatchID 找出当前唯一一份 journal 的批次 ID（hook 里用它读账本）。
func latestJournalBatchID(t *testing.T, f *fixture) string {
	t.Helper()
	entries, err := os.ReadDir(f.journalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", f.journalDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			return strings.TrimSuffix(name, ".json")
		}
	}
	t.Fatalf("hook 触发时 journal 目录里没有账本: %s", f.journalDir)
	return ""
}

// TestTrashJournalWrittenBeforeMove 证明 trash 的顺序是「先记账、后移动」：
// hook 触发时 journal 里已有条目，而文件还在原位。
func TestTrashJournalWrittenBeforeMove(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sessions", "a.jsonl")
	writeTestFile(t, body, "body")

	checked := false
	setJournalHook(t, func() {
		if checked {
			return
		}
		checked = true
		batchID := latestJournalBatchID(t, f)
		j, err := f.cleaner.readJournal(batchID)
		if err != nil {
			t.Fatalf("hook 时读 journal: %v", err)
		}
		if len(j.Moved) != 1 {
			t.Fatalf("hook 时 journal.Moved = %d，want 1（动作前预写）", len(j.Moved))
		}
		if j.Moved[0].Orig != body {
			t.Fatalf("journal.Moved[0].Orig = %q，want %q", j.Moved[0].Orig, body)
		}
		if _, err := os.Lstat(body); err != nil {
			t.Fatalf("hook 时文件应仍在原位（动作尚未执行）: %v", err)
		}
	})

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home,
		[]session.Session{fileSession("a", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !checked {
		t.Fatal("动作前钩子没有触发")
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}

	// 正常完成后账本条目不变，文件已经进废纸篓。
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 1 {
		t.Fatalf("完成后 journal.Moved = %d，want 1", len(j.Moved))
	}
	if fsxExists(body) {
		t.Fatalf("文件应已移入废纸篓: %s", body)
	}
	if !fsxExists(j.Moved[0].Trashed) {
		t.Fatalf("废纸篓副本不存在: %s", j.Moved[0].Trashed)
	}
}

// TestSQLiteJournalWrittenBeforeDelete 证明 SQLite 的顺序同样是「先记账、后删行」：
// hook 时 journal 里已有该库条目（含备份与表名），而库里那行还在。
func TestSQLiteJournalWrittenBeforeDelete(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := filepath.Join(f.home, "agent.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT)`,
		`INSERT INTO sessions (id, title) VALUES ('s1', '标题')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	db.Close()

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "agent.db", Table: "sessions", IDCol: "id",
		}},
	}
	item := session.Session{
		ID: "s1", UID: "demo:s1", AgentID: "demo", Storage: session.StorageSQLite,
	}

	checked := false
	setJournalHook(t, func() {
		if checked {
			return
		}
		checked = true
		batchID := latestJournalBatchID(t, f)
		j, err := f.cleaner.readJournal(batchID)
		if err != nil {
			t.Fatalf("hook 时读 journal: %v", err)
		}
		if len(j.SQLite) != 1 {
			t.Fatalf("hook 时 journal.SQLite = %d，want 1", len(j.SQLite))
		}
		entry := j.SQLite[0]
		if entry.DB != dbPath {
			t.Fatalf("journal DB = %q，want %q", entry.DB, dbPath)
		}
		if !fsxExists(entry.Backup) {
			t.Fatalf("hook 时备份应已存在: %s", entry.Backup)
		}
		if len(entry.Deletes) != 1 || entry.Deletes[0].Table != "sessions" {
			t.Fatalf("hook 时 Deletes = %+v，want 一条 sessions 计划", entry.Deletes)
		}
		if len(entry.Deletes[0].IDs) != 1 || entry.Deletes[0].IDs[0] != "s1" {
			t.Fatalf("hook 时计划删除的 ID = %v，want [s1]", entry.Deletes[0].IDs)
		}
		// 动作尚未执行：库里那行还在。
		if got := countRowsInFixtureDB(t, dbPath, "sessions"); got != 1 {
			t.Fatalf("hook 时库里还有 %d 行，want 1（尚未删）", got)
		}
	})

	res, err := f.cleaner.Delete(spec, f.home, []session.Session{item})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !checked {
		t.Fatal("动作前钩子没有触发")
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}

	// 完成后账本仍是同一条，实际删掉的表/ID 与计划一致；行已经没了。
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 || len(j.SQLite[0].Deletes) != 1 {
		t.Fatalf("完成后 journal.SQLite = %+v，want 一条一条表记录", j.SQLite)
	}
	if got := countRowsInFixtureDB(t, dbPath, "sessions"); got != 0 {
		t.Fatalf("完成后库里还有 %d 行，want 0", got)
	}
}

// TestTrashFailureLeavesNoPlannedEntry 动作失败时账本不能留下「计划了但没做」的
// 条目：hook 里把废纸篓目录删掉，让 RenameUnder 失败。
func TestTrashFailureLeavesNoPlannedEntry(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sessions", "a.jsonl")
	writeTestFile(t, body, "body")

	fired := false
	setJournalHook(t, func() {
		if fired {
			return
		}
		fired = true
		batchID := latestJournalBatchID(t, f)
		trashRoot := filepath.Join(f.userHome, ".Trash", "AgentSweep", batchID)
		if err := os.RemoveAll(trashRoot); err != nil {
			t.Fatalf("删除废纸篓目录以制造失败: %v", err)
		}
	})

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home,
		[]session.Session{fileSession("a", body)})
	if err != nil {
		t.Fatalf("Delete 不应因单条移动失败而返回致命错误: %v", err)
	}
	if !fired {
		t.Fatal("动作前钩子没有触发")
	}
	if res.Count != 0 {
		t.Fatalf("移动失败不应计入 Count，got %d", res.Count)
	}
	if len(res.Errors) == 0 {
		t.Fatal("移动失败必须记 ItemError")
	}
	if !fsxExists(body) {
		t.Fatalf("移动失败后源文件应仍在原位: %s", body)
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 0 {
		t.Fatalf("失败后 journal.Moved 应为空（回滚预写条目），got %+v", j.Moved)
	}
}

// countRowsInFixtureDB 数测试库里的行数（只读）。
func countRowsInFixtureDB(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + quoteIdent(table)).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// fsxExists 是 os.Stat 的薄包装（与生产代码的 fsx.Exists 同名但测试里不引入包，
// 避免和已有 helper 冲突）。
func fsxExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
