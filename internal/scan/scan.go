// Package scan 把声明式目录（catalog.Spec）变成一份会话清单。
//
// 这一层只读：绝不写任何文件，连 mtime 都不碰。写操作全部在 clean 包里 ——
// 「扫描」和「删除」分开，是因为扫描会被自动触发（启动时、切换筛选时），
// 任何一次误写都可能删掉用户没选中的会话。
package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

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

// scanNotice 是一条「扫描照常完成，但用户该知道」的提示。
//
// 不直接写 AgentResult.Err 是因为文件型 Store 在 worker 池里并发解析：
// 并发写同一个结果需要额外加锁，而这里只要各批解析自己收集、回到 One 再按
// key 去重，既没有数据竞争，也不会让同一条提示按会话数重复刷屏。
type scanNotice struct {
	key  string
	text string
}

// addNotice 按 key 去重后追加。
//
// 同一 key 只保留第一次出现的正文（含最具体的路径示例），所以一万条
// 越界会话也只会在 Err 里留下一行。
func addNotice(dst *[]scanNotice, notice scanNotice) {
	if notice.key == "" {
		notice.key = notice.text
	}
	for _, existing := range *dst {
		if existing.key == notice.key {
			return
		}
	}
	*dst = append(*dst, notice)
}

// scanNoticeError 把「非致命提示」和「真正的错误」一起交回调用方。
//
// scanFiles / scanDirs / scanSQLite 的签名被既有测试锁定为 (items, error)，
// 提示又必须离开 worker 池才能汇总，所以用这个 error 当载体：One 用
// errors.As 取出 notices 后只把 err 交给 mergeStore，Err 的拼接语义不变。
type scanNoticeError struct {
	notices []scanNotice
	err     error
}

func (e *scanNoticeError) Error() string {
	parts := make([]string, 0, len(e.notices)+1)
	if e.err != nil {
		parts = append(parts, e.err.Error())
	}
	for _, notice := range e.notices {
		parts = append(parts, notice.text)
	}
	return strings.Join(parts, "; ")
}

func (e *scanNoticeError) Unwrap() error { return e.err }

func wrapScanNotices(notices []scanNotice, err error) error {
	if len(notices) == 0 {
		return err
	}
	return &scanNoticeError{notices: notices, err: err}
}

// appendScanNotices 把去重后的提示并进 AgentResult.Err。
//
// 统一加「警告：」前缀，UI 才能把「扫描成功但有不确定的地方」与真正的
// 失败区分开；提示之间用「；」连接，不让 Err 长成一串分号。
func appendScanNotices(result *AgentResult, notices []scanNotice) {
	if len(notices) == 0 {
		return
	}
	texts := make([]string, 0, len(notices))
	for _, notice := range notices {
		texts = append(texts, notice.text)
	}
	warning := "警告：" + strings.Join(texts, "；")
	if result.Err != "" {
		result.Err += "; " + warning
	} else {
		result.Err = warning
	}
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
				// 回调在持锁期间调用：只保证 done 递增还不够 —— 解锁后再调
				// 回调，多个 goroutine 之间会出现「先到 3/30、再到 2/30」的
				// 倒序。这里串行化的是 UI 侧的短操作（每完成一个 Agent 才调
				// 一次，回调本身只更新进度字段），牺牲的并发可以忽略，换来
				// 「n 永远不倒退」的接口保证。
				//
				// 回调里不要再拿这把 mu，否则自锁。
				mu.Lock()
				done++
				n, current := done, spec.Name
				progress(n, total, current)
				mu.Unlock()
			}
		}(i, spec)
	}
	wg.Wait()
	return results
}

// scanStoreFn 是 One 调用 scanStore 的间接层，只给测试替换用（故意不导出）。
//
// 测试需要构造「某个 Agent 的 Store 解析 panic」与「完成顺序可控」两种场景，
// 而真实的 scanStore 没法稳定制造 panic。生产路径永远指向 scanStore。
var scanStoreFn = scanStore

