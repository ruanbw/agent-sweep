package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一组测试钉住的全是「真实数据里出现过、看起来像 bug」的行为。
// 改动解析启发式时它们应该一起红，否则说明你在改一个没人测过的假设。

func TestSystemPromptIsNotTitle(t *testing.T) {
	// Pi 的形状：第一条是几万字的 system，真正的用户消息在后面。
	head := []byte(`{"type":"message","message":{"role":"system","content":"You are an expert coding assistant. <tools>…</tools>"}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"帮我看一下这台机子有哪些垃圾可以清理"}]}}
`)
	meta := ParseJSONL(head)
	if meta.Title != "帮我看一下这台机子有哪些垃圾可以清理" {
		t.Fatalf("标题应是用户消息，实际 %q", meta.Title)
	}
}

func TestNestedDSHShape(t *testing.T) {
	// DSH：消息裹在 data.inserted[].content[] 里，顶层没有 role。
	head := []byte(`{"type":"agent/inbox/spliced","data":{"inserted":[{"role":"user","content":[{"type":"text","text":"把 bulwark 删掉"}]}]}}`)
	meta := ParseJSONL(head)
	if meta.Title != "把 bulwark 删掉" {
		t.Fatalf("嵌套结构没抽出来：%q", meta.Title)
	}
}

func TestMillisecondTimestamp(t *testing.T) {
	meta := ParseJSONL([]byte(`{"type":"session","cwd":"/tmp/x","timestamp":1792119045599}`))
	if meta.Project != "/tmp/x" {
		t.Errorf("project = %q", meta.Project)
	}
	if meta.Time.Year() != 2026 {
		t.Errorf("毫秒时间戳解析错了：%v", meta.Time)
	}
}

func TestTruncatedJSONDoesNotPanic(t *testing.T) {
	// 会话文件正在被写时读到的就是这种半截内容，解析必须活着。
	meta := ParseJSONObject([]byte(`{"title":"半截","messages":[{"role":"user","content":"he`))
	if meta.Title != "半截" {
		t.Fatalf("截断的 JSON 也应尽量给出标题，实际 %q", meta.Title)
	}
}

func TestReadMetaEscalatesForLongSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	// 造一份 200KB 的 system 提示词，把用户消息推到首轮 64KB 之外。
	filler := strings.Repeat("x", 200*1024)
	content := `{"type":"message","message":{"role":"system","content":"` + filler + `"}}` + "\n" +
		`{"type":"message","message":{"role":"user","content":"真正的标题"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 首轮 64KB 抽不到，ReadMeta 必须自动多读一轮。
	if meta := ParseJSONL(readN(t, path, 64*1024)); meta.Title != "" {
		t.Fatalf("夹具无效：64KB 内不该有标题，实际 %q", meta.Title)
	}
	if meta := ReadMeta(path, true); meta.Title != "真正的标题" {
		t.Fatalf("按需扩容后应抽到标题，实际 %q", meta.Title)
	}
}

func readN(t *testing.T, path string, n int64) []byte {
	t.Helper()
	data, err := readHead(path, n)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
