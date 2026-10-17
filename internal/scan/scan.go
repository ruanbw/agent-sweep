// Package scan 把声明式目录（catalog.Spec）变成一份会话清单。
//
// 这一层只读：绝不写任何文件，连 mtime 都不碰。写操作全部在 clean 包里 ——
// 「扫描」和「删除」分开，是因为扫描会被自动触发（启动时、切换筛选时），
// 任何一次误写都可能删掉用户没选中的会话。
package scan

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// HeadBytes 是抽标题时的首轮读取量。真正要不要多读由 session.ReadMeta 决定。
const HeadBytes = 64 * 1024

// uuidAtEnd 匹配文件名末尾的 uuid。只取末尾：前面可能是时间戳或产品前缀。
var uuidAtEnd = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// AgentResult 一个 Agent 的扫描结果。
type AgentResult struct {
	AgentID    string            `json:"agentId"`
	Name       string            `json:"name"`
	Group      string            `json:"group"`
	Vendor     string            `json:"vendor"`
	Installed  bool              `json:"installed"`
	Home       string            `json:"home"`
	Roots      []string          `json:"roots"`
	Note       string            `json:"note"`
	Custom     bool              `json:"custom"`
	Sessions   []session.Session `json:"sessions"`
	TotalBytes int64             `json:"totalBytes"`
	Err        string            `json:"err,omitempty"`
}

// All 并发扫描全部 Agent。
//
// 一个 Agent 挂掉不能拖垮整次扫描：每个 Agent 的错误收进 Err 带回 UI，
// 其余照常出结果。这是旧版本踩过的坑 —— 一个坏 JSONL 让整个侧栏空掉，
// 用户会以为「什么都没装」。
func All(specs []catalog.Spec) []AgentResult {
	return AllWithProgress(specs, nil)
}

// AllWithProgress 同上，额外汇报进度。
//
// 进度回调在**完成一个 Agent 之后**触发，而不是开始时：扫描 30 个 Agent 时
// 「12/30 正在扫描 Cursor」比「正在扫描」有用得多，而 UI 需要的是已完成数。
func AllWithProgress(specs []catalog.Spec, progress func(done, total int, current string)) []AgentResult {
	results := make([]AgentResult, len(specs))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	total := len(specs)
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec catalog.Spec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = One(spec)
			if progress != nil {
				mu.Lock()
				done++
				current := spec.Name
				n := done
				mu.Unlock()
				progress(n, total, current)
			}
		}(i, spec)
	}
	wg.Wait()
	return results
}

// One 扫描单个 Agent。
func One(spec catalog.Spec) AgentResult {
	result := AgentResult{
		AgentID: spec.ID,
		Name:    spec.Name,
		Group:   string(spec.Group),
		Vendor:  spec.Vendor,
		Note:    spec.Note,
		Custom:  spec.Custom,
		Roots:   spec.RootPaths(nil),
	}
	home := spec.Home(nil)
	if home == "" {
		return result
	}
	result.Installed = true
	result.Home = home

	seen := map[string]bool{}
	// 同一个 sessionId 可能在多个 Store 里出现（Claude Code 的 projects/ 与 sessions/）。
	// 先到先得：Store 的声明顺序就是优先级。
	for _, store := range spec.Stores {
		items, err := scanStore(spec, home, store)
		mergeStore(&result, seen, items, err)
	}

	sort.SliceStable(result.Sessions, func(i, j int) bool {
		return result.Sessions[i].UpdatedAt.After(result.Sessions[j].UpdatedAt)
	})
	return result
}

// mergeStore 把一个 Store 的结果并入 result：先记错误，再收会话。
//
// scanStore 可能**同时**返回已解析出的会话和一个错误 —— scanSQLite 的结尾是
// `return out, rows.Err()`：库被锁、IO 出错或库损坏时，rows 迭代到一半失败，
// 但前面读出来的行仍然有效。早期版本在 err 非空时直接 continue，把这些会话
// 连同结果一起丢掉，UI 就显示「已安装 · 0 条会话」。所以两条路都要走。
//
// 多个 Store 都报错时按声明顺序用 `; ` 拼接，后面的错误不覆盖前面的。
// 去重仍是先到先得：seen 跨 Store 复用，第一个出现的会话（含它的 Bytes）胜出。
func mergeStore(result *AgentResult, seen map[string]bool, items []session.Session, err error) {
	if err != nil {
		if result.Err != "" {
			result.Err += "; " + err.Error()
		} else {
			result.Err = err.Error()
		}
	}
	for _, item := range items {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		result.Sessions = append(result.Sessions, item)
		result.TotalBytes += item.Bytes
	}
}

func scanStore(spec catalog.Spec, home string, store catalog.Store) ([]session.Session, error) {
	switch store.Kind {
	case "jsonl":
		return scanFiles(spec, home, store, true)
	case "json":
		return scanFiles(spec, home, store, false)
	case "dir":
		return scanDirs(spec, home, store)
	case "sqlite":
		return scanSQLite(spec, home, store)
	default:
		return nil, fmt.Errorf("未知的存储形态 %q", store.Kind)
	}
}

