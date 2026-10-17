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

// partialFixture 是本文件专用的最小 fixture。
//
// 不直接复用 clean_test.go / journal_persist_test.go 的 fixture：R11 要验证的是
// 「一条会话的多条路径是否被当成一个整体」，把 HOME、备份目录和 journal 目录
// 全部放进 t.TempDir()，测试之间就不可能互相污染，也不会碰到真实的 ~/.Trash。
type partialFixture struct {
	cleaner    *Cleaner
	home       string
	userHome   string
	backupDir  string
	journalDir string
	now        time.Time
}

func newPartialFixture(t *testing.T, mode string) *partialFixture {
	t.Helper()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	root := t.TempDir()
	now := time.Date(2026, 10, 17, 18, 47, 59, 0, time.UTC)
	backupDir := filepath.Join(root, "backups")
	journalDir := filepath.Join(root, "journal")
	return &partialFixture{
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

func partialWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func partialReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func partialSession(id string, paths ...string) session.Session {
	return session.Session{
		ID:      id,
		UID:     "demo:" + id,
		AgentID: "demo",
		Storage: session.StorageFile,
		Paths:   paths,
	}
}

// partialAssertJournalMatchesDisk 双向核对 journal.Moved 与磁盘现状：
//
//   - journal 里每条记录都必须真的还在废纸篓、且原位置确实为空；
//   - 给定路径里每一条要么「还在原位且没有记录」，要么「已离开原位且有记录」，
//     不允许出现「已经挪走但账本没记」或「账本记了但源文件还在」。
func partialAssertJournalMatchesDisk(t *testing.T, j *journal, paths []string) {
	t.Helper()
	recorded := map[string]string{} // orig -> trashed
	for _, moved := range j.Moved {
		if moved.Orig == "" || moved.Trashed == "" {
			t.Errorf("journal.Moved 记录不完整: %+v", moved)
			continue
		}
		if !fsx.Exists(moved.Trashed) {
			t.Errorf("journal 记录了废纸篓里不存在的副本: %+v", moved)
		}
		if fsx.Exists(moved.Orig) {
			t.Errorf("journal 记录 %s 已移动，但它仍在原位", moved.Orig)
		}
		recorded[moved.Orig] = moved.Trashed
	}
	for _, path := range paths {
		atOrig := fsx.Exists(path)
		_, inJournal := recorded[path]
		switch {
		case atOrig && inJournal:
			t.Errorf("路径 %s 仍在原位，journal 却记录它已移动", path)
		case !atOrig && !inJournal:
			t.Errorf("路径 %s 已离开原位，journal 却没有记录（撤销会漏掉它）", path)
		}
	}
}

// R11：一条会话的第 2 条路径注定失败时，整条会话必须保持原样。
//
// 采用「移动前预检」方案，所以这里断言的是「三条都还在原位」这一种状态；
// 任何「一半在原位、一半在废纸篓」的中间态都算失败。
func TestDeleteFileSessionIsAtomicWhenOnePathCannotMove(t *testing.T) {
	f := newPartialFixture(t, ModeTrash)

	paths := []string{
		filepath.Join(f.home, "sessions", "one", "session.jsonl"),
		filepath.Join(f.home, "sessions", "two", "session.jsonl"),
		filepath.Join(f.home, "sessions", "three", "session.jsonl"),
	}
	contents := []string{"one\n", "two\n", "three\n"}
	for i, path := range paths {
		partialWriteFile(t, path, contents[i])
	}

	// 第 2 条的父目录收回写权限：在不做预检的实现里，第 1 条会先被挪走，
	// 然后第 2 条失败，留下「一半在废纸篓」的中间态。
	blockedParent := filepath.Dir(paths[1])
	if err := os.Chmod(blockedParent, 0o555); err != nil {
		t.Fatalf("Chmod(%s, 0555): %v", blockedParent, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blockedParent, 0o755); err != nil {
			t.Errorf("恢复 %s 权限失败: %v", blockedParent, err)
		}
	})

	sess := partialSession("partial", paths...)
	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{sess})
	if err != nil {
		t.Fatalf("单条会话失败不应变成整个批次的致命错误: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("整条会话没有删成功，Count = %d, want 0", res.Count)
	}
	if len(res.Errors) == 0 {
		t.Fatalf("失败会话必须留下 ItemError")
	}
	for _, itemErr := range res.Errors {
		if itemErr.UID != sess.UID {
			t.Fatalf("ItemError.UID = %q, want %q", itemErr.UID, sess.UID)
		}
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Message, blockedParent) {
		t.Fatalf("应只有第 2 条路径的预检错误（提到 %s），got %+v", blockedParent, res.Errors)
	}

	// 方案是预检：失败发生在任何移动之前，三条都必须原样留在原位。
	for i, path := range paths {
		if !fsx.Exists(path) {
			t.Fatalf("第 %d 条路径被挪走了，整条会话没有保持原样: %s", i+1, path)
		}
		if got := partialReadFile(t, path); got != contents[i] {
			t.Fatalf("第 %d 条路径内容被改动: %q, want %q", i+1, got, contents[i])
		}
	}

	// 废纸篓里不应出现任何本会话的副本。
	trashRoot := filepath.Join(f.userHome, ".Trash", "AgentSweep", res.BatchID)
	entries, err := os.ReadDir(trashRoot)
	if err != nil {
		t.Fatalf("本批次废纸篓目录应已创建: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败的会话不应在废纸篓留下副本，got %v", entries)
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 0 {
		t.Fatalf("失败会话不应写入 moved 记录: %+v", j.Moved)
	}
	if j.Count != 0 {
		t.Fatalf("journal.Count = %d, want 0", j.Count)
	}
	partialAssertJournalMatchesDisk(t, j, paths)

	// 修好失败条件后重删：必须能一次性删掉整条，而不是「永远删不干净」。
	if err := os.Chmod(blockedParent, 0o755); err != nil {
		t.Fatalf("恢复 %s 权限失败: %v", blockedParent, err)
	}
	res2, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{partialSession("partial", paths...)})
	if err != nil {
		t.Fatalf("修复权限后 Delete: %v", err)
	}
	if len(res2.Errors) != 0 {
		t.Fatalf("修复权限后仍有错误: %+v", res2.Errors)
	}
	if res2.Count != 1 {
		t.Fatalf("修复后 Count = %d, want 1", res2.Count)
	}
	for i, path := range paths {
		if fsx.Exists(path) {
			t.Fatalf("修复后第 %d 条路径仍在原位: %s", i+1, path)
		}
	}

	j2, err := f.cleaner.readJournal(res2.BatchID)
	if err != nil {
		t.Fatalf("readJournal(batch2): %v", err)
	}
	if len(j2.Moved) != len(paths) {
		t.Fatalf("成功删除后 journal.Moved = %d, want %d", len(j2.Moved), len(paths))
	}
	partialAssertJournalMatchesDisk(t, j2, paths)

	// 副本内容也要对得上原文件，证明挪走的是同一条会话的三条路径。
	trashRoot2 := filepath.Join(f.userHome, ".Trash", "AgentSweep", res2.BatchID)
	byOrig := map[string]journalMoved{}
	for _, moved := range j2.Moved {
		byOrig[moved.Orig] = moved
	}
	for i, path := range paths {
		moved, ok := byOrig[path]
		if !ok {
			t.Fatalf("journal 缺少 %s 的移动记录: %+v", path, j2.Moved)
		}
		if !strings.HasPrefix(moved.Trashed, trashRoot2+string(filepath.Separator)) {
			t.Fatalf("副本 %s 不在本批次废纸篓 %s 下", moved.Trashed, trashRoot2)
		}
		if got := partialReadFile(t, moved.Trashed); got != contents[i] {
			t.Fatalf("第 %d 条副本内容 = %q, want %q", i+1, got, contents[i])
		}
	}
}

// R11 的回归线：永久删除模式明确不改语义，单条路径失败不影响其余路径，
// Count 也不会因为整条失败而 +1。这个用例防止把 trash 的原子性顺手套到
// permanent 上（那会让原本能删掉的路径也被保留）。
func TestDeletePermanentKeepsPerPathSemantics(t *testing.T) {
	f := newPartialFixture(t, ModePermanent)

	okPath := filepath.Join(f.home, "sessions", "ok", "session.jsonl")
	partialWriteFile(t, okPath, "ok\n")
	// 失败路径：文件躺在只读父目录里。RemoveAll 要 unlink，父目录没有写权限时
	// 返回 EACCES —— 注意不能指望「父目录不存在」，那种情况下 RemoveAll 会直接
	// 返回 nil（不存在 = 目标已达成），用例会假绿。
	badDir := filepath.Join(f.home, "sessions", "bad")
	badPath := filepath.Join(badDir, "session.jsonl")
	partialWriteFile(t, badPath, "bad\n")
	if err := os.Chmod(badDir, 0o555); err != nil {
		t.Fatalf("Chmod(%s): %v", badDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(badDir, 0o755) })

	sess := partialSession("mixed", okPath, badPath)
	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{sess})
	if err != nil {
		t.Fatalf("permanent 单条失败不应让整批返回致命错误: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("整条会话没有全部删成功，Count = %d, want 0", res.Count)
	}
	if len(res.Errors) != 1 || res.Errors[0].UID != sess.UID {
		t.Fatalf("应记录第 2 条路径的 ItemError，got %+v", res.Errors)
	}
	if fsx.Exists(okPath) {
		t.Fatalf("permanent 模式应继续删除其它路径，%s 仍在", okPath)
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 0 {
		t.Fatalf("permanent 模式不应写 moved 记录: %+v", j.Moved)
	}
}
