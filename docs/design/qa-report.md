# AgentSweep 原型验收报告

> 对象：`docs/design/prototype/`（高保真可交互原型）
> 方式：本机 Chrome headless + CDP，零依赖脚本（`docs/design/tools/shoot.mjs`）
> 结论：**通过。** 26/26 截图、33/33 主流程断言、17/17 边界场景断言；console error / warning 为 0，外部请求为 0。无阻断或高优先级问题。

---

## 1. 环境

| 项 | 值 |
|---|---|
| 日期 | 2026-10-17（Asia/Shanghai） |
| macOS | 本机 macOS，Chrome 155.0.8059.82（headless） |
| Node | v24.21.0（全局 `WebSocket` / `fetch`） |
| 页面 | `file://…/docs/design/prototype/index.html` |
| 视口 | 1440×900（主）、1180×760、960×700 |
| 动效 | 截图矩阵启用 `prefers-reduced-motion: reduce`，保证确定性 |
| 依赖 | 无 npm 包、无网络、无外部字体/图片 |

复现命令：

```bash
node docs/design/tools/shoot.mjs smoke          # 工具自检 14/14
node docs/design/tools/shoot.mjs matrix \
  --base docs/design/prototype/index.html \
  --out docs/design/screenshots \
  --report /tmp/agentsweep-final-shots.json --strict

node docs/design/tools/qa-flows.mjs             # 33 项主流程断言
node docs/design/tools/qa-edge-cases.mjs        # 17 项边界场景断言
```

---

## 2. 截图矩阵（26/26 PASS）

全部使用 `dock=0`（隐藏原型控制台）、`theme=light|dark`，输出到 `docs/design/screenshots/`。

| 编号 | 场景 | 文件 |
|---|---|---|
| 01–02 | 主界面 | `01-main-light.png` / `02-main-dark.png` |
| 03–04 | 扫描中 | `03-scanning-light.png` / `04-scanning-dark.png` |
| 05–06 | 空列表 | `05-empty-light.png` / `06-empty-dark.png` |
| 07–08 | 无搜索结果 | `07-noresults-light.png` / `08-noresults-dark.png` |
| 09–10 | 部分 Agent 失败 | `09-error-light.png` / `10-error-dark.png` |
| 11–12 | 设置抽屉 | `11-settings-light.png` / `12-settings-dark.png` |
| 13–14 | 发现抽屉 | `13-discover-light.png` / `14-discover-dark.png` |
| 15–16 | 清理确认（空间账本） | `15-review-light.png` / `16-review-dark.png` |
| 17–18 | 命令面板 | `17-palette-light.png` / `18-palette-dark.png` |
| 19–20 | 快捷键表 | `19-shortcuts-light.png` / `20-shortcuts-dark.png` |
| 21–22 | 右键菜单 | `21-context-light.png` / `22-context-dark.png` |
| 23–24 | 紧凑密度 | `23-compact-light.png` / `24-compact-dark.png` |
| 25–26 | 960 窄窗口 | `25-narrow-light.png` / `26-narrow-dark.png` |
| 00 | 改造前参考 | `00-before-current-ui.png` |

每张截图均通过：非空白（颜色数 ≥ 400、标准差 ≥ 15）、无横向溢出、无 console error。扫描中/错误态的 `rows=26`，空态/无结果态 `rows=0`，浮层场景均能按 query 正确打开。

---

## 3. 主流程断言（33/33 PASS）