// scanFiles 处理「一个文件 = 一条会话」的两种形态（jsonl / json）。
func scanFiles(spec catalog.Spec, home string, store catalog.Store, jsonl bool) ([]session.Session, error) {
	files := fsx.Glob(home, store.Glob)
	items := make([]session.Session, len(files))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.IsDir() {
			continue
		}
		wg.Add(1)
		go func(i int, file string, info os.FileInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			items[i] = buildFileSession(spec, home, store, file, info, jsonl)
		}(i, file, info)
	}
	wg.Wait()

	out := make([]session.Session, 0, len(items))
	for _, item := range items {
		if item.ID != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

func buildFileSession(spec catalog.Spec, home string, store catalog.Store, file string, info os.FileInfo, jsonl bool) session.Session {
	id := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	// 文件名里带 uuid 的（Codex 的 rollout-…-<uuid>、Pi 的 <时间>_<uuid>）取 uuid 段：
	// 前缀是时间戳，会随每次导出变化，不是稳定 id。
	if match := uuidAtEnd.FindStringSubmatch(id); match != nil {
		id = match[1]
	}
	meta := session.ReadMeta(file, jsonl)

	item := session.Session{
		ID:       id,
		UID:      spec.ID + ":" + id,
		AgentID:  spec.ID,
		Title:    meta.Title,
		Project:  meta.Project,
		Bytes:    info.Size(),
		Messages: meta.Messages,
		Snippet:  meta.Snippet,
		Storage:  session.StorageFile,
		Paths:    []string{file},
	}
	if item.Title == "" {
		item.Title = fallbackTitle(id)
	}
	if item.Snippet == "" {
		item.Snippet = item.Project
	}
	item.UpdatedAt = info.ModTime()
	if !meta.Time.IsZero() {
		item.UpdatedAt = meta.Time
	}

	// 旁挂路径：`{id}` 相对会话文件所在目录，`/{id}` 相对 Agent 数据根。
	dir := filepath.Dir(file)
	applyAssoc(home, dir, store.Assoc, id, &item)
	// 会话正在使用时，Agent 会持有这个 lock 文件（flock）。
	// 试一次非阻塞独占锁：拿不到 = 有人在用。这比看 mtime 准得多 ——
	// mtime 只能说明「最近写过」，而一个刚被读过的旧会话 mtime 不会变。
	item.Active = lockHeld(filepath.Join(dir, id+".lock")) || lockHeld(filepath.Join(dir, "session.lock"))
	return item
}

// scanDirs 处理「一个子目录 = 一条会话」。
func scanDirs(spec catalog.Spec, home string, store catalog.Store) ([]session.Session, error) {
	entries := fsx.Glob(home, store.Glob)
	var out []session.Session
	for _, dir := range entries {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		id := filepath.Base(dir)
		if match := uuidAtEnd.FindStringSubmatch(id); match != nil {
			id = match[1]
		}
		meta := session.ReadMetaDir(dir)
		title := meta.Title
		project := meta.Project
		updated := info.ModTime()
		messages := meta.Messages
		if !meta.Time.IsZero() {
			updated = meta.Time
		}
		// 目录结构里没写项目时，从父目录名反推：Claude Code / DSH 都把
		// `/Users/me/proj` 编码成 `--Users-me-proj--` 这种 mangled 名字。
		// 这是有损的（路径里的 `-` 会被还原成 `/`），所以只在没有真实值时用。
		if project == "" {
			project = decodeProjectDir(filepath.Base(filepath.Dir(dir)))
		}

		item := session.Session{
			ID:        id,
			UID:       spec.ID + ":" + id,
			AgentID:   spec.ID,
			Title:     title,
			Project:   project,
			Bytes:     fsx.Size(dir),
			Messages:  messages,
			Storage:   session.StorageDir,
			Paths:     []string{dir},
			UpdatedAt: updated,
		}
		if item.Title == "" {
			item.Title = fallbackTitle(id)
		}
		if item.Snippet == "" {
			item.Snippet = item.Project
		}
		applyAssoc(home, dir, store.Assoc, id, &item)
		item.Active = lockHeld(filepath.Join(dir, "session.lock"))
		out = append(out, item)
	}
	return out, nil
}

// scanSQLite 读表里的会话行。
//
// 用 `SELECT *` 而不是拼列名：各版本的表结构会加减列，硬编码列名会让
// 一个改名直接变成「扫描失败」。读出来再按配置的列名取值，取不到就退回候选列。
func scanSQLite(spec catalog.Spec, home string, store catalog.Store) ([]session.Session, error) {
	dbPath := filepath.Join(home, store.DB)
	if !fsx.Exists(dbPath) {
		return nil, nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.Query(fmt.Sprintf("SELECT * FROM %s", quoteIdent(store.Table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for i, name := range columns {
		index[strings.ToLower(name)] = i
	}

	idIdx := resolveColumn(index, store.IDCol, "id", "session_id", "conversation_id", "thread_id", "uuid")
	titleIdx := resolveColumn(index, store.TitleCol, "title", "name", "summary", "slug", "preview")
	timeIdx := resolveColumn(index, store.TimeCol, "time_updated", "updated_at", "updatedat", "last_modified_time", "last_modified", "mtime", "timestamp", "created_at")
	cwdIdx := resolveColumn(index, store.CWDCol, "directory", "cwd", "project", "workspace", "folder_paths", "path")

	var out []session.Session
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		id := stringValue(pick(values, idIdx))
		if id == "" {
			continue
		}
		item := session.Session{
			ID:      id,
			UID:     spec.ID + ":" + id,
			AgentID: spec.ID,
			Title:   stringValue(pick(values, titleIdx)),
			Project: stringValue(pick(values, cwdIdx)),
			Storage: session.StorageSQLite,
			Bytes:   0, // SQLite 删行不释放空间，真实释放量在 clean 里按文件差算
		}
		raw := pick(values, timeIdx)
		if t, ok := sqliteTime(raw); ok {
			item.UpdatedAt = t
		} else {
			item.UpdatedAt = info.ModTime()
		}
		if item.Title == "" {
			item.Title = fallbackTitle(id)
		}
		item.Snippet = item.Project
		applyAssoc(home, home, store.Assoc, id, &item)
		out = append(out, item)
	}
	return out, rows.Err()
}

// sqliteTime 认现代c 驱动返回的几种时间形态。
func sqliteTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		return v, !v.IsZero()
	case int64:
		return epochTime(v), true
	case float64:
		return epochTime(int64(v)), true
	case []byte:
		return parseAnyTime(string(v))
	case string:
		return parseAnyTime(v)
	}
	return time.Time{}, false
}

func parseAnyTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		return epochTime(n), true
	}
	return time.Time{}, false
}

