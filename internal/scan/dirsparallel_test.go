package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// R24 的回归测试：scanDirs 以前逐个目录串行读元数据。这里覆盖三件事：
// 派发确实走 forEachFile 的固定 worker 池、输出顺序与目录枚举顺序一致、
// 坏条目/空目录不会让其它成功项消失。

// writeDirSessionFixture 造一个「目录即会话」的最小可解析布局：
// sessions/<id>/meta.json 是 ReadMetaDir 认得的候选文件名。
func writeDirSessionFixture(t *testing.T, root, id, title string) string {
	t.Helper()
	dir := filepath.Join(root, "sessions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建会话目录 %s: %v", dir, err)
	}
	meta := fmt.Sprintf(`{"title":%q,"cwd":"/Users/me/proj"}`, title)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("写元数据 %s: %v", dir, err)
	}
	return dir
}

// TestScanDirsDispatchUsesWorkerPool 确定地量 scanDirs 的派发并发度。
//
// 不采样 runtime.NumGoroutine：把单目录处理函数注入进来，让每个任务先登记
// 「我进来了」再等放行。池里只有 scanWorkers 个 worker，所以放行前主
// goroutine 最多只能收到 scanWorkers 个 started；如果实现退化成串行，等第 2
// 个 started 会超时；退化成「每个目录一个 goroutine」则峰值会冲过上限。
func TestScanDirsDispatchUsesWorkerPool(t *testing.T) {
	root := t.TempDir()
	const dirs = 200
	for i := 0; i < dirs; i++ {
		writeDirSessionFixture(t, root, fmt.Sprintf("case-%03d", i), fmt.Sprintf("会话 %03d", i))
	}

	spec := catalog.Spec{ID: "dirs-parallel", Name: "Dirs Parallel", Roots: []string{root}}
	store := catalog.Store{Kind: "dir", Glob: "sessions/*"}

	var inFlight, peak, handled atomic.Int64
	started := make(chan struct{}, dirs)
	release := make(chan struct{})
	done := make(chan []session.Session, 1)
	errCh := make(chan error, 1)
	go func() {
		out, err := scanDirsWith(spec, root, store,
			func(_ catalog.Spec, _ string, _ catalog.Store, dir string, _ os.FileInfo) (session.Session, []scanNotice) {
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				handled.Add(1)
				started <- struct{}{}
				<-release
				inFlight.Add(-1)
				return session.Session{ID: filepath.Base(dir)}, nil
			})
		if err != nil {
			errCh <- err
			return
		}
		done <- out
	}()

	for i := 0; i < scanWorkers; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatalf("只等到 %d 个并发任务，scanDirs 没有把 %d 个 worker 用起来", i, scanWorkers)
		}
	}
	if got := inFlight.Load(); got != scanWorkers {
		close(release)
		t.Fatalf("放行前同时在飞 %d 个目录，期望 %d 个", got, scanWorkers)
	}
	close(release)

	select {
	case out := <-done:
		if got := peak.Load(); got > scanWorkers {
			t.Fatalf("峰值并发 %d，超过 worker 上限 %d", got, scanWorkers)
		}
		if got := handled.Load(); got != dirs {
			t.Fatalf("处理了 %d 个目录，期望 %d 个", got, dirs)
		}
		if len(out) != dirs {
			t.Fatalf("返回 %d 条会话，期望 %d 条", len(out), dirs)
		}
	case err := <-errCh:
		t.Fatalf("scanDirsWith 返回错误: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("scanDirsWith 在放行之后仍未返回")
	}
	if got := inFlight.Load(); got != 0 {
		t.Fatalf("结束后仍有 %d 个目录在飞", got)
	}
}