// One 扫描单个 Agent。
//
// 返回值必须命名：defer 里的 recover 要能改到即将返回的 result。扫描是只读
// 操作，但解析畸形文件会走到解析器与驱动深处，一个没预料到的 panic 不该让
// 整个进程退出 —— CLI `agentsweep scan` 直接调 scan.All，没有 GUI 那层兜底。
func One(spec catalog.Spec) (result AgentResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// 已经扫到的 Sessions 保留：用户至少能看到部分结果，Err 负责说明
			// 这份结果不完整。panic 到这里为止，不再向上抛。
			message := fmt.Sprintf("扫描 %s 时发生 panic：%v", spec.Name, recovered)
			if result.Err != "" {
				result.Err += "; " + message
			} else {
				result.Err = message
			}
		}
	}()
	result = AgentResult{
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
	// 数据根不可信时不要遍历：Home 直接采信 Env 的值，误配一个
	// CLAUDE_CONFIG_DIR=/ 就会让扫描去走整棵树，而且结果里的「已安装」
	// 没有任何意义。Installed 保持 false，只把原因挂到 Err 上。
	if err := catalog.ValidateDataRoot(home); err != nil {
		result.Err = fmt.Sprintf("拒绝扫描不可信的数据根：%v", err)
		return result
	}
	result.Installed = true
	result.Home = home

	seen := map[string]bool{}
	// 提示（越界的 assoc、静默的列回退）先各 Store 收集，最后统一去重并入 Err。
	// 不能由并发的文件解析直接写 result.Err：那既会竞争，也会让同一条提示
	// 按会话数重复一遍。
	var notices []scanNotice
	// 同一个 sessionId 可能在多个 Store 里出现（Claude Code 的 projects/ 与 sessions/）。
	// 先到先得：Store 的声明顺序就是优先级。
	for _, store := range spec.Stores {
		items, err := scanStoreFn(spec, home, store)
		var noticeErr *scanNoticeError
		if errors.As(err, &noticeErr) {
			for _, notice := range noticeErr.notices {
				addNotice(&notices, notice)
			}
			// mergeStore 只该看到真正的错误，既有拼接语义保持不变。
			err = noticeErr.err
		}
		mergeStore(&result, seen, items, err)
	}
	appendScanNotices(&result, notices)

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

// scanWorkers 是单个 Store 内同时解析的文件数。
//
// 取 16：文件解析是「读一小段 + 解析 JSON」的 IO 密集型工作，16 路足以把
// 慢盘的空闲时间填满，又不会让 30 个 Agent（AllWithProgress 里 6 路并发）
// 一次性开出几百个线程。注意这里限制的是**在飞的 goroutine 数**，不再只是
// 「同时运行的函数数」：文件型 Store 动辄几万个文件，先 go 再抢信号量会把
// 几万个 goroutine（连同各自的栈）一起压在内存里。
const scanWorkers = 16

// forEachFile 用固定大小的 worker 池遍历 files，fn 拿到的是原始下标。
//
// 池里只创建 min(workers, len(files)) 个 goroutine：任务从 jobs 里取，
// 取完一个才取下一个，所以任意时刻在飞的 fn 至多是 workers 个。下标由
// 调用方用来写 slots[i]，结果顺序因此与 files 完全一致 —— 调用方（以及
// 更上层的排序/过滤）依赖这一点。
//
// fn 对每个文件都会被调用一次，包括调用方随后要 Stat 跳过的那些；
// 「跳过」是 fn 自己的事，池不做二次过滤，避免把任务悄悄吞掉。
func forEachFile(files []string, workers int, fn func(i int, path string)) {
	if len(files) == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i, files[i])
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// statSkipNotice 把「文件 Stat 失败」分类成该静默还是该告诉用户。
//
// glob 与 Stat 之间被删（ErrNotExist）是正常竞态，目录也不是会话文件，两者
// 保持静默。其它错误（EACCES/EPERM、坏符号链接、iCloud 未下载、IO 错）说明
// 文件真实存在却读不到 —— 结果里少了几条会话而没有任何解释，用户只能以为
// 「本来就没有」。同一类原因聚合一条，正文带上首例路径与原始错误。
func statSkipNotice(path string, err error) (scanNotice, bool) {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return scanNotice{}, false
	}
	kind := "io"
	if errors.Is(err, fs.ErrPermission) {
		kind = "permission"
	}
	return scanNotice{
		key:  "stat:" + kind,
		text: fmt.Sprintf("有文件因权限/IO 无法读取（首例：%s）：%v", path, err),
	}, true
}