func epochTime(n int64) time.Time {
	switch {
	case n <= 0:
		return time.Time{}
	case n > 1e17:
		return time.Unix(0, n)
	case n > 1e12:
		return time.UnixMilli(n)
	default:
		return time.Unix(n, 0)
	}
}

func pick(values []any, idx int) any {
	if idx < 0 || idx >= len(values) {
		return nil
	}
	return values[idx]
}

func resolveColumn(index map[string]int, preferred string, candidates ...string) int {
	if preferred != "" {
		if i, ok := index[strings.ToLower(preferred)]; ok {
			return i
		}
	}
	for _, name := range candidates {
		if i, ok := index[strings.ToLower(name)]; ok {
			return i
		}
	}
	return -1
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	}
	return ""
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// lockHeld 判断某个 lock 文件是否正被别的进程持有。
//
// 用 flock 而不是文件修改时间：mtime 只能说明「最近写过」，
// 一个开着但没在写的会话 mtime 是旧的，而一个刚被读过的旧会话 mtime 也不会变。
// 非阻塞独占锁拿不到，就说明有一个活着的进程正持有它。
func lockHeld(path string) bool {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK)
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return false
}

// applyAssoc 把「与会话同名的旁挂产物」挂到这条会话的删除清单上。
//
// `{id}` 相对 base（文件型会话是文件所在目录，目录型是会话目录本身），
// `/{id}` 相对 Agent 数据根。两种作用域都要支持：Claude Code 的子代理目录在
// 项目目录下，文件快照却在数据根下。
func applyAssoc(home, base string, patterns []string, id string, item *session.Session) {
	for _, pattern := range patterns {
		rel := strings.ReplaceAll(pattern, "{id}", id)
		var target string
		if strings.HasPrefix(rel, "/") {
			target = filepath.Join(home, rel[1:])
		} else {
			target = filepath.Join(base, rel)
		}
		if !fsx.Exists(target) {
			continue
		}
		duplicate := false
		for _, existing := range item.Paths {
			if existing == target {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		item.Paths = append(item.Paths, target)
		item.Bytes += fsx.Size(target)
	}
}

// decodeProjectDir 把 `--Users-me-proj--` / `-Users-me-proj` 还原成 `/Users/me/proj`。
// 认不出（不以 `-` 开头）就返回空串，绝不返回一个猜出来的路径。
func decodeProjectDir(name string) string {
	if !strings.HasPrefix(name, "-") {
		return ""
	}
	trimmed := strings.Trim(name, "-")
	if trimmed == "" {
		return ""
	}
	return "/" + strings.ReplaceAll(trimmed, "-", "/")
}

// fallbackTitle 在没有标题时给一个能认出来的名字。
func fallbackTitle(id string) string {
	if len(id) > 8 {
		return "会话 " + id[:8]
	}
	return "会话 " + id
}
