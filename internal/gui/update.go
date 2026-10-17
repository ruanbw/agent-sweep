package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ruanbw/agent-sweep/internal/update"
)

// UpdateView 是 GET /api/update 的响应。
//
// state 是唯一需要前端分支的字段，其余字段都是给那一屏文案填内容用的：
//
//	idle        还没查过
//	checking    正在查
//	available   有新版本，可以自动装
//	unsupported 有新版本，但当前不是从 .app 运行，只能去发布页手动下载
//	uptodate    已是最新
//	downloading 正在下载（progress 0–70）
//	installing  正在校验 / 解压 / 替换（progress 72–100）
//	done        装完了，进程马上退出重启
//	error       查或装失败，见 error
type UpdateView struct {
	State      string `json:"state"`
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Available  bool   `json:"available"`
	Notes      string `json:"notes"`
	PageURL    string `json:"pageUrl"`
	AssetName  string `json:"assetName"`
	AssetBytes int64  `json:"assetBytes"`
	Progress   int    `json:"progress"`
	CheckedAt  string `json:"checkedAt"`
	Error      string `json:"error"`
	CanApply   bool   `json:"canApply"`
	AutoCheck  bool   `json:"autoCheck"`
}

// updateCacheFreshness 决定自动检查多久跑一次。
//
// 6 小时：GitHub 匿名 API 每小时只给 60 次，把打开窗口的次数映射成请求次数
// 很容易撞限流；一天查几次足够覆盖「发版后当天知道」这个真实需求。
const updateCacheFreshness = 6 * time.Hour

// updateCache 是落盘的检查结果，只用于展示与「要不要联网」的判断。
// 真正安装时会重新拉一次发布元数据，缓存里的下载地址不参与安装。
type updateCache struct {
	CheckedAt   time.Time `json:"checkedAt"`
	Tag         string    `json:"tag"`
	Version     string    `json:"version"`
	Notes       string    `json:"notes"`
	PageURL     string    `json:"pageUrl"`
	AssetName   string    `json:"assetName"`
	AssetSize   int64     `json:"assetSize"`
	PublishedAt time.Time `json:"publishedAt"`
}

// initUpdate 在 NewService 里调用：确定「能不能自动装」并读一次缓存。
//
// 这里刻意不联网：NewService 在测试、serve、GUI 三种入口都会跑，
// 其中只有 GUI 希望启动时安静地查一次更新。
func (s *Service) initUpdate() {
	state := UpdateView{State: "idle", Current: s.version}
	if _, err := update.DetectBundle(); err == nil {
		state.CanApply = true
	}
	s.updateView = state
	s.updateCache = filepath.Join(s.appDir, "update.json")
	s.relaunchUpdate = update.RelaunchAfterExit
	if cache, ok := readUpdateCache(s.updateCache); ok {
		s.applyUpdateCache(cache)
	}
}

func (s *Service) applyUpdateCache(cache updateCache) {
	s.updateView.Latest = cache.Version
	s.updateView.Notes = cache.Notes
	s.updateView.PageURL = cache.PageURL
	s.updateView.AssetName = cache.AssetName
	s.updateView.AssetBytes = cache.AssetSize
	if !cache.CheckedAt.IsZero() {
		s.updateView.CheckedAt = cache.CheckedAt.Format(time.RFC3339)
	}
	s.updateView.Available = update.IsNewer(s.version, cache.Version)
	switch {
	case !s.updateView.Available:
		s.updateView.State = "uptodate"
	case s.updateView.CanApply:
		s.updateView.State = "available"
	default:
		s.updateView.State = "unsupported"
	}
}

func readUpdateCache(path string) (updateCache, bool) {
	var cache updateCache
	data, err := os.ReadFile(path)
	if err != nil {
		return cache, false
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return updateCache{}, false
	}
	if cache.Version == "" {
		return updateCache{}, false
	}
	return cache, true
}

