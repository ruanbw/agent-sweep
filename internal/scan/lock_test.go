package scan

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// R6：lockHeld 的三态语义。
//
// 「打不开」既可能是「没有 lock 文件」（空闲），也可能是「看不出来」（unknown），
// 绝不能一律当成空闲 —— 那会让正在使用的会话显示成可删。这里覆盖四种输入：
// 空闲、被占用、只读文件/只读目录、真正打不开。

// writeSessionFile 写一个最小的会话文件（含父目录）。
// 不复用 scan_test.go 的 writeFile：这两个测试文件各自独立可读。
func writeSessionFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// holdExclusiveLock 用「另一个 fd」持有 LOCK_EX|LOCK_NB，模拟 Agent 进程正在使用会话。
// flock 是按 open file description 计的，同进程的第二个 fd 同样会拿到 EWOULDBLOCK，
// 所以不需要真的 fork 一个进程。返回幂等的释放函数。
func holdExclusiveLock(t *testing.T, path string, flag int) func() {
	t.Helper()
	file, err := os.OpenFile(path, flag, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatalf("测试夹具无法持有独占锁（%v）", err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}
}

// brokenLockPath 造一个「存在但打不开」的 lock 路径：自指符号链接 → ELOOP。
// 这条路径不依赖权限位，因此在 root / CI 容器里同样成立。
func brokenLockPath(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.Symlink(name, path); err != nil {
		t.Skipf("无法创建自指符号链接：%v", err)
	}
	return path
}

func TestLockHeldFree(t *testing.T) {
	dir := t.TempDir()

	// 没有 lock 文件 = 没有在用。ENOENT 必须是 free，不能是 unknown。
	missing := filepath.Join(dir, "absent.lock")
	if got := lockHeld(missing); got != lockFree {
		t.Errorf("lockHeld(不存在的 lock 文件) = %v，期望 lockFree", got)
	}

	idle := filepath.Join(dir, "idle.lock")
	writeSessionFile(t, idle, "")
	if got := lockHeld(idle); got != lockFree {
		t.Errorf("lockHeld(未加锁的 lock 文件) = %v，期望 lockFree", got)
	}
	// 判断一次不能留下锁：连续两次都应拿到锁。
	if got := lockHeld(idle); got != lockFree {
		t.Errorf("第二次 lockHeld = %v，期望 lockFree（上次的试探锁必须已释放）", got)
	}
}

func TestLockHeldBusy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.lock")
	writeSessionFile(t, path, "")

	release := holdExclusiveLock(t, path, os.O_RDWR)
	defer release()

	if got := lockHeld(path); got != lockBusy {
		t.Fatalf("lockHeld(被独占的 lock 文件) = %v，期望 lockBusy", got)
	}

	release()
	if got := lockHeld(path); got != lockFree {
		t.Errorf("释放后 lockHeld = %v，期望 lockFree", got)
	}
}

// TestLockHeldBusyReadOnlyHolder 验证 O_RDONLY fd 上的 flock 语义没变：
// 只读 fd 既能持有独占锁，也能在别人持有时拿到 EWOULDBLOCK。
func TestLockHeldBusyReadOnlyHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ro-holder.lock")
	writeSessionFile(t, path, "")

	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		// 个别平台上只读 fd 不允许独占锁；那不是本修复的语义，跳过而不是失败。
		_ = file.Close()
		t.Skipf("该平台的只读 fd 不支持 LOCK_EX：%v", err)
	}
	defer func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}()

	if got := lockHeld(path); got != lockBusy {
		t.Errorf("只读 fd 持有锁时 lockHeld = %v，期望 lockBusy", got)
	}
}

