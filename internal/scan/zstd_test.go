package scan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// writeZstdSession 把 plain 压成 zstd 写到 path（父目录自动创建）。
func writeZstdSession(t *testing.T, path, plain string) {
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

// TestOneScansCompressedJSONL 覆盖文件型压缩会话：Store 声明 Compress 后，
// glob 命中 .jsonl.zst 也要能抽出标题，ID 取文件名里的 uuid 段。
func TestOneScansCompressedJSONL(t *testing.T) {
	const id = "fdd5b96d-853c-480d-8fa5-ba5d0cf992d3"
	home := t.TempDir()
	writeZstdSession(t, filepath.Join(home, "sessions", "p1", "rollout-"+id+".jsonl.zst"),
		`{"type":"message","message":{"role":"user","content":"帮我排查 ACK 超时"}}`+"\n")

	spec := catalog.Spec{
		ID:    "trae-cli",
		Name:  "TRAE CLI",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "jsonl", Glob: "sessions/*/*.jsonl.zst", Compress: "zstd",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("扫描不该报错: %s", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions = %d，want 1", len(result.Sessions))
	}
	session := result.Sessions[0]
	if session.Title != "帮我排查 ACK 超时" {
		t.Errorf("Title = %q，期望从压缩 JSONL 里抽到标题", session.Title)
	}
	if session.ID != id {
		t.Errorf("ID = %q，want 文件名里的 uuid %q", session.ID, id)
	}
}

// TestOneScansCompressedDir 覆盖目录型压缩会话（Reasonix 的 events.frames）。
func TestOneScansCompressedDir(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions-v4", "s1")
	writeZstdSession(t, filepath.Join(dir, "events.frames"),
		`{"type":"message","message":{"role":"user","content":"帮我看看这个报错"}}`+"\n")
	// 干扰项：隐藏文件排在 events.frames 之前，不能被选中。
	writeZstdSession(t, filepath.Join(dir, ".lock"), `{"title":"不该被读"}`+"\n")

	spec := catalog.Spec{
		ID:    "reasonix",
		Name:  "Reasonix",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "dir", Glob: "sessions-v4/*", Compress: "zstd",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("扫描不该报错: %s", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions = %d，want 1", len(result.Sessions))
	}
	if got := result.Sessions[0].Title; got != "帮我看看这个报错" {
		t.Errorf("Title = %q，期望从 events.frames 抽到标题", got)
	}
}

// TestOnePlainJSONLWithoutCompress 是回归对照：没声明 Compress 时仍走原来的
// 明文解析路径，压缩逻辑不能影响普通会话。
func TestOnePlainJSONLWithoutCompress(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "sessions", "plain.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path,
		[]byte(`{"type":"message","message":{"role":"user","content":"明文的标题"}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	spec := catalog.Spec{
		ID:    "plain",
		Name:  "Plain",
		Roots: []string{home},
		Stores: []catalog.Store{{
			Kind: "jsonl", Glob: "sessions/*.jsonl",
		}},
	}
	result := One(spec)
	if result.Err != "" {
		t.Fatalf("扫描不该报错: %s", result.Err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions = %d，want 1", len(result.Sessions))
	}
	if got := result.Sessions[0].Title; got != "明文的标题" {
		t.Errorf("Title = %q，want 明文的标题", got)
	}
}
