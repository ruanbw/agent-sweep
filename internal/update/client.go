// Package update 负责「检查 GitHub 上有没新版本 → 下载 → 校验 → 替换自己的 .app」。
//
// 为什么不用 Sparkle：Sparkle 需要 EdDSA 私钥签名与一套 appcast，对一个单二进制、
// 单维护者的项目来说，运维成本比它挡住的风险更高。这里的信任链只解决「传输损坏」与
// 「发布包和标签对不上」：HTTPS 取发布元数据 → 下载发布附件 → 用同一发布里的
// checksums.txt 核对 SHA-256 → 只接受根目录是单个 .app、且 Info.plist 版本与发布
// 标签一致的包。
//
// 它**不**证明发布本身没被替换：checksums.txt 和附件来自同一个发布，仓库被攻破时
// 两者可以一起改。真正的发布授权边界是 GitHub 账号与仓库权限，不是这个文件。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// Repo 是发布所在的公开仓库。用环境变量可以指向 fork，测试与自托管都方便。
	Repo = "ruanbw/agent-sweep"
	// DefaultAPIBase 是 GitHub 的公开 API。
	DefaultAPIBase = "https://api.github.com"
)

// maxDownloadBytes 是单个更新附件允许写入磁盘的最大字节数（512 MiB）。
//
// 为什么必须有这个数：ContentLength 和 asset.Size 都是远端自述的，一个恶意或
// 异常的更新源可以声明「还有几十 GB」再慢慢喂，而旧实现只在写完之后才比对长度，
// 磁盘早被填满了。正常 .app 发布包约 30–80 MB，512 MiB 留出六倍以上余量，
// 既不会误伤未来的体积增长，又把「先写满磁盘再报不完整」的窗口压到有限大小。
const maxDownloadBytes = 512 << 20

// Asset 是发布附件。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// Release 是一次发布里我们需要的全部信息。
type Release struct {
	Tag         string
	Version     string
	Name        string
	Notes       string
	PageURL     string
	PublishedAt time.Time
	Assets      []Asset
}

// ErrNoRelease 表示仓库还没有任何正式发布。
var ErrNoRelease = errors.New("还没有发布任何版本（或仓库已改名、转为私有）")

// Client 是 GitHub Releases 的最小客户端。
//
// 所有字段都可以在测试里替换：APIBase 指向 httptest，GOOS / GOARCH 用来挑附件。
type Client struct {
	HTTP      *http.Client
	APIBase   string
	Repo      string
	GOOS      string
	GOARCH    string
	UserAgent string
}

// NewClient 按环境变量与当前版本构造默认客户端。
//
// AGENTSWEEP_UPDATE_API_BASE / AGENTSWEEP_UPDATE_REPO 是给测试与本地调试用的。
// 它不能把正式构建变成「从一个任意地址明文拉包」的通道：只接受 https，或回环地址上的
// http（httptest 用），否则整组覆盖都被忽略、回落到官方 API。自定义仓库必须和自定义
// API 一起出现，避免只改 repo 就把更新源指向别人的仓库。
func NewClient(current string) *Client {
	apiBase := strings.TrimSpace(os.Getenv("AGENTSWEEP_UPDATE_API_BASE"))
	repo := strings.TrimSpace(os.Getenv("AGENTSWEEP_UPDATE_REPO"))
	if apiBase != "" && !allowedAPIBase(apiBase) {
		apiBase = ""
		repo = ""
	}
	if apiBase == "" {
		repo = ""
	}
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if repo == "" {
		repo = Repo
	}
	client := &Client{
		APIBase:   strings.TrimRight(apiBase, "/"),
		Repo:      repo,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		UserAgent: "AgentSweep/" + current,
	}
	// 每一跳都用和附件 URL 相同的 host 白名单校验：GitHub 的下载入口会 302 到
	// objects.githubusercontent.com，HttpClient 默认会安静跟随，必须显式接管。
	client.HTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: client.checkRedirect}
	return client
}

