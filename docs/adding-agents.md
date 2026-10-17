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

## 提交前自查

```bash
go test ./...
go run ./tools/scancheck <your-agent-id>    # 看它到底扫出了什么
go run . discover                           # 确认它没被当成「未收录」重复列出来
```

`scancheck` 会打印前 3 条会话的标题、时间、项目路径和**全部物理路径**。
最后一项尤其要看：`associatedPaths` 少了一项，就是删完还剩一块。
