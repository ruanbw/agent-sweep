# AgentSweep HTTP API 契约

前后端之间**只有一个 net/http handler**：静态资源 + 下面这些 JSON 接口。
没有 bindings 生成器、没有 bundler、没有 websocket。前端是纯 HTML/CSS/JS。

约定：
- 所有响应 `Content-Type: application/json; charset=utf-8`
- 出错时返回非 200 + `{"error":"人话描述"}`
- 时间一律 RFC3339 字符串（`2026-10-17T18:47:59+08:00`）
- 字节一律 int64
- 路径一律绝对路径

## GET /api/bootstrap

一次性拿到首屏需要的全部数据。

```json
{
  "version": "0.1.0",
  "home": "/Users/x",
  "platform": "darwin",
  "prefs": { "deleteMode": "trash", "confirmBeforeDelete": true, "includeSnapshots": true, "autoScan": true },
  "scan": { "status": "done", "startedAt": "...", "finishedAt": "...", "durationMs": 1234,
            "progress": { "done": 12, "total": 30, "current": "Cursor" },
            "issues": [ { "agentId": "cursor", "message": "…" } ] },
  "agents": [ AgentView ],
  "sessions": [ SessionView ],
  "batches": [ BatchView ]
}
```

`scan.status`：`idle` | `running` | `done` | `error`

### AgentView

```json
{
  "id": "claude-code", "name": "Claude Code", "group": "cli", "vendor": "Anthropic",
  "installed": true, "home": "/Users/x/.claude", "roots": ["/Users/x/.claude"],
  "note": "删除会同步裁剪 history.jsonl…", "custom": false,
  "sessionCount": 12, "totalBytes": 1048576, "error": ""
}
```

`group` 取值：`cli` | `ide` | `app` | `extension` | `other`

### SessionView

```json
{
  "uid": "claude-code:abc-123", "agentId": "claude-code", "id": "abc-123",
  "title": "重构 registry", "project": "/Users/x/proj",
  "updatedAt": "2026-10-17T10:00:00+08:00",
  "bytes": 20480, "messages": 24, "snippet": "…",
  "active": false, "storage": "file", "paths": ["/Users/x/.claude/projects/…/abc-123.jsonl"]
}
```

`storage`：`file` | `dir` | `sqlite`。`active=true` 表示会话可能正在使用，UI 必须提示。

### BatchView

```json
{ "id": "20261017-184759", "at": "...", "count": 3, "freed": 1048576,
  "agents": ["claude-code"], "undoable": true }
```

## POST /api/scan

启动一次后台扫描，立即返回 `{"ok":true}`。进度用 `GET /api/scan` 轮询（建议 250ms）。
已经在跑时重复调用是幂等的，不会开第二次。

## GET /api/scan

返回与 `bootstrap.scan` 相同的对象。

## POST /api/delete

```json
{ "uids": ["claude-code:abc-123", "zed:thread-9"] }
```

返回：

```json
{ "batchId": "20261017-184759", "batchIds": ["20261017-184759"],
  "freed": 1048576, "count": 3,
  "errors": [ { "uid": "…", "message": "…" } ] }
```

一次删除里如果混了多个 Agent，每个 Agent 各写一份 journal（撤销要按 Agent 还原），
所以 `batchIds` 是数组，`batchId` 只是其中第一个（兼容只取单个的调用方）。

删除语义：文件/目录进废纸篓（`prefs.deleteMode=trash`）或直接删；索引文件先备份到
`~/Library/Application Support/AgentSweep/backups/<batchId>/`；SQLite 删行前整库备份，删完 VACUUM。
`freed` 是**真实释放**字节数：进废纸篓不算释放，此时为 0，UI 文案要说「已移入废纸篓」。

## POST /api/undo

```json
{ "batchId": "20261017-184759", "batchIds": ["20261017-184759"] }
```

返回 `{"restored": 3, "errors": []}`。只有 `undoable=true` 的批次能撤销。
传 `batchIds` 时按顺序撤销每一批，`batchId` 是单批的简写。

## GET /api/discover

扫描本机「像会话存储、但目录里没有对应 Agent」的目录。

```json
{ "candidates": [ {
  "path": "/Users/x/.someagent", "name": "SomeAgent", "confidence": 0.82,
  "sessionCount": 37, "bytes": 2097152, "evidence": ["目录名含 sessions", "37 个 .jsonl"],
  "suggested": { /* 一份可直接 POST /api/adapters 的 catalog.Spec 草稿 */ }
} ] }
```

## POST /api/adapters

请求体是一份完整的 `catalog.Spec`（见 `internal/catalog/spec.go`），写入
`~/Library/Application Support/AgentSweep/adapters.json`。返回 `{"ok":true,"id":"…"}`。

## DELETE /api/adapters?id=xxx

删掉一条**用户自定义**适配器。内置项返回 400（内置项不该被删）。

## GET /api/prefs · PUT /api/prefs

`prefs` 形状同 bootstrap。PUT 只接受局部字段。

## POST /api/reveal

`{"path":"/abs/path"}` → 在 Finder 中显示。

## POST /api/open

`{"path":"/abs/path"}` → 用默认程序打开。
