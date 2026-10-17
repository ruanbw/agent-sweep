package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTree 造一个 root/<rel> 的嵌套文件，返回路径。
func makeTree(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestRemoveAllUnderRemovesNestedTree 覆盖普通目录/文件递归删除。
func TestRemoveAllUnderRemovesNestedTree(t *testing.T) {
	root := t.TempDir()
	makeTree(t, root, "a/b/one.txt", "1")
	makeTree(t, root, "a/b/c/two.txt", "2")
	makeTree(t, root, "keep.txt", "keep")

	target := filepath.Join(root, "a")
	if err := RemoveAllUnder(root, target); err != nil {
		t.Fatalf("RemoveAllUnder: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("目标目录仍存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatalf("数据根内的同级文件被误删: %v", err)
	}
}

// TestRemoveAllUnderUnlinksSymlinkOnly 覆盖「目录内有指向外部的符号链接」：
// 只删链接本身，外部目标必须完好。
func TestRemoveAllUnderUnlinksSymlinkOnly(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("不要删我"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dir := filepath.Join(root, "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if err := RemoveAllUnder(root, dir); err != nil {
		t.Fatalf("RemoveAllUnder: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("会话目录应已删除: %v", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "不要删我" {
		t.Fatalf("符号链接目标被动了: content=%q err=%v", got, err)
	}
}

// TestRemoveAllUnderRejectsSwappedIntermediateSymlink 是 TOCTOU 的核心回归：
// 校验期间 root/a 还是真目录，校验之后被换成指向外部的符号链接；
// fd 相对删除必须拒绝，外部目录原封不动。
func TestRemoveAllUnderRejectsSwappedIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	makeTree(t, root, "a/b/file.txt", "inside")
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(outsideFile, []byte("外部"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 模拟竞态：把真目录挪走，再用同名符号链接顶替。
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "a-moved")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	err := RemoveAllUnder(root, filepath.Join(root, "a", "b"))
	if err == nil {
		t.Fatal("中间层被换成符号链接时必须拒绝")
	}
	if !strings.Contains(err.Error(), "父目录") {
		t.Fatalf("错误应说明是父目录无法安全打开: %v", err)
	}
	if _, statErr := os.Stat(outsideFile); statErr != nil {
		t.Fatalf("外部文件被删了: %v", statErr)
	}
}

// TestRemoveAllUnderRejectsOutsidePath：词法上越界的路径直接拒绝。
func TestRemoveAllUnderRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "x.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := RemoveAllUnder(root, file); err == nil {
		t.Fatal("数据根之外的路径必须被拒绝")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("外部文件被删了: %v", err)
	}
}

// TestRenameUnderMovesFileAndDir 覆盖正常移动：文件与目录都走 renameat。
func TestRenameUnderMovesFileAndDir(t *testing.T) {
	root := t.TempDir()
	dst := t.TempDir()
	file := makeTree(t, root, "src/one.txt", "content")

	if err := RenameUnder(root, file, dst, "one.txt"); err != nil {
		t.Fatalf("RenameUnder file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "one.txt")); err != nil || string(got) != "content" {
		t.Fatalf("文件未正确移动: content=%q err=%v", got, err)
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Fatalf("源文件仍存在: %v", err)
	}

	dir := filepath.Join(root, "dir")
	makeTree(t, root, "dir/nested/two.txt", "2")
	if err := RenameUnder(root, dir, dst, "dir"); err != nil {
		t.Fatalf("RenameUnder dir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "dir", "nested", "two.txt")); err != nil || string(got) != "2" {
		t.Fatalf("目录未正确移动: content=%q err=%v", got, err)
	}
}

// TestRenameUnderRefusesExistingTarget 目标已存在时不覆盖。
func TestRenameUnderRefusesExistingTarget(t *testing.T) {
	root := t.TempDir()
	dst := t.TempDir()
	src := makeTree(t, root, "src.txt", "new")
	if err := os.WriteFile(filepath.Join(dst, "src.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := RenameUnder(root, src, dst, "src.txt"); err == nil {
		t.Fatal("目标已存在时必须报错")
	}
	if got, err := os.ReadFile(filepath.Join(dst, "src.txt")); err != nil || string(got) != "old" {
		t.Fatalf("目标被覆盖了: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("源文件不应被移走: %v", err)
	}
}

// TestRenameUnderRejectsSwappedIntermediateSymlink 移动侧同样拒绝中间层换链。
func TestRenameUnderRejectsSwappedIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	dst := t.TempDir()
	makeTree(t, root, "a/b/file.txt", "inside")
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "file.txt")
	if err := os.WriteFile(outsideFile, []byte("外部"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "a-moved")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if err := RenameUnder(root, filepath.Join(root, "a", "b", "file.txt"), dst, "stolen.txt"); err == nil {
		t.Fatal("中间层被换成符号链接时必须拒绝移动")
	}
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "外部" {
		t.Fatalf("外部文件被动了: content=%q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "stolen.txt")); !os.IsNotExist(err) {
		t.Fatalf("拒绝时不该在目标目录留下文件: %v", err)
	}
}
