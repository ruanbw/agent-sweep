// AgentSweep：扫描并清理本机所有 AI Agent 的会话记录。
//
// 一个二进制，两个入口：
//   - 不带参数 → 打开桌面窗口
//   - scan / clean / list → 无界面，可进脚本与 CI
//
// 之所以让 CLI 和 GUI 共用同一个包，而不是各写一份：扫描规则、路径护栏、
// 索引裁剪这些东西一旦分叉，两边就会对「哪些文件算这条会话」给出不同答案，
// 用户会看到「CLI 说删了、GUI 还列着」。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/clean"
	"github.com/ruanbw/agent-sweep/internal/gui"
	"github.com/ruanbw/agent-sweep/internal/probe"
	"github.com/ruanbw/agent-sweep/internal/scan"
	"github.com/ruanbw/agent-sweep/internal/session"
	"github.com/ruanbw/agent-sweep/internal/update"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runtime.LockOSThread()
		if err := gui.Run(version); err != nil {
			fmt.Fprintln(os.Stderr, "agentsweep:", err)
			os.Exit(1)
		}
		return
	}

	switch args[0] {
	case "scan":
		runScan(args[1:])
	case "clean":
		runClean(args[1:])
	case "list", "ls":
		runList(args[1:])
	case "discover":
		runDiscover(args[1:])
	case "serve":
		runServe(args[1:])
	case "update":
		runUpdate(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`AgentSweep —— 扫描并清理本机 AI Agent 的会话记录

用法：
  agentsweep                 打开桌面窗口
  agentsweep scan [--json]   扫描并以表格/JSON 输出
  agentsweep list            按 Agent 汇总体积
  agentsweep clean --agent <id>       把该 Agent 的全部会话移入废纸篓
  agentsweep clean --agent <id> --permanent
  agentsweep discover [--json]       找目录里没有的 Agent
  agentsweep serve [--addr host:port] 只开 HTTP 服务（调试 UI / 无窗口环境）
  agentsweep update [--check]        检查并安装新版本（GUI 里也能做）
  agentsweep version
`)
}

// runScan 扫描。--json 的输出结构与 HTTP 接口保持一致，
// 这样脚本和 UI 拿到的是同一份数据，不会出现两套字段名。
func runScan(args []string) {
	flags := flag.NewFlagSet("scan", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "输出 JSON")
	_ = flags.Parse(args)

	started := time.Now()
	results := scan.All(catalog.All())
	elapsed := time.Since(started)

	if *asJSON {
		payload := map[string]any{
			"version":    version,
			"durationMs": elapsed.Milliseconds(),
			"agents":     results,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(payload)
		return
	}

	total := 0
	var bytes int64
	for _, result := range results {
		if len(result.Sessions) > 0 {
			total += len(result.Sessions)
			bytes += result.TotalBytes
		}
	}
	fmt.Printf("扫描完成 · %d 个 Agent 已安装 · %d 条会话 · %s · 耗时 %s\n\n",
		countInstalled(results), total, humanBytes(bytes), elapsed.Round(time.Millisecond))

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "AGENT\t会话\t体积\t数据目录")
	for _, result := range results {
		if !result.Installed {
			continue
		}
		home := result.Home
		if len(home) > 48 {
			home = "…" + home[len(home)-47:]
		}
		fmt.Fprintf(writer, "%s\t%d\t%s\t%s\n", result.Name, len(result.Sessions), humanBytes(result.TotalBytes), home)
	}
	_ = writer.Flush()
}

func runList(args []string) {
	results := scan.All(catalog.All())
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "AGENT\t会话\t体积")
	for _, result := range results {
		if len(result.Sessions) == 0 {
			continue
		}
		fmt.Fprintf(writer, "%s\t%d\t%s\n", result.Name, len(result.Sessions), humanBytes(result.TotalBytes))
		fmt.Fprintf(writer, "\t\t%s\n", "")
	}
	_ = writer.Flush()
}

func runClean(args []string) {
	flags := flag.NewFlagSet("clean", flag.ExitOnError)
	agentID := flags.String("agent", "", "要清理的 Agent ID")
	permanent := flags.Bool("permanent", false, "直接删除而不是移入废纸篓")
	_ = flags.Parse(args)

	if *agentID == "" {
		fmt.Fprintln(os.Stderr, "缺少 --agent。先跑 `agentsweep scan` 看有哪些 Agent。")
		os.Exit(2)
	}
	spec, ok := catalog.Find(*agentID)
	if !ok {
		fmt.Fprintf(os.Stderr, "未知 Agent %q\n", *agentID)
		os.Exit(2)
	}
	home := spec.Home(nil)
	if home == "" {
		fmt.Fprintf(os.Stderr, "%s 没有安装（数据目录不存在）\n", spec.Name)
		os.Exit(1)
	}
	result := scan.One(spec)
	if len(result.Sessions) == 0 {
		fmt.Printf("%s 没有会话\n", spec.Name)
		return
	}

	mode := "trash"
	if *permanent {
		mode = "permanent"
	}
	cleaner := clean.New(clean.Options{
		Mode:       mode,
		BackupDir:  catalog.AppDir() + "/backups",
		JournalDir: catalog.AppDir() + "/journal",
	})
	deleted, err := cleaner.Delete(spec, home, sortedCopy(result.Sessions))
	if err != nil {
		fmt.Fprintln(os.Stderr, "清理失败：", err)
		os.Exit(1)
	}
	if mode == "trash" {
		fmt.Printf("已移入废纸篓 · %d 条（批次 %s）\n", deleted.Count, deleted.BatchID)
	} else {
		fmt.Printf("已删除 · %d 条 · 释放 %s\n", deleted.Count, humanBytes(deleted.Freed))
	}
	for _, item := range deleted.Errors {
		fmt.Fprintf(os.Stderr, "  ! %s: %s\n", item.UID, item.Message)
	}
}

