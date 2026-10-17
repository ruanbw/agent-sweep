package gui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

func TestSanitizeBundleName(t *testing.T) {
	cases := map[string]string{
		"Cursor":              "Cursor.app",
		"Cursor.app":          "Cursor.app",
		"  Warp Agent ":       "Warp Agent.app",
		"../Evil.app":         "",
		"/tmp/Evil.app":       "",
		"Vendor\\Evil.app":    "",
		".app":                "",
		"Antigravity IDE.app": "Antigravity IDE.app",
	}
	for input, want := range cases {
		if got := sanitizeBundleName(input); got != want {
			t.Errorf("sanitizeBundleName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNameVariants(t *testing.T) {
	got := nameVariants("Warp Agent")
	want := []string{"Warp Agent", "Warp"}
	if len(got) != len(want) {
		t.Fatalf("nameVariants = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nameVariants = %v, want %v", got, want)
		}
	}
	if variants := nameVariants("Antigravity IDE"); len(variants) != 2 || variants[1] != "Antigravity" {
		t.Fatalf("Antigravity IDE 应产生去掉 IDE 的候选，得到 %v", variants)
	}
	if variants := nameVariants(""); variants != nil {
		t.Fatalf("空名字不应有候选，得到 %v", variants)
	}
}

func TestFindBundleInDirNested(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "Direct.app"),
		filepath.Join(root, "Vendor", "Nested.app"),
		filepath.Join(root, "Deep", "Deeper", "Deeper2", "TooDeep.app"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	names := map[string]bool{"nested.app": true}
	if got, ok := findBundleInDir(root, names); !ok || filepath.Base(got) != "Nested.app" {
		t.Fatalf("厂商子目录里的 .app 没有找到，got=%q ok=%v", got, ok)
	}
	if _, ok := findBundleInDir(root, map[string]bool{"toodeep.app": true}); ok {
		t.Fatal("超过两层的 .app 不应被找到")
	}
	if _, ok := findBundleInDir(filepath.Join(root, "missing"), map[string]bool{"x.app": true}); ok {
		t.Fatal("不存在的搜索根不应返回结果")
	}
}

func TestValidPNG(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.png")
	var buffer bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if !validPNG(good) {
		t.Fatal("合法 PNG 应通过校验")
	}

	bad := filepath.Join(dir, "bad.png")
	if err := os.WriteFile(bad, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if validPNG(bad) {
		t.Fatal("损坏文件不应通过 PNG 校验")
	}
	if validPNG(filepath.Join(dir, "missing.png")) {
		t.Fatal("不存在的文件不应通过 PNG 校验")
	}
}

func TestIconCacheName(t *testing.T) {
	if iconCacheName("cursor") != iconCacheName("cursor") {
		t.Fatal("同一 id 的缓存名必须稳定")
	}
	if iconCacheName("cursor") == iconCacheName("zed") {
		t.Fatal("不同 id 的缓存名不应相同")
	}
}

func TestIconStoreRebuildsWhenSourceChanges(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS 图标提取只在 darwin 上有效")
	}
	if _, err := exec.LookPath("iconutil"); err != nil {
		t.Skip("缺少 iconutil")
	}
	if _, err := exec.LookPath("sips"); err != nil {
		t.Skip("缺少 sips")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	app := filepath.Join(home, "Applications", "Fake.app")
	resources := filepath.Join(app, "Contents", "Resources")
	if err := os.MkdirAll(resources, 0o755); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleIconFile</key><string>icon</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	icns := filepath.Join(resources, "icon.icns")
	writeICNS(t, icns, color.RGBA{R: 220, G: 60, B: 60, A: 255})

	store := newIconStore(filepath.Join(home, "cache"))
	spec := catalog.Spec{ID: "fake-icon", Name: "Fake", Custom: true, Bundles: []string{"Fake.app"}}
	first, ok := store.ensure(spec)
	if !ok || !validPNG(first) {
		t.Fatalf("首次生成失败：path=%q ok=%v", first, ok)
	}
	firstBytes, _ := os.ReadFile(first)

	// 模拟缓存写坏：换一个新 store（等价于重启进程），应当自动重建。
	if err := os.WriteFile(first, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	store = newIconStore(filepath.Join(home, "cache"))
	second, ok := store.ensure(spec)
	if !ok || !validPNG(second) {
		t.Fatal("损坏的缓存没有自动重建")
	}

	// 源 .icns 变了，新进程应生成不同的 PNG。
	writeICNS(t, icns, color.RGBA{R: 60, G: 80, B: 220, A: 255})
	store = newIconStore(filepath.Join(home, "cache"))
	third, ok := store.ensure(spec)
	if !ok || !validPNG(third) {
		t.Fatal("源图标更新后没有重新生成")
	}
	thirdBytes, _ := os.ReadFile(third)
	if bytes.Equal(firstBytes, thirdBytes) {
		t.Fatal("源图标更新后缓存字节没有变化")
	}
}

func writeICNS(t *testing.T, path string, fill color.RGBA) {
	t.Helper()
	iconset := path + ".iconset"
	if err := os.MkdirAll(iconset, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]int{
		"icon_16x16.png":      16,
		"icon_16x16@2x.png":   32,
		"icon_32x32.png":      32,
		"icon_32x32@2x.png":   64,
		"icon_128x128.png":    128,
		"icon_128x128@2x.png": 256,
		"icon_256x256.png":    256,
		"icon_256x256@2x.png": 512,
		"icon_512x512.png":    512,
		"icon_512x512@2x.png": 1024,
	}
	for name, size := range files {
		img := image.NewRGBA(image.Rect(0, 0, size, size))
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				img.Set(x, y, fill)
			}
		}
		file, err := os.Create(filepath.Join(iconset, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, img); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("iconutil", "-c", "icns", iconset, "-o", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("iconutil 失败：%v\n%s", err, out)
	}
}
