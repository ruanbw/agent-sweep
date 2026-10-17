package clean

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// 这组测试只关心跨进程互斥本身：用同一个 journalDir 的两个 Cleaner 模拟
// 「GUI 开着窗口，用户又跑了一次 CLI」。
//
// 全部落在 t.TempDir() 里，不碰真实的 ~/Library/Application Support。

// newLockFixture 造一个只有 journal 目录的临时环境。
//
// HOME 必须一起指向临时目录：fixture 用 trash 模式，而 Delete 会把正文挪到
// $HOME/.Trash/AgentSweep/<batch>。不隔离 HOME，测试就会往开发者真实的废纸篓里
// 扔文件 —— 那样跑一次测试就污染一次用户数据。
func newLockFixture(t *testing.T) (*Cleaner, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	journalDir := filepath.Join(root, "journal")
	return New(Options{
		Mode:       ModeTrash,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: journalDir,
	}), journalDir
}

// shortenLockTimeout 把包级等待上限临时调短，并复原。
//
// 没有它就没办法低成本覆盖「被占用 → 超时」：真实的 5 秒上限会让一个用例
// 把整包测试时间拖长 5 秒以上。必须在任何加锁动作之前调用；包内用例没有
// t.Parallel，所以不存在并发改写它的窗口。
func shortenLockTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := processLockTimeout
	processLockTimeout = d
	t.Cleanup(func() { processLockTimeout = previous })
}

// lockFilePath 复刻 acquireProcessLock 的锁文件位置（journal 目录的父目录）。
// 测试用它断言锁文件本身没被当成 journal 收进批次列表。
func lockFilePath(journalDir string) string {
	return filepath.Join(filepath.Dir(journalDir), ".clean.lock")
}

func TestProcessLockIsExclusiveAcrossCleaners(t *testing.T) {
	_, journalDir := newLockFixture(t)

	first, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("第一次加锁失败: %v", err)
	}

	// 同一进程、不同 fd：flock 是按 open file description 归属的，所以即使
	// 同进程的第二个调用者也必须等不到 —— 这正是「不能每次 Lock 重新 open
	// 一把锁」的那条要求。
	shortenLockTimeout(t, 200*time.Millisecond)
	done := make(chan struct{})
	acquired := make(chan *processLock, 1)
	go func() {
		defer close(done)
		lock, err := acquireProcessLock(journalDir)
		if err == nil {
			acquired <- lock
		}
	}()

	// 第一个持有期间，第二个必须拿不到。
	select {
	case lock := <-acquired:
		lock.release()
		first.release()
		t.Fatal("锁被持有时第二个持有者也拿到了，互斥失效")
	case <-done:
		// 预期：等到超时后放弃。
	}

	// 超时错误必须是「被占用」而不是「无法加锁」——两者给用户的行动建议不同。
	// 这里直接再请求一次，确认拿到的是可读的超时文案。
	if _, err := acquireProcessLock(journalDir); err == nil {
		first.release()
		t.Fatal("持有期间第二次 acquire 应超时失败")
	} else if !strings.Contains(err.Error(), "另一个 AgentSweep 进程") {
		first.release()
		t.Fatalf("超时错误应说明是另一个进程占用, got: %v", err)
	}

	// 释放后必须马上拿得到。
	first.release()
	second, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("释放后应立即拿到锁: %v", err)
	}
	second.release()
}

// TestProcessLockTimeoutIsReadable 锁被占住且不释放时，错误必须点名「另一个
// 进程」，而不是把 EWOULDBLOCK 原样抛给用户。
func TestProcessLockTimeoutIsReadable(t *testing.T) {
	_, journalDir := newLockFixture(t)
	shortenLockTimeout(t, 150*time.Millisecond)

	holder, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("持有者加锁失败: %v", err)
	}
	defer holder.release()

	start := time.Now()
	_, err = acquireProcessLock(journalDir)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("锁被占用时应超时返回错误")
	}
	if !strings.Contains(err.Error(), "另一个") && !strings.Contains(err.Error(), "占用") {
		t.Fatalf("错误应含「另一个」或「占用」字样, got: %v", err)
	}
	// 必须真的等过一段（不能立刻返回），但也不能无限等。
	if elapsed < 100*time.Millisecond {
		t.Fatalf("应在等待约 150ms 后才超时, 实际 %v", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("超时等待过久: %v", elapsed)
	}
}

