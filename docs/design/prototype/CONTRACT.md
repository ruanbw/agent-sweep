# 原型实现契约（Prototype Contract）

> 这份文件是 `prototype/index.html`（结构与视觉）和 `prototype/app.js`（状态与交互）之间
> 唯一的接口。任何一方需要改 ID、类名或数据形状，先改这里，再动代码。
>
> 目标：一个**双击即可打开**、无网络、无构建、无框架的高保真原型，覆盖 AgentSweep 的主要
> 流程与状态。不是生产代码；但交互必须真实、可评审。

---

## 0. 文件与职责

```
docs/design/
  DESIGN.md                 视觉/主题规范（唯一视觉来源）
  ux-flows.md               UX 流程、状态、文案（另一份交付物）
  prototype/
    CONTRACT.md             本文件
    index.html              结构 + 内联 SVG 图标 + 浮层骨架（UI 负责）
    app.css                 全部样式（UI 负责）
    data.js                 演示数据（已生成；交互方可增补 helper）
    app.js                  状态与交互（交互负责）
  screenshots/              QA 截图
```

- 不用 `fetch`、不用 CDN、不用 `import`、不用构建。`file://` 直接打开必须工作。
- 不引入任何网络字体或图片；Agent 图标使用 `data.js` 里的内联 SVG path。
- 生产应用代码（`internal/gui/assets/*`）**不允许修改**。

---

## 1. 打开方式

```bash
open docs/design/prototype/index.html
```

原型页面由两部分组成：

1. **模拟窗口**（`.window`）：完整还原应用界面，包含原生窗口栏的视觉模拟
   （生产环境由 macOS 提供，不在 HTML 中实现）。
2. **原型控制台**（`.proto-dock`）：窗口外的评审工具，不属于产品 UI。

### URL 参数（用于评审与截图）

| 参数 | 取值 | 作用 |
|---|---|---|
| `theme` | `auto` / `light` / `dark` | 外观；默认 `auto`（跟随系统） |
| `state` | `idle` / `scanning` / `empty` / `noresults` / `error` | 演示状态 |
| `panel` | `settings` / `discover` / `review` / `palette` / `shortcuts` / `context` | 直接打开浮层 |
| `dock` | `0` | 隐藏控制台 |
| `density` | `comfortable` / `compact` | 表格密度 |
| `detail` | `0` | 初始收起详情 |
| `agent` | Agent id | 初始筛选某个 Agent |

示例：

```
index.html?theme=dark&state=idle
index.html?state=scanning&dock=0
index.html?panel=discover&theme=light
index.html?panel=review&state=idle
```

设置后仍可通过控制台改变。参数只决定初始状态，不锁定。

### 原型控制台

固定于窗口下方居中，包含：

- 外观：`自动 / 浅色 / 深色` 分段
- 演示状态：`正常 / 扫描中 / 空列表 / 无结果 / 部分失败`
- 打开：`清理确认 / 发现 / 设置 / 命令面板 / 快捷键`
- `重置演示`：恢复初始数据与状态
- `隐藏`：收起控制台，只留右下角一枚圆形 `原型` 按钮；`?dock=0` 时默认收起

控制台样式必须与产品 UI 明显区分（更小、更暗、无 macOS 风格），不能抢画面。

---

## 2. 数据形状（`window.PROTO_DATA`）

```js
{
  icons: { [iconKey]: { color, dark, viewBox, paths: [string] } },
  iconKeys: { [agentId]: iconKey },
  agents: [{
    id, name, group, vendor, installed, home, roots, note, custom,
    sessionCount, totalBytes, indexPaths: [string]
  }],
  sessions: [{
    uid, agentId, id, title, project,
    updatedAt, bytes, messages, snippet,
    active, storage, paths: [string]
  }],
  candidates: [{ path, name, confidence, sessionCount, bytes, evidence: [string], suggested }],
  batches: [{ id, at, count, freed, agents: [agentId], undoable }]
}
```

- 数据已从现有应用抽取，含 26 条会话、8 个 Agent（其中 1 个未安装）、2 个待发现目录。
- `agents[].sessionCount` / `totalBytes` 是**初始值**；运行时由会话实时派生，不要写死。
- `group`：`cli` / `ide` / `app` / `extension` / `other`。
- `storage`：`file` / `dir` / `sqlite`。
- 时间均为 RFC3339；渲染时转相对/绝对文案。
- 允许在 `data.js` 追加 `ISSUES`（部分失败演示）和 `AGENT_LABELS`；不要改已有字段语义。

