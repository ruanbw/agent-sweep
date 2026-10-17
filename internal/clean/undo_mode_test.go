package clean

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// undoModeFixture 用 t.TempDir() 手写一份 journal 和它引用的文件，全部落在
// 临时目录里：不经过 Delete，不写 ~/.Trash，也不碰任何真实用户目录。
//
// journal 里故意同时带 Moved 与 Indexes 两类条目，这样「mode 校验必须在任何
// 磁盘操作之前」是可以被断言出来的 —— 少一次校验，废纸篓副本就会被挪回去。
type undoModeFixture struct {
	cleaner     *Cleaner
	batchID     string
	bodyPath    string
	trashedPath string
	indexPath   string
	backupPath  string
	journalPath string

	bodyContent   string
	indexContent  string
	backupContent string
}

func newUndoModeFixture(t *testing.T, mode string) *undoModeFixture {
	t.Helper()
	root := t.TempDir()
	batchID := "20261017-184759"
	now := time.Date(2026, 10, 17, 18, 47, 59, 0, time.UTC)

	f := &undoModeFixture{
		batchID:       batchID,
		bodyPath:      filepath.Join(root, "sessions", "sess.jsonl"),
		trashedPath:   filepath.Join(root, "trash", batchID, "sess.jsonl"),
		indexPath:     filepath.Join(root, "index.json"),
		backupPath:    filepath.Join(root, "backups", batchID, "index-1"),
		journalPath:   filepath.Join(root, "journal", batchID+".json"),
		bodyContent:   `{"role":"user","text":"请保留我"}` + "\n",
		indexContent:  `[{"sessionId":"sess"}]`,
		backupContent: `[{"sessionId":"sess"},{"sessionId":"other"}]`,
	}
	// 正文已离开原位（trash 模式在废纸篓里留副本；permanent/未知 mode 下这份
	// 副本本身不该存在，但把两个断言合在一个 fixture 里更能证明「什么都没动」）。
	writeUndoModeFile(t, f.trashedPath, f.bodyContent)
	// 索引已被裁剪，备份是删除前的完整快照。
	writeUndoModeFile(t, f.indexPath, f.indexContent)
	writeUndoModeFile(t, f.backupPath, f.backupContent)

	writeUndoModeJournal(t, filepath.Dir(f.journalPath), journal{
		BatchID: batchID,
		At:      now,
		Mode:    mode,
		Count:   1,
		Agents:  []string{"demo"},
		Moved:   []journalMoved{{Orig: f.bodyPath, Trashed: f.trashedPath}},
		Indexes: []journalBackup{{Orig: f.indexPath, Backup: f.backupPath}},
		SQLite:  []journalDB{},
	})

	f.cleaner = New(Options{
		// Undo 只认 journal 里的 Mode；Cleaner 自身的 Mode 与撤销判定无关。
		Mode:       ModeTrash,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: filepath.Join(root, "journal"),
		Now:        func() time.Time { return now },
	})
	return f
}

func writeUndoModeJournal(t *testing.T, journalDir string, j journal) {
	t.Helper()
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", journalDir, err)
	}
	if err := os.WriteFile(filepath.Join(journalDir, j.BatchID+".json"), data, 0o600); err != nil {
		t.Fatalf("写 journal: %v", err)
	}
}

func writeUndoModeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func readUndoModeFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

// trash 批次的成功路径必须仍然可用：拒绝 permanent 不能把正常撤销一起关掉。
func TestUndoTrashBatchRestores(t *testing.T) {
	f := newUndoModeFixture(t, ModeTrash)

	res, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("trash 批次应可撤销，got err: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("undo errors: %+v", res.Errors)
	}
	// 一份索引备份 + 一份废纸篓副本。
	if res.Restored != 2 {
		t.Fatalf("Restored = %d, want 2", res.Restored)
	}
	if got := readUndoModeFile(t, f.bodyPath); got != f.bodyContent {
		t.Fatalf("恢复后的正文内容不对: %q", got)
	}
	if got := readUndoModeFile(t, f.indexPath); got != f.backupContent {
		t.Fatalf("恢复后的索引不是删前快照: %s", got)
	}
	if _, err := os.Stat(f.trashedPath); !os.IsNotExist(err) {
		t.Fatalf("废纸篓副本应已被挪回，stat err = %v", err)
	}

	j, err := f.cleaner.readJournal(f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if !j.Undone {
		t.Fatal("成功撤销后 journal 应标记 undone")
	}
}

// permanent 批次的正文已经被 os.RemoveAll 删掉，还原索引只会得到半恢复状态。
func TestUndoRejectsPermanentBatch(t *testing.T) {
	f := newUndoModeFixture(t, ModePermanent)
	assertUndoModeRejected(t, f, ModePermanent)
}

// 未知 mode（包括旧 journal 里缺字段得到的空串）同样不能猜着还原。
func TestUndoRejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "archive"} {
		name := mode
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			f := newUndoModeFixture(t, mode)
			assertUndoModeRejected(t, f, mode)
		})
	}
}

func assertUndoModeRejected(t *testing.T, f *undoModeFixture, mode string) {
	t.Helper()
	journalBefore := readUndoModeFile(t, f.journalPath)

	res, err := f.cleaner.Undo(f.batchID)
	if err == nil {
		t.Fatalf("mode %q 的批次不应被撤销", mode)
	}
	if !strings.Contains(err.Error(), f.batchID) {
		t.Fatalf("错误应带上 batchID，got: %v", err)
	}
	if !strings.Contains(err.Error(), "删除模式") {
		t.Fatalf("错误应说明删除模式，got: %v", err)
	}
	if mode != "" && !strings.Contains(err.Error(), mode) {
		t.Fatalf("错误应带上 mode %q，got: %v", mode, err)
	}
	if !strings.Contains(err.Error(), "trash") {
		t.Fatalf("错误应说明只有 trash 可撤销，got: %v", err)
	}

	// 返回给 HTTP 层的 Errors 必须是空切片，能序列化成 []。
	if res.Errors == nil {
		t.Fatal("UndoResult.Errors 必须是空切片而不是 nil")
	}
	if len(res.Errors) != 0 {
		t.Fatalf("被拒绝时不应有逐项错误: %+v", res.Errors)
	}
	if res.Restored != 0 {
		t.Fatalf("被拒绝时 Restored = %d, want 0", res.Restored)
	}

	// journal 文件必须一字未改（不能被标记 Undone）。
	if got := readUndoModeFile(t, f.journalPath); got != journalBefore {
		t.Fatalf("被拒绝时 journal 不应被改写:\nbefore=%s\nafter=%s", journalBefore, got)
	}
	j, err := f.cleaner.readJournal(f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if j.Undone {
		t.Fatal("被拒绝时不应标记 Undone")
	}

	// 被引用的磁盘内容一律不得改动：索引、备份、废纸篓副本都保持原样，
	// 正文也不会被「还原」出来。
	if got := readUndoModeFile(t, f.indexPath); got != f.indexContent {
		t.Fatalf("索引被改动了:\nwant=%s\ngot=%s", f.indexContent, got)
	}
	if got := readUndoModeFile(t, f.backupPath); got != f.backupContent {
		t.Fatalf("索引备份被改动了:\nwant=%s\ngot=%s", f.backupContent, got)
	}
	if got := readUndoModeFile(t, f.trashedPath); got != f.bodyContent {
		t.Fatalf("废纸篓副本被改动了:\nwant=%s\ngot=%s", f.bodyContent, got)
	}
	if _, err := os.Stat(f.bodyPath); !os.IsNotExist(err) {
		t.Fatalf("正文不应被还原，stat err = %v", err)
	}
}