// allowedAPIBase 只放行 https 与回环 http。明文远程地址等于把「谁来投毒更新」交给网络。
func allowedAPIBase(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		return isLoopbackHost(parsed.Hostname())
	default:
		return false
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// allowedAssetHosts 是发布 JSON 里的 URL 唯一允许的 host。
//
// 名单按实际发布链路定的，不是按「GitHub 有哪些域名」：
//   - api.github.com：Latest 的元数据端点，发布 JSON 由它返回；
//   - github.com：browser_download_url 的正规形状，Release 附件的入口；
//   - release-assets.githubusercontent.com：现在 github.com 的附件入口会 302 到这里，
//     安装包的实际字节由它返回（URL 上带短期签名参数）；
//   - objects.githubusercontent.com：较早的附件重定向目标，老发布、缓存或
//     不同地域仍可能返回它；
//   - github-releases.githubusercontent.com：更早的附件域名，留着是为了不把
//     历史版本更新打死。
//
// 刻意不用 *.githubusercontent.com：raw. / gist. / avatars. 同样是 GitHub 的域名，
// 但它们承载的是任意仓库内容和用户头像，不是发布资产，放进来只会扩大攻击面。
var allowedAssetHosts = map[string]struct{}{
	"github.com":                            {},
	"api.github.com":                        {},
	"release-assets.githubusercontent.com":  {},
	"objects.githubusercontent.com":         {},
	"github-releases.githubusercontent.com": {},
}

// allowedAssetHostList 只用于错误文案，让用户一眼看到「拿到了哪个 host、允许哪些」。
const allowedAssetHostList = "github.com、api.github.com、release-assets.githubusercontent.com、objects.githubusercontent.com、github-releases.githubusercontent.com"

// allowedDownloadURL 校验一个从发布 JSON 里拿到的 URL（安装包、checksums.txt）。
//
// 只允许 https + 固定 host：远端 JSON 同时给出包地址和校验和，如果地址本身可以
// 指向任意主机，那么「校验和匹配」只能证明下载到的东西和它自己声明的一致，
// 不能证明来源可信。重定向目标由 checkRedirect 用同一套规则再查一遍。
func allowedDownloadURL(raw string) error {
	return validateUpdateURL(raw, false)
}

// validateUpdateURL 是策略本体。allowLoopback 只在 APIBase 本身是回环地址时打开
// （httptest / 本地调试），见 (*Client).allowLoopbackAssets。
func validateUpdateURL(raw string, allowLoopback bool) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("更新地址为空；只允许 https 的 GitHub 发布域名：%s", allowedAssetHostList)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("更新地址无法解析（%v）；只允许 https 的 GitHub 发布域名：%s", err, allowedAssetHostList)
	}
	host := strings.ToLower(parsed.Hostname())
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(allowLoopback && scheme == "http" && isLoopbackHost(host)) {
		return fmt.Errorf("更新地址的协议是 %q（host %q），不是 https；只允许 https 的 GitHub 发布域名：%s", scheme, host, allowedAssetHostList)
	}
	if parsed.User != nil {
		return fmt.Errorf("更新地址的 host %q 带有用户信息，拒绝；只允许 https 的 GitHub 发布域名：%s", host, allowedAssetHostList)
	}
	if host == "" {
		return fmt.Errorf("更新地址没有 host；只允许 https 的 GitHub 发布域名：%s", allowedAssetHostList)
	}
	if _, ok := allowedAssetHosts[host]; !ok {
		if allowLoopback && isLoopbackHost(host) {
			return nil
		}
		return fmt.Errorf("更新地址的 host 是 %q，不在允许名单内；只允许：%s", host, allowedAssetHostList)
	}
	// 固定 host 上再放开任意端口没有意义，只会给「同一主机上的别的服务」开门。
	if port := parsed.Port(); port != "" && port != "443" {
		return fmt.Errorf("更新地址的 host %q 使用了端口 %s，只允许 443", host, port)
	}
	return nil
}

// checkRedirect 是默认（不放开回环）的重定向守卫，拆成包级函数便于单测。
func checkRedirect(req *http.Request, via []*http.Request) error {
	return checkRedirectWith(req, via, false)
}