// scanFiles 处理「一个文件 = 一条会话」的两种形态（jsonl / json）。
func scanFiles(spec catalog.Spec, home string, store catalog.Store, jsonl bool) ([]session.Session, error) {
	files := fsx.Glob(home, store.Glob)
	// 用指针槽位而不是 make([]session.Session, len(files))：后者会为几万个
	// 文件先分配一整片零值结构体，而真正解析成功的只是其中一部分。空槽位
	// 也保留了「按下标写、按下标读」的顺序语义。
	//
	// notices 跟着槽位一起带回来：worker 里不能碰共享的 AgentResult。
	type fileSlot struct {
		item    *session.Session
		notices []scanNotice
	}
	slots := make([]fileSlot, len(files))
	forEachFile(files, scanWorkers, func(i int, file string) {
		info, err := os.Stat(file)
		if err != nil {
			// 目录不是会话文件，Stat 失败也分两类：ENOENT 是 glob 与 Stat
			// 之间的正常竞态；权限/IO 错误意味着文件真实存在却读不到，用户
			// 只会看到「少了几个会话」，必须留一条提示。
			if notice, ok := statSkipNotice(file, err); ok {
				slots[i].notices = append(slots[i].notices, notice)
			}
			return
		}
		if info.IsDir() {
			return
		}
		item, notices := buildFileSessionWithNotices(spec, home, store, file, info, jsonl)
		slots[i] = fileSlot{item: &item, notices: notices}
	})

	out := make([]session.Session, 0, len(slots))
	var notices []scanNotice
	for _, slot := range slots {
		if slot.item != nil && slot.item.ID != "" {
			out = append(out, *slot.item)
		}
		for _, notice := range slot.notices {
			addNotice(&notices, notice)
		}
	}
	return out, wrapScanNotices(notices, nil)
}

func buildFileSession(spec catalog.Spec, home string, store catalog.Store, file string, info os.FileInfo, jsonl bool) session.Session {
	item, _ := buildFileSessionWithNotices(spec, home, store, file, info, jsonl)
	return item
}

func buildFileSessionWithNotices(spec catalog.Spec, home string, store catalog.Store, file string, info os.FileInfo, jsonl bool) (session.Session, []scanNotice) {
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
	var notices []scanNotice
	applyAssoc(home, dir, store.Assoc, id, &item, &notices)
	// 会话正在使用时，Agent 会持有这个 lock 文件（flock）。
	// 试一次非阻塞独占锁：拿不到 = 有人在用。这比看 mtime 准得多 ——
	// mtime 只能说明「最近写过」，而一个刚被读过的旧会话 mtime 不会变。
	// 判定不了（打不开/权限不足）时取保守值 true：宁可多提示一次「使用中」。
	item.Active = activeFromLock(sessionLockState(
		filepath.Join(dir, id+".lock"),
		filepath.Join(dir, "session.lock"),
	))
	return item, notices
}

// scanDirs 处理「一个子目录 = 一条会话」。
//
// 目录元数据的读取和 scanFiles 一样是 IO 密集型：每个目录最多试 7 个候选
// 文件名、每个文件内部还要按需多读几轮。串行做这件事，几千个会话目录就是
// 上万次串行 open/read，扫描时长随目录数线性增长（R24）。这里复用同一个
// forEachFile worker 池（scanWorkers 路）并发读取，再用下标槽位按目录枚举
// 顺序输出 —— One 里的去重依赖「同一 ID 先到先得」，顺序不能变。
//
// 每个 worker 只写自己的槽位，item 和 notices 都不跨 goroutine 共享，
// 所以 applyAssoc 与 notices 收集不需要额外加锁。
func scanDirs(spec catalog.Spec, home string, store catalog.Store) ([]session.Session, error) {
	return scanDirsWith(spec, home, store, buildDirSession)
}

