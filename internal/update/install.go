package update

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// relaunchCreateAttempts 是重启脚本文件名冲突时的总尝试次数。
//
// 第一次用确定的名字 relaunch-<pid>.sh，冲突（EEXIST，包括 symlink）之后换随机后缀。
// 工作目录是用户缓存目录、路径可预测，本地攻击者能预先放一个同名 symlink 指向别处；
// O_EXCL 保证我们绝不截断或跟随它，而每次创建都带 O_EXCL，所以即使随机后缀被猜中，
// 也只会让我们再换一个名字，不会写到对手指定的路径。
const relaunchCreateAttempts = 5

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

	// 当前版本要在下载之前确定：拿不到就直接拒装，既省掉白下载几十 MB，
	// 也保证任何失败路径都没有碰过旧包。降级保护依赖它，理由见 currentVersion。
	current, err := c.currentVersion(target)
	if err != nil {
		return InstallResult{}, err
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
	// 这一步只证明了「下载到的字节 == 同一个发布声明的字节」，没有证明「是谁发布的」：
	// checksums.txt 和 zip 来自同一个 Release，发布渠道被攻破时可以一起换掉。
	//
	// 将来若引入真正的发布者签名（Minisign/EdDSA，或 Developer ID），校验插在**这里**：
	// 下载完成之后、extractApp 之前。此刻 archivePath 还是发布方签过的那份原始字节；
	// 挪到解压之后就只能对解压结果再算一次哈希，证明的仍然只是「包内自洽」。
	// 威胁模型见 docs/SECURITY.md。

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
	if !IsNewer(current, extractedVersion) {
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

// currentVersion 确定「当前正在运行的版本」，取不到时返回错误而不是兜底值。
//
// 两个来源，按可信度排：
//  1. 目标 .app 的 CFBundleShortVersionString —— 磁盘上真实在跑的那份包；
//  2. UserAgent 里的 AgentSweep/<版本> —— 调用方自报，格式可能偏、也可能是
//     被打包脚本写死的历史值。
//
// 特别注意**不能**再用 "dev" 之类解析不了的值兜底：Install 的降级保护是
// `!IsNewer(current, extracted)`，而 IsNewer 对解析不了的 current 是放行策略
// （开发构建不阻塞更新）。若当前版本退化成 "dev"，任意旧发布包、乃至被重放的
// 旧包都能通过校验装回去 —— 等于把「必须比当前新」这条防线关掉。所以两个
// 来源都不可用时，这里返回错误，让调用方拒绝安装。
//
// 「可用」的标准是能按项目宽松规则解析成版本号（ParseVersion）：plist 里写了
// 一句非版本字符串，与读不到同样无法参与比较，不能当作有效来源往下走。
func (c *Client) currentVersion(target string) (string, error) {
	if version, err := bundleVersion(target); err == nil {
		if version = strings.TrimSpace(version); usableVersion(version) {
			return version, nil
		}
	}
	const prefix = "AgentSweep/"
	if strings.HasPrefix(c.UserAgent, prefix) {
		if version := strings.TrimSpace(strings.TrimPrefix(c.UserAgent, prefix)); usableVersion(version) {
			return version, nil
		}
	}
	return "", fmt.Errorf(
		"无法确定当前版本（%s 里读不到可用的 CFBundleShortVersionString，UserAgent %q 也不是 AgentSweep/<版本号>），拒绝自动更新以免降级",
		target, c.UserAgent)
}

// usableVersion 判断一个自报版本能否参与新旧比较。
func usableVersion(raw string) bool {
	_, err := ParseVersion(raw)
	return err == nil
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
	lipo, err := resolveTool("lipo")
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
		// plutil 只用来读二进制 plist；找不到或执行失败都维持「读不到版本」的
		// 既有语义，由调用方决定是跳过还是报错，不在这里 panic。
		if plutil, err := resolveTool("plutil"); err == nil {
			output, err := exec.Command(plutil, "-extract", "CFBundleShortVersionString", "raw", "-o", "-", plistPath).Output()
			if err == nil {
				if version := strings.TrimSpace(string(output)); version != "" {
					return version, nil
				}
			}
		}
	}
	return "", errors.New("Info.plist 里没有 CFBundleShortVersionString")
}

// commandFunc 执行一条外部命令并返回合并后的 stdout+stderr。
//
// 抽成参数而不是直接 exec.Command：codesign/xattr 会改真实签名、ditto 会真的落盘，
// 测试里必须能全部替掉。
// 用参数注入而不是包级变量，是因为包级可变状态会让并行测试互相踩，
// 而安装路径恰好最不能容忍「测到的不是跑到的」。
type commandFunc func(name string, args ...string) ([]byte, error)

// systemToolDirs 是解析安装工具时的固定查找顺序。
//
// 这些目录在 macOS 上由系统拥有，普通用户改不动；PATH 则可能被 shell 配置、
// CI 环境或父进程注入。安装流程会替换 /Applications 里的 .app，是应用里
// 权限最高的动作，不能因为 PATH 里排前面的同名程序就换一个执行。
var systemToolDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// resolveTool 把工具名解析成绝对路径。
//
// 优先在系统固定目录里找，最后才回退 PATH：固定目录覆盖 macOS 的全部安装
// 工具，PATH 回退只是给非 macOS 或自定义环境留的出口（例如开发机把工具装
// 在 /opt/homebrew/bin）。回退仍然保留，是因为拒绝在 PATH 外找会把这些
// 环境上的自动安装直接打死，收益不抵成本。
func resolveTool(name string) (string, error) {
	if strings.Contains(name, "/") {
		// 调用方已经给了路径：不拼接、不猜，只校验它确实可执行。
		if err := executableFile(name); err != nil {
			return "", err
		}
		return name, nil
	}
	for _, dir := range systemToolDirs {
		candidate := filepath.Join(dir, name)
		if err := executableFile(candidate); err == nil {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("找不到可执行文件 %q：系统目录（%s）与 PATH 都没有", name, strings.Join(systemToolDirs, "、"))
}

// executableFile 检查 path 是否存在、是普通文件（非目录）且带执行位。
func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s 是目录", path)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s 没有执行权限", path)
	}
	return nil
}