func checkRedirectWith(req *http.Request, via []*http.Request, allowLoopback bool) error {
	if req == nil || req.URL == nil {
		return errors.New("更新下载的重定向缺少目标地址，已拒绝")
	}
	if len(via) >= 10 {
		return fmt.Errorf("更新下载重定向超过 10 次，已停止（最后一跳 host %q）", req.URL.Hostname())
	}
	if err := validateUpdateURL(req.URL.String(), allowLoopback); err != nil {
		source := "未知来源"
		if len(via) > 0 && via[len(via)-1].URL != nil {
			source = via[len(via)-1].URL.Hostname()
		}
		return fmt.Errorf("更新下载被重定向到不允许的地址（从 %q 跳到 %q）：%w", source, req.URL.Hostname(), err)
	}
	return nil
}

// allowLoopbackAssets 只有把 APIBase 指到回环地址时才放行回环 http 资产。
//
// 这正是 AGENTSWEEP_UPDATE_API_BASE 的测试/本地调试路径：元数据、安装包、
// 校验文件全部来自本机 httptest。正式构建的 APIBase 是 api.github.com，
// 这里返回 false，远端 JSON 里塞 127.0.0.1 不会变成 SSRF 通道。
func (c *Client) allowLoopbackAssets() bool {
	parsed, err := url.Parse(c.apiBase())
	if err != nil || !isLoopbackHost(parsed.Hostname()) {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func (c *Client) validateAssetURL(raw string) error {
	return validateUpdateURL(raw, c.allowLoopbackAssets())
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	return checkRedirectWith(req, via, c.allowLoopbackAssets())
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// downloadClient 复用同一个 Transport，但去掉整体超时：大文件由 context 限时。
//
// 重定向守卫必须在这里重新挂一次，而不是只依赖调用方注入的 HTTP：注入的 client
// 可能自带一个宽松的 CheckRedirect，不能让它绕过附件 host 白名单。
func (c *Client) downloadClient() *http.Client {
	base := c.httpClient()
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{Transport: transport, CheckRedirect: c.checkRedirect}
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return Repo
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "AgentSweep"
}

// Latest 取最新正式发布。预发布（draft / prerelease）不会被返回，这是 GitHub 的语义。
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", c.apiBase(), c.repo())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", c.userAgent())

	response, err := c.httpClient().Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("检查更新超时：%w", ctx.Err())
		}
		return nil, fmt.Errorf("连不上 GitHub：%w", err)
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNoRelease
	case http.StatusForbidden, http.StatusTooManyRequests:
		hint := ""
		if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
			hint = "（建议 " + retryAfter + " 秒后再试）"
		}
		return nil, fmt.Errorf("GitHub 拒绝了这次请求，可能触发限流%s", hint)
	default:
		return nil, fmt.Errorf("GitHub 返回 %s", response.Status)
	}

	var payload struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		} `json:"assets"`
	}
	// 发布说明可能很长，但 4 MB 足够；更大的响应按坏数据处理，避免被拖死。
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("发布信息不是合法 JSON：%w", err)
	}
	if payload.TagName == "" {
		return nil, errors.New("发布缺少 tag_name")
	}

	release := &Release{
		Tag:         payload.TagName,
		Version:     normalizeVersion(payload.TagName),
		Name:        payload.Name,
		Notes:       strings.TrimSpace(payload.Body),
		PageURL:     payload.HTMLURL,
		PublishedAt: payload.PublishedAt,
	}
	if release.PageURL == "" {
		release.PageURL = fmt.Sprintf("https://github.com/%s/releases/tag/%s", c.repo(), release.Tag)
	}
	for _, asset := range payload.Assets {
		release.Assets = append(release.Assets, Asset{Name: asset.Name, URL: asset.BrowserDownloadURL, Size: asset.Size})
	}
	return release, nil
}

// normalizeVersion 把 v0.1.0 变成 0.1.0，与二进制里烧进去的版本号对齐。
func normalizeVersion(tag string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(tag), "v"), "V")
}