// TestLockHeldReadOnlyFile 是本次修复的核心回归：
// 0444 的 lock 文件用 O_RDWR 打不开（旧代码据此返回 false），
// 但用 O_RDONLY + flock 仍能正确判断为 free。
func TestLockHeldReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.lock")
	writeSessionFile(t, path, "")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	// 前置断言：确认本环境下 O_RDWR 真的打不开。打不开 = 用例有意义；
	// 打得开（root、或忽略权限位的文件系统）说明这个用例证明不了什么，跳过。
	if file, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		_ = file.Close()
		t.Skip("该环境下 0444 文件仍可 O_RDWR 打开（root？），用例无区分度")
	}

	if got := lockHeld(path); got != lockFree {
		t.Fatalf("lockHeld(0444 无锁文件) = %v，期望 lockFree（O_RDONLY + flock）", got)
	}
}

// TestLockHeldBusyReadOnlyFile 证明只读权限不会把「被占用」掩盖成 unknown：
// 先持锁，再把文件改成 0444，仍应判为 busy。
func TestLockHeldBusyReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly-busy.lock")
	writeSessionFile(t, path, "")

	release := holdExclusiveLock(t, path, os.O_RDWR)
	defer release()

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if got := lockHeld(path); got != lockBusy {
		t.Errorf("lockHeld(0444 且被独占) = %v，期望 lockBusy", got)
	}
}

// TestLockHeldReadOnlyDir 只读目录里已存在的 lock 文件仍应能判断。
// （目录只读影响创建/删除，不影响打开已有文件；这里锁住的是「不要因为
// 目录不可写就退化成 unknown」这一行为。）
func TestLockHeldReadOnlyDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ro-dir")
	path := filepath.Join(dir, "session.lock")
	writeSessionFile(t, path, "")

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if got := lockHeld(path); got != lockFree {
		t.Errorf("lockHeld(只读目录中的 lock 文件) = %v，期望 lockFree", got)
	}
}

func TestLockHeldUnknown(t *testing.T) {
	t.Run("打不开的路径", func(t *testing.T) {
		// 自指符号链接：open 返回 ELOOP，既不是 ENOENT 也不是可加的锁。
		path := brokenLockPath(t, t.TempDir(), "loop.lock")
		if got := lockHeld(path); got != lockUnknown {
			t.Errorf("lockHeld(ELOOP) = %v，期望 lockUnknown", got)
		}
	})

	t.Run("权限不足", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 无视 chmod 0000，无法构造 EACCES")
		}
		root := t.TempDir()
		dir := filepath.Join(root, "sealed")
		path := filepath.Join(dir, "session.lock")
		writeSessionFile(t, path, "")
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		// 前置断言：确实打不开，否则 unknown 无从谈起。
		if file, err := os.OpenFile(path, os.O_RDONLY, 0); err == nil {
			_ = file.Close()
			t.Skip("该环境下 chmod 0000 的目录仍可访问，无法构造 EACCES")
		}
		if got := lockHeld(path); got != lockUnknown {
			t.Errorf("lockHeld(EACCES) = %v，期望 lockUnknown", got)
		}
	})
}

// TestActiveFromLock 锁死「三态 → Active」的映射：
// 只有确定的 free 才是 false，busy 与 unknown 都必须提示「使用中」。
func TestActiveFromLock(t *testing.T) {
	cases := []struct {
		state lockState
		want  bool
	}{
		{lockFree, false},
		{lockBusy, true},
		{lockUnknown, true}, // 无法判断时保守取 true，宁可多提示一次
	}
	for _, c := range cases {
		if got := activeFromLock(c.state); got != c.want {
			t.Errorf("activeFromLock(%v) = %v，期望 %v", c.state, got, c.want)
		}
	}
}

