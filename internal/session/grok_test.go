package session

import (
	"os"
	"path/filepath"
	"testing"
)

// Grok 的目录型会话布局：<workspace>/<id>/summary.json 存标题，正文是同目录的
// updates.jsonl（ACP 事件流，只有增量更新，不带标题字段）。
const (
	grokSessionID    = "2f1c9a4e-7b3d-4c8a-9e21-6d5f0a1b2c3d"
	grokSessionUUID8 = "2f1c9a4e"
	grokSummaryTitle = "帮我排查 ACK 超时"
)

// writeGrokSessionDir 造一个 Grok 会话目录；withSummary 控制是否有 summary.json。
func writeGrokSessionDir(t *testing.T, withSummary bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), grokSessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// ACP 事件流只用于证明「正文在这个目录里、但它不提供标题」。
	updates := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + grokSessionID + `","update":{"sessionUpdate":"user_message_chunk"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + grokSessionID + `","update":{"sessionUpdate":"agent_message_chunk"}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), []byte(updates), 0o644); err != nil {
		t.Fatalf("WriteFile updates.jsonl: %v", err)
	}

	if withSummary {
		summary := `{"title":"` + grokSummaryTitle + `","conversationId":"` + grokSessionID +
			`","updatedAt":"2026-10-17T10:03:05Z"}`
		if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(summary), 0o644); err != nil {
			t.Fatalf("WriteFile summary.json: %v", err)
		}
	}
	return dir
}

// TestReadMetaDirReadsGrokSummary 回归「Note 说了标题在 summary.json，但候选名单
// 里没有这个文件名」。缺了它，目录型会话即使有 summary.json，标题也只会退化成
// 「会话 <uuid8>」。
func TestReadMetaDirReadsGrokSummary(t *testing.T) {
	dir := writeGrokSessionDir(t, true)

	meta := ReadMetaDir(dir)
	if meta.Title != grokSummaryTitle {
		t.Errorf("Title = %q，期望读 summary.json 里的 title", meta.Title)
	}
	if meta.Title == "会话 "+grokSessionUUID8 {
		t.Errorf("Title 退化成 uuid 短码 %q：summary.json 没被认出来", meta.Title)
	}
}

// TestReadMetaDirDoesNotInventGrokTitle 钉住反面：目录里只有 updates.jsonl 时，
// 不能从 ACP 事件流里瞎猜标题 —— 宁可空着，让上层用「会话 <uuid8>」兜底。
func TestReadMetaDirDoesNotInventGrokTitle(t *testing.T) {
	dir := writeGrokSessionDir(t, false)

	meta := ReadMetaDir(dir)
	if meta.Title != "" {
		t.Errorf("没有 summary.json 时不该凭空猜标题，got %q", meta.Title)
	}
}
