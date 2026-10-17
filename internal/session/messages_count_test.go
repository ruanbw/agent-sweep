package session

import (
	"strings"
	"testing"
)

// 这一组测试钉住 Messages 的语义：**一条记录最多计一条对话消息**。
//
// 事故背景：Claude Code 的 JSONL 是
// {"type":"user","message":{"role":"user","content":…}}，type 和
// message.role 同时命中；早期 merge 里两个 if 各自 ++，一条消息被数两遍，
// 消息数直接翻倍。meta_test.go 里没有任何 Messages 断言，所以没被发现。
//
// 夹具大多故意不带 content：ParseJSONL 拿到标题且数过 3 条就提前停
// （meta.go 的 early-stop），带正文会把断言的条数截断，那测的就不是
// 计数逻辑了。需要真实形状的地方单独写，并保证条数落在提前停的边界内。

func jsonlLines(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestMessagesClaudeCodeShapeCountsOnce(t *testing.T) {
	// 真实 Claude Code 形状：顶层 type 与 message.role 同时存在，各 3 条。
	head := jsonlLines(
		`{"type":"user","message":{"role":"user","content":"把 README 里过期的链接修掉"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"我先核对一下这些链接。"}}`,
		`{"type":"user","message":{"role":"user","content":"顺便看看 LICENSE"}}`,
	)
	meta := ParseJSONL(head)
	if meta.Messages != 3 {
		t.Fatalf("Messages = %d, want 3：role 和 type 同时命中只能计一条", meta.Messages)
	}
}

func TestMessagesRoleOnlyShape(t *testing.T) {
	// 其它 Agent 形状：只有 role，没有 type。
	head := jsonlLines(
		`{"role":"user"}`,
		`{"role":"assistant"}`,
		`{"role":"user"}`,
		`{"role":"assistant"}`,
	)
	if got := ParseJSONL(head).Messages; got != 4 {
		t.Fatalf("Messages = %d, want 4", got)
	}
}

func TestMessagesTypeOnlyShape(t *testing.T) {
	// Pi / Gemini CLI 一类：只有 type，没有 role（早期实现靠第二个 if 才数到）。
	head := jsonlLines(
		`{"type":"user"}`,
		`{"type":"assistant"}`,
		`{"type":"user"}`,
	)
	if got := ParseJSONL(head).Messages; got != 3 {
		t.Fatalf("Messages = %d, want 3", got)
	}
}

func TestMessagesIgnoresNonChatTypes(t *testing.T) {
	// 非对话类型绝不能计数：system 是提示词，tool / summary 是元信息。
	// 另外 "message" 只是容器类型，Pi 这种形状要靠里面的 role 计数。
	head := jsonlLines(
		`{"type":"system"}`,
		`{"type":"tool"}`,
		`{"type":"summary"}`,
		`{"type":"session"}`,
		`{"type":"notification"}`,
		`{"type":"message","message":{"role":"user"}}`,
		`{"type":"user"}`,
		`{"type":"assistant"}`,
	)
	if got := ParseJSONL(head).Messages; got != 3 {
		t.Fatalf("Messages = %d, want 3：只应计 1 条 role=user 与 2 条 type 对话消息", got)
	}
}

func TestMessagesRoleAndTypeSameLevelCountOnce(t *testing.T) {
	// role 和 type 平铺在同一层时同样只能计一条。
	head := jsonlLines(
		`{"role":"user","type":"user"}`,
		`{"role":"assistant","type":"assistant"}`,
	)
	if got := ParseJSONL(head).Messages; got != 2 {
		t.Fatalf("Messages = %d, want 2", got)
	}
}

func TestMessagesMixedShapes(t *testing.T) {
	// 同一份 JSONL 里混三种形状，人工数出来 9 条对话消息：
	// Claude Code 双字段 2 条 + 只有 role 2 条 + 只有 type 3 条 + 平铺双字段 2 条；
	// 另有 3 条非对话记录（system / tool / summary）必须不计。
	head := jsonlLines(
		`{"type":"user","message":{"role":"user"}}`,           // 1
		`{"type":"assistant","message":{"role":"assistant"}}`, // 2
		`{"role":"user"}`,                         // 3
		`{"role":"assistant"}`,                    // 4
		`{"type":"user"}`,                         // 5
		`{"type":"assistant"}`,                    // 6
		`{"type":"user"}`,                         // 7
		`{"role":"assistant","type":"assistant"}`, // 8
		`{"role":"user","type":"user"}`,           // 9
		`{"type":"system"}`,                       // 不计
		`{"type":"tool"}`,                         // 不计
		`{"type":"summary"}`,                      // 不计
	)
	if got := ParseJSONL(head).Messages; got != 9 {
		t.Fatalf("Messages = %d, want 9（混排总数应等于人工数出来的条数）", got)
	}
}
