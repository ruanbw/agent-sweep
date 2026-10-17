#!/usr/bin/env node
/**
 * AgentSweep 原型验收工具：零依赖 Chrome CDP 截图 / 交互 / 回归。
 *
 * 依赖：
 *   - Node.js >= 22（使用全局 WebSocket / fetch；本机为 Node 24）
 *   - 本机 Chrome 或 Chromium（可用 --chrome / CHROME_PATH 指定）
 *   - 不安装任何 npm 包，也不访问网络（页面本身也不应发出网络请求）
 *
 * 用法见同目录 README.md；自检：`node docs/design/tools/shoot.mjs smoke`
 */

import { spawn } from 'node:child_process';
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { inflateSync } from 'node:zlib';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, '..', '..', '..');
const PROTOTYPE_PATH = path.resolve(REPO_ROOT, 'docs', 'design', 'prototype', 'index.html');
const DEFAULT_OUT_DIR = path.resolve(REPO_ROOT, 'docs', 'design', 'screenshots');

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/* ------------------------------------------------------------------ */
/* Chrome 进程与 CDP 连接                                              */
/* ------------------------------------------------------------------ */

const CHROME_CANDIDATES = [
  process.env.CHROME_PATH,
  process.env.CHROME_BIN,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  path.join(
    process.env.HOME || '',
    'Library/Caches/ms-playwright/chromium-1208/chrome-mac/Chromium.app/Contents/MacOS/Chromium',
  ),
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];

export function findChrome(explicit) {
  const candidates = explicit ? [explicit] : CHROME_CANDIDATES;
  for (const candidate of candidates) {
    if (candidate && existsSync(candidate)) return candidate;
  }
  throw new Error(
    `找不到 Chrome/Chromium。可用 --chrome /path/to/chrome 或环境变量 CHROME_PATH 指定。\n已尝试：\n  ${candidates
      .filter(Boolean)
      .join('\n  ')}`,
  );
}

const ACTIVE_BROWSERS = new Set();
let signalInstalled = false;

function installSignalCleanup() {
  if (signalInstalled) return;
  signalInstalled = true;
  const killAll = () => {
    for (const browser of ACTIVE_BROWSERS) {
      try {
        browser.proc.kill('SIGKILL');
      } catch {
        /* ignore */
      }
    }
  };
  process.once('SIGINT', () => {
    killAll();
    process.exit(130);
  });
  process.once('SIGTERM', () => {
    killAll();
    process.exit(143);
  });
}

/** 最小 CDP 客户端：一个 WebSocket 对应一个 target。 */
class CDP {
  constructor(wsUrl) {
    this.wsUrl = wsUrl;
    this.ws = null;
    this.seq = 0;
    this.pending = new Map();
    this.handlers = new Map();
    this.closed = false;
  }

  async connect(timeoutMs = 10000) {
    await new Promise((resolve, reject) => {
      const ws = new WebSocket(this.wsUrl);
      this.ws = ws;
      const timer = setTimeout(() => {
        try {
          ws.close();
        } catch {
          /* ignore */
        }
        reject(new Error(`连接 CDP WebSocket 超时：${this.wsUrl}`));
      }, timeoutMs);
      ws.addEventListener(
        'open',
        () => {
          clearTimeout(timer);
          resolve();
        },
        { once: true },
      );
      ws.addEventListener(
        'error',
        (event) => {
          clearTimeout(timer);
          reject(new Error(`CDP WebSocket 错误：${event?.message || this.wsUrl}`));
        },
        { once: true },
      );
      ws.addEventListener('message', (event) => this.#onMessage(event));
    });
  }

  #onMessage(event) {
    let msg;
    try {
      msg = JSON.parse(typeof event.data === 'string' ? event.data : String(event.data));
    } catch {
      return;
    }
    if (msg.id !== undefined) {
      const pending = this.pending.get(msg.id);
      if (!pending) return;
      this.pending.delete(msg.id);
      if (msg.error) {
        pending.reject(new Error(`${pending.method} 失败：${msg.error.message || JSON.stringify(msg.error)}`));
      } else {
        pending.resolve(msg.result || {});
      }
      return;
    }
    if (!msg.method) return;
    const handlers = this.handlers.get(msg.method);
    if (!handlers) return;
    for (const handler of handlers) {
      try {
        handler(msg.params || {});
      } catch (error) {
        // 事件处理不应让整个脚本崩溃。
        console.warn(`[shoot] CDP 事件处理失败 ${msg.method}: ${error.message}`);
      }
    }
  }

  on(method, handler) {
    if (!this.handlers.has(method)) this.handlers.set(method, new Set());
    this.handlers.get(method).add(handler);
    return () => this.handlers.get(method)?.delete(handler);
  }

  send(method, params = {}, timeoutMs = 30000) {
    if (!this.ws || this.closed) {
      return Promise.reject(new Error(`CDP 已关闭，无法发送 ${method}`));
    }
    const id = ++this.seq;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`CDP 超时（${timeoutMs}ms）：${method}`));
      }, timeoutMs);
      this.pending.set(id, {
        method,
        resolve: (value) => {
          clearTimeout(timer);
          resolve(value);
        },
        reject: (error) => {
          clearTimeout(timer);
          reject(error);
        },
      });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  close() {
    if (this.closed) return;
    this.closed = true;
    for (const pending of this.pending.values()) {
      pending.reject(new Error(`CDP 已关闭：${pending.method}`));
    }
    this.pending.clear();
    try {
      this.ws?.close();
    } catch {
      /* ignore */
    }
  }
}

/**
 * 启动一个 headless Chrome 实例。
 * @returns {Promise<Browser>}
 */
export async function launchBrowser(options = {}) {
  installSignalCleanup();
  const chromePath = findChrome(options.chromePath);
  const ownedUserDataDir = !options.userDataDir;
  const userDataDir = options.userDataDir || mkdtempSync(path.join(tmpdir(), 'agentsweep-cdp-'));
  const timeoutMs = options.timeoutMs ?? 20000;

  const args = [
    '--headless=new',
    '--remote-debugging-port=0',
    `--user-data-dir=${userDataDir}`,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-background-networking',
    '--disable-background-timer-throttling',
    '--disable-backgrounding-occluded-windows',
    '--disable-renderer-backgrounding',
    '--disable-breakpad',
    '--disable-component-update',
    '--disable-default-apps',
    '--disable-extensions',
    '--disable-sync',
    '--disable-translate',
    '--mute-audio',
    '--metrics-recording-only',
    '--safebrowsing-disable-auto-update',
    // 本地 file:// 原型可以直接加载同目录脚本/样式。
    '--allow-file-access-from-files',
    '--disable-features=Translate,AcceptCHFrame,MediaRouter,OptimizationHints,BackForwardCache',
    ...(options.args || []),
    'about:blank',
  ];

  const proc = spawn(chromePath, args, { stdio: ['ignore', 'ignore', 'pipe'] });
  const stderrChunks = [];
  proc.stderr.on('data', (chunk) => {
    stderrChunks.push(chunk);
    if (stderrChunks.length > 40) stderrChunks.shift();
  });
  const stderrTail = () => Buffer.concat(stderrChunks).toString('utf8').trim().slice(-4000);

  const portFile = path.join(userDataDir, 'DevToolsActivePort');
  const startedAt = Date.now();
  let port = null;
  while (Date.now() - startedAt < timeoutMs) {
    if (proc.exitCode !== null) {
      throw new Error(
        `Chrome 启动失败（退出码 ${proc.exitCode}）。\n${stderrTail() || '（无 stderr）'}\nChrome: ${chromePath}`,
      );
    }
    if (existsSync(portFile)) {
      const text = readFileSync(portFile, 'utf8').trim();
      const firstLine = text.split('\n')[0].trim();
      if (firstLine) {
        port = Number(firstLine);
        break;
      }
    }
    await sleep(50);
  }
  if (!port) {
    try {
      proc.kill('SIGKILL');
    } catch {
      /* ignore */
    }
    throw new Error(`等待 DevToolsActivePort 超时（${timeoutMs}ms）。\n${stderrTail() || '（无 stderr）'}`);
  }

  // 端口就绪后 CDP HTTP 端点可能还需要几十毫秒。
  const host = '127.0.0.1';
  let version = null;
  const httpStartedAt = Date.now();
  while (Date.now() - httpStartedAt < 5000) {
    try {
      const response = await fetch(`http://${host}:${port}/json/version`);
      if (response.ok) {
        version = await response.json();
        break;
      }
    } catch {
      /* retry */
    }
    await sleep(50);
  }
  if (!version?.webSocketDebuggerUrl) {
    try {
      proc.kill('SIGKILL');
    } catch {
      /* ignore */
    }
    throw new Error(`无法读取 Chrome /json/version。\n${stderrTail() || '（无 stderr）'}`);
  }

  const browser = new Browser({
    chromePath,
    proc,
    port,
    host,
    userDataDir,
    ownedUserDataDir,
    version,
    stderrTail,
  });
  ACTIVE_BROWSERS.add(browser);
  return browser;
}

