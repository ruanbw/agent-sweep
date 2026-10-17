package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖两个修复：
//   - U3：下载必须有字节上限，超限时既要即时中止，也要清掉 .part；
//   - C1：重启脚本必须以 O_EXCL 创建，不能截断已有文件、也不能顺着 symlink 写出去。
//
// 全部用 httptest 本地服务器与临时目录：不发真实网络请求，也不执行 /bin/sh。

// TestDownloadSmallFileUnderLimit 锁定正常路径：小文件能完整落盘，进度与内容都对。
func TestDownloadSmallFileUnderLimit(t *testing.T) {
	payload := []byte("PK\x03\x04 small update payload")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "update.zip")
	var lastDone, lastTotal int64
	err := testClient(server).Download(context.Background(),
		Asset{Name: "AgentSweep.zip", URL: server.URL + "/AgentSweep.zip", Size: int64(len(payload))},
		destination, func(done, total int64) {
			lastDone, lastTotal = done, total
		})
	if err != nil {
		t.Fatalf("上限内的下载不应失败：%v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("下载内容 = %q，期望 %q", data, payload)
	}
	if lastDone != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Fatalf("进度 = %d/%d，期望 %d", lastDone, lastTotal, len(payload))
	}
	if _, err := os.Stat(destination + ".part"); !os.IsNotExist(err) {
		t.Fatalf("成功的下载不应留下 .part，stat err = %v", err)
	}
}

// TestDownloadDefaultLimitIsWired 确认 Download 走的是包级上限，而不是忘了传 limit：
// 声明 maxDownloadBytes+1 字节时必须在落盘前拒绝。
func TestDownloadDefaultLimitIsWired(t *testing.T) {
	declared := int64(maxDownloadBytes) + 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(declared, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "update.zip")
	err := testClient(server).Download(context.Background(),
		Asset{Name: "AgentSweep.zip", URL: server.URL + "/AgentSweep.zip", Size: declared},
		destination, nil)
	if err == nil {
		t.Fatal("默认上限没有生效：声明超过 512 MiB 的下载必须被拒绝")
	}
	for _, fragment := range []string{"上限", strconv.FormatInt(declared, 10), strconv.FormatInt(maxDownloadBytes, 10)} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("错误信息应包含 %q，实际：%v", fragment, err)
		}
	}
	for _, path := range []string{destination, destination + ".part"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s 不应被创建，stat err = %v", path, statErr)
		}
	}
}

// TestDownloadRejectsOversizedDeclaredLength 覆盖「还没开始下载就拒绝」：
// server 只发响应头（声明 1 GiB）便不再发 body。若实现不是先看声明长度，
// 这里会卡在读 body 上直到 context 超时；先看就应当立刻返回大小相关的错误。
func TestDownloadRejectsOversizedDeclaredLength(t *testing.T) {
	const declared = int64(1) << 30 // 1 GiB：真读会卡住，也会把磁盘写满
	const limit = int64(1) << 20    // 注入的 1 MiB 小上限

	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(declared, 10))
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// 故意一个 body 字节都不发。客户端读完 header 就应当放弃并关闭响应体，
		// 这个 channel 因此会很快关闭；若它在读 body，则要等下面的 5 秒兜底。
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		close(disconnected)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	destination := filepath.Join(t.TempDir(), "update.zip")
	start := time.Now()
	err := testClient(server).downloadLimited(ctx,
		Asset{Name: "AgentSweep.zip", URL: server.URL + "/AgentSweep.zip"},
		destination, nil, limit)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("声明长度超过上限时必须在下载前拒绝")
	}
	for _, fragment := range []string{
		"上限",
		strconv.FormatInt(declared, 10),
		strconv.FormatInt(limit, 10),
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("错误信息应包含 %q，实际：%v", fragment, err)
		}
	}
	// 有提前拒绝时耗时是毫秒级；没有的话会等到 3 秒的 context 超时。
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("应在读 body 前就拒绝，实际耗时 %v（错误：%v）", elapsed, err)
	}
	for _, path := range []string{destination, destination + ".part"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s 不应被创建，stat err = %v", path, statErr)
		}
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("拒绝后没有关闭响应体，server 仍在等客户端读")
	}
}

