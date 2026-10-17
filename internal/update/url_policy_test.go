package update

// R16 的回归测试：发布 JSON 里的下载/校验 URL 只能指向 GitHub 的发布资产域名，
// 重定向的每一跳也要过同一套检查。这里不碰外网：httptest 只监听 127.0.0.1，
// 跳转到 evil.example 的用例会在发出请求之前被 CheckRedirect 拒绝。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAllowedDownloadURLAllowsGitHubHosts(t *testing.T) {
	allowed := []string{
		"https://github.com/ruanbw/agent-sweep/releases/download/v0.1.1/AgentSweep-v0.1.1-darwin-universal.zip",
		// 2026 年 GitHub 的实际一跳：github.com → release-assets.githubusercontent.com。
		"https://release-assets.githubusercontent.com/github-production-release-asset/123/456?sp=r&sig=deadbeef",
		"https://objects.githubusercontent.com/github-production-release-asset-2e65be/123/456?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef",
		"https://github-releases.githubusercontent.com/123/456?X-Amz-Signature=deadbeef",
		"https://api.github.com/repos/ruanbw/agent-sweep/releases/latest",
		"https://github.com:443/ruanbw/agent-sweep/releases/download/v0.1.1/x.zip",
	}
	for _, raw := range allowed {
		t.Run(raw, func(t *testing.T) {
			if err := allowedDownloadURL(raw); err != nil {
				t.Fatalf("应允许 %q，实际 %v", raw, err)
			}
		})
	}
}

func TestAllowedDownloadURLRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "非 https",
			raw:  "http://github.com/ruanbw/agent-sweep/releases/download/v0.1.1/x.zip",
			want: []string{"https", "github.com"},
		},
		{
			name: "名单外主机",
			raw:  "https://evil.example/AgentSweep.zip",
			want: []string{"evil.example", "只允许："},
		},
		{
			name: "前缀伪装",
			raw:  "https://github.com.evil.example/AgentSweep.zip",
			want: []string{"github.com.evil.example", "只允许："},
		},
		{
			name: "file 协议",
			raw:  "file:///etc/passwd",
			want: []string{"file", "https"},
		},
		{
			name: "空串",
			raw:  "",
			want: []string{"空"},
		},
		{
			name: "协议相对",
			raw:  "//evil.example/x",
			want: []string{"evil.example", "https"},
		},
		{
			name: "raw.githubusercontent.com 不是发布资产通道",
			raw:  "https://raw.githubusercontent.com/ruanbw/agent-sweep/master/x.zip",
			want: []string{"raw.githubusercontent.com", "只允许："},
		},
		{
			name: "云元数据地址",
			raw:  "http://169.254.169.254/latest/meta-data/",
			want: []string{"169.254.169.254", "https"},
		},
		{
			name: "非标准端口",
			raw:  "https://github.com:8443/x.zip",
			want: []string{"8443", "443"},
		},
		{
			name: "用户信息绕过",
			raw:  "https://github.com@evil.example/x.zip",
			want: []string{"evil.example", "用户信息"},
		},
		{
			name: "ftp 协议",
			raw:  "ftp://github.com/x.zip",
			want: []string{"ftp", "https"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := allowedDownloadURL(testCase.raw)
			if err == nil {
				t.Fatalf("必须拒绝 %q", testCase.raw)
			}
			for _, fragment := range testCase.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("错误信息应包含 %q，实际：%v", fragment, err)
				}
			}
		})
	}
}

func TestCheckRedirectAllowsGitHubAssetHost(t *testing.T) {
	via := []*http.Request{httptest.NewRequest(http.MethodGet,
		"https://github.com/ruanbw/agent-sweep/releases/download/v0.1.1/x.zip", nil)}
	targets := []string{
		"https://release-assets.githubusercontent.com/github-production-release-asset/123/456?sp=r&sig=deadbeef",
		"https://objects.githubusercontent.com/github-production-release-asset-2e65be/123/456?X-Amz-Signature=deadbeef",
	}
	for _, target := range targets {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if err := checkRedirect(req, via); err != nil {
			t.Fatalf("GitHub 资产重定向应被允许（%s），实际 %v", target, err)
		}
	}
}

func TestCheckRedirectRejectsForeignHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://evil.example/AgentSweep.zip", nil)
	via := []*http.Request{httptest.NewRequest(http.MethodGet,
		"https://github.com/ruanbw/agent-sweep/releases/download/v0.1.1/x.zip", nil)}
	err := checkRedirect(req, via)
	if err == nil {
		t.Fatal("跳转到名单外主机必须返回错误，而不是静默跟随")
	}
	for _, fragment := range []string{"evil.example", "github.com", "只允许："} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("错误信息应包含 %q，实际：%v", fragment, err)
		}
	}
}

