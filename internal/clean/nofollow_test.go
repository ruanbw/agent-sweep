package clean

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// dirSession 造一条「一个目录 = 一条会话」的内存会话。
func dirSession(id, dir string) session.Session {
	return session.Session{
		ID: id, UID: "demo:" + id, AgentID: "demo",
		Storage: session.StorageDir, Paths: []string{dir},
	}
}

// TestDeletePermanentRemovesNestedTree 确认改用 fd 相对删除后，普通目录型
// 会话仍然整棵删除（HOME 由 newFixture 隔离）。
func TestDeletePermanentRemovesNestedTree(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dir := filepath.Join(f.home, "sessions", "s1")
	writeTestFile(t, filepath.Join(dir, "a", "b", "deep.jsonl"), "deep")
	writeTestFile(t, filepath.Join(dir, "top.jsonl"), "top")

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home,
		[]session.Session{dirSession("s1", dir)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除不该报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d，want 1", res.Count)
	}
	if fsx.Exists(dir) {
		t.Fatalf("会话目录仍存在: %s", dir)
	}
}

// TestDeleteTrashMovesNestedTree 确认 trash 模式走 renameat 后仍能整目录移入
// 废纸篓（HOME 已隔离到 t.TempDir，不碰真实 ~/Trash）。
func TestDeleteTrashMovesNestedTree(t *testing.T) {
	f := newFixture(t, ModeTrash)
	dir := filepath.Join(f.home, "sessions", "s1")
	writeTestFile(t, filepath.Join(dir, "a", "deep.jsonl"), "deep")

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home,
		[]session.Session{dirSession("s1", dir)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除不该报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d，want 1", res.Count)
	}
	if fsx.Exists(dir) {
		t.Fatalf("会话目录仍在原位置: %s", dir)
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 1 {
		t.Fatalf("journal moved = %d，want 1", len(j.Moved))
	}
	if !fsx.Exists(j.Moved[0].Trashed) {
		t.Fatalf("废纸篓副本不存在: %s", j.Moved[0].Trashed)
	}
}

// TestDeleteKeepsSymlinkTargetOutside 是删除侧的 TOCTOU 回归：会话目录里有
// 指向数据根之外的符号链接时，只删链接，外部目标必须完好。
func TestDeleteKeepsSymlinkTargetOutside(t *testing.T) {
	f := newFixture(t, ModePermanent)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("不要删我"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dir := filepath.Join(f.home, "sessions", "s1")
	writeTestFile(t, filepath.Join(dir, "body.jsonl"), "body")
	if err := os.Symlink(victim, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("当前文件系统不支持符号链接: %v", err)
	}

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home,
		[]session.Session{dirSession("s1", dir)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}
	if fsx.Exists(dir) {
		t.Fatalf("会话目录仍存在: %s", dir)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "不要删我" {
		t.Fatalf("符号链接目标被动了: content=%q err=%v", got, err)
	}
}