// dirSessionBuilder 把一个已确认是目录的条目转成会话及其提示。
//
// 抽成参数只是为了让测试能确定地观测 scanDirs 的并发派发（见
// dirsparallel_test.go）；生产路径固定使用 buildDirSession。
type dirSessionBuilder func(spec catalog.Spec, home string, store catalog.Store, dir string, info os.FileInfo) (session.Session, []scanNotice)

func scanDirsWith(spec catalog.Spec, home string, store catalog.Store, build dirSessionBuilder) ([]session.Session, error) {
	entries := fsx.Glob(home, store.Glob)
	// 与 scanFiles 相同的指针槽位：只有真正处理成功的目录才占一个结果，
	// 空槽位保留「按下标写、按下标读」的顺序语义。
	type dirSlot struct {
		item    *session.Session
		notices []scanNotice
	}
	slots := make([]dirSlot, len(entries))
	forEachFile(entries, scanWorkers, func(i int, dir string) {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			// 现状语义：Stat 失败（含 glob 与 Stat 之间的删除、坏符号链接）
			// 和普通文件都静默跳过，不中断其它目录。
			return
		}
		item, notices := build(spec, home, store, dir, info)
		slots[i] = dirSlot{item: &item, notices: notices}
	})

	out := make([]session.Session, 0, len(slots))
	var notices []scanNotice
	for _, slot := range slots {
		if slot.item != nil {
			out = append(out, *slot.item)
		}
		for _, notice := range slot.notices {
			addNotice(&notices, notice)
		}
	}
	return out, wrapScanNotices(notices, nil)
}

// buildDirSession 读取单个目录型会话的元数据与旁挂产物。
func buildDirSession(spec catalog.Spec, home string, store catalog.Store, dir string, info os.FileInfo) (session.Session, []scanNotice) {
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
	var notices []scanNotice
	applyAssoc(home, dir, store.Assoc, id, &item, &notices)
	item.Active = activeFromLock(sessionLockState(filepath.Join(dir, "session.lock")))
	return item, notices
}