---

## 3. 状态模型（`app.js` 内部）

```js
const state = {
  sessions: [], agents: [], batches: [], candidates: [],
  prefs: {
    deleteMode: 'trash',          // trash | permanent
    confirmBeforeDelete: true,
    includeSnapshots: true,
    autoScan: true,
    appearance: 'auto'            // auto | light | dark
  },
  scan: {
    status: 'done',               // idle | running | done | error
    progress: { done: 30, total: 30, current: '' },
    issues: [],                   // [{ agentId, message }]
    durationMs: 2380
  },
  filter: { agentId: 'all', query: '', storage: 'all', age: 'all', size: 'all' },
  sort: { key: 'updatedAt', dir: 'desc' },   // updatedAt | title | bytes | messages
  selected: new Set(),            // uid；跨筛选保留
  focusUid: null,                 // 详情面板当前会话；≠ 选中
  anchorUid: null,                // Shift 连选锚点
  detailOpen: true,
  density: 'comfortable',
  sheet: null,                    // settings | discover | palette | shortcuts
  dialog: null,                   // review
  menu: null,                     // context
  popover: null,                  // filter | quick
  deleting: false,
  demo: { ... }                   // 原型控制台状态
};
```

**关键语义：浏览 ≠ 选择。**
- 点击行 / ↑↓ / J K 只改变 `focusUid`，详情面板随之更新，**不勾选**。
- 勾选只能通过复选框、空格、X、`⌘A`、快速选择、右键菜单执行。
- 首次加载自动聚焦第一条可见会话（详情不空白），但**不选中**，清理按钮保持禁用。

### 派生值

```js
visibleSessions()   // 按 filter + query + sort 排序后的会话
selectedSessions()  // selected 中有会话对象（可能包含当前筛选之外的）
selectedBytes()     // 选中会话的体积和
trashBytes()        // 废纸篓模式下始终为 0；永久模式 = selectedBytes()
agentsInSelection() // [{ agent, count, bytes }]
activeInSelection() // 选中中 active=true 的条数
hiddenSelected()    // selected 中不在 visibleSessions 里的数量
```

---

## 4. DOM 契约

> ID 是硬契约；`data-*` 是行为钩子；类名由 UI 与 JS 共用，UI 可以新增装饰类，
> 但不能重命名下面列出的类。

### 4.1 根

```html
<div class="prototype" id="prototype" data-density="comfortable">
  <div class="proto-stage">
    <div class="window" id="window">
      <div class="window-titlebar">…</div>
      <div class="app" id="app" data-detail="open">…</div>
      <!-- 浮层放在 .window 内，绝对定位覆盖 .app -->
    </div>
    <div class="proto-dock" id="protoDock">…</div>
    <button class="proto-dock-toggle" id="protoDockToggle" hidden>原型</button>
  </div>
</div>
```

- 主题：`document.documentElement.dataset.theme = 'light' | 'dark'`；`auto` 时删除该属性。
- 密度：`#prototype.dataset.density`。
- 详情：`#app.dataset.detail = 'open' | 'closed'`。

### 4.2 顶栏

| ID / 选择器 | 元素 | 行为 |
|---|---|---|
| `#brandHome` | 品牌按钮 | 点击清空筛选与搜索，聚焦全部 |
| `#demoBadge` | 徽标 | 始终隐藏（原型不是 mock 后端） |
| `#searchInput` | `<input type="search">` | 输入即筛选；`⌘F` 聚焦；Esc 清空并失焦 |
| `#searchClear` | 按钮 | 清空搜索，`hidden` 控制显隐 |
| `#searchKbd` | `<kbd>` | 显示 `⌘F`；输入框有值时隐藏 |
| `#paletteBtn` | 按钮 `⌘K` | 打开命令面板 |
| `#discoverBtn` | 按钮 | 打开发现抽屉；`#discoverCount` 显示候选数 |
| `#detailToggle` | 图标按钮 | 切换详情栏；`aria-pressed` 同步 |
| `#settingsBtn` | 图标按钮 | 打开设置 |
| `#scanBtn` | 主按钮 | 开始扫描；`#scanBtnLabel` 文字在 `扫描 / 扫描中` 间切换 |
| `#scanProgress` | 顶部进度条容器 | 扫描时显示；内部 `#scanProgressBar` 用 `transform: scaleX()` |
| `#scanIssuesBtn`（可选） | 按钮 | 扫描有问题时显示数量，点击打开错误详情 |

