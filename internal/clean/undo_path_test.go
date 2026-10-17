package clean

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// R10：撤销恢复废纸篓条目时必须重放删除时的越界护栏。
//
// 删除侧有 checkPathInHome，撤销侧此前完全没有校验：journal 只是一份可以被
// 改写（journal 目录权限、未来的导入/恢复接口、旧版本写入）的 JSON，里面的
// Orig 直接进 os.MkdirAll(0755) + movePath 就是一条「撤销即任意写」的路。
// 本文件的用例都把数据根与废纸篓放在 t.TempDir() 里，不触碰真实 ~/.Trash。

// undoPathFixture 是一份手写 journal 及其引用的磁盘状态。
type undoPathFixture struct {
	cleaner     *Cleaner
	batchID     string
	home        string
	journalPath string
}

// newUndoPathFixture 建一个批次目录，journal 由调用方通过 saveJournal 落盘。
func newUndoPathFixture(t *testing.T, batchID string) *undoPathFixture {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 10, 18, 0, 0, 1, 0, time.UTC)
	return &undoPathFixture{
		cleaner: New(Options{
			Mode:       ModeTrash,
			BackupDir:  filepath.Join(root, "backups"),
			JournalDir: filepath.Join(root, "journal"),
			Now:        func() time.Time { return now },
		}),
		batchID:     batchID,
		home:        filepath.Join(root, "home"),
		journalPath: filepath.Join(root, "journal", batchID+".json"),
	}
}

// saveJournal 把 journal 按内存里的 json tag 落盘。Home 为空时 omitempty 会
// 让 "home" 键整个消失，正好复现旧版本写出来的 journal。
func (f *undoPathFixture) saveJournal(t *testing.T, j journal) {
	t.Helper()
	if j.BatchID == "" {
		j.BatchID = f.batchID
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(f.journalPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(f.journalPath), err)
	}
	if err := os.WriteFile(f.journalPath, data, 0o600); err != nil {
		t.Fatalf("写 journal: %v", err)
	}
}

func (f *undoPathFixture) readJournalRaw(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.journalPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", f.journalPath, err)
	}
	return string(data)
}

func undoPathWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func undoPathReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func undoPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// 正常批次：journal 带正确 Home，Moved 条目都在 Home 内（包括需要重建中间
// 目录的路径），撤销必须照常成功。
func TestUndoRestoresMovedEntriesInsideJournalHome(t *testing.T) {
	f := newUndoPathFixture(t, "20261018-000001")
	trash := filepath.Join(t.TempDir(), "trash")

	origA := filepath.Join(f.home, "projects", "a", "one.jsonl")
	origB := filepath.Join(f.home, "projects", "b", "two.jsonl")
	// 恢复 origA 时中间目录已不存在，Undo 需要自己 MkdirAll。
	trashedA := filepath.Join(trash, "one.jsonl")
	trashedB := filepath.Join(trash, "two.jsonl")
	undoPathWriteFile(t, trashedA, "content-a")
	undoPathWriteFile(t, trashedB, "content-b")

	f.saveJournal(t, journal{
		BatchID: f.batchID,
		At:      time.Date(2026, 10, 18, 0, 0, 1, 0, time.UTC),
		Mode:    ModeTrash,
		Home:    f.home,
		Moved: []journalMoved{
			{Orig: origA, Trashed: trashedA},
			{Orig: origB, Trashed: trashedB},
		},
	})

	res, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("带正确 home 的批次应可撤销, got err: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("undo errors: %+v", res.Errors)
	}
	if res.Restored != 2 {
		t.Fatalf("Restored = %d, want 2", res.Restored)
	}
	if got := undoPathReadFile(t, origA); got != "content-a" {
		t.Fatalf("origA 内容 = %q, want %q", got, "content-a")
	}
	if got := undoPathReadFile(t, origB); got != "content-b" {
		t.Fatalf("origB 内容 = %q, want %q", got, "content-b")
	}
	for _, trashed := range []string{trashedA, trashedB} {
		if undoPathExists(trashed) {
			t.Fatalf("废纸篓副本应已被挪回: %s", trashed)
		}
	}

	j, err := f.cleaner.readJournal(f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if !j.Undone {
		t.Fatal("成功撤销后 journal 应标记 undone")
	}
}

// Delete 必须把自己用的数据根写进 journal，否则撤销侧拿到空 Home 只能拒绝。
func TestDeleteRecordsHomeInJournalAndUndoUsesIt(t *testing.T) {
	// HOME 指向临时目录：trash 模式的废纸篓落在 t.TempDir() 里，不碰真实 ~/.Trash。
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	now := time.Date(2026, 10, 18, 0, 0, 2, 0, time.UTC)
	c := New(Options{
		Mode:       ModeTrash,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: filepath.Join(root, "journal"),
		Now:        func() time.Time { return now },
	})
	home := filepath.Join(root, "home")
	body := filepath.Join(home, "projects", "p", "sess.jsonl")
	undoPathWriteFile(t, body, "keep-me")

	res, err := c.Delete(catalog.Spec{ID: "demo"}, home, []session.Session{{
		ID:      "sess",
		UID:     "demo:sess",
		AgentID: "demo",
		Storage: session.StorageFile,
		Paths:   []string{body},
	}})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}

	j, err := c.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if j.Home != home {
		t.Fatalf("journal.Home = %q, want %q", j.Home, home)
	}
	if len(j.Moved) != 1 {
		t.Fatalf("journal.Moved = %d, want 1", len(j.Moved))
	}
	// 端到端：Delete 写下的 home 足以让 Undo 重新通过护栏并恢复。
	undo, err := c.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("undo errors: %+v", undo.Errors)
	}
	if got := undoPathReadFile(t, body); got != "keep-me" {
		t.Fatalf("恢复后的正文 = %q, want %q", got, "keep-me")
	}
}