// scanSQLite 读表里的会话行。
//
// 用 `SELECT *` 而不是拼列名：各版本的表结构会加减列，硬编码列名会让
// 一个改名直接变成「扫描失败」。读出来再按配置的列名取值，取不到就退回候选列。
//
// 全程有**双重上限**，因为调用方是 6 路并发的 AllWithProgress：一个 Agent 的库
// 被自己占着（Agent 常驻时很常见，比如它正开着写事务），没有上限就会让整次扫描
// 永不返回，GUI 的扫描状态卡在 running，而 startScan 的幂等判断又拦住用户重试。
// 第一层是 DSN 里的 busy_timeout（等锁的时长），第二层是这里的 ctx（查询总时长）。
// 超时不算「0 条会话」：带着已读到的行一起返回错误，UI 才能说清是哪个 Agent 超时。
func scanSQLite(spec catalog.Spec, home string, store catalog.Store) ([]session.Session, error) {
	dbPath := filepath.Join(home, store.DB)
	if !fsx.Exists(dbPath) {
		return nil, nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteScanDSN(dbPath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), sqliteQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s", quoteIdent(store.Table)))
	if err != nil {
		return nil, sqliteQueryError(dbPath, err)
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

	idChoice := resolveColumn(index, "idCol", store.IDCol, "id", "session_id", "conversation_id", "thread_id", "uuid")
	titleChoice := resolveColumn(index, "titleCol", store.TitleCol, "title", "name", "summary", "slug", "preview")
	timeChoice := resolveColumn(index, "timeCol", store.TimeCol, "time_updated", "updated_at", "updatedat", "last_modified_time", "last_modified", "mtime", "timestamp", "created_at")
	cwdChoice := resolveColumn(index, "cwdCol", store.CWDCol, "directory", "cwd", "project", "workspace", "folder_paths", "path")

	// R17：列名回退以前是完全静默的。只有真正发生回退才记，显式配置命中时
	// 一条都不产生；按「表 + 角色 + 实际列」去重，同一个 Store 重复扫描也
	// 不会多刷一行。
	var notices []scanNotice
	for _, choice := range []columnChoice{idChoice, titleChoice, timeChoice, cwdChoice} {
		if !choice.fallback {
			continue
		}
		addNotice(&notices, scanNotice{
			key:  "column:" + store.Table + ":" + choice.role + ":" + choice.name + ":" + choice.preferred,
			text: choice.warning(store.Table),
		})
	}

	var out []session.Session
	// 逐行读失败与缺 ID 的行都保留「跳过、不中断整表」的原行为，但必须计数：
	// 否则用户只看到「少了几个会话」，没有任何线索。计数按库聚合，循环结束
	// 后并入 notices。
	scanErrRows := 0
	noIDRows := 0
	var firstScanErr error
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			scanErrRows++
			if firstScanErr == nil {
				firstScanErr = err
			}
			continue
		}
		id := stringValue(pick(values, idChoice.index))
		if id == "" {
			noIDRows++
			continue
		}
		item := session.Session{
			ID:      id,
			UID:     spec.ID + ":" + id,
			AgentID: spec.ID,
			Title:   stringValue(pick(values, titleChoice.index)),
			Project: stringValue(pick(values, cwdChoice.index)),
			Storage: session.StorageSQLite,
			Bytes:   0, // SQLite 删行不释放空间，真实释放量在 clean 里按文件差算
		}
		raw := pick(values, timeChoice.index)
		if t, ok := sqliteTime(raw); ok {
			item.UpdatedAt = t
		} else {
			item.UpdatedAt = info.ModTime()
		}
		if item.Title == "" {
			item.Title = fallbackTitle(id)
		}
		item.Snippet = item.Project
		applyAssoc(home, home, store.Assoc, id, &item, &notices)
		out = append(out, item)
	}
	if scanErrRows > 0 || noIDRows > 0 {
		detail := ""
		if firstScanErr != nil {
			detail = fmt.Sprintf("（首例：%v）", firstScanErr)
		}
		addNotice(&notices, scanNotice{
			key: "sqlite:" + dbPath + ":rows",
			text: fmt.Sprintf("SQLite 表 %s 有 %d 行因列类型无法读取、%d 行缺少可用 ID 被跳过%s",
				store.Table, scanErrRows, noIDRows, detail),
		})
	}
	// 迭代到一半超时/被占时 rows.Err() 才会说话；此时 out 里已有的行照样返回，
	// mergeStore 会把它们收下并把错误记进 AgentResult.Err（见 partial_test.go）。
	return out, wrapScanNotices(notices, sqliteQueryError(dbPath, rows.Err()))
}

// SQLite 扫描的两层时间上限。取值理由见下；两者必须一起看。
const (
	// sqliteBusyTimeout 是「等别的连接让出锁」的上限，写进 DSN。
	//
	// 1.5s 的选择：比 probe 的 500ms 宽 —— 探测只是判断「像不像会话库」，等不到
	// 可以跳过；而扫描是主路径，Agent 正在写盘时多等一会儿，换回来的是完整会话
	// 列表而不是「已安装 · 0 条」。但它必须明显小于 sqliteQueryTimeout：SQLite 的
	// busy 等待是在锁重试里自旋的，驱动的 context 中断只在 VM 指令之间检查，
	// 打断不了这段等待（实测 busy_timeout=10s + ctx=300ms 仍会等满 10s）。
	// 所以真正封顶「被占」时长的是这个值，1.5s 足以覆盖一次正常写事务。
	sqliteBusyTimeout = 1500 * time.Millisecond
	// sqliteQueryTimeout 是一次 SELECT 的总上限（含上面那段等锁时间）。
	//
	// 取 4s：几万行的会话库在慢盘上通常 1s 内读完，4s 留足余量；而 6 路并发的
	// 扫描里最坏也只多花 4s，GUI 的「扫描中」不会长期挂着，用户也不用重启应用
	// 才能再点一次扫描。4s 落在 3–5s 的建议区间，且比 busy_timeout 大得多，
	// 保证等锁失败时报的是「被占用」而不是含混的「超时」。
	sqliteQueryTimeout = 4 * time.Second
)