export class Browser {
  constructor(config) {
    Object.assign(this, config);
    this.browserCdp = null;
    this.closed = false;
    this.userAgent = config.version?.['User-Agent'] || 'unknown';
  }

  get cdpVersion() {
    return this.version?.['Protocol-Version'] || 'unknown';
  }

  async #ensureBrowserCdp() {
    if (this.browserCdp) return this.browserCdp;
    const cdp = new CDP(this.version.webSocketDebuggerUrl);
    await cdp.connect();
    this.browserCdp = cdp;
    return cdp;
  }

  async newPage({ width = 1440, height = 900, deviceScaleFactor = 1 } = {}) {
    if (this.closed) throw new Error('Browser 已关闭');
    const response = await fetch(`http://${this.host}:${this.port}/json/new?about:blank`, {
      method: 'PUT',
    });
    if (!response.ok) {
      throw new Error(`创建标签页失败：HTTP ${response.status}`);
    }
    const target = await response.json();
    const page = new Page({
      browser: this,
      targetId: target.id,
      wsUrl: target.webSocketDebuggerUrl,
      width,
      height,
      deviceScaleFactor,
    });
    await page.attach();
    return page;
  }

  async close() {
    if (this.closed) return;
    this.closed = true;
    try {
      const cdp = await this.#ensureBrowserCdp();
      await cdp.send('Browser.close', {}, 1500).catch(() => {});
      cdp.close();
    } catch {
      /* ignore */
    }
    const exited = new Promise((resolve) => {
      if (this.proc.exitCode !== null) return resolve();
      this.proc.once('exit', resolve);
      setTimeout(resolve, 2500);
    });
    try {
      this.proc.kill('SIGTERM');
    } catch {
      /* ignore */
    }
    await exited;
    try {
      if (this.proc.exitCode === null) this.proc.kill('SIGKILL');
    } catch {
      /* ignore */
    }
    if (this.ownedUserDataDir) {
      try {
        rmSync(this.userDataDir, { recursive: true, force: true });
      } catch {
        /* ignore */
      }
    }
    ACTIVE_BROWSERS.delete(this);
  }
}

/* ------------------------------------------------------------------ */
/* 页面操作                                                            */
/* ------------------------------------------------------------------ */

const KEY_NAMES = {
  Escape: ['Escape', 'Escape', 27],
  Enter: ['Enter', 'Enter', 13],
  Tab: ['Tab', 'Tab', 9],
  Backspace: ['Backspace', 'Backspace', 8],
  Delete: ['Delete', 'Delete', 46],
  ArrowUp: ['ArrowUp', 'ArrowUp', 38],
  ArrowDown: ['ArrowDown', 'ArrowDown', 40],
  ArrowLeft: ['ArrowLeft', 'ArrowLeft', 37],
  ArrowRight: ['ArrowRight', 'ArrowRight', 39],
  Home: ['Home', 'Home', 36],
  End: ['End', 'End', 35],
  PageUp: ['PageUp', 'PageUp', 33],
  PageDown: ['PageDown', 'PageDown', 34],
  Space: [' ', 'Space', 32],
  ' ': [' ', 'Space', 32],
};

const PUNCT_KEYS = {
  ',': ['Comma', 188],
  '.': ['Period', 190],
  '/': ['Slash', 191],
  '\\': ['Backslash', 220],
  ';': ['Semicolon', 186],
  "'": ['Quote', 222],
  '[': ['BracketLeft', 219],
  ']': ['BracketRight', 221],
  '-': ['Minus', 189],
  '=': ['Equal', 187],
  '`': ['Backquote', 192],
};

const SHIFTED_CHARS = '~!@#$%^&*()_+{}|:"<>?';

function describeKey(name, options = {}) {
  if (KEY_NAMES[name]) {
    const [key, code, keyCode] = KEY_NAMES[name];
    return {
      key,
      code,
      keyCode,
      text: name === 'Space' || name === ' ' ? ' ' : null,
      shift: Boolean(options.shift),
    };
  }
  if (name.length !== 1) {
    throw new Error(`不支持的按键：${name}（支持单字符、Escape、Enter、Tab、Space、方向键等）`);
  }

  let key = name;
  let code = '';
  let keyCode = 0;
  let shift = Boolean(options.shift);

  if (/[a-zA-Z]/.test(name)) {
    const upper = name.toUpperCase();
    if (name === upper) shift = true;
    key = shift ? upper : name.toLowerCase();
    code = `Key${upper}`;
    keyCode = upper.charCodeAt(0);
  } else if (/[0-9]/.test(name)) {
    key = name;
    code = `Digit${name}`;
    keyCode = name.charCodeAt(0);
  } else if (PUNCT_KEYS[name]) {
    [code, keyCode] = PUNCT_KEYS[name];
  } else {
    keyCode = name.charCodeAt(0) || 0;
  }

  if (SHIFTED_CHARS.includes(name)) {
    shift = true;
    key = name;
  }
  if (name === '?') {
    shift = true;
    key = '?';
    code = 'Slash';
    keyCode = 191;
  }

  return { key, code, keyCode, text: name, shift };
}

export class Page {
  constructor({ browser, targetId, wsUrl, width, height, deviceScaleFactor }) {
    this.browser = browser;
    this.targetId = targetId;
    this.wsUrl = wsUrl;
    this.width = width;
    this.height = height;
    this.deviceScaleFactor = deviceScaleFactor;
    this.cdp = new CDP(wsUrl);

    this.consoleAll = [];
    this.consoleErrors = [];
    this.consoleWarnings = [];
    this.logEntries = [];
    this.requests = [];
    this.externalRequests = [];
    this.closed = false;
  }