// AssetFor 从发布附件里挑出本机该下载的包。
//
// 优先 universal（一个包覆盖两种 CPU），其次是本机架构。名字里必须同时出现
// agentsweep 和 darwin：发布里还挂着 CLI 的 tar.gz 和校验文件，不能误选。
func (c *Client) AssetFor(release *Release) (Asset, error) {
	if release == nil {
		return Asset{}, errors.New("没有可用的发布")
	}
	goos := c.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := c.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if goos != "darwin" {
		return Asset{}, fmt.Errorf("当前平台 %s 还没有自动更新包", goos)
	}

	var best Asset
	bestScore := 0
	for _, asset := range release.Assets {
		// 附件名来自远端，不能带路径分隔符或 ..：它会被拼进本地暂存目录。
		if asset.Name != filepath.Base(asset.Name) || strings.ContainsAny(asset.Name, `/\`) || strings.Contains(asset.Name, "..") {
			continue
		}
		lower := strings.ToLower(asset.Name)
		if !strings.Contains(lower, "agentsweep") || !strings.Contains(lower, "darwin") || !strings.HasSuffix(lower, ".zip") {
			continue
		}
		score := 0
		switch {
		case strings.Contains(lower, "universal"):
			score = 3
		case strings.Contains(lower, goarch):
			score = 2
		case goarch == "amd64" && (strings.Contains(lower, "intel") || strings.Contains(lower, "x86_64")):
			score = 2
		case goarch == "arm64" && strings.Contains(lower, "apple-silicon"):
			score = 2
		}
		if score > bestScore {
			best, bestScore = asset, score
		}
	}
	if bestScore == 0 {
		return Asset{}, fmt.Errorf("发布里没有适用于 %s 的安装包", goarch)
	}
	return best, nil
}

// Checksums 下载发布里的 checksums.txt 并解析成「文件名 → SHA-256」。
func (c *Client) Checksums(ctx context.Context, release *Release) (map[string]string, error) {
	var checksumAsset *Asset
	for index := range release.Assets {
		if strings.EqualFold(release.Assets[index].Name, "checksums.txt") {
			checksumAsset = &release.Assets[index]
			break
		}
	}
	if checksumAsset == nil {
		return nil, errors.New("发布里缺少 checksums.txt，拒绝安装未校验的包")
	}
	data, err := c.fetch(ctx, checksumAsset.URL, 2<<20)
	if err != nil {
		return nil, fmt.Errorf("下载校验文件失败：%w", err)
	}

	sums := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[1], "*")
		if len(hash) != 64 || !isHex(hash) || name == "" {
			continue
		}
		sums[name] = hash
	}
	if len(sums) == 0 {
		return nil, errors.New("checksums.txt 里没有可用的条目")
	}
	return sums, nil
}

func (c *Client) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	if err := c.validateAssetURL(url); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", c.userAgent())
	response, err := c.downloadClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败：%s", url, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

func isHex(text string) bool {
	for index := 0; index < len(text); index++ {
		char := text[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// Download 把附件下载到 dest，边下边回报进度。
//
// 先写 .part 再改名：中途断网留下半个文件，下次不会把它当完整的包用。
//
// 这里刻意不用 Client.HTTP 的 30 秒总超时：发布包有十几 MB，慢链路上下不完是常态，
// 而「下到一半被整体超时掐断」对用户毫无解释力。安装调用方给的是一个 15 分钟的
// context，真正的取消边界交给它。
func (c *Client) Download(ctx context.Context, asset Asset, dest string, progress func(done, total int64)) error {
	return c.downloadLimited(ctx, asset, dest, progress, maxDownloadBytes)
}

// downloadLimited 是 Download 的实现；limit 是本次允许落盘的最大字节数。
//
// 把 limit 做成参数而不是直接读常量，是为了能用很小的上限覆盖「超限中止」这条
// 分支：真去流 512 MiB 才能测到它的话，测试既慢又平白占用磁盘。
func (c *Client) downloadLimited(ctx context.Context, asset Asset, dest string, progress func(done, total int64), limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("下载上限必须是正数（实际 %d 字节）", limit)
	}
	if asset.Name != filepath.Base(asset.Name) || strings.ContainsAny(asset.Name, `/\`) {
		return fmt.Errorf("附件名不合法：%q", asset.Name)
	}
	// 安装包地址来自远端 JSON，必须先过 host 白名单；重定向由 downloadClient
	// 的 CheckRedirect 逐跳复查。
	if err := c.validateAssetURL(asset.URL); err != nil {
		return fmt.Errorf("安装包地址不可信：%w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", c.userAgent())
	response, err := c.downloadClient().Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("下载超时：%w", ctx.Err())
		}
		return fmt.Errorf("下载失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 %s", response.Status)
	}
	// 远端自述的长度先查一遍：声明就超限时既不建 .part，也不读 body。
	// ContentLength 为 -1（chunked / 关闭连接定界）时跳过，交给下面的实读计数。
	if response.ContentLength > limit {
		return fmt.Errorf("下载未开始即中止：%s 声明 %d 字节（%s），超过单次下载上限 %d 字节（%s）",
			asset.Name, response.ContentLength, describeBytes(response.ContentLength), limit, describeBytes(limit))
	}

	total := response.ContentLength
	if total <= 0 {
		total = asset.Size
	}
	partial := dest + ".part"
	file, err := os.Create(partial)
	if err != nil {
		return err
	}
	writer := &progressWriter{writer: file, total: total, report: progress}
	_, copyErr := copyDownloadBody(writer, response.Body, limit)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(partial)
		if ctx.Err() != nil {
			return fmt.Errorf("下载中断：%w", ctx.Err())
		}
		if errors.Is(copyErr, errDownloadTooLarge) {
			return fmt.Errorf("%s 下载中止：%w", asset.Name, copyErr)
		}
		return fmt.Errorf("下载中断：%w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(partial)
		return closeErr
	}
	if total > 0 && writer.done != total {
		_ = os.Remove(partial)
		return fmt.Errorf("下载不完整：期望 %d 字节，实际 %d", total, writer.done)
	}
	if progress != nil {
		progress(writer.done, writer.total)
	}
	return os.Rename(partial, dest)
}

// errDownloadTooLarge 标记「响应体超过本次允许的字节数」，调用方据此给出大小相关的错误。
var errDownloadTooLarge = errors.New("响应体超出大小限制")

// copyDownloadBody 把 src 写进 dst，最多写入 limit 字节。
//
// 用 LimitReader 多读 1 字节来判断「还有没有更多」：既不会把超出上限的部分落盘，
// 也不依赖 ContentLength —— 它可以缺失（-1），也可以撒谎。返回的 written 是实际
// 写入 dst 的字节数；超限时它恰好是 limit+1。
func copyDownloadBody(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("下载上限必须是正数（实际 %d 字节）", limit)
	}
	written, err := io.CopyBuffer(dst, io.LimitReader(src, limit+1), make([]byte, 256<<10))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, fmt.Errorf("%w：上限 %d 字节（%s），已收到至少 %d 字节后中止",
			errDownloadTooLarge, limit, describeBytes(limit), written)
	}
	return written, nil
}

// describeBytes 给错误文案补一个人看的单位；整数单位只在整除时才换算，
// 避免「0.98 MiB」这种既是近似值又不好核对的说法。
func describeBytes(size int64) string {
	switch {
	case size <= 0:
		return "0 字节"
	case size%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", size>>20)
	case size%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", size>>10)
	default:
		return fmt.Sprintf("%d 字节", size)
	}
}

// progressWriter 每 100ms 汇报一次，避免每个 256KB 块都触发一次 UI 更新。
type progressWriter struct {
	writer io.Writer
	done   int64
	total  int64
	report func(done, total int64)
	last   time.Time
}

func (p *progressWriter) Write(data []byte) (int, error) {
	written, err := p.writer.Write(data)
	p.done += int64(written)
	if p.report != nil && (p.last.IsZero() || time.Since(p.last) >= 100*time.Millisecond) {
		p.report(p.done, p.total)
		p.last = time.Now()
	}
	return written, err
}

// SHA256File 是 checksums.txt 核对用的摘要。
func SHA256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
