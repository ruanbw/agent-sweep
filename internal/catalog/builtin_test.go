package catalog

import (
	"path/filepath"
	"strings"
	"testing"
)

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

// TestDevinCLIDoesNotTreatXDGDataHomeAsDataRoot 钉住 Home 的 Env 语义。
//
// Spec.Home 命中 Env 时直接把变量值当数据根返回，不做拼接；而 XDG_DATA_HOME
// 是父目录（普通机器上是 ~/.local/share）。把它列进 Env，用户一旦设置该变量，
// 数据根就变成 ~/.local/share，Store 被解析成 ~/.local/share/cli/sessions.db，
// 少了 devin/ 一段 —— 扫描永远 0 条且不报错，用户只会以为「没装 Devin CLI」。
// 其余条目的 Env 都是目录级变量（CLAUDE_CONFIG_DIR / CODEX_HOME …），只有这条
// 曾经是父级，所以单独钉一条用例。
func TestDevinCLIDoesNotTreatXDGDataHomeAsDataRoot(t *testing.T) {
	const xdgDataHome = "/tmp/xdg-data"

	specs := Builtin()
	var spec *Spec
	for i := range specs {
		if specs[i].ID == "devin-cli" {
			spec = &specs[i]
			break
		}
	}
	if spec == nil {
		t.Fatal("内置目录里没有 devin-cli，测试需随声明更新")
	}

	for _, key := range spec.Env {
		if key == "XDG_DATA_HOME" {
			t.Fatalf("devin-cli 的 Env 又包含了 XDG_DATA_HOME：它是父目录，被当成数据根会丢掉 devin/ 一段")
		}
	}

	// 注入一个「用户设置了 XDG_DATA_HOME」的环境，看 Home 会不会被带偏。
	got := spec.Home(func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdgDataHome
		}
		return ""
	})
	for _, forbidden := range []string{xdgDataHome, filepath.Join(xdgDataHome, "cli")} {
		if got == forbidden {
			t.Fatalf("Home 把 XDG_DATA_HOME 当成了数据根：got %q", got)
		}
	}
	// 只允许两种结果：本机没装（空串），或 Roots 里 ~/.local/share/devin 的
	// 展开值。不硬断具体路径，因为后者取决于本机是否真的有这个目录。
	if wantRoot := expand("~/.local/share/devin"); got != "" && got != wantRoot {
		t.Fatalf("Home = %q，只允许空串或 Roots 展开值 %q", got, wantRoot)
	}
}

// TestCodeBuddyIDECLIConfigsAreSplit 钉住「IDE 与 CLI 必须两条声明」。
//
// Spec.Home 只返回第一个存在的根，所以把两个数据根塞进同一条 Spec 时，只有
// 排在前面的那个能被扫描到 —— 本机 IDE 根存在，CLI 的 projects/*/*.jsonl 就
// 永远在 IDE 根下找。这不是扫描引擎的 bug，是声明必须按数据根拆开，先例是
// trae / trae-cn 和 cursor / cursor-agent-cli。
//
// 断言分两层：Roots 各管一边（防止再把 ~/.codebuddy 塞回 IDE 那条），
// 以及 Store glob 各归其位（防止有人把两条 Store 合并回去 —— 合并后即使
// Roots 拆开，总有一条 Store 会在错误的根下空转）。
func TestCodeBuddyIDECLIConfigsAreSplit(t *testing.T) {
	specs := Builtin()
	var ide, cli *Spec
	for i := range specs {
		switch specs[i].ID {
		case "codebuddy":
			ide = &specs[i]
		case "codebuddy-cli":
			cli = &specs[i]
		}
	}
	if ide == nil {
		t.Fatal("内置目录里没有 codebuddy（IDE 版），测试需随声明更新")
	}
	if cli == nil {
		t.Fatal("内置目录里没有 codebuddy-cli：CLI 会话会重新变成扫不到")
	}
	if ide.Group != GroupIDE {
		t.Errorf("codebuddy 的分组 = %q，want %q", ide.Group, GroupIDE)
	}
	if cli.Group != GroupCLI {
		t.Errorf("codebuddy-cli 的分组 = %q，want %q", cli.Group, GroupCLI)
	}

	for _, root := range ide.Roots {
		if root == "~/.codebuddy" {
			t.Errorf("IDE 版的 Roots 里又混进了 CLI 根 ~/.codebuddy：Home 只取第一个存在的根，CLI 会被压住")
		}
	}
	if len(cli.Roots) != 1 || cli.Roots[0] != "~/.codebuddy" {
		t.Errorf("codebuddy-cli 的 Roots = %v，want 只有 [~/.codebuddy]", cli.Roots)
	}

	// 每条 Store 的 glob 都要落在自己的根下：IDE 走 VS Code fork 的
	// workspaceStorage，CLI 走 Claude Code 同构的 projects/。
	if len(ide.Stores) == 0 {
		t.Error("codebuddy 没有 Store")
	}
	for _, store := range ide.Stores {
		if !strings.HasPrefix(store.Glob, "User/workspaceStorage") {
			t.Errorf("IDE 版的 Store glob = %q，应以 User/workspaceStorage 开头（CLI 的 projects/ 属于 codebuddy-cli）", store.Glob)
		}
	}
	if len(cli.Stores) == 0 {
		t.Error("codebuddy-cli 没有 Store")
	}
	for _, store := range cli.Stores {
		if !strings.HasPrefix(store.Glob, "projects/") {
			t.Errorf("CLI 版的 Store glob = %q，应以 projects/ 开头（IDE 的 chatSessions 属于 codebuddy）", store.Glob)
		}
	}
}
