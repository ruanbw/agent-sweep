package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试不碰真实网络、也不碰真实的应用包：发布用 httptest 假扮，
// 目标 .app 建在临时目录里。更新逻辑最危险的失败模式是「把用户的包搞坏」，
// 所以每个失败用例都要顺带断言旧包还在、还能读出版本号。

func infoPlist(version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>AgentSweep</string>
	<key>CFBundleShortVersionString</key>
	<string>` + version + `</string>
</dict>
</plist>
`
}

func writeBundle(t *testing.T, parent, version string) string {
	t.Helper()
	app := filepath.Join(parent, "AgentSweep.app")
	macOS := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macOS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(infoPlist(version)), 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(macOS, "AgentSweep")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho agentsweep\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

// makeAppZip 造一个 ditto 形状的包：唯一顶层目录 AgentSweep.app。
func makeAppZip(t *testing.T, version string) []byte {
	t.Helper()
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	add := func(name, body string, mode os.FileMode) {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("AgentSweep.app/", "", os.ModeDir|0o755)
	add("AgentSweep.app/Contents/Info.plist", infoPlist(version), 0o644)
	add("AgentSweep.app/Contents/MacOS/AgentSweep", "#!/bin/sh\necho agentsweep\n", 0o755)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type fakeRelease struct {
	server *httptest.Server
	zip    []byte
	name   string
}

// serveRelease 假扮 GitHub Releases：latest、附件、校验文件三个端点。
func serveRelease(t *testing.T, tag, zipName string, zipBytes []byte, checksumOverride string) *fakeRelease {
	t.Helper()
	fixture := &fakeRelease{zip: zipBytes, name: zipName}
	sum := sha256.Sum256(zipBytes)
	checksum := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		checksum = checksumOverride
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"tag_name": %q,
			"name": %q,
			"body": "修复了扫描器崩溃",
			"html_url": %q,
			"published_at": "2026-10-17T12:00:00Z",
			"assets": [
				{"name": %q, "browser_download_url": %q, "size": %d},
				{"name": "checksums.txt", "browser_download_url": %q, "size": 100}
			]
		}`, tag, "AgentSweep "+tag,
			fixture.server.URL+"/releases/"+tag,
			zipName, fixture.server.URL+"/download/"+zipName, len(zipBytes),
			fixture.server.URL+"/download/checksums.txt")
	})
	mux.HandleFunc("/download/"+zipName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(fixture.zip)))
		_, _ = w.Write(fixture.zip)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", checksum, zipName)
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func testClient(server *httptest.Server) *Client {
	return &Client{
		HTTP:      server.Client(),
		APIBase:   server.URL,
		Repo:      "owner/repo",
		GOOS:      "darwin",
		GOARCH:    "amd64",
		UserAgent: "AgentSweep/0.1.0",
	}
}

func TestLatestParsesRelease(t *testing.T) {
	fixture := serveRelease(t, "v0.1.1", "AgentSweep-v0.1.1-darwin-universal.zip", makeAppZip(t, "0.1.1"), "")
	release, err := testClient(fixture.server).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.Tag != "v0.1.1" || release.Version != "0.1.1" {
		t.Fatalf("解析出的 tag/version = %q/%q", release.Tag, release.Version)
	}
	if release.PageURL == "" || release.Notes == "" {
		t.Fatalf("发布页或说明为空：%+v", release)
	}
	if len(release.Assets) != 2 {
		t.Fatalf("资产数量 = %d", len(release.Assets))
	}
}

func TestLatestWithoutRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	if _, err := testClient(server).Latest(context.Background()); err != ErrNoRelease {
		t.Fatalf("未发布时应返回 ErrNoRelease，得到 %v", err)
	}
}

func TestAssetForPrefersUniversal(t *testing.T) {
	release := &Release{
		Version: "0.1.1",
		Assets: []Asset{
			{Name: "agentsweep-v0.1.1-darwin-universal.tar.gz"},
			{Name: "AgentSweep-v0.1.1-darwin-arm64.zip"},
			{Name: "AgentSweep-v0.1.1-darwin-amd64.zip"},
			{Name: "AgentSweep-v0.1.1-darwin-universal.zip"},
		},
	}
	client := &Client{GOOS: "darwin", GOARCH: "amd64"}
	asset, err := client.AssetFor(release)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != "AgentSweep-v0.1.1-darwin-universal.zip" {
		t.Fatalf("应选 universal zip，实际 %q", asset.Name)
	}

	client.GOARCH = "arm64"
	release.Assets = release.Assets[1:3]
	asset, err = client.AssetFor(release)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != "AgentSweep-v0.1.1-darwin-arm64.zip" {
		t.Fatalf("应选 arm64 zip，实际 %q", asset.Name)
	}
}

func TestAssetForMissing(t *testing.T) {
	release := &Release{Assets: []Asset{{Name: "AgentSweep-v0.1.1-darwin-arm64.zip"}}}
	client := &Client{GOOS: "darwin", GOARCH: "amd64"}
	if _, err := client.AssetFor(release); err == nil {
		t.Fatal("没有可用包时应该报错")
	}
}