`#scanKbd` 显示 `⌘R`，扫描中隐藏。

### 4.3 侧栏

| ID | 内容 |
|---|---|
| `#agentList` | 所有 Agent 行的容器 |
| `#agentAll` | 「全部会话」行；`data-agent="all"` |
| `#agentGroups` | JS 渲染的分组容器 |
| `#agentTotal` | 侧栏标题右侧总数（可选） |
| `#sidebarCollapse` | 窄窗口下的折叠按钮（可选） |
| `#versionLabel` | 版本号 |

JS 渲染的 Agent 行：

```html
<button class="agent-row" data-agent="claude-code" aria-pressed="false" type="button">
  <span class="agent-icon">…inline svg…</span>
  <span class="agent-name">Claude Code</span>
  <span class="agent-count">8</span>
  <span class="agent-meta">618 KB</span>
</button>
<div class="agent-group-label">命令行<span>4</span></div>
```

- 当前项：`.is-active` + `aria-pressed="true"`。
- 未安装/无会话：`.is-muted` + 右侧「未安装」，点击仍可筛选（展示空状态）。
- Agent 图标：`data.js` 的 path 生成 `<svg viewBox="0 0 24 24">`，颜色用 `currentColor`；
  外层 `.agent-icon` 背景使用品牌色的 8–10% 与深色模式的 `dark` 色。

### 4.4 列表

| ID | 内容 |
|---|---|
| `#listTitle` | 当前标题（全部会话 / Agent 名） |
| `#listMeta` | `26 条 · 5.4 MB` 或筛选结果摘要 |
| `#filterBtn` | 打开筛选浮层；`#filterCount` 显示生效的筛选数 |
| `#quickSelectBtn` | 打开快速选择菜单 |
| `#densityBtn` | 切换密度；`aria-pressed` 表示紧凑 |
| `#filterChips` | 生效筛选的 chip 容器；无筛选时 `hidden` |
| `#issueBanner` | 扫描失败提示条；含 `#issueBannerText` 与 `#issueBannerBtn` |
| `#tableWrap` | 表格滚动容器 |
| `#sessionTable` | `<table>` |
| `#sessionBody` | `<tbody>` |
| `#selectAll` | 表头三态复选框 |
| `#emptyState` | 空状态容器；内部 `#emptyTitle` `#emptyDesc` `#emptyAction` |
| `#skeleton` | 首次加载/扫描骨架容器 |

排序按钮：`button.sort-btn[data-sort="updatedAt|title|bytes|messages"]`，
外层 `<th aria-sort="ascending|descending|none">`。

行结构（JS 渲染）：

```html
<tr class="session-row" data-uid="claude-code:c1" tabindex="0"
    aria-selected="false" data-active="true">
  <td class="cell-check"><input type="checkbox" class="row-check" aria-label="选择会话"></td>
  <td class="cell-title">
    <div class="row-title-line">
      <span class="row-agent-icon agent-icon is-sm">…</span>
      <span class="row-title">重构 catalog registry 的加载顺序</span>
      <span class="badge badge-warn">可能正在使用</span>
    </div>
    <div class="row-snippet">把 Spec 的解析从 switch 改成表驱动…</div>
  </td>
  <td class="cell-project"><span class="path-text">~/Projects/agent-sweep</span></td>
  <td class="cell-num">42</td>
  <td class="cell-num cell-size">180 KB</td>
  <td class="cell-time">今天 19:32</td>
  <td class="cell-actions">
    <button class="row-more" aria-label="更多操作">…</button>
  </td>
</tr>
```

- 选中：`aria-selected="true"` + `.is-selected`。
- 焦点（详情中的会话）：`.is-focused`，且 `tabindex="0"`（其余行为 `-1`）。
- 全部会话视图显示每行 Agent 图标；单 Agent 视图隐藏（`.hide-agent-icon`）。
- 摘要为空（如 SQLite 会话）时显示「SQLite 行 · 正文在数据库中」这类说明，不留空白。
- 活动会话用警告徽标，不用红色。

### 4.5 详情

| ID | 内容 |
|---|---|
| `#detailPane` | 侧栏容器 |
| `#detailClose` | 关闭按钮 |
| `#detailBody` | 可滚动内容 |
| `#detailFoot` | 底部操作栏 |
| `#detailTitle` | 「详情」 |

