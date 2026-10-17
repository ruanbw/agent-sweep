package update

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试一次都不执行真实的 codesign / xattr / ditto：它们会改掉真实签名，
// 在开发机上跑测试不该留下任何副作用。命令执行通过 runCommand 参数注入替掉，
// 因此这里没有包级可变状态，也不需要 t.Parallel 之类的额外约束。
//
// 核心断言有两条：
//  1. 去隔离与重签的失败必须变成调用方看得见的错误，并带上命令与输出摘要；
//  2. 两个都失败时两条原因都要在，不能被最后一条覆盖。

var (
	errXattrInjected    = errors.New("injected: xattr 失败")
	errCodesignInjected = errors.New("injected: codesign 失败")
)

type commandCall struct {
	name string
	args []string
}

// stubCommand 返回一个假的命令执行器，并记录每次调用，供断言命令与参数。
func stubCommand(handler func(name string, args []string) ([]byte, error)) (commandFunc, *[]commandCall) {
	calls := new([]commandCall)
	run := func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, commandCall{name: name, args: append([]string(nil), args...)})
		return handler(name, args)
	}
	return run, calls
}

func hasArg(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

// requireErrorContains 断言 err 非空，且文案包含全部关键词。
func requireErrorContains(t *testing.T, err error, wanted ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望返回错误，得到 nil")
	}
	for _, want := range wanted {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
}

func TestEnsureSignatureKeepsExistingSignature(t *testing.T) {
	app := filepath.Join(t.TempDir(), "AgentSweep.app")
	run, calls := stubCommand(func(name string, args []string) ([]byte, error) {
		return nil, nil
	})

	if err := ensureSignatureWith(app, run); err != nil {
		t.Fatalf("verify 通过时不应报错：%v", err)
	}
	// 签名已存在就不要重签：重签会把发布方的签名标识换成 ad-hoc。
	if len(*calls) != 1 {
		t.Fatalf("已签名时只应 verify 一次，实际 %d 次：%+v", len(*calls), *calls)
	}
	if call := (*calls)[0]; call.name != "codesign" || !hasArg(call.args, "--verify") {
		t.Fatalf("第一次调用应是 codesign --verify，实际：%+v", call)
	}
}

func TestEnsureSignatureAdHocSignsWhenVerifyFails(t *testing.T) {
	app := filepath.Join(t.TempDir(), "AgentSweep.app")
	run, calls := stubCommand(func(name string, args []string) ([]byte, error) {
		if hasArg(args, "--verify") {
			return []byte("code object is not signed at all"), errCodesignInjected
		}
		return nil, nil
	})

	// verify 不过本身不算失败，它是「需要重签」的信号；重签成功就该返回 nil。
	if err := ensureSignatureWith(app, run); err != nil {
		t.Fatalf("重签成功时不应报错：%v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("应先 verify 再重签，实际 %d 次调用：%+v", len(*calls), *calls)
	}
	sign := (*calls)[1]
	if sign.name != "codesign" {
		t.Fatalf("第二次调用应是 codesign，实际：%+v", sign)
	}
	for _, want := range []string{"--force", "--deep", "--sign", "-", app} {
		if !hasArg(sign.args, want) {
			t.Fatalf("重签参数缺少 %q：%+v", want, sign.args)
		}
	}
}

func TestEnsureSignatureReportsSignFailure(t *testing.T) {
	app := filepath.Join(t.TempDir(), "AgentSweep.app")
	stderr := "AgentSweep.app: bundle format unrecognized, invalid, or unsuitable"
	run, _ := stubCommand(func(name string, args []string) ([]byte, error) {
		if hasArg(args, "--verify") {
			return []byte("code object is not signed at all"), errCodesignInjected
		}
		return []byte(stderr), errCodesignInjected
	})

	err := ensureSignatureWith(app, run)
	requireErrorContains(t, err, "codesign", "--force", "--sign", app, stderr)
	if !errors.Is(err, errCodesignInjected) {
		t.Fatalf("错误没有保留底层原因：%v", err)
	}
}

func TestRemoveQuarantineSucceeds(t *testing.T) {
	app := filepath.Join(t.TempDir(), "AgentSweep.app")
	run, calls := stubCommand(func(name string, args []string) ([]byte, error) {
		return nil, nil
	})

	if err := removeQuarantine(app, run); err != nil {
		t.Fatalf("xattr 成功时不应报错：%v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("应只调用一次 xattr，实际 %d 次：%+v", len(*calls), *calls)
	}
	if call := (*calls)[0]; call.name != "xattr" || !hasArg(call.args, "-dr") || !hasArg(call.args, "com.apple.quarantine") || !hasArg(call.args, app) {
		t.Fatalf("xattr 参数不正确：%+v", call)
	}
}

func TestRemoveQuarantineReportsXattrFailure(t *testing.T) {
	app := filepath.Join(t.TempDir(), "AgentSweep.app")
	stderr := "No such xattr: com.apple.quarantine"
	run, _ := stubCommand(func(name string, args []string) ([]byte, error) {
		return []byte(stderr), errXattrInjected
	})

	err := removeQuarantine(app, run)
	// 用户需要看到是哪个包、哪条命令、命令说了什么。
	requireErrorContains(t, err, "xattr", "com.apple.quarantine", app, stderr)
	if !errors.Is(err, errXattrInjected) {
		t.Fatalf("错误没有保留底层原因：%v", err)
	}
}

// fakeDittoRun 让 copyBundle 走 copyTree 兜底：既不执行真实 ditto，
// 也能真的把新包复制出来，供 validateBundle 校验。
func fakeDittoRun(t *testing.T, extra func(name string, args []string) ([]byte, error)) commandFunc {
	t.Helper()
	run, _ := stubCommand(func(name string, args []string) ([]byte, error) {
		if name == "ditto" {
			return nil, copyTree(args[0], args[1])
		}
		if extra != nil {
			return extra(name, args)
		}
		return nil, nil
	})
	return run
}

// 换位成功之后签名失败：必须返回错误，但绝不能回滚 —— 回滚会让用户手上
// 只剩一个半新半旧的包。目标位置必须仍是新版本。
func TestReplaceBundleKeepsNewBundleWhenPostStepsFail(t *testing.T) {
	parent := t.TempDir()
	target := writeBundle(t, parent, "0.1.0")
	newApp := writeBundle(t, filepath.Join(t.TempDir(), "staged"), "0.1.1")

	run := fakeDittoRun(t, func(name string, args []string) ([]byte, error) {
		if name == "xattr" {
			return []byte("No such xattr: com.apple.quarantine"), errXattrInjected
		}
		return nil, fmt.Errorf("测试里不应调用 %s：%v", name, args)
	})
	sign := func(string) error {
		return fmt.Errorf("codesign 重签失败：%w", errCodesignInjected)
	}

	var percents []int
	err := replaceBundleWith(newApp, target, func(percent int, note string) {
		percents = append(percents, percent)
	}, run, sign)

	requireErrorContains(t, err, "新版本已就位", target, "xattr", "codesign")
	if !errors.Is(err, errXattrInjected) {
		t.Fatalf("去隔离失败被吞掉了：%v", err)
	}
	if !errors.Is(err, errCodesignInjected) {
		t.Fatalf("重签失败被吞掉了：%v", err)
	}

	// 不回滚：目标位置必须还是新包。
	version, versionErr := bundleVersion(target)
	if versionErr != nil {
		t.Fatalf("目标位置读不出新包：%v", versionErr)
	}
	if version != "0.1.1" {
		t.Fatalf("目标位置版本 = %q，期望新版本 0.1.1（这些失败不该回滚）", version)
	}

	// 进度 98 仍然在去隔离之前发出，且收尾失败不会假装走到 100。
	if len(percents) != 2 || percents[0] != 94 || percents[1] != 98 {
		t.Fatalf("进度序列 = %v，期望 [94 98]（98 应保持去隔离前的时机）", percents)
	}
}

// 成功路径一个字节都不该变：返回 nil、新包就位、旧包清掉、进度仍是 94/98。
func TestReplaceBundleSucceedsWhenPostStepsSucceed(t *testing.T) {
	parent := t.TempDir()
	target := writeBundle(t, parent, "0.1.0")
	newApp := writeBundle(t, filepath.Join(t.TempDir(), "staged"), "0.1.1")

	run := fakeDittoRun(t, nil)
	signedApp := ""
	sign := func(appDir string) error {
		signedApp = appDir
		return nil
	}

	var percents []int
	if err := replaceBundleWith(newApp, target, func(percent int, note string) {
		percents = append(percents, percent)
	}, run, sign); err != nil {
		t.Fatalf("两个命令都成功时不应报错：%v", err)
	}

	if signedApp != target {
		t.Fatalf("签名对象 = %q，期望目标路径 %q", signedApp, target)
	}
	version, err := bundleVersion(target)
	if err != nil || version != "0.1.1" {
		t.Fatalf("目标位置版本 = %q，err = %v", version, err)
	}
	if leftovers, _ := filepath.Glob(target + ".old-*"); len(leftovers) != 0 {
		t.Fatalf("成功路径应清掉旧包，实际残留：%v", leftovers)
	}
	if len(percents) != 2 || percents[0] != 94 || percents[1] != 98 {
		t.Fatalf("进度序列 = %v，期望 [94 98]", percents)
	}
}