// runCommand 是生产路径上的实现。
//
// 先 resolveTool 再 exec：注入测试用的是逻辑名（"ditto"/"codesign"），
// 而生产路径不允许裸名进 exec.Command —— 裸名会按调用方 PATH 解析，
// 正是本函数要避开的那个洞。
func runCommand(name string, args ...string) ([]byte, error) {
	resolved, err := resolveTool(name)
	if err != nil {
		return nil, err
	}
	return exec.Command(resolved, args...).CombinedOutput()
}

// commandFailure 拼出「哪条命令失败、为什么、命令自己说了什么」。
//
// 只有 exit status 1 的错误对用户毫无信息量，所以带上 stderr 摘要；但摘要要限长：
// codesign --deep 失败时会把整棵包的内容打出来，全量塞进 UI 没有人看得完。
func commandFailure(name string, args []string, output []byte, err error) error {
	command := name + " " + strings.Join(args, " ")
	summary := strings.TrimSpace(string(output))
	if summary == "" {
		return fmt.Errorf("%s 失败：%w", command, err)
	}
	const maxSummaryRunes = 400
	if runes := []rune(summary); len(runes) > maxSummaryRunes {
		summary = string(runes[:maxSummaryRunes]) + "…"
	}
	return fmt.Errorf("%s 失败：%w；输出：%s", command, err, summary)
}

// replaceBundle 用「先复制到旁边、校验通过、再两次 rename 换位」的顺序替换。
//
// 直接把新包复制到目标路径，中途崩溃/休眠会留下一个半新半旧、点开就崩的包；
// 先落到 target.new-* 再换位，崩溃窗口只剩两个 rename（文件系统层面近似原子）。
// 换位由 swapBundle 负责：它把「第二次 rename 失败就把旧包放回原位」当作硬约束，
// 所以本函数返回错误时，目标位置不会是一个空洞。进程恰好在这两次 rename 之间
// 被杀死时函数没有机会回滚，仍会留下 target.old-*，需要手工移回 —— 那是崩溃，
// 不是可处理的错误返回路径。
func replaceBundle(newApp, target string, progress ProgressFunc) error {
	return replaceBundleWith(newApp, target, progress, runCommand, ensureSignature)
}

