package clean

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// fixture 把测试里最容易写错的三件事一次固定下来：
//   - HOME 指向临时目录，避免真的往开发机的 ~/.Trash 里扔东西
//   - 备份/journal 与数据根分开，能验证「备份在应用私有目录」这条契约
//   - Now 固定，batchID 可预测，且能测同秒冲突
type fixture struct {
	cleaner    *Cleaner
	home       string
	userHome   string
	backupDir  string
	journalDir string
	now        time.Time
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	root := t.TempDir()
	now := time.Date(2026, 10, 17, 18, 47, 59, 0, time.UTC)
	backupDir := filepath.Join(root, "backups")
	journalDir := filepath.Join(root, "journal")
	return &fixture{
		cleaner:    New(Options{Mode: mode, BackupDir: backupDir, JournalDir: journalDir, Now: func() time.Time { return now }}),
		home:       t.TempDir(),
		userHome:   userHome,
		backupDir:  backupDir,
		journalDir: journalDir,
		now:        now,
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func fileSession(id string, paths ...string) session.Session {
	return session.Session{
		ID:      id,
		UID:     "demo:" + id,
		AgentID: "demo",
		Storage: session.StorageFile,
		Paths:   paths,
	}
}

func TestDeleteTrashMovesWithoutFreed(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "projects", "p", "abc.jsonl")
	content := `{"type":"user","text":"hello"}` + "\n"
	writeTestFile(t, body, content)

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{fileSession("abc", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	// 进废纸篓没有释放空间，Freed 必须是 0，否则 UI 会谎报战果。
	if res.Freed != 0 {
		t.Fatalf("trash Freed = %d, want 0", res.Freed)
	}
	if fsx.Exists(body) {
		t.Fatalf("正文仍在原位置: %s", body)
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 1 {
		t.Fatalf("journal moved = %d, want 1", len(j.Moved))
	}
	wantRoot := filepath.Join(f.userHome, ".Trash", "AgentSweep", res.BatchID)
	if !strings.HasPrefix(j.Moved[0].Trashed, wantRoot+string(filepath.Separator)) {
		t.Fatalf("trashed path = %s, want under %s", j.Moved[0].Trashed, wantRoot)
	}
	if got := readTestFile(t, j.Moved[0].Trashed); got != content {
		t.Fatalf("trashed content = %q, want %q", got, content)
	}
}

func TestDeletePermanentCountsFreed(t *testing.T) {
	f := newFixture(t, ModePermanent)
	body := filepath.Join(f.home, "sessions", "big.jsonl")
	content := strings.Repeat("x", 8192)
	writeTestFile(t, body, content)

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{fileSession("big", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if res.Freed < int64(len(content)) {
		t.Fatalf("Freed = %d, want >= %d", res.Freed, len(content))
	}
	if fsx.Exists(body) {
		t.Fatalf("永久删除后文件仍存在: %s", body)
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 0 {
		t.Fatalf("permanent 模式不应写 moved 记录，got %d", len(j.Moved))
	}
}

func TestJSONLIndexTrim(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sessions", "gone.jsonl")
	writeTestFile(t, body, `{"role":"user"}`+"\n")

	original := strings.Join([]string{
		`{"sessionId":"keep","display":"保留"}`,
		`{"sessionId":"gone","display":"删掉"}`,
		`this line is not json`,
		`{"sessionId":"keep-2","display":"也保留"}`,
	}, "\n") + "\n"
	indexPath := filepath.Join(f.home, "history.jsonl")
	writeTestFile(t, indexPath, original)

	onlyPath := filepath.Join(f.home, "only.jsonl")
	writeTestFile(t, onlyPath, `{"sessionId":"gone"}`+"\n")

	spec := catalog.Spec{
		ID: "demo",
		Indexs: []catalog.Index{
			{Kind: "jsonl-lines", Glob: "history.jsonl", Key: "sessionId"},
			{Kind: "jsonl-lines", Glob: "only.jsonl", Key: "sessionId"},
		},
	}

	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("gone", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}

	got := readTestFile(t, indexPath)
	if strings.Contains(got, `"sessionId":"gone"`) || strings.Contains(got, `"sessionId": "gone"`) {
		t.Fatalf("索引里的目标行没删掉:\n%s", got)
	}
	// 认不出的行必须原样保留，不能因为解析失败就顺手扔掉。
	if !strings.Contains(got, "this line is not json") {
		t.Fatalf("非法 JSON 行被误删:\n%s", got)
	}
	if !strings.Contains(got, `"keep"`) || !strings.Contains(got, `"keep-2"`) {
		t.Fatalf("无关行被误删:\n%s", got)
	}
	// 全部删空时保留空文件，而不是把文件删掉。
	if fsx.Exists(onlyPath) == false {
		t.Fatalf("全删空的 jsonl 索引文件不应被删除")
	}
	if data, _ := os.ReadFile(onlyPath); len(data) != 0 {
		t.Fatalf("全删空的 jsonl 索引应为空文件，got %q", string(data))
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Indexes) != 2 {
		t.Fatalf("journal indexes = %d, want 2", len(j.Indexes))
	}
	for _, backup := range j.Indexes {
		if !fsx.Exists(backup.Backup) {
			t.Fatalf("索引备份不存在: %s", backup.Backup)
		}
	}
	if got := readTestFile(t, indexPathBackup(t, j, indexPath)); got != original {
		t.Fatalf("备份内容不是删前快照")
	}
}

func indexPathBackup(t *testing.T, j *journal, orig string) string {
	t.Helper()
	for _, backup := range j.Indexes {
		if backup.Orig == orig {
			return backup.Backup
		}
	}
	t.Fatalf("journal 里找不到 %s 的备份", orig)
	return ""
}

func TestJSONArrayShapes(t *testing.T) {
	cases := []struct {
		name        string
		index       catalog.Index
		content     string
		wantRemoved bool
		wantPresent string
		wantAbsent  string
	}{
		{
			name:        "top-level array",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId"},
			content:     `[{"sessionId":"sess-a","title":"marker-a"},{"sessionId":"sess-b","title":"marker-b"}]`,
			wantPresent: "marker-b",
			wantAbsent:  "marker-a",
		},
		{
			name:        "wrapped array",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId", Path: "entries"},
			content:     `{"version":1,"entries":[{"sessionId":"sess-a","title":"marker-a"},{"sessionId":"sess-b","title":"marker-b"}]}`,
			wantPresent: "marker-b",
			wantAbsent:  "marker-a",
		},
		{
			name:        "sessions dictionary",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId", Path: "sessions"},
			content:     `{"sessions":{"sess-a":{"title":"marker-a"},"sess-b":{"title":"marker-b"}}}`,
			wantPresent: "marker-b",
			wantAbsent:  "marker-a",
		},
		{
			name:        "wrapped array without path",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId"},
			content:     `{"version":1,"entries":[{"sessionId":"sess-a","title":"marker-a"},{"sessionId":"sess-b","title":"marker-b"}]}`,
			wantPresent: "marker-b",
			wantAbsent:  "marker-a",
		},
		{
			name:        "sessions dictionary without path",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId"},
			content:     `{"sessions":{"sess-a":{"title":"marker-a"},"sess-b":{"title":"marker-b"}}}`,
			wantPresent: "marker-b",
			wantAbsent:  "marker-a",
		},
		{
			name:        "top-level dictionary",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId"},
			content:     `{"sess-a":{"title":"marker-a"},"sess-b":{"title":"marker-b"}}`,
			wantPresent: "marker-b",
			wantAbsent:  "marker-a",
		},
		{
			name:        "empty array removes file",
			index:       catalog.Index{Kind: "json-array", Glob: "index.json", Key: "sessionId"},
			content:     `[{"sessionId":"sess-a","title":"marker-a"}]`,
			wantRemoved: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, ModeTrash)
			body := filepath.Join(f.home, "sess-a.jsonl")
			writeTestFile(t, body, "{}\n")
			indexPath := filepath.Join(f.home, "index.json")
			writeTestFile(t, indexPath, tc.content)

			res, err := f.cleaner.Delete(
				catalog.Spec{ID: "demo", Indexs: []catalog.Index{tc.index}},
				f.home,
				[]session.Session{fileSession("sess-a", body)},
			)
			if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if len(res.Errors) != 0 {
				t.Fatalf("unexpected errors: %+v", res.Errors)
			}

			j, err := f.cleaner.readJournal(res.BatchID)
			if err != nil {
				t.Fatalf("readJournal: %v", err)
			}
			if tc.wantRemoved {
				if fsx.Exists(indexPath) {
					t.Fatalf("裁空的 json-array 文件应被删除")
				}
				if len(j.Indexes) != 1 || !fsx.Exists(j.Indexes[0].Backup) {
					t.Fatalf("删除索引文件前必须留备份: %+v", j.Indexes)
				}
				return
			}

			got := readTestFile(t, indexPath)
			if !strings.Contains(got, tc.wantPresent) {
				t.Fatalf("保留项 %q 不见了:\n%s", tc.wantPresent, got)
			}
			if strings.Contains(got, tc.wantAbsent) {
				t.Fatalf("目标项 %q 还在:\n%s", tc.wantAbsent, got)
			}
			// 输出必须是合法 JSON，且使用 2 空格缩进。
			var value any
			if err := json.Unmarshal([]byte(got), &value); err != nil {
				t.Fatalf("裁剪结果不是合法 JSON: %v\n%s", err, got)
			}
			if !strings.Contains(got, "\n  ") {
				t.Fatalf("JSON 未使用 2 空格缩进:\n%s", got)
			}
		})
	}
}

func TestJSONDictTrim(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sess-a.jsonl")
	writeTestFile(t, body, "{}\n")
	indexPath := filepath.Join(f.home, "dict.json")
	writeTestFile(t, indexPath, `{"sess-a":{"title":"marker-a"},"sess-b":{"title":"marker-b"}}`)

	spec := catalog.Spec{
		ID:     "demo",
		Indexs: []catalog.Index{{Kind: "json-dict", Glob: "dict.json"}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("sess-a", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	got := readTestFile(t, indexPath)
	if strings.Contains(got, "marker-a") || !strings.Contains(got, "marker-b") {
		t.Fatalf("json-dict 裁剪结果不对:\n%s", got)
	}
}

func TestDeleteRejectsPathOutsideHome(t *testing.T) {
	cases := []struct {
		name string
		path func(f *fixture) string
	}{
		{
			name: "parent escape",
			path: func(f *fixture) string {
				outside := t.TempDir()
				return filepath.Join(outside, "secret.txt")
			},
		},
		{
			name: "home itself",
			path: func(f *fixture) string { return f.home },
		},
		{
			name: "prefix sibling",
			path: func(f *fixture) string { return f.home + "-other/secret.txt" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, ModePermanent)
			victim := tc.path(f)
			if tc.name == "parent escape" {
				writeTestFile(t, victim, "do not delete")
			}

			res, err := f.cleaner.Delete(
				catalog.Spec{ID: "demo"},
				f.home,
				[]session.Session{fileSession("evil", victim)},
			)
			if err != nil {
				t.Fatalf("Delete 不应因单条路径越界而返回致命错误: %v", err)
			}
			if res.Count != 0 {
				t.Fatalf("越界会话不应计入 Count，got %d", res.Count)
			}
			if len(res.Errors) == 0 {
				t.Fatalf("越界路径必须记 ItemError")
			}
			if tc.name == "parent escape" && !fsx.Exists(victim) {
				t.Fatalf("越界文件被删了: %s", victim)
			}
		})
	}
}

func TestUndoRestoresFilesAndIsIdempotent(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "projects", "p", "sess.jsonl")
	bodyContent := `{"role":"user","text":"请保留我"}` + "\n"
	writeTestFile(t, body, bodyContent)

	indexOriginal := `[{"sessionId":"sess","title":"要删的"},{"sessionId":"other","title":"留着的"}]`
	indexPath := filepath.Join(f.home, "index.json")
	writeTestFile(t, indexPath, indexOriginal)

	spec := catalog.Spec{
		ID:     "demo",
		Indexs: []catalog.Index{{Kind: "json-array", Glob: "index.json", Key: "sessionId"}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("sess", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if fsx.Exists(body) {
		t.Fatalf("删除后正文应不在原位")
	}
	if strings.Contains(readTestFile(t, indexPath), "要删的") {
		t.Fatalf("删除后索引应已裁剪")
	}

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("undo errors: %+v", undo.Errors)
	}
	// 一个索引备份 + 一个废纸篓文件。
	if undo.Restored != 2 {
		t.Fatalf("Restored = %d, want 2", undo.Restored)
	}
	if got := readTestFile(t, body); got != bodyContent {
		t.Fatalf("恢复后的正文内容不对: %q", got)
	}
	if got := readTestFile(t, indexPath); got != indexOriginal {
		t.Fatalf("恢复后的索引不是删前快照:\n%s", got)
	}

	batches, err := f.cleaner.Batches(10)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("Batches = %d, want 1", len(batches))
	}
	if batches[0].Undoable {
		t.Fatalf("撤销后的批次不应再是 undoable")
	}

	second, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("重复 Undo 应幂等，got err: %v", err)
	}
	if second.Restored != 0 {
		t.Fatalf("重复 Undo Restored = %d, want 0", second.Restored)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("重复 Undo 不应报错: %+v", second.Errors)
	}
}

func TestUndoSkipsExistingTarget(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sess.jsonl")
	writeTestFile(t, body, "original")
	// 用固定 ID，方便在撤销前重新创建同名文件模拟「用户又新建了一条同名会话」。
	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{fileSession("sess", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	writeTestFile(t, body, "newer content")

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if undo.Restored != 0 {
		t.Fatalf("目标已存在时不应覆盖，Restored = %d", undo.Restored)
	}
	if len(undo.Errors) == 0 {
		t.Fatalf("目标已存在必须记一条 error")
	}
	if got := readTestFile(t, body); got != "newer content" {
		t.Fatalf("撤销不应覆盖已存在的目标，got %q", got)
	}
}

func TestSQLiteDeleteAndUndo(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := filepath.Join(f.home, "agent.db")

	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT)`,
		`CREATE TABLE message (id INTEGER PRIMARY KEY, session_id TEXT, body TEXT)`,
		`INSERT INTO session (id, title) VALUES ('s1', '要删'), ('s2', '留着')`,
		`INSERT INTO message (session_id, body) VALUES ('s1', 'm1'), ('s1', 'm2'), ('s2', 'm3')`,
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
			Kind: "sqlite", DB: "agent.db", Table: "session", IDCol: "id",
			Rows: []string{"message:session_id"},
		}},
	}
	s := session.Session{ID: "s1", UID: "demo:s1", AgentID: "demo", Storage: session.StorageSQLite}

	res, err := f.cleaner.Delete(spec, f.home, []session.Session{s})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	assertRowCounts(t, dbPath, 1, 1)

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 {
		t.Fatalf("journal sqlite = %d, want 1", len(j.SQLite))
	}
	if !fsx.Exists(j.SQLite[0].Backup) {
		t.Fatalf("SQLite 备份不存在: %s", j.SQLite[0].Backup)
	}
	if len(j.SQLite[0].Deletes) != 2 {
		t.Fatalf("journal 删除记录 = %d, want 2（主表 + message）", len(j.SQLite[0].Deletes))
	}
	// 备份必须是删行前的完整库。
	assertRowCounts(t, j.SQLite[0].Backup, 2, 3)

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("undo errors: %+v", undo.Errors)
	}
	if undo.Restored != 1 {
		t.Fatalf("Restored = %d, want 1", undo.Restored)
	}
	// 主表和子表都要回来，不能留下孤儿消息。
	assertRowCounts(t, dbPath, 2, 3)
}

func TestSQLiteMissingTableRecordsError(t *testing.T) {
	f := newFixture(t, ModePermanent)
	dbPath := filepath.Join(f.home, "agent.db")
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY)`); err != nil {
		db.Close()
		t.Fatalf("create: %v", err)
	}
	db.Close()

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "sqlite", DB: "agent.db", Table: "missing_table", IDCol: "id",
		}},
	}
	s := session.Session{ID: "s1", UID: "demo:s1", AgentID: "demo", Storage: session.StorageSQLite}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{s})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("表不存在时不应计数，got %d", res.Count)
	}
	if len(res.Errors) == 0 {
		t.Fatalf("表不存在必须记 ItemError")
	}
	// 删除失败时库应保持原样（事务回滚），不能出现删了一半的状态。
	var n int
	readDB, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer readDB.Close()
	if err := readDB.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("session 行数 = %d, want 0", n)
	}
}

