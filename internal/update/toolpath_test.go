package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeTool 在 dir 下造一个同名可执行文件，内容只打印一行。
// 它绝不会被真的执行：这些用例只检查「选中了哪条路径」。
func writeFakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestResolveToolPrefersSystemDirsOverPath 是 C2 的核心回归：PATH 被指向一个
// 只含伪造 plutil 的目录时，解析结果必须是系统目录里的真 plutil。
// 安装流程会替换 /Applications 里的 .app，不能因为 PATH 排前面就换程序执行。
func TestResolveToolPrefersSystemDirsOverPath(t *testing.T) {
	systemPlutil := filepath.Join("/usr/bin", "plutil")
	if _, err := os.Stat(systemPlutil); err != nil {
		t.Skipf("本机没有 %s，跳过：这条用例验证的是「系统目录优先于 PATH」", systemPlutil)
	}

	fakeDir := t.TempDir()
	fakePlutil := writeFakeTool(t, fakeDir, "plutil")
	t.Setenv("PATH", fakeDir)

	got, err := resolveTool("plutil")
	if err != nil {
		t.Fatalf("resolveTool(plutil): %v", err)
	}
	if got == fakePlutil {
		t.Fatalf("解析到了 PATH 里的伪造 plutil：%s", got)
	}
	if got != systemPlutil {
		t.Fatalf("resolveTool(plutil) = %q，want %q", got, systemPlutil)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("解析结果必须是绝对路径，got %q", got)
	}
}

// TestResolveToolFallsBackToPath 保留 PATH 回退的既有语义：系统目录里没有的
// 工具（非 macOS 或自定义安装环境）仍然按 PATH 找，不能一刀切拒装。
func TestResolveToolFallsBackToPath(t *testing.T) {
	const name = "agentsweep-toolpath-fallback"
	fakeDir := t.TempDir()
	want := writeFakeTool(t, fakeDir, name)
	t.Setenv("PATH", fakeDir)

	got, err := resolveTool(name)
	if err != nil {
		t.Fatalf("resolveTool(%s): %v", name, err)
	}
	if got != want {
		t.Fatalf("resolveTool(%s) = %q，want PATH 里的 %q", name, got, want)
	}
}

// TestResolveToolReportsMissingTool 断言找不到时给出带工具名的中文错误，
// 而不是一个空路径继续往下拼命令。
func TestResolveToolReportsMissingTool(t *testing.T) {
	const name = "agentsweep-no-such-tool-xyz"
	if _, err := resolveTool(name); err == nil {
		t.Fatalf("不存在的工具 %s 必须报错", name)
	} else if !strings.Contains(err.Error(), name) {
		t.Fatalf("错误信息应带上工具名 %q，实际为：%v", name, err)
	}
}

// TestResolveToolKeepsExplicitPath 名字里带 / 时按调用方给的路径校验并原样
// 返回，不做目录拼接，也不猜。
func TestResolveToolKeepsExplicitPath(t *testing.T) {
	const shell = "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		t.Skipf("本机没有 %s，跳过", shell)
	}
	got, err := resolveTool(shell)
	if err != nil {
		t.Fatalf("resolveTool(%s): %v", shell, err)
	}
	if got != shell {
		t.Fatalf("resolveTool(%s) = %q，want 原样返回", shell, got)
	}
}

// TestRunCommandIgnoresPollutedPath 验证生产调用链真的经过 resolveTool：
// PATH 被指向只含伪造 plutil 的目录时，runCommand 仍执行系统目录里的真 plutil，
// 伪造脚本一次都不该被跑到。
//
// 用 plutil 而不是 xattr/codesign：前两者会改真实文件或签名，plutil 空参数
// 只会打印用法并返回非零，无副作用。
func TestRunCommandIgnoresPollutedPath(t *testing.T) {
	systemPlutil := filepath.Join("/usr/bin", "plutil")
	if _, err := os.Stat(systemPlutil); err != nil {
		t.Skipf("本机没有 %s，跳过", systemPlutil)
	}

	fakeDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "fake-was-run")
	// 伪造脚本必须用 touch 的绝对路径：PATH 马上会被改成只含 fakeDir，
	// 裸 touch 会找不到，marker 永远不出现，用例就退化成「怎么都通过」。
	touchPath, err := exec.LookPath("touch")
	if err != nil {
		t.Skipf("找不到 touch，跳过: %v", err)
	}
	script := "#!/bin/sh\n" + shellQuote(touchPath) + " " + shellQuote(marker) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "plutil"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PATH", fakeDir)

	// 真 plutil 空参数会报用法错误，这里不关心返回值，只看谁被执行了。
	_, _ = runCommand("plutil")

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("生产 runCommand 执行了 PATH 里的伪造 plutil")
	}
}