// replaceBundleWith 是 replaceBundle 的实现。run 与 sign 两个注入口让测试能替掉
// ditto/xattr/codesign —— 它们会真的改一份签名，测试里一条都不能执行。
// sign 单独一个口，是因为它还要先判断 codesign 在不在、签名是否已经存在。
func replaceBundleWith(newApp, target string, progress ProgressFunc, run commandFunc, sign func(string) error) error {
	parent := filepath.Dir(target)
	if err := ensureWritable(parent); err != nil {
		return fmt.Errorf("没有权限写入 %s：%w", parent, err)
	}
	stamp := time.Now().UnixNano()
	stagedPath := fmt.Sprintf("%s.new-%d", target, stamp)
	oldPath := fmt.Sprintf("%s.old-%d", target, stamp)
	// stagedPath 只是刚复制出来的副本，失败时清掉它不会丢用户数据；即使删不掉，
	// 也不影响「旧包还在」这个不变式，因此这里的错误可以忽略。
	defer os.RemoveAll(stagedPath)

	progress(94, "复制新版本")
	if err := copyBundle(newApp, stagedPath, run); err != nil {
		return fmt.Errorf("复制新版本失败：%w", err)
	}
	if err := validateBundle(stagedPath); err != nil {
		return fmt.Errorf("新版本校验失败：%w", err)
	}

	if err := swapBundle(target, stagedPath, oldPath, os.Rename); err != nil {
		return err
	}
	// 新包已经就位。从这里往下的失败一律不回滚：回滚会再造一次空窗，
	// 用户手上只剩一个半新半旧的包。但每一步失败都不能吞 —— 旧包占着磁盘、
	// Gatekeeper 拦截、签名坏了起不来，这三种后果都得让用户看见。
	var failures []error
	if err := removeOldBundle(oldPath); err != nil {
		failures = append(failures, err)
	}

	progress(98, "清理隔离属性")
	if err := removeQuarantine(target, run); err != nil {
		failures = append(failures, err)
	}
	if err := sign(target); err != nil {
		failures = append(failures, err)
	}
	return postReplaceFailure(target, failures)
}

// postReplaceFailure 把「新包已就位之后」的失败汇总成一个错误。
//
// errors.Join 保留每一条原因，而不是只报最后一条：去隔离与重签同时失败时，
// UI/日志里两条都得在，用户才知道该先修哪一条。文案必须说清新版本已经就位，
// 否则用户只会看到一句失败、然后去旧位置找一个已经被换掉的包。
func postReplaceFailure(target string, failures []error) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("新版本已就位（%s），但收尾失败：%w", target, errors.Join(failures...))
}

// renameFunc 让测试可以注入「第二次换位失败」这类真实环境难以构造的错误。
type renameFunc func(oldpath, newpath string) error

// swapBundle 完成两次 rename 换位，并把「目标位置不能为空」当作硬约束。
//
// 第一次 rename 失败时目标原样未动；第二次失败时无条件把 oldPath 还原回 target。
// 还原也失败时不再假装成功，而是把旧包的实际路径写进错误，让用户能手动救回来。
func swapBundle(target, stagedPath, oldPath string, rename renameFunc) error {
	if err := rename(target, oldPath); err != nil {
		return fmt.Errorf("移开旧版本失败，%s 仍是旧版本：%w", target, err)
	}
	if err := rename(stagedPath, target); err != nil {
		restoreErr := rename(oldPath, target)
		// rename 返回 nil 仍要确认目标真的回来了：这条不变式不能只靠返回值相信，
		// 否则注入实现（或未来的包装）说谎时目标会悄悄留空。
		if restoreErr == nil {
			if info, statErr := os.Stat(target); statErr != nil {
				restoreErr = statErr
			} else if !info.IsDir() {
				restoreErr = fmt.Errorf("%s 不是目录", target)
			}
		}
		if restoreErr != nil {
			return fmt.Errorf("换上新的版本失败：%w；把旧版本还原到 %s 也失败：%w。旧版本完整保留在 %s，请手动把它移回 %s",
				err, target, restoreErr, oldPath, target)
		}
		return fmt.Errorf("换上新的版本失败，已把旧版本从 %s 还原到 %s：%w", oldPath, target, err)
	}
	return nil
}

