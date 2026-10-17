package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试只操作 t.TempDir() 里的假 .app，不调用 ditto/codesign/xattr，
// 也不碰真实应用目录。核心断言是：第二次 rename 失败后，原路径上的旧包必须
// 原样存在 ——「目标位置不能为空」是 replaceBundle 的不变式。

const (
	rollbackVersionFile = "VERSION"
	oldVersion          = "0.1.0"
	newVersion          = "0.1.1"
)

func writeRollbackBundle(t *testing.T, app, version string) {
	t.Helper()
	contents := filepath.Join(app, "Contents")
	if err := os.MkdirAll(filepath.Join(contents, "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contents, rollbackVersionFile), []byte(version), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rollbackBundleVersion(t *testing.T, app string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(app, "Contents", rollbackVersionFile))
	if err != nil {
		t.Fatalf("读 %s 失败：%v", app, err)
	}
	return string(data)
}

// 第二次 rename 失败时，swapBundle 必须把旧包放回 target，且错误信息带路径。
func TestSwapBundleRestoresOldWhenSecondRenameFails(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "AgentSweep.app")
	staged := filepath.Join(parent, "AgentSweep.app.new-1")
	old := filepath.Join(parent, "AgentSweep.app.old-1")
	writeRollbackBundle(t, target, oldVersion)
	writeRollbackBundle(t, staged, newVersion)

	installErr := errors.New("injected: 第二次 rename 失败")
	calls := 0
	rename := func(from, to string) error {
		calls++
		if calls == 2 {
			return installErr
		}
		return os.Rename(from, to)
	}

	err := swapBundle(target, staged, old, rename)
	if err == nil {
		t.Fatal("第二次 rename 失败后 swapBundle 返回 nil，目标位置可能已丢包")
	}
	if !errors.Is(err, installErr) {
		t.Fatalf("错误没有保留底层原因：%v", err)
	}
	if calls != 3 {
		t.Fatalf("rename 调用次数 = %d，期望 3（移开旧包、失败的就位、还原）", calls)
	}
	if got := rollbackBundleVersion(t, target); got != oldVersion {
		t.Fatalf("原路径上的旧包版本 = %q，期望 %q", got, oldVersion)
	}
	if _, statErr := os.Stat(old); !os.IsNotExist(statErr) {
		t.Fatalf("回滚后旧包不应继续留在 %s：%v", old, statErr)
	}
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Fatalf("没换成的新包应仍在 %s 等待调用方清理：%v", staged, statErr)
	}
	if !strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), old) {
		t.Fatalf("错误信息应同时包含目标路径与旧包路径，实际：%v", err)
	}
}

// 还原也失败时，错误必须说明旧包的真实位置，用户才能手动救回来。
func TestSwapBundleReportsOldPathWhenRollbackFails(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "AgentSweep.app")
	staged := filepath.Join(parent, "AgentSweep.app.new-1")
	old := filepath.Join(parent, "AgentSweep.app.old-1")
	writeRollbackBundle(t, target, oldVersion)
	writeRollbackBundle(t, staged, newVersion)

	calls := 0
	rename := func(from, to string) error {
		calls++
		if calls == 1 {
			return os.Rename(from, to)
		}
		return fmt.Errorf("injected: 第 %d 次 rename 失败", calls)
	}

	err := swapBundle(target, staged, old, rename)
	if err == nil {
		t.Fatal("还原失败后 swapBundle 返回 nil")
	}
	if calls != 3 {
		t.Fatalf("rename 调用次数 = %d，期望 3（含一次失败的还原）", calls)
	}
	// 还原失败是唯一允许目标为空的状态，但旧包必须完整留在 old 位置。
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("还原失败时目标位置不应有内容：%v", statErr)
	}
	if got := rollbackBundleVersion(t, old); got != oldVersion {
		t.Fatalf("旧包在 %s 的内容 = %q，期望 %q", old, got, oldVersion)
	}
	for _, want := range []string{old, target, "手动"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

// 第一次 rename 就失败时，目标位置根本没动，必须原样保留旧包。
func TestSwapBundleLeavesTargetWhenFirstRenameFails(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "AgentSweep.app")
	staged := filepath.Join(parent, "AgentSweep.app.new-1")
	old := filepath.Join(parent, "AgentSweep.app.old-1")
	writeRollbackBundle(t, target, oldVersion)
	writeRollbackBundle(t, staged, newVersion)

	failErr := errors.New("injected: 第一次 rename 失败")
	rename := func(from, to string) error { return failErr }

	err := swapBundle(target, staged, old, rename)
	if err == nil {
		t.Fatal("第一次 rename 失败后 swapBundle 返回 nil")
	}
	if !errors.Is(err, failErr) {
		t.Fatalf("错误没有保留底层原因：%v", err)
	}
	if got := rollbackBundleVersion(t, target); got != oldVersion {
		t.Fatalf("目标位置版本 = %q，期望旧版本 %q", got, oldVersion)
	}
	if _, statErr := os.Stat(old); !os.IsNotExist(statErr) {
		t.Fatalf("未发生换位时不应出现 %s：%v", old, statErr)
	}
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Fatalf("新包应仍在 %s：%v", staged, statErr)
	}
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("错误信息应包含目标路径 %s，实际：%v", target, err)
	}
}

// 成功路径：旧包先挪到 old 位置，新包就位，最后旧包被清理。
func TestSwapBundleSuccessReplacesTargetAndCleansOld(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "AgentSweep.app")
	staged := filepath.Join(parent, "AgentSweep.app.new-1")
	old := filepath.Join(parent, "AgentSweep.app.old-1")
	writeRollbackBundle(t, target, oldVersion)
	writeRollbackBundle(t, staged, newVersion)

	if err := swapBundle(target, staged, old, os.Rename); err != nil {
		t.Fatalf("swapBundle 成功路径失败：%v", err)
	}
	if got := rollbackBundleVersion(t, target); got != newVersion {
		t.Fatalf("目标位置版本 = %q，期望新版本 %q", got, newVersion)
	}
	// rename 之后、清理之前，旧包确实在 old 位置，而不是一次性被覆盖。
	if got := rollbackBundleVersion(t, old); got != oldVersion {
		t.Fatalf("old 位置的旧包版本 = %q，期望 %q", got, oldVersion)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("staged 路径应已被消费：%v", statErr)
	}

	if err := removeOldBundle(old); err != nil {
		t.Fatalf("清理旧包失败：%v", err)
	}
	if _, statErr := os.Stat(old); !os.IsNotExist(statErr) {
		t.Fatalf("成功路径结束后旧包仍留在 %s：%v", old, statErr)
	}
}

// 删旧包失败不能被吞掉：错误要指明新版本已就位，以及旧包还留在哪。
func TestRemoveOldBundleReportsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 会绕过目录权限，无法构造 RemoveAll 失败")
	}
	parent := t.TempDir()
	old := filepath.Join(parent, "AgentSweep.app.old-1")
	writeRollbackBundle(t, old, oldVersion)
	// 目录不可读时 RemoveAll 无法枚举内容，必然失败。
	if err := os.Chmod(old, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(old, 0o755) })

	err := removeOldBundle(old)
	if err == nil {
		t.Fatal("旧包不可读时 removeOldBundle 返回 nil，删除失败被吞掉了")
	}
	for _, want := range []string{old, "新版本已就位"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}