func (s *Service) writeUpdateCache(release *update.Release, asset update.Asset) {
	// 发布说明可能很长；缓存只负责展示，截断到 256 KB，避免把磁盘当数据库。
	notes := release.Notes
	if len(notes) > 256<<10 {
		notes = notes[:256<<10] + "\n…"
	}
	cache := updateCache{
		CheckedAt:   time.Now(),
		Tag:         release.Tag,
		Version:     release.Version,
		Notes:       notes,
		PageURL:     release.PageURL,
		AssetName:   asset.Name,
		AssetSize:   asset.Size,
		PublishedAt: release.PublishedAt,
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.updateCache), 0o755)
	_ = os.WriteFile(s.updateCache, data, 0o644)
}

// MARK: - 状态机

// beginUpdate / endUpdate 用一个 owner 令牌把「检查」和「安装」串行化。
//
// 之前是「先读 updateBusy、解锁、再上锁置 true」：两个并发 apply 都能通过，
// 或者一个 check 与一个 apply 交叉执行，最后两个 replaceBundle 同时改同一个包。
func (s *Service) beginUpdate(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateBusy {
		return false
	}
	s.updateBusy = true
	s.updateOwner = owner
	return true
}

func (s *Service) endUpdate(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateOwner == owner {
		s.updateBusy = false
		s.updateOwner = ""
	}
}

func (s *Service) snapshotUpdate() UpdateView {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := s.updateView
	view.AutoCheck = s.prefs.AutoCheckUpdates
	return view
}

// MARK: - handlers

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	writeJSON(w, s.snapshotUpdate())
}

func (s *Service) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	writeJSON(w, s.checkUpdate(ctx))
}

func (s *Service) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}

	// 检查与置位在同一把锁里完成：两个并发请求只能有一个开始安装。
	s.mu.Lock()
	view := s.updateView
	if s.updateBusy {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "已经有一个更新在进行了")
		return
	}
	if !view.Available {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "没有可安装的新版本")
		return
	}
	if !view.CanApply {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "当前不是从 .app 运行，请从发布页手动下载")
		return
	}
	switch view.State {
	case "available", "error":
		// error 也允许：安装失败后 Available 仍然为 true，重试不需要重新联网检查。
	default:
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "当前状态不能安装更新")
		return
	}
	s.updateBusy = true
	s.updateOwner = "install"
	s.updateView.State = "downloading"
	s.updateView.Progress = 0
	s.updateView.Error = ""
	s.mu.Unlock()

	// 先回 200 再在后台装：下载要几十秒，前端应该拿到句柄开始轮询而不是干等。
	writeJSON(w, map[string]bool{"ok": true})
	go s.installUpdate()
}

// MARK: - 检查

// autoCheckUpdates 是启动时的静默检查。缓存还新鲜就不联网。
func (s *Service) autoCheckUpdates() {
	s.mu.Lock()
	auto := s.prefs.AutoCheckUpdates
	cachePath := s.updateCache
	s.mu.Unlock()
	if !auto || s.updateFresh(cachePath) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.checkUpdate(ctx)
}

func (s *Service) updateFresh(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < updateCacheFreshness
}

// touchUpdateCache 让失败也参与「6 小时一次」的节流。
//
// 不写新的检查结果（免得把失败伪装成 uptodate），只更新 mtime。没有缓存时保持没有：
// 下一次启动本来就是一次合理的重试。
func (s *Service) touchUpdateCache() {
	if _, err := os.Stat(s.updateCache); err != nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(s.updateCache, now, now)
}

