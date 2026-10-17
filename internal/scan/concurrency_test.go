package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// R5 的回归测试：scanFiles 原来先 go 再抢信号量，glob 命中几万个文件时
// 会创建同样多的 goroutine 一起阻塞在 sem <-，内存和调度都跟着文件数涨。
// 这里不去采样 runtime.NumGoroutine（受测试框架和其它包影响，很脆），
// 而是直接测抽出来的 worker 池 forEachFile，用原子计数器量峰值并发。

// scanFixture 是一份最小但真实的 JSONL 会话内容。
const scanFixture = `{"type":"user","cwd":"/Users/me/proj","message":{"role":"user","content":"并发展示"}}
`

// writeScanFixture 写一个会话文件；目录不存在就建。
// 名字避开 scan_test.go 的 writeFile，测试文件之间不共享 helper。
func writeScanFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录 %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写夹具 %s: %v", path, err)
	}
}

// seedJSONLFiles 在 root/projects/p 下造 n 个真实会话文件，返回路径。
func seedJSONLFiles(t *testing.T, root string, n int) []string {
	t.Helper()
	files := make([]string, n)
	for i := range files {
		path := filepath.Join(root, "projects", "p", fmt.Sprintf("s-%03d.jsonl", i))
		writeScanFixture(t, path, scanFixture)
		files[i] = path
	}
	return files
}

// TestScanWorkersKeepsSixteen 钉住并发度：R5 只改「在飞 goroutine 的创建方式」，
// 16 这个值属于既有行为，不该在重构里被顺手改掉。
func TestScanWorkersKeepsSixteen(t *testing.T) {
	if scanWorkers != 16 {
		t.Fatalf("scanWorkers = %d，期望保持 16 路并发", scanWorkers)
	}
}

// TestForEachFileCapsInFlight 验证池里同时在飞的任务数真正封顶。
//
// 做法是让每个任务先登记「我进来了」再等放行：池里只有 workers 个 worker，
// 所以主 goroutine 最多只能收到 workers 个 started。如果实现退化成
// 「每个文件一个 goroutine」，收完 workers 个之后关闭 release，峰值会立刻
// 冲过 workers，断言就能抓住；如果退化成串行，等第 2 个 started 会超时。
func TestForEachFileCapsInFlight(t *testing.T) {
	root := t.TempDir()
	files := seedJSONLFiles(t, root, 200)
	const workers = 4

	var inFlight, peak, processed atomic.Int64
	started := make(chan struct{}, len(files))
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		forEachFile(files, workers, func(_ int, _ string) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			processed.Add(1)
			started <- struct{}{}
			<-release
			inFlight.Add(-1)
		})
	}()

	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatalf("只等到 %d 个并发任务，worker 池没有把 %d 个 worker 用起来", i, workers)
		}
	}
	if got := inFlight.Load(); got != workers {
		close(release)
		t.Fatalf("放行前同时在飞 %d 个任务，期望 %d 个", got, workers)
	}
	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("forEachFile 在放行之后仍未返回")
	}
	if got := peak.Load(); got > workers {
		t.Fatalf("峰值并发 %d，超过 worker 上限 %d", got, workers)
	}
	if got := processed.Load(); got != int64(len(files)) {
		t.Fatalf("处理了 %d 个文件，期望 %d 个", got, len(files))
	}
	if got := inFlight.Load(); got != 0 {
		t.Fatalf("结束后仍有 %d 个任务在飞", got)
	}
}

// TestForEachFileVisitsEveryIndexOnce 覆盖「不丢任务」与「下标保序」：
// 200 个任务里先删掉 20 个文件，模拟 glob 与 Stat 之间的消失；fn 仍然必须
// 对全部 200 个下标各调用一次，且拿到的 path 与输入下标对应。
func TestForEachFileVisitsEveryIndexOnce(t *testing.T) {
	root := t.TempDir()
	files := seedJSONLFiles(t, root, 200)
	removed := 0
	for i := 0; i < len(files); i += 10 {
		if err := os.Remove(files[i]); err != nil {
			t.Fatalf("删除夹具 %s: %v", files[i], err)
		}
		removed++
	}

	seen := make([]atomic.Int32, len(files))
	var processed, statFailures, mismatches atomic.Int64
	forEachFile(files, 7, func(i int, path string) {
		seen[i].Add(1)
		processed.Add(1)
		if path != files[i] {
			// 只记数，不在 worker 里调 t：等主 goroutine 收尾后再断言。
			mismatches.Add(1)
			return
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			statFailures.Add(1)
		}
	})

	if got := processed.Load(); got != int64(len(files)) {
		t.Fatalf("处理了 %d 个任务，期望 %d 个（Stat 失败也不能让任务消失）", got, len(files))
	}
	if got := mismatches.Load(); got != 0 {
		t.Fatalf("%d 个任务拿到的下标与路径不匹配", got)
	}
	if got := statFailures.Load(); got != int64(removed) {
		t.Fatalf("Stat 失败 %d 个，期望 %d 个", got, removed)
	}
	for i := range files {
		if got := seen[i].Load(); got != 1 {
			t.Fatalf("下标 %d 被调用 %d 次，期望 1 次", i, got)
		}
	}
}

// TestScanFilesPreservesGlobOrder 验证 scanFiles 的输出顺序就是 glob 顺序：
// 结果按下标写回，过滤空 ID 时保序。夹具里故意插一个目录和一个坏符号链接
// （Stat 会跳过），并在它们之后继续放有效文件，确保跳过不会吞掉后面的成功项。
func TestScanFilesPreservesGlobOrder(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "p")

	var expected []string
	for i := 0; i < 198; i++ {
		name := fmt.Sprintf("case-%03d.jsonl", i)
		writeScanFixture(t, filepath.Join(project, name), scanFixture)
		expected = append(expected, strings.TrimSuffix(name, ".jsonl"))
	}
	// 目录也会被 glob 的 * 段命中；os.Stat 能认出它是目录，应当静默跳过。
	if err := os.Mkdir(filepath.Join(project, "case-198-dir.jsonl"), 0o755); err != nil {
		t.Fatalf("建目录夹具: %v", err)
	}
	// 坏符号链接：glob 能看到路径，Stat 会失败，同样必须跳过。
	if err := os.Symlink(filepath.Join(project, "missing-target"),
		filepath.Join(project, "case-199-dangling.jsonl")); err != nil {
		t.Logf("平台不支持符号链接，跳过坏链接这一项: %v", err)
	}
	// 排在两个跳过项之后的有效文件：证明跳过不会截断或吞掉后续结果。
	for _, name := range []string{"case-200.jsonl", "case-201.jsonl"} {
		writeScanFixture(t, filepath.Join(project, name), scanFixture)
		expected = append(expected, strings.TrimSuffix(name, ".jsonl"))
	}
	sort.Strings(expected) // fsx.Glob 按字典序返回

	spec := catalog.Spec{ID: "order", Name: "Order", Roots: []string{root}}
	store := catalog.Store{Kind: "jsonl", Glob: "projects/*/*.jsonl"}
	out, err := scanFiles(spec, root, store, true)
	if err != nil {
		t.Fatalf("scanFiles 返回错误: %v", err)
	}
	if len(out) != len(expected) {
		t.Fatalf("得到 %d 条会话，期望 %d 条；目录/坏链接应跳过，但成功项不能丢", len(out), len(expected))
	}
	for i := range expected {
		if out[i].ID != expected[i] {
			t.Fatalf("第 %d 条 ID = %q，期望 %q：输出顺序必须与 glob 顺序一致", i, out[i].ID, expected[i])
		}
	}
}