  async attach() {
    await this.cdp.connect();
    const { cdp } = this;

    cdp.on('Runtime.consoleAPICalled', (params) => {
      const text = (params.args || [])
        .map((arg) => {
          if (arg.value !== undefined) return typeof arg.value === 'string' ? arg.value : String(arg.value);
          if (arg.description) return arg.description;
          if (arg.type === 'undefined') return 'undefined';
          return `<${arg.type}>`;
        })
        .join(' ');
      const frame = params.stackTrace?.callFrames?.[0];
      const entry = {
        source: 'console',
        type: params.type,
        text,
        url: frame?.url || null,
        line: frame?.lineNumber ?? null,
        column: frame?.columnNumber ?? null,
      };
      this.consoleAll.push(entry);
      if (params.type === 'error') this.consoleErrors.push(entry);
      if (params.type === 'warning' || params.type === 'warn') this.consoleWarnings.push(entry);
    });

    cdp.on('Runtime.exceptionThrown', (params) => {
      const details = params.exceptionDetails || {};
      const description = details.exception?.description || details.text || '未知异常';
      const entry = {
        source: 'exception',
        type: 'error',
        text: description,
        url: details.url || details.stackTrace?.callFrames?.[0]?.url || null,
        line: details.lineNumber ?? null,
        column: details.columnNumber ?? null,
      };
      this.consoleAll.push(entry);
      this.consoleErrors.push(entry);
    });

    cdp.on('Log.entryAdded', (params) => {
      const entry = params.entry || {};
      const record = {
        source: 'log',
        type: entry.level,
        text: entry.text || '',
        url: entry.url || null,
        line: entry.lineNumber ?? null,
        column: null,
      };
      if (entry.level === 'error') {
        this.logEntries.push(record);
        this.consoleErrors.push(record);
      } else if (entry.level === 'warning') {
        this.logEntries.push(record);
        this.consoleWarnings.push(record);
      }
    });

    cdp.on('Network.requestWillBeSent', (params) => {
      const url = params.request?.url || '';
      const request = {
        url,
        method: params.request?.method || 'GET',
        resourceType: params.type || 'Other',
      };
      this.requests.push(request);
      if (!/^(file|data|blob|about|chrome|chrome-extension|devtools):/i.test(url)) {
        this.externalRequests.push(request);
      }
    });

    await Promise.all([
      cdp.send('Page.enable'),
      cdp.send('Runtime.enable'),
      cdp.send('Log.enable'),
      cdp.send('Network.enable'),
    ]);
    await this.setViewport(this.width, this.height, this.deviceScaleFactor);
  }

