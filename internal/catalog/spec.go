// Package catalog 是「本机有哪些 Agent、它们的会话存在哪、长什么样」的唯一事实来源。
//
// 设计上的关键取舍：**不写死一份 Agent 名单**。
//
// 市面上每周都在冒新的 Agent，把 30 个 `switch` 分支写进代码，等于每出一款就改一次
// 源码、发一次版。这里把「一个 Agent」压成一份声明（Spec）：数据根、会话文件长什么样、
// 索引在哪。扫描引擎只认这份声明，不认 Agent 名字 —— 于是新增一款 Agent 是往
// builtin.go 加一条数据，或者让用户在「发现」页点一下保存，完全不用碰引擎。
//
// 声明表达不了的东西才写代码（比如 Claude Code 的 sessions-index.json 有四种形状），
// 那种归 internal/scan 里的专用解析器。
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Group 决定侧栏分组，只影响展示。
type Group string

const (
	GroupCLI       Group = "cli"       // 命令行 Agent
	GroupIDE       Group = "ide"       // IDE 里的会话
	GroupApp       Group = "app"       // 独立桌面应用
	GroupExtension Group = "extension" // 编辑器扩展
	GroupOther     Group = "other"     // 指纹发现出来的未知 Agent
)

// Spec 一款 Agent 的完整声明。
type Spec struct {
	ID      string   `json:"id"`      // 稳定标识，落盘进设置与日志，不要改
	Name    string   `json:"name"`    // 显示名
	Group   Group    `json:"group"`   // 侧栏分组
	Vendor  string   `json:"vendor"`  // 厂商，仅用于展示与去重提示
	Bundles []string `json:"bundles"` // 可能的 .app 名，按优先级

	// Roots 候选数据根，`~` 开头。第一个存在的即为 Home。
	// 多个候选是为了兼容版本迁移（如 ~/.aider 与 ~/.config/aider）。
	Roots []string `json:"roots"`
	// Env 覆盖数据根的环境变量名，按顺序取第一个非空的。
	Env []string `json:"env"`

	Stores []Store `json:"stores"`
	Indexs []Index `json:"indexes"`

	// Note 是给用户看的一句话说明（删除时会改哪些索引、有什么坑）。
	Note string `json:"note,omitempty"`
	// Custom 表示这条来自用户自己保存的适配器，而不是内置目录。
	Custom bool `json:"custom,omitempty"`
}

// Store 描述「一条会话」落在磁盘上的形态。
//
// 只有四种形态，覆盖了目前见过的全部 Agent：
//   - jsonl:   一个 .jsonl 文件 = 一条会话（Claude Code / Codex / Pi）
//   - json:    一个 .json 文件 = 一条会话（Gemini CLI / Continue）
//   - dir:     一个子目录 = 一条会话（Cline 的 tasks/<id>/）
//   - sqlite:  会话是表里的一行（Zed / opencode / Antigravity）
//
// 解析只取头部 64KB，标题 / 项目路径 / 时间戳都在前几行 —— 扫全量是纯浪费。
type Store struct {
	Kind string `json:"kind"`
	// Glob 相对 Roots 的模式，支持 `*` 与 `**`。空表示根目录本身。
	Glob string `json:"glob,omitempty"`
	// SQLite 形态专用
	DB       string `json:"db,omitempty"`    // 相对 Roots 的库文件
	Table    string `json:"table,omitempty"` // 表名
	IDCol    string `json:"idCol,omitempty"` // 主键列
	TitleCol string `json:"titleCol,omitempty"`
	TimeCol  string `json:"timeCol,omitempty"` // 通常是 int64 毫秒，也可能是 ISO 字符串
	CWDCol   string `json:"cwdCol,omitempty"`
	// Rows 是「删这条会话还要连带删哪些表」的补充，格式 `table:fkcol`。
	// 没写就只删主表一行；写了就连带清理，避免留下孤儿子消息。
	Rows []string `json:"rows,omitempty"`
	// Assoc 是与会话同名的旁挂目录/文件名（相对会话文件所在目录），
	// 比如 Claude Code 的 `file-history/<id>`。删除时一起进废纸篓。
	Assoc []string `json:"assoc,omitempty"`
	// Compress 表示会话正文**整体**是压缩流，不是逐行压缩。目前只支持 "" 与
	// "zstd"：扫描时只解前 1MB 左右抽元数据，删除仍是整文件/整目录删除，不需要
	// 解压。TRAE CLI 的 rollout-*.jsonl.zst、Reasonix 1.x 的 events.frames 用它。
	Compress string `json:"compress,omitempty"`
}

