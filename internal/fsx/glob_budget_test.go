package fsx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// seedGlobTree 造一棵 3 层树，每层一个 .jsonl：
//
//	root/top.jsonl
//	root/a/one.jsonl
//	root/a/b/two.jsonl
//	root/a/b/c/three.jsonl
func seedGlobTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{
		"top.jsonl",
		"a/one.jsonl",
		"a/b/two.jsonl",
		"a/b/c/three.jsonl",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return root
}

// TestGlobBoundedEntryBudget 钉住条目上限：第 maxEntries+1 条出现时停下，
// 返回恰好 maxEntries 条并标记 truncated。
func TestGlobBoundedEntryBudget(t *testing.T) {
	root := seedGlobTree(t)

	paths, truncated := GlobBounded(root, "**/*.jsonl", 2, 0)
	if !truncated {
		t.Fatal("4 个匹配只给 2 条预算，必须标记 truncated")
	}
	if len(paths) != 2 {
		t.Fatalf("返回 %d 条，want 恰好 2 条", len(paths))
	}
	for _, path := range paths {
		if filepath.Ext(path) != ".jsonl" {
			t.Fatalf("返回了非匹配文件: %s", path)
		}
	}
}

// TestGlobBoundedDepthBudget 钉住 `**` 递归深度：超过 maxDepth 的目录不再下探，
// 已经能匹配到的浅层文件照常返回。
func TestGlobBoundedDepthBudget(t *testing.T) {
	root := seedGlobTree(t)

	paths, truncated := GlobBounded(root, "**/*.jsonl", 0, 1)
	if !truncated {
		t.Fatal("还有更深的目录没展开，必须标记 truncated")
	}
	want := []string{
		filepath.Join(root, "a", "one.jsonl"),
		filepath.Join(root, "top.jsonl"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("maxDepth=1 的结果 = %v，want %v", paths, want)
	}
}

// TestGlobBoundedUnlimitedMatchesGlob 确认预算充足时与旧 Glob 完全一致（含顺序），
// 并且不误报截断。
func TestGlobBoundedUnlimitedMatchesGlob(t *testing.T) {
	root := seedGlobTree(t)

	got, truncated := GlobBounded(root, "**/*.jsonl", 100, 10)
	if truncated {
		t.Fatal("预算充足时不该标记 truncated")
	}
	want := Glob(root, "**/*.jsonl")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GlobBounded = %v，want 与 Glob 一致 %v", got, want)
	}

	// 0 表示不限制，同样要与 Glob 逐项相等。
	unlimited, truncated := GlobBounded(root, "**/*.jsonl", 0, 0)
	if truncated {
		t.Fatal("0 表示不限制，不该标记 truncated")
	}
	if !reflect.DeepEqual(unlimited, want) {
		t.Fatalf("GlobBounded(0,0) = %v，want %v", unlimited, want)
	}
}

// TestGlobStillUnlimited 确认既有入口没有因为加预算而改变行为。
func TestGlobStillUnlimited(t *testing.T) {
	root := seedGlobTree(t)
	paths := Glob(root, "**/*.jsonl")
	if len(paths) != 4 {
		t.Fatalf("Glob 返回 %d 条，want 4（旧入口不应有任何预算）", len(paths))
	}
}