  /** 去重后的错误（console.error / 未捕获异常 / Log error，可能互相重复）。 */
  uniqueErrors() {
    const seen = new Set();
    return this.consoleErrors.filter((entry) => {
      const key = [entry.type, entry.text, entry.url, entry.line].join('|');
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }

  clearConsole() {
    this.consoleAll = [];
    this.consoleErrors = [];
    this.consoleWarnings = [];
    this.logEntries = [];
    this.requests = [];
    this.externalRequests = [];
  }

  async setViewport(width, height, deviceScaleFactor = 1) {
    this.width = width;
    this.height = height;
    this.deviceScaleFactor = deviceScaleFactor;
    await this.cdp.send('Emulation.setDeviceMetricsOverride', {
      width,
      height,
      deviceScaleFactor,
      mobile: false,
      screenWidth: width,
      screenHeight: height,
    });
  }

  async #setMedia({ colorScheme = null, reducedMotion = true } = {}) {
    const features = [];
    if (reducedMotion) features.push({ name: 'prefers-reduced-motion', value: 'reduce' });
    if (colorScheme) features.push({ name: 'prefers-color-scheme', value: colorScheme });
    await this.cdp.send('Emulation.setEmulatedMedia', {
      media: '',
      features,
    });
  }

  /** 打开 URL（`page` 的完整别名）。等 load、字体、两帧 rAF，再等 waitMs。 */
  async open(url, options = {}) {
    const {
      width = this.width,
      height = this.height,
      deviceScaleFactor = this.deviceScaleFactor,
      waitMs = 300,
      waitFor = null,
      timeoutMs = 15000,
      colorScheme = null,
      reducedMotion = true,
    } = options;

    if (width !== this.width || height !== this.height || deviceScaleFactor !== this.deviceScaleFactor) {
      await this.setViewport(width, height, deviceScaleFactor);
    }
    await this.#setMedia({ colorScheme, reducedMotion });

    await this.cdp.send('Page.navigate', { url }, timeoutMs);
    await this.waitForFunction('document.readyState === "complete"', {
      timeout: timeoutMs,
      description: 'document.readyState === complete',
    });
    await this.evaljs(
      'document.fonts && document.fonts.ready ? document.fonts.ready.then(() => true) : true',
      { awaitPromise: true, timeoutMs: 5000 },
    ).catch(() => {});
    await this.evaljs(
      'new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true))))',
      { awaitPromise: true, timeoutMs: 5000 },
    ).catch(() => {});
    if (waitFor) {
      await this.waitFor(waitFor, { timeout: timeoutMs });
    }
    if (waitMs > 0) await sleep(waitMs);
    return this;
  }

  /** 兼容任务描述中的 `page(url, {width, height, waitMs})` 命名。 */
  async page(url, options = {}) {
    return this.open(url, options);
  }

  /** Runtime.evaluate，返回值已 JSON 化；页面异常会抛错。 */
  async evaljs(expression, options = {}) {
    const {
      awaitPromise = true,
      returnByValue = true,
      userGesture = true,
      timeoutMs = 30000,
    } = options;
    const result = await this.cdp.send(
      'Runtime.evaluate',
      { expression, returnByValue, awaitPromise, userGesture, includeCommandLineAPI: false },
      timeoutMs,
    );
    if (result.exceptionDetails) {
      const details = result.exceptionDetails;
      const description = details.exception?.description || details.text || '未知异常';
      const error = new Error(`页面执行异常：${description}`);
      error.exceptionDetails = details;
      throw error;
    }
    return result.result?.value;
  }

  async waitForFunction(expression, options = {}) {
    const { timeout = 5000, interval = 50, description = expression } = options;
    const startedAt = Date.now();
    let lastError = null;
    while (Date.now() - startedAt <= timeout) {
      try {
        const value = await this.evaljs(expression, { timeoutMs: Math.min(timeout + 1000, 10000) });
        if (value) return value;
      } catch (error) {
        lastError = error;
      }
      await sleep(interval);
    }
    throw new Error(
      `waitFor 超时（${timeout}ms）：${description}${lastError ? `\n最后一次错误：${lastError.message}` : ''}`,
    );
  }

  /** 等待元素出现（默认可见）。 */
  async waitFor(selector, options = {}) {
    const { timeout = 5000, visible = true, interval = 50 } = options;
    const expression = `(() => {
      const elements = document.querySelectorAll(${JSON.stringify(selector)});
      const element = elements[0];
      if (!element) return false;
      ${
        visible
          ? `if (element.hidden) return false;
             const style = getComputedStyle(element);
             if (style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0) return false;
             const rect = element.getBoundingClientRect();
             if (rect.width <= 0 || rect.height <= 0) return false;`
          : ''
      }
      return true;
    })()`;
    return this.waitForFunction(expression, {
      timeout,
      interval,
      description: `元素 ${selector}${visible ? ' 可见' : ' 存在'}`,
    });
  }

  async waitForHidden(selector, options = {}) {
    const { timeout = 5000, interval = 50 } = options;
    return this.waitForFunction(
      `(() => {
        const element = document.querySelector(${JSON.stringify(selector)});
        if (!element) return true;
        if (element.hidden) return true;
        const rect = element.getBoundingClientRect();
        if (rect.width <= 0 || rect.height <= 0) return true;
        const style = getComputedStyle(element);
        return style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity) === 0;
      })()`,
      { timeout, interval, description: `元素 ${selector} 隐藏` },
    );
  }

  /** 用真实鼠标事件点击元素（CSS 选择器，可指定索引 / 修饰键）。 */
  async click(selector, options = {}) {
    const { index = 0, button = 'left', clickCount = 1, meta = false, shift = false, ctrl = false, alt = false } = options;
    const scroll = options.scroll !== false;
    const info = await this.evaljs(`(() => {
      const elements = Array.from(document.querySelectorAll(${JSON.stringify(selector)}));
      const element = elements[${index}];
      if (!element) return { found: false, count: elements.length };
      ${scroll ? 'element.scrollIntoView({ block: "center", inline: "center" });' : ''}
      const rect = element.getBoundingClientRect();
      return {
        found: true,
        count: elements.length,
        x: rect.left + rect.width / 2,
        y: rect.top + rect.height / 2,
        width: rect.width,
        height: rect.height,
        tag: element.tagName,
        text: (element.textContent || '').trim().slice(0, 80),
        disabled: Boolean(element.disabled),
      };
    })()`);

    if (!info.found) {
      throw new Error(`click 失败：找不到元素 ${selector}（匹配 ${info.count} 个）`);
    }
    if (info.width <= 0 || info.height <= 0) {
      throw new Error(`click 失败：元素 ${selector} 尺寸为 ${info.width}×${info.height}`);
    }

    const modifiers = (alt ? 1 : 0) | (ctrl ? 2 : 0) | (meta ? 4 : 0) | (shift ? 8 : 0);
    const x = info.x;
    const y = info.y;
    await this.cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, button: 'none' });
    await this.cdp.send('Input.dispatchMouseEvent', {
      type: 'mousePressed',
      x,
      y,
      button,
      clickCount,
      modifiers,
    });
    await this.cdp.send('Input.dispatchMouseEvent', {
      type: 'mouseReleased',
      x,
      y,
      button,
      clickCount,
      modifiers,
    });
    await sleep(60);
    return info;
  }

  async hover(selector, options = {}) {
    const { index = 0 } = options;
    const info = await this.evaljs(`(() => {
      const element = document.querySelectorAll(${JSON.stringify(selector)})[${index}];
      if (!element) return { found: false };
      element.scrollIntoView({ block: 'center', inline: 'center' });
      const rect = element.getBoundingClientRect();
      return { found: true, x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
    })()`);
    if (!info.found) throw new Error(`hover 失败：找不到元素 ${selector}`);
    await this.cdp.send('Input.dispatchMouseEvent', {
      type: 'mouseMoved',
      x: info.x,
      y: info.y,
      button: 'none',
    });
    await sleep(60);
    return info;
  }

  /** 发送真实按键；组合键用 {meta, shift, ctrl, alt}。 */
  async key(name, options = {}) {
    const descriptor = describeKey(name, options);
    const {
      meta = false,
      ctrl = false,
      alt = false,
      delay = 20,
    } = options;
    const shift = Boolean(options.shift) || descriptor.shift;
    const modifiers = (alt ? 1 : 0) | (ctrl ? 2 : 0) | (meta ? 4 : 0) | (shift ? 8 : 0);
    const base = {
      modifiers,
      key: descriptor.key,
      code: descriptor.code || undefined,
      windowsVirtualKeyCode: descriptor.keyCode || undefined,
      nativeVirtualKeyCode: descriptor.keyCode || undefined,
    };
    const text = !meta && !ctrl && !alt && descriptor.text ? descriptor.text : null;
    await this.cdp.send('Input.dispatchKeyEvent', {
      type: text ? 'keyDown' : 'rawKeyDown',
      ...base,
      ...(text ? { text, unmodifiedText: text } : {}),
    });
    await this.cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', ...base });
    await sleep(delay);
    return { key: descriptor.key, code: descriptor.code, modifiers };
  }

  /** 直接设置输入框值并派发 input/change 事件。 */
  async fill(selector, value) {
    return this.evaljs(`(() => {
      const element = document.querySelector(${JSON.stringify(selector)});
      if (!element) return false;
      element.focus();
      element.value = ${JSON.stringify(value)};
      element.dispatchEvent(new Event('input', { bubbles: true }));
      element.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    })()`);
  }

  async type(text) {
    await this.cdp.send('Input.insertText', { text });
    await sleep(30);
  }

  /**
   * 截图并（可选）写入文件。
   * @returns {{buffer: Buffer, path: string|null, stats: object}}
   */
  async screenshot(options = {}) {
    const { file = null, format = 'png', fullPage = false, clip = null } = options;
    const params = {
      format,
      fromSurface: true,
      captureBeyondViewport: fullPage,
    };
    if (clip) {
      params.clip = { scale: 1, ...clip };
      params.captureBeyondViewport = true;
    } else if (fullPage) {
      const metrics = await this.cdp.send('Page.getLayoutMetrics');
      const size = metrics.cssContentSize || metrics.contentSize;
      if (size) {
        params.clip = { x: 0, y: 0, width: Math.ceil(size.width), height: Math.ceil(size.height), scale: 1 };
      }
    }
    const result = await this.cdp.send('Page.captureScreenshot', params, 30000);
    const buffer = Buffer.from(result.data, 'base64');
    let filePath = null;
    if (file) {
      filePath = path.resolve(file);
      mkdirSync(path.dirname(filePath), { recursive: true });
      writeFileSync(filePath, buffer);
    }
    return {
      buffer,
      path: filePath,
      stats: format === 'png' ? analyzePng(buffer) : { format, bytes: buffer.length, supported: false },
    };
  }

  /** 原型页面状态快照，用于截图记录与回归报告。 */
  async inspect() {
    return this.evaljs(`(() => {
      const $ = (selector) => document.querySelector(selector);
      const overlayIds = [
        'settingsSheet', 'discoverSheet', 'reviewDialog', 'shortcutsDialog',
        'palette', 'filterPopover', 'quickPopover', 'contextMenu',
      ];
      const overlays = {};
      for (const id of overlayIds) {
        const element = document.getElementById(id);
        overlays[id] = element ? !element.hidden : null;
      }
      const doc = document.documentElement;
      const win = $('#window');
      const table = $('#sessionTable');
      const dock = $('#protoDock');
      const summary = $('#selectionSummary');
      const scanStatus = $('#scanStatus');
      return {
        title: document.title || null,
        url: location.href,
        theme: doc.dataset.theme || 'auto',
        density: $('#prototype')?.dataset.density || null,
        detail: $('#app')?.dataset.detail || null,
        appPresent: Boolean($('#app')),
        rows: document.querySelectorAll('#sessionBody tr.session-row').length,
        agentRows: document.querySelectorAll('#agentList [data-agent]').length,
        visibleAgents: document.querySelectorAll('#agentList [data-agent].is-active').length,
        selected: document.querySelectorAll('#sessionBody tr.session-row[aria-selected="true"]').length,
        focused: document.querySelectorAll('#sessionBody tr.session-row.is-focused').length,
        openOverlays: Object.keys(overlays).filter((id) => overlays[id]),
        overlays,
        toastCount: document.querySelectorAll('#toasts .toast').length,
        selectionSummary: summary ? summary.textContent.replace(/\\s+/g, ' ').trim() : null,
        scanStatus: scanStatus ? scanStatus.textContent.replace(/\\s+/g, ' ').trim() : null,
        dockHidden: dock ? dock.hidden : null,
        horizontalScroll: {
          document: doc.scrollWidth - doc.clientWidth,
          window: win ? win.scrollWidth - win.clientWidth : null,
          table: table ? table.scrollWidth - table.clientWidth : null,
        },
        viewport: { width: innerWidth, height: innerHeight, dpr: devicePixelRatio },
        activeElement: document.activeElement?.id || document.activeElement?.tagName || null,
      };
    })()`);
  }

  async close() {
    if (this.closed) return;
    this.closed = true;
    try {
      this.cdp.close();
    } catch {
      /* ignore */
    }
    try {
      await fetch(`http://${this.browser.host}:${this.browser.port}/json/close/${this.targetId}`);
    } catch {
      /* ignore */
    }
  }
}