func TestBatchIDCollisionSuffix(t *testing.T) {
	f := newFixture(t, ModePermanent)
	bodyA := filepath.Join(f.home, "a.jsonl")
	bodyB := filepath.Join(f.home, "b.jsonl")
	writeTestFile(t, bodyA, "a")
	writeTestFile(t, bodyB, "b")

	resA, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{fileSession("a", bodyA)})
	if err != nil {
		t.Fatalf("Delete A: %v", err)
	}
	resB, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{fileSession("b", bodyB)})
	if err != nil {
		t.Fatalf("Delete B: %v", err)
	}
	if resA.BatchID == resB.BatchID {
		t.Fatalf("同一秒内两次删除拿到了相同 batchID: %s", resA.BatchID)
	}
	if resB.BatchID != resA.BatchID+"-2" {
		t.Fatalf("第二个 batchID = %s, want %s-2", resB.BatchID, resA.BatchID)
	}

	batches, err := f.cleaner.Batches(1)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("Batches(1) = %d, want 1", len(batches))
	}
}

// assertRowCounts 直接查库，避免测试依赖 clean 包内部的 statistics。
func assertRowCounts(t *testing.T, dbPath string, wantSessions, wantMessages int) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var sessions, messages int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&sessions); err != nil {
		t.Fatalf("%s count session: %v", dbPath, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM message`).Scan(&messages); err != nil {
		t.Fatalf("%s count message: %v", dbPath, err)
	}
	if sessions != wantSessions || messages != wantMessages {
		t.Fatalf("%s rows = (%d,%d), want (%d,%d)", dbPath, sessions, messages, wantSessions, wantMessages)
	}
}

func TestSQLiteIndexRowsTrimAndUndo(t *testing.T) {
	f := newFixture(t, ModeTrash)
	body := filepath.Join(f.home, "sess-a.jsonl")
	writeTestFile(t, body, "{}\n")

	idxPath := filepath.Join(f.home, "index.db")
	db, err := sql.Open("sqlite", sqliteDSN(idxPath, false))
	if err != nil {
		t.Fatalf("open index db: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE index_rows (id TEXT PRIMARY KEY)`,
		`INSERT INTO index_rows (id) VALUES ('sess-a'), ('sess-b')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	db.Close()

	spec := catalog.Spec{
		ID: "demo",
		Indexs: []catalog.Index{{
			Kind: "sqlite-rows", DB: "index.db", Table: "index_rows", IDCol: "id",
		}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("sess-a", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if got := countTable(t, idxPath, "index_rows"); got != 1 {
		t.Fatalf("index_rows = %d, want 1", got)
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.SQLite) != 1 || j.SQLite[0].DB != idxPath {
		t.Fatalf("journal 未记录索引库: %+v", j.SQLite)
	}
	if !fsx.Exists(j.SQLite[0].Backup) {
		t.Fatalf("索引库备份不存在: %s", j.SQLite[0].Backup)
	}

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undo.Errors) != 0 {
		t.Fatalf("undo errors: %+v", undo.Errors)
	}
	if got := countTable(t, idxPath, "index_rows"); got != 2 {
		t.Fatalf("撤销后 index_rows = %d, want 2", got)
	}
}

func countTable(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, true))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + quoteIdent(table)).Scan(&n); err != nil {
		t.Fatalf("%s count: %v", table, err)
	}
	return n
}

func TestTrashNameCollision(t *testing.T) {
	f := newFixture(t, ModeTrash)
	bodyA := filepath.Join(f.home, "a", "session.jsonl")
	bodyB := filepath.Join(f.home, "b", "session.jsonl")
	writeTestFile(t, bodyA, "content-a")
	writeTestFile(t, bodyB, "content-b")

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home, []session.Session{
		fileSession("a", bodyA),
		fileSession("b", bodyB),
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}

	j, err := f.cleaner.readJournal(res.BatchID)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(j.Moved) != 2 {
		t.Fatalf("moved = %d, want 2", len(j.Moved))
	}
	if j.Moved[0].Trashed == j.Moved[1].Trashed {
		t.Fatalf("废纸篓重名未加序号: %s", j.Moved[0].Trashed)
	}
	if got := readTestFile(t, j.Moved[0].Trashed); got != "content-a" {
		t.Fatalf("第一个副本内容 = %q", got)
	}
	if got := readTestFile(t, j.Moved[1].Trashed); got != "content-b" {
		t.Fatalf("第二个副本内容 = %q", got)
	}

	undo, err := f.cleaner.Undo(res.BatchID)
	if err != nil || len(undo.Errors) != 0 {
		t.Fatalf("Undo: %+v, %v", undo, err)
	}
	if got := readTestFile(t, bodyA); got != "content-a" {
		t.Fatalf("bodyA 恢复内容 = %q", got)
	}
	if got := readTestFile(t, bodyB); got != "content-b" {
		t.Fatalf("bodyB 恢复内容 = %q", got)
	}
}
