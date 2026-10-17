package clean

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// journalFixture 与 clean_test.go 的 fixture 同构，但完全自包含：
// 本文件只验证「journal 的落盘时机」，不依赖别的测试文件里的辅助函数。
type journalFixture struct {
	cleaner    *Cleaner
	home       string
	userHome   string
	backupDir  string
	journalDir string
	now        time.Time
}

func newJournalFixture(t *testing.T, mode string) *journalFixture {
	t.Helper()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	root := t.TempDir()
	now := time.Date(2026, 10, 17, 18, 47, 59, 0, time.UTC)
	backupDir := filepath.Join(root, "backups")
	journalDir := filepath.Join(root, "journal")
	return &journalFixture{
		cleaner: New(Options{
			Mode:       mode,
			BackupDir:  backupDir,
			JournalDir: journalDir,
			Now:        func() time.Time { return now },
		}),
		home:       t.TempDir(),
		userHome:   userHome,
		backupDir:  backupDir,
		journalDir: journalDir,
		now:        now,
	}
}

func writeJournalFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func readJournalFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func journalFixtureSession(id string, paths ...string) session.Session {
	return session.Session{
		ID:      id,
		UID:     "demo:" + id,
		AgentID: "demo",
		Storage: session.StorageFile,
		Paths:   paths,
	}
}

// 正常批量删除：journal 必须完整、可解析，Moved/Count 与 Result 对得上，
// 并且 Batches/Undo 能按同一份格式读回去。
func TestDeletePersistsJournalForCompletedBatch(t *testing.T) {
	f := newJournalFixture(t, ModeTrash)
	first := filepath.Join(f.home, "projects", "a", "first.jsonl")
	second := filepath.Join(f.home, "projects", "b", "second.jsonl")
	firstContent := `{"role":"user","text":"first"}` + "\n"
	secondContent := `{"role":"user","text":"second"}` + "\n"
	writeJournalFixtureFile(t, first, firstContent)
	writeJournalFixtureFile(t, second, secondContent)

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{
		journalFixtureSession("first", first),
		journalFixtureSession("second", second),
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Count != 2 {
		t.Fatalf("Count = %d, want 2", res.Count)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}

	journalPath := f.cleaner.journalPath(res.BatchID)
	if !fsx.Exists(journalPath) {
		t.Fatalf("删除完成后 journal 不在磁盘上: %s", journalPath)
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if j.BatchID != res.BatchID {
		t.Fatalf("journal.BatchID = %q, want %q", j.BatchID, res.BatchID)
	}
	if j.Mode != ModeTrash {
		t.Fatalf("journal.Mode = %q, want %q", j.Mode, ModeTrash)
	}
	if j.Count != 2 {
		t.Fatalf("journal.Count = %d, want 2", j.Count)
	}
	if len(j.Moved) != 2 {
		t.Fatalf("journal.Moved = %d, want 2", len(j.Moved))
	}

	// 每条 Moved 都要指回原路径，并且废纸篓副本内容完整；这是「反推不出
	// 原路径」的 bug 修复后，审计/撤销真正依赖的信息。
	byOrig := map[string]journalMoved{}
	for _, moved := range j.Moved {
		byOrig[moved.Orig] = moved
	}
	for _, tc := range []struct{ path, content string }{
		{first, firstContent},
		{second, secondContent},
	} {
		moved, ok := byOrig[tc.path]
		if !ok {
			t.Fatalf("journal.Moved 缺少原路径 %s: %+v", tc.path, j.Moved)
		}
		if moved.Trashed == "" || !fsx.Exists(moved.Trashed) {
			t.Fatalf("废纸篓副本不存在: %+v", moved)
		}
		if got := readJournalFixtureFile(t, moved.Trashed); got != tc.content {
			t.Fatalf("废纸篓副本内容 = %q, want %q", got, tc.content)
		}
	}

	batches, err := f.cleaner.Batches(10)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("Batches = %d, want 1", len(batches))
	}
	if batches[0].Count != 2 || !batches[0].Undoable {
		t.Fatalf("批次摘要不对: %+v", batches[0])
	}

	// 新写入时机不能改变 journal 格式：Undo 必须还能完整撤销。
	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("undo errors: %+v", undo.Errors)
	}
	if undo.Restored != 2 {
		t.Fatalf("Restored = %d, want 2", undo.Restored)
	}
	if got := readJournalFixtureFile(t, first); got != firstContent {
		t.Fatalf("first 恢复内容 = %q, want %q", got, firstContent)
	}
	if got := readJournalFixtureFile(t, second); got != secondContent {
		t.Fatalf("second 恢复内容 = %q, want %q", got, secondContent)
	}
}

