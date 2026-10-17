package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/clean"
	"github.com/ruanbw/agent-sweep/internal/probe"
	"github.com/ruanbw/agent-sweep/internal/scan"
	"github.com/ruanbw/agent-sweep/internal/session"
	"github.com/ruanbw/agent-sweep/internal/update"
)

// Prefs 是落盘契约。键名不要改：改一个等于静默丢弃用户已保存的设置。
type Prefs struct {
	DeleteMode          string `json:"deleteMode"` // trash | permanent
	ConfirmBeforeDelete bool   `json:"confirmBeforeDelete"`
	IncludeSnapshots    bool   `json:"includeSnapshots"`
	AutoScan            bool   `json:"autoScan"`
	AutoCheckUpdates    bool   `json:"autoCheckUpdates"`
}

func defaultPrefs() Prefs {
	return Prefs{
		DeleteMode:          "trash",
		ConfirmBeforeDelete: true,
		IncludeSnapshots:    true,
		AutoScan:            true,
		AutoCheckUpdates:    true,
	}
}

// ScanStatus 是一次扫描的状态快照，UI 轮询它。
type ScanStatus struct {
	Status     string      `json:"status"` // idle | running | done | error
	StartedAt  string      `json:"startedAt,omitempty"`
	FinishedAt string      `json:"finishedAt,omitempty"`
	DurationMs int64       `json:"durationMs"`
	Progress   Progress    `json:"progress"`
	Issues     []ScanIssue `json:"issues"`
}

type Progress struct {
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current"`
}

type ScanIssue struct {
	AgentID string `json:"agentId"`
	Message string `json:"message"`
}

// AgentView 是 AgentResult 的 UI 投影：把会话聚合进计数，避免前端再算一遍。
type AgentView struct {
	AgentID      string   `json:"id"`
	Name         string   `json:"name"`
	Group        string   `json:"group"`
	Vendor       string   `json:"vendor"`
	Installed    bool     `json:"installed"`
	Home         string   `json:"home"`
	Roots        []string `json:"roots"`
	Note         string   `json:"note"`
	Custom       bool     `json:"custom"`
	SessionCount int      `json:"sessionCount"`
	TotalBytes   int64    `json:"totalBytes"`
	Error        string   `json:"error"`
}

type SessionView struct {
	UID       string   `json:"uid"`
	AgentID   string   `json:"agentId"`
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Project   string   `json:"project"`
	UpdatedAt string   `json:"updatedAt"`
	Bytes     int64    `json:"bytes"`
	Messages  int      `json:"messages"`
	Snippet   string   `json:"snippet"`
	Active    bool     `json:"active"`
	Storage   string   `json:"storage"`
	Paths     []string `json:"paths"`
}

// Service 是唯一的后端状态。
//
// 所有 HTTP 请求都在 goroutine 里跑，`mu` 保护下面几份数据。
// 扫描结果整份替换而不是增量合并：增量合并的代价是「两条扫描结果同一 uid 谁赢」
// 这种没法回答的问题，而扫描本身只要几百毫秒。
type Service struct {
	mu      sync.Mutex
	version string
	// appDir 是应用自己的数据目录（备份 / journal / prefs）。
	// 做成字段而不是到处调 catalog.AppDir()：测试要把它指向临时目录，
	// 否则跑一次测试就会往开发者真实的 ~/Library 里写 journal。
	appDir string
	// openCommand 执行系统的「打开」动作。生产环境指向 exec.Command，
	// 测试换成记录器，才能断言被拒绝的路径根本没走到 open。
	openCommand func(name string, args ...string) error
	prefs       Prefs
	specs       []catalog.Spec
	agents      []AgentView
	sessions    []session.Session
	byUID       map[string]session.Session
	scan        ScanStatus
	scanning    bool
	cleaner     *clean.Cleaner
	found       []probe.Candidate
	icons       *iconStore

	// 更新相关的状态都在 update.go 里维护，这里只放字段：
	// updater 在 NewService 之后不再改变，updateView / lastRelease 由 mu 保护。
	updater        *update.Client
	updateView     UpdateView
	updateBusy     bool
	updateOwner    string
	updateCache    string
	updateTarget   string // 测试注入；空 = 安装到当前运行的 .app
	lastRelease    *update.Release
	relaunchUpdate func(appPath string, pid int, workDir string) error
	onRestart      func()
	// guardLocal 只在 serve 模式打开：桌面壳用 wails://，不需要也不应该套这层限制。
	guardLocal bool
}