/* ------------------------------------------------------------------ */
/* PNG 分析（无依赖解码，用于「截图是否空白」自检）                     */
/* ------------------------------------------------------------------ */

const PNG_SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

function paethPredictor(a, b, c) {
  const p = a + b - c;
  const pa = Math.abs(p - a);
  const pb = Math.abs(p - b);
  const pc = Math.abs(p - c);
  if (pa <= pb && pa <= pc) return a;
  if (pb <= pc) return b;
  return c;
}

/**
 * 解码 Chrome 的 8-bit PNG 截图并给出「是否空白」的统计。
 * 支持 colorType 2 (RGB) / 6 (RGBA) / 0 (灰度)，bitDepth 8，非隔行。
 */
export function analyzePng(buffer) {
  const bytes = buffer.length;
  if (bytes < 16 || !buffer.subarray(0, 8).equals(PNG_SIGNATURE)) {
    return { supported: false, bytes, width: null, height: null, blank: null, reason: '不是 PNG' };
  }

  let offset = 8;
  let header = null;
  const idatChunks = [];
  while (offset + 8 <= buffer.length) {
    const length = buffer.readUInt32BE(offset);
    const type = buffer.toString('ascii', offset + 4, offset + 8);
    const dataStart = offset + 8;
    const dataEnd = dataStart + length;
    if (dataEnd + 4 > buffer.length) break;
    if (type === 'IHDR') {
      header = {
        width: buffer.readUInt32BE(dataStart),
        height: buffer.readUInt32BE(dataStart + 4),
        bitDepth: buffer[dataStart + 8],
        colorType: buffer[dataStart + 9],
        interlace: buffer[dataStart + 12],
      };
    } else if (type === 'IDAT') {
      idatChunks.push(buffer.subarray(dataStart, dataEnd));
    } else if (type === 'IEND') {
      break;
    }
    offset = dataEnd + 4;
  }

  if (!header) {
    return { supported: false, bytes, width: null, height: null, blank: null, reason: '缺少 IHDR' };
  }
  const { width, height, bitDepth, colorType, interlace } = header;
  const bppByColorType = { 0: 1, 2: 3, 4: 2, 6: 4 };
  const bpp = bppByColorType[colorType];
  if (bitDepth !== 8 || !bpp || interlace !== 0) {
    return {
      supported: false,
      bytes,
      width,
      height,
      colorType,
      bitDepth,
      interlace,
      blank: null,
      reason: '不支持的 PNG 格式',
    };
  }

  let raw;
  try {
    raw = inflateSync(Buffer.concat(idatChunks));
  } catch (error) {
    return { supported: false, bytes, width, height, blank: null, reason: `IDAT 解压失败: ${error.message}` };
  }

  const stride = width * bpp;
  const pixels = Buffer.alloc(height * stride);
  let position = 0;
  for (let y = 0; y < height; y += 1) {
    if (position >= raw.length) break;
    const filter = raw[position];
    position += 1;
    const rowStart = y * stride;
    const previousStart = rowStart - stride;
    for (let x = 0; x < stride; x += 1) {
      const rawValue = raw[position + x] ?? 0;
      const left = x >= bpp ? pixels[rowStart + x - bpp] : 0;
      const up = y > 0 ? pixels[previousStart + x] : 0;
      const upLeft = y > 0 && x >= bpp ? pixels[previousStart + x - bpp] : 0;
      let value;
      switch (filter) {
        case 0:
          value = rawValue;
          break;
        case 1:
          value = (rawValue + left) & 0xff;
          break;
        case 2:
          value = (rawValue + up) & 0xff;
          break;
        case 3:
          value = (rawValue + ((left + up) >> 1)) & 0xff;
          break;
        case 4:
          value = (rawValue + paethPredictor(left, up, upLeft)) & 0xff;
          break;
        default:
          return { supported: false, bytes, width, height, blank: null, reason: `未知 PNG filter ${filter}` };
      }
      pixels[rowStart + x] = value;
    }
    position += stride;
  }

  const totalPixels = width * height;
  const step = Math.max(1, Math.floor(totalPixels / 20000));
  const colors = new Set();
  let samples = 0;
  let transparent = 0;
  let sum = 0;
  let sumSq = 0;
  let center = null;

  for (let index = 0; index < totalPixels; index += step) {
    const pixelOffset = index * bpp;
    let r;
    let g;
    let b;
    let a = 255;
    if (colorType === 0) {
      r = g = b = pixels[pixelOffset];
    } else if (colorType === 4) {
      r = g = b = pixels[pixelOffset];
      a = pixels[pixelOffset + 1];
    } else if (colorType === 2) {
      r = pixels[pixelOffset];
      g = pixels[pixelOffset + 1];
      b = pixels[pixelOffset + 2];
    } else {
      r = pixels[pixelOffset];
      g = pixels[pixelOffset + 1];
      b = pixels[pixelOffset + 2];
      a = pixels[pixelOffset + 3];
    }
    if (a === 0) transparent += 1;
    const luminance = 0.2126 * r + 0.7152 * g + 0.0722 * b;
    sum += luminance;
    sumSq += luminance * luminance;
    samples += 1;
    if (colors.size < 12000) colors.add((r << 16) | (g << 8) | b);
  }

  const centerIndex = (Math.floor(height / 2) * width + Math.floor(width / 2)) * bpp;
  if (colorType === 0) {
    center = [pixels[centerIndex], pixels[centerIndex], pixels[centerIndex], 255];
  } else if (colorType === 4) {
    center = [pixels[centerIndex], pixels[centerIndex], pixels[centerIndex], pixels[centerIndex + 1]];
  } else {
    center = [
      pixels[centerIndex],
      pixels[centerIndex + 1],
      pixels[centerIndex + 2],
      colorType === 6 ? pixels[centerIndex + 3] : 255,
    ];
  }

  const mean = sum / samples;
  const stdDev = Math.sqrt(Math.max(0, sumSq / samples - mean * mean));
  const transparentRatio = transparent / samples;
  const blank = transparentRatio > 0.99 || (colors.size <= 2 && stdDev < 2);

  return {
    supported: true,
    bytes,
    width,
    height,
    distinctColors: colors.size,
    stdDev: Number(stdDev.toFixed(2)),
    meanLuminance: Number(mean.toFixed(2)),
    transparentRatio: Number(transparentRatio.toFixed(3)),
    center,
    blank,
  };
}

/* ------------------------------------------------------------------ */
/* 批量截图矩阵                                                        */
/* ------------------------------------------------------------------ */

/**
 * 场景定义：query 会与 theme / dock=0 合并；waitMs 为该场景额外等待时间。
 * 顺序即文件名序号（每场景浅色、深色两张）。
 */
export const SCENES = [
  { id: 'main', label: '主界面', query: { state: 'idle' }, waitMs: 450 },
  { id: 'scanning', label: '扫描中', query: { state: 'scanning' }, waitMs: 260 },
  { id: 'empty', label: '空列表', query: { state: 'empty' }, waitMs: 350 },
  { id: 'noresults', label: '无结果', query: { state: 'noresults' }, waitMs: 350 },
  { id: 'error', label: '部分失败', query: { state: 'error' }, waitMs: 700 },
  { id: 'settings', label: '设置', query: { panel: 'settings' }, waitMs: 500 },
  { id: 'discover', label: '发现', query: { panel: 'discover' }, waitMs: 1100 },
  { id: 'review', label: '清理确认', query: { panel: 'review' }, waitMs: 500 },
  { id: 'palette', label: '命令面板', query: { panel: 'palette' }, waitMs: 500 },
  { id: 'shortcuts', label: '快捷键', query: { panel: 'shortcuts' }, waitMs: 500 },
  { id: 'context', label: '右键菜单', query: { panel: 'context' }, waitMs: 500 },
  { id: 'compact', label: '紧凑密度', query: { density: 'compact' }, waitMs: 450 },
  { id: 'narrow', label: '窄窗口', query: { state: 'idle' }, width: 960, height: 700, waitMs: 450 },
];

