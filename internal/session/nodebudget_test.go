package session

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 这一组测试钉住 R26：解析会话头部时的 JSON 遍历必须有共享的节点预算。
//
// 事故背景：deepUserText / deepTitle 只有深度上限（8 / 6），没有节点数上限。
// 一条深宽但合法的记录（1MB 单行导出、畸形嵌套）会让单条会话做 4 次全树
// 遍历；扫描器同时跑 16 个文件 worker + 6 个 Agent 时，CPU/内存直接尖峰，
// 扫描长时间无响应。预算耗尽只能「少抽点」：不能 panic、不能报错，也不能
// 改变「解析失败退回 fallback」的既有语义。

func TestWideRecordReturnsQuickly(t *testing.T) {
	// 5 万个相邻字段：JSON 合法、单行很大，解析必须在线性时间内结束。
	var b strings.Builder
	b.WriteString(`{"id":"wide-1","cwd":"/tmp/wide","timestamp":1792119045599`)
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, `,"a%d":"x"`, i)
	}
	b.WriteString("}\n")

	start := time.Now()
	meta := ParseJSONL([]byte(b.String()))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("宽记录解析耗时 %v，超过 2s 上限", elapsed)
	}
	if meta.Project != "/tmp/wide" {
		t.Fatalf("Project = %q, want /tmp/wide（基本字段不应受宽度影响）", meta.Project)
	}
	if meta.Time.Year() != 2026 {
		t.Fatalf("Time = %v，毫秒时间戳解析不应受宽度影响", meta.Time)
	}
}

func TestDeepRecordReturnsWithoutPanic(t *testing.T) {
	// 500 层嵌套：parse 本身不能 panic；遍历必须在深度上限处停住。
	const depth = 500
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"data":`)
	}
	b.WriteString(`{"role":"user","content":"藏在 500 层里"}`)
	b.WriteString(strings.Repeat("}", depth))
	head := []byte(b.String())

	if meta := ParseJSONObject(head); meta.Title != "" {
		t.Fatalf("超过深度上限的标题不应被抽出，实际 %q", meta.Title)
	}
	if meta := ParseJSONL(append(head, '\n')); meta.Title != "" {
		t.Fatalf("ParseJSONL 也不应越过深度上限，实际 %q", meta.Title)
	}
}

func TestNormalClaudeCodeRecordUnchanged(t *testing.T) {
	// 普通 Claude Code 形状：预算不能把正常的标题 / snippet / 时间 / 计数改坏。
	head := []byte(strings.Join([]string{
		`{"type":"user","cwd":"/Users/ruanbw/projects/agent-sweep","timestamp":"2026-10-17T09:30:00.000Z","message":{"role":"user","content":[{"type":"text","text":"修一下 README 里过期的链接"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"我先核对一下。"}]}}`,
	}, "\n") + "\n")

	meta := ParseJSONL(head)

	if meta.Title != "修一下 README 里过期的链接" {
		t.Fatalf("Title = %q, want %q", meta.Title, "修一下 README 里过期的链接")
	}
	if meta.Snippet != "修一下 README 里过期的链接" {
		t.Fatalf("Snippet = %q, want %q", meta.Snippet, "修一下 README 里过期的链接")
	}
	if meta.Project != "/Users/ruanbw/projects/agent-sweep" {
		t.Fatalf("Project = %q", meta.Project)
	}
	wantTime := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	if !meta.Time.Equal(wantTime) {
		t.Fatalf("Time = %v, want %v", meta.Time, wantTime)
	}
	if meta.Messages != 2 {
		t.Fatalf("Messages = %d, want 2", meta.Messages)
	}
}

func TestBudgetBoundsVisitedNodes(t *testing.T) {
	// 一棵 1 万节点的宽树：没有预算时 deepUserText 会把它们全部走完。
	items := make([]any, 10000)
	for i := range items {
		items[i] = map[string]any{"data": "x"}
	}

	// 显式预算：访问节点数必须封顶，且同一份预算不能在函数之间重置。
	const limit = 25
	b := &budget{left: limit}
	if got := deepUserText(items, 0, b); got != "" {
		t.Fatalf("这棵树里没有用户正文，结果应为空，实际 %q", got)
	}
	if b.used > limit {
		t.Fatalf("访问了 %d 个节点，超过预算 %d", b.used, limit)
	}
	if b.used != limit || b.left != 0 {
		t.Fatalf("预算应恰好耗尽：used=%d left=%d，want used=%d left=0", b.used, b.left, limit)
	}

	before := b.used
	if got := deepTitle(items, 0, b); got != "" {
		t.Fatalf("预算耗尽后 deepTitle 不应再抽出内容，实际 %q", got)
	}
	if b.used != before {
		t.Fatalf("同一份已耗尽预算下 deepTitle 又访问了节点：%d -> %d", before, b.used)
	}

	// 默认预算下，即使树远大于预算，访问数也必须封顶。
	full := newBudget()
	_ = deepUserText(items, 0, full)
	if full.used > mergeNodeBudget {
		t.Fatalf("默认预算下访问了 %d 个节点，超过 %d", full.used, mergeNodeBudget)
	}
	if full.left < 0 {
		t.Fatalf("预算余额不能为负：%d", full.left)
	}
	if full.used != mergeNodeBudget {
		t.Fatalf("1 万节点应耗尽默认预算：used=%d, want %d", full.used, mergeNodeBudget)
	}
}