// TestDownloadAbortsWhenBodyExceedsLimit 覆盖「长度未知、实际流超限」：
// server 用 chunked 持续输出 1 MiB，注入 64 KiB 上限，必须在超限处中止。
func TestDownloadAbortsWhenBodyExceedsLimit(t *testing.T) {
	const limit = int64(64 << 10)
	const bodySize = int64(1 << 20)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不写 Content-Length：响应是 chunked，长度未知，只能靠实读字节数兜底。
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for sent := int64(0); sent < bodySize; sent += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "update.zip")
	err := testClient(server).downloadLimited(context.Background(),
		Asset{Name: "AgentSweep.zip", URL: server.URL + "/AgentSweep.zip"},
		destination, nil, limit)
	if err == nil {
		t.Fatal("流超过上限时必须失败")
	}
	// 错误要同时说清「上限是多少」和「已经收到多少」。
	for _, fragment := range []string{
		"上限",
		strconv.FormatInt(limit, 10),
		strconv.FormatInt(limit+1, 10),
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("错误信息应包含 %q，实际：%v", fragment, err)
		}
	}
	for _, path := range []string{destination, destination + ".part"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("超限中止后 %s 必须被清理，stat err = %v", path, statErr)
		}
	}
}

// TestCopyDownloadBodyEnforcesLimit 直接锁定计数逻辑的边界：限度内写完，
// 超限只写 limit+1 字节（用于判定「还有更多」），且不会继续读下去。
func TestCopyDownloadBodyEnforcesLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 200)

	t.Run("上限内完整写出", func(t *testing.T) {
		var sink bytes.Buffer
		written, err := copyDownloadBody(&sink, bytes.NewReader(payload[:100]), 100)
		if err != nil {
			t.Fatalf("恰好等于上限不应报错：%v", err)
		}
		if written != 100 || sink.Len() != 100 {
			t.Fatalf("写入 = %d，缓冲区 = %d，期望 100", written, sink.Len())
		}
	})

	t.Run("超限写满上限加一字节", func(t *testing.T) {
		var sink bytes.Buffer
		written, err := copyDownloadBody(&sink, bytes.NewReader(payload), 64)
		if err == nil {
			t.Fatal("超过上限时必须报错")
		}
		if !errors.Is(err, errDownloadTooLarge) {
			t.Fatalf("错误应可识别为超限，实际：%v", err)
		}
		if written != 65 || sink.Len() != 65 {
			t.Fatalf("写入 = %d，缓冲区 = %d，期望 65", written, sink.Len())
		}
		if !strings.Contains(err.Error(), "上限 64 字节") {
			t.Fatalf("错误应写明上限，实际：%v", err)
		}
	})

	t.Run("非正上限直接拒绝", func(t *testing.T) {
		var sink bytes.Buffer
		if _, err := copyDownloadBody(&sink, bytes.NewReader(payload), 0); err == nil {
			t.Fatal("0 上限必须被拒绝")
		}
		if sink.Len() != 0 {
			t.Fatalf("拒绝时不应写入任何字节，实际 %d", sink.Len())
		}
	})
}

// assertRelaunchScript 断言脚本是一个真实的普通文件、权限 0700、内容与生成函数一致。
func assertRelaunchScript(t *testing.T, scriptPath, wantContent string) {
	t.Helper()
	info, err := os.Lstat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s 不应是 symlink", scriptPath)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("%s 权限 = %04o，期望 0700", scriptPath, info.Mode().Perm())
	}
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != wantContent {
		t.Fatalf("脚本内容 = %q，期望 %q", data, wantContent)
	}
	if !strings.HasPrefix(string(data), "#!/bin/sh") || !strings.Contains(string(data), "open -n ") {
		t.Fatalf("脚本不是可用的重启脚本：%s", data)
	}
}

