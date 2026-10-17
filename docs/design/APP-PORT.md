# 把设计原型落到真实应用（Port Brief）

> 目标：让 `internal/gui/assets` 的真实 AgentSweep 应用呈现 `docs/design/prototype/` 的效果，
> 保留现有 Go API 与全部真实功能。原型本身继续作为设计参考，不被修改。

## 0. 结论先行

- 视觉与交互基线：`docs/design/prototype/index.html` / `app.css` / `app.js`。
- 数据来源：`docs/api.md` 的真实 HTTP API（`/api/bootstrap`、`/api/scan`、`/api/delete`、
  `/api/undo`、`/api/discover`、`/api/adapters`、`/api/prefs`、`/api/reveal`、`/api/open`、`/api/icon`）。
- 不修改 Go 代码（`internal/gui/*.go`）；静态资源已 embed，改 assets 即可。
- 原 assets 已备份到 `/tmp/agentsweep-assets-backup/`；需要回退时从那里取。

## 1. 文件与职责

| 文件 | 负责人 | 说明 |
|---|---|---|
| `internal/gui/assets/index.html` | UI | 去掉原型外壳（`.prototype/.proto-stage/.window/.window-titlebar/.proto-dock`），保留 `.app` 与所有浮层；加载 `brand-icons.js` + `app.js` |
| `internal/gui/assets/app.css` | UI | 从原型 CSS 派生：全窗口布局、去掉窗口框/纸纹/控制台样式；保留 token/组件/响应式/动效 |
| `internal/gui/assets/app.js` | Logic | 从原型 JS 派生：把演示数据层替换为真实 API；保留状态/渲染/交互/键盘 |
| `internal/gui/assets/brand-icons.js` | Logic | 已生成：`window.AGENT_BRAND = { icons, iconKeys }`（32 个品牌图标） |

不要动 `docs/design/` 下的原型与文档；不要动 Go 文件。

## 2. index.html 要求

- 根节点直接是 `.app`（无 `.window` 包裹）；`<body>` 充满视口。
- 删除：`.proto-stage`、`.window`、`.window-titlebar`（交通灯）、`.proto-dock`、`#protoDockToggle`、
  所有 `data-demo` / `data-open` 控制台按钮、`?dock` 相关。
- 保留：topbar、sidebar、list-pane、detail-pane、statusbar、settingsSheet、discoverSheet、
  reviewDialog、palette、shortcutsDialog、filterPopover、quickPopover、contextMenu、scrim、toasts、
  内联 SVG sprite。
- 脚本顺序：`brand-icons.js` → `app.js`；删除 `data.js`。
- 保留所有 ID（app.js 依赖）；`#demoBadge` 可删除或保留隐藏（真实后端无需演示徽标）。
- 原型里的 `data-static-sample` 静态示例可保留（JS 会覆盖），但不要作为最终内容。
- 生产窗口使用 macOS 原生标题栏；HTML 不画交通灯。

## 3. app.css 要求

- 把原 `.window` 的尺寸/边框/阴影/圆角/容器查询转移到 `.app`：
  - `body { margin:0; height:100vh; background: var(--canvas); }`
  - `.app { height:100vh; display:grid; grid-template-rows: var(--topbar-h) minmax(0,1fr) var(--statusbar-h); container-type: inline-size; container-name: window; }`
  - 删除 `.prototype`、`.proto-stage`、`.window`、`.window-titlebar`、`.proto-dock` 及窗口外纸纹规则。
- 保留设计 token、浅/深色、密度、响应式、容器查询、焦点态、动效、reduced-motion。
- 关键断点仍按窗口宽度：≤1280 隐藏消息列、≤1180 项目列 104px、≤1000 侧栏图标栏、≤1120 详情收成浮层。
- 背景直接是暖纸/墨色画布；不要页面居中留白（真实应用占满窗口）。

## 4. app.js 数据层适配（核心）

从原型 JS 出发，替换掉演示/评审逻辑：

### 4.1 必须删除

- `window.PROTO_DATA`、`data.js`、`createMock*`、演示状态（`?state=`、dock、`setDemoState`、`resetDemo`）。
- `URLSearchParams` 的 `theme/state/panel/dock/density/detail/agent` 演示参数（可保留 `theme` 作为调试？不需要）。
- 所有模拟扫描、模拟删除、模拟发现、模拟批次的定时器。
- 原型窗口/控制台相关事件。