// 越界批次：journal 的 Orig 指向 Home 之外（绝对路径 + 字面 ../），Trashed 却
// 指向真实文件。被拒条目只能进 Errors，Home 外不得创建或覆盖任何东西，同一
// 批次里 Home 内的其它条目仍必须恢复。
func TestUndoRejectsMovedEntriesOutsideHome(t *testing.T) {
	f := newUndoPathFixture(t, "20261018-000003")
	outside := t.TempDir()
	trash := filepath.Join(t.TempDir(), "trash")

	// 外部已存在的文件：用来证明撤销没有覆盖它。
	keep := filepath.Join(outside, "keep.txt")
	undoPathWriteFile(t, keep, "do not touch")
	// 两条越界目标都不存在，确保不会在 pathExists 那一步就被「目标已存在」提前跳过，
	// 从而真正走到新加的护栏。
	escapeAbsolute := filepath.Join(outside, "deep", "nested", "evil.txt")
	escapeParent := filepath.Join(f.home, "..", "outside-sibling", "evil.txt")
	// 手写 "../" 而不是 filepath.Join（后者会做词法清理），保留真实的穿越形态。
	escapeDotDot := f.home + string(filepath.Separator) + ".." + string(filepath.Separator) + "outside-sibling" + string(filepath.Separator) + "evil.txt"

	goodOrig := filepath.Join(f.home, "sessions", "good.jsonl")
	trashedGood := filepath.Join(trash, "good.jsonl")
	trashedAbsolute := filepath.Join(trash, "evil-abs.jsonl")
	trashedDotDot := filepath.Join(trash, "evil-dotdot.jsonl")
	trashedKeep := filepath.Join(trash, "keep.jsonl")
	undoPathWriteFile(t, trashedGood, "good-content")
	undoPathWriteFile(t, trashedAbsolute, "evil-absolute")
	undoPathWriteFile(t, trashedDotDot, "evil-dotdot")
	undoPathWriteFile(t, trashedKeep, "overwrite-attempt")

	f.saveJournal(t, journal{
		BatchID: f.batchID,
		At:      time.Date(2026, 10, 18, 0, 0, 3, 0, time.UTC),
		Mode:    ModeTrash,
		Home:    f.home,
		Moved: []journalMoved{
			{Orig: escapeAbsolute, Trashed: trashedAbsolute},
			// 好条目夹在坏条目中间：无论正序还是逆序，都要求「单条失败不中断」。
			{Orig: goodOrig, Trashed: trashedGood},
			{Orig: escapeDotDot, Trashed: trashedDotDot},
			// 目标已存在于 Home 之外：护栏必须先于「目标已存在」判定生效，
			// 否则错误信息会掩盖越界事实，且行为取决于文件系统当前状态。
			{Orig: keep, Trashed: trashedKeep},
		},
	})

	res, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("单条越界不应让整次撤销返回致命错误: %v", err)
	}
	if len(res.Errors) != 3 {
		t.Fatalf("三条越界条目都应进 Errors, got %d: %+v", len(res.Errors), res.Errors)
	}
	for _, e := range res.Errors {
		if !strings.Contains(e, "数据根之外") {
			t.Fatalf("拒绝原因应说明越界（即使目标已存在）, got: %s", e)
		}
	}
	if res.Restored != 1 {
		t.Fatalf("Restored = %d, want 1（仅 Home 内那条）", res.Restored)
	}

	// Home 内条目仍被恢复 —— 单条被拒不能中断其余恢复。
	if got := undoPathReadFile(t, goodOrig); got != "good-content" {
		t.Fatalf("Home 内条目未恢复, got %q", got)
	}
	if undoPathExists(trashedGood) {
		t.Fatalf("Home 内条目的废纸篓副本应已被挪回: %s", trashedGood)
	}

	// Home 外：文件不得被创建，连中间目录都不该被 MkdirAll 造出来。
	if undoPathExists(escapeAbsolute) {
		t.Fatalf("数据根之外的路径被创建了: %s", escapeAbsolute)
	}
	if undoPathExists(filepath.Dir(filepath.Dir(escapeAbsolute))) {
		t.Fatalf("越界条目不应创建目标目录: %s", filepath.Dir(filepath.Dir(escapeAbsolute)))
	}
	if undoPathExists(escapeParent) || undoPathExists(escapeDotDot) {
		t.Fatalf("字面 ../ 越界路径被创建了: %s", escapeDotDot)
	}
	if undoPathExists(filepath.Join(f.home, "..", "outside-sibling")) {
		t.Fatalf("越界条目不应创建 home 的兄弟目录")
	}
	// 外部既有文件既不能被覆盖，内容也不能变。
	if got := undoPathReadFile(t, keep); got != "do not touch" {
		t.Fatalf("数据根之外的文件被覆盖了: %q", got)
	}
	// 被拒条目的废纸篓副本必须原封不动，否则文件会在被拒时凭空消失。
	for _, trashed := range []string{trashedAbsolute, trashedDotDot} {
		if !undoPathExists(trashed) {
			t.Fatalf("被拒条目的废纸篓副本不应被移动: %s", trashed)
		}
	}
}