// runServe 只起 HTTP 服务，不开窗口。
//
// 存在的理由有两个：一是在没有 WindowServer 的环境（CI、SSH）里也能看 UI；
// 二是排查「界面上没显示」时，可以直接 curl /api/bootstrap 看后端到底给了什么，
// 不用在原生窗口和日志之间来回猜。
func runServe(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", "127.0.0.1:8765", "监听地址")
	_ = flags.Parse(args)
	if err := gui.Serve(*addr, version); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// runUpdate 把 GUI 的更新流程搬进终端。
//
// 它和 GUI 走同一个 internal/update 包：挑包、校验、替换是同一份实现，
// 所以不会出现「CLI 说装好了、GUI 又说有更新」这种两边各有一套规则的分叉。
func runUpdate(args []string) {
	flags := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := flags.Bool("check", false, "只检查，不安装")
	asJSON := flags.Bool("json", false, "输出 JSON（不安装）")
	_ = flags.Parse(args)

	client := update.NewClient(version)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	release, err := client.Latest(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "检查更新失败：", err)
		os.Exit(1)
	}
	available := update.IsNewer(version, release.Version)

	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(map[string]any{
			"current":   version,
			"latest":    release.Version,
			"available": available,
			"pageUrl":   release.PageURL,
		})
		return
	}

	fmt.Printf("当前 %s · 最新 %s\n", version, release.Version)
	if !available {
		fmt.Println("已是最新版本。")
		return
	}
	fmt.Printf("新版本 %s 可用：%s\n", release.Version, release.PageURL)
	if *checkOnly {
		return
	}

	target, err := update.DetectBundle()
	if err != nil {
		fmt.Fprintln(os.Stderr, "自动安装跳过：", err)
		fmt.Printf("请从发布页手动下载：%s\n", release.PageURL)
		os.Exit(2)
	}

	installCtx, installCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer installCancel()
	result, err := client.Install(installCtx, release, target, update.DefaultWorkDir(), func(percent int, note string) {
		fmt.Fprintf(os.Stderr, "\r%3d%% %s", percent, note)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n安装失败：", err)
		os.Exit(1)
	}
	if result.PreviousVersion == "" {
		fmt.Printf("\r已更新到 %s · %s\n", result.Version, result.Path)
		return
	}
	fmt.Printf("\r已更新 %s → %s · %s\n", result.PreviousVersion, result.Version, result.Path)
}

// runDiscover 打印指纹发现的候选。
//
// 输出里带 suggested，是为了让「命令行发现 → 手工编辑 → 放进 adapters.json」
// 这条路走得通：用户不必开 GUI 也能收录一个新 Agent。
func runDiscover(args []string) {
	flags := flag.NewFlagSet("discover", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "输出 JSON")
	_ = flags.Parse(args)

	candidates, err := probe.Discover(catalog.All())
	if err != nil {
		fmt.Fprintln(os.Stderr, "发现失败：", err)
		os.Exit(1)
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(map[string]any{"candidates": candidates})
		return
	}
	if len(candidates) == 0 {
		fmt.Println("没有发现未收录的 Agent。")
		return
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "置信度\t会话\t体积\t路径")
	for _, candidate := range candidates {
		fmt.Fprintf(writer, "%.2f\t%d\t%s\t%s\n",
			candidate.Confidence, candidate.SessionCount, humanBytes(candidate.Bytes), candidate.Path)
	}
	_ = writer.Flush()
}

func sortedCopy(items []session.Session) []session.Session {
	out := append([]session.Session(nil), items...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func countInstalled(results []scan.AgentResult) int {
	n := 0
	for _, result := range results {
		if result.Installed {
			n++
		}
	}
	return n
}

// humanBytes 用 1024 进制。
//
// 不用 1000：这个应用通篇在讲磁盘占用，而 ls -h / du / Finder 都是 1024。
// 同一个 2,411,724 字节，按 1000 报 2.4 MB、按 1024 报 2.3 MB —— 用户会以为删漏了。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit && exp < 4; value /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	text := fmt.Sprintf("%.1f %s", value, suffix)
	return strings.Replace(text, ".0 ", " ", 1)
}