func NewService(version string) *Service {
	appDir := catalog.AppDir()
	svc := &Service{
		version:     version,
		appDir:      appDir,
		openCommand: openWithSystem,
		prefs:       loadPrefs(appDir),
		byUID:       map[string]session.Session{},
		// Issues 给空切片而不是 nil：nil 会序列化成 null，
		// 前端 `issues.length` 直接抛错 —— 契约里写的是数组，就得是数组。
		scan:  ScanStatus{Status: "idle", Issues: []ScanIssue{}},
		icons: newIconStore(filepath.Join(appDir, "icons")),
	}
	svc.cleaner = clean.New(clean.Options{
		Mode:       svc.prefs.DeleteMode,
		BackupDir:  filepath.Join(appDir, "backups"),
		JournalDir: filepath.Join(appDir, "journal"),
	})
	svc.updater = update.NewClient(version)
	svc.initUpdate()
	return svc
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/delete", s.handleDelete)
	mux.HandleFunc("/api/undo", s.handleUndo)
	mux.HandleFunc("/api/discover", s.handleDiscover)
	mux.HandleFunc("/api/adapters", s.handleAdapters)
	mux.HandleFunc("/api/prefs", s.handlePrefs)
	mux.HandleFunc("/api/reveal", s.handleReveal)
	mux.HandleFunc("/api/open", s.handleOpen)
	mux.HandleFunc("/api/icon", s.handleIcon)
	mux.HandleFunc("/api/update", s.handleUpdate)
	mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("/api/update/apply", s.handleUpdateApply)
	mux.Handle("/", staticHandler())
	return s.guard(mux)
}

// MARK: - bootstrap

func (s *Service) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	agents := s.agents
	if agents == nil {
		agents = []AgentView{}
	}
	payload := map[string]any{
		"version":  s.version,
		"platform": runtime.GOOS,
		"prefs":    s.prefs,
		"scan":     s.scan,
		"agents":   agents,
		"sessions": s.viewSessions(),
	}
	s.mu.Unlock()

	home, _ := os.UserHomeDir()
	payload["home"] = home
	payload["batches"] = s.batches()
	writeJSON(w, payload)
}

func (s *Service) viewSessions() []SessionView {
	out := make([]SessionView, 0, len(s.sessions))
	for _, item := range s.sessions {
		out = append(out, SessionView{
			UID:       item.UID,
			AgentID:   item.AgentID,
			ID:        item.ID,
			Title:     item.Title,
			Project:   item.Project,
			UpdatedAt: item.UpdatedAt.Format(time.RFC3339),
			Bytes:     item.Bytes,
			Messages:  item.Messages,
			Snippet:   item.Snippet,
			Active:    item.Active,
			Storage:   item.Storage,
			Paths:     item.Paths,
		})
	}
	return out
}

// MARK: - scan

func (s *Service) handleScan(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.startScan()
		writeJSON(w, map[string]bool{"ok": true})
	case http.MethodGet:
		s.mu.Lock()
		status := s.scan
		s.mu.Unlock()
		writeJSON(w, status)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// startScan 幂等：已经在扫描时什么都不做。
// 重复触发在 UI 上很常见（用户连点、启动自动扫描 + 手动扫描），开两次是纯浪费。
func (s *Service) startScan() {
	s.startScanWith(catalog.All, scan.AllWithProgress)
}

// scanRunner 是一次扫描的执行器。抽成参数只为让测试能注入 panic，
// 生产路径只有 scan.AllWithProgress 一个实现。
type scanRunner func(specs []catalog.Spec, progress func(done, total int, current string)) []scan.AgentResult

// startScanWith 是 startScan 的实现，specs/run 可注入（测试用）。
// 状态抢占在调用方 goroutine 里同步完成，保证紧接着的第二次调用一定看到 scanning=true。
func (s *Service) startScanWith(specs func() []catalog.Spec, run scanRunner) {
	if !s.beginScan() {
		return
	}
	go s.runScan(specs, run)
}

// beginScan 抢占扫描状态，返回 false 表示已经有一次扫描在跑。
func (s *Service) beginScan() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanning {
		return false
	}
	s.scanning = true
	s.scan = ScanStatus{
		Status:    "running",
		StartedAt: time.Now().Format(time.RFC3339),
		Progress:  Progress{},
	}
	return true
}