// sqliteQueryError 给驱动错误补上中文上下文，并把「超时」与「库被占」分开。
//
// 这两种错误在 UI 里的含义不同：超时是这次没读完（重试可能就好），被占是
// Agent 自己正开着库（多半要先退出那个 Agent）。两者都用 %w 包装，调用方仍可
// errors.Is(err, context.DeadlineExceeded) 或 errors.As(err, *sqlite.Error) 判断。
func sqliteQueryError(dbPath string, err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		// 扩展错误码的高位是细分原因（如 SQLITE_BUSY_SNAPSHOT），主码看低 8 位。
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return fmt.Errorf("SQLite 库 %s 被其它进程占用（等待上限 %s）: %w",
				filepath.Base(dbPath), sqliteBusyTimeout, err)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("读取 SQLite 库 %s 超时（上限 %s）: %w",
			filepath.Base(dbPath), sqliteQueryTimeout, err)
	}
	return fmt.Errorf("读取 SQLite 库 %s 失败: %w", filepath.Base(dbPath), err)
}

// sqliteDSN 拼出 `file:<path>?mode=ro&_pragma=query_only(1)`。
//
// 路径只做 EscapedPath（空格 → %20，`?`、`#`、`%` 也一并转义），绝不把裸路径
// 直接拼进 `file:` URI：`?` / `#` 在 macOS 上是完全合法的文件名字符，裸拼会让
// URI 被截断或改变语义 —— 轻则打开到另一个文件，重则直接打不开，而上层会把
// 它记成错误或「0 条会话」。
//
// 写法与 internal/probe 的 sqliteReadOnlyDSN 对齐（同样只转义 path、不用
// url.URL.String() 生成 file:/// 三斜杠形态）；两个包各留一份，不跨包导出。
// 扫描专用的 DSN 在 sqliteScanDSN 里额外追加 busy_timeout，这里保持只读四件套
// 的最小形态（dsn_test.go 逐字节断言了这条输出）。
func sqliteDSN(path string) string {
	escaped := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	return "file:" + escaped + "?mode=ro&_pragma=query_only(1)"
}

