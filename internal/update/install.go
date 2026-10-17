package update

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ErrNotAppBundle 表示当前进程不是从 .app 里跑起来的（go run / bin/AgentSweep）。
// 这种时候替换「自己」没有意义，UI 会把用户引到发布页。
var ErrNotAppBundle = errors.New("当前不是从 .app 运行，无法自动安装")

// maxExtractedBytes 是解压后的大小上限。正常 .app 约 20–40 MB，
// 给到 1 GB 是为了不误伤未来的体积增长，同时挡住压缩炸弹。
const maxExtractedBytes = 1 << 30

// maxEntries 挡住「几万个空文件」这种解压放大。正常 .app 最多几千个条目。
const maxEntries = 20000

// DetectBundle 从可执行文件位置反推 .app 路径。
//
// 路径形状是固定的：X.app/Contents/MacOS/X。任何一段对不上都返回 ErrNotAppBundle，
// 不做「猜一个最近的 .app」这种聪明事 —— 猜错就是往别人的目录里覆盖东西。
func DetectBundle() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrNotAppBundle
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("读不到自身路径：%w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	if strings.Contains(executable, "/AppTranslocation/") {
		return "", errors.New("AgentSweep 正从系统临时目录运行，请先把它拖进「应用程序」再更新")
	}
	macOSDir := filepath.Dir(executable)
	contentsDir := filepath.Dir(macOSDir)
	bundle := filepath.Dir(contentsDir)
	if filepath.Base(macOSDir) != "MacOS" ||
		filepath.Base(contentsDir) != "Contents" ||
		!strings.HasSuffix(strings.ToLower(bundle), ".app") {
		return "", ErrNotAppBundle
	}
	info, err := os.Stat(bundle)
	if err != nil || !info.IsDir() {
		return "", ErrNotAppBundle
	}
	return bundle, nil
}

// InstallResult 描述一次成功的替换。
type InstallResult struct {
	Path            string
	Version         string
	PreviousVersion string
}

// ProgressFunc 回报安装进度：percent 0–100，note 是给人看的一句话。
type ProgressFunc func(percent int, note string)

// DefaultWorkDir 是下载与解压的落脚点。放缓存目录：更新失败也不该污染用户文档。
func DefaultWorkDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "AgentSweep", "updates")
}

