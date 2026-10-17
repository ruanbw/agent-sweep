# AgentSweep

扫描本机所有 AI Agent 的会话记录，勾选后清理。**默认进废纸篓，可撤销。**

<p align="center"><sub>Go + Wails v3 + 系统 WKWebView + 手写前端（无框架、无 bundler、无 bindings 生成器）</sub></p>

<img src="docs/screenshot.png" alt="AgentSweep 主界面：左侧 Agent 列表、中间会话表格、右侧会话详情" width="1100">

<sub>截图是内置演示数据（`index.html` 双击即可打开），不是真实会话。</sub>

---

## 为什么重做

上一版（[conversation-clean](https://github.com/ruanbw/conversation-clean)）是 Electron + React，
给 15 个 Agent 各写了一个扫描器。它有两个结构性问题：

1. **名单写死在代码里。** `AGENT_CATEGORIES` 是个 const 元组，新增一款 Agent = 改类型 + 改注册表 + 改 UI 文案 + 发版。
   市面上每周都在冒新的 Agent，这条路走不通。AgentSweep 内置 37 款，剩下的交给指纹发现。
2. **删了正文，没删索引。** 很多 Agent 同时维护「会话文件 + SQLite / JSON 索引」。
   只删文件，Agent 侧会留下一批点进去是空的幽灵会话 —— 比不删还糟。

AgentSweep 的答案是三件事：**声明式目录**、**指纹发现**、**备份后清理**。

## 三个设计决定

### 1. 一个 Agent 是一份声明，不是一段代码

`internal/catalog/spec.go` 把「一款 Agent」压成四个字段：数据根、会话形态、索引位置、说明。
目前只有四种形态，覆盖了所有见过的 Agent：

| 形态 | 含义 | 例子 |
|---|---|---|
| `jsonl` | 一个 `.jsonl` = 一条会话 | Claude Code、Codex、Pi |
| `json` | 一个 `.json` = 一条会话 | Gemini CLI、Continue |
| `dir` | 一个子目录 = 一条会话 | Cline、DSH |
| `sqlite` | 表里一行 = 一条会话 | Zed、opencode、Antigravity |

新增内置 Agent = 往 `internal/catalog/builtin.go` 加一条数据。不碰引擎。

### 2. 目录里没有的 Agent，靠指纹找

`internal/probe` 不看目录名白名单，而是看**内容**：JSONL 里有没有 `role`/`content`、
SQLite 里有没有 `session`/`thread`/`message` 表、目录结构是不是「一会话一目录」。
命中就给一个置信度，并生成一份可以直接保存的适配器草稿。

这是「支持市面上所有 Agent」真正成立的原因 —— 不需要我们认识它。

### 3. 删除是可逆的

一次清理 = 先备份所有会被改到的索引 / SQLite 库 → 正文进废纸篓 → 裁索引 → 写 journal。
撤销按 journal 逆序还原。

- 文件型会话进 `~/.Trash/AgentSweep/<批次>/`，删错了拖回来就行
- SQLite 型会话（Zed / opencode）删行前整库备份，撤销直接还原文件
- 索引先备份再改，改坏了也能回去

**「进废纸篓」不计入「已释放空间」** —— 它没释放。UI 上这两个数字分开说。

## 架构

```
main.go                    一个二进制，两个入口：GUI / CLI
internal/catalog/          Agent 声明 + 内置目录 + 用户适配器
internal/probe/            指纹发现（未知 Agent）
internal/session/          统一会话模型 + 各格式解析启发式
internal/scan/             只读扫描引擎（并发、按 Agent 隔离错误）
internal/clean/            备份 → 进废纸篓 / 删行 → 裁索引 → journal → 撤销
internal/gui/              Wails v3 壳 + 同一个 http.Handler 上的静态资源与 /api
internal/gui/assets/       手写 HTML/CSS/JS，embed 进二进制
```

前后端只有一份契约：[`docs/api.md`](docs/api.md)。没有 bindings 生成器、没有 websocket、
没有 node 工具链 —— `go build` 就是全部构建步骤。

## 构建

```bash
make build      # bin/AgentSweep
make app        # AgentSweep.app（手工拼 bundle，不依赖 wails CLI）
make run        # 构建并打开
make test
```

要求：Go 1.25+、Xcode Command Line Tools（cgo 链接 WebKit）。macOS 12+。

## CLI

```bash
agentsweep                        # 打开窗口
agentsweep scan                   # 表格
agentsweep scan --json            # 与 HTTP 同构的 JSON
agentsweep clean --agent dsh      # 移入废纸篓
agentsweep clean --agent dsh --permanent
agentsweep discover               # 找出目录里没有的 Agent
agentsweep serve --addr :8766     # 只开 HTTP（调试 UI / 无窗口环境）
```

## 加一款目录里没有的 Agent

1. 打开「发现」页，找到它的数据目录
2. 点「收录为 Agent」，草稿会自动填好数据根、形态和 glob
3. 回到列表就能看到会话

草稿写进 `~/Library/Application Support/AgentSweep/adapters.json`，不改代码、不用重启。
内置目录与用户适配器同 ID 时，用户版本优先。

## 已知边界

- **只在 macOS 上验证过。** 路径表是 macOS 布局；Linux / Windows 的根目录不同（`~/.config`、`%APPDATA%`），
  目录声明里写的是 `~/...`，换平台要补一份路径表。
- **SQLite 型会话算不出单条体积。** 一行占多少盘取决于页大小和共享页，硬报一个数字是撒谎，
  所以列表里显示 0，真实释放量按 VACUUM 前后文件差算。
- **部分 Agent 的删除需要它自己没在运行。** 库被锁住时删除会失败并回报错误，不会静默跳过。
- 目录是活的。写错的路径代价很低（显示未安装），写漏的代价是用户找不到会话 —— 欢迎补 PR。
