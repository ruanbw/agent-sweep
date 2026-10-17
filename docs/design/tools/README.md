# 原型验收工具 `shoot.mjs`

零依赖的 Chrome DevTools Protocol 工具：启动本机 Chrome headless，打开 `file://` 原型，
等待渲染、截图、点击、按键、读取 console、执行断言，并支持按场景批量截图。

> 本工具只读取 `docs/design/prototype/`，不会修改原型文件，不会安装 npm 依赖，不访问外网。
> 正式截图（`docs/design/screenshots/`）由 task-4 生成；本任务的自测产物全部写入 `/tmp/`。

---

## 1. 环境与自检

| 项 | 值 |
|---|---|
| Node.js | **v24.21.0**（需要 ≥ 22，使用全局 `WebSocket` / `fetch`） |
| Chrome | `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`（155.0.8059.82） |
| Chromium 回退 | `~/Library/Caches/ms-playwright/chromium-1208/chrome-mac/Chromium.app/Contents/MacOS/Chromium` |
| npm 依赖 | 无 |
| 自检命令 | `node docs/design/tools/shoot.mjs smoke` |

自检结果（本机）：

- `smoke` **PASS 14/14**：截图非空白、按颜色采样验证截到真实渲染、console.error/warn 收集、
  `click()`、`fill()`、`key()`（⌘A / Escape）、`waitFor()`（先确认元素隐藏、再等待出现）、
  `evaljs()` 返回值、无外部网络请求、`inspect()` 快照。
- 自检产物：
  - `/tmp/agentsweep-shoot-smoke/smoke-main.png`
  - `/tmp/agentsweep-shoot-smoke/smoke-swatch.png`
  - `/tmp/agentsweep-shoot-smoke/result.json`
- 批量矩阵试跑（临时 HTML，非正式验收）：`/tmp/agentsweep-matrix-smoke/`，4/4 张成功，
  验证了文件名、query、报告与 `--strict` 行为；`--only settings` 稳定输出 `11-settings-light.png`。
- 真实原型冒烟（仅证明工具可用，**不是** task-4 验收结论）：以 2026-10-17 20:07 的原型运行
  浅色 13 个场景，13/13 张成功、0 console error、`panel=*` 浮层均按 query 打开；
  产物在 `/tmp/agentsweep-prototype-smoke2/`，未写入 `docs/design/screenshots/`。

---

## 2. 快速开始

```bash
# 1) 工具自检（不依赖原型，约 3 秒）
node docs/design/tools/shoot.mjs smoke

# 2) 单张截图
node docs/design/tools/shoot.mjs shot \
  --url "file://$PWD/docs/design/prototype/index.html?theme=dark&state=idle&dock=0" \
  --out /tmp/agent-sweep-main-dark.png --width 1440 --height 900 --wait 400

# 3) 读取页面状态 / 断言
node docs/design/tools/shoot.mjs eval \
  --url "file://$PWD/docs/design/prototype/index.html?theme=light&state=idle&dock=0" \
  --expr "({rows:document.querySelectorAll('#sessionBody tr.session-row').length, title:document.getElementById('listTitle')?.textContent})"

# 4) 全量截图矩阵（13 场景 × 浅/深 = 26 张）
node docs/design/tools/shoot.mjs matrix \
  --base docs/design/prototype/index.html \
  --out docs/design/screenshots \
  --report /tmp/agentsweep-shots.json \
  --strict

# 5) 只补拍某几个场景 / 某个主题
node docs/design/tools/shoot.mjs matrix --only settings,review --themes dark \
  --out /tmp/retake --report /tmp/retake.json
```

> `--url` 里带 query 时建议直接使用 `file://` 绝对 URL；`--base` 接受路径或 URL。

---

## 3. 截图矩阵

每个场景先浅色、后深色，编号 01–26；全部使用 `dock=0`（隐藏原型控制台）、
默认视口 `1440×900`（`narrow` 为 `960×700`）、`prefers-reduced-motion: reduce`。

