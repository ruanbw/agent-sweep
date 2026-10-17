package clean

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// findBatch 从 Batches() 里取出指定批次；找不到直接失败。
func findBatch(t *testing.T, f *fixture, batchID string) Batch {
	t.Helper()
	batches, err := f.cleaner.Batches(100)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	for _, batch := range batches {
		if batch.ID == batchID {
			return batch
		}
	}
	t.Fatalf("Batches() 里找不到批次 %s", batchID)
	return Batch{}
}

// TestUndoRestoresBodiesBeforeIndexes 回归 J9：撤销顺序必须是「先正文、后索引」。
//
// 旧顺序先覆盖索引/数据库，再搬正文。正文恢复失败时（废纸篓副本被清、权限、
// 目标被占），索引已经指回一个不存在的正文 —— 正是删除流程反复避免的
// 「点进去是空的」幽灵会话。新顺序在正文没全部回来时整批跳过索引/DB。
func TestUndoRestoresBodiesBeforeIndexes(t *testing.T) {
	f := newFixture(t, ModeTrash)

	body1 := filepath.Join(f.home, "sessions", "s1.jsonl")
	body2 := filepath.Join(f.home, "sessions", "s2.jsonl")
	content1 := `{"type":"user","text":"一"}` + "\n"
	content2 := `{"type":"user","text":"二"}` + "\n"
	writeTestFile(t, body1, content1)
	writeTestFile(t, body2, content2)

	// 一个引用两条会话的 JSONL 索引，删除时会被裁掉两行并备份。
	indexPath := filepath.Join(f.home, "history.jsonl")
	writeTestFile(t, indexPath, `{"sessionId":"s1"}`+"\n"+`{"sessionId":"s2"}`+"\n")
	originalIndex := readTestFile(t, indexPath)

	spec := catalog.Spec{
		ID:     "demo",
		Stores: []catalog.Store{{Kind: "jsonl", Glob: "sessions/*.jsonl"}},
		Indexs: []catalog.Index{{Kind: "jsonl-lines", Glob: "history.jsonl", Key: "sessionId"}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{
		fileSession("s1", body1),
		fileSession("s2", body2),
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("删除不该报错: %+v", res.Errors)
	}

	trimmedIndex := readTestFile(t, indexPath)
	if trimmedIndex == originalIndex {
		t.Fatal("索引没有被裁掉，测试前提不成立")
	}
	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 2 {
		t.Fatalf("journal moved = %d，want 2（两条正文）", len(j.Moved))
	}
	if len(j.Indexes) != 1 {
		t.Fatalf("journal indexes = %d，want 1（一条索引备份）", len(j.Indexes))
	}

	// 人为弄丢 s1 的废纸篓副本：正文恢复必然失败。
	if err := os.Remove(j.Moved[0].Trashed); err != nil {
		t.Fatalf("删除废纸篓副本以便构造失败: %v", err)
	}

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if !fsx.Exists(body2) {
		t.Fatalf("s2 的正文应当被恢复: %s", body2)
	}
	if got := readTestFile(t, body2); got != content2 {
		t.Fatalf("s2 正文内容 = %q，want %q", got, content2)
	}
	if fsx.Exists(body1) {
		t.Fatalf("s1 的废纸篓副本已丢失，正文不该被恢复: %s", body1)
	}
	// 关键断言：正文阶段失败时，索引必须保持删除后的样子，不能指回没回来的正文。
	if got := readTestFile(t, indexPath); got != trimmedIndex {
		t.Fatalf("正文未全部恢复却还原了索引：\n got: %q\nwant: %q", got, trimmedIndex)
	}
	joined := strings.Join(undo.Errors, " | ")
	if !strings.Contains(joined, "废纸篓副本不存在") {
		t.Errorf("Errors 应包含正文恢复失败的具体原因，实际：%v", undo.Errors)
	}
	if !strings.Contains(joined, "已跳过索引与数据库还原") {
		t.Errorf("Errors 应说明索引/DB 被整批跳过，实际：%v", undo.Errors)
	}
	// 部分失败不标 Undone：用户修好副本后还能重试。
	if batch := findBatch(t, f, res.BatchID); !batch.Undoable {
		t.Fatalf("部分失败的批次必须仍可撤销（Undone=false），实际 Undoable=false")
	}

	// 把副本放回去再撤销一次：已恢复的 s2 应被安静跳过，整批收敛到 Undone。
	writeTestFile(t, j.Moved[0].Trashed, content1)
	undo, err = f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("第二次 Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("副本补齐后重试应当干净收敛，Errors = %v", undo.Errors)
	}
	if got := readTestFile(t, body1); got != content1 {
		t.Fatalf("s1 正文内容 = %q，want %q", got, content1)
	}
	if got := readTestFile(t, body2); got != content2 {
		t.Fatalf("s2 正文被重复搬运或改写：got %q", got)
	}
	if got := readTestFile(t, indexPath); got != originalIndex {
		t.Fatalf("重试成功后索引应还原成原样：\n got: %q\nwant: %q", got, originalIndex)
	}
	if batch := findBatch(t, f, res.BatchID); batch.Undoable {
		t.Fatal("整批撤销成功后批次不应再显示可撤销（Undone=true）")
	}
}
