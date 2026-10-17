package scan

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// keepScanStore 允许单个用例替换 scanStoreFn，并在用例结束时还原默认实现。
// 包内所有用例都不并行执行，所以这个包级注入点是安全的。
func keepScanStore(t *testing.T) func(catalog.Spec, string, catalog.Store) ([]session.Session, error) {
	t.Helper()
	original := scanStoreFn
	t.Cleanup(func() { scanStoreFn = original })
	return original
}

// TestOneRecoversPanic 回归 data #8：One 里没有任何 recover。
//
// GUI 的 runScan 有自己的兜底，但 CLI `agentsweep scan` 直接调 scan.All ——
// 一个畸形文件或驱动 panic 就会让整个进程退出。One 必须把 panic 收成一条
// Agent 级错误，并且不影响同批次的其它 Agent。
func TestOneRecoversPanic(t *testing.T) {
	original := keepScanStore(t)
	home := t.TempDir()
	writeScanFixture(t, filepath.Join(home, "sessions", "a.jsonl"), scanFixture)

	scanStoreFn = func(spec catalog.Spec, dir string, store catalog.Store) ([]session.Session, error) {
		if spec.ID == "boom" {
			panic("坏文件把解析器打崩了")
		}
		return original(spec, dir, store)
	}

	makeSpec := func(id, name string) catalog.Spec {
		return catalog.Spec{
			ID:     id,
			Name:   name,
			Roots:  []string{home},
			Stores: []catalog.Store{{Kind: "jsonl", Glob: "sessions/*.jsonl"}},
		}
	}

	// panic 不能穿出 One：穿出去这个测试会直接 panic 失败。
	boom := One(makeSpec("boom", "Boom Agent"))
	if !strings.Contains(boom.Err, "panic") {
		t.Fatalf("panic 应被收成 Agent 级错误，实际 Err = %q", boom.Err)
	}
	if !strings.Contains(boom.Err, "Boom Agent") {
		t.Fatalf("错误信息应点名是哪个 Agent，实际 Err = %q", boom.Err)
	}

	// 另一个 Agent 走真实 scanStore，必须照常出结果。
	ok := One(makeSpec("ok", "OK Agent"))
	if ok.Err != "" {
		t.Fatalf("另一个 Agent 不该被 panic 连累，Err = %q", ok.Err)
	}
	if !ok.Installed {
		t.Fatal("正常 Agent 应标记为已安装")
	}
	if len(ok.Sessions) != 1 {
		t.Fatalf("正常 Agent 的会话数 = %d，want 1", len(ok.Sessions))
	}
}

// TestAllWithProgressCallbacksAreMonotonic 回归 data #9：回调在解锁之后调用，
// done 虽单调递增，但到达顺序可能倒序（先 3/30 再到 2/30）。
//
// 这里通过注入让下标越大的 Agent 越早完成：如果回调没有串行化，n 一定倒退。
func TestAllWithProgressCallbacksAreMonotonic(t *testing.T) {
	keepScanStore(t)
	const total = 30
	home := t.TempDir()

	scanStoreFn = func(spec catalog.Spec, dir string, store catalog.Store) ([]session.Session, error) {
		var index int
		_, _ = fmt.Sscanf(spec.ID, "agent-%d", &index)
		// 0 号睡最久、29 号最快完成，制造与声明顺序相反的完成顺序。
		time.Sleep(time.Duration(total-index) * time.Millisecond)
		return nil, nil
	}

	specs := make([]catalog.Spec, total)
	for i := range specs {
		specs[i] = catalog.Spec{
			ID:     fmt.Sprintf("agent-%d", i),
			Name:   fmt.Sprintf("Agent %d", i),
			Roots:  []string{home},
			Stores: []catalog.Store{{Kind: "jsonl", Glob: "sessions/*.jsonl"}},
		}
	}

	var mu sync.Mutex
	var dones []int
	var totals []int
	var currents []string
	results := AllWithProgress(specs, func(done, count int, current string) {
		// 回调自己的锁与 scan 内部那把无关：这里只保护测试记录。
		mu.Lock()
		dones = append(dones, done)
		totals = append(totals, count)
		currents = append(currents, current)
		mu.Unlock()
	})

	if len(results) != total {
		t.Fatalf("结果数 = %d，want %d", len(results), total)
	}
	for _, result := range results {
		if result.Err != "" {
			t.Fatalf("注入的 Store 不该报错: %s: %s", result.AgentID, result.Err)
		}
	}
	if len(dones) != total {
		t.Fatalf("回调次数 = %d，want %d", len(dones), total)
	}
	for i := range dones {
		// 核心断言：到达顺序与 n 严格一致，n 永不倒退。
		if dones[i] != i+1 {
			t.Fatalf("第 %d 个回调 done = %d，want %d（回调乱序/倒退）", i, dones[i], i+1)
		}
		if totals[i] != total {
			t.Fatalf("第 %d 个回调 total = %d，want %d", i, totals[i], total)
		}
		if currents[i] == "" {
			t.Fatalf("第 %d 个回调的 current 为空，UI 无法显示正在扫谁", i)
		}
	}
}

// TestAllKeepsDefaultScanStore 确认注入复原后默认路径没被改坏：
// 空列表返回空，真实 Spec 走真实 scanStore 仍能扫出会话。
func TestAllKeepsDefaultScanStore(t *testing.T) {
	if results := All(nil); len(results) != 0 {
		t.Fatalf("空 Spec 列表应返回空结果，got %d", len(results))
	}

	home := t.TempDir()
	writeScanFixture(t, filepath.Join(home, "sessions", "b.jsonl"), scanFixture)
	results := All([]catalog.Spec{{
		ID:     "real",
		Name:   "Real Agent",
		Roots:  []string{home},
		Stores: []catalog.Store{{Kind: "jsonl", Glob: "sessions/*.jsonl"}},
	}})
	if len(results) != 1 {
		t.Fatalf("结果数 = %d，want 1", len(results))
	}
	if results[0].Err != "" {
		t.Fatalf("默认路径不该报错: %s", results[0].Err)
	}
	if len(results[0].Sessions) != 1 {
		t.Fatalf("默认 scanStore 未生效，会话数 = %d，want 1", len(results[0].Sessions))
	}
}
