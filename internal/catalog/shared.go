package catalog

// SharedIndex 是「属于某个宿主 Agent 的数据根、但会在多个 Agent 的删除中被
// 联动裁剪」的索引。
//
// 为什么不能塞进普通 Index：Index 的相对路径是相对「当前被删 Agent 的 Home」
// 解析的，而这类索引在**另一个** Agent 的数据根里。最典型的是 VS Code 的
// globalStorage/agent-host.db —— Claude Code / Copilot CLI 的会话登记在那里，
// 删正文时如果不联动裁剪，VS Code 的 Agent Sessions 会留下点进去是空的幽灵条目。
type SharedIndex struct {
	Host string // 宿主 Spec ID，用它的 Home 解析 DB
	DB   string // 相对宿主 Home 的库路径
	// Tables 是要裁的表，按「子表在前」的顺序：session_chats 引用 sessions，
	// 先删引用再删主行，即便某天加了外键也不会先坏约束。
	Tables []string
	IDCol  string   // 指向会话 ID 的列（agent-host.db 里是 session_uri）
	Agents []string // 哪些 Agent 的删除会触发
}

// SharedIndexes 返回需要跨 Spec 联动裁剪的索引清单。
//
// 本机（VS Code globalStorage/agent-host.db）实机确认的 schema：
// sessions(session_uri PK, provider, start_time, external, registration_source,
// modified_time)、sessions_v2(session_uri PK, …, payload)、
// session_chat_catalogs(session_uri PK, revision, …)、
// session_chats(session_uri, chat_uri, chat_order, …) 与 metadata。
// 注意 session_chats.session_uri 外键指向 session_chat_catalogs（ON DELETE
// CASCADE），只删 chats/sessions 会留下 catalog 这一行孤儿 —— 所以它也在
// 裁剪清单里，排在 chats 之后（子表先删）。metadata 里能查到
// sessionRegistryBackfilled:claude / :copilotcli，说明它是这两个 Agent 的
// 跨会话注册表。0 行时也照样处理：删了正文不裁它，等注册表回填就会出现幽灵会话。
func SharedIndexes() []SharedIndex {
	return []SharedIndex{{
		Host:   "vscode",
		DB:     "globalStorage/agent-host.db",
		Tables: []string{"session_chats", "session_chat_catalogs", "sessions_v2", "sessions"},
		IDCol:  "session_uri",
		Agents: []string{"claude-code", "copilot-cli"},
	}}
}