// Index 描述「删了正文之后还要裁掉的索引」。
//
// 只删正文不裁索引 = Agent 侧留下一批点进去是空的幽灵会话，比不删还糟。
type Index struct {
	Kind string `json:"kind"` // jsonl-lines | json-array | json-dict | sqlite-rows
	Glob string `json:"glob"` // 相对 Roots
	// Key 是索引里指向 sessionId 的字段名（jsonl / json 形态）。
	Key string `json:"key,omitempty"`
	// Path 是 json-dict 里边包一层的键，如 `sessions`；空表示顶层字典。
	Path string `json:"path,omitempty"`
	// SQLite 形态
	DB    string `json:"db,omitempty"`
	Table string `json:"table,omitempty"`
	IDCol string `json:"idCol,omitempty"`
}

// Home 返回第一个存在的候选数据根；都不存在时返回空串。
// env 用于测试注入，nil 表示读进程环境。
func (s *Spec) Home(env func(string) string) string {
	if env == nil {
		env = os.Getenv
	}
	for _, key := range s.Env {
		if v := env(key); v != "" {
			return expand(v)
		}
	}
	for _, root := range s.Roots {
		p := expand(root)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// RootPaths 返回全部候选根的真实路径（不判断存在），供 UI 展示与「打开目录」。
func (s *Spec) RootPaths(env func(string) string) []string {
	if env == nil {
		env = os.Getenv
	}
	var out []string
	for _, key := range s.Env {
		if v := env(key); v != "" {
			out = append(out, expand(v))
		}
	}
	for _, root := range s.Roots {
		out = append(out, expand(root))
	}
	return out
}

// Installed 只看数据根在不在 —— 不猜「用户有没有装」。
//
// 有些 Agent 把数据放在 `~/Library/Application Support/<Vendor>`，只要目录在就算装过；
// 空目录也照样列出，因为「有目录但零会话」本身是有用信息（说明清干净了）。
func (s *Spec) Installed(env func(string) string) bool {
	return s.Home(env) != ""
}

func expand(p string) string {
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return os.ExpandEnv(p)
}

// Expand 暴露给其它包用（用户自定义适配器里也会写 `~/...`）。
func Expand(p string) string { return expand(p) }

// ValidateDataRoot 判断一个「数据根」是否可信。
//
// 数据根优先由环境变量决定：Home 命中 Env 就直接返回变量值，不做任何路径
// 收敛。删除侧又以这个根作为「路径必须落在根内」的边界，扫描侧则从它开始
// 遍历 —— 所以根一旦是文件系统根、卷根，或用户主目录本身，护栏就形同虚设：
// / 之下什么路径都「在根内」，等于没有边界。误配的 CLAUDE_CONFIG_DIR=/、
// 被脚本污染的 CODEX_HOME=~，都会把删除变成对整棵树开口子，因此凡是拿根
// 做包含性校验的调用方（clean / scan）都必须先过这里。
//
// 不检查存在性：根暂时不存在是正常的（还没用过、外置盘没挂上）。删除会在
// 打开库/文件时因 IO 失败报错，扫描本就允许根暂时为空。
func ValidateDataRoot(home string) error {
	if home == "" {
		return fmt.Errorf("数据根为空")
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("数据根不是绝对路径: %s", home)
	}
	clean := filepath.Clean(home)
	// 卷根（含 /）的特征是「父目录就是自己」。
	if filepath.Dir(clean) == clean {
		return fmt.Errorf("数据根不能是文件系统根或卷根: %s", clean)
	}
	if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
		// 两边都 Clean 再比：结尾多一条斜杠不该让校验漏判。
		if filepath.Clean(userHome) == clean {
			return fmt.Errorf("数据根不能是用户主目录: %s", clean)
		}
	}
	return nil
}