详情内容由 JS 渲染，必须包含（顺序固定）：
1. `.detail-identity`：Agent 图标 + 名称 + 厂商 + 存储徽标
2. `.detail-title`：会话标题
3. `.detail-meta`：相对时间 + 消息数
4. `.detail-badges`：活动/存储/自定义
5. `.callout-warn`（有活动会话时）
6. `.detail-stats`：体积 / 消息 / 最后更新（3 格）
7. `.detail-section`「文件位置」：路径列表 + 复制/显示按钮
8. `.detail-section`「清理影响」：`.impact-row` × 4
9. `.detail-note`：Agent 说明
10. 底部：`#detailReveal`「在 Finder 中显示」+ `#detailSelect`「选中此会话 / 取消选中」

无焦点会话时显示 `.detail-empty`：图标 + 「选一条会话查看详情」+ 提示可点击行。

### 4.6 状态栏

| ID | 内容 |
|---|---|
| `#scanStatus` | 状态点 + 文案 + 进度；`role="status"` |
| `#selectionSummary` | `已选 3 条 · 180 KB` + 小字 `真实释放 0 B` |
| `#undoChip` | 最近可撤销批次：`已移入废纸篓 4 条 · 撤销`；无则 `hidden` |
| `#cleanBtn` | 清理按钮；`#cleanBtnLabel` 动态文案 |

清理按钮状态：
- 未选中：禁用，`aria-disabled="true"`，tooltip「选择会话后可清理」。
- 选中 + 废纸篓：墨色实底，`清理 3 条`。
- 选中 + 永久：危险色实底，`永久删除 3 条`。
- 删除中：禁用 + `正在清理…`。

### 4.7 浮层

| ID | 类型 | 说明 |
|---|---|---|
| `#scrim` | 遮罩 | 抽屉/对话框/命令面板共用；点击关闭 |
| `#settingsSheet` | 抽屉 | 设置 |
| `#discoverSheet` | 宽抽屉 | 发现 |
| `#reviewDialog` | 对话框 | 清理确认（空间账本） |
| `#shortcutsDialog` | 小对话框 | 快捷键表 |
| `#palette` | 命令面板 | `#paletteInput` + `#paletteResults` |
| `#filterPopover` | 浮层 | 筛选 |
| `#quickPopover` | 浮层 | 快速选择 |
| `#contextMenu` | 菜单 | 行右键/更多 |
| `#toasts` | 容器 | `aria-live="polite"` |
| `#sheetBackdrop` 可省略 | — | 用 `#scrim` 统一 |

所有浮层用 `hidden` 属性控制显隐；打开时给 `.app` 加 `.has-overlay`（用于禁用滚动/模糊）。

#### 设置抽屉控件

- `input[name="deleteMode"][value="trash|permanent"]`（用 `.segmented` 包裹）
- `#prefConfirm`、`#prefSnapshots`、`#prefAutoScan`（checkbox）
- `input[name="appearance"][value="auto|light|dark"]`
- `#batchList`（JS 渲染历史批次）
- `#openBackupsBtn`（打开备份目录，原型中 toast 说明）
- `#closeSettings` / `[data-close-sheet]`
- `#settingsVersion`

#### 发现抽屉

- `#discoverSub`：`2 个看起来像会话存储的目录`
- `#discoverList`：JS 渲染候选卡片
- `#rediscoverBtn`：重新发现（模拟 600ms）
- 卡片：`.candidate-card`，含名称、路径、置信度、证据 chips、数量/体积、`data-adopt` 按钮
- 添加后卡片变为 `.is-adopted`（「已收录」），侧栏出现新 Agent，toast 提示

#### 清理确认

- `#reviewTitle`、`#reviewLede`
- `#reviewAgents`：Agent chip 列表
- `#reviewLedger`：空间账本（3 行 + 总计）
- `#reviewActive`：活动会话警告（无则 hidden）
- `#reviewModeSeg`：模式切换（trash/permanent）
- `#reviewPermanentWarn`：永久删除警告（hidden 控制）
- `#reviewAck` + `#reviewAckRow`：永久删除确认勾选
- `#reviewCancel`、`#reviewConfirm`（文案动态）
- `#reviewError`

#### 命令面板