// TestProcessLockUnlockableDistinguishedFromBusy 区分「无法加锁」与「被占用」：
// 锁文件打不开（自指符号链接 → ELOOP）时报的是加锁失败，不该误报成另一个进程。
func TestProcessLockUnlockableDistinguishedFromBusy(t *testing.T) {
	root := t.TempDir()
	journalDir := filepath.Join(root, "journal")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// 锁文件位置放一个自指链接：open 直接 ELOOP，任何进程都无法加锁。
	link := lockFilePath(journalDir)
	if err := os.Symlink(filepath.Base(link), link); err != nil {
		t.Skipf("无法创建自指符号链接：%v", err)
	}
	cleaner := New(Options{
		Mode:       ModePermanent,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: journalDir,
	})

	victim := filepath.Join(root, "home", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatalf("准备 victim: %v", err)
	}
	if err := os.WriteFile(victim, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("准备 victim: %v", err)
	}

	shortenLockTimeout(t, 150*time.Millisecond)
	_, err := cleaner.Delete(catalog.Spec{ID: "demo"}, filepath.Dir(victim), nil)
	if err == nil {
		t.Fatal("锁文件打不开时 Delete 必须返回错误")
	}
	if !strings.Contains(err.Error(), "清理锁") {
		t.Fatalf("错误应说明是清理锁打不开, got: %v", err)
	}
	if strings.Contains(err.Error(), "另一个 AgentSweep 进程") {
		t.Fatalf("打不开锁不该被误报成「另一个进程正在清理」: %v", err)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("加锁失败时不应动用户文件: %v", statErr)
	}
}

