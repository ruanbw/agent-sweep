package gui

// 图标解析：内置目录里的 Agent 大多数没有安装 .app（Claude Code、Codex 都是
// 纯命令行），这些由前端内置的品牌矢量图兜底。这里负责另一类 —— 用户从
// 「发现」页收录的 Agent，以及装了桌面版的 Cursor / Zed / Trae 之类：
// 直接从 macOS 的 .app bundle 里取官方图标，缓存成 PNG 由 /api/icon 提供。
//
// 为什么不在构建时把几十个图标打进二进制：App 图标会随版本更新，而且我们
// 无法为「未来才出现的 Agent」预先准备图标。运行时取系统里已有的那份，
// 永远和用户桌面上的图标一致。
//
// 缓存带源文件 mtime/size 元数据：App 升级换图标后会自动重新生成，而不是
// 永远显示第一版。缓存文件本身也做 PNG 解码校验，写坏了会自愈。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

const (
	iconPixels       = 128
	iconCommandLimit = 5 * time.Second
	iconMissTTL      = 10 * time.Minute
)

type iconStore struct {
	dir string

	mu       sync.Mutex
	ready    map[string]string
	misses   map[string]time.Time
	inflight map[string]*iconCall
}

type iconCall struct {
	done chan struct{}
	path string
	ok   bool
}

type iconMeta struct {
	Source string `json:"source"`
	MTime  int64  `json:"mtime"`
	Size   int64  `json:"size"`
}

func newIconStore(dir string) *iconStore {
	return &iconStore{
		dir:      dir,
		ready:    map[string]string{},
		misses:   map[string]time.Time{},
		inflight: map[string]*iconCall{},
	}
}

// handleIcon 输出某个 Agent 的官方图标。取不到时 404，前端会退回内置品牌图。
func (s *Service) handleIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	spec, ok := catalog.Find(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.icons == nil {
		http.NotFound(w, r)
		return
	}
	path, ok := s.icons.ensure(spec)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// 源图标变了会重新生成；浏览器缓存一小时足够减少重复请求。
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, path)
}

// ensure 把一个 .app 的图标转成 PNG 放进缓存。
//
// 缓存命中在锁外先判断，只有真正需要生成时才进入临界区；同一个 Agent 的
// 并发请求合并成一次 sips，避免几十个图标请求排队等同一把锁。
func (s *iconStore) ensure(spec catalog.Spec) (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}

	s.mu.Lock()
	if path, ok := s.ready[spec.ID]; ok {
		if isFile(path) {
			s.mu.Unlock()
			return path, true
		}
		delete(s.ready, spec.ID)
	}
	if until, ok := s.misses[spec.ID]; ok && time.Now().Before(until) {
		s.mu.Unlock()
		return "", false
	}
	if call, ok := s.inflight[spec.ID]; ok {
		s.mu.Unlock()
		<-call.done
		return call.path, call.ok
	}
	call := &iconCall{done: make(chan struct{})}
	s.inflight[spec.ID] = call
	s.mu.Unlock()

	path, ok := s.build(spec)

	s.mu.Lock()
	delete(s.inflight, spec.ID)
	if ok {
		s.ready[spec.ID] = path
	} else {
		s.misses[spec.ID] = time.Now().Add(iconMissTTL)
	}
	call.path, call.ok = path, ok
	close(call.done)
	s.mu.Unlock()
	return path, ok
}

// build 在锁外完成所有扫盘与子进程调用。
func (s *iconStore) build(spec catalog.Spec) (string, bool) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", false
	}
	dst := filepath.Join(s.dir, iconCacheName(spec.ID))

	// 缓存命中：PNG 可解码 + 源文件 mtime/size 没变。
	if meta, ok := readIconMeta(dst); ok && validPNG(dst) {
		if mtime, size, ok := statSource(meta.Source); ok && mtime == meta.MTime && size == meta.Size {
			return dst, true
		}
	}

	bundle, ok := findAppBundle(spec)
	if !ok {
		return "", false
	}
	src, ok := appIconFile(bundle)
	if !ok {
		return "", false
	}
	mtime, size, ok := statSource(src)
	if !ok {
		return "", false
	}

	tmp, err := os.CreateTemp(s.dir, ".icon-*.png")
	if err != nil {
		return "", false
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(context.Background(), iconCommandLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sips", "-s", "format", "png", "-Z", "128", src, "--out", tmpPath)
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return "", false
	}
	if !validPNG(tmpPath) {
		return "", false
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", false
	}
	writeIconMeta(dst, iconMeta{Source: src, MTime: mtime, Size: size})
	return dst, true
}

func iconCacheName(id string) string {
	sum := sha1.Sum([]byte(id))
	return hex.EncodeToString(sum[:8]) + ".png"
}

func iconMetaPath(cache string) string { return cache + ".json" }

func readIconMeta(cache string) (iconMeta, bool) {
	var meta iconMeta
	data, err := os.ReadFile(iconMetaPath(cache))
	if err != nil {
		return meta, false
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return iconMeta{}, false
	}
	return meta, meta.Source != ""
}

func writeIconMeta(cache string, meta iconMeta) {
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	_ = os.WriteFile(iconMetaPath(cache), data, 0o644)
}

func statSource(path string) (int64, int64, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0, 0, false
	}
	return info.ModTime().UnixNano(), info.Size(), true
}

func validPNG(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	_, err = png.DecodeConfig(file)
	return err == nil
}

