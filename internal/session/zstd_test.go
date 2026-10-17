package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zstdFixture 是一条最小的会话记录：标题来自第一条 user 消息。
const zstdFixture = `{"type":"message","message":{"role":"user","content":"帮我排查超时"}}` + "\n"

// writeZstdPlain 把 plain 压成 zstd 写到 path（父目录自动创建）。
func writeZstdPlain(t *testing.T, path, plain string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writer, err := zstd.NewWriter(file)
	if err != nil {
		file.Close()
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := writer.Write([]byte(plain)); err != nil {
		writer.Close()
		file.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		t.Fatalf("writer.Close: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file.Close: %v", err)
	}
}

// TestReadMetaZstdJSONL 覆盖文件型压缩会话（TRAE CLI 的 rollout-*.jsonl.zst）。
func TestReadMetaZstdJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl.zst")
	writeZstdPlain(t, path, zstdFixture)

	meta := ReadMetaZstdJSONL(path)
	if meta.Title != "帮我排查超时" {
		t.Fatalf("Title = %q，期望从 zstd 解出的 JSONL 里抽到标题", meta.Title)
	}
}

// TestReadMetaZstdJSONLDoesNotPanic 钉住坏输入：截断的流、非 zstd 字节、
// 不存在的文件都只返回空 Meta。
func TestReadMetaZstdJSONLDoesNotPanic(t *testing.T) {
	dir := t.TempDir()

	// 只保留 zstd magic（前 4 字节）：解不出任何完整记录。
	full := filepath.Join(dir, "full.zst")
	writeZstdPlain(t, full, strings.Repeat(zstdFixture, 50))
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	truncated := filepath.Join(dir, "truncated.zst")
	if err := os.WriteFile(truncated, data[:4], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if meta := ReadMetaZstdJSONL(truncated); meta.Title != "" {
		t.Errorf("截断的 zstd 流不该抽出标题，got %q", meta.Title)
	}

	garbage := filepath.Join(dir, "garbage.zst")
	if err := os.WriteFile(garbage, []byte("not a zstd stream"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if meta := ReadMetaZstdJSONL(garbage); meta.Title != "" {
		t.Errorf("非 zstd 字节不该抽出标题，got %q", meta.Title)
	}

	if meta := ReadMetaZstdJSONL(filepath.Join(dir, "missing.zst")); meta.Title != "" {
		t.Errorf("不存在的文件不该抽出标题，got %q", meta.Title)
	}
}

// TestReadMetaZstdDirPrefersEventsFrames 覆盖目录型压缩会话（Reasonix 1.x）。
// 隐藏文件故意放在排序更前面（以 . 开头），用来证明选择逻辑不是「随便取第一个」。
func TestReadMetaZstdDirPrefersEventsFrames(t *testing.T) {
	dir := t.TempDir()
	writeZstdPlain(t, filepath.Join(dir, ".hidden.frames"), `{"title":"隐藏文件标题"}`+"\n")
	writeZstdPlain(t, filepath.Join(dir, "events.frames"), zstdFixture)

	meta := ReadMetaZstdDir(dir)
	if meta.Title != "帮我排查超时" {
		t.Fatalf("Title = %q，期望优先读取 events.frames", meta.Title)
	}
}

// TestReadMetaZstdDirEdgeCases：只有隐藏文件 / 空目录都返回空 Meta。
func TestReadMetaZstdDirEdgeCases(t *testing.T) {
	hiddenOnly := t.TempDir()
	writeZstdPlain(t, filepath.Join(hiddenOnly, ".hidden.frames"), `{"title":"隐藏文件标题"}`+"\n")
	if meta := ReadMetaZstdDir(hiddenOnly); meta.Title != "" {
		t.Errorf("只有隐藏文件时不该选中它，got %q", meta.Title)
	}

	empty := t.TempDir()
	if meta := ReadMetaZstdDir(empty); meta.Title != "" {
		t.Errorf("空目录不该抽出标题，got %q", meta.Title)
	}
}
