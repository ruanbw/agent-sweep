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
			Stores: []Store{
				// v0.99 起的真实布局：sessions/<项目>/<时间戳>_<id>/<agent-id>/run-N/session.jsonl。
				// 一条会话是那个 <agent-id> 目录（run-N 是它的重跑），所以按 dir 收，
				// 体积也算到整个目录头上。早先声明的 `sessions/*/*.jsonl` 在新布局下
				// 一条都匹配不到 —— 保留它是为了兼容还没升级的老版本落盘。
				// 第三层限定成 uuid：同一层还并排放着 `subagent-artifacts/`（子代理的
				// 输入输出留档），它不是一个会话，用 `*` 会把它当成一条会话收进来。
				dirStore("sessions/*/*/[0-9a-f]*-[0-9a-f]*-[0-9a-f]*-[0-9a-f]*-[0-9a-f]*"),
				jsonlStore("sessions/*/*.jsonl"),
			},
			Note: "sessions/<项目>/<时间戳>_<id>/<agent>/run-N/session.jsonl；一个 <agent> 目录即一条会话。",
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
			Stores: []Store{
				jsonStore("User/workspaceStorage/*/chatSessions/*.json"),
				// CodeBuddy Code（CLI）与 Claude Code 同构：projects/<mangled-cwd>/<id>.jsonl，
				// 子代理在同一层的 <id>/subagents/agent-*.jsonl。IDE 版是 VS Code fork
				// 的 chatSessions，两者互不影响，所以两条 Store 都要留着。
				jsonlStore("projects/*/*.jsonl"),
			},
			Note: "IDE 版在 workspaceStorage/chatSessions，CLI 版在 projects/ 下（Claude Code 同构）。",
		},
		{
			ID: "workbuddy", Name: "WorkBuddy", Group: GroupApp, Vendor: "Tencent",
			Bundles: []string{"WorkBuddy.app", "WorkBuddy AI.app"},
			Roots:   []string{"~/.workbuddy", "~/.workbuddy-ai"},
			Stores: []Store{
				// 真实布局（5.8.x 实测）：projects/<mangled-cwd>/<session-id>.jsonl，
				// 同名的 <session-id>/ 目录里是 tool-results 等旁挂产物，
				// <session-id>.file-rollback.ndjson 是文件回滚日志 —— 三者同删。
				// 早先声明的 conversations/* 与 sessions/*.json 在这些版本里都不存在。
				jsonlStore("projects/*/*.jsonl", "{id}", "{id}.file-rollback.ndjson"),
			},
			// 会话索引：标题、状态、创建/更新时间都在这张表里。
			//
			// 必须是 Index 而不是 Store：Store 只参与扫描，删正文之后没人会
			// 回头裁这张表，下一轮扫描它又作为一条「会话」冒出来 —— 用户得删
			// 两次，第二次删的还是没有正文的空壳。Index 才会在删除流程里按
			// 已删 ID 裁行，并与正文删除一起进 journal 供撤销。
			Indexs: []Index{sqliteIndex("workbuddy.db", "sessions", "id")},
			Note:   "正文是 projects/<项目>/<会话>.jsonl；workbuddy.db 里另有一份会话索引，删除会一并裁掉。",
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

		// —— 2026-10 补充收录（CLI / 桌面客户端）——
		//
		// 这批的共同点：会话形态都落在现有四种 Store 内，不需要动扫描引擎。
		// 路径来自各项目的上游源码与独立实现的会话格式注册表（deja-vu 会话格式
		// registry、jazzyalex/agent-sessions），不是凭印象写的；本机没装的条目
		// 无法实测，但写错只会显示「未安装」，不会误删。
		//
		// 一条铁律：**CLI 与它的 IDE 版是两套数据根**。Spec.Home 只取第一个存在
		// 的根，把两者塞进同一条会让用户没法只清其中一个 —— 所以数据根不同的
		// 一律拆开，跟 trae / trae-cn 的处理一致。
		{
			ID: "trae-cli", Name: "TRAE CLI", Group: GroupCLI, Vendor: "ByteDance",
			Roots: []string{"~/.trae"}, Env: []string{"TRAE_HOME"},
			Stores: []Store{
				// rollout-*.jsonl 与 Codex 的 rollout 同构，标题解析可直接复用。
				// 后缀 .jsonl.zst 的变体暂不支持：引擎还没有通用的解压路径。
				jsonlStore("cli/sessions/**/rollout-*.jsonl"),
				jsonlStore("cli/archived_sessions/**/rollout-*.jsonl"),
			},
			Indexs: []Index{jsonlIndex("cli/history.jsonl", "session_id")},
			Note:   "与 Trae IDE 是两套数据；cli/ 下是 Codex rollout 同构的 JSONL。",
		},
		{
			ID: "grok", Name: "Grok Build", Group: GroupCLI, Vendor: "xAI",
			Roots: []string{"~/.grok"}, Env: []string{"GROK_HOME"},
			// sessions/<workspace>/<id>/ 里是 summary.json（标题）+ updates.jsonl
			// （ACP 事件流）。按目录收，一条会话 = 一个目录，两个文件一起走。
			Stores: []Store{dirStore("sessions/*/*")},
			Note:   "标题在 summary.json；正文是 ACP 事件流 updates.jsonl。",
		},
		{
			ID: "devin-cli", Name: "Devin CLI", Group: GroupCLI, Vendor: "Cognition",
			// Roots 不用 XDG_DATA_HOME 兜底：Spec.Home 里 Env 的语义是「变量值就是
			// 数据根」，而 XDG_DATA_HOME 是父目录（~/.local/share）。列进 Env 之后，
			// 用户一旦设置它，库会被解析成 <XDG_DATA_HOME>/cli/sessions.db —— 少了
			// devin/ 一段，永远扫到 0 条，而且不报错，看起来像「没装」。其它条目的
			// Env 都是 CLAUDE_CONFIG_DIR / CODEX_HOME 这种目录级变量，没有这个歧义。
			Roots: []string{"~/.local/share/devin"},
			// 一会话一行 + 5 张以 session_id 为外键的子表，正好落在 Rows 能力内。
			// 老版本的库名是 cli_sessions.db，但这里只声明当前版本的 cli/sessions.db：
			// 缺库时 deleteSQLiteSessions 会先给该 Spec 的每条会话记一条
			// 「库文件不存在」的 backupErr，于是整款 Agent 的删除全部失败。盲列
			// 第二条换不来兼容，只会让升级过的机器彻底删不动 —— 老库名按「当前版本
			// 不支持」处理，等拿到实机样本确认后再补。
			Stores: []Store{
				sqliteStore("cli/sessions.db", "sessions", "id", "title", "last_activity_at", "working_directory",
					"message_nodes:session_id", "prompt_history:session_id",
					"rendered_commits:session_id", "tool_call_state:session_id",
					"subagent_heads:session_id"),
			},
			Note: "删行 + 备份整库；子代理的消息在 subagent_heads 里另记，已一并带上。",
		},
		{
			ID: "junie", Name: "Junie", Group: GroupCLI, Vendor: "JetBrains",
			Roots: []string{"~/.junie"}, Env: []string{"JUNIE_HOME"},
			Stores: []Store{dirStore("sessions/*")},
			Indexs: []Index{jsonlIndex("sessions/index.jsonl", "id")},
			Note:   "一个会话一个目录，正文是 events.jsonl；sessions/index.jsonl 是索引。",
		},
		{
			ID: "mistral-vibe", Name: "Mistral Vibe", Group: GroupCLI, Vendor: "Mistral",
			Roots: []string{"~/.vibe"}, Env: []string{"VIBE_HOME"},
			Stores: []Store{dirStore("logs/session/*")},
			Note:   "logs/session/<id>/ 是一份 transcript。",
		},
		{
			// 与 pi 同构（扁平根），解析完全共享。
			ID: "kimchi", Name: "Kimchi Coding", Group: GroupCLI, Vendor: "Castle",
			Roots:  []string{"~/.config/kimchi"},
			Stores: []Store{jsonlStore("harness/sessions/*/*.jsonl")},
		},
		{
			ID: "commandcode", Name: "Command Code", Group: GroupCLI, Vendor: "Command Code",
			Roots:  []string{"~/.commandcode"},
			Stores: []Store{jsonlStore("projects/*/*.jsonl")},
		},
		{
			ID: "muse-code", Name: "Muse Code", Group: GroupCLI, Vendor: "Muse",
			Roots:  []string{"~/.local/share/muse"},
			Stores: []Store{jsonlStore("sessions/**/*/session.jsonl")},
		},
		{
			ID: "zcode", Name: "ZCode", Group: GroupCLI, Vendor: "ZCode",
			Roots:  []string{"~/.zcode"},
			Stores: []Store{jsonlStore("projects/*/*.jsonl")},
			// cli/db/db.sqlite 是同一批会话的另一份索引，格式与 opencode 同族，
			// 但表名没核实 —— 猜表名比不裁索引更危险，故先不写进 Indexs。
			Note: "cli/db/db.sqlite 另有一份索引，当前版本不裁，删完可能留下条目。",
		},
		{
			ID: "reasonix", Name: "Reasonix", Group: GroupCLI, Vendor: "Reasonix",
			Roots: []string{"~/.reasonix"}, Env: []string{"REASONIX_STATE_HOME"},
			Stores: []Store{
				jsonlStore("sessions/*.jsonl"),
				jsonlStore("projects/*/sessions/*.jsonl"),
			},
			Note: "1.x 的 sessions-v4/*/events.frames 是 zstd 帧，当前扫不到。",
		},
		{
			ID: "codewhale", Name: "CodeWhale", Group: GroupCLI, Vendor: "CodeWhale",
			Roots: []string{"~/.codewhale", "~/.deepseek"}, Env: []string{"CODEWHALE_HOME"},
			Stores: []Store{jsonStore("sessions/*.json")},
		},
		{
			ID: "openclaw", Name: "OpenClaw", Group: GroupCLI, Vendor: "OpenClaw",
			Roots: []string{"~/.openclaw"}, Env: []string{"OPENCLAW_STATE_DIR"},
			Stores: []Store{jsonlStore("agents/*/sessions/*.jsonl")},
			Note:   "agents/<agent>/sessions/ 下是 append-only 的 pi 格式 JSONL。",
		},
		{
			ID: "billion-context", Name: "Billion Context", Group: GroupCLI, Vendor: "Billion Context",
			Roots:  []string{"~/.local/share/billion-context"},
			Stores: []Store{jsonStore("sessions/*/*.json")},
			Note:   "按上游协议分目录（sessions/openai/…），标题在 payload.meta.title。",
		},
		{
			// pi 的会话格式被若干衍生 harness 复用，解析共享，各加一条即可。
			ID: "omp", Name: "Oh My Pi", Group: GroupCLI, Vendor: "omp",
			Roots:  []string{"~/.omp/agent"},
			Stores: []Store{jsonlStore("sessions/**/*.jsonl")},
		},
		{
			ID: "prime-agent", Name: "Prime Agent", Group: GroupCLI, Vendor: "PrimeIntellect",
			Roots:  []string{"~/.prime/agent"},
			Stores: []Store{jsonlStore("sessions/**/*.jsonl")},
		},
		{
			ID: "senpi", Name: "Senpi", Group: GroupCLI, Vendor: "Senpi",
			Roots:  []string{"~/.senpi/agent"},
			Stores: []Store{jsonlStore("sessions/*/*.jsonl")},
		},
		{
			ID: "gajae-code", Name: "Gajae Code", Group: GroupCLI, Vendor: "gjc",
			Roots:  []string{"~/.gjc/agent"},
			Stores: []Store{jsonlStore("sessions/*/*.jsonl")},
		},

		// —— 同名产品的 CLI 形态：数据根与 IDE 版不同，只能单独一条 ——
		{
			ID: "cursor-agent-cli", Name: "Cursor Agent CLI", Group: GroupCLI, Vendor: "Anysphere",
			Roots: []string{"~/.cursor"}, Env: []string{"CURSOR_CONFIG_DIR"},
			Stores: []Store{
				jsonlStore("projects/**/agent-transcripts/**/*.jsonl"),
			},
			// chats/*/*/store.db 也是会话库，但一个会话一个库（DB 目前不支持 glob）。
			Note: "与 Cursor IDE 是两套数据；chats/*/*/store.db 暂时扫不到，只能清 transcript。",
		},
		{
			ID: "cline-cli", Name: "Cline CLI", Group: GroupCLI, Vendor: "Cline",
			Roots: []string{"~/.cline"},
			Env:   []string{"CLINE_SESSION_DATA_DIR", "CLINE_DATA_DIR"},
			Stores: []Store{
				jsonStore("data/sessions/*/*.messages.json"),
				jsonStore("sessions/*/*.messages.json"),
			},
		},
		{
			ID: "kiro-cli", Name: "Kiro CLI", Group: GroupCLI, Vendor: "AWS",
			Roots: []string{
				"~/Library/Application Support/kiro-cli",
				"~/.local/share/kiro-cli",
				"~/.kiro",
			},
			Stores: []Store{
				jsonlStore("sessions/cli/*.jsonl"),
				jsonlStore("sessions/*/sess_*/messages.jsonl"),
			},
		},

		// —— 本地优先的桌面 AI 客户端 ——
		{
			ID: "anythingllm", Name: "AnythingLLM", Group: GroupApp, Vendor: "Mintplex Labs",
			Bundles: []string{"AnythingLLM.app"},
			Roots: []string{
				"~/Library/Application Support/anythingllm-desktop",
				"~/.config/anythingllm-desktop",
			},
			Stores: []Store{
				// workspace_chats 是「一轮问答」一行，不是线程级的会话。按行删能
				// 精确清掉某一轮，但同一线程在列表里会出现多行 —— 这是它的数据
				// 模型决定的，写进 Note 而不是假装它是一条会话。
				sqliteStore("storage/anythingllm.db", "workspace_chats", "id", "prompt", "createdAt", ""),
			},
			Note: "按「一轮问答」一行计，同一线程会有多行；删行后会 VACUUM，空间当场回收。",
		},
		{
			ID: "lm-studio", Name: "LM Studio", Group: GroupApp, Vendor: "Element Labs",
			Bundles: []string{"LM Studio.app"},
			Roots:   []string{"~/.lmstudio", "~/.cache/lm-studio"},
			Stores:  []Store{jsonStore("conversations/*.json")},
			Note:    "官方明确不保证会话 JSON 结构稳定，所以只靠启发式抽标题。",
		},
		{
			// VS Code 的 Copilot Agent 会为每个「会话工作区」建一个 <uuid>/session.db。
			// 库里是 turns / local_turns / file_edits / chat_drafts / reviewed_files，
			// 外加 session_metadata 里的 git / github 信息。一个目录 = 一条会话。
			// 注意：本机 108 个库里 turns 全为 0（只有元数据），所以体积很小、
			// 标题会退化成 uuid —— 这是真实情况，不是扫描出错，别当成漏扫去修。
			ID: "copilot-agent-workspace", Name: "Copilot Agent Workspace", Group: GroupIDE, Vendor: "GitHub",
			Bundles: []string{"Visual Studio Code.app", "Visual Studio Code - Insiders.app"},
			Roots: []string{
				"~/Library/Application Support/Code/agentSessionData",
				"~/Library/Application Support/Code - Insiders/agentSessionData",
			},
			Stores: []Store{dirStore("*")},
			Note:   "一个 uuid 目录一条会话，正文在目录内的 session.db；多数会话只有元数据、没有 turns。",
		},
		{
			ID: "open-design", Name: "Open Design", Group: GroupApp, Vendor: "Open Design",
			Bundles: []string{"Open Design.app"},
			Roots: []string{
				"~/Library/Application Support/Open Design/namespaces/release-stable-intel/data",
				"~/Library/Application Support/Open Design/namespaces/release-stable-canary/data",
			},
			Stores: []Store{
				// 正文在 app.sqlite 的 conversations 表；聊天产物（chat-artifact-blobs）
				// 与项目文件在同级目录，删行不影响它们，因此不登记 Assoc。
				sqliteStore("app.sqlite", "conversations", "id", "title", "updated_at", ""),
			},
			Note: "会话行在 app.sqlite；产物文件在 chat-artifact-blobs/ 与 projects/，删行不会带走它们。",
		},
		{
			ID: "jan", Name: "Jan", Group: GroupApp, Vendor: "Jan HQ",
			Bundles: []string{"Jan.app"},
			Roots:   []string{"~/Library/Application Support/Jan", "~/.config/Jan"},
			Stores:  []Store{jsonStore("data/threads/*.json")},
		},
	}
}