// TestWriteRelaunchScriptDoesNotTruncateExistingFile：目标名已存在普通文件时，
// 必须换新名字，旧文件一个字节都不能动。
func TestWriteRelaunchScriptDoesNotTruncateExistingFile(t *testing.T) {
	workDir := t.TempDir()
	pid := 4242
	appPath := "/Applications/My App's AgentSweep.app"
	existing := filepath.Join(workDir, fmt.Sprintf("relaunch-%d.sh", pid))
	const original = "#!/bin/sh\n# 上一个进程留下的脚本，不能被截断\n"
	if err := os.WriteFile(existing, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	scriptPath, err := writeRelaunchScript(workDir, appPath, pid)
	if err != nil {
		t.Fatalf("名字冲突时应回退到新文件名，实际：%v", err)
	}
	if scriptPath == existing {
		t.Fatalf("已存在的路径不能被复用：%s", scriptPath)
	}
	base := filepath.Base(scriptPath)
	if !strings.HasPrefix(base, fmt.Sprintf("relaunch-%d-", pid)) || !strings.HasSuffix(base, ".sh") {
		t.Fatalf("回退文件名 %q 应带随机后缀", base)
	}

	unchanged, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != original {
		t.Fatalf("旧文件被截断/改写：%q", unchanged)
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("旧文件权限被改了：%04o", info.Mode().Perm())
	}

	assertRelaunchScript(t, scriptPath, relaunchScript(appPath, pid))
}

// TestWriteRelaunchScriptDoesNotFollowSymlink 是 C1 的关键断言：
// 预置的同名 symlink 不能把脚本写到它的目标上。
func TestWriteRelaunchScriptDoesNotFollowSymlink(t *testing.T) {
	workDir := t.TempDir()
	pid := 5150
	appPath := "/Applications/AgentSweep.app"
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim.txt")
	const victimContent = "外部文件：既不能被截断，也不能被写入\n"
	if err := os.WriteFile(victim, []byte(victimContent), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workDir, fmt.Sprintf("relaunch-%d.sh", pid))
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	scriptPath, err := writeRelaunchScript(workDir, appPath, pid)
	if err != nil {
		t.Fatalf("symlink 冲突时应回退到新文件名，实际：%v", err)
	}
	if scriptPath == link {
		t.Fatalf("不能顺着 symlink 写：%s", scriptPath)
	}

	// 关键断言：symlink 指向的外部文件内容一个字都没变。
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != victimContent {
		t.Fatalf("symlink 目标被写入/截断：%q", got)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("预置的 symlink 不应被替换：%v", linkInfo.Mode())
	}
	if target, err := os.Readlink(link); err != nil || target != victim {
		t.Fatalf("symlink 目标 = %q，err = %v，期望 %q", target, err, victim)
	}

	assertRelaunchScript(t, scriptPath, relaunchScript(appPath, pid))
}

// TestWriteRelaunchScriptModeIs0700 单独锁定权限位（umask 不应把它削掉）。
func TestWriteRelaunchScriptModeIs0700(t *testing.T) {
	workDir := t.TempDir()
	scriptPath, err := writeRelaunchScript(workDir, "/Applications/AgentSweep.app", 1234)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("%s 权限 = %04o，期望 0700", scriptPath, perm)
	}
}

// TestWriteRelaunchScriptKeepsPathEscaping：含空格/引号的路径仍要按单引号规则转义。
// 只断言生成的文本，不执行脚本。
func TestWriteRelaunchScriptKeepsPathEscaping(t *testing.T) {
	workDir := t.TempDir()
	appPath := `/Applications/My App's "Agent Sweep".app`
	scriptPath, err := writeRelaunchScript(workDir, appPath, 31337)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	quoted := shellQuote(appPath)
	// xattr 与 open 两处都必须用转义后的路径。
	if count := strings.Count(string(data), quoted); count != 2 {
		t.Fatalf("转义后的路径出现 %d 次，期望 2 次：\n%s", count, data)
	}
	if !strings.Contains(string(data), `'\''`) {
		t.Fatalf("单引号应转义成 '\\''：\n%s", data)
	}
	if strings.Contains(string(data), appPath) {
		t.Fatalf("未转义的原样路径不应出现：\n%s", data)
	}
}