- `#paletteInput`
- `#paletteResults`：分组结果
- `#paletteEmpty`
- 结果行：`.palette-row[data-action]` / `[data-uid]` / `[data-agent]`
- 键盘：↑↓ 移动、Enter 执行、Esc 关闭、Tab 不离开面板

#### 快捷键对话框

静态结构：`#shortcutsList`，两组（通用 / 列表），每行 `.shortcut-row`，含 `<kbd>`。

### 4.8 原型控制台

- `#protoDock`、`#protoDockToggle`
- `input[name="dockTheme"]`（auto/light/dark）
- `#dockState` 按钮组 `data-demo="idle|scanning|empty|noresults|error"`
- `#dockPanels` 按钮组 `data-open="settings|discover|review|palette|shortcuts"`
- `#dockReset`、`#dockHide`

---

## 5. 行为规格

### 5.1 启动

1. 读取 URL 参数 → 设置主题、密度、详情、初始 Agent、演示状态、初始浮层。
2. 克隆 `PROTO_DATA` 到 `state`；派生 `agents` 的计数与体积。
3. 自动聚焦第一条可见会话（`focusUid`），不选中。
4. 渲染侧栏、列表、详情、状态栏、批次回执。
5. `state=scanning` 时自动开始扫描；`state=empty` 时清空会话；`state=noresults` 时把
   `searchInput` 设为 `"zzz-不存在"` 并聚焦；`state=error` 时注入 2 条 issues 并显示 banner。

### 5.2 扫描

- 点击 `#scanBtn` 或 `⌘R`：`scan.status='running'`，进度 0/30，按钮文案「扫描中」，
  `#scanProgress` 显示，状态栏显示 `12/30 · Cursor`，侧栏当前 Agent 状态点脉冲。
- 每 120ms 推进 2–3 个 Agent；约 1.6s 完成；完成后 `status='done'`，toast
  「扫描完成 · 26 条会话 · 5.4 MB」，按钮恢复「扫描」。
- 扫描中再次触发是幂等的（不重开）。
- `state=error`：完成后 issues 非空，顶部 banner 与状态栏警告出现；点击 banner 展开
  issue 列表（`.issue-list`），每条 `Agent 名称 · 错误信息`，附「重试」（toast 模拟）。
- 扫描中不阻塞选择与详情浏览；不显示全屏 loading。

### 5.3 搜索 / 筛选 / 排序

- 搜索：标题、项目、摘要、ID、Agent 名；不区分大小写；输入即更新（无需防抖，数据量小）。
- 搜索命中标题时用 `<mark>` 高亮（只高亮标题）；无结果显示空状态：
  「没有匹配「zzz-不存在」的会话」+「清除搜索」按钮。
- 筛选浮层（点击 `#filterBtn`）：
  - 存储：全部 / 文件 / 目录 / SQLite
  - 时间：全部 / 今天 / 7 天内 / 30 天内 / 更早
  - 体积：全部 / 小于 10 KB / 10 KB–1 MB / 大于 1 MB
  - 底部「重置筛选」；每项即时生效；激活的筛选在 `#filterChips` 显示可移除 chip。
  - 点击外部/Esc 关闭；`#filterCount` 显示非「全部」的组数。
- 排序：点击表头在 asc/desc 间切换；默认 `updatedAt desc`；`aria-sort` 同步；
  「体积」「消息」默认降序；「标题」用 `localeCompare('zh-Hans')`。
- 当前 Agent 筛选与搜索叠加；`#listTitle` 显示 Agent 名，`#listMeta` 显示结果计数与体积。

### 5.4 选择

- 复选框：切换单条；触发后 `focusUid` 也设为该行。
- 行点击 / ↑↓ / J K：只改焦点（`focusUid`），不改变选中。
- 空格 / X：切换焦点行的选中。
- `⌘A` / `Ctrl+A`：选中全部可见会话（输入框内不拦截）。
- Shift + 点击复选框 / Shift + 空格：从 `anchorUid` 到当前行的范围选择（基于当前可见顺序）。
- 表头全选：三态；点击在「全选可见」与「清空可见」间切换。
- 已选中但被筛掉的行：`#selectionSummary` 显示「其中 2 条不在当前筛选」，
  清理仍然作用于全部已选（必须明确告知）。
- `Esc`：有浮层时先关浮层；否则清空搜索；再按清空选择；再按移除焦点。
- 详情底部 `#detailSelect` 切换焦点会话的选中状态。

### 5.5 清理

