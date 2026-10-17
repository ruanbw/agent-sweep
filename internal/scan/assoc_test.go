package scan

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// 这一组用例只覆盖两个回归点：
//   R15  assoc 模板不能把数据根之外的路径写进 Session.Paths；
//   R17  列名回退必须留下用户能看见的警告，且按表/列去重。

func writeAssocFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedAssocSQLite 造一个最小的 sqlite 会话库：表 session 同时带上
// id/title/created_at/cwd，方便分别测试「列配置正确」与「回退到候选列」。
func seedAssocSQLite(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开夹具库：%v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE session (
		id TEXT PRIMARY KEY,
		title TEXT,
		created_at INTEGER,
		cwd TEXT
	)`); err != nil {
		t.Fatalf("建表：%v", err)
	}
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("s%d", i+1)
		if _, err := db.Exec(
			`INSERT INTO session VALUES (?, ?, 1792119045599, '/Users/me/proj')`,
			id, "会话 "+id,
		); err != nil {
			t.Fatalf("插行：%v", err)
		}
	}
}

func assocSQLiteStore(db string, assoc ...string) catalog.Store {
	return catalog.Store{
		Kind: "sqlite", DB: db, Table: "session",
		IDCol: "id", TitleCol: "title", TimeCol: "created_at", CWDCol: "cwd",
		Assoc: assoc,
	}
}

func hasPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

// TestAssocAcceptsPathsInsideRoot 覆盖两条合法作用域：
//   - `/{id}` 相对数据根；
//   - `foo/{id}` 相对会话文件所在目录。
//
// 同时断言 Bytes 确实把两份旁挂产物算了进去。
func TestAssocAcceptsPathsInsideRoot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "p")
	sessionPath := filepath.Join(project, "s1.jsonl")
	sessionBody := `{"message":{"role":"user","content":"hi"}}` + "\n"
	writeAssocFile(t, sessionPath, sessionBody)

	fooPath := filepath.Join(project, "foo", "s1")
	fooBody := "foo-sidecar"
	writeAssocFile(t, fooPath, fooBody)

	brainPath := filepath.Join(root, "brain", "s1")
	brainBody := "brain-sidecar-data"
	writeAssocFile(t, brainPath, brainBody)

	spec := catalog.Spec{
		ID: "assoc-ok", Name: "Assoc OK",
		Roots: []string{root},
		Stores: []catalog.Store{{
			Kind: "jsonl", Glob: "projects/*/*.jsonl",
			Assoc: []string{"/brain/{id}", "foo/{id}"},
		}},
	}

	result := One(spec)
	if result.Err != "" {
		t.Fatalf("合法 assoc 不应产生警告：%s", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d", len(result.Sessions))
	}
	item := result.Sessions[0]
	for _, want := range []string{sessionPath, brainPath, fooPath} {
		if !hasPath(item.Paths, want) {
			t.Errorf("Paths 缺少 %s：%v", want, item.Paths)
		}
	}
	wantBytes := int64(len(sessionBody) + len(fooBody) + len(brainBody))
	if item.Bytes != wantBytes {
		t.Errorf("Bytes = %d，期望 %d（正文 + 两份旁挂）", item.Bytes, wantBytes)
	}
}

// TestAssocRejectsEscapingPatterns 是 R15 的核心：Join 会先 Clean 掉 `..`，
// 所以必须在 Join 之后判包含性。每个越界 pattern 都指向真实存在的 outside
// 文件 —— 不拒绝的话它会被写进 Paths，Bytes 也会把别人的文件算进来。
func TestAssocRejectsEscapingPatterns(t *testing.T) {
	base := t.TempDir()
	// 数据根放深两层，让 `../../outside` 正好落到 base/a/outside。
	root := filepath.Join(base, "a", "b", "home")
	outsideFromRelative := filepath.Join(base, "a", "outside")
	outsideFromRooted := filepath.Join(base, "a", "b", "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAssocFile(t, outsideFromRelative, "outside-relative")
	writeAssocFile(t, outsideFromRooted, "outside-rooted")

	db := "assoc.db"
	seedAssocSQLite(t, filepath.Join(root, db), 1)

	cases := []struct {
		name    string
		pattern string
		outside string
	}{
		{"相对路径上跳", "../../outside", outsideFromRelative},
		{"数据根相对上跳", "/../outside", outsideFromRooted},
		{"双斜杠绝对绕法", "//../../outside", outsideFromRelative},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := catalog.Spec{
				ID: "assoc-bad", Name: "Assoc Bad",
				Roots:  []string{root},
				Stores: []catalog.Store{assocSQLiteStore(db, c.pattern)},
			}
			result := One(spec)
			if len(result.Sessions) != 1 {
				t.Fatalf("期望 1 条会话，实际 %d", len(result.Sessions))
			}
			item := result.Sessions[0]
			if hasPath(item.Paths, c.outside) {
				t.Fatalf("越界路径被收进了 Paths：%v", item.Paths)
			}
			if len(item.Paths) != 0 {
				t.Errorf("越界 pattern 不应产生任何 Paths，实际 %v", item.Paths)
			}
			if item.Bytes != 0 {
				t.Errorf("越界文件不能计入 Bytes，实际 %d", item.Bytes)
			}
			if !strings.Contains(result.Err, c.pattern) {
				t.Errorf("Err 应说明被拒的 pattern %q，实际 %q", c.pattern, result.Err)
			}
			if !strings.Contains(result.Err, "数据根") || !strings.Contains(result.Err, "已跳过") {
				t.Errorf("Err 应是可读的越界说明，实际 %q", result.Err)
			}
		})
	}
}

// TestAssocMixedKeepsLegalRejectsIllegal 锁定「一条合法一条越界」的边界：
// 合法那条照常进 Paths 与 Bytes，越界那条只留警告，不能互相影响。
func TestAssocMixedKeepsLegalRejectsIllegal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "a", "b", "home")
	outside := filepath.Join(base, "a", "outside")
	writeAssocFile(t, outside, "outside-data")

	brainBody := "brain-inside"
	brainPath := filepath.Join(root, "brain", "s1")
	writeAssocFile(t, brainPath, brainBody)

	db := "assoc.db"
	seedAssocSQLite(t, filepath.Join(root, db), 1)

	spec := catalog.Spec{
		ID: "assoc-mixed", Name: "Assoc Mixed",
		Roots:  []string{root},
		Stores: []catalog.Store{assocSQLiteStore(db, "/brain/{id}", "../../outside")},
	}
	result := One(spec)
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d", len(result.Sessions))
	}
	item := result.Sessions[0]
	if !hasPath(item.Paths, brainPath) {
		t.Fatalf("合法路径应保留：%v", item.Paths)
	}
	if hasPath(item.Paths, outside) {
		t.Fatalf("越界路径不应保留：%v", item.Paths)
	}
	if item.Bytes != int64(len(brainBody)) {
		t.Errorf("Bytes 只应累加合法旁挂，实际 %d，期望 %d", item.Bytes, len(brainBody))
	}
	if !strings.Contains(result.Err, "../../outside") {
		t.Errorf("越界 pattern 应留下警告，实际 %q", result.Err)
	}
	if strings.Contains(result.Err, "/brain/{id}") {
		t.Errorf("合法 pattern 不应被报成问题，实际 %q", result.Err)
	}
}

// TestSQLiteExplicitColumnsNoWarning 是 R17 的反向断言：列显式配好并命中时
// 一条警告都不能有，否则正常扫描会被提醒刷屏。
func TestSQLiteExplicitColumnsNoWarning(t *testing.T) {
	root := t.TempDir()
	db := "warn.db"
	seedAssocSQLite(t, filepath.Join(root, db), 1)

	spec := catalog.Spec{
		ID: "explicit", Name: "Explicit",
		Roots:  []string{root},
		Stores: []catalog.Store{assocSQLiteStore(db)},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("显式列命中不应产生警告：%q", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d", len(result.Sessions))
	}
}

// TestSQLiteColumnFallbackIsReported 覆盖两种真实回退：
//   - 根本没配列名；
//   - 配了但表里没有这列。
//
// 警告必须同时带表名、配置角色和最终使用的列名。
func TestSQLiteColumnFallbackIsReported(t *testing.T) {
	root := t.TempDir()
	db := "warn.db"
	seedAssocSQLite(t, filepath.Join(root, db), 1)

	t.Run("未配置列名", func(t *testing.T) {
		store := assocSQLiteStore(db)
		store.TimeCol = ""
		result := One(catalog.Spec{
			ID: "fallback-empty", Name: "Fallback Empty",
			Roots: []string{root}, Stores: []catalog.Store{store},
		})
		if !strings.Contains(result.Err, "警告：") {
			t.Fatalf("回退应是警告级说明，实际 %q", result.Err)
		}
		for _, want := range []string{"session", "timeCol", "created_at"} {
			if !strings.Contains(result.Err, want) {
				t.Errorf("警告缺少 %q：%s", want, result.Err)
			}
		}
		if strings.Count(result.Err, "timeCol") != 1 {
			t.Errorf("单个回退只应记一次，实际 %q", result.Err)
		}
		if len(result.Sessions) != 1 {
			t.Fatalf("回退不能丢会话，实际 %d 条", len(result.Sessions))
		}
	})

	t.Run("配置的列不存在", func(t *testing.T) {
		store := assocSQLiteStore(db)
		store.TimeCol = "updated_at"
		result := One(catalog.Spec{
			ID: "fallback-missing", Name: "Fallback Missing",
			Roots: []string{root}, Stores: []catalog.Store{store},
		})
		for _, want := range []string{"session", "timeCol", "updated_at", "created_at"} {
			if !strings.Contains(result.Err, want) {
				t.Errorf("警告缺少 %q：%s", want, result.Err)
			}
		}
	})
}

// TestSQLiteFallbackWarningsAreDeduplicated 覆盖「回退很多列 / 多个 Store
// 各回退一次」的情况：警告按「表 + 角色 + 实际列」去重，不按会话数、也不按
// Store 数重复。
func TestSQLiteFallbackWarningsAreDeduplicated(t *testing.T) {
	root := t.TempDir()
	db := "warn.db"
	seedAssocSQLite(t, filepath.Join(root, db), 3)

	first := assocSQLiteStore(db)
	first.TimeCol = ""
	second := assocSQLiteStore(db)
	second.TimeCol = ""

	spec := catalog.Spec{
		ID: "fallback-dedup", Name: "Fallback Dedup",
		Roots: []string{root},
		// 两个 Store 扫描同一张表、各自回退一次：去重后只该剩一条说明，
		// 也不能因为表里有 3 行就出现 6 条。
		Stores: []catalog.Store{first, second},
	}
	result := One(spec)
	if got := strings.Count(result.Err, "回退到列 created_at"); got != 1 {
		t.Fatalf("回退警告应合并去重，实际出现 %d 次：%s", got, result.Err)
	}
	if got := strings.Count(result.Err, "警告："); got != 1 {
		t.Fatalf("多条提示应合并到一条警告里，实际 %d 条：%s", got, result.Err)
	}
	if len(result.Sessions) != 3 {
		t.Fatalf("期望 3 条会话，实际 %d", len(result.Sessions))
	}
}

// TestSQLiteFallbackWarningsMergeAcrossColumns 锁定另一个合并方向：同一张表
// 回退两列时，两条说明并进同一条 Err，而不是互相覆盖。
func TestSQLiteFallbackWarningsMergeAcrossColumns(t *testing.T) {
	root := t.TempDir()
	db := "warn.db"
	seedAssocSQLite(t, filepath.Join(root, db), 1)

	timeStore := assocSQLiteStore(db)
	timeStore.TimeCol = ""

	cwdStore := assocSQLiteStore(db)
	cwdStore.CWDCol = ""

	result := One(catalog.Spec{
		ID: "fallback-merge", Name: "Fallback Merge",
		Roots:  []string{root},
		Stores: []catalog.Store{timeStore, cwdStore},
	})
	for _, want := range []string{"timeCol", "created_at", "cwdCol", "cwd"} {
		if !strings.Contains(result.Err, want) {
			t.Errorf("合并后的警告缺少 %q：%s", want, result.Err)
		}
	}
	if got := strings.Count(result.Err, "警告："); got != 1 {
		t.Errorf("所有提示应汇总进一条警告，实际 %d 条：%s", got, result.Err)
	}
}