func TestDownload(t *testing.T) {
	fixture := serveRelease(t, "v0.1.1", "AgentSweep-v0.1.1-darwin-amd64.zip", makeAppZip(t, "0.1.1"), "")
	client := testClient(fixture.server)
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := client.AssetFor(release)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "update.zip")
	var lastDone, lastTotal int64
	if err := client.Download(context.Background(), asset, destination, func(done, total int64) {
		lastDone, lastTotal = done, total
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, fixture.zip) {
		t.Fatal("下载内容与发布不一致")
	}
	if lastDone != int64(len(fixture.zip)) || lastTotal != int64(len(fixture.zip)) {
		t.Fatalf("进度 = %d/%d，期望 %d", lastDone, lastTotal, len(fixture.zip))
	}
}

func TestInstallReplacesBundle(t *testing.T) {
	parent := t.TempDir()
	target := writeBundle(t, parent, "0.1.0")
	fixture := serveRelease(t, "v0.1.1", "AgentSweep-v0.1.1-darwin-amd64.zip", makeAppZip(t, "0.1.1"), "")
	client := testClient(fixture.server)
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var percents []int
	result, err := client.Install(context.Background(), release, target, t.TempDir(), func(percent int, note string) {
		percents = append(percents, percent)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != "0.1.1" || result.PreviousVersion != "0.1.0" {
		t.Fatalf("结果版本 = %+v", result)
	}
	version, err := bundleVersion(target)
	if err != nil || version != "0.1.1" {
		t.Fatalf("替换后版本 = %q，err = %v", version, err)
	}
	executable := filepath.Join(target, "Contents", "MacOS", "AgentSweep")
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("可执行位在复制后丢了")
	}
	if len(percents) == 0 || percents[len(percents)-1] != 100 {
		t.Fatalf("进度序列 = %v，应以 100 结尾", percents)
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	parent := t.TempDir()
	target := writeBundle(t, parent, "0.1.0")
	fixture := serveRelease(t, "v0.1.1", "AgentSweep-v0.1.1-darwin-amd64.zip", makeAppZip(t, "0.1.1"),
		strings.Repeat("0", 64))
	client := testClient(fixture.server)
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Install(context.Background(), release, target, t.TempDir(), nil); err == nil {
		t.Fatal("校验和不匹配时必须失败")
	} else if !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("错误信息应说明校验失败：%v", err)
	}
	if version, _ := bundleVersion(target); version != "0.1.0" {
		t.Fatalf("失败后旧包被改动了：版本 %q", version)
	}
}

func TestInstallRejectsVersionMismatch(t *testing.T) {
	parent := t.TempDir()
	target := writeBundle(t, parent, "0.1.0")
	// 标签说 0.1.1，包里却是 0.1.0：以包内 Info.plist 为准，拒绝安装。
	fixture := serveRelease(t, "v0.1.1", "AgentSweep-v0.1.1-darwin-amd64.zip", makeAppZip(t, "0.1.0"), "")
	client := testClient(fixture.server)
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Install(context.Background(), release, target, t.TempDir(), nil); err == nil {
		t.Fatal("包内版本不一致时必须失败")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("错误信息应说明版本不一致：%v", err)
	}
	if version, _ := bundleVersion(target); version != "0.1.0" {
		t.Fatalf("失败后旧包被改动了：版本 %q", version)
	}
}

func TestInstallRejectsSameVersion(t *testing.T) {
	parent := t.TempDir()
	target := writeBundle(t, parent, "0.1.0")
	fixture := serveRelease(t, "v0.1.0", "AgentSweep-v0.1.0-darwin-amd64.zip", makeAppZip(t, "0.1.0"), "")
	client := testClient(fixture.server)
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Install(context.Background(), release, target, t.TempDir(), nil); err == nil {
		t.Fatal("同版本重装应该被拒绝")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	entry, _ := writer.Create("AgentSweep.app/../../evil")
	_, _ = entry.Write([]byte("pwned"))
	_ = writer.Close()

	archive := filepath.Join(t.TempDir(), "evil.zip")
	if err := os.WriteFile(archive, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractApp(archive, t.TempDir()); err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
}

func TestExtractRejectsMultipleRoots(t *testing.T) {
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	first, _ := writer.Create("AgentSweep.app/Contents/Info.plist")
	_, _ = first.Write([]byte(infoPlist("0.1.1")))
	second, _ := writer.Create("Other.app/Contents/Info.plist")
	_, _ = second.Write([]byte(infoPlist("0.1.1")))
	_ = writer.Close()

	archive := filepath.Join(t.TempDir(), "multi.zip")
	if err := os.WriteFile(archive, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractApp(archive, t.TempDir()); err == nil {
		t.Fatal("多个顶层目录必须被拒绝")
	}
}

func TestDetectBundleOutsideApp(t *testing.T) {
	if _, err := DetectBundle(); err != ErrNotAppBundle {
		t.Fatalf("测试二进制不在 .app 里，应返回 ErrNotAppBundle，得到 %v", err)
	}
}

func TestRelaunchScriptQuotesPath(t *testing.T) {
	script := relaunchScript("/Applications/My App's AgentSweep.app", 4242)
	if !strings.Contains(script, "kill -0 4242") {
		t.Fatalf("脚本没有等 PID：%s", script)
	}
	// 单引号必须被转义成 '\''，否则路径里有引号时 /bin/sh 会解析错。
	if !strings.Contains(script, `'/Applications/My App'\''s AgentSweep.app'`) {
		t.Fatalf("脚本里的路径没有正确转义：%s", script)
	}
	if !strings.Contains(script, "open -n") {
		t.Fatalf("脚本没有重开应用：%s", script)
	}
	if !strings.Contains(script, "600") {
		t.Fatalf("脚本没有等待上限，PID 复用时会永远转下去：%s", script)
	}
}

func TestNewClientRejectsInsecureAPIBase(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		repo     string
		wantAPI  string
		wantRepo string
	}{
		{name: "https honored", base: "https://ghe.example.com/api/v3", repo: "acme/agent-sweep",
			wantAPI: "https://ghe.example.com/api/v3", wantRepo: "acme/agent-sweep"},
		{name: "loopback http honored", base: "http://127.0.0.1:9999",
			wantAPI: "http://127.0.0.1:9999", wantRepo: Repo},
		{name: "localhost http honored", base: "http://localhost:9999",
			wantAPI: "http://localhost:9999", wantRepo: Repo},
		{name: "remote http ignored", base: "http://evil.example.com", repo: "evil/repo",
			wantAPI: DefaultAPIBase, wantRepo: Repo},
		{name: "repo alone ignored", base: "", repo: "evil/repo",
			wantAPI: DefaultAPIBase, wantRepo: Repo},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("AGENTSWEEP_UPDATE_API_BASE", testCase.base)
			t.Setenv("AGENTSWEEP_UPDATE_REPO", testCase.repo)
			client := NewClient("0.1.0")
			if client.APIBase != testCase.wantAPI || client.Repo != testCase.wantRepo {
				t.Fatalf("APIBase/Repo = %q/%q，期望 %q/%q", client.APIBase, client.Repo, testCase.wantAPI, testCase.wantRepo)
			}
		})
	}
}

func TestAssetForRejectsTraversalName(t *testing.T) {
	release := &Release{Assets: []Asset{
		{Name: "AgentSweep-darwin-universal-../../../../x.zip", URL: "https://example.invalid/x"},
	}}
	client := &Client{GOOS: "darwin", GOARCH: "amd64"}
	if asset, err := client.AssetFor(release); err == nil {
		t.Fatalf("带路径分隔符的附件名必须被拒绝，实际选中 %q", asset.Name)
	}
}

func TestDownloadRejectsUnsafeName(t *testing.T) {
	client := &Client{}
	if err := client.Download(context.Background(), Asset{Name: "../x.zip", URL: "https://example.invalid/x"}, filepath.Join(t.TempDir(), "x"), nil); err == nil {
		t.Fatal("不安全附件名必须被拒绝")
	}
}

func TestLatestRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	_, err := testClient(server).Latest(context.Background())
	if err == nil {
		t.Fatal("429 必须返回错误")
	}
	if !strings.Contains(err.Error(), "限流") || !strings.Contains(err.Error(), "60") {
		t.Fatalf("错误信息应说明限流与 Retry-After：%v", err)
	}
}