// sqliteScanDSN 是扫描真正使用的 DSN：在 sqliteDSN 之后追加 busy_timeout。
//
// 单独一个函数而不是直接改 sqliteDSN，是因为后者被 dsn_test.go 逐字节断言，
// 而 busy_timeout 只在「库正被 Agent 占着」的扫描场景里才有意义。driver 按
// 顺序应用 _pragma 且会把 busy_timeout 排到最前，随后 query_only 落地，
// 只读语义（mode=ro + query_only）一个不丢。
func sqliteScanDSN(path string) string {
	return sqliteDSN(path) + fmt.Sprintf("&_pragma=busy_timeout(%d)", sqliteBusyTimeout/time.Millisecond)
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

// columnChoice 记录一次「从表里挑列」的结果，以及它是显式命中还是回退。
//
// 把实际列名与配置列名一起带出来，是为了让回退可观测：只返回下标的话，
// 绑定到 path / created_at 这类泛化名字时用户完全看不出来（R17）。
type columnChoice struct {
	index     int
	name      string // 实际使用的列名（小写）
	preferred string // 配置里明确写的列名；空表示没配
	role      string // idCol / titleCol / timeCol / cwdCol
	fallback  bool
}

// warning 生成一条用户能读懂的说明。只有 fallback 为真时才该调用。
func (c columnChoice) warning(table string) string {
	if c.preferred == "" {
		return fmt.Sprintf("表 %s 未声明 %s，回退到列 %s", table, c.role, c.name)
	}
	return fmt.Sprintf("表 %s 配置的 %s=%q 不存在，回退到列 %s", table, c.role, c.preferred, c.name)
}

// resolveColumn 优先用配置的列名；配置缺失或列不存在时才退回候选列。
//
// 返回值带上「实际用了哪一列、是否回退」，调用方才能把以前完全静默的回退
// 报出来。candidates 的顺序就是优先级，命中第一个即止。
func resolveColumn(index map[string]int, role, preferred string, candidates ...string) columnChoice {
	if preferred != "" {
		key := strings.ToLower(preferred)
		if i, ok := index[key]; ok {
			return columnChoice{index: i, name: key, preferred: preferred, role: role}
		}
	}
	for _, name := range candidates {
		key := strings.ToLower(name)
		if i, ok := index[key]; ok {
			return columnChoice{index: i, name: key, preferred: preferred, role: role, fallback: true}
		}
	}
	return columnChoice{index: -1, preferred: preferred, role: role}
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

// lockState 是 lock 文件的三态判断结果。
//
// 三态而不是 bool：`打不开` 和 `没人锁` 是两件事，把前者当成后者会让
// 「正在使用」的会话显示成空闲，用户于是勾选并删掉它。
type lockState int

const (
	// lockFree 确定没有活着的进程持有这个 lock 文件。
	lockFree lockState = iota
	// lockBusy 确定有别的进程持有这把锁。
	lockBusy
	// lockUnknown 无法判断（打不开、权限不足、flock 报错）。
	lockUnknown
)

// lockHeld 判断某个 lock 文件是否正被别的进程持有。
//
// 用 flock 而不是文件修改时间：mtime 只能说明「最近写过」，
// 一个开着但没在写的会话 mtime 是旧的，而一个刚被读过的旧会话 mtime 也不会变。
// 非阻塞独占锁拿不到，就说明有一个活着的进程正持有它。
//
// 用 O_RDONLY 而不是 O_RDWR：flock(2) 在 macOS/Linux 上对只读 fd 同样有效，
// 而 O_RDWR 会在只读目录、别人拥有的 lock 文件、或被 chmod 0444 的文件上
// 直接 EACCES —— 那种情况下我们仍然应该能正确判断，而不是误判成空闲。
//
// 语义：
//   - ENOENT（没有 lock 文件）→ lockFree，没有 lock 文件就是没在用；
//   - 拿到 LOCK_EX|LOCK_NB → lockFree，并立刻 LOCK_UN 释放；
//   - EWOULDBLOCK / EAGAIN → lockBusy；
//   - 其它任何错误（打开失败、flock 失败）→ lockUnknown。
func lockHeld(path string) lockState {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return lockFree
		}
		// EACCES / EPERM / EIO / …：不是「没人用」，是「看不出来」。
		return lockUnknown
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return lockBusy
		}
		return lockUnknown
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return lockFree
}

// activeFromLock 把三态 lock 判断映射到 session.Session.Active。
//
// Active 只表达「可能正在被使用」，所以 lockUnknown 取保守值 true：
// 宁可多提示一次「使用中」，也不要让用户误删一个正在用的会话。
func activeFromLock(state lockState) bool {
	return state != lockFree
}

// sessionLockState 合并一个会话可能带的多把 lock 文件
// （文件型会话是 `{id}.lock` 与 `session.lock`，目录型只有 `session.lock`）。
//
// 越「忙」越优先：任意一把 busy → busy；否则任意一把 unknown → unknown；
// 全部 free（含全部不存在）→ free。这条顺序保证 unknown 不会被另一把
// 明确的 free 覆盖成「空闲」——保守方向只增不减。
func sessionLockState(paths ...string) lockState {
	state := lockFree
	for _, path := range paths {
		switch lockHeld(path) {
		case lockBusy:
			return lockBusy
		case lockUnknown:
			state = lockUnknown
		}
	}
	return state
}