// 中间层 symlink：Home 内某段是符号链接指向外部，Orig 词法上仍在 Home 内。
// 词法层必须放行、真实路径层必须拒绝，以此证明 checkPathInHome 真的是被调用
// 的那一层，而不是只做了字符串判断。
func TestUndoRejectsSymlinkedParentEscape(t *testing.T) {
	f := newUndoPathFixture(t, "20261018-000004")
	outside := t.TempDir()
	trash := filepath.Join(t.TempDir(), "trash")

	linkDir := filepath.Join(f.home, "projects")
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", f.home, err)
	}
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Skipf("当前文件系统不支持符号链接: %v", err)
	}
	orig := filepath.Join(linkDir, "sess.jsonl")
	trashed := filepath.Join(trash, "sess.jsonl")
	undoPathWriteFile(t, trashed, "must-not-move")

	// 词法层认为这是 Home 内的合法路径；只有真实路径层能看出它落在外部。
	if err := checkPathInHomeLexical(f.home, orig); err != nil {
		t.Fatalf("词法层本应放行（否则此用例失去意义）: %v", err)
	}
	if err := checkPathInHome(f.home, orig); err == nil {
		t.Fatalf("checkPathInHome 应拒绝中间层 symlink 越界: %s", orig)
	}

	f.saveJournal(t, journal{
		BatchID: f.batchID,
		At:      time.Date(2026, 10, 18, 0, 0, 4, 0, time.UTC),
		Mode:    ModeTrash,
		Home:    f.home,
		Moved:   []journalMoved{{Orig: orig, Trashed: trashed}},
	})

	res, err := f.cleaner.Undo(f.batchID)
	if err != nil {
		t.Fatalf("单条 symlink 越界不应让整次撤销返回致命错误: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("symlink 越界必须记一条 error, got: %+v", res.Errors)
	}
	if res.Restored != 0 {
		t.Fatalf("Restored = %d, want 0", res.Restored)
	}
	if undoPathExists(filepath.Join(outside, "sess.jsonl")) {
		t.Fatalf("经由 symlink 在数据根之外创建了文件: %s", filepath.Join(outside, "sess.jsonl"))
	}
	if !undoPathExists(trashed) {
		t.Fatalf("被拒条目的废纸篓副本不应被移动: %s", trashed)
	}
}

