package gui

// R20 回归：/api/icon 必须使用扫描时留下的 s.specs 快照，而不是每次重新读
// adapters.json。这里的断言只覆盖查找与缓存链路，不触发 sips —— 真正的
// PNG 转换由 icons_test.go 在 darwin 上单独验证。

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// writeAdaptersFile 在隔离后的 HOME 下写 adapters.json。
// 调用前必须 t.Setenv("HOME", ...)，否则会写进开发者真实的目录。
func writeAdaptersFile(t *testing.T, specs []catalog.Spec) {
	t.Helper()
	path := catalog.AdaptersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newSnapshotService 构造最小 Service：数据目录与图标缓存都指向临时目录，
// 不碰真实用户数据。
func newSnapshotService(t *testing.T) *Service {
	t.Helper()
	svc := NewService("test")
	svc.appDir = t.TempDir()
	svc.icons = newIconStore(filepath.Join(svc.appDir, "icons"))
	return svc
}

// writeTestPNG 写一个合法 PNG 并返回字节，供预置的图标缓存使用。
func writeTestPNG(t *testing.T, path string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestIconSpecUsesScanSnapshotNotAdaptersFile 是核心回归：磁盘与快照里是两份
// 同 ID 但内容不同的声明，图标查找必须命中快照那份。
func TestIconSpecUsesScanSnapshotNotAdaptersFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// 快照的 Bundles 指向一个真实存在的临时 .app（只做文件查找，不跑 sips）。
	fakeApp := filepath.Join(home, "Applications", "AgentSweepSnapshotOnly.app")
	resources := filepath.Join(fakeApp, "Contents", "Resources")
	if err := os.MkdirAll(resources, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "AppIcon.icns"), []byte("fake icns"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 磁盘上的同 ID spec：Bundles 指向不存在的 .app，Roots 也不一样。
	diskSpec := catalog.Spec{
		ID:      "custom",
		Name:    "磁盘版 Custom",
		Vendor:  "disk",
		Bundles: []string{"AgentSweepDiskOnly.app"},
		Roots:   []string{filepath.Join(home, "disk-root")},
	}
	writeAdaptersFile(t, []catalog.Spec{diskSpec})

	// 快照里的同 ID spec：内容与磁盘版本不同。
	snapshotSpec := catalog.Spec{
		ID:      "custom",
		Name:    "快照版 Custom",
		Vendor:  "snapshot",
		Custom:  true,
		Bundles: []string{"AgentSweepSnapshotOnly.app"},
		Roots:   []string{filepath.Join(home, "snapshot-root")},
	}
	svc := newSnapshotService(t)
	svc.specs = []catalog.Spec{snapshotSpec}

	// 先证明磁盘上确实是另一份同 ID 的声明，这个用例才有区分力。
	onDisk, ok := catalog.Find("custom")
	if !ok {
		t.Fatal("fixture 无效：adapters.json 里应有 custom")
	}
	if onDisk.Name != diskSpec.Name || !reflect.DeepEqual(onDisk.Bundles, diskSpec.Bundles) {
		t.Fatalf("fixture 无效：磁盘声明与快照应不同，得到 %+v", onDisk)
	}

	got, ok := svc.iconSpec("custom")
	if !ok {
		t.Fatal("快照里有 custom，iconSpec 应命中")
	}
	if !reflect.DeepEqual(got, snapshotSpec) {
		t.Fatalf("iconSpec 没有使用扫描快照：got=%+v want=%+v", got, snapshotSpec)
	}

	// 查找链路的落点：快照的 bundle 能命中临时 .app，磁盘的不能。
	bundle, ok := findAppBundle(got)
	if !ok || bundle != fakeApp {
		t.Fatalf("快照 spec 的 bundle 应命中 %q，得到 %q ok=%v", fakeApp, bundle, ok)
	}
	if bundle, ok := findAppBundle(onDisk); ok {
		t.Fatalf("磁盘 spec 指向不存在的 bundle，不应命中任何 .app，得到 %q", bundle)
	}
}

// TestIconSpecFallsBackWhenSnapshotEmpty 覆盖「服务刚启动、还没扫描过」：
// 快照为空时允许回退 catalog.Find 保持图标可用；一旦有快照就不再回退。
func TestIconSpecFallsBackWhenSnapshotEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fallback := catalog.Spec{
		ID:      "fallback",
		Name:    "未扫描时的目录",
		Bundles: []string{"AgentSweepFallbackOnly.app"},
		Roots:   []string{"~/fallback-root"},
	}
	writeAdaptersFile(t, []catalog.Spec{fallback})

	svc := newSnapshotService(t)
	if len(svc.specs) != 0 {
		t.Fatal("fixture 应保持空快照")
	}
	got, ok := svc.iconSpec("fallback")
	if !ok {
		t.Fatal("快照为空时应回退到 catalog.Find")
	}
	if got.ID != fallback.ID || got.Name != fallback.Name {
		t.Fatalf("回退命中了错误的声明：%+v", got)
	}

	// 有快照之后就不该再回退：快照外的 ID 必须按不存在处理。
	svc.specs = []catalog.Spec{{ID: "known", Name: "Known"}}
	if _, ok := svc.iconSpec("fallback"); ok {
		t.Fatal("快照非空时不应回退到磁盘目录")
	}
}

// TestHandleIconSnapshotSuppressesDiskFallback 走完整 HTTP 链路。
// 磁盘目录里没有 custom，旧实现会 404；新实现应命中快照并返回缓存图标。
func TestHandleIconSnapshotSuppressesDiskFallback(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// ensure 在任何平台都会先拒绝非 darwin 的生成；这里预置缓存仍绕不过
		// 那个平台判断，所以这条 HTTP 断言只在 macOS 上有意义。
		t.Skip("图标缓存链路只在 darwin 上可用")
	}
	t.Setenv("HOME", t.TempDir())

	writeAdaptersFile(t, []catalog.Spec{{ID: "legacy-only", Name: "Legacy"}})

	svc := newSnapshotService(t)
	svc.specs = []catalog.Spec{{ID: "custom", Name: "Custom", Custom: true}}

	// 预置 ready 缓存，避免 ensure 走到 sips。
	pngPath := filepath.Join(svc.icons.dir, iconCacheName("custom"))
	pngBytes := writeTestPNG(t, pngPath)
	svc.icons.ready["custom"] = pngPath

	recorder := httptest.NewRecorder()
	svc.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/icon?id=custom", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("快照里的 custom 应返回图标，得到 %d：%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control 契约变了：%q", got)
	}
	if !bytes.Equal(recorder.Body.Bytes(), pngBytes) {
		t.Fatal("返回的图标内容不是缓存文件")
	}
}

// TestHandleIconUnknownIDReturns404 是回归：空 id、快照里没有的 id 都必须 404。
// disk-only 只存在于 adapters.json —— 快照非空时不能拿它补，否则又回到
// 「图标看新目录、删除看旧快照」的不一致。
func TestHandleIconUnknownIDReturns404(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	writeAdaptersFile(t, []catalog.Spec{{ID: "disk-only", Name: "Disk Only"}})

	svc := newSnapshotService(t)
	svc.specs = []catalog.Spec{{ID: "custom", Name: "Custom", Custom: true}}

	for _, target := range []string{
		"/api/icon",
		"/api/icon?id=",
		"/api/icon?id=missing",
		"/api/icon?id=disk-only",
	} {
		recorder := httptest.NewRecorder()
		svc.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s 应返回 404，得到 %d：%s", target, recorder.Code, recorder.Body.String())
		}
	}
}
