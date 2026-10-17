package fsx

import (
	"fmt"
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

	paths, truncated := GlobBounded(root, "**/*.jsonl", 2, 0, 0)
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

	paths, truncated := GlobBounded(root, "**/*.jsonl", 0, 1, 0)
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

	got, truncated := GlobBounded(root, "**/*.jsonl", 100, 10, 10_000)
	if truncated {
		t.Fatal("预算充足时不该标记 truncated")
	}
	want := Glob(root, "**/*.jsonl")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GlobBounded = %v，want 与 Glob 一致 %v", got, want)
	}

	// 0 表示不限制，同样要与 Glob 逐项相等。
	unlimited, truncated := GlobBounded(root, "**/*.jsonl", 0, 0, 0)
	if truncated {
		t.Fatal("0 表示不限制，不该标记 truncated")
	}
	if !reflect.DeepEqual(unlimited, want) {
		t.Fatalf("GlobBounded(0,0,0) = %v，want %v", unlimited, want)
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

// TestGlobBoundedVisitedBudget 钉住「访问目录项数」预算：一条都不匹配的宽
// glob 也会在 ReadDir 之后停下，而不是把单层目录扫穿。
func TestGlobBoundedVisitedBudget(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		path := filepath.Join(root, fmt.Sprintf("file-%d.txt", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	paths, truncated := GlobBounded(root, "nomatch/*.x", 0, 0, 3)
	if !truncated {
		t.Fatal("单层 5 个目录项只给 3 条访问预算，必须标记 truncated")
	}
	if len(paths) != 0 {
		t.Fatalf("pattern 一条都不匹配，结果应为空，got %v", paths)
	}

	// 0 表示不限制：与 Glob 完全一致（空结果、不截断）。
	paths, truncated = GlobBounded(root, "nomatch/*.x", 0, 0, 0)
	if truncated {
		t.Fatal("maxVisited=0 表示不限制，不该标记 truncated")
	}
	if want := Glob(root, "nomatch/*.x"); !reflect.DeepEqual(paths, want) {
		t.Fatalf("maxVisited=0 的结果 = %v，want %v", paths, want)
	}
}

// TestGlobBoundedCombinedBudgets 结果与访问两个预算同时收紧时，先命中的那个
// 生效，整体仍然要标 truncated。
func TestGlobBoundedCombinedBudgets(t *testing.T) {
	root := seedGlobTree(t)
	if _, truncated := GlobBounded(root, "**/*.jsonl", 2, 0, 3); !truncated {
		t.Fatal("结果条数与访问条目数预算同时收紧时必须标记 truncated")
	}
}

// TestGlobBoundedStopReason 钉住三维预算各自命中的原因，scan 侧据此在提示里
// 指明「结果太多 / 目录太深 / 单层太宽」。
func TestGlobBoundedStopReason(t *testing.T) {
	root := seedGlobTree(t)

	cases := []struct {
		name       string
		maxEntries int
		maxDepth   int
		maxVisited int
		want       GlobTruncation
	}{
		{"结果条数", 2, 0, 10_000, GlobTruncatedEntries},
		{"递归深度", 100, 1, 0, GlobTruncatedDepth},
		{"访问条目数", 100, 0, 3, GlobTruncatedVisited},
		{"访问条目数含目录项本身", 100, 0, 1, GlobTruncatedVisited},
		{"均未命中", 100, 10, 10_000, GlobNotTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stop := GlobBoundedStop(root, "**/*.jsonl", tc.maxEntries, tc.maxDepth, tc.maxVisited)
			if stop != tc.want {
				t.Fatalf("stop = %v，want %v", stop, tc.want)
			}
		})
	}
}