### 4.2 必须接上

- **bootstrap**：`GET /api/bootstrap` → 填充 `agents/sessions/batches/prefs/scan`；计算计数与体积；首屏加载态；失败进入可重试错误态（不要静默演示数据）。
- **扫描**：`POST /api/scan` → 每 250ms `GET /api/scan` → 更新进度/当前 Agent/issues；完成后
  重新拉取 bootstrap（保留筛选/焦点/选择中仍存在的 UID）。
- **清理**：`POST /api/delete {uids}` → 用响应的 `batchId/batchIds/freed/count/errors`
  更新列表、批次、toast；错误按 `errors[]` 逐条提示；不要只在本地删除。
- **撤销**：`POST /api/undo {batchId, batchIds}` → 成功后重新拉取 bootstrap；失败 toast。
- **发现**：`GET /api/discover` → 候选卡片；`POST /api/adapters`（用 candidate.suggested）→ 成功后
  `DELETE /api/adapters?id=` 可选；收录后刷新 bootstrap。
- **设置**：`PUT /api/prefs` 局部字段；成功后再更新 UI；失败回滚开关并 toast。
- **Finder/打开**：`POST /api/reveal` / `POST /api/open`；原型 toast 改为真实调用 + 错误 toast。
- **图标**：内置 Agent 先用 `window.AGENT_BRAND`（icons/iconKeys）；没有品牌图的 custom Agent
  用 `GET /api/icon?id=…`，失败回退首字母；暗色用 icon.dark。
- **外观**：API 没有 appearance 字段。支持 `自动/浅色/深色`，存 `localStorage`，默认自动；
  写 `document.documentElement.dataset.theme`，自动时删除属性。
- **密度**：`localStorage`，默认舒适。

### 4.3 数据映射

| 原型字段 | 真实 API |
|---|---|
| `agents[].sessionCount/totalBytes` | 由 `sessions` 实时派生 |
| `agents[].indexPaths` | 无 → 详情/账本不显示索引路径；用 `agent.note` 说明 |
| `sessions[].*` | 同名字段（`uid/agentId/id/title/project/updatedAt/bytes/messages/snippet/active/storage/paths`） |
| `candidates` | `GET /api/discover` 的 `candidates[]`（含 `suggested`） |
| `batches[].*` | `BatchView`：`id/at/count/freed/agents/undoable`；**没有 deleteMode**，不要编造模式 |
| `scan` | `status/startedAt/finishedAt/durationMs/progress/issues` |
| `prefs` | `deleteMode/confirmBeforeDelete/includeSnapshots/autoScan` + 本地 appearance |

批次行文案建议：`今天 16:37 · 4 条` + `真实释放 0 B · Claude Code、Cursor`；
可撤销才显示「撤销」，不可撤销显示「不可撤销」；不要写“移入废纸篓”或“永久删除”除非能推断
（API 没有 mode，别猜）。

### 4.4 必须保留的交互（来自设计）

- 浏览 ≠ 选择（焦点驱动详情；复选框/空格/⌘A/快速选择才选择）。
- 搜索、存储/时间/体积筛选、排序、快速选择（替换选择）、密度、详情开关。
- 空间账本：正文体积来自选中；索引备份只写「已备份 / 不额外备份」；真实释放：
  `deleteMode=trash` 时 0 B，permanent 时正文体积。
- 永久删除必须勾选确认；活动会话警告点名 Agent。
- 状态栏：批次回执、已选摘要、清理按钮；`⌘Z` 撤销。
- 命令面板、快捷键表、右键/更多菜单、toast、焦点陷阱/归还、Esc 层级。
- 空/无结果/扫描中/部分失败状态。
- 无障碍 aria 与 reduced-motion。

## 5. 验收

```bash
go test ./...
make build
bin/AgentSweep serve --addr :8766   # 后台；真实 API
# 用 headless Chrome 打开 http://127.0.0.1:8766 截图/交互
make app && open AgentSweep.app     # 最终由 Lead 启动 GUI
```

- 首屏、扫描、选择、清理（废纸篓/永久）、撤销、发现、设置、筛选、命令面板全部真实可用。
- 无 console error；无 `PROTO_DATA`/demo 残留；无外部字体/图片。
- 1180×760 / 1440×900 / 960×700 布局正常。
- `file://` 直接打开时显示连接失败错误态（不再使用演示数据）。
