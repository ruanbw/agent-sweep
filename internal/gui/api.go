package gui

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
	appDir   string
	prefs    Prefs
	specs    []catalog.Spec
	agents   []AgentView
	sessions []session.Session
	byUID    map[string]session.Session
	scan     ScanStatus
	scanning bool
	cleaner  *clean.Cleaner
	found    []probe.Candidate
	icons    *iconStore

	// 更新相关的状态都在 update.go 里维护，这里只放字段：
	// updater 在 NewService 之后不再改变，updateView / lastRelease 由 mu 保护。
	updater     *update.Client
	updateView  UpdateView
	updateBusy  bool
	updateCache string
	lastRelease *update.Release
	onRestart   func()
}

func NewService(version string) *Service {
	appDir := catalog.AppDir()
	svc := &Service{
		version: version,
		appDir:  appDir,
		prefs:   loadPrefs(appDir),
		byUID:   map[string]session.Session{},
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
	return mux
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
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// startScan 幂等：已经在扫描时什么都不做。
// 重复触发在 UI 上很常见（用户连点、启动自动扫描 + 手动扫描），开两次是纯浪费。
func (s *Service) startScan() {
	s.mu.Lock()
	if s.scanning {
		s.mu.Unlock()
		return
	}
	s.scanning = true
	s.scan = ScanStatus{
		Status:    "running",
		StartedAt: time.Now().Format(time.RFC3339),
		Progress:  Progress{},
	}
	s.mu.Unlock()

	go func() {
		started := time.Now()
		specs := catalog.All()
		results := scan.AllWithProgress(specs, func(done, total int, current string) {
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

		s.mu.Lock()
		s.specs = specs
		s.agents = agents
		s.sessions = sessions
		s.byUID = byUID
		s.scanning = false
		s.scan = ScanStatus{
			Status:     "done",
			StartedAt:  s.scan.StartedAt,
			FinishedAt: time.Now().Format(time.RFC3339),
			DurationMs: time.Since(started).Milliseconds(),
			Progress:   Progress{Done: len(results), Total: len(results)},
			Issues:     orEmptyIssues(issues),
		}
		s.mu.Unlock()
	}()
}

// MARK: - delete / undo

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		UIDs []string `json:"uids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"请求体不是合法 JSON"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		BatchID  string   `json:"batchId"`
		BatchIDs []string `json:"batchIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"请求体不是合法 JSON"}`, http.StatusBadRequest)
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
		http.Error(w, jsonError(err.Error()), http.StatusInternalServerError)
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
			http.Error(w, jsonError("适配器不是合法 JSON"), http.StatusBadRequest)
			return
		}
		if spec.ID == "" || spec.Name == "" || len(spec.Roots) == 0 {
			http.Error(w, jsonError("适配器至少要填 id / name / roots"), http.StatusBadRequest)
			return
		}
		if len(spec.Stores) == 0 {
			http.Error(w, jsonError("适配器至少要声明一个 store"), http.StatusBadRequest)
			return
		}
		if err := catalog.AddCustom(spec); err != nil {
			http.Error(w, jsonError(err.Error()), http.StatusInternalServerError)
			return
		}
		s.invalidate()
		writeJSON(w, map[string]any{"ok": true, "id": spec.ID})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		removed, err := catalog.RemoveCustom(id)
		if err != nil {
			http.Error(w, jsonError(err.Error()), http.StatusInternalServerError)
			return
		}
		if !removed {
			http.Error(w, jsonError("内置 Agent 不能删除"), http.StatusBadRequest)
			return
		}
		s.invalidate()
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			http.Error(w, jsonError("设置不是合法 JSON"), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		prefs := s.prefs
		s.mu.Unlock()
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
		s.mu.Lock()
		s.prefs = prefs
		s.mu.Unlock()
		if err := savePrefs(s.appDir, prefs); err != nil {
			http.Error(w, jsonError(err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, prefs)
		if prefs.AutoCheckUpdates && !autoCheckBefore {
			go s.autoCheckUpdates()
		}
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (s *Service) handleReveal(w http.ResponseWriter, r *http.Request) {
	path := decodePath(w, r)
	if path == "" {
		return
	}
	if err := exec.Command("open", "-R", path).Run(); err != nil {
		http.Error(w, jsonError(err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Service) handleOpen(w http.ResponseWriter, r *http.Request) {
	path := decodePath(w, r)
	if path == "" {
		return
	}
	if err := exec.Command("open", path).Run(); err != nil {
		http.Error(w, jsonError(err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func decodePath(w http.ResponseWriter, r *http.Request) string {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		http.Error(w, jsonError("缺少 path"), http.StatusBadRequest)
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

func jsonError(message string) string {
	data, _ := json.Marshal(map[string]string{"error": message})
	return string(data)
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
