package clean

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R12：撤销只有「整批成功」才允许标记 undone。
//
// 旧实现不管有没有失败都写 j.Undone = true，于是「一条恢复失败」会让整个批次在
// Batches() 里变成不可撤销：UI 收掉入口，用户即使修好权限/清出空间也无法重试，
// 只能手工搬文件。可失败的那些条目恰恰是可重试的 —— 已经恢复的条目在重试时会
// 命中「目标已存在且副本已不在」的已恢复分支，安静跳过。
//
// 本文件的用例全部落在 t.TempDir() 里（journal 的 Trashed 是显式路径），
// 不经过 Delete，也不碰真实 ~/.Trash。

// undoRetryFixture 是一个「一条正常、一条副本丢失」的批次。
type undoRetryFixture struct {
	f *undoPathFixture

	goodOrig    string
	goodTrashed string
	lostOrig    string
	lostTrashed string
}

// newUndoRetryFixture 手写 journal（含 Home），并只把 good 条目的废纸篓副本
// 真正落盘；lost 条目的 Trashed 故意指向不存在的文件，模拟「副本丢了」。
func newUndoRetryFixture(t *testing.T, batchID string) *undoRetryFixture {
	t.Helper()
	f := newUndoPathFixture(t, batchID)
	trash := filepath.Join(t.TempDir(), "trash")

	r := &undoRetryFixture{
		f:           f,
		goodOrig:    filepath.Join(f.home, "projects", "good", "keep.jsonl"),
		goodTrashed: filepath.Join(trash, "keep.jsonl"),
		lostOrig:    filepath.Join(f.home, "projects", "lost", "gone.jsonl"),
		lostTrashed: filepath.Join(trash, "gone.jsonl"),
	}
	undoPathWriteFile(t, r.goodTrashed, "good-content")

	f.saveJournal(t, journal{
		BatchID: f.batchID,
		At:      time.Date(2026, 10, 18, 1, 1, 0, 0, time.UTC),
		Mode:    ModeTrash,
		Home:    f.home,
		Count:   2,
		Agents:  []string{"demo"},
		// 次序无所谓：正常条目会成功，副本丢失的条目会失败，两者互不影响。
		Moved: []journalMoved{
			{Orig: r.lostOrig, Trashed: r.lostTrashed},
			{Orig: r.goodOrig, Trashed: r.goodTrashed},
		},
	})
	return r
}