func TestExtractSkipsMacOSXEntries(t *testing.T) {
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	add := func(name, body string) {
		entry, _ := writer.Create(name)
		_, _ = entry.Write([]byte(body))
	}
	// ditto --sequesterRsrc 的真实形状：__MACOSX 是第二个顶层目录。
	add("AgentSweep.app/Contents/Info.plist", infoPlist("0.1.1"))
	add("AgentSweep.app/Contents/MacOS/AgentSweep", "#!/bin/sh\n")
	add("__MACOSX/._AgentSweep.app", "resource fork")
	add("__MACOSX/AgentSweep.app/._Info.plist", "resource fork")
	_ = writer.Close()

	archive := filepath.Join(t.TempDir(), "ditto.zip")
	if err := os.WriteFile(archive, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	appDir, err := extractApp(archive, t.TempDir())
	if err != nil {
		t.Fatalf("真实的 ditto 产物不应被拒绝：%v", err)
	}
	if _, err := bundleVersion(appDir); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRejectsEscapingSymlink(t *testing.T) {
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	header := &zip.FileHeader{Name: "AgentSweep.app/Contents/Info.plist"}
	entry, _ := writer.CreateHeader(header)
	_, _ = entry.Write([]byte(infoPlist("0.1.1")))

	linkHeader := &zip.FileHeader{Name: "AgentSweep.app/escape"}
	linkHeader.SetMode(os.ModeSymlink | 0o777)
	link, _ := writer.CreateHeader(linkHeader)
	_, _ = link.Write([]byte("../../../etc/passwd"))
	_ = writer.Close()

	archive := filepath.Join(t.TempDir(), "symlink.zip")
	if err := os.WriteFile(archive, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractApp(archive, t.TempDir()); err == nil {
		t.Fatal("越界符号链接必须被拒绝")
	}
}