export function buildMatrix(themes = ['light', 'dark'], only = null) {
  const allThemes = ['light', 'dark'];
  const themeSet = new Set(themes);
  const sceneSet = only ? new Set(only) : null;
  if (only && !SCENES.some((scene) => only.includes(scene.id))) {
    throw new Error(`--only 没有匹配的场景；可选：${SCENES.map((scene) => scene.id).join(', ')}`);
  }
  const shots = [];
  let number = 0;
  // 编号始终按完整矩阵计算，--only / --themes 只决定是否输出；
  // 这样补拍单张时文件名与全量运行一致（settings-light 永远是 11）。
  for (const scene of SCENES) {
    for (const theme of allThemes) {
      number += 1;
      if (sceneSet && !sceneSet.has(scene.id)) continue;
      if (!themeSet.has(theme)) continue;
      shots.push({
        scene,
        theme,
        number,
        filename: `${String(number).padStart(2, '0')}-${scene.id}-${theme}.png`,
      });
    }
  }
  return shots;
}

function resolveUrl(input) {
  if (/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(input)) return input;
  return pathToFileURL(path.resolve(input)).href;
}

function buildQuery(params) {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') search.set(key, String(value));
  }
  return search.toString();
}

/**
 * 按场景 × 主题批量截图。
 * @returns {Promise<{results: object[], ok: boolean, outDir: string}>}
 */
export async function shootMatrix(options = {}) {
  const {
    base = PROTOTYPE_PATH,
    outDir = DEFAULT_OUT_DIR,
    width = 1440,
    height = 900,
    only = null,
    themes = ['light', 'dark'],
    waitMs = null,
    report = null,
    chromePath = null,
    reducedMotion = true,
    quiet = false,
  } = options;

  const baseUrl = resolveUrl(base);
  if (baseUrl.startsWith('file:')) {
    const filePath = fileURLToPath(baseUrl);
    if (!existsSync(filePath)) {
      throw new Error(`原型页面不存在：${filePath}\n（index.html 可能仍在并行开发；--base 可指向其他页面）`);
    }
  }

  const shots = buildMatrix(themes, only);
  mkdirSync(outDir, { recursive: true });
  const browser = await launchBrowser({ chromePath });
  const results = [];

  if (!quiet) {
    console.log(`Chrome: ${browser.chromePath}`);
    console.log(`协议: ${browser.cdpVersion}  视口: ${width}×${height}  输出: ${path.resolve(outDir)}`);
    const sceneCount = new Set(shots.map((shot) => shot.scene.id)).size;
    console.log(`共 ${shots.length} 张（${themes.join('/')} × ${sceneCount} 个场景）\n`);
  }

  try {
    for (const [index, shot] of shots.entries()) {
      const { scene, theme } = shot;
      const viewport = {
        width: scene.width || width,
        height: scene.height || height,
      };
      const query = buildQuery({ theme, dock: '0', ...scene.query });
      const url = `${baseUrl}${baseUrl.includes('?') ? '&' : '?'}${query}`;
      const file = path.join(outDir, shot.filename);
      const startedAt = Date.now();
      const page = await browser.newPage({ ...viewport });
      const record = {
        file: path.resolve(file),
        filename: shot.filename,
        scene: scene.id,
        label: scene.label,
        theme,
        query,
        url,
        viewport,
        ok: false,
        ms: 0,
        consoleErrors: [],
        consoleWarnings: [],
        externalRequests: [],
        stats: null,
        inspect: null,
        error: null,
        warnings: [],
      };
      try {
        await page.open(url, {
          ...viewport,
          waitMs: waitMs ?? scene.waitMs ?? 350,
          reducedMotion,
        });
        record.inspect = await page.inspect();
        const capture = await page.screenshot({ file });
        record.stats = capture.stats;
        record.consoleErrors = page.uniqueErrors().map((entry) => ({
          type: entry.type,
          text: entry.text,
          url: entry.url,
          line: entry.line,
        }));
        record.consoleWarnings = page.consoleWarnings.map((entry) => ({ text: entry.text, url: entry.url }));
        record.externalRequests = page.externalRequests.map((request) => request.url);
        record.ok = true;
      } catch (error) {
        record.error = error.message;
      } finally {
        record.ms = Date.now() - startedAt;
        await page.close().catch(() => {});
      }

      if (record.ok) {
        if (!record.stats?.supported) {
          record.warnings.push(record.stats?.reason || '无法分析截图格式');
        } else if (record.stats.blank) {
          record.ok = false;
          record.error = `截图为空白（colors=${record.stats.distinctColors} stdDev=${record.stats.stdDev}）`;
        }
        if (record.consoleErrors.length > 0) {
          record.warnings.push(`${record.consoleErrors.length} 个 console/页面错误`);
        }
        if (record.externalRequests.length > 0) {
          record.warnings.push(`${record.externalRequests.length} 个外部网络请求`);
        }
        if (record.inspect?.theme !== theme) {
          record.warnings.push(`主题参数=${theme}，实际 data-theme=${record.inspect?.theme}`);
        }
      }

      results.push(record);
      if (!quiet) {
        const status = record.ok ? 'OK ' : 'ERR';
        const stats = record.stats?.supported
          ? `colors=${String(record.stats.distinctColors).padStart(5)} stdDev=${String(record.stats.stdDev).padStart(6)}`
          : 'stats=?';
        const inspect = record.inspect
          ? `rows=${record.inspect.rows} agents=${record.inspect.agentRows} overlays=[${record.inspect.openOverlays.join(',')}]`
          : 'inspect=?';
        const errorText = record.error ? `  ${record.error}` : '';
        console.log(
          `[${String(index + 1).padStart(2)}/${shots.length}] ${status} ${shot.filename}  ${stats}  ${inspect}  ${record.ms}ms${errorText}`,
        );
        for (const warning of record.warnings) console.log(`      ⚠ ${warning}`);
        for (const entry of record.consoleErrors.slice(0, 5)) {
          console.log(`      ✗ ${entry.type}: ${String(entry.text).split('\n')[0].slice(0, 160)}`);
        }
      }
    }
  } finally {
    await browser.close();
  }

  const failed = results.filter((result) => !result.ok);
  const withErrors = results.filter((result) => result.consoleErrors.length > 0);
  const summary = {
    generatedAt: new Date().toISOString(),
    chrome: browser.chromePath,
    chromeVersion: browser.userAgent,
    baseUrl,
    outDir: path.resolve(outDir),
    total: results.length,
    failed: failed.length,
    withConsoleErrors: withErrors.length,
    ok: failed.length === 0 && withErrors.length === 0,
  };

  if (!quiet) {
    console.log(`\n完成：${results.length - failed.length}/${results.length} 张成功。`);
    if (failed.length) {
      console.log('失败：');
      for (const record of failed) console.log(`  - ${record.filename}: ${record.error}`);
    }
    if (withErrors.length) {
      console.log('含 console 错误的截图：');
      for (const record of withErrors) {
        console.log(`  - ${record.filename}: ${record.consoleErrors[0]?.text}`);
      }
    }
    if (summary.ok) console.log('状态：PASS');
    else console.log('状态：FAIL');
  }

  if (report) {
    const reportPath = path.resolve(report);
    mkdirSync(path.dirname(reportPath), { recursive: true });
    writeFileSync(reportPath, JSON.stringify({ summary, results }, null, 2));
    if (!quiet) console.log(`报告：${reportPath}`);
  }

  return { results, summary };
}