// 部分失败：Errors 非空、只恢复了一条，且 journal 必须保持 Undone == false，
// 否则 Batches() 会当作已撤销而收掉撤销入口。
func TestUndoPartialFailureKeepsBatchRetryable(t *testing.T) {
	r := newUndoRetryFixture(t, "20261018-010101")

	res, err := r.f.cleaner.Undo(r.f.batchID)
	if err != nil {
		t.Fatalf("单条副本丢失不应让整次撤销返回致命错误: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("应只有副本丢失那一条进 Errors, got %d: %+v", len(res.Errors), res.Errors)
	}
	if !strings.Contains(res.Errors[0], "废纸篓副本不存在") {
		t.Fatalf("失败原因应是副本丢失, got: %s", res.Errors[0])
	}
	if res.Restored != 1 {
		t.Fatalf("Restored = %d, want 1（只有正常那条）", res.Restored)
	}

	// 好的那条确实回来了；坏的那条不能被凭空创建。
	if got := undoPathReadFile(t, r.goodOrig); got != "good-content" {
		t.Fatalf("正常条目未恢复, got %q", got)
	}
	if undoPathExists(r.lostOrig) {
		t.Fatalf("副本丢失的条目不应被创建: %s", r.lostOrig)
	}

	j, err := r.f.cleaner.readJournal(r.f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if j.Undone {
		t.Fatal("部分失败不应把整批标记为 Undone")
	}

	// 用户可见的后果：批次必须仍然是 undoable，否则 UI 不再提供重试入口。
	batches, err := r.f.cleaner.Batches(10)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("Batches = %d, want 1", len(batches))
	}
	if !batches[0].Undoable {
		t.Fatalf("部分失败的批次必须仍可撤销: %+v", batches[0])
	}
}

// 用户把缺失的副本补回去之后重试：剩下的条目被恢复，整批收敛为 Undone。
//
// 关键点是重试时已成功的条目会同时满足「目标已存在」和「副本不存在」，
// 这个组合是「上次已恢复」而不是冲突，必须安静跳过 —— 若它进了 Errors，
// 失败的批次永远无法通过重试变成 Undone。
func TestUndoRetryAfterRepairCompletesBatch(t *testing.T) {
	r := newUndoRetryFixture(t, "20261018-010102")

	first, err := r.f.cleaner.Undo(r.f.batchID)
	if err != nil {
		t.Fatalf("第一次 Undo: %v", err)
	}
	if len(first.Errors) == 0 {
		t.Fatal("fixture 应构造出一次部分失败")
	}

	// 模拟用户把丢失的废纸篓副本找回。
	undoPathWriteFile(t, r.lostTrashed, "lost-content")

	res, err := r.f.cleaner.Undo(r.f.batchID)
	if err != nil {
		t.Fatalf("修复后重试: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("重试时已恢复的条目应被安静跳过，不该再报错: %+v", res.Errors)
	}
	if res.Restored != 1 {
		t.Fatalf("重试只应恢复剩下的那一条, Restored = %d", res.Restored)
	}
	if got := undoPathReadFile(t, r.lostOrig); got != "lost-content" {
		t.Fatalf("补回副本后未恢复第二条, got %q", got)
	}
	// 已恢复的条目不能被重试覆盖或再次搬动。
	if got := undoPathReadFile(t, r.goodOrig); got != "good-content" {
		t.Fatalf("重试改动了已恢复的条目: %q", got)
	}
	if undoPathExists(r.goodTrashed) {
		t.Fatalf("已恢复条目的副本不该重新出现: %s", r.goodTrashed)
	}

	j, err := r.f.cleaner.readJournal(r.f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if !j.Undone {
		t.Fatal("全部恢复成功后应标记 Undone")
	}
	batches, err := r.f.cleaner.Batches(10)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(batches) != 1 || batches[0].Undoable {
		t.Fatalf("撤销完成后不应再可撤销: %+v", batches)
	}
}

// 全成功时行为照旧：标记 Undone，且重复撤销仍然安静返回（幂等）。
func TestUndoAllSuccessMarksUndoneAndIsIdempotent(t *testing.T) {
	f := newUndoPathFixture(t, "20261018-010103")
	trash := filepath.Join(t.TempDir(), "trash")

	origA := filepath.Join(f.home, "projects", "a", "one.jsonl")
	origB := filepath.Join(f.home, "projects", "b", "two.jsonl")
	trashedA := filepath.Join(trash, "one.jsonl")
	trashedB := filepath.Join(trash, "two.jsonl")
	undoPathWriteFile(t, trashedA, "content-a")
	undoPathWriteFile(t, trashedB, "content-b")

	f.saveJournal(t, journal{
		BatchID: f.batchID,
		At:      time.Date(2026, 10, 18, 1, 1, 3, 0, time.UTC),
		Mode:    ModeTrash,
		Home:    f.home,
		Count:   2,
		Moved: []journalMoved{
			{Orig: origA, Trashed: trashedA},
			{Orig: origB, Trashed: trashedB},
		},
	})

	res, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("undo errors: %+v", res.Errors)
	}
	if res.Restored != 2 {
		t.Fatalf("Restored = %d, want 2", res.Restored)
	}

	j, err := f.cleaner.readJournal(f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if !j.Undone {
		t.Fatal("全部成功后 journal 应标记 undone")
	}

	// 幂等：第二次调用必须安静返回，不报错、不重复搬文件。
	second, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("重复 Undo 应安静成功, got err: %v", err)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("重复 Undo 不应报错: %+v", second.Errors)
	}
	if second.Restored != 0 {
		t.Fatalf("重复 Undo Restored = %d, want 0", second.Restored)
	}
	if got := undoPathReadFile(t, origA); got != "content-a" {
		t.Fatalf("重复 Undo 改动了 origA: %q", got)
	}
	if got := undoPathReadFile(t, origB); got != "content-b" {
		t.Fatalf("重复 Undo 改动了 origB: %q", got)
	}
}