// scanOutcome 是一次扫描的收尾数据。
type scanOutcome struct {
	status string
	// applyResults 为 true 才整份替换扫描结果；异常路径保留上一次的数据，
	// 半份结果覆盖内存比保留旧结果更糟。
	applyResults bool
	specs        []catalog.Spec
	agents       []AgentView
	sessions     []session.Session
	byUID        map[string]session.Session
	issues       []ScanIssue
	// progress 为 nil 表示保留当前进度（异常路径），不谎报「全部完成」。
	progress *Progress
}

// runScan 跑一次扫描。收尾放在 defer 里：正常结束、提前 return、panic
// 都必须经过 finishScan，状态机不能让 scanning 永远停在 true。
func (s *Service) runScan(specs func() []catalog.Spec, run scanRunner) {
	started := time.Now()
	// 默认按失败收尾：没有显式声明成功的退出，不能变成一次「done」。
	outcome := scanOutcome{
		status: "error",
		issues: []ScanIssue{{Message: "扫描异常终止: 未完成"}},
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			// panic 不能被吞成成功：前端要看到一条错误，而不是旧数据 + done。
			outcome = scanOutcome{
				status: "error",
				issues: []ScanIssue{{Message: fmt.Sprintf("扫描异常终止: %v", recovered)}},
			}
		}
		s.finishScan(started, outcome)
	}()

	specsList := specs()
	results := run(specsList, func(done, total int, current string) {
		s.mu.Lock()
		s.scan.Progress = Progress{Done: done, Total: total, Current: current}
		s.mu.Unlock()
	})

	agents := make([]AgentView, 0, len(results))
	var sessions []session.Session
	byUID := map[string]session.Session{}
	var issues []ScanIssue
	for _, result := range results {
		view := AgentView{
			AgentID:      result.AgentID,
			Name:         result.Name,
			Group:        result.Group,
			Vendor:       result.Vendor,
			Installed:    result.Installed,
			Home:         result.Home,
			Roots:        result.Roots,
			Note:         result.Note,
			Custom:       result.Custom,
			SessionCount: len(result.Sessions),
			TotalBytes:   result.TotalBytes,
			Error:        result.Err,
		}
		agents = append(agents, view)
		if result.Err != "" {
			issues = append(issues, ScanIssue{AgentID: result.AgentID, Message: result.Err})
		}
		for _, item := range result.Sessions {
			sessions = append(sessions, item)
			byUID[item.UID] = item
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	progress := Progress{Done: len(results), Total: len(results)}
	outcome = scanOutcome{
		status:       "done",
		applyResults: true,
		specs:        specsList,
		agents:       agents,
		sessions:     sessions,
		byUID:        byUID,
		issues:       issues,
		progress:     &progress,
	}
}

// finishScan 是扫描状态机唯一的出口：复位 scanning，并把 scan 收敛到终态。
// 成功、panic、提前返回都必须经过它，否则前端会一直转圈且无法再发起扫描。
func (s *Service) finishScan(started time.Time, outcome scanOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if outcome.applyResults {
		s.specs = outcome.specs
		s.agents = outcome.agents
		s.sessions = outcome.sessions
		s.byUID = outcome.byUID
	}
	progress := s.scan.Progress
	if outcome.progress != nil {
		progress = *outcome.progress
	}
	s.scanning = false
	s.scan = ScanStatus{
		Status:     outcome.status,
		StartedAt:  s.scan.StartedAt,
		FinishedAt: time.Now().Format(time.RFC3339),
		DurationMs: time.Since(started).Milliseconds(),
		Progress:   progress,
		Issues:     orEmptyIssues(outcome.issues),
	}
}

// MARK: - delete / undo

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		UIDs []string `json:"uids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	s.mu.Lock()
	if len(body.UIDs) == 0 {
		s.mu.Unlock()
		writeJSON(w, clean.Result{})
		return
	}
	// 按 Agent 分组：同一 Agent 的会话必须一次性交给同一个清理器，
	// 否则索引文件会被反复读写，而且「删完索引为空就删文件」的判断会失效。
	grouped := map[string][]session.Session{}
	for _, uid := range body.UIDs {
		item, ok := s.byUID[uid]
		if !ok {
			continue
		}
		grouped[item.AgentID] = append(grouped[item.AgentID], item)
	}
	specs := s.specs
	mode := s.prefs.DeleteMode
	byUID := s.byUID
	s.mu.Unlock()

	// 每次删除按当前偏好新建一个清理器，而不是复用单例再改模式：
	// 删除可能并发触发（用户连点），让一个可变模式的对象跨请求共享，
	// 会出现「用户已经切成废纸篓，这一批却按旧的直接删」的时序错误。
	cleaner := clean.New(clean.Options{
		Mode:       mode,
		BackupDir:  filepath.Join(s.appDir, "backups"),
		JournalDir: filepath.Join(s.appDir, "journal"),
	})

	var total clean.Result
	var batchIDs []string
	for agentID, items := range grouped {
		spec, ok := findSpec(specs, agentID)
		if !ok {
			continue
		}
		home := spec.Home(nil)
		result, err := cleaner.Delete(spec, home, items)
		if err != nil {
			total.Errors = append(total.Errors, clean.ItemError{UID: agentID, Message: err.Error()})
			continue
		}
		total.Freed += result.Freed
		total.Count += result.Count
		total.Errors = append(total.Errors, result.Errors...)
		if result.BatchID != "" {
			batchIDs = append(batchIDs, result.BatchID)
		}
	}

	// 从内存里剔掉已删的会话，UI 立刻反映，不必等下一次扫描。
	// 注意判定用「这次真正发起删除的 uid」，不是 count —— 清理器可能部分失败。
	// 只把**确实删成功**的从内存里剔掉：清洁器可能部分失败（库被锁、权限不足），
	// 把失败的也藏起来，用户会以为删掉了，下次扫描又冒出来。
	failed := map[string]bool{}
	for _, itemErr := range total.Errors {
		failed[itemErr.UID] = true
	}
	deleted := map[string]bool{}
	for _, uid := range body.UIDs {
		if _, ok := byUID[uid]; ok && !failed[uid] {
			deleted[uid] = true
		}
	}
	s.mu.Lock()
	kept := s.sessions[:0]
	for _, item := range s.sessions {
		if deleted[item.UID] {
			continue
		}
		kept = append(kept, item)
	}
	s.sessions = kept
	s.recountLocked()
	s.mu.Unlock()

	if total.Errors == nil {
		total.Errors = []clean.ItemError{}
	}
	payload := map[string]any{
		"batchId":  first(batchIDs),
		"batchIds": batchIDs,
		"freed":    total.Freed,
		"count":    total.Count,
		"errors":   total.Errors,
	}
	writeJSON(w, payload)
}

func (s *Service) handleUndo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		BatchID  string   `json:"batchId"`
		BatchIDs []string `json:"batchIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	ids := body.BatchIDs
	if len(ids) == 0 && body.BatchID != "" {
		ids = []string{body.BatchID}
	}
	restored := 0
	var errs []string
	for _, id := range ids {
		result, err := s.cleaner.Undo(id)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		restored += result.Restored
		errs = append(errs, result.Errors...)
	}
	if errs == nil {
		errs = []string{}
	}
	writeJSON(w, map[string]any{"restored": restored, "errors": errs})
}