// applyAssoc 把「与会话同名的旁挂产物」挂到这条会话的删除清单上。
//
// `{id}` 相对 base（文件型会话是文件所在目录，目录型是会话目录本身），
// `/{id}` 相对 Agent 数据根。两种作用域都要支持：Claude Code 的子代理目录在
// 项目目录下，文件快照却在数据根下。
//
// 解析结果必须严格落在数据根内才收：filepath.Join 会先 Clean 掉 `..`，
// 一个自定义适配器写的 `../../outside` 会直接变成数据根之外的路径。删不删
// 得掉由 clean 的 checkPathInHome 兜底，但扫描结果本身不能先不可信 —— UI
// 会展示它，Bytes 也会把别人的文件算进来（R15）。
//
// notices 是可选收集器：生产调用点都会传入，直接调用（例如测试）可以省略。
func applyAssoc(home, base string, patterns []string, id string, item *session.Session, sinks ...*[]scanNotice) {
	var notices *[]scanNotice
	if len(sinks) > 0 {
		notices = sinks[0]
	}
	for _, pattern := range patterns {
		rel := strings.ReplaceAll(pattern, "{id}", id)
		var target string
		if strings.HasPrefix(rel, "/") {
			target = filepath.Join(home, rel[1:])
		} else {
			target = filepath.Join(base, rel)
		}
		// 必须在 Join 之后判断包含性：原始 pattern 里看不到 Clean 的结果。
		if !assocWithinHome(home, target) {
			if notices != nil {
				addNotice(notices, scanNotice{
					key:  "assoc:" + pattern,
					text: fmt.Sprintf("旁挂模板 %q 解析出的路径越出数据根 %s，已跳过：%s", pattern, home, target),
				})
			}
			continue
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

// assocWithinHome 判断 assoc 解析出的路径是否严格位于数据根 home 内。
//
// 只做词法判断：这里的目标是「不要把越界路径收进扫描结果」，不需要 clean
// 包 checkPathInHome 那层真实路径语义（scan 也不能 import clean，会成环）。
// 符号链接造成的越界仍由删除侧的 checkPathInHomeReal 兜底；扫描侧宁可只
// 拒绝显式 `..`，也不跟随链接去猜。
func assocWithinHome(home, target string) bool {
	if strings.TrimSpace(home) == "" || strings.TrimSpace(target) == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(target))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	// "." 是数据根本身，不是某条会话的旁挂产物；收进删除清单等于把整个
	// Agent 根挂到这条会话上。
	if rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

// shortIDFallback 是 shortID 在输入为空时的安全占位。
//
// 调用方拿 shortID 的返回值拼标题 / 键，空串会让多条会话塌成同一个键，
// 所以这里宁可给一个明确可认的占位，也不返回 ""。
const shortIDFallback = "unknown"

// shortID 返回 id 的前 n 个 rune（不是前 n 个字节）。
//
// 会话 ID 来自文件名，可能含中文、emoji，甚至（在 Unix 上）非法 UTF-8 字节。
// 按字节切 id[:n] 会把一个多字节 rune 劈成两半，产出非法 UTF-8：JSON 序列化
// 时变成替换字符、map key 可能撞车。这里先转成 []rune 再切，转换本身会把
// 非法字节逐个替换成 U+FFFD，因此返回值永远是合法 UTF-8。
//
// n < 1 时按 1 处理；id 为空时返回 shortIDFallback，绝不产出空串。
func shortID(id string, n int) string {
	if n < 1 {
		n = 1
	}
	runes := []rune(id)
	if len(runes) == 0 {
		return shortIDFallback
	}
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}

// fallbackTitle 在没有标题时给一个能认出来的名字。
func fallbackTitle(id string) string {
	return "会话 " + shortID(id, 8)
}