// 旧格式 journal（没有 home 字段）：无法校验就 fail-closed，拒绝并给出明确
// 说明，且不得移动任何文件、不得标记 Undone。
func TestUndoRejectsLegacyJournalWithoutHome(t *testing.T) {
	f := newUndoPathFixture(t, "20261018-000005")
	trash := filepath.Join(t.TempDir(), "trash")
	orig := filepath.Join(f.home, "sessions", "sess.jsonl")
	trashed := filepath.Join(trash, "sess.jsonl")
	undoPathWriteFile(t, trashed, "legacy")

	// Home 留空：omitempty 会让磁盘上的 JSON 完全没有 "home" 键，与旧版本一致。
	f.saveJournal(t, journal{
		BatchID: f.batchID,
		At:      time.Date(2026, 10, 18, 0, 0, 5, 0, time.UTC),
		Mode:    ModeTrash,
		Moved:   []journalMoved{{Orig: orig, Trashed: trashed}},
	})
	raw := f.readJournalRaw(t)
	if strings.Contains(raw, `"home"`) {
		t.Fatalf("旧格式 fixture 不应包含 home 字段: %s", raw)
	}

	res, err := f.cleaner.Undo(f.batchID)
	if err == nil {
		t.Fatal("缺少 home 的旧格式 journal 必须被拒绝")
	}
	if !strings.Contains(err.Error(), "旧格式") {
		t.Fatalf("错误应说明是旧格式, got: %v", err)
	}
	if !strings.Contains(err.Error(), "home") {
		t.Fatalf("错误应说明缺少 home 字段, got: %v", err)
	}
	if !strings.Contains(err.Error(), "手工") {
		t.Fatalf("错误应给出人工处理建议, got: %v", err)
	}
	if res.Errors == nil {
		t.Fatal("UndoResult.Errors 必须是空切片而不是 nil")
	}
	if len(res.Errors) != 0 {
		t.Fatalf("致命拒绝时不应有逐项错误: %+v", res.Errors)
	}
	if res.Restored != 0 {
		t.Fatalf("Restored = %d, want 0", res.Restored)
	}

	// 不得移动任何文件，也不得创建目标。
	if !undoPathExists(trashed) {
		t.Fatalf("旧格式被拒时废纸篓副本不应被移动: %s", trashed)
	}
	if got := undoPathReadFile(t, trashed); got != "legacy" {
		t.Fatalf("废纸篓副本内容被改动: %q", got)
	}
	if undoPathExists(orig) {
		t.Fatalf("旧格式被拒时不应恢复正文: %s", orig)
	}
	// journal 必须一字未改（不能被标记 Undone）。
	if got := f.readJournalRaw(t); got != raw {
		t.Fatalf("被拒时 journal 不应被改写:\nbefore=%s\nafter=%s", raw, got)
	}
	j, err := f.cleaner.readJournal(f.batchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if j.Undone {
		t.Fatal("被拒时不应标记 Undone")
	}
}