/* ------------------------------------------------------------------ */
/* smoke 自检                                                          */
/* ------------------------------------------------------------------ */

const SMOKE_FIXTURE = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>AgentSweep Shooter Smoke</title>
<style>
  html, body { margin: 0; padding: 0; }
  body {
    min-height: 100vh; display: grid; place-items: center;
    background: #f4f2ec; color: #1b1f26;
    font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", sans-serif;
  }
  .card {
    width: 520px; padding: 24px; background: #fff;
    border: 1px solid #e2ded4; border-radius: 12px;
    box-shadow: 0 12px 40px -24px rgba(38, 34, 26, .35);
  }
  h1 { margin: 16px 0 8px; font-size: 21px; }
  #swatch { height: 120px; border-radius: 8px; background: #3f6599; }
  #out { font-size: 14px; margin: 8px 0; color: #545c68; }
  #inp { height: 32px; width: 200px; padding: 0 8px; border: 1px solid #cfc9bd; border-radius: 7px; }
  #btn { height: 32px; padding: 0 14px; border: 1px solid #1b1f26; border-radius: 7px; background: #1b1f26; color: #fff; }
  #late { margin-top: 12px; font-size: 12px; color: #2e6f5e; }
</style>
</head>
<body>
  <main class="card" id="card">
    <div id="swatch"></div>
    <h1>AgentSweep Shooter Smoke</h1>
    <p id="out">idle</p>
    <input id="inp" aria-label="smoke input" placeholder="type here">
    <button id="btn" type="button">click me</button>
    <div id="late" hidden>late node</div>
  </main>
<script>
  console.error('smoke-expected-error');
  console.warn('smoke-expected-warning');
  window.__smokeData = { ok: true, n: 42, nested: { value: '中文' } };
  let clicks = 0;
  document.getElementById('btn').addEventListener('click', () => {
    clicks += 1;
    document.getElementById('out').textContent = 'clicked:' + clicks;
  });
  document.addEventListener('keydown', (event) => {
    if (event.metaKey && event.key === 'a') {
      event.preventDefault();
      document.getElementById('out').textContent = 'key:meta+a';
    }
    if (event.key === 'Escape') {
      document.getElementById('out').textContent = 'key:Escape';
    }
  });
  setTimeout(() => {
    const late = document.getElementById('late');
    late.hidden = false;
    late.textContent = 'late node ready';
  }, 5000);