1. 点击 `#cleanBtn`（有选中）：
   - `prefs.confirmBeforeDelete === true` → 打开 `#reviewDialog`；
   - 否则直接执行。
2. 打开确认时生成：会话数、体积、涉及的 Agent（含各 Agent 条数/体积）、活动会话数、
   当前模式、空间账本。
3. 模式在对话框内可切换，并与设置同步；切到「永久删除」时：
   - 显示危险警告 `#reviewPermanentWarn`；
   - 显示 `#reviewAck`，未勾选时 `#reviewConfirm` 禁用；
   - 账本总计改为真实释放体积。
4. 确认：
   - 关闭对话框，设置 `deleting=true`，按钮显示「正在清理…」；
   - 650ms 后：从 `state.sessions` 移除选中；生成 batch（trash 模式 freed=0，permanent 为体积）；
   - 清空选择；`focusUid` 指向下一条可见会话；刷新侧栏/列表/状态栏；
   - toast：trash →「已移入废纸篓 3 条 · 撤销」；permanent →「已永久删除 3 条 · 释放 180 KB」；
   - `#undoChip` 显示可撤销批次（仅 trash 且 `<24h` 的演示批次）。
5. 撤销：
   - 来自 toast 操作、`#undoChip`、设置里的批次行；
   - 恢复会话，batch.undoable=false，toast「已恢复 3 条会话」；
   - 若恢复的会话在当前筛选外，不强行改筛选，只提示「已恢复到原列表」。
6. 确认对话框的取消 / Esc / 点遮罩：关闭，不改变数据；焦点回到清理按钮。
7. 错误模拟：若 `state=error` 且处理中，可以让第 2 个会话失败，toast 显示
   「2 个会话未能清理」+「查看」；不要用 alert。

### 5.6 详情

- 焦点变化时更新详情；同一次渲染内切换用 140ms 淡入，不用滚动复位。
- 点击路径的「复制」：写剪贴板（`navigator.clipboard`，file:// 可能失败 → toast 兜底），
  toast「已复制路径」；「在 Finder 中显示」在原型中 toast「原型不会打开 Finder」。
- 活动会话显示警告 callout，文案带 Agent 名称。
- SQLite 会话：文件位置显示「正文在 SQLite 数据库中」，并列出索引库路径；提示备份整库。
- 无焦点：显示空详情。

### 5.7 发现

- 打开时：`#discoverList` 显示 3 行骨架，600ms 后显示候选（模拟网络）。
- 点击 `收录为 Agent`：卡片变已收录；向侧栏添加 Agent（`custom:true`，位于「其他」）；
  为它生成 3 条演示会话（标题含候选名）；toast「已收录 Warp Agent · 3 条会话」；
  新 Agent 自动选中？**不选中**，只滚动到并短暂高亮。
- 置信度用细进度条 + 百分比；证据 chips 最多 3 条。
- `#rediscoverBtn`：重新扫描，toast「没有新的候选目录」。
- 空候选：空状态「没有发现未收录的目录」+ 说明。

### 5.8 设置

- 所有开关即时生效；无「保存」按钮；关闭后保留。
- 清理方式切换同步到状态栏清理按钮样式与空间账本。
- 外观切换立即应用（auto 跟随系统）。
- 最近清理：列表行显示时间、条数、模式（移入废纸篓/永久删除）、真实释放、撤销按钮
  （仅 undoable）；空状态「还没有清理记录」。
- 备份目录行：点击显示 toast；说明文字用 `~/Library/Application Support/AgentSweep/backups/`。

### 5.9 命令面板

- `⌘K` 打开；打开时清空输入、聚焦。
- 空输入时显示：`动作`（扫描、打开设置、发现未收录、切换外观、切换详情、快捷键、
  撤销上次清理）、`Agent`（全部 + 每个 Agent）。
- 输入时：`会话` 结果（最多 6 条，高亮匹配）、`动作`（按关键字过滤）、`Agent`。
- ↑↓ 在扁平结果列表移动；Enter 执行；Esc 关闭；打开面板时焦点陷阱。
- 选中会话：设置焦点、关闭面板、滚动到行；选中 Agent：筛选；动作：执行。

### 5.10 快捷键

全局（输入框聚焦时不拦截，除 `⌘K`/`Esc`）：