| # | 文件 | 场景 | 主要 query | 额外等待 |
|---|---|---|---|---|
| 01/02 | `01-main-light.png` / `02-main-dark.png` | 主界面 | `state=idle` | 450ms |
| 03/04 | `03-scanning-light.png` / `04-scanning-dark.png` | 扫描中 | `state=scanning` | 260ms |
| 05/06 | `05-empty-light.png` / `06-empty-dark.png` | 空列表 | `state=empty` | 350ms |
| 07/08 | `07-noresults-light.png` / `08-noresults-dark.png` | 无结果 | `state=noresults` | 350ms |
| 09/10 | `09-error-light.png` / `10-error-dark.png` | 部分失败 | `state=error` | 700ms |
| 11/12 | `11-settings-light.png` / `12-settings-dark.png` | 设置 | `panel=settings` | 500ms |
| 13/14 | `13-discover-light.png` / `14-discover-dark.png` | 发现 | `panel=discover` | 1100ms（等待 600ms 骨架结束） |
| 15/16 | `15-review-light.png` / `16-review-dark.png` | 清理确认 | `panel=review` | 500ms |
| 17/18 | `17-palette-light.png` / `18-palette-dark.png` | 命令面板 | `panel=palette` | 500ms |
| 19/20 | `19-shortcuts-light.png` / `20-shortcuts-dark.png` | 快捷键 | `panel=shortcuts` | 500ms |
| 21/22 | `21-context-light.png` / `22-context-dark.png` | 右键菜单 | `panel=context` | 500ms |
| 23/24 | `23-compact-light.png` / `24-compact-dark.png` | 紧凑密度 | `density=compact` | 450ms |
| 25/26 | `25-narrow-light.png` / `26-narrow-dark.png` | 窄窗口 | `state=idle`（960×700） | 450ms |

- 所有 query 都会与 `theme=<light|dark>`、`dock=0` 合并，例如：
  `index.html?theme=dark&dock=0&panel=settings`。
- `--only` / `--themes` 只决定**输出哪些**，文件名编号始终按完整矩阵计算：
  `--only settings` 仍生成 `11-settings-light.png` / `12-settings-dark.png`，方便补拍。
- 先写临时目录再确认，避免污染正式截图；工具不会删除 `--out` 中已有文件。

### 报告（`--report`）

```jsonc
{
  "summary": {
    "chrome": "/Applications/.../Google Chrome",
    "chromeVersion": "HeadlessChrome/155...",
    "baseUrl": "file:///.../index.html",
    "outDir": "/.../docs/design/screenshots",
    "total": 26, "failed": 0, "withConsoleErrors": 0, "ok": true
  },
  "results": [{
    "filename": "01-main-light.png",
    "scene": "main", "theme": "light",
    "query": "theme=light&dock=0&state=idle",
    "url": "file:///.../index.html?theme=light&dock=0&state=idle",
    "viewport": { "width": 1440, "height": 900 },
    "ms": 1200,
    "ok": true,
    "stats": {
      "width": 1440, "height": 900, "bytes": 183420,
      "distinctColors": 4210, "stdDev": 47.2,
      "transparentRatio": 0, "blank": false, "center": [244, 242, 236, 255]
    },
    "inspect": {
      "theme": "light", "density": "comfortable", "detail": "open",
      "rows": 26, "agentRows": 9, "selected": 0, "focused": 1,
      "openOverlays": ["settingsSheet"], "toastCount": 0,
      "horizontalScroll": { "document": 0, "window": 0, "table": 0 },
      "viewport": { "width": 1440, "height": 900, "dpr": 1 }
    },
    "consoleErrors": [], "consoleWarnings": [], "externalRequests": [],
    "warnings": ["主题参数=light，实际 data-theme=auto"], "error": null
  }]
}
```

- `consoleErrors`：`console.error`、未捕获异常、`Log` 域 error（去重后，含 url/line）。
- `externalRequests`：排除 `file:` / `data:` / `blob:` / `about:` / `chrome*:` 后的请求；
  原型要求为空。
- `warnings`：主题不一致、外部请求、非 fatal 的异常都会被记录，但不让 `summary.ok` 变 false。
- `--strict`：`failed > 0`（导航失败/空白截图）或存在 console 错误时进程退出码 1；
  不加 `--strict` 时仍返回报告，适合先观察再修。

### 空白截图判定

工具内置最小 PNG 解码（`zlib.inflateSync` + unfilter，支持 Chrome 的 8-bit RGB/RGBA），
统计取样像素的 `distinctColors` / `stdDev` / `transparentRatio`：