// 中途失败：第 2 条路径越界，Delete 不应把它当致命错误，但第 1 条已经离开
// 原位的文件必须已经落在 journal 里。这正是进程崩溃后还能恢复/审计的依据。
func TestDeleteKeepsJournalWhenLaterSessionFails(t *testing.T) {
	f := newJournalFixture(t, ModeTrash)
	first := filepath.Join(f.home, "sessions", "first.jsonl")
	firstContent := `{"role":"user","text":"first"}` + "\n"
	writeJournalFixtureFile(t, first, firstContent)

	outside := t.TempDir()
	second := filepath.Join(outside, "second.jsonl")
	secondContent := `{"role":"user","text":"do not delete"}` + "\n"
	writeJournalFixtureFile(t, second, secondContent)

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{
		journalFixtureSession("first", first),
		journalFixtureSession("second", second),
	})
	if err != nil {
		t.Fatalf("单条路径越界不应让整批返回致命错误: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("越界会话必须留下且只留下一条 ItemError: %+v", res.Errors)
	}

	journalPath := f.cleaner.journalPath(res.BatchID)
	if !fsx.Exists(journalPath) {
		t.Fatalf("中途失败后 journal 仍必须存在: %s", journalPath)
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 1 {
		t.Fatalf("journal.Moved = %d, want 1（第 1 条已删，第 2 条越界）", len(j.Moved))
	}
	if j.Moved[0].Orig != first {
		t.Fatalf("Moved 记录的原路径 = %q, want %q", j.Moved[0].Orig, first)
	}
	if j.Count != 1 {
		t.Fatalf("journal.Count = %d, want 1", j.Count)
	}
	if !fsx.Exists(j.Moved[0].Trashed) {
		t.Fatalf("第 1 条的废纸篓副本不存在: %s", j.Moved[0].Trashed)
	}
	if got := readJournalFixtureFile(t, j.Moved[0].Trashed); got != firstContent {
		t.Fatalf("第 1 条副本内容 = %q, want %q", got, firstContent)
	}

	// 第 1 条确实已经离开原位，第 2 条则必须原封不动。
	if fsx.Exists(first) {
		t.Fatalf("第 1 条应已移入废纸篓，原位置仍在: %s", first)
	}
	if !fsx.Exists(second) {
		t.Fatalf("越界的第 2 条被删了: %s", second)
	}
	if got := readJournalFixtureFile(t, second); got != secondContent {
		t.Fatalf("第 2 条内容被改动: %q", got)
	}
}

// journal 写不进去时，必须在任何破坏性动作之前停手：文件还在、返回 error，
// 且错误里说明这是 journal 的问题。旧实现会先删文件、最后才写 journal，
// 这个用例在旧实现上会看到文件已经消失。
func TestDeleteDoesNotStartWhenInitialJournalWriteFails(t *testing.T) {
	f := newJournalFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sessions", "keep.jsonl")
	writeJournalFixtureFile(t, body, "must survive")

	// 先建好 journal 目录再收回写权限：reserveBatch 的 MkdirAll 对已存在的目录
	// 是 no-op，真正失败的是随后的初始 journal 写入。
	if err := os.MkdirAll(f.journalDir, 0o500); err != nil {
		t.Fatalf("MkdirAll(%s): %v", f.journalDir, err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(f.journalDir, 0o700)
	})

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{
		journalFixtureSession("keep", body),
	})
	if err == nil {
		t.Fatal("journal 写不进去时必须返回 error")
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Fatalf("错误应说明是 journal 写盘失败, got: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("未开始删除时 Count = %d, want 0", res.Count)
	}
	if !fsx.Exists(body) {
		t.Fatalf("journal 写不进去时不应开始删除，文件已消失: %s", body)
	}
	if got := readJournalFixtureFile(t, body); got != "must survive" {
		t.Fatalf("文件内容被改动: %q", got)
	}
	if res.BatchID == "" {
		t.Fatal("失败时仍应返回已预留的 batchID，便于定位空批次")
	}
	if fsx.Exists(f.cleaner.journalPath(res.BatchID)) {
		t.Fatalf("初始 journal 写入失败却存在 journal 文件: %s", f.cleaner.journalPath(res.BatchID))
	}
}