| 键 | 行为 |
|---|---|
| `⌘K` | 命令面板 |
| `⌘F` | 聚焦搜索 |
| `⌘R` | 扫描 |
| `⌘,` | 设置 |
| `⌘⇧D` | 发现 |
| `⌘\` | 切换详情 |
| `⌘Z` | 撤销最近可撤销批次（不在输入框内） |
| `⌘A` | 选择全部可见 |
| `?` | 快捷键表 |
| `Esc` | 关闭浮层 / 清空搜索 / 清空选择 |

列表：

| 键 | 行为 |
|---|---|
| `↑` `↓` / `J` `K` | 移动焦点 |
| `Space` / `X` | 切换选中 |
| `Shift + Space` | 范围选择 |
| `Enter` | 聚焦并展开详情（若无详情则打开） |
| `⌘⌫` | 清理选中 |

### 5.11 右键菜单

- 行右键或「⋯」打开 `#contextMenu`，位置跟随鼠标/按钮，自动避让窗口边缘。
- 项：在 Finder 中显示、复制路径、选中/取消选中、清理此条（若未选中则先选中）。
- Esc / 点击外部关闭；键盘 ↑↓/Enter 可用。

### 5.12 通知

- 最多 3 条，底部居中；6 秒自动关闭；悬停暂停；带关闭按钮。
- 操作按钮最多 1 个（撤销 / 查看）。
- 出现和消失只做 8px 位移 + 淡入淡出；不阻断点击。

### 5.13 响应式

- 由 CSS 负责；JS 只处理 `#detailToggle` 的状态。
- 视口 < 1120 时默认 `data-detail="closed"`（除非 URL 指定 `detail=1`）；
  打开时详情以浮层形式盖在列表右侧（带遮罩）。
- 侧栏 < 1000 收成 64px：只留图标，名称/计数隐藏；悬停显示 title。
- 表格列按 DESIGN.md 断点隐藏；不出现横向滚动条。
- 原型窗口高度不足时，表格内部滚动，状态栏固定。

---

## 6. 视觉实现提示

- 以 `DESIGN.md` 为唯一 token 来源；在 `app.css` 的 `:root` 定义 token，
  深色用 `@media (prefers-color-scheme: dark)` + `[data-theme="dark"]`。
- 窗口内部不使用大阴影；只有 `.window` 本身（原型容器）有阴影。
- 顶部栏与状态栏用 `--canvas-deep`，列表用 `--surface`，侧栏用 `--canvas-deep`，
  详情用 `--surface`；之间用 1px 发丝线。
- 选中行左侧 2px 指示条用 `box-shadow: inset 2px 0 var(--accent)`，不改变布局。
- 复选框用原生 input 自定义外观（`appearance:none`），保证可访问。
- 所有按钮有 `:hover`、`:active`、`:focus-visible`、`:disabled` 状态。
- 行悬停不位移；只有按钮按下位移 1px。
- 表格数值列 `font-variant-numeric: tabular-nums`。
- 时间/体积格式化函数放 `app.js`，UI 不猜。
- 不使用 `innerHTML` 拼接用户可控内容；数据是本地可信的，但仍用 `textContent` 或模板转义。

---

## 7. 验收清单

- [ ] 双击 `index.html`（`file://`）无报错、无网络请求、字体/图标正常。
- [ ] 26 条会话、8 个 Agent 正确渲染；侧栏计数与体积由数据派生。
- [ ] 浏览（焦点）与选择分离；首次加载只聚焦、不勾选。
- [ ] 勾选、Shift 连选、⌘A、三态全选、快速选择均正确。
- [ ] 搜索、三类筛选、排序可组合；筛选 chip 可移除；空结果有出口。
- [ ] 详情随焦点更新；活动会话警告、SQLite 不同说明、路径复制/显示可用（toast）。
- [ ] 扫描进度、完成 toast、错误 banner 均正常；扫描中可继续浏览。
- [ ] 清理确认含空间账本；废纸篓 0 B / 永久删除真实释放；永久删除需勾选确认。
- [ ] 清理后 toast + 撤销 + 状态栏批次回执；撤销恢复成功。
- [ ] 发现抽屉可收录候选，侧栏即时更新。
- [ ] 设置即时生效；外观切换、密度切换、批次撤销正常。
- [ ] 命令面板、快捷键表、右键菜单可开可关，键盘可达。
- [ ] 浅色/深色全部状态可读；对比度与焦点环可见。
- [ ] 1180×760、1440×900、960×700 三档不溢出、不横向滚动。
- [ ] `prefers-reduced-motion` 下无动画。