</script>
</body>
</html>
`;

/**
 * 用临时 HTML 自检工具链：截图非空白、console 收集、点击、按键、等待元素、JSON 返回值。
 * 输出到 /tmp（不污染正式 screenshots 目录）。
 */
export async function runSmoke(options = {}) {
  const outDir = path.resolve(options.outDir || path.join(tmpdir(), 'agentsweep-shoot-smoke'));
  mkdirSync(outDir, { recursive: true });
  const fixturePath = path.join(outDir, 'fixture.html');
  writeFileSync(fixturePath, SMOKE_FIXTURE, 'utf8');

  const browser = await launchBrowser({ chromePath: options.chromePath });
  const page = await browser.newPage({ width: 900, height: 600 });
  const checks = [];
  const addCheck = (name, ok, detail) => {
    checks.push({ name, ok: Boolean(ok), detail });
    const marker = ok ? 'OK  ' : 'FAIL';
    console.log(`  [${marker}] ${name}${detail === undefined ? '' : ` — ${typeof detail === 'string' ? detail : JSON.stringify(detail)}`}`);
  };

  let shotStats = null;
  let swatchStats = null;
  let resultPath = null;
  try {
    await page.open(pathToFileURL(fixturePath).href, { waitMs: 400, reducedMotion: true });
    console.log(`\nChrome: ${browser.chromePath}\nFixture: ${fixturePath}\n`);

    // 1. 基本 DOM / JSON 返回值
    const title = await page.evaljs('document.title');
    addCheck('evaljs 返回 JSON', JSON.stringify(await page.evaljs('window.__smokeData')) === JSON.stringify({ ok: true, n: 42, nested: { value: '中文' } }), title);

    // 2. console.error / warn 收集
    const expectedError = page.uniqueErrors().find((entry) => entry.text.includes('smoke-expected-error'));
    addCheck('收集 console.error', Boolean(expectedError), expectedError?.text);
    const expectedWarning = page.consoleWarnings.find((entry) => entry.text.includes('smoke-expected-warning'));
    addCheck('收集 console.warn', Boolean(expectedWarning), expectedWarning?.text);

    // 3. 点击
    await page.click('#btn');
    const clickedText = await page.evaljs("document.getElementById('out').textContent");
    addCheck('click() 触发真实点击', clickedText === 'clicked:1', clickedText);

    // 4. 键盘组合（meta+a）
    await page.fill('#inp', 'hello world');
    const inputValue = await page.evaljs("document.getElementById('inp').value");
    addCheck('fill() 写入输入框', inputValue === 'hello world', inputValue);
    await page.click('#card');
    await page.key('a', { meta: true });
    const keyText = await page.evaljs("document.getElementById('out').textContent");
    addCheck('key() meta+a 组合键', keyText === 'key:meta+a', keyText);
    await page.key('Escape');
    const escapeText = await page.evaljs("document.getElementById('out').textContent");
    addCheck('key() Escape', escapeText === 'key:Escape', escapeText);

    // 5. waitFor（元素在 5s 后才显示；先确认它此刻仍隐藏）
    const lateHiddenBefore = await page.evaljs("document.getElementById('late').hidden");
    addCheck('waitFor 前元素仍隐藏', lateHiddenBefore === true, String(lateHiddenBefore));
    const waitStartedAt = Date.now();
    await page.waitFor('#late', { timeout: 8000 });
    addCheck('waitFor() 等待延迟元素', true, `${Date.now() - waitStartedAt}ms`);

    // 6. 截图非空白（整页）
    const shot = await page.screenshot({ file: path.join(outDir, 'smoke-main.png') });
    shotStats = shot.stats;
    addCheck('整页截图非空白', shotStats.supported && !shotStats.blank, shotStats);
    addCheck('截图尺寸符合视口', shotStats.width === 900 && shotStats.height === 600, `${shotStats.width}×${shotStats.height} ${shotStats.bytes} bytes`);

    // 7. 裁剪截图采样品牌色（证明截到的是真实渲染内容，而非白底）
    const rect = await page.evaljs(`(() => {
      const rect = document.getElementById('swatch').getBoundingClientRect();
      return { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
    })()`);
    const swatchShot = await page.screenshot({
      file: path.join(outDir, 'smoke-swatch.png'),
      clip: { ...rect, scale: 1 },
    });
    swatchStats = swatchShot.stats;
    const [r, g, b] = swatchStats.center || [];
    const closeToBrand = Math.abs(r - 63) <= 12 && Math.abs(g - 101) <= 12 && Math.abs(b - 153) <= 12;
    addCheck('截图采到品牌色 #3F6599', closeToBrand, `center=rgb(${r},${g},${b})`);

    // 8. 无外部网络请求
    addCheck('无外部网络请求', page.externalRequests.length === 0, page.externalRequests.map((request) => request.url));

    // 9. 状态快照
    const inspect = await page.inspect();
    addCheck('inspect() 快照可读', inspect.title === 'AgentSweep Shooter Smoke' && inspect.viewport.width === 900, inspect.viewport);

    const ok = checks.every((check) => check.ok);
    const result = {
      ok,
      generatedAt: new Date().toISOString(),
      chrome: browser.chromePath,
      chromeVersion: browser.userAgent,
      protocolVersion: browser.cdpVersion,
      node: process.version,
      fixture: fixturePath,
      checks,
      screenshots: { main: path.join(outDir, 'smoke-main.png'), swatch: path.join(outDir, 'smoke-swatch.png') },
      screenshotStats: { main: shotStats, swatch: swatchStats },
      console: {
        errors: page.uniqueErrors(),
        warnings: page.consoleWarnings,
        all: page.consoleAll,
      },
      externalRequests: page.externalRequests,
      inspect,
    };
    resultPath = path.join(outDir, 'result.json');
    writeFileSync(resultPath, JSON.stringify(result, null, 2));

    console.log(`\n截图：${result.screenshots.main}`);
    console.log(`截图：${result.screenshots.swatch}`);
    console.log(`结果：${resultPath}`);
    console.log(`\nsmoke ${ok ? 'PASS' : 'FAIL'}（${checks.filter((check) => check.ok).length}/${checks.length}）`);
    return { ok, checks, outDir, resultPath, result };
  } finally {
    await page.close().catch(() => {});
    await browser.close();
  }
}

/* ------------------------------------------------------------------ */
/* CLI                                                                 */
/* ------------------------------------------------------------------ */

function parseArgs(argv) {
  const parsed = { _: [] };
  for (let index = 0; index < argv.length; index += 1) {
    const token = argv[index];
    if (!token.startsWith('--')) {
      parsed._.push(token);
      continue;
    }
    let key = token.slice(2);
    let value;
    const equalsIndex = key.indexOf('=');
    if (equalsIndex >= 0) {
      value = key.slice(equalsIndex + 1);
      key = key.slice(0, equalsIndex);
    } else if (index + 1 < argv.length && !argv[index + 1].startsWith('--')) {
      index += 1;
      value = argv[index];
    }
    if (key.startsWith('no-') && value === undefined) {
      parsed[key.slice(3)] = false;
    } else if (value === undefined) {
      parsed[key] = true;
    } else {
      parsed[key] = value;
    }
  }
  return parsed;
}

function numberOption(value, fallback) {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? Math.round(number) : fallback;
}

const HELP = `AgentSweep 原型验收工具（零依赖 Chrome CDP）

用法：
  node docs/design/tools/shoot.mjs smoke [--out /tmp/agentsweep-shoot-smoke]
  node docs/design/tools/shoot.mjs shot  --url <file:///…index.html?query> [--out /tmp/a.png]
        [--width 1440] [--height 900] [--wait 400] [--wait-for ".selector"]
        [--full-page] [--no-reduced-motion] [--chrome /path/to/chrome]
  node docs/design/tools/shoot.mjs eval  --url <file:///…> --expr "document.title"
        [--width 1440] [--height 900] [--wait 300]
  node docs/design/tools/shoot.mjs matrix [--base docs/design/prototype/index.html]
        [--out docs/design/screenshots] [--only main,settings] [--themes light,dark]
        [--width 1440] [--height 900] [--wait 400] [--report /tmp/matrix.json]
        [--no-reduced-motion] [--strict]

快速开始：
  node docs/design/tools/shoot.mjs smoke
  node docs/design/tools/shoot.mjs matrix --base docs/design/prototype/index.html \\
    --out docs/design/screenshots --report /tmp/agentsweep-shots.json --strict

说明：
  - 自动启动本机 Chrome headless（--remote-debugging-port=0），退出时清理。
  - 矩阵默认 13 个场景 × 浅色/深色 = 26 张，命名 01-main-light.png 等。
  - Node >= 22（使用全局 WebSocket / fetch），不需要 npm install。
`;

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const command = args._[0] || 'help';

  if (command === 'help' || command === '--help' || args.help) {
    console.log(HELP);
    return;
  }

  if (command === 'smoke') {
    const { ok } = await runSmoke({
      outDir: args.out,
      chromePath: args.chrome || args.chromePath,
    });
    process.exitCode = ok ? 0 : 1;
    return;
  }

  if (command === 'shot') {
    if (!args.url) throw new Error('shot 需要 --url <file:///… 或 http://…>');
    const browser = await launchBrowser({ chromePath: args.chrome || args.chromePath });
    const page = await browser.newPage({
      width: numberOption(args.width, 1440),
      height: numberOption(args.height, 900),
    });
    try {
      await page.open(resolveUrl(String(args.url)), {
        waitMs: args.wait === undefined ? 400 : Number(args.wait),
        waitFor: args['wait-for'] || args['waitFor'] || null,
        reducedMotion: args.reducedMotion !== false,
      });
      const capture = await page.screenshot({
        file: args.out ? String(args.out) : null,
        fullPage: Boolean(args['full-page'] || args.fullPage),
      });
      const result = {
        url: resolveUrl(String(args.url)),
        out: capture.path,
        screenshot: capture.stats,
        consoleErrors: page.uniqueErrors(),
        consoleWarnings: page.consoleWarnings,
        externalRequests: page.externalRequests,
        inspect: await page.inspect(),
      };
      console.log(JSON.stringify(result, null, 2));
      process.exitCode = capture.stats.supported && !capture.stats.blank ? 0 : 1;
    } finally {
      await page.close().catch(() => {});
      await browser.close();
    }
    return;
  }

  if (command === 'eval') {
    if (!args.url) throw new Error('eval 需要 --url <file:///… 或 http://…>');
    if (!args.expr) throw new Error('eval 需要 --expr "表达式"');
    const browser = await launchBrowser({ chromePath: args.chrome || args.chromePath });
    const page = await browser.newPage({
      width: numberOption(args.width, 1440),
      height: numberOption(args.height, 900),
    });
    try {
      await page.open(resolveUrl(String(args.url)), {
        waitMs: args.wait === undefined ? 300 : Number(args.wait),
        waitFor: args['wait-for'] || null,
        reducedMotion: args.reducedMotion !== false,
      });
      const value = await page.evaljs(String(args.expr));
      console.log(JSON.stringify(value, null, 2));
      const errors = page.uniqueErrors();
      if (errors.length) {
        console.error(`\n页面 console 错误 ${errors.length} 条：`);
        for (const entry of errors) console.error(`  - ${entry.text}`);
        process.exitCode = 1;
      }
    } finally {
      await page.close().catch(() => {});
      await browser.close();
    }
    return;
  }

  if (command === 'matrix') {
    const only = args.only
      ? String(args.only)
          .split(',')
          .map((item) => item.trim())
          .filter(Boolean)
      : null;
    const themes = args.themes
      ? String(args.themes)
          .split(',')
          .map((item) => item.trim())
          .filter(Boolean)
      : ['light', 'dark'];
    for (const theme of themes) {
      if (!['light', 'dark'].includes(theme)) throw new Error(`--themes 只支持 light,dark；收到：${theme}`);
    }
    const { summary } = await shootMatrix({
      base: args.base ? String(args.base) : PROTOTYPE_PATH,
      outDir: args.out ? String(args.out) : DEFAULT_OUT_DIR,
      width: numberOption(args.width, 1440),
      height: numberOption(args.height, 900),
      waitMs: args.wait === undefined ? null : Number(args.wait),
      only,
      themes,
      report: args.report ? String(args.report) : null,
      chromePath: args.chrome || args.chromePath,
      reducedMotion: args.reducedMotion !== false,
    });
    if (args.strict && !summary.ok) process.exitCode = 1;
    return;
  }

  throw new Error(`未知命令：${command}\n\n${HELP}`);
}

const isMain =
  process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));

if (isMain) {
  main().catch((error) => {
    console.error(`\n[shoot] 失败：${error.stack || error.message}`);
    process.exitCode = 1;
  });
}