// TestLockFileIsNotTreatedAsJournal 要求 5：锁文件不能被当成 journal 读走。
//
// 锁文件叫 .clean.lock 且落在 journal 的父目录，readJournal 只按 "<batchID>.json"
// 取文件、Batches 会跳过 "." 前缀与非 .json 文件，所以三点都要成立：
//   - 锁文件确实建出来了；
//   - Batches 不会把它当批次；
//   - 拿它当 batchID 去 readJournal 也读不到东西。
func TestLockFileIsNotTreatedAsJournal(t *testing.T) {
	cleaner, journalDir := newLockFixture(t)
	lock, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("加锁失败: %v", err)
	}
	defer lock.release()

	lockPath := lockFilePath(journalDir)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("锁文件应已存在 %s: %v", lockPath, err)
	}

	// 一次真实删除，确认锁文件不会混进批次列表。
	home := t.TempDir()
	body := filepath.Join(home, "s", "a.jsonl")
	if err := os.MkdirAll(filepath.Dir(body), 0o755); err != nil {
		t.Fatalf("准备 body: %v", err)
	}
	if err := os.WriteFile(body, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("准备 body: %v", err)
	}

	// Delete 自己会再取一次锁：先在同一个进程里放掉，避免测试持有者把它挡住。
	lock.release()
	res, err := cleaner.Delete(catalog.Spec{ID: "demo"}, home, []session.Session{fileSession("a", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	batches, err := cleaner.Batches(0)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(batches) != 1 || batches[0].ID != res.BatchID {
		t.Fatalf("Batches = %+v, want 仅 %s", batches, res.BatchID)
	}
	for _, b := range batches {
		if strings.Contains(b.ID, "lock") {
			t.Fatalf("锁文件被当成了 journal 批次: %+v", b)
		}
	}

	// 直接拿锁文件名当 batchID：必须读不到（不是解析成一份空 journal）。
	if _, err := cleaner.readJournal(".clean.lock"); err == nil {
		t.Fatal("readJournal 不应把锁文件读成 journal")
	}
	// journal 目录里也不该出现锁文件。
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", journalDir, err)
	}
	for _, e := range entries {
		if e.Name() == ".clean.lock" {
			t.Fatalf("锁文件不应落在 journal 目录里: %s", filepath.Join(journalDir, e.Name()))
		}
	}
}

// TestDeleteWaitsForLockWithoutTouchingData 要求 5 的回归线：锁被别的持有者
// 占住时，Delete 必须在等到超时后报错退出，且用户数据一个字节都不能动。
func TestDeleteWaitsForLockWithoutTouchingData(t *testing.T) {
	cleaner, journalDir := newLockFixture(t)
	shortenLockTimeout(t, 200*time.Millisecond)

	home := t.TempDir()
	body := filepath.Join(home, "s", "keep.jsonl")
	content := "must survive\n"
	if err := os.MkdirAll(filepath.Dir(body), 0o755); err != nil {
		t.Fatalf("准备 body: %v", err)
	}
	if err := os.WriteFile(body, []byte(content), 0o644); err != nil {
		t.Fatalf("准备 body: %v", err)
	}

	// 另一个「进程」（这里用另一个 fd 的持有者）占住锁不放。
	holder, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("持有者加锁失败: %v", err)
	}
	defer holder.release()

	res, err := cleaner.Delete(catalog.Spec{ID: "demo"}, home, []session.Session{fileSession("keep", body)})
	if err == nil {
		t.Fatal("锁被占用时 Delete 必须返回错误")
	}
	if !strings.Contains(err.Error(), "另一个 AgentSweep 进程") {
		t.Fatalf("错误应说明是另一个进程在清理, got: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("未拿到锁时 Count = %d, want 0", res.Count)
	}
	if res.BatchID != "" {
		t.Fatalf("未拿到锁时不应预留批次, got %q", res.BatchID)
	}
	// 目标文件必须原样留在原位。
	if got, readErr := os.ReadFile(body); readErr != nil || string(got) != content {
		t.Fatalf("加锁失败时文件被改动: content=%q err=%v", got, readErr)
	}
	// 连初始 journal 都不该落盘。
	if entries, readErr := os.ReadDir(journalDir); readErr == nil && len(entries) != 0 {
		t.Fatalf("未拿到锁时不应写 journal, got %v", entryNames(entries))
	}

	// 释放之后同一份 Delete 必须成功 —— 证明失败只是「等不到锁」，不是状态被弄坏。
	holder.release()
	res, err = cleaner.Delete(catalog.Spec{ID: "demo"}, home, []session.Session{fileSession("keep", body)})
	if err != nil {
		t.Fatalf("锁释放后 Delete 应成功: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("释放后 Count = %d, want 1", res.Count)
	}
}

func TestProcessLockDifferentDirsDoNotBlock(t *testing.T) {
	_, dirA := newLockFixture(t)
	_, dirB := newLockFixture(t)

	lockA, err := acquireProcessLock(dirA)
	if err != nil {
		t.Fatalf("A 加锁失败: %v", err)
	}
	defer lockA.release()

	done := make(chan error, 1)
	go func() {
		lock, err := acquireProcessLock(dirB)
		if err == nil {
			lock.release()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("不同 journal 目录不应该互相阻塞: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("不同 journal 目录之间发生了阻塞")
	}
}

// TestProcessLockBlocksRealProcess 是真实跨进程检查的**子进程侧** helper。
//
// 父进程用 -test.run 只选中本函数把它拉起来；它拿到锁后打一行 ready 并一直
// 持有一段，父进程据此断言自己拿不到锁。
func TestProcessLockBlocksRealProcess(t *testing.T) {
	if os.Getenv("AGENTSWEEP_LOCK_HELPER") != "1" {
		t.Skip("仅作为子进程 helper 运行")
	}
	journalDir := os.Getenv("AGENTSWEEP_LOCK_DIR")
	lock, err := acquireProcessLock(journalDir)
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		return
	}
	os.Stdout.WriteString("ready\n")
	// 留足父进程观察的时间；父进程释放后本进程也随之结束。
	time.Sleep(2 * time.Second)
	lock.release()
}

// TestProcessLockCrossesRealProcessBoundary 起一个真子进程持锁，父进程断言
// 自己等不到 —— 这才是 sync.Mutex 覆盖不到的那一层。
func TestProcessLockCrossesRealProcessBoundary(t *testing.T) {
	_, journalDir := newLockFixture(t)
	shortenLockTimeout(t, 300*time.Millisecond)

	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessLockBlocksRealProcess$")
	cmd.Env = append(os.Environ(),
		"AGENTSWEEP_LOCK_HELPER=1",
		"AGENTSWEEP_LOCK_DIR="+journalDir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动子进程（环境限制）: %v", err)
	}
	// 无论断言怎么结束，都别把子进程漏在后台。
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// 等子进程明确报出 ready，再尝试加锁；否则会与「子进程还没起来」混淆。
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "ready" || strings.HasPrefix(line, "error:") {
				ready <- line
				return
			}
		}
		ready <- ""
	}()

	select {
	case line := <-ready:
		if line != "ready" {
			t.Skipf("子进程未能持锁（环境限制）: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Skip("子进程未在 5 秒内就绪（环境限制）")
	}

	// 子进程持锁期间，父进程必须失败，并报「另一个进程」。
	if _, err := acquireProcessLock(journalDir); err == nil {
		t.Fatal("子进程持锁期间父进程竟然拿到了锁，flock 没有跨进程生效")
	} else if !strings.Contains(err.Error(), "另一个") {
		t.Fatalf("跨进程占用时应报「另一个」进程, got: %v", err)
	}
}

// TestDeleteUsesProcessLock 是接线检查：Delete 必须真的去抢这把锁。
//
// 让**锁所在目录**不可创建（父路径是普通文件）时，Delete 应在碰任何用户文件
// 之前就失败并报出加锁错误。锁文件落在 JournalDir 的父目录，所以这里把父目录
// 设成 blocker 下面的路径。
func TestDeleteUsesProcessLock(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}

	cleaner := New(Options{
		Mode:       ModePermanent,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: filepath.Join(blocker, "app", "journal"), // 锁目录不可创建
	})

	victim := filepath.Join(root, "home", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatalf("准备 victim 失败: %v", err)
	}
	if err := os.WriteFile(victim, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("准备 victim 失败: %v", err)
	}

	_, err := cleaner.Delete(catalog.Spec{ID: "demo"}, filepath.Dir(victim), nil)
	if err == nil {
		t.Fatal("加锁失败时 Delete 必须返回错误")
	}
	// 两种失败形态都算「加锁阶段就停了」：锁目录建不出来，或锁文件打不开。
	if !strings.Contains(err.Error(), "清理锁") &&
		!strings.Contains(err.Error(), "应用数据目录") &&
		!strings.Contains(err.Error(), "journal 目录") {
		t.Fatalf("错误应说明是加锁失败，got: %v", err)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("加锁失败时不应动用户文件: %v", statErr)
	}
}
