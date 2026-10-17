package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// 这组测试固定 R21 的修复：应用数据目录必须是构造参数，而不是在
// NewService 里直接取 catalog.AppDir()。
//
// 所有测试都把 HOME 指到 t.TempDir()，即使被测代码误调 catalog.AppDir()
// 也只会落进临时目录；NewServiceWith 本身则完全不读 catalog.AppDir()。

// TestNewServiceWithBindsEverythingToInjectedDir 证明注入的 appDir 是
// prefs / icons / cleaner 的 backups+journal / updateCache 的共同根。
func TestNewServiceWithBindsEverythingToInjectedDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	appDir := t.TempDir()

	// 预先落一份非默认 prefs：构造后能读到，说明 loadPrefs 用的是注入目录，
	// 而不是真实 ~/Library/Application Support/AgentSweep。
	prefs := `{"deleteMode":"trash","confirmBeforeDelete":false,` +
		`"includeSnapshots":false,"autoScan":false,"autoCheckUpdates":false}`
	if err := os.WriteFile(filepath.Join(appDir, "prefs.json"), []byte(prefs), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewServiceWith("test", appDir)

	if svc.appDir != appDir {
		t.Fatalf("appDir = %q，期望 %q", svc.appDir, appDir)
	}
	if svc.icons == nil {
		t.Fatal("icons 未初始化")
	}
	if got, want := svc.icons.dir, filepath.Join(appDir, "icons"); got != want {
		t.Fatalf("icons.dir = %q，期望 %q", got, want)
	}
	if got, want := svc.updateCache, filepath.Join(appDir, "update.json"); got != want {
		t.Fatalf("updateCache = %q，期望 %q", got, want)
	}
	if svc.prefs.AutoScan || svc.prefs.ConfirmBeforeDelete {
		t.Fatalf("prefs 没从注入目录读取：%+v", svc.prefs)
	}

	// clean.Cleaner 的 BackupDir / JournalDir 是 clean.Options 的未导出字段，
	// gui 包读不到，所以这里用行为断言：即便 sessions 为空，Delete 也会
	// reserveBatch —— 在 BackupDir 下建 <batchID>/，并在 JournalDir 下写
	// <batchID>.json。这两个落盘位置就是 cleaner 实际根目录的证据。
	dataRoot := t.TempDir()
	result, err := svc.cleaner.Delete(catalog.Spec{}, dataRoot, nil)
	if err != nil {
		t.Fatalf("cleaner.Delete 失败：%v", err)
	}
	if result.BatchID == "" {
		t.Fatal("Delete 没返回 batchID")
	}

	backupDir := filepath.Join(appDir, "backups", result.BatchID)
	if info, err := os.Stat(backupDir); err != nil || !info.IsDir() {
		t.Fatalf("备份目录不在注入的 appDir 下（%s）：err=%v", backupDir, err)
	}
	journalFile := filepath.Join(appDir, "journal", result.BatchID+".json")
	if info, err := os.Stat(journalFile); err != nil || info.IsDir() {
		t.Fatalf("journal 不在注入的 appDir 下（%s）：err=%v", journalFile, err)
	}

	// Batches 直接读 JournalDir。能列出刚写的批次，说明读的也是注入目录。
	batches, err := svc.cleaner.Batches(20)
	if err != nil {
		t.Fatalf("Batches 失败：%v", err)
	}
	found := false
	for _, batch := range batches {
		if batch.ID == result.BatchID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Batches 没读到批次 %s（读到 %+v）", result.BatchID, batches)
	}

	// Undo 走 readJournal + writeJournal，两条路径都来自 JournalDir。
	// 能对刚建的批次成功撤销（trash、无移动项），说明 journal 根确实是
	// appDir/journal；若仍指向真实用户目录，这里会报「找不到批次」。
	if _, err := svc.cleaner.Undo(result.BatchID); err != nil {
		t.Fatalf("Undo 没读到注入目录下的 journal：%v", err)
	}

	// 整个构造与删除/撤销过程都不该创建真实应用目录。
	realAppDir := filepath.Join(home, "Library", "Application Support", "AgentSweep")
	if _, err := os.Stat(realAppDir); !os.IsNotExist(err) {
		t.Fatalf("写入了真实应用目录 %s：%v", realAppDir, err)
	}
}

// TestNewServiceEqualsNewServiceWithCatalogAppDir 锁住生产路径：
// NewService(version) 必须等价于 NewServiceWith(version, catalog.AppDir())。
func TestNewServiceEqualsNewServiceWithCatalogAppDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	appDir := catalog.AppDir()

	produced := NewService("test")
	injected := NewServiceWith("test", appDir)

	if produced.appDir != injected.appDir {
		t.Fatalf("appDir 不一致：NewService=%q，NewServiceWith=%q", produced.appDir, injected.appDir)
	}
	if produced.appDir != appDir {
		t.Fatalf("NewService 没使用 catalog.AppDir()：got %q，want %q", produced.appDir, appDir)
	}
	if produced.icons.dir != injected.icons.dir {
		t.Fatalf("icons.dir 不一致：%q vs %q", produced.icons.dir, injected.icons.dir)
	}
	if produced.updateCache != injected.updateCache {
		t.Fatalf("updateCache 不一致：%q vs %q", produced.updateCache, injected.updateCache)
	}

	// 派生路径必须都从 appDir 算出来。
	if got, want := produced.icons.dir, filepath.Join(appDir, "icons"); got != want {
		t.Fatalf("icons.dir = %q，期望 %q", got, want)
	}
	if got, want := produced.updateCache, filepath.Join(appDir, "update.json"); got != want {
		t.Fatalf("updateCache = %q，期望 %q", got, want)
	}
	if produced.prefs != injected.prefs {
		t.Fatalf("prefs 不一致：%+v vs %+v", produced.prefs, injected.prefs)
	}
}

// TestNewServiceWithServesBootstrap 是回归：注入目录构造出的服务仍能完整
// 响应 GET /api/bootstrap，且不依赖真实用户目录。
func TestNewServiceWithServesBootstrap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	svc := NewServiceWith("test", t.TempDir())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	svc.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("/api/bootstrap 返回 %d：%s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("/api/bootstrap Content-Type = %q，期望 application/json", contentType)
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON：%v（body=%s）", err, recorder.Body.String())
	}
	if payload["version"] != "test" {
		t.Fatalf("version = %v，期望 test", payload["version"])
	}
	if payload["home"] != home {
		t.Fatalf("home = %v，期望隔离后的 %q", payload["home"], home)
	}
	if _, ok := payload["prefs"]; !ok {
		t.Fatalf("bootstrap 缺 prefs：%v", payload)
	}
	if _, ok := payload["batches"]; !ok {
		t.Fatalf("bootstrap 缺 batches：%v", payload)
	}
}
