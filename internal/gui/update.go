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
//	downloading 正在下载（progress 0–78）
//	installing  正在替换应用（progress 78–100）
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
	cache := updateCache{
		CheckedAt:   time.Now(),
		Tag:         release.Tag,
		Version:     release.Version,
		Notes:       release.Notes,
		PageURL:     release.PageURL,
		AssetName:   asset.Name,
		AssetSize:   asset.Size,
		PublishedAt: release.PublishedAt,
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.updateCache, data, 0o644)
}

// MARK: - handlers

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	view := s.updateView
	view.AutoCheck = s.prefs.AutoCheckUpdates
	s.mu.Unlock()
	writeJSON(w, view)
}

func (s *Service) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	writeJSON(w, s.checkUpdate(ctx))
}

func (s *Service) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	view := s.updateView
	busy := s.updateBusy
	s.mu.Unlock()

	if busy {
		http.Error(w, jsonError("已经有一个更新在进行了"), http.StatusConflict)
		return
	}
	if !view.Available {
		http.Error(w, jsonError("没有可安装的新版本"), http.StatusBadRequest)
		return
	}
	if !view.CanApply {
		http.Error(w, jsonError("当前不是从 .app 运行，请从发布页手动下载"), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.updateBusy = true
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

// checkUpdate 拉一次最新发布并更新状态。
//
// 并发调用直接返回当前状态：用户连点「检查更新」不应该打出多个 GitHub 请求。
func (s *Service) checkUpdate(ctx context.Context) UpdateView {
	s.mu.Lock()
	if s.updateBusy {
		view := s.updateView
		s.mu.Unlock()
		return view
	}
	previous := s.updateView
	s.updateBusy = true
	s.updateView.State = "checking"
	s.updateView.Error = ""
	s.updateView.Progress = 0
	s.updateView.AutoCheck = s.prefs.AutoCheckUpdates
	s.mu.Unlock()

	release, err := s.updater.Latest(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateBusy = false
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	s.mu.Lock()
	release := s.lastRelease
	s.mu.Unlock()
	if release == nil {
		latest, err := s.updater.Latest(ctx)
		if err != nil {
			s.failUpdate(err)
			return
		}
		release = latest
	}

	result, err := s.updater.Install(ctx, release, "", update.DefaultWorkDir(), func(percent int, note string) {
		s.mu.Lock()
		s.updateView.Progress = percent
		if percent >= 78 && s.updateView.State == "downloading" {
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
	s.updateBusy = false
	// 装完就把缓存删掉：下次启动直接问 GitHub，而不是相信「刚装完时」的旧答案。
	_ = os.Remove(s.updateCache)
	s.mu.Unlock()

	if onRestart != nil {
		// 留一拍让前端把「安装完成」画出来，再退出进程交给重启脚本。
		time.Sleep(900 * time.Millisecond)
		onRestart()
	}
}

func (s *Service) failUpdate(err error) {
	s.mu.Lock()
	s.updateBusy = false
	s.updateView.State = "error"
	s.updateView.Error = err.Error()
	s.mu.Unlock()
}
