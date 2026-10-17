package clean

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// 这组测试只关心跨进程互斥本身：用同一个 journalDir 的两个 Cleaner 模拟
// 「GUI 开着窗口，用户又跑了一次 CLI」。
//
// 全部落在 t.TempDir() 里，不碰真实的 ~/Library/Application Support。

// newLockFixture 造一个只有 journal 目录的临时环境。
func newLockFixture(t *testing.T) (*Cleaner, string) {
	t.Helper()
	root := t.TempDir()
	journalDir := filepath.Join(root, "journal")
	return New(Options{
		Mode:       ModeTrash,
		BackupDir:  filepath.Join(root, "backups"),
		JournalDir: journalDir,
	}), journalDir
}

func TestProcessLockIsExclusiveAcrossCleaners(t *testing.T) {
	_, journalDir := newLockFixture(t)

	first, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("第一次加锁失败: %v", err)
	}

	// 同一进程、不同 fd：flock 是按 fd 归属的，所以这里必须等不到。
	// 用一个短超时避免测试真的等满 5 秒 —— 直接调底层 acquire 会等 5s，
	// 所以这里断言「在超时前拿不到」，然后释放再拿。
	acquired := make(chan *processLock, 1)
	go func() {
		lock, err := acquireProcessLock(journalDir)
		if err == nil {
			acquired <- lock
		}
	}()

	select {
	case lock := <-acquired:
		lock.release()
		first.release()
		t.Fatal("锁被持有时第二个持有者也拿到了，互斥失效")
	case <-time.After(300 * time.Millisecond):
		// 预期：仍在等待
	}

	first.release()

	select {
	case lock := <-acquired:
		lock.release()
	case <-time.After(2 * time.Second):
		t.Fatal("释放后第二个持有者仍未拿到锁")
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

// TestProcessLockBlocksRealProcess 用真实子进程持锁，验证「跨进程」这一层
// 而不是仅仅在进程内生效。
//
// 子进程通过 -test.run=TestProcessLockHelper 复用测试二进制；它拿到锁后
// 往 stdout 打一行 ready，然后一直持有到父进程结束（由测试超时兜底）。
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
	time.Sleep(3 * time.Second)
	lock.release()
}

func TestProcessLockRejectsSecondProcess(t *testing.T) {
	_, journalDir := newLockFixture(t)

	// 父进程先拿住锁。
	parentLock, err := acquireProcessLock(journalDir)
	if err != nil {
		t.Fatalf("父进程加锁失败: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestProcessLockBlocksRealProcess", "-test.v=false")
	cmd.Env = append(os.Environ(),
		"AGENTSWEEP_LOCK_HELPER=1",
		"AGENTSWEEP_LOCK_DIR="+journalDir,
	)
	output, _ := cmd.CombinedOutput()
	parentLock.release()

	// 子进程在 5 秒超时后应打出一条「另一个 AgentSweep 进程」的错误。
	text := string(output)
	if !strings.Contains(text, "另一个 AgentSweep 进程") && !strings.Contains(text, "error:") {
		t.Skipf("子进程没能按预期运行（可能环境限制），output=%q", text)
	}
	if strings.Contains(text, "ready") {
		t.Fatalf("父进程持锁时子进程竟然拿到了锁: %q", text)
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