// TestSessionLockStateMerges 多把 lock 文件合并时，unknown 不能被另一把
// 明确的 free 覆盖掉。
func TestSessionLockStateMerges(t *testing.T) {
	dir := t.TempDir()
	free := filepath.Join(dir, "free.lock")
	writeSessionFile(t, free, "")

	broken := brokenLockPath(t, dir, "broken.lock")

	if got := sessionLockState(free, free); got != lockFree {
		t.Errorf("sessionLockState(free, free) = %v，期望 lockFree", got)
	}
	if got := sessionLockState(free, broken); got != lockUnknown {
		t.Errorf("sessionLockState(free, unknown) = %v，期望 lockUnknown", got)
	}
	if got := sessionLockState(filepath.Join(dir, "missing.lock"), broken); got != lockUnknown {
		t.Errorf("sessionLockState(缺失, unknown) = %v，期望 lockUnknown", got)
	}
}

// buildLockTestSession 走一次真实的 buildFileSession，返回 Active。
func buildLockTestSession(t *testing.T, dir, name string) bool {
	t.Helper()
	path := filepath.Join(dir, name)
	writeSessionFile(t, path, `{"message":{"role":"user","content":"hi"}}`)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	spec := catalog.Spec{ID: "lock-test", Name: "Lock Test"}
	store := catalog.Store{Kind: "jsonl", Glob: "*.jsonl"}
	item := buildFileSession(spec, dir, store, path, info, true)
	if item.ID == "" {
		t.Fatal("buildFileSession 没有产出 id")
	}
	return item.Active
}

// TestBuildFileSessionActive 验证调用点：free → false，busy → true，unknown → true。
func TestBuildFileSessionActive(t *testing.T) {
	t.Run("空闲", func(t *testing.T) {
		dir := t.TempDir()
		if active := buildLockTestSession(t, dir, "idle.jsonl"); active {
			t.Error("无 lock 文件时 Active 应为 false")
		}
	})

	t.Run("被占用", func(t *testing.T) {
		dir := t.TempDir()
		lockPath := filepath.Join(dir, "session.lock")
		writeSessionFile(t, lockPath, "")
		release := holdExclusiveLock(t, lockPath, os.O_RDWR)
		defer release()
		if active := buildLockTestSession(t, dir, "busy.jsonl"); !active {
			t.Error("session.lock 被独占时 Active 应为 true")
		}
	})

	t.Run("无法判断", func(t *testing.T) {
		dir := t.TempDir()
		// 会话 id 取自文件名：broken.lock 就是这条会话的 `{id}.lock`。
		brokenLockPath(t, dir, "broken.lock")
		if active := buildLockTestSession(t, dir, "broken.jsonl"); !active {
			t.Error("lock 文件打不开时 Active 应取保守值 true")
		}
	})
}

// TestScanDirsActive 覆盖目录型会话的调用点（scanDirs 里给 Active 赋值的那行）。
func TestScanDirsActive(t *testing.T) {
	root := t.TempDir()

	freeDir := filepath.Join(root, "sessions", "free-one")
	writeSessionFile(t, filepath.Join(freeDir, "session.lock"), "")

	busyDir := filepath.Join(root, "sessions", "busy-one")
	writeSessionFile(t, filepath.Join(busyDir, "session.lock"), "")
	release := holdExclusiveLock(t, filepath.Join(busyDir, "session.lock"), os.O_RDWR)
	defer release()

	unknownDir := filepath.Join(root, "sessions", "unknown-one")
	if err := os.MkdirAll(unknownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brokenLockPath(t, unknownDir, "session.lock")

	spec := catalog.Spec{ID: "lock-dir", Name: "Lock Dir", Roots: []string{root}}
	store := catalog.Store{Kind: "dir", Glob: "sessions/*"}
	items, err := scanDirs(spec, root, store)
	if err != nil {
		t.Fatal(err)
	}

	byID := make(map[string]session.Session, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	for _, c := range []struct {
		id     string
		active bool
	}{
		{"free-one", false},
		{"busy-one", true},
		{"unknown-one", true},
	} {
		item, ok := byID[c.id]
		if !ok {
			t.Fatalf("缺少会话 %q（实际 %d 条）", c.id, len(items))
		}
		if item.Active != c.active {
			t.Errorf("%s: Active = %v，期望 %v", c.id, item.Active, c.active)
		}
	}
}
