package catalog

// builtin 是内置目录。每条都只描述「数据在哪、长什么样」，不含任何扫描逻辑。
//
// 新增一款 Agent 的标准动作是往这里加一条；如果它的格式已经超出四种 Store 形态，
// 才需要动 internal/scan。这样目录能跟着市场长，引擎保持稳定。
//
// 说明：这里的路径来自各 Agent 的实际落盘布局。写错的代价很低 —— 路径不存在时
// 该 Agent 只是显示「未安装」，不会误删任何东西；但写漏的代价是用户找不到会话。
// 所以宁可多列几个候选根（Roots 支持多个）。

func jsonlStore(glob string, assoc ...string) Store {
	return Store{Kind: "jsonl", Glob: glob, Assoc: assoc}
}

func jsonStore(glob string) Store {
	return Store{Kind: "json", Glob: glob}
}

func dirStore(glob string) Store {
	return Store{Kind: "dir", Glob: glob}
}

func sqliteStore(db, table, id, title, timeCol, cwd string, rows ...string) Store {
	return Store{
		Kind: "sqlite", DB: db, Table: table,
		IDCol: id, TitleCol: title, TimeCol: timeCol, CWDCol: cwd, Rows: rows,
	}
}

func jsonlIndex(glob, key string) Index {
	return Index{Kind: "jsonl-lines", Glob: glob, Key: key}
}

func jsonIndex(glob, key, path string) Index {
	return Index{Kind: "json-array", Glob: glob, Key: key, Path: path}
}

func sqliteIndex(db, table, id string) Index {
	return Index{Kind: "sqlite-rows", DB: db, Table: table, IDCol: id}
}

