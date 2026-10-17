# 新增一款 Agent

有两种方式，按「有没有人认得它」选。

## 一、它已经在「发现」页里

打开发现页 → 点「收录为 Agent」。草稿会写进
`~/Library/Application Support/AgentSweep/adapters.json`，下一次扫描就生效，不用改代码、不用重启。

草稿是**推断**出来的（Roots / Glob / Table 都可能不准），收录后请核对一遍：

- 会话数是 1 而体积几十 MB → Glob 指到了「装着会话文件的目录」而不是会话本身
- 会话数是 0 → Roots 指错了，或者它根本不用文件存会话
- 标题全是「会话 xxxxxxxx」→ 正文解析没命中，见下面「标题抽不出来」

## 二、写进内置目录

往 `internal/catalog/builtin.go` 加一条 `Spec`。一条声明只需要回答四个问题。

### 1. 数据在哪

```go
Roots: []string{"~/.claude"},            // 候选路径，第一个存在的即为数据根
Env:   []string{"CLAUDE_CONFIG_DIR"},    // 可选：环境变量覆盖
```

`Roots` 可以写多个，用于兼容版本迁移（`~/.aider` 与 `~/.config/aider`）。

### 2. 一条会话长什么样

四种形态，占满目前见过的所有 Agent：

| kind | 含义 | glob 示例 |
|---|---|---|
| `jsonl` | 一个 `.jsonl` = 一条会话 | `projects/*/*.jsonl`、`sessions/**/*.jsonl` |
| `json` | 一个 `.json` = 一条会话 | `tmp/*/chats/*.json` |
| `dir` | 一个子目录 = 一条会话 | `tasks/*` |
| `sqlite` | 表里一行 = 一条会话 | 见下 |

`glob` 支持 `*`（单层）与 `**`（任意层），**相对数据根**。

sqlite 形态要填表和列：

```go
sqliteStore("threads/threads.db", "threads", "id", "summary", "updated_at", "folder_paths",
    "messages:thread_id",   // ← 可选：删主表行时连带删的子表
)
```

列的候选名有一套兜底（`id` / `session_id` / `conversation_id`；`updated_at` / `time_updated` / …），
但显式写出来更可靠。

### 3. 删会话还要带走什么

```go
Assoc: []string{"{id}", "/file-history/{id}"}
```

- `{id}` 相对**会话文件所在目录**
- `/{id}` 相对**数据根**

Claude Code 的子代理工作区是前者，文件快照是后者。写漏的后果是磁盘上留一堆孤儿目录。

### 4. 删了正文还要裁什么索引

```go
Indexs: []Index{
    {Kind: "jsonl-lines", Glob: "history.jsonl", Key: "sessionId"},
    {Kind: "json-array",  Glob: "projects/*/sessions-index.json", Key: "sessionId"},
    {Kind: "sqlite-rows", DB: "state.vscdb", Table: "...", IDCol: "..."},
}
```

**这一步不能省。** 只删正文不裁索引，Agent 侧会留下一批点进去是空的幽灵会话 ——
比不删还糟。四种 `Kind` 见 `spec.go` 的注释。

## 标题抽不出来

标题走启发式（`internal/session/meta.go`），顺序是：

1. 顶层 `title` / `name` / `summary` / `slug` / `display`
2. 第一条 `role=user` 的正文（支持嵌套：`message.content`、`data.inserted[].content[]`）
3. 嵌套包装层里的 `title`（如 `payload.meta.title`）
4. 文件名

`system` / `tool` 角色的话**永远不会**被当标题 —— 它们动辄几万字。

如果某款 Agent 的格式完全抽不出标题，优先补 `deepTitle` / `deepUserText` 的容器键，
而不是给它单写一个解析器：前者一次覆盖一批同类格式，后者每加一款就多一份要维护的代码。

## 已调研但暂不能收录（缺实测/形态不支持）

下面这些是 2026-10 调研里找到、但**没有降级硬收**的候选。共同原则：宁可少一款，
也不要凭印象编 schema —— 写错表名会天天打印扫描错误，写错列名则可能删错行。
每条都记下「差什么证据」，等有实机样本再补。

### 形态不支持（现有四种 Store 表达不了）

- **Aider** — 线索：项目根目录的 `.aider.chat.history.md`（追加型聚合 markdown），
  `~/.aider.chat.history.md` 只在从 home 启动时出现。不收：一个文件混了多条会话，
  没有 session ID、没有消息级时间戳，`jsonl`/`dir` 都不成立。下一步：要做得写
  「按 header 时间戳切段」的专用解析器，收益不匹配。
- **Cherry Studio / Chatbox** — 线索：`~/Library/Application Support/CherryStudio`，
  会话在 IndexedDB（Dexie/LevelDB）单体里。不收：几千条会话挤在一个 LevelDB，
  既不能按会话删文件，也不能按行裁；社区还有单体库损坏全丢的 issue。下一步：
  只能做「整库清理」提示，不能做逐会话删除。
- **Void** — 线索：VS Code fork，线程存在 `state.vscdb` 的 ItemTable，一个 key 塞
  整个 threads 字典（源码 `THREAD_STORAGE_KEY`）。不收：单体 blob，只能整键重写，
  没有「一条会话」的存储粒度。下一步：若上游拆成一会话一记录再评估。
- **JetBrains AI Assistant** — 线索：`workspace/<workspaceId>.xml` 的
  `ChatSessionStateTemp` + `aia-task-history/<uid>.events`（每行 base64 JSON）。
  不收：XML + 行式 base64 都超出四种 Store，需要新的解析器与 XML 安全解析。
  下一步：先做「目录/会话计数 + 整文件进废纸篓」，行级裁剪另开引擎能力。