| # | 断言 | 结果 |
|---|---|---|
| 1 | 初始 26 条会话 / 0 选择 / 清理禁用 | PASS |
| 2 | 点击行只更新焦点与详情，不勾选 | PASS |
| 3 | 勾选 2 条后状态栏与按钮同步 | PASS |
| 4 | 快速选择「超过 30 天」**替换**为 5 条 | PASS |
| 5 | Shift 连选区间 = 5 条 | PASS |
| 6 | 首次加载只聚焦第一条、不勾选 | PASS |
| 7 | 复选框聚焦时 ⌘A 全选 26 条 | PASS（本轮修复） |
| 8 | 搜索命中与计数正确 | PASS |
| 9 | Esc 清空搜索并恢复列表 | PASS |
| 10 | 存储筛选 SQLite = 3 条 + 可移除 chip | PASS |
| 11 | 按体积降序首行为 2.1 MB | PASS |
| 12 | 清理确认：3 条标题 + 3 行账本 + 总计 | PASS |
| 13 | 账本「已备份」且不编造索引体积 | PASS |
| 14 | 活动会话警告点名 Claude Code | PASS |
| 15 | 永久删除：警告 + 勾选门槛 + 确认禁用 | PASS |
| 16 | 勾选后确认可用 | PASS |
| 17 | 删除 3 条 + toast 撤销 + 状态栏批次回执 | PASS |
| 18 | 撤销恢复 26 条 + 「已恢复」回执 | PASS |
| 19 | 发现抽屉 2 个候选 + 「收录为 Agent」 | PASS |
| 20 | 收录后侧栏 8→9 行、已收录、toast | PASS |
| 21 | 设置外观即时切换深色 | PASS |
| 22 | 永久模式同步状态栏清理按钮 | PASS |
| 23 | 最近清理列表可滚动 | PASS |
| 24 | 密度切换紧凑 | PASS |
| 25 | 命令面板打开、↑↓ 移动、分组存在 | PASS |
| 26 | 右键菜单打开且 4 项 | PASS |
| 27 | `?` 打开快捷键表（15 行） | PASS |
| 28 | 复制路径有 toast 兜底 | PASS |
| 29 | toast 约 6s 自动消失 | PASS |
| 30 | aria：行/Agent/sort/live/progressbar 语义 | PASS |
| 31 | reduced-motion 媒体查询存在 | PASS |
| 32 | 1440×900 无横向滚动 | PASS |
| 33 | Ctrl/⌘ 路径之外的焦点与关闭行为一致 | PASS |

---

## 4. 边界场景断言（17/17 PASS）

| # | 断言 | 结果 |
|---|---|---|
| 1 | 扫描中：进度、按钮、列表保留可浏览 | PASS |
| 2 | 扫描完成 toast + 状态恢复 | PASS |
| 3 | 部分失败 banner 显示 2 个 Agent | PASS |
| 4 | banner 展开逐 Agent 明细 + 重试 | PASS |
| 5 | 重试后 issue 减少 | PASS |
| 6 | 时间筛选「更早」= 5 条 | PASS |
| 7 | 体积筛选「大于 1 MB」= 2 条 | PASS |
| 8 | 快速选择空会话 = 2 条 | PASS |
| 9 | 快速选择最大 10 条 = 10 条 | PASS |
| 10 | 快速选择清除 = 0 条 | PASS |
| 11 | J 移动焦点且不选择 | PASS |
| 12 | 详情开关收起 | PASS |
| 13 | 详情开关展开 | PASS |
| 14 | 无结果空态 + 清除入口 | PASS |
| 15 | 清除搜索恢复 26 条 | PASS |
| 16 | ⌘Z 撤销最近清理 | PASS |
| 17 | 设置开关可切换 | PASS |

---

## 5. 无障碍抽查

- **Tab 顺序**：skip-link → 品牌 → 搜索 → ⌘K → 发现 → 详情 → 设置 → 扫描，顺序符合阅读顺序；表格行使用 roving tabindex。
- **焦点环**：键盘移动后行焦点环为 `2px solid #3F6599`、offset `-2px`，可见、不被裁切。
- **焦点陷阱**：设置抽屉内连续 Tab 8 次，焦点始终留在抽屉内；清理确认打开后焦点在对话框内。
- **焦点归还**：清理确认关闭后回到 `#cleanBtn`；设置抽屉关闭后回到 `#settingsBtn`；发现抽屉关闭后回到 `#discoverBtn`（均在一帧后验证）。
- **ARIA**：`aria-selected`（行）、`aria-pressed`（Agent）、`aria-sort`（表头）、`aria-live="polite"`（toast/状态）、`role="progressbar"` + `aria-valuenow`（扫描）均存在。
- **对比度**（相对 `--surface`，实测）：
  - 浅色：正文 15.8:1、次级 5.6:1、元信息 5.1:1、石板蓝 5.7:1、警告 5.2:1、危险 5.2:1、成功 5.2:1。
  - 深色：正文 14.6:1、次级 7.7:1、元信息 4.8:1、强调 8.3:1。
  - 均满足 WCAG AA（小字 ≥ 4.5:1）。