// TestScanDirsPreservesEnumerationOrder 钉住「输出顺序 = 目录枚举顺序」：
// 连跑 3 次结果完全一致。每个目录还挂一个自己的旁挂文件，用来确认并发
// 读取时 applyAssoc 不会把路径串到别的会话上。
func TestScanDirsPreservesEnumerationOrder(t *testing.T) {
	root := t.TempDir()
	const dirs = 200
	expected := make([]string, 0, dirs)
	titles := make(map[string]string, dirs)
	for i := 0; i < dirs; i++ {
		id := fmt.Sprintf("case-%03d", i)
		title := fmt.Sprintf("会话 %03d", i)
		writeDirSessionFixture(t, root, id, title)
		expected = append(expected, id)
		titles[id] = title
		// 旁挂文件：/assoc/{id} 相对数据根，只有存在的才会被挂上。
		assoc := filepath.Join(root, "assoc", id)
		if err := os.MkdirAll(filepath.Dir(assoc), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(assoc, []byte("snapshot"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	spec := catalog.Spec{ID: "dirs-order", Name: "Dirs Order", Roots: []string{root}}
	store := catalog.Store{Kind: "dir", Glob: "sessions/*", Assoc: []string{"/assoc/{id}"}}

	var first []string
	for run := 0; run < 3; run++ {
		out, err := scanDirs(spec, root, store)
		if err != nil {
			t.Fatalf("第 %d 次 scanDirs 返回错误: %v", run+1, err)
		}
		if len(out) != len(expected) {
			t.Fatalf("第 %d 次得到 %d 条会话，期望 %d 条", run+1, len(out), len(expected))
		}
		ids := make([]string, len(out))
		for i, item := range out {
			ids[i] = item.ID
			if item.ID != expected[i] {
				t.Fatalf("第 %d 次第 %d 条 ID = %q，期望 %q：输出顺序必须与目录枚举顺序一致",
					run+1, i, item.ID, expected[i])
			}
			if item.Title != titles[item.ID] {
				t.Fatalf("第 %d 次 %q 的标题 = %q，期望 %q：并发读取串了元数据",
					run+1, item.ID, item.Title, titles[item.ID])
			}
			wantAssoc := filepath.Join(root, "assoc", item.ID)
			if !slices.Contains(item.Paths, wantAssoc) {
				t.Fatalf("第 %d 次 %q 缺少自己的旁挂路径 %q（实际 %v）",
					run+1, item.ID, wantAssoc, item.Paths)
			}
			if len(item.Paths) != 2 {
				t.Fatalf("第 %d 次 %q 的路径数 = %d，期望 2：%v",
					run+1, item.ID, len(item.Paths), item.Paths)
			}
		}
		if run == 0 {
			first = ids
			continue
		}
		if !slices.Equal(first, ids) {
			t.Fatalf("第 %d 次输出顺序与第 1 次不同", run+1)
		}
	}
}

// TestScanDirsKeepsValidSessionsAroundBadEntries 覆盖「不丢任务」：普通文件、
// 坏符号链接会被 Stat 跳过；没有元数据的空目录仍按现状输出（fallback 标题），
// 其余有效目录一个都不能少。
func TestScanDirsKeepsValidSessionsAroundBadEntries(t *testing.T) {
	root := t.TempDir()
	const dirs = 200
	expected := make(map[string]string, dirs)
	for i := 0; i < dirs; i++ {
		id := fmt.Sprintf("case-%03d", i)
		title := fmt.Sprintf("会话 %03d", i)
		writeDirSessionFixture(t, root, id, title)
		expected[id] = title
	}

	// 普通文件也会被 glob 的 * 段命中，但 Stat 后不是目录，必须静默跳过。
	notADir := filepath.Join(root, "sessions", "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 坏符号链接：glob 能看到，Stat 会失败，同样必须跳过。
	if err := os.Symlink(filepath.Join(root, "sessions", "missing-target"),
		filepath.Join(root, "sessions", "dangling")); err != nil {
		t.Logf("平台不支持符号链接，跳过坏链接这一项: %v", err)
	}
	// 空目录：没有可解析元数据，但现状语义是照常输出一条 fallback 会话，
	// 并行化不能把它当成「读取失败」悄悄丢掉。
	emptyDir := filepath.Join(root, "sessions", "empty-dir")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := catalog.Spec{ID: "dirs-bad", Name: "Dirs Bad", Roots: []string{root}}
	store := catalog.Store{Kind: "dir", Glob: "sessions/*"}
	out, err := scanDirs(spec, root, store)
	if err != nil {
		t.Fatalf("scanDirs 返回错误: %v", err)
	}

	byID := make(map[string]session.Session, len(out))
	for _, item := range out {
		byID[item.ID] = item
	}
	if len(out) != dirs+1 {
		t.Fatalf("得到 %d 条会话，期望 %d 条（%d 个有效目录 + 1 个空目录）",
			len(out), dirs+1, dirs)
	}
	if _, ok := byID["not-a-dir"]; ok {
		t.Error("普通文件不应出现在会话结果里")
	}
	if _, ok := byID["dangling"]; ok {
		t.Error("坏符号链接不应出现在会话结果里")
	}
	for id, title := range expected {
		item, ok := byID[id]
		if !ok {
			t.Fatalf("缺少有效会话 %q（实际 %d 条）", id, len(out))
		}
		if item.Title != title {
			t.Errorf("%s: 标题 = %q，期望 %q", id, item.Title, title)
		}
	}
	empty, ok := byID["empty-dir"]
	if !ok {
		t.Fatal("空目录应照旧输出一条 fallback 会话")
	}
	if empty.Title != fallbackTitle("empty-dir") {
		t.Errorf("空目录标题 = %q，期望 %q", empty.Title, fallbackTitle("empty-dir"))
	}
}