// checkUpdate 拉一次最新发布并更新状态。
//
// 并发调用直接返回当前状态：用户连点「检查更新」不应该打出多个 GitHub 请求。
func (s *Service) checkUpdate(ctx context.Context) UpdateView {
	if !s.beginUpdate("check") {
		return s.snapshotUpdate()
	}
	defer s.endUpdate("check")

	s.mu.Lock()
	previous := s.updateView
	s.updateView.State = "checking"
	s.updateView.Error = ""
	s.updateView.Progress = 0
	s.updateView.AutoCheck = s.prefs.AutoCheckUpdates
	s.mu.Unlock()

	release, err := s.updater.Latest(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateView.Current = s.version
	s.updateView.CheckedAt = time.Now().Format(time.RFC3339)

	if err != nil {
		s.updateView.Error = err.Error()
		// 失败时保留上一次已知的版本信息：网络抖一下不该让「有新版本」的提示消失。
		s.updateView.State = "error"
		if previous.Latest != "" {
			s.updateView.Latest = previous.Latest
			s.updateView.Available = previous.Available
			s.updateView.Notes = previous.Notes
			s.updateView.PageURL = previous.PageURL
			s.updateView.AssetName = previous.AssetName
			s.updateView.AssetBytes = previous.AssetBytes
		}
		s.touchUpdateCache()
		return s.updateView
	}

	s.lastRelease = release
	s.updateView.Latest = release.Version
	s.updateView.Notes = release.Notes
	s.updateView.PageURL = release.PageURL
	s.updateView.Available = update.IsNewer(s.version, release.Version)

	asset, assetErr := s.updater.AssetFor(release)
	if assetErr == nil {
		s.updateView.AssetName = asset.Name
		s.updateView.AssetBytes = asset.Size
		s.writeUpdateCache(release, asset)
	} else {
		s.updateView.AssetName = ""
		s.updateView.AssetBytes = 0
		s.touchUpdateCache()
	}

	switch {
	case !s.updateView.Available:
		s.updateView.State = "uptodate"
	case s.updateView.CanApply && assetErr == nil:
		s.updateView.State = "available"
	default:
		// 有新版本但这台机器装不了（不是 .app）或这个平台没有包：给发布页。
		s.updateView.State = "unsupported"
		if assetErr != nil {
			s.updateView.Error = assetErr.Error()
		}
	}
	return s.updateView
}

// MARK: - 安装

func (s *Service) installUpdate() {
	defer s.endUpdate("install")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	s.mu.Lock()
	release := s.lastRelease
	target := s.updateTarget
	relaunch := s.relaunchUpdate
	s.mu.Unlock()
	if relaunch == nil {
		relaunch = update.RelaunchAfterExit
	}
	if release == nil {
		latest, err := s.updater.Latest(ctx)
		if err != nil {
			s.failUpdate(err)
			return
		}
		release = latest
	}

	result, err := s.updater.Install(ctx, release, target, update.DefaultWorkDir(), func(percent int, note string) {
		s.mu.Lock()
		s.updateView.Progress = percent
		if percent >= 72 && s.updateView.State == "downloading" {
			s.updateView.State = "installing"
		}
		s.mu.Unlock()
	})
	if err != nil {
		s.failUpdate(err)
		return
	}

	s.mu.Lock()
	onRestart := s.onRestart
	s.updateView.State = "done"
	s.updateView.Progress = 100
	s.updateView.Current = result.Version
	s.updateView.Latest = result.Version
	s.updateView.Available = false
	s.updateView.Error = ""
	// 装完就把缓存删掉：下次启动直接问 GitHub，而不是相信「刚装完时」的旧答案。
	_ = os.Remove(s.updateCache)
	s.mu.Unlock()

	if onRestart != nil {
		// 先把「等旧进程退出再 open 新包」的脚本挂上去，再退出自己。
		// 顺序反了就是「应用关了但没重开」——这个 bug 只有真跑一次更新才会发现。
		if err := relaunch(result.Path, os.Getpid(), update.DefaultWorkDir()); err != nil {
			s.mu.Lock()
			s.updateView.State = "error"
			s.updateView.Error = "更新已安装，但自动重启失败：" + err.Error() + "（请手动重新打开）"
			s.mu.Unlock()
			return
		}
		// 留一拍让前端把「安装完成」画出来，再退出进程交给重启脚本。
		// 前端在 installing 状态会以 500ms 轮询，1.8s 足够显示 done。
		time.Sleep(1800 * time.Millisecond)
		onRestart()
	}
}

func (s *Service) failUpdate(err error) {
	s.mu.Lock()
	s.updateView.State = "error"
	s.updateView.Error = err.Error()
	s.mu.Unlock()
}