// findAppBundle 按 bundle 名在常见位置找 .app。
//
// 内置项只认声明里的 Bundles —— 它们最准确，也避免「Antigravity IDE」因为
// 去掉 IDE 后缀而误用 Antigravity 的图标。用户收录的项没有 Bundles，才从
// 数据根、显示名和厂商名反推。
func findAppBundle(spec catalog.Spec) (string, bool) {
	names := map[string]bool{}
	addName := func(raw string) {
		name := strings.TrimSpace(raw)
		if name == "" {
			return
		}
		name = sanitizeBundleName(name)
		if name == "" {
			return
		}
		names[strings.ToLower(name)] = true
	}
	for _, bundle := range spec.Bundles {
		addName(bundle)
	}
	if len(names) == 0 && spec.Custom {
		for _, candidate := range inferredBundleNames(spec) {
			addName(candidate)
		}
	}
	if len(names) == 0 {
		return "", false
	}

	home, _ := os.UserHomeDir()
	searchDirs := []string{
		"/Applications",
		filepath.Join(home, "Applications"),
		"/Applications/Utilities",
		"/System/Applications",
		"/System/Applications/Utilities",
	}
	for _, dir := range searchDirs {
		if path, ok := findBundleInDir(dir, names); ok {
			return path, true
		}
	}
	return "", false
}

// findBundleInDir 在最多两层的子目录里找一个 .app，兼容厂商目录与 Setapp。
func findBundleInDir(root string, names map[string]bool) (string, bool) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", false
	}
	var found string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return filepath.SkipDir
		}
		depth := strings.Count(rel, string(filepath.Separator))
		if depth > 2 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".app") {
			if names[strings.ToLower(entry.Name())] {
				found = path
				return filepath.SkipAll
			}
			// 不进入 .app 内部。
			return filepath.SkipDir
		}
		return nil
	})
	if found != "" {
		return found, true
	}
	return "", false
}

func sanitizeBundleName(raw string) string {
	name := strings.TrimSpace(raw)
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(name), ".app") {
		name += ".app"
	}
	if strings.TrimSuffix(strings.ToLower(name), ".app") == "" {
		return ""
	}
	return name
}

// inferredBundleNames 只用于用户收录的 Agent。返回的候选按可信度排序。
func inferredBundleNames(spec catalog.Spec) []string {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		name := sanitizeBundleName(raw)
		if name == "" || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}

	for _, variant := range nameVariants(spec.Name) {
		add(variant)
	}
	for _, root := range spec.RootPaths(nil) {
		for _, part := range pathPartsForBundleName(root) {
			add(part)
		}
	}
	for _, variant := range nameVariants(spec.Vendor) {
		add(variant)
	}
	return out
}

// pathPartsForBundleName 从数据根里取「像应用名」的目录段。跳过 home 本身
// 和 Library / Application Support 这类通用段，避免生成 ruanbw.app 这种候选。
func pathPartsForBundleName(root string) []string {
	home, _ := os.UserHomeDir()
	clean := filepath.Clean(root)
	if home != "" && clean == home {
		return nil
	}
	if home != "" && strings.HasPrefix(clean, home+string(filepath.Separator)) {
		clean = strings.TrimPrefix(clean, home+string(filepath.Separator))
	}
	parts := strings.Split(clean, string(filepath.Separator))
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || isGenericPathPart(part) {
			continue
		}
		out = append(out, part)
	}
	return out
}

// nameVariants 把「Warp Agent」「Claude Code」变成更可能命中 .app 的名字。
// 只去掉明确的产品后缀；不要为了多命中就把每个词都拆开。
func nameVariants(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	out := []string{name}
	for _, suffix := range []string{
		" Agent", " CLI", " AI", " IDE", " Chat", " Nightly",
		" Insiders", " Next", " CN", " Preview",
	} {
		if trimmed := strings.TrimSuffix(name, suffix); trimmed != name && trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func isGenericPathPart(part string) bool {
	switch strings.ToLower(part) {
	case "library", "application support", "applications", "user", "users",
		"globalstorage", "workspacestorage", "sessions", "session", "threads",
		"projects", "chats", "data", "storage", "state", "tmp", "cache",
		"caches", "vscode", "code", "global", "agent", "agent-sessions",
		"local", "share", "config", "home", "default-workspace":
		return true
	}
	return false
}

// appIconFile 找到 bundle 里真正可以交给 sips 的 .icns。
func appIconFile(bundle string) (string, bool) {
	resources := filepath.Join(bundle, "Contents", "Resources")
	info := filepath.Join(bundle, "Contents", "Info.plist")

	for _, key := range []string{"CFBundleIconFile", "CFBundleIconName"} {
		value := plistString(info, key)
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(value), ".icns") {
			value += ".icns"
		}
		candidate := filepath.Join(resources, value)
		if isFile(candidate) {
			return candidate, true
		}
	}

	entries, err := os.ReadDir(resources)
	if err != nil {
		return "", false
	}
	var icons []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".icns") {
			continue
		}
		icons = append(icons, entry.Name())
	}
	if len(icons) == 0 {
		return "", false
	}
	sort.SliceStable(icons, func(i, j int) bool { return icons[i] < icons[j] })
	for _, name := range icons {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "appicon") || strings.Contains(lower, "application") {
			return filepath.Join(resources, name), true
		}
	}
	return filepath.Join(resources, icons[0]), true
}

func plistString(info, key string) string {
	if !isFile(info) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), iconCommandLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, "plutil", "-extract", key, "raw", "-o", "-", info).Output()
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