// Builtin 返回内置目录。顺序即侧栏顺序（组内）。
func Builtin() []Spec {
	return []Spec{
		{
			ID: "claude-code", Name: "Claude Code", Group: GroupCLI, Vendor: "Anthropic",
			Bundles: []string{"Claude Code.app", "Claude.app"},
			Roots:   []string{"~/.claude"}, Env: []string{"CLAUDE_CONFIG_DIR"},
			Stores: []Store{
				jsonlStore("projects/*/*.jsonl", "{id}", "/file-history/{id}", "/plans/{id}", "/session-env/{id}"),
				jsonlStore("sessions/*.jsonl"),
			},
			Indexs: []Index{
				jsonlIndex("history.jsonl", "sessionId"),
				jsonIndex("projects/*/sessions-index.json", "sessionId", ""),
				jsonIndex("sessions-index.json", "sessionId", ""),
			},
			Note: "删除会同步裁剪 history.jsonl 与 sessions-index.json，否则留下幽灵会话。",
		},
		{
			ID: "codex", Name: "Codex", Group: GroupCLI, Vendor: "OpenAI",
			Bundles: []string{"Codex.app", "ChatGPT.app"},
			Roots:   []string{"~/.codex"}, Env: []string{"CODEX_HOME"},
			Stores: []Store{jsonlStore("sessions/**/*.jsonl")},
			Indexs: []Index{jsonlIndex("history.jsonl", "session_id")},
			Note:   "会话按日期分层存放在 sessions/YYYY/MM/DD/。",
		},
		{
			ID: "gemini-cli", Name: "Gemini CLI", Group: GroupCLI, Vendor: "Google",
			Roots: []string{"~/.gemini"}, Env: []string{"GEMINI_CLI_HOME"},
			Stores: []Store{
				jsonStore("tmp/*/chats/*.json"),
				jsonStore("tmp/*/chats/recording-*.json"),
			},
			Note: "会话在 tmp/<项目哈希>/chats/ 下。",
		},
		{
			ID: "qwen-code", Name: "Qwen Code", Group: GroupCLI, Vendor: "Alibaba",
			Roots:  []string{"~/.qwen"},
			Stores: []Store{jsonStore("tmp/*/chats/*.json")},
		},
		{
			ID: "opencode", Name: "OpenCode", Group: GroupCLI, Vendor: "SST",
			Bundles: []string{"OpenCode.app"},
			Roots:   []string{"~/.local/share/opencode", "~/Library/Application Support/opencode"},
			Env:     []string{"OPENCODE_DATA_DIR"},
			Stores: []Store{
				// message 用 session_id 外键，能一条语句带走；part 挂在 message 上，
				// 两段式级联表达不了，会留下孤立的 part 行（体积不归这里计）。
				sqliteStore("opencode.db", "session", "id", "title", "time_updated", "directory",
					"message:session_id"),
			},
			Note: "会话与消息都在 SQLite 里，删除是删行 + 备份整库，不进废纸篓。",
		},
		{
			ID: "pi", Name: "Pi Agent", Group: GroupCLI, Vendor: "Pi",
			Bundles: []string{"Pi Agent.app", "Pi.app"},
			Roots:   []string{"~/.pi/agent", "~/.pi"}, Env: []string{"PI_HOME"},
			Stores: []Store{jsonlStore("sessions/*/*.jsonl")},
			Note:   "sessions/<项目>/<时间戳>_<id>.jsonl。",
		},
		{
			ID: "dsh", Name: "DeepSeek Harness", Group: GroupCLI, Vendor: "DeepSeek",
			Bundles: []string{"DeepSeek Harness.app"},
			Roots:   []string{"~/.dsh"}, Env: []string{"DSH_HOME"},
			Stores: []Store{dirStore("sessions/*/*")},
			Note:   "每个会话是一个目录，正文是 session.v4.jsonl.zstd（压缩）。",
		},
		{
			ID: "continue", Name: "Continue", Group: GroupExtension, Vendor: "Continue",
			Bundles: []string{"Visual Studio Code.app"},
			Roots:   []string{"~/.continue"}, Env: []string{"CONTINUE_GLOBAL_DIR"},
			Stores: []Store{jsonStore("sessions/*.json")},
		},
		{
			ID: "cline", Name: "Cline", Group: GroupExtension, Vendor: "Cline",
			Roots:  []string{"~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev"},
			Stores: []Store{dirStore("tasks/*")},
			Note:   "每个任务一个目录，正文在 api_conversation_history.json。",
		},
		{
			ID: "roo-code", Name: "Roo Code", Group: GroupExtension, Vendor: "Roo",
			Roots: []string{
				"~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline",
				"~/Library/Application Support/Cursor/User/globalStorage/rooveterinaryinc.roo-cline",
			},
			Stores: []Store{dirStore("tasks/*")},
		},
		{
			ID: "kilo-code", Name: "Kilo Code", Group: GroupExtension, Vendor: "Kilo",
			Roots:  []string{"~/Library/Application Support/Code/User/globalStorage/kilocode.kilo-code"},
			Stores: []Store{dirStore("tasks/*")},
		},
		{
			ID: "vscode", Name: "VS Code Chat", Group: GroupIDE, Vendor: "Microsoft",
			Bundles: []string{"Visual Studio Code.app"},
			Roots:   []string{"~/Library/Application Support/Code/User"}, Env: []string{"VSCODE_USER_DATA"},
			Stores: []Store{jsonStore("workspaceStorage/*/chatSessions/*.json")},
			Indexs: []Index{sqliteIndex("globalStorage/state.vscdb", "", "")},
			Note:   "chatSessions 是正文，state.vscdb 里是索引；删文件后索引会留空条目。",
		},
		{
			ID: "cursor", Name: "Cursor", Group: GroupIDE, Vendor: "Anysphere",
			Bundles: []string{"Cursor.app"},
			Roots:   []string{"~/Library/Application Support/Cursor"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
			Note:    "Cursor 同时维护 JSONL 与 state.vscdb 两套索引，版本之间布局会变。",
		},
		{
			ID: "windsurf", Name: "Windsurf", Group: GroupIDE, Vendor: "Codeium",
			Bundles: []string{"Windsurf.app"},
			Roots:   []string{"~/Library/Application Support/Windsurf"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "trae", Name: "Trae", Group: GroupIDE, Vendor: "ByteDance",
			Bundles: []string{"Trae.app"},
			Roots:   []string{"~/Library/Application Support/Trae"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "kiro", Name: "Kiro", Group: GroupIDE, Vendor: "AWS",
			Bundles: []string{"Kiro.app"},
			Roots:   []string{"~/Library/Application Support/Kiro"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "qoder", Name: "Qoder", Group: GroupIDE, Vendor: "Alibaba",
			Bundles: []string{"Qoder.app"},
			Roots:   []string{"~/Library/Application Support/Qoder"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "zed", Name: "Zed AI", Group: GroupIDE, Vendor: "Zed",
			Bundles: []string{"Zed.app"},
			Roots:   []string{"~/Library/Application Support/Zed"}, Env: []string{"ZED_HOME"},
			Stores: []Store{
				sqliteStore("threads/threads.db", "threads", "id", "summary", "updated_at", "folder_paths"),
			},
			Note: "线程正文在 threads.db 的 data 列（BLOB）里，删除只能删行并备份整库。",
		},
		{
			ID: "antigravity", Name: "Antigravity", Group: GroupIDE, Vendor: "Google",
			Bundles: []string{"Antigravity.app"},
			Roots:   []string{"~/.gemini/antigravity"}, Env: []string{"ANTIGRAVITY_HOME"},
			Stores: []Store{
				{
					Kind: "sqlite", DB: "conversation_summaries.db", Table: "conversation_summaries",
					IDCol: "conversation_id", TitleCol: "title",
					TimeCol: "last_modified_time", CWDCol: "workspace_uris",
					// 正文不在库里，在 brain/<id> 与 conversations/<id> 下；
					// 只删摘要行会留下一堆再也看不到的正文目录。
					Assoc: []string{"/brain/{id}", "/conversations/{id}"},
				},
			},
			Note: "摘要库有 FTS 索引表，删行后需要重建索引才不会搜出空会话。",
		},
		{
			ID: "openhands", Name: "OpenHands", Group: GroupCLI, Vendor: "All Hands",
			Roots:  []string{"~/.openhands", "~/.open-devin"},
			Stores: []Store{dirStore("sessions/*")},
		},
		{
			ID: "openviking", Name: "OpenViking", Group: GroupCLI, Vendor: "OpenViking",
			Roots:  []string{"~/.openviking"},
			Stores: []Store{jsonlStore("sessions/*.jsonl")},
		},
		{
			ID: "goose", Name: "Goose", Group: GroupCLI, Vendor: "Block",
			Bundles: []string{"Goose.app"},
			Roots:   []string{"~/.local/share/goose", "~/.config/goose"},
			Stores:  []Store{jsonlStore("sessions/*.jsonl")},
		},
		{
			ID: "amp", Name: "Amp", Group: GroupCLI, Vendor: "Sourcegraph",
			Roots:  []string{"~/.local/share/amp", "~/.config/amp"},
			Stores: []Store{jsonStore("threads/*.json")},
		},
		{
			ID: "crush", Name: "Crush", Group: GroupCLI, Vendor: "Charm",
			Roots:  []string{"~/.local/share/crush", "~/.config/crush"},
			Stores: []Store{dirStore("sessions/*"), jsonStore("sessions/*.json")},
		},
		{
			ID: "copilot-cli", Name: "Copilot CLI", Group: GroupCLI, Vendor: "GitHub",
			Roots:  []string{"~/.copilot"},
			Stores: []Store{jsonlStore("history-session-state/*.jsonl"), dirStore("session-state/*")},
		},
		{
			ID: "codebuddy", Name: "CodeBuddy", Group: GroupIDE, Vendor: "Tencent",
			Bundles: []string{"CodeBuddy.app"},
			Roots:   []string{"~/Library/Application Support/CodeBuddy", "~/.codebuddy"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "workbuddy", Name: "WorkBuddy", Group: GroupApp, Vendor: "Tencent",
			Bundles: []string{"WorkBuddy.app", "WorkBuddy AI.app"},
			Roots:   []string{"~/.workbuddy", "~/.workbuddy-ai"},
			Stores:  []Store{dirStore("conversations/*"), jsonStore("sessions/*.json")},
			Note:    "目录结构随版本变化，未命中时会出现在「发现」页。",
		},
		{
			// 同一款 IDE 的稳定版 / 预览版 / 国内版数据目录是并列的，
			// 分开列而不是写成通配：一旦合并，侧栏会把两个版本算成一个 Agent，
			// 用户无法只清其中一个。
			ID: "trae-cn", Name: "Trae CN", Group: GroupIDE, Vendor: "ByteDance",
			Bundles: []string{"Trae CN.app", "Trae.app"},
			Roots:   []string{"~/Library/Application Support/Trae CN"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "cursor-nightly", Name: "Cursor Nightly", Group: GroupIDE, Vendor: "Anysphere",
			Bundles: []string{"Cursor Nightly.app"},
			Roots:   []string{"~/Library/Application Support/Cursor Nightly"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "windsurf-next", Name: "Windsurf Next", Group: GroupIDE, Vendor: "Codeium",
			Bundles: []string{"Windsurf - Next.app"},
			Roots:   []string{"~/Library/Application Support/Windsurf - Next"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "vscode-insiders", Name: "VS Code Insiders", Group: GroupIDE, Vendor: "Microsoft",
			Bundles: []string{"Visual Studio Code - Insiders.app"},
			Roots:   []string{"~/Library/Application Support/Code - Insiders/User"},
			Stores:  []Store{jsonStore("workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "vscodium", Name: "VSCodium", Group: GroupIDE, Vendor: "VSCodium",
			Bundles: []string{"VSCodium.app"},
			Roots:   []string{"~/Library/Application Support/VSCodium/User"},
			Stores:  []Store{jsonStore("workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "antigravity-ide", Name: "Antigravity IDE", Group: GroupIDE, Vendor: "Google",
			Bundles: []string{"Antigravity IDE.app"},
			Roots:   []string{"~/.gemini/antigravity-ide"},
			Stores:  []Store{jsonStore("User/workspaceStorage/*/chatSessions/*.json")},
		},
		{
			ID: "cody", Name: "Cody", Group: GroupExtension, Vendor: "Sourcegraph",
			Roots: []string{
				"~/Library/Application Support/Code/User/globalStorage/sourcegraph.cody-ai",
				"~/Library/Application Support/Cursor/User/globalStorage/sourcegraph.cody-ai",
			},
			Stores: []Store{dirStore("globalStorage/sourcegraph.cody-ai/*")},
		},
		{
			ID: "augment", Name: "Augment", Group: GroupExtension, Vendor: "Augment",
			Roots: []string{
				"~/Library/Application Support/Code/User/globalStorage/augment.vscode-augment",
			},
			Stores: []Store{jsonStore("globalStorage/augment.vscode-augment/*.json")},
		},
		{
			ID: "kimi-cli", Name: "Kimi CLI", Group: GroupCLI, Vendor: "Moonshot",
			Roots:  []string{"~/.kimi", "~/.moonshot"},
			Stores: []Store{jsonStore("sessions/*.json"), jsonlStore("sessions/*.jsonl")},
		},
		{
			ID: "iflow", Name: "iFlow CLI", Group: GroupCLI, Vendor: "iFlow",
			Roots:  []string{"~/.iflow"},
			Stores: []Store{jsonStore("tmp/*/chats/*.json"), jsonlStore("sessions/*.jsonl")},
		},
	}
}
