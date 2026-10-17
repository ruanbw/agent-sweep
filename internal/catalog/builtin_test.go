package catalog

import "testing"

// 内置目录的「结构约束」测试。目录靠人手加条目，最容易犯的两类错
// 单看代码都像对的：ID 重名（保存设置时会互相覆盖）和必填字段漏写
// （会变成扫不到东西但不报错的空条目）。
func TestBuiltinEntriesAreWellFormed(t *testing.T) {
	specs := Builtin()
	if len(specs) == 0 {
		t.Fatal("内置目录为空")
	}
	seenID := map[string]bool{}
	seenName := map[string]bool{}
	for _, spec := range specs {
		if spec.ID == "" {
			t.Errorf("%s: ID 不能为空", spec.Name)
		}
		if spec.Name == "" {
			t.Errorf("%s: Name 不能为空", spec.ID)
		}
		if seenID[spec.ID] {
			t.Errorf("ID 重复：%s", spec.ID)
		}
		seenID[spec.ID] = true
		if seenName[spec.Name] {
			t.Errorf("Name 重复：%s", spec.Name)
		}
		seenName[spec.Name] = true

		switch spec.Group {
		case GroupCLI, GroupIDE, GroupApp, GroupExtension, GroupOther:
		default:
			t.Errorf("%s: 未知分组 %q", spec.ID, spec.Group)
		}
		if len(spec.Roots) == 0 && len(spec.Env) == 0 {
			t.Errorf("%s: Roots 与 Env 不能都为空，否则永远判为未安装", spec.ID)
		}
		if len(spec.Stores) == 0 {
			t.Errorf("%s: 没有 Store，永远扫不到会话", spec.ID)
		}
		for _, store := range spec.Stores {
			switch store.Kind {
			case "jsonl", "json", "dir":
				if store.Glob == "" {
					t.Errorf("%s: %s Store 缺少 Glob", spec.ID, store.Kind)
				}
			case "sqlite":
				if store.DB == "" || store.Table == "" {
					t.Errorf("%s: sqlite Store 缺少 DB 或 Table", spec.ID)
				}
			default:
				t.Errorf("%s: 未知 Store 形态 %q", spec.ID, store.Kind)
			}
		}
	}
}

// 一款 Agent 的 ID 会写进设置、日志和用户自定义适配器，改名会丢配置。
// 这条测试把「当前已发布的 ID」钉住，避免无意改名。
func TestBuiltinIDsAreStable(t *testing.T) {
	// 早期版本就有的 ID：任何一条消失都说明发生了不兼容改动。
	legacy := []string{
		"claude-code", "codex", "gemini-cli", "qwen-code", "opencode", "pi", "dsh",
		"continue", "cline", "roo-code", "kilo-code", "vscode", "cursor", "windsurf",
		"trae", "kiro", "qoder", "zed", "antigravity", "openhands", "openviking",
		"goose", "amp", "crush", "copilot-cli", "codebuddy", "workbuddy", "trae-cn",
		"cursor-nightly", "windsurf-next", "vscode-insiders", "vscodium",
		"antigravity-ide", "cody", "augment", "kimi-cli", "iflow",
	}
	have := map[string]bool{}
	for _, spec := range Builtin() {
		have[spec.ID] = true
	}
	for _, id := range legacy {
		if !have[id] {
			t.Errorf("内置 ID %q 消失了：它可能已写进用户的设置与适配器", id)
		}
	}
}