func TestCheckRedirectLoopbackOnlyWhenPolicyAllows(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/final", nil)
	via := []*http.Request{httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/start", nil)}
	// 默认策略（正式构建）不放行明文回环跳转。
	if err := checkRedirect(req, via); err == nil {
		t.Fatal("默认策略不应允许明文回环重定向")
	}
	// APIBase 本身是回环时（httptest / 本地调试）才放行。
	if err := checkRedirectWith(req, via, true); err != nil {
		t.Fatalf("回环更新源的回环跳转应被允许，实际 %v", err)
	}
}

func TestDownloadRejectsForeignAssetURL(t *testing.T) {
	client := &Client{APIBase: DefaultAPIBase}
	err := client.Download(context.Background(),
		Asset{Name: "AgentSweep.zip", URL: "https://evil.example/AgentSweep.zip"},
		filepath.Join(t.TempDir(), "update.zip"), nil)
	if err == nil {
		t.Fatal("名单外的安装包地址必须在发请求前被拒绝")
	}
	if !strings.Contains(err.Error(), "evil.example") {
		t.Fatalf("错误信息应说明实际 host：%v", err)
	}
}

func TestChecksumsRejectsForeignAssetURL(t *testing.T) {
	release := &Release{Assets: []Asset{
		{Name: "checksums.txt", URL: "https://evil.example/checksums.txt"},
	}}
	client := &Client{APIBase: DefaultAPIBase}
	if _, err := client.Checksums(context.Background(), release); err == nil {
		t.Fatal("名单外的 checksums.txt 地址必须在发请求前被拒绝")
	} else if !strings.Contains(err.Error(), "evil.example") {
		t.Fatalf("错误信息应说明实际 host：%v", err)
	}
}

func TestDownloadRejectsRedirectToForeignHost(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "https://evil.example/AgentSweep.zip", http.StatusFound)
	}))
	defer server.Close()

	client := &Client{
		HTTP:      server.Client(),
		APIBase:   server.URL,
		Repo:      "owner/repo",
		GOOS:      "darwin",
		GOARCH:    "amd64",
		UserAgent: "AgentSweep/0.1.0",
	}
	destination := filepath.Join(t.TempDir(), "update.zip")
	err := client.Download(context.Background(),
		Asset{Name: "AgentSweep.zip", URL: server.URL + "/AgentSweep.zip"}, destination, nil)
	if err == nil {
		t.Fatal("跳转到名单外主机时必须失败")
	}
	for _, fragment := range []string{"重定向", "evil.example", "objects.githubusercontent.com"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("错误信息应包含 %q，实际：%v", fragment, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("不应跟随重定向，实际请求数 = %d", got)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("被拒绝的下载不应落盘，stat err = %v", statErr)
	}
}

func TestDownloadFollowsLoopbackRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{
		HTTP:      server.Client(),
		APIBase:   server.URL,
		Repo:      "owner/repo",
		GOOS:      "darwin",
		GOARCH:    "amd64",
		UserAgent: "AgentSweep/0.1.0",
	}
	destination := filepath.Join(t.TempDir(), "update.zip")
	if err := client.Download(context.Background(),
		Asset{Name: "AgentSweep.zip", URL: server.URL + "/start"}, destination, nil); err != nil {
		t.Fatalf("回环更新源内部的重定向应继续可用：%v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("下载内容 = %q", data)
	}
}

func TestAllowedAPIBaseKeepsLoopbackHTTP(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:1234",
		"http://localhost:9999",
		"http://[::1]:8080",
	} {
		if !allowedAPIBase(base) {
			t.Fatalf("回环 http 更新源应继续可用：%q", base)
		}
	}
	for _, base := range []string{"http://evil.example", "http://10.0.0.1", "file:///etc/passwd"} {
		if allowedAPIBase(base) {
			t.Fatalf("远端明文/非 http 更新源必须被拒绝：%q", base)
		}
	}
}

func TestClientLoopbackAssetPolicy(t *testing.T) {
	loopback := &Client{APIBase: "http://127.0.0.1:1234"}
	if err := loopback.validateAssetURL("http://127.0.0.1:1234/download/AgentSweep.zip"); err != nil {
		t.Fatalf("回环 APIBase 下的回环资产地址应被允许：%v", err)
	}

	production := &Client{APIBase: DefaultAPIBase}
	for _, raw := range []string{
		"http://127.0.0.1:1234/download/AgentSweep.zip",
		"http://169.254.169.254/latest/meta-data/",
	} {
		if err := production.validateAssetURL(raw); err == nil {
			t.Fatalf("正式 APIBase 不应放行 %q", raw)
		}
	}
}