func (s *Service) batches() []clean.Batch {
	batches, err := s.cleaner.Batches(20)
	if err != nil {
		return nil
	}
	return batches
}

// recountLocked 重新聚合每个 Agent 的会话数与体积（删除后调用）。
func (s *Service) recountLocked() {
	counts := map[string]int{}
	bytes := map[string]int64{}
	for _, item := range s.sessions {
		counts[item.AgentID]++
		bytes[item.AgentID] += item.Bytes
	}
	for i := range s.agents {
		s.agents[i].SessionCount = counts[s.agents[i].AgentID]
		s.agents[i].TotalBytes = bytes[s.agents[i].AgentID]
	}
}

// MARK: - discover / adapters

func (s *Service) handleDiscover(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	specs := s.specs
	cached := s.found
	s.mu.Unlock()
	if len(specs) == 0 {
		specs = catalog.All()
	}
	if len(cached) > 0 {
		writeJSON(w, map[string]any{"candidates": cached})
		return
	}
	candidates, err := probe.Discover(specs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.found = candidates
	s.mu.Unlock()
	writeJSON(w, map[string]any{"candidates": candidates})
}

func (s *Service) handleAdapters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var spec catalog.Spec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, "适配器不是合法 JSON")
			return
		}
		if spec.ID == "" || spec.Name == "" || len(spec.Roots) == 0 {
			writeError(w, http.StatusBadRequest, "适配器至少要填 id / name / roots")
			return
		}
		if len(spec.Stores) == 0 {
			writeError(w, http.StatusBadRequest, "适配器至少要声明一个 store")
			return
		}
		if err := catalog.AddCustom(spec); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.invalidate()
		writeJSON(w, map[string]any{"ok": true, "id": spec.ID})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		removed, err := catalog.RemoveCustom(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !removed {
			writeError(w, http.StatusBadRequest, "内置 Agent 不能删除")
			return
		}
		s.invalidate()
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// invalidate 让下一次 bootstrap 清掉发现缓存并重扫。
func (s *Service) invalidate() {
	s.mu.Lock()
	s.found = nil
	s.mu.Unlock()
	s.startScan()
}

// MARK: - prefs / shell

func (s *Service) handlePrefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		prefs := s.prefs
		s.mu.Unlock()
		writeJSON(w, prefs)
	case http.MethodPut, http.MethodPost:
		// 用 map[string]json.RawMessage 解，而不是直接解进 Prefs：
		// 前端抽屉是逐开关发请求的，直接解会把「没传的字段」读成 false，
		// 于是一改删除模式就会悄悄关掉「删除前确认」。字段存在才改。
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeError(w, http.StatusBadRequest, "设置不是合法 JSON")
			return
		}
		// 读-改-写在同一个临界区里：并发切两个开关时，先解锁再写回会丢掉其中一个。
		s.mu.Lock()
		prefs := s.prefs
		autoCheckBefore := prefs.AutoCheckUpdates
		if value, ok := raw["deleteMode"]; ok {
			var mode string
			if json.Unmarshal(value, &mode) == nil && (mode == "trash" || mode == "permanent") {
				prefs.DeleteMode = mode
			}
		}
		for key, target := range map[string]*bool{
			"confirmBeforeDelete": &prefs.ConfirmBeforeDelete,
			"includeSnapshots":    &prefs.IncludeSnapshots,
			"autoScan":            &prefs.AutoScan,
			"autoCheckUpdates":    &prefs.AutoCheckUpdates,
		} {
			if value, ok := raw[key]; ok {
				_ = json.Unmarshal(value, target)
			}
		}
		if err := savePrefs(s.appDir, prefs); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.prefs = prefs
		s.mu.Unlock()
		writeJSON(w, prefs)
		if prefs.AutoCheckUpdates && !autoCheckBefore {
			// 打开开关就立刻查一次；缓存新鲜也不能跳过，否则用户会以为开关没生效。
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				s.checkUpdate(ctx)
			}()
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

