package scan

import (
	"errors"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// R3：Store 迭代到一半失败时，已经解析出来的会话必须留下。
// scanSQLite 的结尾是 `return out, rows.Err()`，所以「有 items + 有 err」是
// 真实会发生的组合（库被锁 / IO 错误 / 库损坏），不是构造出来的假设。

func TestMergeStoreKeepsPartialItemsOnError(t *testing.T) {
	result := AgentResult{}
	seen := map[string]bool{}
	items := []session.Session{
		{ID: "a", Title: "第一条", Bytes: 10},
		{ID: "b", Title: "第二条", Bytes: 20},
	}

	mergeStore(&result, seen, items, errors.New("database is locked"))

	if len(result.Sessions) != 2 {
		t.Fatalf("err 非空时 items 不应被丢弃，实际保留 %d 条", len(result.Sessions))
	}
	if result.Sessions[0].ID != "a" || result.Sessions[1].ID != "b" {
		t.Errorf("会话顺序变了：%+v", result.Sessions)
	}
	if result.TotalBytes != 30 {
		t.Errorf("TotalBytes 应累加保留的会话，实际 %d", result.TotalBytes)
	}
	if result.Err != "database is locked" {
		t.Errorf("错误应被记录，实际 %q", result.Err)
	}
}

func TestMergeStoreAccumulatesErrors(t *testing.T) {
	result := AgentResult{}
	seen := map[string]bool{}

	// 两个 Store 各自「读出一部分 + 报错」，错误和 items 都不能互相覆盖。
	mergeStore(&result, seen, []session.Session{{ID: "a", Bytes: 1}}, errors.New("库被锁"))
	mergeStore(&result, seen, []session.Session{{ID: "b", Bytes: 2}}, errors.New("库损坏"))

	if !strings.Contains(result.Err, "库被锁") || !strings.Contains(result.Err, "库损坏") {
		t.Fatalf("两条错误都应保留，实际 %q", result.Err)
	}
	if result.Err != "库被锁; 库损坏" {
		t.Errorf("错误应按声明顺序用 \"; \" 拼接，实际 %q", result.Err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("两次 partial 结果都应保留，实际 %d 条", len(result.Sessions))
	}
	if result.TotalBytes != 3 {
		t.Errorf("TotalBytes = %d，期望 3", result.TotalBytes)
	}
}

func TestMergeStoreNoErrorKeepsDedupAndBytes(t *testing.T) {
	result := AgentResult{}
	seen := map[string]bool{}

	mergeStore(&result, seen, []session.Session{
		{ID: "a", Title: "先到", Bytes: 5},
		{ID: "b", Title: "乙", Bytes: 7},
	}, nil)
	// 同一个 sessionId 在后一个 Store 里再出现：先到先得，标题和 Bytes 都取第一个。
	mergeStore(&result, seen, []session.Session{
		{ID: "a", Title: "后到", Bytes: 100},
		{ID: "c", Title: "丙", Bytes: 3},
	}, nil)

	if result.Err != "" {
		t.Fatalf("无错误路径不应写入 Err，实际 %q", result.Err)
	}
	if len(result.Sessions) != 3 {
		t.Fatalf("期望去重后 3 条，实际 %d", len(result.Sessions))
	}
	if result.Sessions[0].Title != "先到" {
		t.Errorf("去重应保留先到的那条，实际标题 %q", result.Sessions[0].Title)
	}
	if result.TotalBytes != 15 {
		t.Errorf("TotalBytes 只算去重后保留的会话，实际 %d（期望 5+7+3）", result.TotalBytes)
	}
}

// One 这一层也要确认错误不会被后一个 Store 覆盖：用一个必然失败的未知形态，
// 不依赖真实 SQLite 故障。
func TestOneAggregatesStoreErrors(t *testing.T) {
	root := t.TempDir()
	spec := catalog.Spec{
		ID: "broken", Name: "Broken",
		Roots: []string{root},
		Stores: []catalog.Store{
			{Kind: "bogus-a"},
			{Kind: "bogus-b"},
		},
	}

	result := One(spec)
	if !result.Installed {
		t.Fatal("数据根存在，应判定为已安装")
	}
	if !strings.Contains(result.Err, `未知的存储形态 "bogus-a"`) ||
		!strings.Contains(result.Err, `未知的存储形态 "bogus-b"`) {
		t.Fatalf("两个 Store 的错误都应保留，实际 %q", result.Err)
	}
}