// removeOldBundle 删除换位成功后留下的旧包。
//
// 不能 `_ = os.RemoveAll(oldPath)`：删除失败意味着磁盘上多留一份完整的旧应用，
// 调用方需要知道「新版本已就位，但旧包清不掉、它在哪」。本项目没有 logger，
// 所以把错误并入返回值，而不是静默吞掉。
func removeOldBundle(oldPath string) error {
	if err := os.RemoveAll(oldPath); err != nil {
		return fmt.Errorf("新版本已就位，但删除旧版本失败，旧包仍保留在 %s：%w", oldPath, err)
	}
	return nil
}

// copyBundle 优先用 ditto：它保留资源分支、扩展属性与权限，签名才不会因为复制而失效。
// run 让测试能替掉 ditto —— 测试里不执行任何真实外部命令。
func copyBundle(source, destination string, run commandFunc) error {
	if _, err := resolveTool("ditto"); err != nil {
		return copyTree(source, destination)
	}
	output, err := run("ditto", source, destination)
	if err == nil {
		return nil
	}
	// ditto 一个字都没说却失败时（极少见）仍然退回纯 Go 复制：
	// 这与原来「有输出就报错、没输出就兜底」的行为一致。
	if message := strings.TrimSpace(string(output)); message != "" {
		return fmt.Errorf("ditto %s → %s 失败：%w；输出：%s", source, destination, err, message)
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

// removeQuarantine 去掉 Gatekeeper 的隔离属性。
//
// 失败不回滚（新包已经在目标位置），但必须上报：留着 com.apple.quarantine 的包
// 下次打开会被 Gatekeeper 拦下，而用户刚看到「更新成功」，只会以为应用坏了。
func removeQuarantine(appDir string, run commandFunc) error {
	args := []string{"-dr", "com.apple.quarantine", appDir}
	if output, err := run("xattr", args...); err != nil {
		return commandFailure("xattr", args, output, err)
	}
	return nil
}

// ensureSignature 检查签名，坏了就重新做一次 ad-hoc 签名。
//
// 我们发布的就是 ad-hoc 签名，重新签不会降低安全性；但少了这一步，
// 被 ditto 之外的方式复制过的包会直接拒绝启动。
// codesign 不可用时仍然跳过：没有工具就没有可做的事，这不是安装失败。
//
// 这里必须说清楚这一段的**边界**，免得后来者把「重签成功」读成「包可信」：
//
//   - ad-hoc 重签（--sign -）的唯一作用是让替换后的包还能运行。它给包贴的是本机
//     临时身份，没有证书链、没有团队 ID，也不比替换前更可信。
//   - codesign --verify 只回答「这个包的签名与内容自洽吗」。ad-hoc 签名同样能通过，
//     所以它**不是**身份验证：任何拿到写权限的人都能生成一份能过的 ad-hoc 签名。
//   - 产物未做 Apple 公证（notarization），因此 Gatekeeper 也给不出「发布者是谁」的
//     结论；我们自己在 replaceBundle 里主动去掉了 com.apple.quarantine，等于放弃了
//     这一层系统检查。
//   - 真正的信任边界在别处：GitHub 账号 + 仓库写权限 + HTTPS，加上「zip 与
//     checksums.txt 出自同一个 Release」这一条自洽性约束。它们证明的是「没有被
//     第三方中途替换、也没有上传错包」，而不是「授权发布者就是我以为的那个人」。
//     仓库或账号失守时，攻击者可以同时替换 zip 与 checksums.txt，这条链不会报警。
//   - 若将来做发布者签名，校验点不在重签这里，而在 Install 里下载完成之后、
//     解压之前的原始压缩包上（见那里的注释）。重签只处理「能不能启动」，
//     与「该不该信任」是两件事，不要合并。
//
// 因此本函数刻意不增加任何「看起来更安全」的动作：多做一次 verify、多打印一句
// 「签名有效」，只会让调用方误以为身份已经核验过。要提升保证，得换信任根
// （Minisign/EdDSA 公钥，或 Developer ID + 公证），而不是在这里加料。
func ensureSignature(appDir string) error {
	if _, err := resolveTool("codesign"); err != nil {
		return nil
	}
	return ensureSignatureWith(appDir, runCommand)
}

// ensureSignatureWith 是 ensureSignature 的实现，run 让测试能替掉真实的 codesign。
//
// 「签名已存在」的判断保留在这里：verify 能过就原样不动，因为重签会把发布方的
// 签名标识换成 ad-hoc。verify 不过才 --force 重签，而重签失败必须变成错误 ——
// 那正是「更新完打不开」的直接原因。
func ensureSignatureWith(appDir string, run commandFunc) error {
	verifyArgs := []string{"--verify", "--deep", "--strict", appDir}
	if _, err := run("codesign", verifyArgs...); err == nil {
		return nil
	}
	signArgs := []string{"--force", "--deep", "--sign", "-", appDir}
	if output, err := run("codesign", signArgs...); err != nil {
		return commandFailure("codesign", signArgs, output, err)
	}
	return nil
}

// RelaunchAfterExit 写一个分离的 shell 脚本：等当前进程退出，再打开新包。
//
// 为什么不在进程里直接 `open`：旧进程还占着窗口，新进程起来会两个图标并排；
// 交给脚本等 PID 消失再启动，用户看到的是一次干净的重启。
func RelaunchAfterExit(appPath string, pid int, workDir string) error {
	scriptPath, err := writeRelaunchScript(workDir, appPath, pid)
	if err != nil {
		return err
	}
	command := exec.Command("/bin/sh", scriptPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdout = nil
	command.Stderr = nil
	return command.Start()
}

// writeRelaunchScript 用 O_EXCL 建脚本文件，返回最终落盘的路径。
//
// 为什么不能用 os.WriteFile：目标已存在时它会截断原有文件；如果那个路径被预先
// 放成指向别处的 symlink，写入还会顺着链接落到 symlink 目标上（经典 symlink
// attack）。工作目录在用户缓存目录里、名字又是可预测的 relaunch-<pid>.sh，
// 所以这里只接受「我新建了这个文件」：O_CREATE|O_EXCL 让已存在的普通文件或
// symlink 都变成 EEXIST，而不是一次静默覆盖。
//
// 冲突时的回退是换带随机后缀的新名字重试（最多 relaunchCreateAttempts 次）。
// 只有 EEXIST 才重试：目录不可写、磁盘满之类换名字也没用，直接报错更诚实。
func writeRelaunchScript(workDir string, appPath string, pid int) (string, error) {
	if workDir == "" {
		workDir = DefaultWorkDir()
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}
	body := []byte(relaunchScript(appPath, pid))
	base := fmt.Sprintf("relaunch-%d", pid)
	var lastErr error
	for attempt := 0; attempt < relaunchCreateAttempts; attempt++ {
		name := base + ".sh"
		if attempt > 0 {
			suffix, err := randomSuffix()
			if err != nil {
				return "", err
			}
			name = base + "-" + suffix + ".sh"
		}
		scriptPath := filepath.Join(workDir, name)
		file, err := os.OpenFile(scriptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				lastErr = err
				continue
			}
			return "", fmt.Errorf("创建重启脚本 %s 失败：%w", scriptPath, err)
		}
		if _, err := file.Write(body); err != nil {
			file.Close()
			_ = os.Remove(scriptPath)
			return "", fmt.Errorf("写入重启脚本 %s 失败：%w", scriptPath, err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(scriptPath)
			return "", fmt.Errorf("写入重启脚本 %s 失败：%w", scriptPath, err)
		}
		// 创建模式会受 umask 影响，这里再显式确认一次 0700：脚本里有可执行路径，
		// 不该让同机其他用户读到或改到。
		if err := os.Chmod(scriptPath, 0o700); err != nil {
			_ = os.Remove(scriptPath)
			return "", fmt.Errorf("设置重启脚本 %s 权限失败：%w", scriptPath, err)
		}
		return scriptPath, nil
	}
	return "", fmt.Errorf("在 %s 里创建重启脚本失败：连试 %d 个名字都已被占用（最后一个错误：%v）",
		workDir, relaunchCreateAttempts, lastErr)
}

// randomSuffix 给脚本名一个本地攻击者猜不到的后缀。
func randomSuffix() (string, error) {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", fmt.Errorf("生成重启脚本名的随机后缀失败：%w", err)
	}
	return hex.EncodeToString(buffer[:]), nil
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
