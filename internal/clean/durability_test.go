package clean

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/fsx"
)

// J3：原子写在 rename 之后必须 fsync 父目录，否则掉电后目录项可能回退，
// journal 会「写了但没了」，等价于删除做了却没有账本。
//
// 这里能验证的只是「正常路径内容/权限正确且不报错」；目录 fsync 本身是否真的
// 生效无法从用户态断言，而 EINVAL/ENOTSUP/ENOSYS 的不支持分支也没有可移植的
// 构造手段（需要伪造一次目录 Sync 失败）。见实现里的 syncDir。

func TestWriteFileAtomicWritesContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	content := []byte(`{"batchId":"b1"}`)

	if err := fsx.WriteFileAtomic(path, content, 0o640); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(got) != string(content) {
		t.Fatalf("内容 = %q, want %q", got, content)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Fatalf("权限 = %o, want 640", perm)
	}

	// rename 成功后临时文件已被消费，目录里只应剩下目标文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "journal.json" {
		t.Fatalf("临时文件未清理干净，目录内容 = %v", entryNames(entries))
	}
}

func TestWriteFileAtomicReadOnlyParentLeavesNoTemp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 会绕过目录写权限，无法构造写入失败")
	}
	parent := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir(%s): %v", parent, err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatalf("Chmod(%s): %v", parent, err)
	}
	// t.Cleanup 后注册先执行：先恢复权限，TempDir 的清理才能删掉目录。
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	path := filepath.Join(parent, "journal.json")
	if err := fsx.WriteFileAtomic(path, []byte(`{}`), 0o600); err == nil {
		t.Fatalf("只读目录里的原子写应失败")
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("失败后不应留下目标文件, Lstat err = %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", parent, err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败后不应留下临时文件，目录内容 = %v", entryNames(entries))
	}
}

// J10：sidecar 删除失败必须返回给调用方，而不是被 _ = os.Remove 吞掉。
// 残留的新 WAL 在下次打开时可能重放到被覆盖的旧库上，静默会让风险不可见。

func TestRemoveSQLiteSidecarsRemovesAll(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "agent.db")
	durabilityWriteFile(t, db, "main")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		durabilityWriteFile(t, db+suffix, suffix)
	}

	if err := removeSQLiteSidecars(db); err != nil {
		t.Fatalf("全部 sidecar 可删时应返回 nil, got: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if durabilityExists(db + suffix) {
			t.Errorf("sidecar %s 未被删除", db+suffix)
		}
	}
	if !durabilityExists(db) {
		t.Fatalf("主库文件不应被 sidecar 清理删除")
	}
}

func TestRemoveSQLiteSidecarsReportsFailureAndContinues(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "agent.db")
	durabilityWriteFile(t, db, "main")
	// 非空目录会让 os.Remove 失败（ENOTEMPTY），用来模拟「sidecar 删不掉」。
	blocked := db + "-shm"
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("Mkdir(%s): %v", blocked, err)
	}
	durabilityWriteFile(t, filepath.Join(blocked, "occupied"), "x")
	durabilityWriteFile(t, db+"-wal", "wal")
	durabilityWriteFile(t, db+"-journal", "journal")

	err := removeSQLiteSidecars(db)
	if err == nil {
		t.Fatalf("不可删的 sidecar 必须返回错误")
	}
	if !strings.Contains(err.Error(), "-shm") {
		t.Fatalf("错误里应指出失败的 sidecar, got: %v", err)
	}
	// 一个失败不能短路其余 sidecar：否则撤销会把可清理的残留留在盘上。
	for _, suffix := range []string{"-wal", "-journal"} {
		if durabilityExists(db + suffix) {
			t.Errorf("失败后仍应尝试删除 %s", db+suffix)
		}
	}
	info, statErr := os.Stat(blocked)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("失败的 sidecar 应保持原状, Stat err = %v", statErr)
	}
}

func TestRemoveSQLiteSidecarsMissingIsNotAnError(t *testing.T) {
	db := filepath.Join(t.TempDir(), "never-existed.db")
	if err := removeSQLiteSidecars(db); err != nil {
		t.Fatalf("sidecar 不存在是正常情况, 应返回 nil, got: %v", err)
	}
}

// Undo 的集成路径：sidecar 删不掉时要进 UndoResult.Errors，且不能覆盖主库
// （覆盖后旧 WAL 一旦重放就是库损坏）。错误可见 + 不标记 undone，修好 sidecar
// 后重试仍能完成恢复。
func TestUndoSidecarFailureIsReportedAndRetryable(t *testing.T) {
	f := newUndoPathFixture(t, "20261018-000099")
	db := filepath.Join(f.home, "agent.db")
	backup := filepath.Join(t.TempDir(), "agent.db.bak")
	durabilityWriteFile(t, db, "current-main")
	durabilityWriteFile(t, backup, "backup-main")

	blocked := db + "-shm"
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("Mkdir(%s): %v", blocked, err)
	}
	durabilityWriteFile(t, filepath.Join(blocked, "occupied"), "x")

	f.saveJournal(t, journal{
		BatchID: f.batchID,
		Mode:    ModePermanent,
		Home:    f.home,
		SQLite:  []journalDB{{DB: db, Backup: backup}},
	})

	res, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("sidecar 删除失败不应让 Undo 返回致命错误: %v", err)
	}
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0], "sidecar") {
		t.Fatalf("sidecar 失败必须出现在 UndoResult.Errors 里, got: %+v", res.Errors)
	}
	if res.Restored != 0 {
		t.Fatalf("sidecar 未清掉时不应恢复主库, Restored = %d", res.Restored)
	}
	if got := durabilityReadFile(t, db); got != "current-main" {
		t.Fatalf("sidecar 未清掉时主库不应被覆盖, got %q", got)
	}
	j, err := f.cleaner.readJournal(f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if j.Undone {
		t.Fatalf("失败的批次不能标记 undone，否则无法重试")
	}

	// 修好 sidecar 后重试：同一条记录应能完成恢复。
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatalf("RemoveAll(%s): %v", blocked, err)
	}
	res, err = f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("重试 Undo: %v", err)
	}
	if len(res.Errors) != 0 || res.Restored != 1 {
		t.Fatalf("重试应恢复数据库, got Restored=%d Errors=%+v", res.Restored, res.Errors)
	}
	if got := durabilityReadFile(t, db); got != "backup-main" {
		t.Fatalf("重试后主库应恢复为备份内容, got %q", got)
	}
}

func durabilityWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func durabilityReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func durabilityExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
