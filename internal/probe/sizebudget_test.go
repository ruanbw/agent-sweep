// sizeBudget 的专项测试：验证 R25「体积统计必须服从 3 秒硬预算」。
//
// 这些用例只碰 t.TempDir()，不依赖真实 HOME；大目录用例刻意让
// 条目预算远小于文件数，确保「提前返回 + 截断标志」真的生效。
package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/fsx"
)

// writeSizedFiles 在 dir 下写 n 个文件，每个 size 字节，返回总字节数。
func writeSizedFiles(t *testing.T, dir string, n, size int) int64 {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	payload := []byte(strings.Repeat("x", size))
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("file-%05d.txt", i))
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return int64(n * size)
}

func TestSizeBudgetSmallTreeExact(t *testing.T) {
	root := t.TempDir()
	want := writeSizedFiles(t, root, 10, 128)

	b := &sizeBudget{remaining: 500, deadline: time.Now().Add(time.Minute)}
	got, truncated := b.size(root)
	if truncated {
		t.Fatalf("10 个文件不应触发截断（visited=%d）", b.visited)
	}
	if got != want {
		t.Errorf("size = %d，期望 %d", got, want)
	}
	if b.visited == 0 {
		t.Error("visited 应大于 0")
	}
}

func TestSizeBudgetLargeTreeTruncates(t *testing.T) {
	root := t.TempDir()
	const files, size = 5000, 64
	want := writeSizedFiles(t, root, files, size)

	// 基线：fsx.Size 会完整递归整棵树。
	fullStart := time.Now()
	full := fsx.Size(root)
	fullElapsed := time.Since(fullStart)
	if full != want {
		t.Fatalf("基线 fsx.Size = %d，期望 %d", full, want)
	}

	const budget = 500
	b := &sizeBudget{remaining: budget, deadline: time.Now().Add(time.Minute)}
	start := time.Now()
	got, truncated := b.size(root)
	elapsed := time.Since(start)

	if !truncated {
		t.Fatalf("预算 %d、文件 %d，应触发截断（visited=%d）", budget, files, b.visited)
	}
	if got <= 0 {
		t.Errorf("截断时已统计部分应大于 0，实际 %d", got)
	}
	if got > want {
		t.Errorf("截断值 %d 不应超过真实大小 %d", got, want)
	}
	if b.visited > budget {
		t.Errorf("visited = %d，不应超过条目预算 %d", b.visited, budget)
	}

	// 全量统计太短时（<3ms）计时噪声占比过大，只在样本足够长时比较耗时。
	if fullElapsed > 3*time.Millisecond && elapsed*2 >= fullElapsed {
		t.Errorf("截断统计耗时 %v，未明显小于全量 %v", elapsed, fullElapsed)
	}
	t.Logf("全量 %d 字节/%v；预算 %d 条得到 %d 字节/%v（visited=%d，truncated=%v）",
		full, fullElapsed, budget, got, elapsed, b.visited, truncated)
}

func TestSizeBudgetSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	visible := int64(len("visible"))
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("visible"), 0o644); err != nil {
		t.Fatalf("write visible: %v", err)
	}

	// 外部大目录：如果被跟随，visited 会远超 10。
	external := t.TempDir()
	writeSizedFiles(t, external, 100, 64)
	if err := os.Symlink(external, filepath.Join(root, "external-link")); err != nil {
		t.Skipf("当前平台不支持 symlink: %v", err)
	}
	// 文件符号链接同样不应计入。
	if err := os.Symlink(filepath.Join(external, "file-00000.txt"), filepath.Join(root, "file-link")); err != nil {
		t.Skipf("当前平台不支持 symlink: %v", err)
	}

	b := &sizeBudget{remaining: 1000, deadline: time.Now().Add(time.Minute)}
	got, truncated := b.size(root)
	if truncated {
		t.Fatalf("不应截断（visited=%d）", b.visited)
	}
	if got != visible {
		t.Errorf("size = %d，期望只统计普通文件 %d（符号链接不计入）", got, visible)
	}
	if b.visited > 5 {
		t.Errorf("visited = %d，说明递归进了符号链接指向的外部目录", b.visited)
	}
}