- 透明像素 > 99%，或颜色 ≤ 2 且亮度标准差 < 2 → 判定 `blank: true`（矩阵记为失败）；
- 同时返回中心像素、字节数、尺寸，便于人工复核。

---

## 4. Node API

```js
import {
  launchBrowser, analyzePng, shootMatrix, runSmoke, SCENES, buildMatrix,
} from './docs/design/tools/shoot.mjs';

const browser = await launchBrowser();            // 自动找 Chrome；--chrome 可指定
const page = await browser.newPage({ width: 1440, height: 900 });

try {
  await page.open('file:///.../index.html?theme=light&state=idle&dock=0', {
    waitMs: 400,              // 在 load + 字体 + 两帧 rAF 之后额外等待
    waitFor: '#sessionTable', // 可选：等元素出现
    reducedMotion: true,      // 可选：默认 true
    colorScheme: 'dark',      // 可选：模拟 prefers-color-scheme（theme=auto 时有用）
  });
  // page.page(url, opts) 是 page.open 的别名（与任务描述命名一致）

  await page.click('#settingsBtn');               // 真实鼠标事件
  await page.waitFor('#settingsSheet');
  await page.click('input[name="dockTheme"][value="dark"]');

  await page.key('k', { meta: true });            // ⌘K
  await page.key('Escape');
  await page.key(' ', { shift: true });           // Shift+Space（范围选择）
  await page.key('a', { meta: true });            // ⌘A
  await page.fill('#searchInput', 'keychain');    // 直接写值 + input/change
  await page.type('补充文字');                     // 聚焦元素的输入
  await page.hover('.session-row');

  const rows = await page.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length");
  const state = await page.inspect();             // 原型状态快照（rows/overlays/overflow…）

  const shot = await page.screenshot({ file: '/tmp/main.png' });
  console.log(shot.stats.blank, shot.stats.distinctColors);

  console.log(page.consoleErrors);                // 原始收集
  console.log(page.uniqueErrors());               // 去重
  console.log(page.consoleWarnings);
  console.log(page.externalRequests);

  // 选择 3 条 → 打开清理确认（示例）
  await page.click('.row-check', { index: 0 });
  await page.click('.row-check', { index: 1 });
  await page.click('.row-check', { index: 2 });
  await page.click('#cleanBtn');
  await page.waitFor('#reviewDialog');
} finally {
  await page.close();
  await browser.close();
}
```

### `Page` 方法速查

| 方法 | 说明 |
|---|---|
| `open(url, opts)` / `page(url, opts)` | 等待 `document.readyState==='complete'`、字体、两帧 rAF，再等 `waitMs` |
| `evaljs(expression, opts)` | `Runtime.evaluate`，`returnByValue + awaitPromise`；异常抛错 |
| `waitFor(selector, {timeout, visible})` | 等待元素出现（默认要求可见） |
| `waitForHidden(selector, {timeout})` | 等待元素隐藏 |
| `waitForFunction(expression, {timeout})` | 自定义条件轮询 |
| `click(selector, {index, button, meta, shift, ctrl, alt})` | 计算元素中心并派发真实鼠标事件；`button:'right'` 可右键 |
| `hover(selector)` | 移动鼠标 |
| `key(name, {meta, shift, ctrl, alt})` | 真实按键；支持单字符、`Escape`、`Enter`、`Tab`、`Space`、方向键、`?` |
| `fill(selector, value)` | 设置输入框值并派发 `input`/`change` |
| `type(text)` | 向当前焦点输入文本（`Input.insertText`） |
| `screenshot({file, fullPage, clip})` | 返回 `{ buffer, path, stats }`；`clip` 为 CSS 像素 |
| `inspect()` | 读取 rows / agents / overlays / theme / 横向溢出 / 视口等快照 |
| `setViewport(w,h,dpr)` | 运行时改视口 |
| `uniqueErrors()` / `clearConsole()` | 去重错误 / 清空收集器 |
| `close()` | 关闭标签页 |

### 批量 API

```js
const { results, summary } = await shootMatrix({
  base: 'docs/design/prototype/index.html',
  outDir: '/tmp/shots',
  only: ['main', 'settings'],
  themes: ['light'],
  width: 1440,
  height: 900,
  report: '/tmp/shots/report.json',
});
```

---

## 5. 已知限制与注意事项

