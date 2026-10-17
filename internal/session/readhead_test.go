package session

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一组测试钉住 R23：readHead 用单次 Read 读文件，把「短读」当成了 EOF。
//
// 事故背景：Read 的契约允许一次只返回一部分字节（管道、网络卷、加密卷、
// 某些 FUSE 都这样），n < limit 不等于文件结束。旧代码只看 len(head) < limit
// 就判定读完，于是外层 64KB→256KB→1MB 的扩容根本没发生，标题也静默退回
// fallbackTitle（用户看到「未命名会话」）。同时每轮扩容都重新 make 一次
// 缓冲区，最坏 1MB/次 × 16 并发。
//
// 这里用 shortReader 复现「一次只给几个字节」的 reader，钉住
// readHeadFrom 会读到文件真正结束为止；不再需要全局 hook。

// shortReader 每次 Read 最多给 step 个字节，但不返回 io.EOF，
// 用来模拟真实世界里的短读。数据耗尽后才返回 io.EOF。
type shortReader struct {
	data []byte
	step int
}

func (r *shortReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.step
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// errReader 立刻失败，用来钉住错误必须向上返回。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadHeadLargeFileFillsLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")

	// 标题夹在 200KB 之后、总长超过 256KB：既证明读满了 limit，
	// 也证明 256KB 容量内的标题能正常解析出来。
	var b strings.Builder
	b.WriteString(`{"type":"message","message":{"role":"system","content":"`)
	b.WriteString(strings.Repeat("x", 200*1024))
	b.WriteString(`"}}` + "\n")
	b.WriteString(`{"type":"message","message":{"role":"user","content":"大文件里的标题"}}` + "\n")
	b.WriteString(`{"type":"message","message":{"role":"system","content":"`)
	b.WriteString(strings.Repeat("y", 100*1024))
	b.WriteString(`"}}` + "\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 200*1024 {
		t.Fatalf("夹具无效：文件应大于 200KB，实际 %d", info.Size())
	}

	const limit = 256 * 1024
	head, err := readHead(path, limit)
	if err != nil {
		t.Fatalf("读大文件失败：%v", err)
	}
	if int64(len(head)) != limit {
		t.Fatalf("应读满 %d 字节，实际 %d", limit, len(head))
	}
	if meta := ParseJSONL(head); meta.Title != "大文件里的标题" {
		t.Fatalf("读满 limit 后标题应可解析，实际 %q", meta.Title)
	}
}

func TestReadHeadFromFillsLimitDespiteShortReads(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 1000)

	// 先确认夹具真的会短读：单次 Read 只能拿到 step 个字节。
	probe := &shortReader{data: data, step: 7}
	buf := make([]byte, len(data))
	if n, _ := probe.Read(buf); n != 7 {
		t.Fatalf("夹具无效：单次 Read 应只返回 7 字节，实际 %d", n)
	}

	r := &shortReader{data: data, step: 7}
	head, err := readHeadFrom(r, int64(len(data)))
	if err != nil {
		t.Fatalf("短读不应报错：%v", err)
	}
	if len(head) != len(data) {
		t.Fatalf("短读被当成 EOF 了：应读到 %d 字节，实际 %d", len(data), len(head))
	}
	if !bytes.Equal(head, data) {
		t.Fatal("短读读到的内容不完整")
	}
}

func TestReadHeadFromShortDataReadsToEOF(t *testing.T) {
	data := []byte("短文件的内容")
	head, err := readHeadFrom(&shortReader{data: data, step: 3}, 64*1024)
	if err != nil {
		t.Fatalf("文件比 limit 短不是错误：%v", err)
	}
	if !bytes.Equal(head, data) {
		t.Fatalf("应返回全部内容，实际 %q", head)
	}
}

func TestReadHeadEmptyFileIsNotError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	head, err := readHead(path, 64*1024)
	if err != nil {
		t.Fatalf("空文件不应返回错误（也不能变成 EOF）：%v", err)
	}
	if len(head) != 0 {
		t.Fatalf("空文件应返回 0 字节，实际 %d", len(head))
	}

	// 同样的语义要覆盖 open 之外的那层：reader 直接给 EOF。
	head, err = readHeadFrom(strings.NewReader(""), 64*1024)
	if err != nil {
		t.Fatalf("空 reader 不应返回错误：%v", err)
	}
	if len(head) != 0 {
		t.Fatalf("空 reader 应返回 0 字节，实际 %d", len(head))
	}
}

func TestReadHeadSmallFileReturnsAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.jsonl")
	data := []byte(`{"title":"小文件"}` + "\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	head, err := readHead(path, 64*1024)
	if err != nil {
		t.Fatalf("读小文件失败：%v", err)
	}
	if !bytes.Equal(head, data) {
		t.Fatalf("应返回全部内容，实际 %q", head)
	}
}

func TestReadHeadFromPropagatesRealError(t *testing.T) {
	if _, err := readHeadFrom(errReader{}, 16); err == nil {
		t.Fatal("真实 I/O 错误必须向上返回")
	}
}

func TestReadMetaParsesTempJSONL(t *testing.T) {
	// 回归：真实临时文件上的标题 / 项目解析保持正确（不碰用户目录）。
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := strings.Join([]string{
		`{"type":"session","cwd":"/tmp/agent-sweep-fixture","timestamp":1792119045599}`,
		`{"type":"message","message":{"role":"user","content":"清理一下这台机子"}}`,
		`{"type":"message","message":{"role":"assistant","content":"好的"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	meta := ReadMeta(path, true)
	if meta.Title != "清理一下这台机子" {
		t.Fatalf("标题解析错误：%q", meta.Title)
	}
	if meta.Project != "/tmp/agent-sweep-fixture" {
		t.Fatalf("项目解析错误：%q", meta.Project)
	}
	if meta.Time.Year() != 2026 {
		t.Fatalf("时间解析错误：%v", meta.Time)
	}
}