- **reduced motion**：`prefers-reduced-motion: reduce` 下行进入动画时长为 0s、transition 约 0.00001s，扫描脉冲关闭。
- **触控目标**：图标按钮 28–32px，相邻间距 ≥ 8px；桌面场景满足。

---

## 6. 响应式

| 视口 | 结果 |
|---|---|
| 1440×900 | 窗口 1360，三栏完整；项目/消息列可见；无横向滚动 |
| 1180×760 | 窗口约 1184，三栏完整；消息列隐藏、项目列保留；无横向滚动 |
| 960×700 | 窗口约 864，侧栏收成 64px 图标栏，详情默认收起，项目/消息列隐藏；无横向滚动 |

所有场景状态栏固定、表格内部滚动，内容不被 footer 遮挡；设置抽屉内容独立滚动。

---

## 7. 已知问题（均为 Low / 打磨项，不影响交付）

| 级别 | 问题 | 建议 |
|---|---|---|
| Low | 设置抽屉「最近清理」在 760 高窗口下最后一条初始位于折叠线下方（body 可滚动，已验证 `overflow:auto`） | 压缩 batch 行高，或给列表底部加渐隐提示 / 把备份路径固定到 footer |
| Low | 1180 窗口下带「可能正在使用」徽标的长标题会省略 | 已加 `title` 全文；可把列表徽标缩为「使用中」或改为悬停展开 |
| Low | 960 窄窗隐藏项目/消息列 | 符合设计；完整信息在详情面板，可在首次窄窗时用 tooltip 说明 |
| Low | `file://` 下 `navigator.clipboard` 可能被浏览器拒绝 | 已有 toast 兜底，验证通过 |
| Info | 原型的窗口栏/交通灯是视觉模拟 | 生产环境使用 macOS 原生窗口栏，不在 HTML 中实现 |
| Info | 原型控制台只用于评审 | 生产构建不包含；`?dock=0` 可隐藏 |

---

## 8. 本轮验收中修复的问题

1. **复选框聚焦后 ⌘A 失效**：`isTypingTarget` 把 checkbox 当文本输入；已改为只吞文本类输入。
2. **无结果状态仍有 1 条命中**：演示 query 从 `keychain` 改为不命中的关键词。
3. **快速选择追加而非替换**：改为替换当前选择，符合 UX 决策 Q4。
4. **确认框活动 Agent 回退成「Agent」**：改为使用实际 Agent 名（如 Claude Code）。
5. **空间账本编造索引体积**：移除 8KB×文件数估算，改为「已备份」状态。
6. **详情关键信息被折在下方**：把「清理影响」移到「文件位置」之前。
7. **1180 窗口标题/项目列被挤爆**：改为容器查询 + 项目 basename + 隐藏消息列。
8. **焦点行看起来像已勾选**：改为左侧 2px 指示 + 4% tint，选中态才满宽着色。

---

## 9. 结论

原型已达到可评审、可演示、可据以实现的标准：

- 设计系统、UX 文档、可交互原型、验收工具、27 张截图与验收报告全部就位；
- 主流程 33/33、边界 17/17、截图 26/26 全部通过；
- 无 console error/warning、无外部请求、无横向溢出；
- 唯一剩余问题均为低优先级打磨项，不阻塞设计评审或后续落地。