1. **`file://` 与剪贴板。** 原型是本地文件页面，`navigator.clipboard.writeText()` 可能因
   “非安全上下文 / 无用户授权”失败；`evaljs` 已带 `userGesture: true`，但工具不授予
   clipboard 权限。验收时应验证 app.js 的 toast 兜底路径，而不是要求剪贴板真的写入。
2. **字体。** 只用系统字体（macOS SF Pro / PingFang SC）。如果本机缺少某字体，截图与原设计
   会有细微差异；原型禁止网络字体，因此不存在字体加载请求。
3. **`theme=auto`。** headless 默认按浅色渲染。要验证深色请显式传 `theme=dark`，
   或在 API 中使用 `open(url, {colorScheme:'dark'})`；批量矩阵始终显式传主题。
4. **动画。** 为保证截图确定性，`matrix` 默认模拟 `prefers-reduced-motion: reduce`；
   需要真实动效时加 `--no-reduced-motion`（单帧结果仍可能受时序影响）。
5. **`panel=review` 的前提。** 直接打开清理确认依赖 `app.js` 按 CONTRACT 生成演示选择；
   若实现未生成选择，截图会显示 0 条。此时应使用 API 先点击行复选框、再点 `#cleanBtn`，
   或把该问题记为原型缺陷。
6. **模拟窗口。** 截图包含 `.proto-stage` 纸纹与模拟窗口标题栏，不含 macOS 原生窗口边框；
   与生产截图对比时只看窗口内部。
7. **每张截图使用独立标签页。** 避免上一场景的 `localStorage` / 动画 / 焦点状态串扰；
   代价是无法通过截图验证“跨会话持久化”，需要持久化时请在同一 `Page` 内操作。
8. **PNG 分析范围。** 仅支持 8-bit 非隔行 PNG（Chrome 截图格式）；JPEG 等格式只报字节数。
9. **端口与进程。** 使用 `--remote-debugging-port=0` + 临时 `--user-data-dir`，不会占用固定端口、
   不会读写用户日常 Chrome profile；异常退出可用 `pkill -f agentsweep-cdp-` 清理残留。
10. **无网络。** 工具本身不请求外网；`externalRequests` 只是被动观察，不做请求拦截/断网。
11. **无触摸 / 输入法。** 只覆盖鼠标与键盘；中文文本请用 `fill()`（不会触发 IME 组合事件）。
12. **Node 版本。** Node < 22 没有全局 `WebSocket`，会直接报错；请使用本机 Node 24。
13. **并行开发期间。** 如果 `index.html` / `app.css` / `app.js` 正在被其他成员写入，截图可能
    捕获到中间状态（例如资源 404、半渲染表格）。正式验收前重新运行即可；工具会把
    缺失资源记录为 console error。
14. **正式截图目录。** 本任务不写入 `docs/design/screenshots/`；`matrix` 默认输出到该目录，
    仅由 task-4 执行。

---

## 6. 故障排查

| 现象 | 处理 |
|---|---|
| `找不到 Chrome/Chromium` | 用 `--chrome /path/to/Chrome` 或 `export CHROME_PATH=...`；Chromium 回退路径见上 |
| `等待 DevToolsActivePort 超时` | 检查是否有安全软件拦截 headless Chrome；查看工具打印的 stderr 尾部 |
| 截图全白 / `blank: true` | URL 是否正确、`--wait` 是否过短、页面是否报错；先用 `eval` 看 `#app` 是否存在 |
| `consoleErrors` 有资源 404 | 通常是 `app.css` / `data.js` 未写完，或相对路径错误 |
| `waitFor` 超时 | 检查选择器；用 `eval` 打印 `document.body.innerHTML.slice(0,500)` 定位 |
| 主题警告 `实际 data-theme=auto` | app.js 未读取 `theme` 参数，或 URL 被覆盖；检查 query 与初始化顺序 |
| 想清理 Chrome 残留 | `pkill -f agentsweep-cdp-`（临时 user-data-dir 前缀） |

---

## 7. 文件

```
docs/design/tools/
  shoot.mjs     CDP 客户端 + PNG 分析 + 矩阵 + smoke 自检 + CLI
  README.md     本文件
```

正式验收命令（task-4）：

```bash
node docs/design/tools/shoot.mjs matrix \
  --base docs/design/prototype/index.html \
  --out docs/design/screenshots \
  --report /tmp/agentsweep-shots.json \
  --strict
```
