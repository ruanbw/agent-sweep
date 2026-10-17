package clean

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbw/agent-sweep/internal/catalog"
	"github.com/ruanbw/agent-sweep/internal/fsx"
	"github.com/ruanbw/agent-sweep/internal/session"
)

// TestDeleteAcceptsPathsExplainedByDeclaration 覆盖 Store.Glob + 相对 Assoc：
// 会话正文与同名旁挂目录都在声明范围内时正常删除。
func TestDeleteAcceptsPathsExplainedByDeclaration(t *testing.T) {
	f := newFixture(t, ModePermanent)
	body := filepath.Join(f.home, "sessions", "a.jsonl")
	assoc := filepath.Join(f.home, "sessions", "a")
	writeTestFile(t, body, "body")
	writeTestFile(t, filepath.Join(assoc, "tool.json"), "tool")

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "jsonl", Glob: "sessions/*.jsonl", Assoc: []string{"{id}"},
		}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("a", body, assoc)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("声明内的路径不该报错: %+v", res.Errors)
	}
	if res.Count != 1 {
		t.Fatalf("Count = %d，want 1", res.Count)
	}
	if fsx.Exists(body) || fsx.Exists(assoc) {
		t.Fatal("声明内的正文/旁挂应被删除")
	}
}

// TestDeleteAcceptsAbsoluteAssoc 覆盖以 `/` 开头的 Assoc：按数据根拼。
func TestDeleteAcceptsAbsoluteAssoc(t *testing.T) {
	f := newFixture(t, ModePermanent)
	body := filepath.Join(f.home, "sessions", "a.jsonl")
	assoc := filepath.Join(f.home, "file-history", "a")
	writeTestFile(t, body, "body")
	writeTestFile(t, filepath.Join(assoc, "v1"), "history")

	spec := catalog.Spec{
		ID: "demo",
		Stores: []catalog.Store{{
			Kind: "jsonl", Glob: "sessions/*.jsonl", Assoc: []string{"/file-history/{id}"},
		}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("a", body, assoc)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}
	if fsx.Exists(body) || fsx.Exists(assoc) {
		t.Fatal("声明内的正文/绝对 Assoc 应被删除")
	}
}

// TestDeleteRejectsUndeclaredPath 是核心回归：Paths 里混进一条声明解释不了的
// 路径时，整条会话都不能删，已声明的那些也保持原样。
func TestDeleteRejectsUndeclaredPath(t *testing.T) {
	f := newFixture(t, ModePermanent)
	body := filepath.Join(f.home, "sessions", "a.jsonl")
	stray := filepath.Join(f.home, "other", "b.jsonl")
	writeTestFile(t, body, "body")
	writeTestFile(t, stray, "stray")

	spec := catalog.Spec{
		ID:     "demo",
		Stores: []catalog.Store{{Kind: "jsonl", Glob: "sessions/*.jsonl"}},
	}
	res, err := f.cleaner.Delete(spec, f.home, []session.Session{fileSession("a", body, stray)})
	if err != nil {
		t.Fatalf("Delete 不应因单条路径被拒而返回致命错误: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("声明解释不了的会话不应计入 Count，got %d", res.Count)
	}
	if len(res.Errors) == 0 {
		t.Fatal("必须有 ItemError 说明原因")
	}
	message := res.Errors[0].Message
	for _, want := range []string{"无法由 Agent demo", "b.jsonl"} {
		if !strings.Contains(message, want) {
			t.Errorf("错误信息缺少 %q：%s", want, message)
		}
	}
	// 一条都没删：已声明的正文也必须还在。
	if !fsx.Exists(body) || !fsx.Exists(stray) {
		t.Fatal("整条会话被拒时不能删掉任何一条路径")
	}
}

// TestDeleteSkipsDeclarationCheckWithoutStores：没有 Stores 的 spec（旧适配器/
// 测试夹具）保持旧行为，不因缺少声明而拒删。
func TestDeleteSkipsDeclarationCheckWithoutStores(t *testing.T) {
	f := newFixture(t, ModePermanent)
	body := filepath.Join(f.home, "sessions", "a.jsonl")
	writeTestFile(t, body, "body")

	res, err := f.cleaner.Delete(catalog.Spec{ID: "demo"}, f.home,
		[]session.Session{fileSession("a", body)})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(res.Errors) != 0 || res.Count != 1 {
		t.Fatalf("Count = %d, Errors = %+v", res.Count, res.Errors)
	}
	if fsx.Exists(body) {
		t.Fatal("没有 Stores 时仍应按旧行为删除")
	}
}