- **Windsurf / Devin Desktop Cascade** — 线索：`~/.codeium/windsurf/cascade/<id>.pb`。
  不收：一会话一个 AES-GCM 加密 protobuf，标题读不出、单条删除也无法安全重写。
- **Aider 之外的「项目内散落文件」类**（含部分老版 Copilot 的 per-project log）：
  没有中心数据根，无法按 Agent 展示与勾选，需要「项目内扫描」这一新功能。

### 云端 / 服务端，本地没有逐会话正文

- **Claude Desktop 标准版**、**ChatGPT Desktop**、**豆包 / 腾讯元宝 / 夸克 / Manus / Coze**、
  **Kimi 桌面端**：会话在厂商云端，本地只有 WebView 缓存或 IndexedDB 快照。
  下一步：只可能收录「本地缓存清理」，不当作会话正文。
- **Dify / n8n / LibreChat / LobeChat（服务端模式）**：会话在 Postgres/MongoDB 或
  服务端 IndexedDB，本地 SQLite 里常是工作流执行记录而不是 AI 会话。
- **Replit Agent**、**Warp Agent**：云端为主，本机没有可枚举的逐会话存储。

### 有路径线索，但缺「会话表/文件」的实测证据

- **Tabnine Chat** — 线索：`globalStorage/Tabnine.tabnine-vscode/*.json`（三方工具
  支持解析）。差：没有实机确认 JSON 是「会话」还是扩展配置。下一步：装一次后
  看文件名与内容结构。
- **Gemini Code Assist（VS Code 扩展）** — 线索：扩展 `globalStorage` 目录，与
  `gemini-cli`、`antigravity` 是三套独立数据。差：具体 store 形态与文件命名。
- **Amazon Q Developer（扩展 + CLI）** — 线索：`~/.aws/amazonq/`，Chat Wizard 有可配
  路径说明路径不唯一。差：扩展侧是 `globalStorage` 还是 home 目录、会话粒度未知。
- **Comate 文心快码** — 线索：`~/Library/Application Support/Comate/history`
  （中文二手教程）。差：一手官方路径与会话形态都未证实。
- **Qoder CLI** — 线索：现有条目只覆盖 Qoder IDE；CLI 数据根未证实。
- **Plandex** — 线索：`~/.plandex-home`（Harbor 镜像用它做 volume）。差：具体库名/
  表名未知，v2 可能改为本地 server 自有 store。
- **Docker cagent** — 线索：config `~/.config/cagent`、data `~/.cagent`
  （`GetConfigDir/GetDataDir` 源码可证）。差：默认 SQLite 库名与表名未实测；
  本机 `~/.cagent` 不存在。下一步：有会话的机器上列一次 `~/.cagent/*.db`。
- **Magpie** — 线索：`~/.config/magpie` 有 65MB。实测只有 providers/quotas/plugins/
  routing/*.jsonl.gz，没有 sessions 目录或 .db —— 更像模型路由台，不是会话型 Agent。
  下一步：确认它是否根本不落会话，若如此直接排除。
- **browser-use** — 线索：`~/.config/browseruse` 有 config/profiles/extensions。
  源码只定义配置与浏览器 profile，不落会话 transcript。下一步：若要收只能收配置清理。
- **CodeGeeX** — 线索：IDE 补全 + 云端问答，未见本地会话库证据。下一步：实机确认。

## 提交前自查

```bash
go test ./...
go run ./tools/scancheck <your-agent-id>    # 看它到底扫出了什么
go run . discover                           # 确认它没被当成「未收录」重复列出来
```

`scancheck` 会打印前 3 条会话的标题、时间、项目路径和**全部物理路径**。
最后一项尤其要看：`associatedPaths` 少了一项，就是删完还剩一块。

## 收录一款 Agent 的检查清单

内置目录现在有 60+ 条，靠手加条目最容易犯的错不是写错路径（那只会显示「未安装」），
而是下面这几类 —— 它们都不报错，只是静默地扫不到东西。

1. **CLI 与 IDE 版是不是两套数据根？** `Spec.Home` 只取第一个存在的根，
   把两者塞进同一条会让用户没法只清其中一个。数据根不同就拆成两条
   （`trae` / `trae-cn`、`cursor` / `cursor-agent-cli` 都是这么处理的）。
2. **一条会话到底是「文件」还是「目录」？** Pi 是目录（`<uuid>/run-0/session.jsonl`），
   Codex 是文件。选错了要么扫不到，要么把整个会话目录算成一条。
3. **同层还有没有兄弟目录？** Pi 的 `sessions/<项目>/` 下并排放着 `subagent-artifacts/`，
   用 `*` 会把它当成一条会话。用字符类收窄（`[0-9a-f]*-...`）比事后过滤可靠。
4. **标题能不能抽出来？** 抽不出时先用 `go run ./tools/scancheck <id>` 看是不是
   正文被环境说明（`<system-reminder>`）占住了 —— `internal/session/meta.go`
   的 `isBoilerplate` 就是为这个留的。
5. **删正文之后索引要不要裁？** 只删正文不裁索引 = 幽灵会话。
   表名没核实就别猜，写进 `Note` 说明「当前版本不裁」比猜错安全。

`internal/catalog/builtin_test.go` 会替你兜住结构错误（ID 重名、必填字段缺失、
未知 Store 形态），但**兜不住路径写错** —— 那一条只能靠 `scancheck` 实测。