func TestSizeBudgetSkipsHiddenEntries(t *testing.T) {
	root := t.TempDir()
	visible := int64(len("visible"))
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("visible"), 0o644); err != nil {
		t.Fatalf("write visible: %v", err)
	}
	// 隐藏目录与隐藏文件都不应计入，也不应消耗条目预算。
	writeSizedFiles(t, filepath.Join(root, ".cache"), 50, 64)
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte(strings.Repeat("x", 32)), 0o644); err != nil {
		t.Fatalf("write hidden: %v", err)
	}

	b := &sizeBudget{remaining: 1000, deadline: time.Now().Add(time.Minute)}
	got, truncated := b.size(root)
	if truncated {
		t.Fatalf("不应截断（visited=%d）", b.visited)
	}
	if got != visible {
		t.Errorf("size = %d，期望 %d（隐藏项应跳过）", got, visible)
	}
	if b.visited != 2 {
		t.Errorf("visited = %d，期望 2（根目录 + 1 个可见文件；隐藏项不消耗预算）", b.visited)
	}
}

func TestSizeBudgetExpiredDeadlineReturnsImmediately(t *testing.T) {
	root := t.TempDir()
	writeSizedFiles(t, root, 200, 64)

	b := &sizeBudget{remaining: 100_000, deadline: time.Now().Add(-time.Millisecond)}
	start := time.Now()
	got, truncated := b.size(root)
	elapsed := time.Since(start)

	if !truncated {
		t.Error("过期 deadline 应返回截断标志")
	}
	if got != 0 {
		t.Errorf("时间已过期不应统计任何字节，实际 %d", got)
	}
	if b.visited != 0 {
		t.Errorf("时间已过期不应遍历任何条目，visited = %d", b.visited)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("过期 deadline 应立即返回，实际耗时 %v", elapsed)
	}
}

// newCollectWalker 构造一个只含单条候选的最小 walker，供 collect 调用点测试。
func newCollectWalker(t *testing.T, remaining int, deadline time.Time) (*walker, string, int64) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "AgentData")
	const files, size = 50, 100
	want := writeSizedFiles(t, dir, files, size)

	info := &dirInfo{
		path:     dir,
		score:    minConfidence,
		evidence: []string{"测试用的内容指纹证据"},
	}
	w := &walker{
		home:       home,
		infos:      []*dirInfo{info},
		infoByPath: map[string]*dirInfo{dir: info},
		deadline:   deadline,
		sizeBudget: &sizeBudget{remaining: remaining, deadline: deadline},
	}
	return w, dir, want
}

func TestCollectCarriesSizeTruncation(t *testing.T) {
	w, dir, realSize := newCollectWalker(t, 5, time.Now().Add(time.Minute))

	candidates := w.collect()
	if len(candidates) != 1 {
		t.Fatalf("期望 1 个候选，实际 %d 个：%+v", len(candidates), candidates)
	}
	candidate := candidates[0]
	if candidate.Path != dir {
		t.Errorf("Path = %q，期望 %q", candidate.Path, dir)
	}
	if !candidate.sizeTruncated {
		t.Error("collect 应把「体积统计被截断」反映到 Candidate.sizeTruncated")
	}
	if candidate.Bytes <= 0 || candidate.Bytes >= realSize {
		t.Errorf("Bytes = %d，期望是 (0, %d) 之间的下界", candidate.Bytes, realSize)
	}
	joined := strings.Join(candidate.Evidence, " | ")
	if !strings.Contains(joined, "体积统计达到扫描预算") {
		t.Errorf("Evidence 应说明体积被截断：%v", candidate.Evidence)
	}
}

func TestCollectExpiredDeadlineMarksTruncated(t *testing.T) {
	w, _, _ := newCollectWalker(t, 100_000, time.Now().Add(-time.Millisecond))

	candidates := w.collect()
	if len(candidates) != 1 {
		t.Fatalf("期望 1 个候选，实际 %d 个：%+v", len(candidates), candidates)
	}
	candidate := candidates[0]
	if !candidate.sizeTruncated {
		t.Error("扫描时间用尽时 Candidate.sizeTruncated 应为 true")
	}
	if candidate.Bytes != 0 {
		t.Errorf("扫描时间用尽时 Bytes = %d，期望 0", candidate.Bytes)
	}
	joined := strings.Join(candidate.Evidence, " | ")
	if !strings.Contains(joined, "扫描时间用尽") {
		t.Errorf("Evidence 应说明时间用尽：%v", candidate.Evidence)
	}
}
