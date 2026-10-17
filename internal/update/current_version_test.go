package update

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBundleWithoutVersion 造一个没有 CFBundleShortVersionString 的 .app：
// 用来模拟「bundle 版本读不到」的同时，目标路径又真实存在，这样能验证
// 拒装路径没有动过旧包。
func writeBundleWithoutVersion(t *testing.T, parent string) string {
	t.Helper()
	app := filepath.Join(parent, "AgentSweep.app")
	macOS := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macOS, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>AgentSweep</string>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macOS, "AgentSweep"), []byte("#!/bin/sh\necho agentsweep\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

// TestInstallUsesBundleVersionWhenUserAgentUnusable 回归 U6：当前版本不能只靠
// UserAgent。UA 为空 / dev / 甚至自报一个更高的版本时，都应以目标 .app 里
// 真实记录的 CFBundleShortVersionString 为准。
func TestInstallUsesBundleVersionWhenUserAgentUnusable(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
	}{
		{"UA 为空", ""},
		{"UA 是 dev", "AgentSweep/dev"},
		// 若实现采信 UA，就会认为当前是 9.9.9、拒绝安装 1.3.0；bundle 才是真相。
		{"UA 自报更高版本", "AgentSweep/9.9.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := writeBundle(t, t.TempDir(), "1.2.0")
			fixture := serveRelease(t, "v1.3.0", "AgentSweep-v1.3.0-darwin-amd64.zip", makeAppZip(t, "1.3.0"), "")
			client := testClient(fixture.server)
			client.UserAgent = tc.userAgent

			release, err := client.Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Install(context.Background(), release, target, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("bundle 里有 1.2.0，安装 1.3.0 应当放行: %v", err)
			}
			if result.Version != "1.3.0" || result.PreviousVersion != "1.2.0" {
				t.Fatalf("结果 = %+v，want Version=1.3.0 PreviousVersion=1.2.0", result)
			}
			if version, err := bundleVersion(target); err != nil || version != "1.3.0" {
				t.Fatalf("替换后版本 = %q, err = %v", version, err)
			}
		})
	}
}

// TestInstallRejectsDowngradeWhenUserAgentUnusable 验证降级保护真的生效：
// UA 拿不到版本时，旧代码用 "dev" 兜底会让 IsNewer 放行，1.1.0 能盖掉 1.2.0。
func TestInstallRejectsDowngradeWhenUserAgentUnusable(t *testing.T) {
	for _, userAgent := range []string{"", "AgentSweep/dev"} {
		t.Run("UA="+userAgent, func(t *testing.T) {
			target := writeBundle(t, t.TempDir(), "1.2.0")
			fixture := serveRelease(t, "v1.1.0", "AgentSweep-v1.1.0-darwin-amd64.zip", makeAppZip(t, "1.1.0"), "")
			client := testClient(fixture.server)
			client.UserAgent = userAgent

			release, err := client.Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Install(context.Background(), release, target, t.TempDir(), nil)
			if err == nil {
				t.Fatal("比当前版本旧的包必须被拒绝")
			}
			if !strings.Contains(err.Error(), "不比当前版本新") {
				t.Fatalf("错误信息应说明新版本不够新：%v", err)
			}
			if version, _ := bundleVersion(target); version != "1.2.0" {
				t.Fatalf("拒装后旧包被改动了：版本 %q", version)
			}
		})
	}
}

// TestInstallRefusesWithoutCurrentVersion 验证两个来源都拿不到时拒绝安装，
// 而不是用 "dev" 继续走；并且拒绝发生在下载、建目录、替换之前。
func TestInstallRefusesWithoutCurrentVersion(t *testing.T) {
	newRelease := func(t *testing.T, client *Client) *Release {
		t.Helper()
		release, err := client.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return release
	}

	t.Run("target 不存在", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "AgentSweep.app")
		fixture := serveRelease(t, "v1.3.0", "AgentSweep-v1.3.0-darwin-amd64.zip", makeAppZip(t, "1.3.0"), "")
		client := testClient(fixture.server)
		client.UserAgent = "AgentSweep/dev"
		workDir := filepath.Join(t.TempDir(), "work")

		_, err := client.Install(context.Background(), newRelease(t, client), target, workDir, nil)
		if err == nil {
			t.Fatal("bundle 与 UA 都拿不到版本时必须拒绝安装")
		}
		if !strings.Contains(err.Error(), "无法确定当前版本") {
			t.Fatalf("错误信息应说明当前版本未知：%v", err)
		}
		if _, statErr := os.Stat(target); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("拒装不该创建目标：stat err = %v", statErr)
		}
		if _, statErr := os.Stat(workDir); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("拒装应发生在下载/建目录之前，workDir 却存在：%v", statErr)
		}
	})

	t.Run("Info.plist 没有版本号", func(t *testing.T) {
		target := writeBundleWithoutVersion(t, t.TempDir())
		plistPath := filepath.Join(target, "Contents", "Info.plist")
		before, err := os.ReadFile(plistPath)
		if err != nil {
			t.Fatal(err)
		}

		fixture := serveRelease(t, "v1.3.0", "AgentSweep-v1.3.0-darwin-amd64.zip", makeAppZip(t, "1.3.0"), "")
		client := testClient(fixture.server)
		client.UserAgent = ""

		_, err = client.Install(context.Background(), newRelease(t, client), target, t.TempDir(), nil)
		if err == nil {
			t.Fatal("bundle 没有版本号且 UA 为空时必须拒绝安装")
		}
		if !strings.Contains(err.Error(), "无法确定当前版本") {
			t.Fatalf("错误信息应说明当前版本未知：%v", err)
		}
		after, readErr := os.ReadFile(plistPath)
		if readErr != nil {
			t.Fatalf("目标 Info.plist 读不到了，说明包被动过：%v", readErr)
		}
		if string(after) != string(before) {
			t.Fatal("拒装后目标 Info.plist 被改动了")
		}
	})
}