// Install 下载并替换 target（空字符串则自动定位当前 .app）。
//
// 顺序刻意如此：先在校验和通过之后才解压，解压并验证 Info.plist 版本之后才动旧包，
// 任何一步失败都保持旧包原样。安装过程中不需要管理员权限，但目标目录必须可写。
func (c *Client) Install(ctx context.Context, release *Release, target, workDir string, progress ProgressFunc) (InstallResult, error) {
	if release == nil {
		return InstallResult{}, errors.New("没有可安装的发布")
	}
	if progress == nil {
		progress = func(int, string) {}
	}
	if target == "" {
		detected, err := DetectBundle()
		if err != nil {
			return InstallResult{}, err
		}
		target = detected
	}
	if runtime.GOOS != "darwin" {
		return InstallResult{}, errors.New("自动安装目前只支持 macOS")
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if !strings.HasSuffix(strings.ToLower(target), ".app") {
		return InstallResult{}, fmt.Errorf("%s 不是 .app", target)
	}

	asset, err := c.AssetFor(release)
	if err != nil {
		return InstallResult{}, err
	}
	if workDir == "" {
		workDir = DefaultWorkDir()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("创建更新目录失败：%w", err)
	}
	staging, err := os.MkdirTemp(workDir, "install-")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.RemoveAll(staging)

	progress(0, "下载 "+asset.Name)
	archiveName := filepath.Base(asset.Name)
	if archiveName == "" || archiveName == "." || archiveName == string(filepath.Separator) {
		return InstallResult{}, fmt.Errorf("附件名不合法：%q", asset.Name)
	}
	archivePath := filepath.Join(staging, archiveName)
	err = c.Download(ctx, asset, archivePath, func(done, total int64) {
		if total <= 0 {
			progress(1, "正在下载…")
			return
		}
		percent := int(done * 70 / total)
		if percent > 70 {
			percent = 70
		}
		progress(percent, "正在下载…")
	})
	if err != nil {
		return InstallResult{}, err
	}

	progress(72, "核对 SHA-256")
	sums, err := c.Checksums(ctx, release)
	if err != nil {
		return InstallResult{}, err
	}
	expected, ok := sums[asset.Name]
	if !ok {
		return InstallResult{}, fmt.Errorf("checksums.txt 里没有 %s", asset.Name)
	}
	actual, err := SHA256File(archivePath)
	if err != nil {
		return InstallResult{}, err
	}
	if !strings.EqualFold(actual, expected) {
		return InstallResult{}, fmt.Errorf("SHA-256 不匹配（期望 %s，实际 %s），已放弃安装", expected, actual)
	}

	progress(78, "解压")
	extracted, err := extractApp(archivePath, filepath.Join(staging, "app"))
	if err != nil {
		return InstallResult{}, err
	}
	progress(88, "校验新版本")
	extractedVersion, err := bundleVersion(extracted)
	if err != nil {
		return InstallResult{}, err
	}
	if normalizeVersion(extractedVersion) != normalizeVersion(release.Version) {
		return InstallResult{}, fmt.Errorf("包内版本 %s 与发布 %s 不一致", extractedVersion, release.Version)
	}
	if !IsNewer(c.currentVersion(), extractedVersion) {
		return InstallResult{}, fmt.Errorf("下载到的版本 %s 不比当前版本新", extractedVersion)
	}
	// 架构不对的包装上去只会「更新完打不开」，宁可现在拒绝。lipo 不存在时跳过。
	if err := validateArchitecture(extracted); err != nil {
		return InstallResult{}, err
	}

	progress(92, "替换应用")
	previous := ""
	if currentBefore, err := bundleVersion(target); err == nil {
		previous = currentBefore
	}
	if err := replaceBundle(extracted, target, progress); err != nil {
		return InstallResult{}, err
	}
	progress(100, "安装完成")
	return InstallResult{Path: target, Version: extractedVersion, PreviousVersion: previous}, nil
}

// currentVersion 由 NewClient 的 UserAgent 反推，避免 Install 再多一个参数。
// 用 UserAgent 而不是新增字段，是因为调用方已经在构造它时知道版本了。
func (c *Client) currentVersion() string {
	const prefix = "AgentSweep/"
	if strings.HasPrefix(c.UserAgent, prefix) {
		return strings.TrimPrefix(c.UserAgent, prefix)
	}
	return "dev"
}

// extractApp 解压 zip 并返回其中的 .app 目录。
//
// zip 只接受「唯一根目录、且根是 .app」的形状 —— 这正是 ditto --keepParent 的产物。
// 这条约束同时挡住了两条路：往压缩包里塞第二个顶层目录，以及 ../ 之类越界路径。
func extractApp(archivePath, destDir string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("打不开压缩包：%w", err)
	}
	defer reader.Close()

	root := ""
	var declared int64
	var extracted int64
	if len(reader.File) > maxEntries {
		return "", fmt.Errorf("压缩包条目超过 %d 个，拒绝处理", maxEntries)
	}
	for _, entry := range reader.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		cleaned := path.Clean(name)
		if cleaned == "." || cleaned == "" {
			continue
		}
		// ditto 会把扩展属性放进 __MACOSX/，它们不是应用的一部分。
		if cleaned == "__MACOSX" || strings.HasPrefix(cleaned, "__MACOSX/") || strings.Contains(cleaned, "/__MACOSX/") {
			continue
		}
		if strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
			return "", fmt.Errorf("压缩包里有越界路径：%s", entry.Name)
		}
		parts := strings.Split(cleaned, "/")
		if root == "" {
			root = parts[0]
			if !strings.HasSuffix(strings.ToLower(root), ".app") {
				return "", errors.New("压缩包里没有 .app")
			}
		} else if parts[0] != root {
			return "", fmt.Errorf("压缩包有多个顶层目录：%s 与 %s", root, parts[0])
		}

		declared += int64(entry.UncompressedSize64)
		if declared > maxExtractedBytes {
			return "", errors.New("压缩包解压后超过 1 GB，拒绝处理")
		}
		destination := filepath.Join(destDir, filepath.FromSlash(cleaned))
		if !strings.HasPrefix(destination, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return "", fmt.Errorf("压缩包里有越界路径：%s", entry.Name)
		}

		mode := entry.Mode()
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return "", err
		}
		if mode&os.ModeSymlink != 0 {
			source, err := readZipEntry(entry)
			if err != nil {
				return "", err
			}
			target := string(source)
			if strings.HasPrefix(target, "/") || strings.Contains(target, "..") {
				return "", fmt.Errorf("压缩包里有可疑符号链接：%s → %s", entry.Name, target)
			}
			extracted += int64(len(target))
			if extracted > maxExtractedBytes {
				return "", errors.New("压缩包解压后超过 1 GB，拒绝处理")
			}
			_ = os.Remove(destination)
			if err := os.Symlink(target, destination); err != nil {
				return "", err
			}
			continue
		}
		permissions := os.FileMode(0o644)
		if mode&0o111 != 0 {
			permissions = 0o755
		}
		source, err := entry.Open()
		if err != nil {
			return "", err
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions)
		if err != nil {
			source.Close()
			return "", err
		}
		// 按**实际写出的字节**限流：header 里的 UncompressedSize64 是压缩包自己声明的，
		// 一个撒谎的包可以在 Go 的 EOF 检查之前就把磁盘填满。
		remaining := maxExtractedBytes - extracted
		if remaining <= 0 {
			source.Close()
			file.Close()
			return "", errors.New("压缩包解压后超过 1 GB，拒绝处理")
		}
		written, copyErr := io.Copy(file, io.LimitReader(source, remaining+1))
		extracted += written
		source.Close()
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if extracted > maxExtractedBytes {
			return "", errors.New("压缩包解压后超过 1 GB，拒绝处理")
		}
	}
	if root == "" {
		return "", errors.New("压缩包是空的")
	}
	appDir := filepath.Join(destDir, root)
	if err := validateBundle(appDir); err != nil {
		return "", err
	}
	return appDir, nil
}