func (s *Service) handleReveal(w http.ResponseWriter, r *http.Request) {
	path := decodePath(w, r)
	if path == "" {
		return
	}
	// Finder 只能显示本地路径；会话文件与应用数据目录之外的一律拒绝，
	// 否则一个 .app/.command 路径就等于一次任意执行。
	if !s.allowedLocalPath(path) {
		writeError(w, http.StatusForbidden, "只能显示扫描结果或应用数据目录里的路径")
		return
	}
	if err := s.runOpen("open", "-R", path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Service) handleOpen(w http.ResponseWriter, r *http.Request) {
	path := decodePath(w, r)
	if path == "" {
		return
	}
	// 前端只会用它打开两种东西：会话文件，或 GitHub 发布页。URL 走白名单，
	// 免得一条被污染的缓存把 releasePage 变成任意命令/任意站点。
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if !allowedExternalURL(path) {
			writeError(w, http.StatusBadRequest, "只允许打开 GitHub 上的发布页")
			return
		}
	} else if !s.allowedLocalPath(path) {
		// 本地路径必须是扫描结果里的会话路径，或应用自己的数据目录。
		// 这里不做「尽量放行」：open 会执行 .app/.command/.terminal。
		writeError(w, http.StatusForbidden, "只能打开扫描结果或应用数据目录里的路径")
		return
	}
	if err := s.runOpen("open", path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// allowedLocalPath 判断本地路径是否可以交给 `open`。
//
// 只允许两类根：
//   - 扫描结果里会话实际占用的路径（s.sessions[].Paths，可能是文件或目录）；
//   - 应用自己的数据目录（journal / 备份 / 日志），即 catalog.AppDir()。
//
// 必须同时满足字面路径与真实路径都在根内：只看字面路径，会话目录里的一个
// symlink 就能把 /etc/passwd 带出去；只看真实路径，前缀相近的兄弟目录
// （/home/a 与 /home/ab）又会被 strings.HasPrefix 误判。任一路径不存在也拒绝。
func (s *Service) allowedLocalPath(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	return allowedLocalPathIn(path, s.localPathRoots())
}

// localPathRoots 快照当前允许的根目录。复制一份再解锁：
// 后面的 EvalSymlinks 是文件系统调用，不该在持有 s.mu 时做。
func (s *Service) localPathRoots() []string {
	s.mu.Lock()
	roots := make([]string, 0, len(s.sessions)+2)
	for _, item := range s.sessions {
		roots = append(roots, item.Paths...)
	}
	appDir := s.appDir
	s.mu.Unlock()

	// 正常情况下 appDir 就是 catalog.AppDir()；两个都收，服务被测试换成
	// 临时目录时应用自己的数据目录仍然可打开。
	roots = append(roots, appDir, catalog.AppDir())

	seen := make(map[string]bool, len(roots))
	out := roots[:0]
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		clean := filepath.Clean(root)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

// allowedLocalPathIn 是不碰 Service 状态的纯判断，便于单测。
func allowedLocalPathIn(path string, roots []string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	cleanPath := filepath.Clean(path)
	resolvedPath, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		// 不存在（或中途有断链）的路径没有「打开」的意义，直接拒绝。
		return false
	}
	resolvedPath = filepath.Clean(resolvedPath)

	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		cleanRoot := filepath.Clean(root)
		if !pathWithin(cleanRoot, cleanPath) {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
		if err != nil {
			continue
		}
		if pathWithin(filepath.Clean(resolvedRoot), resolvedPath) {
			return true
		}
	}
	return false
}

// pathWithin 用 filepath.Rel 判断 target 是否等于 root 或位于 root 之下。
// 不用 strings.HasPrefix：/home/a 会错误地放行 /home/ab。
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runOpen 调用可注入的打开实现。openCommand 为空时回落到真实执行，
// 但空 Service（测试里直接构造）不应该因此 panic。
func (s *Service) runOpen(name string, args ...string) error {
	if s.openCommand != nil {
		return s.openCommand(name, args...)
	}
	return openWithSystem(name, args...)
}

func openWithSystem(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// allowedExternalURL 只放行 GitHub 域名。更新器的 pageUrl 来自发布元数据，
// 正常永远是 github.com；这里不做「尽量放行」，宁可让异常值打不开。
func allowedExternalURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "github.com" || host == "www.github.com"
}

func decodePath(w http.ResponseWriter, r *http.Request) string {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		writeError(w, http.StatusBadRequest, "缺少 path")
		return ""
	}
	return body.Path
}

// MARK: - 小工具

func findSpec(specs []catalog.Spec, id string) (catalog.Spec, bool) {
	for _, spec := range specs {
		if spec.ID == id {
			return spec, true
		}
	}
	return catalog.Spec{}, false
}

func snapshotPrefs(s *Service) Prefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prefs
}

func orEmptyIssues(issues []ScanIssue) []ScanIssue {
	if issues == nil {
		return []ScanIssue{}
	}
	return issues
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

// writeError 统一错误响应：Content-Type 必须是 JSON。
//
// 之前用 http.Error 会把它写成 text/plain，前端解析失败只能显示「HTTP 409」，
// 而真正有用的那句话（比如「已经有一个更新在进行了」）被丢掉了。
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(map[string]string{"error": message})
}

func loadPrefs(dir string) Prefs {
	prefs := defaultPrefs()
	path := filepath.Join(dir, "prefs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return prefs
	}
	if err := json.Unmarshal(data, &prefs); err != nil {
		return defaultPrefs()
	}
	if prefs.DeleteMode != "trash" && prefs.DeleteMode != "permanent" {
		prefs.DeleteMode = "trash"
	}
	return prefs
}

func savePrefs(dir string, prefs Prefs) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "prefs.json"), data, 0o644)
}
