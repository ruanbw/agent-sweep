package clean

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// guardSymlinkOrSkip 在文件系统不支持符号链接时跳过，避免把平台限制误报成回归。
func guardSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前文件系统不支持符号链接: %v", err)
	}
}

// R1：中间层 symlink 越界。
//
// home/projects 是指到外部目录的符号链接时，home/projects/s1.jsonl 词法上仍在
// home 内，但 os.RemoveAll / moveToTrash 会由内核顺着链接删到数据根之外。
// 校验必须看穿中间层链接，并且拒绝时外部文件要原封不动。
func TestDeleteRejectsSymlinkedParentEscape(t *testing.T) {
	f := newFixture(t, ModePermanent)
	outside := t.TempDir()
	victim := filepath.Join(outside, "s1.jsonl")
	writeTestFile(t, victim, "不要删我")

	linkDir := filepath.Join(f.home, "projects")
	guardSymlinkOrSkip(t, outside, linkDir)
	target := filepath.Join(linkDir, "s1.jsonl")

	// 先单独验证校验函数：即使不经过 Delete，也必须拒绝。
	if err := checkPathInHome(f.home, target); err == nil {
		t.Fatalf("中间层 symlink 越界未被拒绝: %s", target)
	}

	res, err := f.cleaner.Delete(
		catalog.Spec{ID: "demo"},
		f.home,
		[]session.Session{fileSession("s1", target)},
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

	// 关键断言：数据根之外的文件必须还在，内容也不能变。
	if !fsx.Exists(victim) {
		t.Fatalf("外部文件被删了: %s", victim)
	}
	if got := readTestFile(t, victim); got != "不要删我" {
		t.Fatalf("外部文件内容被改了: %q", got)
	}
	// 整条会话被拒绝时，链接本身也不该被动过。
	if _, err := os.Lstat(linkDir); err != nil {
		t.Fatalf("被拒绝时不应改动中间层链接: %v", err)
	}
}

// 正向：home 内的普通文件仍然能正常删除（permanent 模式，完全不碰 ~/.Trash）。
func TestDeleteStillRemovesRegularFileInHome(t *testing.T) {
	f := newFixture(t, ModePermanent)
	body := filepath.Join(f.home, "sessions", "ok.jsonl")
	writeTestFile(t, body, "bye")

	if err := checkPathInHome(f.home, body); err != nil {
		t.Fatalf("home 内的普通文件不应被拒绝: %v", err)
	}
	res, err := f.cleaner.Delete(
		catalog.Spec{ID: "demo"},
		f.home,
		[]session.Session{fileSession("ok", body)},
	)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	if fsx.Exists(body) {
		t.Fatalf("普通文件没有被删除: %s", body)
	}
}

// 正向：home 内部指向 home 内部的链接不算越界，不应被误伤。
func TestDeleteAllowsSymlinkResolvingInsideHome(t *testing.T) {
	f := newFixture(t, ModePermanent)
	realDir := filepath.Join(f.home, "real-projects")
	body := filepath.Join(realDir, "s1.jsonl")
	writeTestFile(t, body, "inside")

	linkDir := filepath.Join(f.home, "projects")
	guardSymlinkOrSkip(t, realDir, linkDir)

	res, err := f.cleaner.Delete(
		catalog.Spec{ID: "demo"},
		f.home,
		[]session.Session{fileSession("s1", filepath.Join(linkDir, "s1.jsonl"))},
	)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}
	if fsx.Exists(filepath.Join(linkDir, "s1.jsonl")) {
		t.Fatalf("home 内的链接路径应被正常删除")
	}
	if fsx.Exists(body) {
		t.Fatalf("真实文件应随链接路径一起被删除: %s", body)
	}
}

// 正向：最后一段本身是 symlink 时允许删除 —— 只删链接，不跟目标。
func TestDeleteTrailingSymlinkRemovesOnlyLink(t *testing.T) {
	f := newFixture(t, ModePermanent)
	outside := t.TempDir()
	realFile := filepath.Join(outside, "real.jsonl")
	writeTestFile(t, realFile, "keep me")

	link := filepath.Join(f.home, "victim.link")
	guardSymlinkOrSkip(t, realFile, link)

	if err := checkPathInHome(f.home, link); err != nil {
		t.Fatalf("尾部 symlink 应允许删除（只删链接本身）: %v", err)
	}
	res, err := f.cleaner.Delete(
		catalog.Spec{ID: "demo"},
		f.home,
		[]session.Session{fileSession("victim", link)},
	)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d, want 1", res.Count)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("链接本身应被删除，Lstat err = %v", err)
	}
	// 关键断言：链接目标必须毫发无伤。
	if !fsx.Exists(realFile) {
		t.Fatalf("链接目标被删了: %s", realFile)
	}
	if got := readTestFile(t, realFile); got != "keep me" {
		t.Fatalf("链接目标内容被改了: %q", got)
	}
}

// 负向：普通 ../ 逃逸仍被拦住；空路径、数据根本身、空 home 的既有拒绝行为不变。
func TestCheckPathInHomeKeepsLexicalGuard(t *testing.T) {
	home := t.TempDir()

	rejected := []struct {
		name string
		home string
		path string
	}{
		{name: "parent escape", home: home, path: filepath.Join(home, "..", "secret.txt")},
		{name: "sibling prefix", home: home, path: home + "-other/secret.txt"},
		{name: "home itself", home: home, path: home},
		{name: "empty path", home: home, path: ""},
		{name: "empty home", home: "", path: filepath.Join(home, "sessions", "s1.jsonl")},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkPathInHome(tc.home, tc.path); err == nil {
				t.Fatalf("应拒绝 %q（home=%q），却放行了", tc.path, tc.home)
			}
		})
	}

	// 对照：home 内正常路径仍应放行，避免上面全是「因错误而通过」。
	ok := filepath.Join(home, "sessions", "s1.jsonl")
	writeTestFile(t, ok, "{}")
	if err := checkPathInHome(home, ok); err != nil {
		t.Fatalf("home 内路径被误拒: %v", err)
	}
}

// 路径不存在属于正常情况（用户可能刚手动清过）：真实路径层应退回词法判断，
// 而不是把「不存在」当成越界错误。
func TestCheckPathInHomeAllowsMissingTarget(t *testing.T) {
	home := t.TempDir()

	// 父目录都不存在：EvalSymlinks 会失败，此时沿用词法结论。
	if err := checkPathInHome(home, filepath.Join(home, "gone", "s1.jsonl")); err != nil {
		t.Fatalf("父目录不存在时应退回词法判断，不应报错: %v", err)
	}

	// 父目录存在、文件本身不存在。
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkPathInHome(home, filepath.Join(dir, "not-yet.jsonl")); err != nil {
		t.Fatalf("文件不存在时应退回词法判断，不应报错: %v", err)
	}
}