func readZipEntry(entry *zip.File) ([]byte, error) {
	source, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer source.Close()
	return io.ReadAll(io.LimitReader(source, 64<<10))
}

// validateBundle 只检查「能被 LaunchServices 认出来」的最低条件。
func validateBundle(appDir string) error {
	info, err := os.Stat(appDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s 不是目录", appDir)
	}
	if _, err := os.Stat(filepath.Join(appDir, "Contents", "Info.plist")); err != nil {
		return errors.New("包里缺少 Contents/Info.plist")
	}
	executableName := bundleExecutableName(appDir)
	if executableName == "" {
		return errors.New("Info.plist 里没有 CFBundleExecutable")
	}
	// CFBundleExecutable 是包内自述数据，不能让它跳出 Contents/MacOS：
	// 否则一个伪造的 plist 就能让校验去 stat/chmod 系统上任意已有文件。
	if executableName != filepath.Base(executableName) || strings.ContainsAny(executableName, `/\`) || strings.Contains(executableName, "..") {
		return fmt.Errorf("Info.plist 里的 CFBundleExecutable 不合法：%q", executableName)
	}
	macOSDir := filepath.Join(appDir, "Contents", "MacOS")
	executable := filepath.Join(macOSDir, executableName)
	if filepath.Dir(executable) != macOSDir {
		return fmt.Errorf("可执行文件不在 Contents/MacOS 内：%q", executableName)
	}
	fileInfo, err := os.Stat(executable)
	if err != nil || fileInfo.IsDir() {
		return errors.New("包里缺少可执行文件")
	}
	if fileInfo.Mode()&0o111 == 0 {
		if err := os.Chmod(executable, 0o755); err != nil {
			return fmt.Errorf("可执行文件没有执行权限，且改写失败：%w", err)
		}
	}
	if _, err := bundleVersion(appDir); err != nil {
		return err
	}
	return nil
}

func bundleExecutableName(appDir string) string {
	plistPath := filepath.Join(appDir, "Contents", "Info.plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return ""
	}
	match := executablePattern.FindSubmatch(data)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(string(match[1]))
}

var (
	executablePattern   = regexp.MustCompile(`(?s)<key>CFBundleExecutable</key>\s*<string>([^<]*)</string>`)
	shortVersionPattern = regexp.MustCompile(`(?s)<key>CFBundleShortVersionString</key>\s*<string>([^<]*)</string>`)
)

// validateArchitecture 用 lipo 确认包里真的有本机架构的 slice。
//
// 这是额外防线：发布脚本已经把两个架构拼好了，但「标签写对、包装错」正是那种
// 更新完打不开、又很难在事后解释的故障。lipo 不可用（脚本文件、非 Mach-O）时跳过。
func validateArchitecture(appDir string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	lipo, err := exec.LookPath("lipo")
	if err != nil {
		return nil
	}
	executableName := bundleExecutableName(appDir)
	if executableName == "" {
		return nil
	}
	output, err := exec.Command(lipo, "-archs", filepath.Join(appDir, "Contents", "MacOS", executableName)).Output()
	if err != nil {
		return nil
	}
	wanted := runtime.GOARCH
	if wanted == "amd64" {
		wanted = "x86_64"
	}
	archs := strings.Fields(string(output))
	for _, arch := range archs {
		if arch == wanted {
			return nil
		}
	}
	return fmt.Errorf("安装包里没有 %s 架构（只有 %s）", wanted, strings.Join(archs, ", "))
}

// bundleVersion 先按 XML plist 正则读；读不到再退回 plutil（二进制 plist 的兜底）。
func bundleVersion(appDir string) (string, error) {
	plistPath := filepath.Join(appDir, "Contents", "Info.plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return "", fmt.Errorf("读不到 Info.plist：%w", err)
	}
	if match := shortVersionPattern.FindSubmatch(data); match != nil {
		version := strings.TrimSpace(string(match[1]))
		if version != "" {
			return version, nil
		}
	}
	if runtime.GOOS == "darwin" {
		output, err := exec.Command("plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", plistPath).Output()
		if err == nil {
			if version := strings.TrimSpace(string(output)); version != "" {
				return version, nil
			}
		}
	}
	return "", errors.New("Info.plist 里没有 CFBundleShortVersionString")
}

// replaceBundle 用「先复制到旁边、校验通过、再两次 rename 换位」的顺序替换。
//
// 直接把新包复制到目标路径，中途崩溃/休眠会留下一个半新半旧、点开就崩的包；
// 先落到 target.new-* 再换位，崩溃窗口只剩两个 rename（文件系统层面近似原子），
// 最坏情况是目标缺失、旁边留着 target.old-*/target.new-*，可以手工恢复。
func replaceBundle(newApp, target string, progress ProgressFunc) error {
	parent := filepath.Dir(target)
	if err := ensureWritable(parent); err != nil {
		return fmt.Errorf("没有权限写入 %s：%w", parent, err)
	}
	stamp := time.Now().UnixNano()
	stagedPath := fmt.Sprintf("%s.new-%d", target, stamp)
	oldPath := fmt.Sprintf("%s.old-%d", target, stamp)
	defer os.RemoveAll(stagedPath)

	progress(94, "复制新版本")
	if err := copyBundle(newApp, stagedPath); err != nil {
		return fmt.Errorf("复制新版本失败：%w", err)
	}
	if err := validateBundle(stagedPath); err != nil {
		return fmt.Errorf("新版本校验失败：%w", err)
	}

	if err := os.Rename(target, oldPath); err != nil {
		return fmt.Errorf("移开旧版本失败：%w", err)
	}
	if err := os.Rename(stagedPath, target); err != nil {
		if rollbackErr := os.Rename(oldPath, target); rollbackErr != nil {
			return fmt.Errorf("换上新的版本失败：%v；旧版本回滚失败，原包保留在 %s：%v", err, oldPath, rollbackErr)
		}
		return fmt.Errorf("换上新的版本失败，已回滚到旧版本：%w", err)
	}
	_ = os.RemoveAll(oldPath)

	progress(98, "清理隔离属性")
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", target).Run()
	ensureSignature(target)
	return nil
}

// copyBundle 优先用 ditto：它保留资源分支、扩展属性与权限，签名才不会因为复制而失效。
func copyBundle(source, destination string) error {
	if _, err := exec.LookPath("ditto"); err == nil {
		if output, err := exec.Command("ditto", source, destination).CombinedOutput(); err == nil {
			return nil
		} else if strings.TrimSpace(string(output)) != "" {
			return fmt.Errorf("%s", strings.TrimSpace(string(output)))
		}
	}
	return copyTree(source, destination)
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return os.MkdirAll(destination, 0o755)
		}
		target := filepath.Join(destination, relative)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(current, target, info.Mode().Perm())
		}
	})
}

func copyFile(source, destination string, permissions os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func ensureWritable(directory string) error {
	probe, err := os.CreateTemp(directory, ".agentsweep-write-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

// ensureSignature 检查签名，坏了就重新做一次 ad-hoc 签名。
//
// 我们发布的就是 ad-hoc 签名，重新签不会降低安全性；但少了这一步，
// 被 ditto 之外的方式复制过的包会直接拒绝启动。
func ensureSignature(appDir string) {
	if _, err := exec.LookPath("codesign"); err != nil {
		return
	}
	if exec.Command("codesign", "--verify", "--deep", "--strict", appDir).Run() == nil {
		return
	}
	_ = exec.Command("codesign", "--force", "--deep", "--sign", "-", appDir).Run()
}

// RelaunchAfterExit 写一个分离的 shell 脚本：等当前进程退出，再打开新包。
//
// 为什么不在进程里直接 `open`：旧进程还占着窗口，新进程起来会两个图标并排；
// 交给脚本等 PID 消失再启动，用户看到的是一次干净的重启。
func RelaunchAfterExit(appPath string, pid int, workDir string) error {
	if workDir == "" {
		workDir = DefaultWorkDir()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	scriptPath := filepath.Join(workDir, fmt.Sprintf("relaunch-%d.sh", pid))
	if err := os.WriteFile(scriptPath, []byte(relaunchScript(appPath, pid)), 0o700); err != nil {
		return err
	}
	command := exec.Command("/bin/sh", scriptPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdout = nil
	command.Stderr = nil
	return command.Start()
}

// relaunchScript 单独拆出来是为了能测：路径必须正确转义，
// 否则应用装在带空格或引号的目录下就重启不了。
func relaunchScript(appPath string, pid int) string {
	return fmt.Sprintf(`#!/bin/sh
# AgentSweep 更新完成后重启：等旧进程退出，再打开新包。
# 最多等 120 秒（600 × 0.2s）：PID 被复用或变成僵尸时不能让脚本永远转下去。
i=0
while kill -0 %d 2>/dev/null; do
  i=$((i + 1))
  if [ "$i" -ge 600 ]; then break; fi
  sleep 0.2
done
xattr -dr com.apple.quarantine %s 2>/dev/null
open -n %s
`, pid, shellQuote(appPath), shellQuote(appPath))
}

// shellQuote 用单引号包住路径；路径里的单引号转成 '\”。
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}
