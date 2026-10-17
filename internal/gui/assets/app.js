/* AgentSweep front-end. No frameworks, no build step, no external assets. */
(() => {
  "use strict";

  /* ------------------------------------------------------------- constants */

  const SVG_NS = "http://www.w3.org/2000/svg";

  const GROUPS = [
    ["cli", "命令行"],
    ["ide", "IDE"],
    ["app", "桌面应用"],
    ["extension", "编辑器扩展"],
    ["other", "未收录"],
  ];
  const GROUP_LABELS = Object.fromEntries(GROUPS);
  const STORAGE_LABELS = { file: "文件", dir: "目录", sqlite: "SQLite 行" };

  const DEFAULT_PREFS = {
    deleteMode: "trash",
    confirmBeforeDelete: true,
    includeSnapshots: true,
    autoScan: true,
  };

  const state = {
    mock: false,
    mockApi: null,
    data: null,
    agents: [],
    agentById: new Map(),
    sessions: [],
    sessionByUid: new Map(),
    batches: [],
    prefs: { ...DEFAULT_PREFS },
    scan: { status: "idle", progress: { done: 0, total: 0, current: "" }, issues: [] },

    agentFilter: "all",
    search: "",
    sortKey: "updatedAt",
    sortDir: "desc",

    selected: new Set(),
    focusUid: null,
    anchorUid: null,
    detailUid: null,

    detailOpen: true,
    lastInput: "pointer",
    sheet: null,
    sheetReturnFocus: null,
    modalReturnFocus: null,
    pendingDeleteUids: null,

    pollTimer: null,
    initialLoading: true,
    autoScanChecked: false,
    deleting: false,
    didInitialSelect: false,
    discover: { candidates: [], loaded: false, loading: false, error: "" },
  };

  const dom = {};

  /* ----------------------------------------------------------------- utils */

  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value === null || value === undefined || value === false) continue;
        if (key === "class") node.className = value;
        else if (key === "text") node.textContent = String(value);
        else if (key === "dataset") {
          for (const [k, v] of Object.entries(value)) {
            if (v !== null && v !== undefined) node.dataset[k] = String(v);
          }
        } else if (key === "style") {
          Object.assign(node.style, value);
        } else if (value === true) {
          node.setAttribute(key, "");
        } else {
          node.setAttribute(key, String(value));
        }
      }
    }
    append(node, children);
    return node;
  }

  function append(node, children) {
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
  }

  function icon(name, extraClass) {
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", extraClass ? `icon ${extraClass}` : "icon");
    svg.setAttribute("aria-hidden", "true");
    const use = document.createElementNS(SVG_NS, "use");
    use.setAttribute("href", `#i-${name}`);
    svg.append(use);
    return svg;
  }

  function clone(value) {
    return value === undefined ? undefined : JSON.parse(JSON.stringify(value));
  }

  function sum(list, key) {
    return list.reduce((total, item) => total + (Number(item && item[key]) || 0), 0);
  }

  function pad2(n) {
    return String(n).padStart(2, "0");
  }

  function formatMessageCount(messages) {
    const n = Number(messages);
    return Number.isFinite(n) && n > 0 ? String(n) : "—";
  }

  function formatBytes(bytes) {
    const n = Number(bytes) || 0;
    if (n <= 0) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let value = n;
    let index = 0;
    while (value >= 1024 && index < units.length - 1) {
      value /= 1024;
      index += 1;
    }
    const digits = index === 0 ? 0 : value < 100 ? 1 : 0;
    let text = value.toFixed(digits);
    if (text.includes(".")) text = text.replace(/\.0+$/, "");
    return `${text} ${units[index]}`;
  }

  function parseDate(value) {
    const time = Date.parse(String(value || ""));
    return Number.isFinite(time) ? time : 0;
  }

  function formatClock(date) {
    return `${pad2(date.getHours())}:${pad2(date.getMinutes())}`;
  }

  function isSameDay(a, b) {
    return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  }

  function formatRelativeTime(value) {
    const time = parseDate(value);
    if (!time) return "—";
    const date = new Date(time);
    const now = new Date();
    const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
    const dayDiff = Math.floor((startOfToday - new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime()) / 86400000);

    if (isSameDay(date, now)) return `今天 ${formatClock(date)}`;
    if (dayDiff === 1) return `昨天 ${formatClock(date)}`;
    if (dayDiff > 1 && dayDiff < 7) {
      const weekdays = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
      return `${weekdays[date.getDay()]} ${formatClock(date)}`;
    }
    if (date.getFullYear() === now.getFullYear()) return `${date.getMonth() + 1}月${date.getDate()}日`;
    return `${date.getFullYear()}/${pad2(date.getMonth() + 1)}/${pad2(date.getDate())}`;
  }

  function formatDateTime(value) {
    const time = parseDate(value);
    if (!time) return "—";
    const date = new Date(time);
    return `${date.getFullYear()}年${date.getMonth() + 1}月${date.getDate()}日 ${formatClock(date)}`;
  }

  function formatShortDate(value) {
    const time = parseDate(value);
    if (!time) return "—";
    const date = new Date(time);
    return `${date.getMonth() + 1}月${date.getDate()}日 ${formatClock(date)}`;
  }

  function normalizeScan(scan) {
    const source = scan && typeof scan === "object" ? scan : {};
    const progress = source.progress && typeof source.progress === "object" ? source.progress : {};
    const status = ["idle", "running", "done", "error"].includes(source.status) ? source.status : "idle";
    return {
      ...source,
      status,
      progress: {
        done: Number(progress.done) || 0,
        total: Number(progress.total) || 0,
        current: typeof progress.current === "string" ? progress.current : "",
      },
      issues: Array.isArray(source.issues) ? source.issues : [],
    };
  }

  function isTypingTarget(target) {
    if (!(target instanceof Element)) return false;
    const tag = target.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.isContentEditable;
  }

  function isOverlayOpen() {
    return state.sheet !== null || !dom.confirmModal.hidden;
  }

  function agentName(agentId) {
    const agent = state.agentById.get(agentId);
    return agent ? agent.name || agent.id : agentId;
  }

  function initialLetter(name) {
    const text = String(name || "").trim();
    return text ? text.slice(0, 1).toUpperCase() : "?";
  }

  function displayPath(path) {
    if (!path) return "—";
    const home = state.data && typeof state.data.home === "string" ? state.data.home.replace(/\/+$/, "") : "";
    if (home && path === home) return "~";
    if (home && path.startsWith(`${home}/`)) return `~${path.slice(home.length)}`;
    return path;
  }

  /* ------------------------------------------------------------------- api */

  class ApiError extends Error {
    constructor(message, status) {
      super(message);
      this.name = "ApiError";
      this.status = status || 0;
    }
  }

  async function api(method, path, body) {
    if (state.mock && state.mockApi) return state.mockApi.request(method, path, body);

    const options = { method, headers: { Accept: "application/json" } };
    if (body !== undefined) {
      options.headers["Content-Type"] = "application/json";
      options.body = JSON.stringify(body);
    }

    let response;
    try {
      response = await fetch(path, options);
    } catch (err) {
      throw new ApiError("无法连接本地服务", 0);
    }

    let data = null;
    const type = response.headers.get("content-type") || "";
    if (type.includes("application/json")) {
      try {
        data = await response.json();
      } catch (err) {
        data = null;
      }
    }

    if (!response.ok) {
      throw new ApiError((data && data.error) || `请求失败（HTTP ${response.status}）`, response.status);
    }
    return data;
  }

  /* ------------------------------------------------------------ mock backend */

  function createMockBackend() {
    const now = Date.now();
    const ago = (minutes) => new Date(now - minutes * 60000).toISOString();
    const claudePath = (id) => `/Users/x/.claude/projects/-Users-x-Projects-agent-sweep/${id}.jsonl`;
    const cursorPath = (id) => `/Users/x/Library/Application Support/Cursor/User/workspaceStorage/${id}/chatSessions/${id}.jsonl`;

    const agents = [
      {
        id: "claude-code",
        name: "Claude Code",
        group: "cli",
        vendor: "Anthropic",
        installed: true,
        home: "/Users/x/.claude",
        roots: ["/Users/x/.claude"],
        note: "删除会同步裁剪 history.jsonl 与 sessions-index.json，否则 Agent 里会留下点进去是空的幽灵会话。索引会先备份到 Application Support/AgentSweep/backups/。",
        custom: false,
        sessionCount: 0,
        totalBytes: 0,
        error: "",
      },
      {
        id: "cursor",
        name: "Cursor",
        group: "ide",
        vendor: "Anysphere",
        installed: true,
        home: "/Users/x/Library/Application Support/Cursor",
        roots: ["/Users/x/Library/Application Support/Cursor"],
        note: "Cursor 同时维护 chatSessions 文件与 state.vscdb 两套索引，版本之间布局会变。删除时会一起处理文件与索引条目，并备份整库。",
        custom: false,
        sessionCount: 0,
        totalBytes: 0,
        error: "",
      },
      {
        id: "workbuddy",
        name: "WorkBuddy",
        group: "app",
        vendor: "Tencent",
        installed: false,
        home: "",
        roots: ["/Users/x/.workbuddy"],
        note: "目录结构随版本变化，未命中时会出现在「发现」页。",
        custom: false,
        sessionCount: 0,
        totalBytes: 0,
        error: "",
      },
    ];

    const sessions = [
      {
        uid: "claude-code:c1",
        agentId: "claude-code",
        id: "c1",
        title: "重构 catalog registry 的加载顺序",
        project: "/Users/x/Projects/agent-sweep",
        updatedAt: ago(18),
        bytes: 184320,
        messages: 42,
        snippet: "把 Spec 的解析从 switch 改成表驱动，顺便补上 roots 的候选顺序测试…",
        active: true,
        storage: "file",
        paths: [claudePath("9f2c1d")],
      },
      {
        uid: "claude-code:c2",
        agentId: "claude-code",
        id: "c2",
        title: "排查 SQLite 删行后的 FTS 残留",
        project: "/Users/x/Projects/agent-sweep",
        updatedAt: ago(3 * 60 + 12),
        bytes: 96768,
        messages: 28,
        snippet: "VACUUM 之前先重建 FTS 索引，否则搜索还是会命中空会话。",
        active: false,
        storage: "file",
        paths: [claudePath("4ab771")],
      },
      {
        uid: "claude-code:c3",
        agentId: "claude-code",
        id: "c3",
        title: "扫描器只读每份会话的头部 64KB",
        project: "/Users/x/Projects/agent-sweep",
        updatedAt: ago(26 * 60),
        bytes: 61440,
        messages: 19,
        snippet: "标题、项目路径、时间戳都在前几行，全量读取纯属浪费。",
        active: false,
        storage: "file",
        paths: [claudePath("77d0e2")],
      },
      {
        uid: "claude-code:c4",
        agentId: "claude-code",
        id: "c4",
        title: "macOS 废纸篓的撤销语义",
        project: "/Users/x/Projects/agent-sweep",
        updatedAt: ago(2 * 24 * 60 + 40),
        bytes: 41984,
        messages: 14,
        snippet: "进废纸篓不算真正释放空间，freed 在 trash 模式下必须是 0。",
        active: false,
        storage: "file",
        paths: [claudePath("1c9b03")],
      },
      {
        uid: "claude-code:c5",
        agentId: "claude-code",
        id: "c5",
        title: "给发现页写路径启发式",
        project: "/Users/x/Projects/agent-discover",
        updatedAt: ago(3 * 24 * 60 + 95),
        bytes: 73728,
        messages: 23,
        snippet: "目录名含 sessions、文件后缀集中、最近 30 天有写入 —— 三个信号加权。",
        active: false,
        storage: "file",
        paths: ["/Users/x/.claude/projects/-Users-x-Projects-agent-discover/2f8d11.jsonl"],
      },
      {
        uid: "claude-code:c6",
        agentId: "claude-code",
        id: "c6",
        title: "三栏布局的第一版草稿",
        project: "/Users/x/Projects/agentsweep-ui",
        updatedAt: ago(4 * 24 * 60 + 30),
        bytes: 122880,
        messages: 31,
        snippet: "左栏 Agent、中栏会话、右栏详情；不知道右栏应不应该默认展开。",
        active: false,
        storage: "file",
        paths: ["/Users/x/.claude/projects/-Users-x-Projects-agentsweep-ui/5be446.jsonl"],
      },
      {
        uid: "claude-code:c7",
        agentId: "claude-code",
        id: "c7",
        title: "周报：本周清理了多少会话",
        project: "/Users/x/Documents/notes",
        updatedAt: ago(6 * 24 * 60 + 15),
        bytes: 20480,
        messages: 9,
        snippet: "把 batch 的 freed 字段按天汇总，注意废纸篓批次的 0 值。",
        active: false,
        storage: "file",
        paths: ["/Users/x/.claude/projects/-Users-x-Documents-notes/a1e908.jsonl"],
      },
      {
        uid: "claude-code:c8",
        agentId: "claude-code",
        id: "c8",
        title: "JSONL 大文件的流式读取笔记",
        project: "/Users/x/Documents/notes",
        updatedAt: ago(9 * 24 * 60 + 120),
        bytes: 30720,
        messages: 12,
        snippet: "边界条件：a < b 时优先按体积排序，再按时间排序。",
        active: false,
        storage: "file",
        paths: ["/Users/x/.claude/projects/-Users-x-Documents-notes/88c3aa.jsonl"],
      },
      {
        uid: "cursor:cu1",
        agentId: "cursor",
        id: "cu1",
        title: "会话列表滚动卡顿的定位",
        project: "/Users/x/Projects/harness-web",
        updatedAt: ago(45),
        bytes: 1613824,
        messages: 63,
        snippet: "每次选中都重建整张表，滚动容器的高度被反复重置。",
        active: false,
        storage: "file",
        paths: [cursorPath("1a7f0c")],
      },
      {
        uid: "cursor:cu2",
        agentId: "cursor",
        id: "cu2",
        title: "Cursor 的 state.vscdb 索引结构",
        project: "/Users/x/Projects/agent-sweep",
        updatedAt: ago(5 * 60 + 8),
        bytes: 245760,
        messages: 37,
        snippet: "cursorDiskKV 里存着会话元数据，删除时要连键一起删。",
        active: false,
        storage: "sqlite",
        paths: [],
      },
      {
        uid: "cursor:cu3",
        agentId: "cursor",
        id: "cu3",
        title: "把 Tailwind 配置迁回 CSS 变量",
        project: "/Users/x/Projects/harness-web",
        updatedAt: ago(2 * 24 * 60 + 6 * 60),
        bytes: 389120,
        messages: 54,
        snippet: "主题色只剩一处来源，深色模式不用再写第二套规则体。",
        active: false,
        storage: "file",
        paths: [cursorPath("bb32e1")],
      },
      {
        uid: "cursor:cu4",
        agentId: "cursor",
        id: "cu4",
        title: "离线环境的字体回退策略",
        project: "/Users/x/Projects/harness-web",
        updatedAt: ago(3 * 24 * 60 + 4 * 60),
        bytes: 81920,
        messages: 18,
        snippet: "不引 CDN 字体，直接依赖系统的 SF Pro 与 PingFang。",
        active: false,
        storage: "file",
        paths: [cursorPath("cf44a0")],
      },
      {
        uid: "cursor:cu5",
        agentId: "cursor",
        id: "cu5",
        title: "导出 token 用量的临时脚本",
        project: "/Users/x/Projects/tools",
        updatedAt: ago(5 * 24 * 60 + 180),
        bytes: 51200,
        messages: 11,
        snippet: "按天聚合 usage，先别管精度，能看趋势就行。",
        active: false,
        storage: "file",
        paths: [cursorPath("3e0cd9")],
      },
      {
        uid: "cursor:cu6",
        agentId: "cursor",
        id: "cu6",
        title: "路径展开 ~ 的单元测试",
        project: "/Users/x/Projects/tools",
        updatedAt: ago(8 * 24 * 60 + 60),
        bytes: 40960,
        messages: 16,
        snippet: "覆盖 ~、~/foo、$HOME/foo 三种写法，最后一种要用 os.ExpandEnv。",
        active: false,
        storage: "file",
        paths: [cursorPath("a5d1f7")],
      },
      {
        uid: "cursor:cu7",
        agentId: "cursor",
        id: "cu7",
        title: "旧项目的图片压缩脚本",
        project: "/Users/x/Projects/legacy-site",
        updatedAt: ago(19 * 24 * 60),
        bytes: 93184,
        messages: 22,
        snippet: "sharp 的 quality 参数在 78 附近性价比最高。",
        active: false,
        storage: "file",
        paths: [cursorPath("2b8c16")],
      },
    ];

    const candidates = [
      {
        path: "/Users/x/Library/Application Support/Warp/agent-sessions",
        name: "Warp Agent",
        confidence: 0.82,
        sessionCount: 6,
        bytes: 3565158,
        evidence: ["目录名含 sessions", "6 个会话子目录里有 conversation.json", "最近 7 天有写入"],
        suggested: {
          id: "warp-agent",
          name: "Warp Agent",
          group: "other",
          vendor: "Warp",
          bundles: ["Warp.app"],
          roots: ["~/Library/Application Support/Warp/agent-sessions"],
          env: [],
          stores: [{ kind: "dir", glob: "*" }],
          indexes: [],
          note: "由「发现」收录。Warp 的会话按目录存放，删除整个目录即可。",
        },
      },
      {
        path: "/Users/x/.muse/sessions",
        name: "Muse",
        confidence: 0.64,
        sessionCount: 3,
        bytes: 921600,
        evidence: ["3 个 .jsonl 文件", "文件头部含 role / createdAt 字段", "目录不在内置目录中"],
        suggested: {
          id: "muse",
          name: "Muse",
          group: "other",
          vendor: "Muse Labs",
          bundles: [],
          roots: ["~/.muse"],
          env: [],
          stores: [{ kind: "jsonl", glob: "sessions/*.jsonl" }],
          indexes: [],
          note: "由「发现」收录。会话为每个 .jsonl 一个文件。",
        },
      },
    ];

    const seed = {
      version: "0.1.0",
      home: "/Users/x",
      platform: "darwin",
      prefs: { ...DEFAULT_PREFS },
      scan: {
        status: "done",
        startedAt: ago(3),
        finishedAt: ago(2.8),
        durationMs: 2380,
        progress: { done: 30, total: 30, current: "" },
        issues: [],
      },
      agents,
      sessions,
      batches: [
        {
          id: "20261017-153012",
          at: ago(3 * 60 + 18),
          count: 4,
          freed: 0,
          agents: ["claude-code", "cursor"],
          undoable: true,
        },
      ],
      candidates,
    };

    const db = clone(seed);
    const trash = new Map();
    let scanTimer = null;
    let batchSeq = 0;

    const scanSources = [
      "Claude Code", "Cursor", "Codex", "Gemini CLI", "Zed AI",
      "Cline", "Windsurf", "Trae", "OpenCode", "workbuddy",
    ];

    function nextBatchId() {
      const d = new Date();
      const stamp = `${d.getFullYear()}${pad2(d.getMonth() + 1)}${pad2(d.getDate())}-${pad2(d.getHours())}${pad2(d.getMinutes())}${pad2(d.getSeconds())}`;
      batchSeq += 1;
      return batchSeq > 1 ? `${stamp}-${batchSeq}` : stamp;
    }

    function bootstrap() {
      const counts = new Map();
      for (const session of db.sessions) {
        const item = counts.get(session.agentId) || { count: 0, bytes: 0 };
        item.count += 1;
        item.bytes += Number(session.bytes) || 0;
        counts.set(session.agentId, item);
      }
      return {
        version: db.version,
        home: db.home,
        platform: db.platform,
        prefs: clone(db.prefs),
        scan: clone(db.scan),
        agents: db.agents.map((agent) => {
          const item = counts.get(agent.id) || { count: 0, bytes: 0 };
          return {
            ...clone(agent),
            sessionCount: item.count,
            totalBytes: item.bytes,
          };
        }),
        sessions: clone(db.sessions),
        batches: clone(db.batches),
      };
    }

    function startScan() {
      if (db.scan.status === "running") return { ok: true };
      const started = Date.now();
      db.scan = {
        status: "running",
        startedAt: new Date(started).toISOString(),
        finishedAt: "",
        durationMs: 0,
        progress: { done: 0, total: 30, current: "Claude Code" },
        issues: [],
      };
      if (scanTimer) clearInterval(scanTimer);
      scanTimer = setInterval(() => {
        if (db.scan.status !== "running") return;
        const done = Math.min(30, db.scan.progress.done + 2);
        db.scan.progress.done = done;
        db.scan.progress.current = scanSources[Math.min(scanSources.length - 1, Math.floor(done / 3))] || "";
        if (done >= 30) {
          db.scan.status = "done";
          db.scan.finishedAt = new Date().toISOString();
          db.scan.durationMs = Date.now() - started;
          db.scan.progress.current = "";
          clearInterval(scanTimer);
          scanTimer = null;
        }
      }, 250);
      return { ok: true };
    }

    function deleteSessions(body) {
      const uids = new Set(Array.isArray(body && body.uids) ? body.uids.map(String) : []);
      const removed = db.sessions.filter((session) => uids.has(session.uid));
      const mode = db.prefs.deleteMode === "permanent" ? "permanent" : "trash";
      const freed = mode === "permanent" ? sum(removed, "bytes") : 0;
      db.sessions = db.sessions.filter((session) => !uids.has(session.uid));

      const batch = {
        id: nextBatchId(),
        at: new Date().toISOString(),
        count: removed.length,
        freed,
        agents: [...new Set(removed.map((session) => session.agentId))],
        undoable: mode === "trash" && removed.length > 0,
      };
      db.batches.unshift(batch);
      if (batch.undoable) trash.set(batch.id, removed);
      return { batchId: batch.id, freed, count: removed.length, errors: [] };
    }

    function undoBatch(body) {
      const id = body && body.batchId;
      const batch = db.batches.find((item) => item.id === id);
      const removed = trash.get(id);
      if (!batch || !batch.undoable || !removed) throw new Error("这个批次不可撤销");
      db.sessions.push(...removed);
      trash.delete(id);
      batch.undoable = false;
      return { restored: removed.length, errors: [] };
    }

    function adoptAdapter(spec) {
      if (!spec || typeof spec !== "object") throw new Error("适配器内容为空");
      const id = String(spec.id || "").trim();
      if (!id) throw new Error("适配器缺少 id");
      if (db.agents.some((agent) => agent.id === id)) throw new Error("这个 Agent 已经收录过了");

      const candidate = db.candidates.find((item) => item.suggested && item.suggested.id === id)
        || db.candidates.find((item) => spec.roots && item.path === String(spec.roots[0] || "").replace(/^~/, "/Users/x"));

      db.agents.push({
        id,
        name: spec.name || id,
        group: spec.group || "other",
        vendor: spec.vendor || "",
        installed: true,
        home: candidate ? candidate.path : String((spec.roots && spec.roots[0]) || "").replace(/^~/, "/Users/x"),
        roots: Array.isArray(spec.roots) ? spec.roots : [],
        note: spec.note || "",
        custom: true,
        sessionCount: 0,
        totalBytes: 0,
        error: "",
      });

      if (candidate) {
        const count = Math.min(Number(candidate.sessionCount) || 0, 8);
        for (let index = 0; index < count; index += 1) {
          const path = `${candidate.path}/session-${pad2(index + 1)}`;
          db.sessions.push({
            uid: `${id}:demo-${index + 1}`,
            agentId: id,
            id: `demo-${index + 1}`,
            title: `${candidate.name} 会话 ${index + 1}`,
            project: "/Users/x/Projects/demo-project",
            updatedAt: new Date(Date.now() - (index + 1) * 6 * 3600000).toISOString(),
            bytes: 40960 + index * 8192,
            messages: 8 + index * 3,
            snippet: "由「发现」收录的演示会话，还没有解析出摘要。",
            active: false,
            storage: "file",
            paths: [path],
          });
        }
        db.candidates = db.candidates.filter((item) => item !== candidate);
      }
      return { ok: true, id };
    }

    function updatePrefs(patch) {
      if (!patch || typeof patch !== "object") return { ok: true, prefs: clone(db.prefs) };
      if (["trash", "permanent"].includes(patch.deleteMode)) db.prefs.deleteMode = patch.deleteMode;
      for (const key of ["confirmBeforeDelete", "includeSnapshots", "autoScan"]) {
        if (typeof patch[key] === "boolean") db.prefs[key] = patch[key];
      }
      return { ok: true, prefs: clone(db.prefs) };
    }

    async function request(method, path, body) {
      const verb = String(method || "GET").toUpperCase();
      const endpoint = String(path || "").split("?")[0];

      if (verb === "GET" && endpoint === "/api/bootstrap") return bootstrap();
      if (verb === "POST" && endpoint === "/api/scan") return startScan();
      if (verb === "GET" && endpoint === "/api/scan") return clone(db.scan);
      if (verb === "POST" && endpoint === "/api/delete") return deleteSessions(body);
      if (verb === "POST" && endpoint === "/api/undo") return undoBatch(body);
      if (verb === "GET" && endpoint === "/api/discover") return { candidates: clone(db.candidates) };
      if (verb === "POST" && endpoint === "/api/adapters") return adoptAdapter(body);
      if (verb === "GET" && endpoint === "/api/prefs") return clone(db.prefs);
      if (verb === "PUT" && endpoint === "/api/prefs") return updatePrefs(body);
      if (verb === "POST" && (endpoint === "/api/reveal" || endpoint === "/api/open")) return { ok: true };

      throw new Error(`演示后端没有实现 ${verb} ${endpoint}`);
    }

    return { request };
  }

  /* --------------------------------------------------------------- bootstrap */

  async function boot() {
    cacheDom();
    wireEvents();
    state.detailOpen = window.innerWidth >= 1020;
    syncDetailVisibility();
    syncPrefsUi();
    renderDetail();
    renderScanStatus();

    try {
      const data = await api("GET", "/api/bootstrap");
      applyBootstrap(data, { initial: true });
      void loadDiscover({ quiet: true });
      maybeAutoScan();
    } catch (err) {
      console.warn("AgentSweep: backend unavailable, switching to built-in demo data.", err);
      state.mock = true;
      state.mockApi = createMockBackend();
      try {
        const data = await api("GET", "/api/bootstrap");
        applyBootstrap(data, { initial: true });
        dom.demoBadge.hidden = false;
        dom.demoBadge.title = `无法连接本地服务，正在显示内置演示数据。${err && err.message ? `（${err.message}）` : ""}`;
        loadDiscover({ quiet: true });
        maybeAutoScan();
      } catch (mockErr) {
        console.error("AgentSweep: demo data failed", mockErr);
        renderScanStatus();
      }
    }
  }

  function cacheDom() {
    const ids = [
      "app", "demoBadge", "scanBtn", "scanBtnLabel", "scanKbd", "searchWrap", "searchInput",
      "searchKbd", "searchClear", "discoverBtn", "discoverCount", "discoverLabel", "detailToggle", "settingsBtn",
      "agentAll", "allCount", "allMeta", "agentGroups", "listTitle", "listSub", "tableWrap",
      "selectAll", "sessionBody", "initialLoading", "emptyState", "emptyTitle", "emptyDesc", "emptyPrimary",
      "detailPane", "detailClose", "detailBody", "scanStatus", "selectionSummary", "cleanBtn",
      "sheetLayer", "sheetBackdrop", "settingsSheet", "discoverSheet", "deleteModeHint",
      "prefConfirm", "prefSnapshots", "prefAutoScan", "batchNote", "batchList", "versionLabel",
      "discoverList", "confirmModal", "confirmDialog", "modalBackdrop", "confirmTitle", "confirmLede", "confirmAgents",
      "confirmActive", "confirmMode", "confirmModeText", "confirmError", "confirmCancel",
      "confirmDelete", "toasts",
    ];
    for (const id of ids) dom[id] = document.getElementById(id);
  }

  function wireEvents() {
    dom.scanBtn.addEventListener("click", () => {
      void startScan();
    });

    dom.settingsBtn.addEventListener("click", openSettings);
    dom.discoverBtn.addEventListener("click", openDiscover);

    dom.detailToggle.addEventListener("click", () => {
      setDetailOpen(!state.detailOpen);
    });
    dom.detailClose.addEventListener("click", () => {
      setDetailOpen(false);
    });

    dom.agentAll.addEventListener("click", () => selectAgent("all"));

    dom.searchInput.addEventListener("input", () => {
      state.search = dom.searchInput.value;
      syncSearchUi();
      renderSessions();
    });

    dom.searchClear.addEventListener("click", () => {
      clearSearch({ focus: true });
    });

    dom.selectAll.addEventListener("change", () => {
      const visible = getVisibleSessions();
      if (dom.selectAll.checked) {
        for (const session of visible) state.selected.add(session.uid);
        if (visible.length && (!state.focusUid || !state.selected.has(state.focusUid))) {
          state.focusUid = visible[0].uid;
        }
      } else {
        for (const session of visible) state.selected.delete(session.uid);
      }
      state.anchorUid = visible.length ? visible[0].uid : null;
      syncSelectionUi();
    });

    dom.sessionBody.addEventListener("change", (event) => {
      const target = event.target;
      if (!(target instanceof HTMLInputElement)) return;
      if (target.type !== "checkbox" || !target.dataset.uid) return;
      setSelected(target.dataset.uid, target.checked);
    });

    dom.sessionBody.addEventListener("click", (event) => {
      const target = event.target;
      if (!(target instanceof Element)) return;
      if (target.closest("input, button, a, label")) return;
      const row = target.closest("tr[data-uid]");
      if (!row || !dom.sessionBody.contains(row)) return;
      const uid = row.dataset.uid;
      if (event.shiftKey) {
        selectRange(uid);
        return;
      }
      if (event.metaKey || event.ctrlKey) {
        setSelected(uid, !state.selected.has(uid));
        return;
      }
      selectOnly(uid);
    });

    document.querySelectorAll(".th-sort").forEach((button) => {
      button.addEventListener("click", () => setSort(button.dataset.sort));
    });

    dom.cleanBtn.addEventListener("click", requestDelete);

    dom.confirmCancel.addEventListener("click", () => closeConfirm());
    dom.modalBackdrop.addEventListener("click", () => closeConfirm());
    dom.confirmDelete.addEventListener("click", () => {
      void submitPendingDelete();
    });

    dom.sheetBackdrop.addEventListener("click", () => closeSheet());
    document.querySelectorAll("[data-close-sheet]").forEach((button) => {
      button.addEventListener("click", () => closeSheet());
    });

    document.querySelectorAll('input[name="deleteMode"]').forEach((input) => {
      input.addEventListener("change", () => {
        if (!input.checked) return;
        void updatePref({ deleteMode: input.value });
      });
    });

    dom.prefConfirm.addEventListener("change", () => {
      void updatePref({ confirmBeforeDelete: dom.prefConfirm.checked });
    });
    dom.prefSnapshots.addEventListener("change", () => {
      void updatePref({ includeSnapshots: dom.prefSnapshots.checked });
    });
    dom.prefAutoScan.addEventListener("change", () => {
      void updatePref({ autoScan: dom.prefAutoScan.checked });
    });

    document.addEventListener("pointerdown", () => {
      state.lastInput = "pointer";
    }, true);
    document.addEventListener("keydown", () => {
      state.lastInput = "keyboard";
    }, true);
    document.addEventListener("keydown", onKeydown);

    window.addEventListener("resize", () => {
      if (window.innerWidth < 900 && state.detailOpen && !state.userToggledDetail) {
        setDetailOpen(false);
      }
    });
  }

  /* --------------------------------------------------------------- bootstrap data */

  function applyBootstrap(data, options) {
    const opts = options || {};
    state.data = data && typeof data === "object" ? data : {};
    state.agents = Array.isArray(state.data.agents) ? state.data.agents : [];
    state.agentById = new Map(state.agents.map((agent) => [agent.id, agent]));
    state.sessions = (Array.isArray(state.data.sessions) ? state.data.sessions : []).filter(
      (session) => session && typeof session.uid === "string" && typeof session.agentId === "string"
    );
    state.sessionByUid = new Map(state.sessions.map((session) => [session.uid, session]));
    state.batches = Array.isArray(state.data.batches) ? state.data.batches : [];
    state.prefs = { ...DEFAULT_PREFS, ...(state.data.prefs && typeof state.data.prefs === "object" ? state.data.prefs : {}) };
    state.scan = normalizeScan(state.data.scan);

    for (const uid of [...state.selected]) {
      if (!state.sessionByUid.has(uid)) state.selected.delete(uid);
    }
    if (state.focusUid && !state.selected.has(state.focusUid)) state.focusUid = null;
    if (!state.focusUid && state.selected.size === 1) state.focusUid = [...state.selected][0];
    if (state.agentFilter !== "all" && !state.agentById.has(state.agentFilter)) state.agentFilter = "all";

    // Select the first visible session exactly once — after the first data load
    // (or after the first scan finishes if the initial load was still empty),
    // so the detail pane is useful immediately and never fights the user later.
    if (!state.didInitialSelect && state.selected.size === 0) {
      const first = getVisibleSessions()[0];
      if (first) {
        state.selected.add(first.uid);
        state.focusUid = first.uid;
        state.anchorUid = first.uid;
        state.didInitialSelect = true;
      }
    }

    state.initialLoading = false;
    dom.initialLoading.hidden = true;
    dom.demoBadge.hidden = !state.mock;
    dom.versionLabel.textContent = state.data.version || "0.1.0";

    renderAgents();
    renderSessions();
    renderScanStatus();
    renderPrefs();
    renderBatches();
    syncDiscoverCount();

    if (state.scan.status === "running") ensureScanPolling(250);
  }

  async function refreshBootstrap(options) {
    const opts = options || {};
    try {
      const data = await api("GET", "/api/bootstrap");
      applyBootstrap(data);
    } catch (err) {
      if (!opts.quiet) {
        showToast("刷新失败", { detail: err.message || "无法读取最新数据", tone: "error", icon: "alert" });
      }
    }
  }

  /* --------------------------------------------------------------- rendering */

  function getVisibleSessions() {
    const query = state.search.trim().toLocaleLowerCase();
    const filtered = state.sessions.filter((session) => {
      if (state.agentFilter !== "all" && session.agentId !== state.agentFilter) return false;
      if (!query) return true;
      const haystack = [session.title, session.project, session.snippet, session.id, agentName(session.agentId)]
        .map((value) => String(value || "").toLocaleLowerCase())
        .join("\n");
      return haystack.includes(query);
    });
    filtered.sort(compareSessions);
    return filtered;
  }

  function compareSessions(a, b) {
    let result = 0;
    if (state.sortKey === "bytes") {
      result = (Number(a.bytes) || 0) - (Number(b.bytes) || 0);
    } else if (state.sortKey === "title") {
      result = String(a.title || "").localeCompare(String(b.title || ""), "zh-Hans-CN", {
        numeric: true,
        sensitivity: "base",
      });
    } else {
      result = parseDate(a.updatedAt) - parseDate(b.updatedAt);
    }
    if (result === 0) {
      result = String(a.title || "").localeCompare(String(b.title || ""), "zh-Hans-CN", { numeric: true });
    }
    return state.sortDir === "asc" ? result : -result;
  }

  function setSort(key) {
    if (!["title", "bytes", "updatedAt"].includes(key)) return;
    if (state.sortKey === key) {
      state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
    } else {
      state.sortKey = key;
      state.sortDir = key === "title" ? "asc" : "desc";
    }
    renderSessions();
  }

  function renderAgents() {
    dom.allCount.textContent = String(state.sessions.length);
    dom.allMeta.textContent = state.sessions.length ? formatBytes(sum(state.sessions, "bytes")) : "所有 Agent";

    const isAll = state.agentFilter === "all";
    dom.agentAll.classList.toggle("is-active", isAll);
    if (isAll) dom.agentAll.setAttribute("aria-current", "true");
    else dom.agentAll.removeAttribute("aria-current");

    const grouped = new Map(GROUPS.map(([key]) => [key, []]));
    for (const agent of state.agents) {
      const key = grouped.has(agent.group) ? agent.group : "other";
      grouped.get(key).push(agent);
    }

    dom.agentGroups.textContent = "";

    if (state.agents.length === 0) {
      dom.agentGroups.append(
        el("div", {
          class: "agent-empty",
          text:
            state.scan.status === "running"
              ? "正在扫描，找到的 Agent 会出现在这里…"
              : "还没有识别到 Agent。扫描一次，或到「发现」里收录一个。",
        })
      );
      return;
    }

    for (const [key, label] of GROUPS) {
      const agents = grouped.get(key);
      if (!agents || agents.length === 0) continue;

      const section = el("section", { class: "agent-group" });
      section.append(el("div", { class: "group-title", text: label }));
      for (const agent of agents) section.append(buildAgentRow(agent));
      dom.agentGroups.append(section);
    }
  }

  function buildAgentRow(agent) {
    const active = state.agentFilter === agent.id;
    const installed = agent.installed !== false;
    const count = Number(agent.sessionCount) || 0;
    const bytes = Number(agent.totalBytes) || 0;
    const meta = !installed ? "未安装" : agent.error ? "读取失败" : formatBytes(bytes);

    const row = el("button", {
      type: "button",
      class: [
        "agent-row",
        active ? "is-active" : "",
        installed ? "" : "is-uninstalled",
        agent.error ? "is-error" : "",
      ]
        .filter(Boolean)
        .join(" "),
      title: agent.error || (installed ? `${agent.name || agent.id} · ${count} 条会话 · ${formatBytes(bytes)}` : "未安装 · 数据目录不存在"),
    });
    if (active) row.setAttribute("aria-current", "true");

    row.append(
      el("span", { class: "agent-glyph", "aria-hidden": "true", text: initialLetter(agent.name || agent.id) }),
      el("span", { class: "agent-name", text: agent.name || agent.id }),
      el("span", { class: "agent-count", text: String(count) }),
      el("span", { class: "agent-meta", text: meta })
    );
    row.addEventListener("click", () => selectAgent(agent.id));
    return row;
  }

  function selectAgent(agentId) {
    if (state.agentFilter === agentId) return;
    state.agentFilter = agentId;
    state.anchorUid = null;
    dom.tableWrap.scrollTop = 0;
    renderAgents();
    renderSessions();
  }

  function renderSessions() {
    const visible = getVisibleSessions();
    renderListHead(visible);
    renderSortState();
    renderRows(visible);
    updateSelectAll(visible);
    renderEmptyState(visible);
    renderDetail();
    renderStatus();
  }

  function renderListHead(visible) {
    const agent = state.agentFilter === "all" ? null : state.agentById.get(state.agentFilter);
    dom.listTitle.textContent = agent ? agent.name || agent.id : "全部会话";

    const bytes = sum(visible, "bytes");
    if (state.search.trim()) {
      dom.listSub.textContent = `匹配 ${visible.length} 条 · ${formatBytes(bytes)}`;
    } else if (state.sessions.length === 0 && state.scan.status === "running") {
      dom.listSub.textContent = "正在扫描…";
    } else if (agent && agent.installed === false) {
      dom.listSub.textContent = "未安装";
    } else {
      dom.listSub.textContent = `${visible.length} 条会话 · ${formatBytes(bytes)}`;
    }
  }

  function renderSortState() {
    const labels = { title: "标题", bytes: "体积", updatedAt: "最后更新" };
    document.querySelectorAll(".th-sort").forEach((button) => {
      const th = button.closest("th");
      if (!th) return;
      const label = labels[button.dataset.sort] || button.textContent;
      if (button.dataset.sort === state.sortKey) {
        th.setAttribute("aria-sort", state.sortDir === "asc" ? "ascending" : "descending");
        button.title = `按${label}${state.sortDir === "asc" ? "升序" : "降序"}排列，点击切换`;
      } else {
        th.removeAttribute("aria-sort");
        button.title = `按${label}排序`;
      }
    });
  }

  function renderRows(visible) {
    const fragment = document.createDocumentFragment();
    for (const session of visible) fragment.append(buildSessionRow(session));
    dom.sessionBody.textContent = "";
    dom.sessionBody.append(fragment);
  }

  function buildSessionRow(session) {
    const selected = state.selected.has(session.uid);
    const row = el("tr", {
      class: selected ? "is-selected" : "",
      dataset: { uid: session.uid },
    });

    const checkCell = el("td", { class: "cell-check" });
    const checkbox = el("input", {
      type: "checkbox",
      "aria-label": `选择「${session.title || "未命名会话"}」`,
    });
    checkbox.checked = selected;
    checkbox.dataset.uid = session.uid;
    checkCell.append(checkbox);

    const titleCell = el("td", { class: "cell-title" });
    const inner = el("div", { class: "cell-title-inner" });
    const titleLine = el("div", { class: "title-line" });
    titleLine.append(
      el("span", {
        class: "session-title",
        text: session.title || "未命名会话",
        title: session.title || "未命名会话",
      })
    );
    if (session.active) titleLine.append(el("span", { class: "chip-active", text: "使用中" }));
    inner.append(titleLine);
    if (session.snippet) {
      inner.append(el("span", { class: "snippet", text: session.snippet, title: session.snippet }));
    }
    titleCell.append(inner);

    const projectCell = el("td", { class: "col-project" });
    projectCell.append(
      el("span", {
        class: "project-path",
        text: displayPath(session.project),
        title: session.project || "",
      })
    );

    const rawMessages = Number(session.messages) || 0;
    const messageCell = el("td", {
      class: "cell-num",
      text: formatMessageCount(rawMessages),
      title: rawMessages > 0 ? `${rawMessages} 条消息` : "消息数无法统计",
    });

    const sizeCell = el("td", { class: "cell-num", text: formatBytes(session.bytes) });
    sizeCell.title = `${Number(session.bytes) || 0} 字节`;

    const timeCell = el("td", {
      class: "cell-time",
      text: formatRelativeTime(session.updatedAt),
      title: formatDateTime(session.updatedAt),
    });

    row.append(checkCell, titleCell, projectCell, messageCell, sizeCell, timeCell);
    return row;
  }

  function updateSelectAll(visible) {
    const selectedCount = visible.reduce((count, session) => count + (state.selected.has(session.uid) ? 1 : 0), 0);
    dom.selectAll.checked = visible.length > 0 && selectedCount === visible.length;
    dom.selectAll.indeterminate = selectedCount > 0 && selectedCount < visible.length;
    dom.selectAll.disabled = visible.length === 0;
  }

  function renderEmptyState(visible) {
    if (visible.length > 0) {
      dom.emptyState.hidden = true;
      dom.emptyPrimary.onclick = null;
      return;
    }

    dom.emptyState.hidden = false;
    const query = state.search.trim();
    const agent = state.agentFilter === "all" ? null : state.agentById.get(state.agentFilter);
    let title = "没有匹配的会话";
    let desc = "换个关键词，或切回「全部会话」看看。";
    let actionLabel = "清除筛选";
    let action = resetFilters;

    if (state.sessions.length === 0 && state.scan.status === "running") {
      const progress = state.scan.progress;
      const progressText = progress.total > 0
        ? `已扫描 ${progress.done}/${progress.total}${progress.current ? ` · ${progress.current}` : ""}`
        : "正在准备扫描…";
      title = "正在扫描本机会话";
      desc = `${progressText}。找到的会话会陆续出现在这里。`;
      actionLabel = "";
      action = null;
    } else if (state.sessions.length === 0) {
      title = "还没有扫描到会话";
      desc = "点右上角「扫描」，AgentSweep 会读取本机所有 Agent 的会话记录。";
      actionLabel = "开始扫描";
      action = () => {
        void startScan();
      };
    } else if (agent && agent.installed === false) {
      title = `${agent.name || agent.id} 未安装`;
      desc = "没有找到它的数据目录。安装并使用过后再扫描，这里就会出现会话。";
      actionLabel = "查看全部会话";
      action = () => selectAgent("all");
    } else if (query) {
      title = "没有匹配的会话";
      desc = `没有找到包含「${query}」的会话。`;
      actionLabel = "清除搜索";
      action = () => clearSearch({ focus: true });
    } else if (agent) {
      const count = state.sessions.filter((session) => session.agentId === agent.id).length;
      if (count === 0) {
        title = `${agent.name || agent.id} 还没有会话`;
        desc = "它已经安装，但目前没有可清理的会话记录。";
      }
      actionLabel = "查看全部会话";
      action = () => selectAgent("all");
    }

    dom.emptyTitle.textContent = title;
    dom.emptyDesc.textContent = desc;
    dom.emptyPrimary.textContent = actionLabel;
    dom.emptyPrimary.hidden = !actionLabel;
    dom.emptyPrimary.onclick = action || null;
  }

  function renderDetail() {
    const selectedSessions = getSelectedSessions();
    let session = state.focusUid ? state.sessionByUid.get(state.focusUid) : null;
    if (!session && selectedSessions.length > 0) session = selectedSessions[selectedSessions.length - 1];

    dom.detailBody.textContent = "";

    if (!session) {
      const empty = el("div", { class: "detail-empty" });
      const iconBox = el("div", { class: "empty-icon", "aria-hidden": "true" });
      iconBox.append(icon("panel"));
      empty.append(
        iconBox,
        el("h3", { text: "选择一条会话" }),
        el("p", { text: "这里会显示完整路径、体积、时间和这个 Agent 的清理说明。勾选多条可以批量清理。" })
      );
      dom.detailBody.append(empty);
      state.detailUid = null;
      return;
    }

    const agent = state.agentById.get(session.agentId);
    const fragment = document.createDocumentFragment();

    if (selectedSessions.length > 1) {
      const note = el("div", { class: "selection-note" });
      note.append(icon("layers"), el("span", { text: `已选 ${selectedSessions.length} 条，这里显示最后点选的一条。` }));
      fragment.append(note);
    }

    fragment.append(el("h3", { class: "detail-title", text: session.title || "未命名会话" }));

    const chips = el("div", { class: "chip-row" });
    if (session.active) chips.append(el("span", { class: "chip chip-warn", text: "可能正在使用" }));
    chips.append(el("span", { class: "chip chip-accent", text: agent ? agent.name || agent.id : session.agentId }));
    chips.append(
      el("span", {
        class: "chip",
        text: STORAGE_LABELS[session.storage] || session.storage || "文件",
      })
    );
    if (agent && agent.group) chips.append(el("span", { class: "chip", text: GROUP_LABELS[agent.group] || "未收录" }));
    fragment.append(chips);

    if (session.active) {
      const warning = el("div", { class: "callout callout-warn" });
      warning.append(
        icon("alert"),
        el("span", { text: "这条会话可能正在使用；清理前建议先退出对应的 Agent。" })
      );
      fragment.append(warning);
    }

    const stats = el("div", { class: "stat-grid" });
    stats.append(
      statCell("体积", formatBytes(session.bytes), false),
      statCell("消息数", formatMessageCount(session.messages), false),
      statCell("最后更新", formatDateTime(session.updatedAt), true)
    );
    fragment.append(stats);

    const metaList = el("div", { class: "meta-list" });
    const agentValue = el("div", { class: "meta-value" });
    agentValue.append(document.createTextNode(agent ? agent.name || agent.id : session.agentId));
    if (
      agent &&
      agent.vendor &&
      !String(agent.name || "").toLowerCase().includes(String(agent.vendor).toLowerCase())
    ) {
      agentValue.append(el("span", { class: "vendor", text: agent.vendor }));
    }
    metaList.append(metaRow("Agent", agentValue));
    metaList.append(metaRow("会话 ID", el("div", { class: "meta-value mono", text: session.id || "—" })));
    metaList.append(metaRow("项目", el("div", { class: "meta-value mono", text: session.project || "—" })));
    fragment.append(metaList);

    const pathsSection = el("section", { class: "detail-section" });
    pathsSection.append(el("h4", { class: "detail-section-title", text: "文件位置" }));
    const paths = Array.isArray(session.paths) ? session.paths.filter(Boolean) : [];
    if (paths.length > 0) {
      const list = el("div", { class: "path-list" });
      for (const path of paths) {
        const card = el("div", { class: "path-card" });
        card.append(el("div", { class: "path-text", text: path }));
        const reveal = el("button", { class: "link-btn", type: "button" });
        reveal.append(icon("folder"), el("span", { text: "在 Finder 中显示" }));
        reveal.addEventListener("click", () => {
          void revealPath(path, reveal);
        });
        card.append(reveal);
        list.append(card);
      }
      pathsSection.append(list);
    } else if (session.storage === "sqlite") {
      pathsSection.append(
        el("div", {
          class: "info-line",
          text: "会话存在 SQLite 中，删除时会先备份整库、删掉对应行，再 VACUUM。",
        })
      );
    } else {
      pathsSection.append(el("div", { class: "info-line", text: "没有可显示的文件路径。" }));
    }
    fragment.append(pathsSection);

    const noteSection = el("section", { class: "detail-section" });
    noteSection.append(el("h4", { class: "detail-section-title", text: "清理会碰到" }));
    const hasNote = Boolean(agent && agent.note);
    const noteCard = el("div", { class: "note-card" + (hasNote ? "" : " neutral") });
    const noteHead = el("div", { class: "note-card-head" });
    noteHead.append(icon(hasNote ? "alert" : "info"), el("span", { text: hasNote ? "Agent 说明" : "没有额外索引" }));
    noteCard.append(
      noteHead,
      el("div", {
        text: hasNote ? agent.note : "删除这个 Agent 的会话不需要额外裁剪索引，删除文件即可。",
      })
    );
    noteSection.append(noteCard);
    noteSection.append(
      el("p", {
        class: "detail-hint",
        text:
          state.prefs.deleteMode === "permanent"
            ? "当前设置为直接删除，不会进废纸篓，也无法撤销。"
            : "默认移入废纸篓，可撤销；索引会先备份到 Application Support/AgentSweep/backups/。",
      })
    );
    fragment.append(noteSection);

    dom.detailBody.append(fragment);

    if (state.detailUid !== session.uid) {
      dom.detailBody.scrollTop = 0;
      state.detailUid = session.uid;
    }
  }

  function statCell(label, value, wide) {
    const cell = el("div", { class: "stat" + (wide ? " stat-wide" : "") });
    cell.append(el("span", { class: "stat-label", text: label }), el("span", { class: "stat-value", text: value }));
    return cell;
  }

  function metaRow(label, valueNode) {
    const row = el("div", { class: "meta-row" });
    row.append(el("span", { class: "meta-label", text: label }), valueNode);
    return row;
  }

  function renderStatus() {
    const selected = getSelectedSessions();
    const bytes = sum(selected, "bytes");

    dom.selectionSummary.textContent = "";
    append(dom.selectionSummary, [
      document.createTextNode("已选 "),
      el("b", { text: String(selected.length) }),
      document.createTextNode(" 条 · 合计 "),
      el("b", { text: formatBytes(bytes) }),
    ]);

    dom.cleanBtn.disabled = selected.length === 0 || state.deleting;
    if (selected.length === 0) {
      dom.cleanBtn.title = "没有选中会话";
    } else if (state.prefs.deleteMode === "permanent") {
      dom.cleanBtn.title = `直接删除 ${selected.length} 条会话（不可撤销）`;
    } else {
      dom.cleanBtn.title = `将 ${selected.length} 条会话移入废纸篓`;
    }
  }

  function renderScanStatus() {
    const running = state.scan.status === "running";
    const scan = state.scan;
    dom.scanStatus.textContent = "";

    dom.scanBtn.disabled = running;
    dom.scanBtn.classList.toggle("is-busy", running);
    dom.scanBtnLabel.textContent = running ? "扫描中" : "扫描";
    dom.scanKbd.hidden = running;
    dom.scanBtn.title = running ? "正在扫描…" : "扫描本机所有 Agent 的会话（⌘R）";

    if (state.initialLoading && scan.status !== "running") {
      const wrap = el("div", { class: "scan-status-text" });
      wrap.append(el("span", { class: "scan-spinner", "aria-hidden": "true" }), el("span", { class: "label", text: "正在读取本机会话…" }));
      dom.scanStatus.append(wrap);
      return;
    }

    if (running) {
      const progress = scan.progress;
      const total = progress.total;
      const current = progress.current;
      const label = total > 0
        ? (current ? `${progress.done}/${total} 正在扫描 ${current}` : `${progress.done}/${total} 正在扫描…`)
        : "正在扫描…";

      const wrap = el("div", { class: "scan-status-text" });
      wrap.append(el("span", { class: "scan-spinner", "aria-hidden": "true" }), el("span", { class: "label", text: label }));

      if (total > 0) {
        const bar = el("div", {
          class: "scan-progress",
          role: "progressbar",
          "aria-valuemin": "0",
          "aria-valuemax": String(total),
          "aria-valuenow": String(progress.done),
          "aria-label": "扫描进度",
        });
        const fill = el("i");
        fill.style.width = `${Math.min(100, Math.max(0, (progress.done / total) * 100))}%`;
        bar.append(fill);
        wrap.append(bar);
      }
      dom.scanStatus.append(wrap);
      return;
    }

    const wrap = el("div", { class: "scan-status-text" });
    const issues = Array.isArray(scan.issues) ? scan.issues : [];

    if (scan.status === "error") {
      wrap.classList.add("scan-error");
      wrap.append(icon("alert"), el("span", { class: "label", text: "扫描失败" }));
      dom.scanStatus.title = issues[0] && issues[0].message ? issues[0].message : "上次扫描未能完成";
    } else if (scan.status === "done") {
      wrap.classList.add("scan-done");
      wrap.append(icon("check"));
      if (issues.length > 0) {
        wrap.append(
          el("span", {
            class: "label",
            text: `${state.sessions.length} 条会话 · ${issues.length} 个来源读取失败`,
          })
        );
        dom.scanStatus.title = issues
          .map((issue) => `${agentName(issue.agentId)}：${issue.message || "读取失败"}`)
          .join("\n");
      } else {
        wrap.append(el("span", { class: "label", text: `扫描完成 · ${state.sessions.length} 条会话` }));
        dom.scanStatus.title = scan.finishedAt ? `完成于 ${formatDateTime(scan.finishedAt)}` : "";
      }
    } else {
      wrap.append(icon("info"), el("span", { class: "label", text: "尚未扫描" }));
      dom.scanStatus.title = "";
    }
    dom.scanStatus.append(wrap);
  }

  function renderPrefs() {
    syncPrefsUi();
    renderStatus();
  }

  function syncPrefsUi() {
    document.querySelectorAll('input[name="deleteMode"]').forEach((input) => {
      input.checked = input.value === state.prefs.deleteMode;
    });
    dom.prefConfirm.checked = state.prefs.confirmBeforeDelete !== false;
    dom.prefSnapshots.checked = state.prefs.includeSnapshots !== false;
    dom.prefAutoScan.checked = state.prefs.autoScan !== false;
    dom.deleteModeHint.textContent =
      state.prefs.deleteMode === "permanent"
        ? "直接删除，不进废纸篓，无法撤销。"
        : "默认移入废纸篓，可撤销。";
    dom.app.classList.toggle("is-permanent", state.prefs.deleteMode === "permanent");
  }

  function renderBatches() {
    const batches = [...state.batches].sort((a, b) => parseDate(b.at) - parseDate(a.at));
    dom.batchNote.textContent = batches.length > 0 ? `${batches.length} 次` : "";
    dom.batchList.textContent = "";

    if (batches.length === 0) {
      dom.batchList.append(
        el("div", {
          class: "batch-empty",
          text: "还没有清理记录。清理完成后会出现在这里，可撤销的批次会保留「撤销」按钮。",
        })
      );
      return;
    }

    for (const batch of batches) {
      const item = el("div", { class: "batch-item" });
      const copy = el("div", { class: "batch-copy" });

      const title = el("div", { class: "batch-title" });
      title.append(
        document.createTextNode(`${Number(batch.count) || 0} 条会话`),
        el("span", { class: "batch-sep", text: "·" }),
        document.createTextNode(Number(batch.freed) > 0 ? `已释放 ${formatBytes(batch.freed)}` : "已移入废纸篓")
      );

      const names = (Array.isArray(batch.agents) ? batch.agents : []).map((id) => agentName(id));
      const sub = el("div", {
        class: "batch-sub",
        text: `${formatShortDate(batch.at)}${names.length ? ` · ${names.join("、")}` : ""}`,
      });

      copy.append(title, sub);
      item.append(copy);

      if (batch.undoable) {
        const undo = el("button", { class: "btn btn-small", type: "button", text: "撤销" });
        undo.addEventListener("click", () => {
          void undoBatch(batch.id, undo);
        });
        item.append(undo);
      } else {
        item.append(el("span", { class: "batch-state", text: "不可撤销" }));
      }

      dom.batchList.append(item);
    }
  }

  function renderDiscover() {
    dom.discoverList.textContent = "";
    const discover = state.discover;

    if (discover.loading && discover.candidates.length === 0) {
      const loading = el("div", { class: "discover-loading" });
      loading.append(el("div", { class: "skeleton-card" }), el("div", { class: "skeleton-card" }));
      dom.discoverList.append(loading);
      return;
    }

    if (discover.error && discover.candidates.length === 0) {
      const box = el("div", { class: "panel-empty" });
      const iconBox = el("div", { class: "empty-icon", "aria-hidden": "true" });
      iconBox.append(icon("alert"));
      box.append(
        iconBox,
        el("strong", { text: "无法读取发现结果" }),
        el("span", { text: discover.error }),
        retryButton(() => loadDiscover())
      );
      dom.discoverList.append(box);
      return;
    }

    if (discover.candidates.length === 0) {
      const box = el("div", { class: "panel-empty" });
      const iconBox = el("div", { class: "empty-icon", "aria-hidden": "true" });
      iconBox.append(icon("sparkle"));
      box.append(
        iconBox,
        el("strong", { text: "没有发现新的 Agent 目录" }),
        el("span", { text: "如果某个 Agent 刚安装，扫描一次后再来看看。" }),
        retryButton(() => loadDiscover())
      );
      dom.discoverList.append(box);
      return;
    }

    for (const candidate of discover.candidates) {
      dom.discoverList.append(buildCandidateCard(candidate));
    }
  }

  function retryButton(onClick) {
    const button = el("button", { class: "btn btn-small", type: "button", text: "重新检查" });
    button.addEventListener("click", onClick);
    return button;
  }

  function buildCandidateCard(candidate) {
    const spec = candidate.suggested && typeof candidate.suggested === "object" ? candidate.suggested : {};
    const confidence = Math.max(0, Math.min(1, Number(candidate.confidence) || 0));
    const card = el("article", { class: "candidate" });

    const head = el("div", { class: "candidate-head" });
    const nameWrap = el("div", { class: "candidate-name-wrap" });
    nameWrap.append(el("div", { class: "candidate-name", text: candidate.name || spec.name || "未知 Agent" }));
    if (spec.vendor) nameWrap.append(el("div", { class: "candidate-vendor", text: spec.vendor }));

    const confidenceEl = el("div", { class: "confidence", title: `置信度 ${Math.round(confidence * 100)}%` });
    const confidenceBar = el("span", { class: "confidence-bar" });
    const confidenceFill = el("i");
    confidenceFill.style.width = `${confidence * 100}%`;
    confidenceBar.append(confidenceFill);
    confidenceEl.append(confidenceBar, el("span", { text: `${Math.round(confidence * 100)}%` }));

    head.append(nameWrap, confidenceEl);

    const path = el("div", { class: "candidate-path" });
    path.append(icon("folder"), el("span", { text: candidate.path || "（没有路径）" }));

    const stats = el("div", {
      class: "candidate-stats",
      text: `${Number(candidate.sessionCount) || 0} 条会话 · ${formatBytes(candidate.bytes)}`,
    });

    const evidence = el("ul", { class: "evidence" });
    for (const item of Array.isArray(candidate.evidence) ? candidate.evidence : []) {
      evidence.append(el("li", { text: String(item) }));
    }

    const foot = el("div", { class: "candidate-foot" });
    foot.append(
      el("span", { class: "candidate-group", text: `建议分组：${GROUP_LABELS[spec.group] || "未收录"}` })
    );
    const adopt = el("button", { class: "btn btn-small btn-primary", type: "button", text: "收录为 Agent" });
    adopt.addEventListener("click", () => {
      void adoptCandidate(candidate, adopt);
    });
    foot.append(adopt);

    card.append(head, path, stats);
    if (evidence.childElementCount > 0) card.append(evidence);
    card.append(foot);
    return card;
  }

  function syncDiscoverCount() {
    const loaded = state.discover.loaded;
    const count = state.discover.candidates.length;
    dom.discoverCount.hidden = !loaded;
    dom.discoverLabel.hidden = !loaded;
    if (loaded) {
      dom.discoverCount.textContent = String(count);
      dom.discoverLabel.textContent = "个未收录 Agent";
      dom.discoverBtn.title = `${count} 个未收录 Agent`;
      dom.discoverBtn.setAttribute("aria-label", `发现 ${count} 个未收录 Agent`);
    } else {
      dom.discoverBtn.title = "查看发现的未收录 Agent";
      dom.discoverBtn.setAttribute("aria-label", "发现未收录 Agent");
    }
  }

  /* --------------------------------------------------------------- selection */

  function getSelectedSessions() {
    const out = [];
    for (const uid of state.selected) {
      const session = state.sessionByUid.get(uid);
      if (session) out.push(session);
    }
    return out;
  }

  function setSelected(uid, selected) {
    if (!state.sessionByUid.has(uid)) return;
    if (selected) {
      state.selected.add(uid);
      state.focusUid = uid;
    } else {
      state.selected.delete(uid);
      if (state.focusUid === uid) state.focusUid = null;
    }
    state.anchorUid = uid;
    validateFocus();
    syncSelectionUi();
  }

  function selectOnly(uid) {
    if (!state.sessionByUid.has(uid)) return;
    state.selected.clear();
    state.selected.add(uid);
    state.focusUid = uid;
    state.anchorUid = uid;
    syncSelectionUi();
  }

  function selectRange(uid) {
    const visible = getVisibleSessions();
    const targetIndex = visible.findIndex((session) => session.uid === uid);
    if (targetIndex < 0) return;
    let anchorIndex = visible.findIndex((session) => session.uid === state.anchorUid);
    if (anchorIndex < 0) anchorIndex = targetIndex;

    const start = Math.min(anchorIndex, targetIndex);
    const end = Math.max(anchorIndex, targetIndex);
    state.selected.clear();
    for (let index = start; index <= end; index += 1) state.selected.add(visible[index].uid);
    state.focusUid = uid;
    syncSelectionUi();
  }

  function validateFocus() {
    if (state.focusUid && !state.selected.has(state.focusUid)) state.focusUid = null;
    if (!state.focusUid && state.selected.size > 0) {
      const uids = [...state.selected];
      state.focusUid = uids[uids.length - 1];
    }
  }

  function syncSelectionUi() {
    const rows = dom.sessionBody.querySelectorAll("tr[data-uid]");
    for (const row of rows) {
      const uid = row.dataset.uid;
      const selected = state.selected.has(uid);
      row.classList.toggle("is-selected", selected);
      const checkbox = row.querySelector('input[type="checkbox"]');
      if (checkbox) checkbox.checked = selected;
    }
    validateFocus();
    updateSelectAll(getVisibleSessions());
    renderDetail();
    renderStatus();
  }

  function clearSelection() {
    state.selected.clear();
    state.focusUid = null;
    state.anchorUid = null;
    syncSelectionUi();
  }

  /* ---------------------------------------------------------------- filters */

  function clearSearch(options) {
    const opts = options || {};
    state.search = "";
    dom.searchInput.value = "";
    syncSearchUi();
    renderSessions();
    if (opts.focus) dom.searchInput.focus();
  }

  function syncSearchUi() {
    dom.searchWrap.classList.toggle("has-value", dom.searchInput.value.length > 0);
    dom.searchClear.hidden = dom.searchInput.value.length === 0;
  }

  function resetFilters() {
    state.search = "";
    state.agentFilter = "all";
    dom.searchInput.value = "";
    syncSearchUi();
    renderAgents();
    renderSessions();
  }

  /* -------------------------------------------------------------- prefs/drawers */

  async function updatePref(patch) {
    const previous = { ...state.prefs };
    Object.assign(state.prefs, patch);
    syncPrefsUi();
    renderDetail();
    renderStatus();

    try {
      const result = await api("PUT", "/api/prefs", patch);
      if (result && result.prefs && typeof result.prefs === "object") {
        state.prefs = { ...state.prefs, ...result.prefs };
      }
      syncPrefsUi();
      renderDetail();
      renderStatus();
    } catch (err) {
      state.prefs = previous;
      syncPrefsUi();
      renderDetail();
      renderStatus();
      showToast("设置没有保存", { detail: err.message || "请稍后再试", tone: "error", icon: "alert" });
    }
  }

  function openSettings() {
    syncPrefsUi();
    renderBatches();
    openSheet("settings");
  }

  function openDiscover() {
    openSheet("discover");
    renderDiscover();
    void loadDiscover();
  }

  function openSheet(name) {
    if (state.sheet === name) return;
    if (!state.sheet) state.sheetReturnFocus = document.activeElement;
    state.sheet = name;
    dom.sheetLayer.hidden = false;
    dom.settingsSheet.hidden = name !== "settings";
    dom.discoverSheet.hidden = name !== "discover";

    const sheet = name === "settings" ? dom.settingsSheet : dom.discoverSheet;
    const focusTarget = state.lastInput === "keyboard" ? sheet.querySelector(".sheet-close") : sheet;
    if (focusTarget) focusTarget.focus({ preventScroll: true });
  }

  function closeSheet() {
    if (!state.sheet) return;
    state.sheet = null;
    dom.sheetLayer.hidden = true;
    dom.settingsSheet.hidden = true;
    dom.discoverSheet.hidden = true;
    const previous = state.sheetReturnFocus;
    state.sheetReturnFocus = null;
    if (previous && previous instanceof HTMLElement && document.contains(previous) && !previous.disabled) {
      previous.focus();
    }
  }

  async function loadDiscover(options) {
    const opts = options || {};
    if (state.discover.loading) return;
    state.discover.loading = true;
    state.discover.error = "";
    if (state.sheet === "discover") renderDiscover();

    try {
      const result = await api("GET", "/api/discover");
      state.discover.candidates = Array.isArray(result && result.candidates) ? result.candidates : [];
      state.discover.loaded = true;
    } catch (err) {
      state.discover.error = err.message || "无法读取发现结果";
      if (!opts.quiet) console.warn("AgentSweep: discover failed", err);
    } finally {
      state.discover.loading = false;
      syncDiscoverCount();
      if (state.sheet === "discover") renderDiscover();
    }
  }

  function specFromCandidate(candidate) {
    const name = String(candidate.name || "未命名 Agent");
    let id = name
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "");
    if (!id) id = `custom-${Date.now()}`;
    return {
      id,
      name,
      group: "other",
      vendor: "",
      bundles: [],
      roots: candidate.path ? [candidate.path.replace(/^\/Users\/[^/]+/, "~")] : [],
      env: [],
      stores: [{ kind: "dir", glob: "*" }],
      indexes: [],
      note: "由「发现」收录。",
    };
  }

  async function adoptCandidate(candidate, button) {
    const spec = candidate.suggested && typeof candidate.suggested === "object"
      ? candidate.suggested
      : specFromCandidate(candidate);
    button.disabled = true;
    button.textContent = "正在收录…";

    try {
      await api("POST", "/api/adapters", spec);
      state.discover.candidates = state.discover.candidates.filter((item) => item !== candidate);
      syncDiscoverCount();
      renderDiscover();
      await refreshBootstrap({ quiet: true });
      showToast(`已收录 ${candidate.name || spec.name || "Agent"}`, {
        icon: "sparkle",
        tone: "success",
        detail: "现在可以在左侧列表里看到它。",
      });
    } catch (err) {
      button.disabled = false;
      button.textContent = "收录为 Agent";
      showToast("收录失败", { detail: err.message || "请稍后再试", tone: "error", icon: "alert" });
    }
  }

  /* -------------------------------------------------------------------- scan */

  function maybeAutoScan() {
    if (state.autoScanChecked) return;
    state.autoScanChecked = true;
    if (!state.data) return;
    if (state.prefs.autoScan === false) return;
    if (state.scan.status !== "idle") return;
    void startScan();
  }

  // Keep the header, empty state and sidebar in sync while a scan runs without
  // touching the session rows — old data must stay on screen.
  function refreshScanSurface() {
    if (state.sessions.length === 0) {
      const visible = getVisibleSessions();
      renderListHead(visible);
      renderEmptyState(visible);
    }
  }

  function ensureScanPolling(delay) {
    if (state.pollTimer !== null) return;
    if (!state.scan || state.scan.status !== "running") return;
    state.pollTimer = window.setTimeout(pollScan, typeof delay === "number" ? delay : 250);
  }

  function stopScanPolling() {
    if (state.pollTimer !== null) {
      window.clearTimeout(state.pollTimer);
      state.pollTimer = null;
    }
  }

  async function startScan() {
    if (state.scan.status === "running") return;
    try {
      await api("POST", "/api/scan");
    } catch (err) {
      showToast("无法开始扫描", { detail: err.message || "请稍后再试", tone: "error", icon: "alert" });
      return;
    }

    state.scan = {
      ...state.scan,
      status: "running",
      startedAt: new Date().toISOString(),
      finishedAt: "",
      durationMs: 0,
      progress: { done: 0, total: 0, current: "" },
      issues: [],
    };
    if (state.agents.length === 0) renderAgents();
    renderScanStatus();
    refreshScanSurface();
    ensureScanPolling(250);
  }

  async function pollScan() {
    state.pollTimer = null;
    if (!state.scan || state.scan.status !== "running") return;

    try {
      const scan = await api("GET", "/api/scan");
      state.scan = normalizeScan(scan);
      renderScanStatus();

      if (state.scan.status === "running") {
        refreshScanSurface();
        ensureScanPolling(250);
      } else {
        if (state.scan.status === "error") {
          if (state.agents.length === 0) renderAgents();
          refreshScanSurface();
        }
        // Leave the scanning placeholder (or the existing rows) on screen while
        // the fresh bootstrap is in flight, so there is no empty-state flicker.
        await refreshBootstrap({ quiet: true });
        if (state.sessions.length === 0) refreshScanSurface();
      }
    } catch (err) {
      state.scan = { ...state.scan, status: "error" };
      if (state.agents.length === 0) renderAgents();
      renderScanStatus();
      refreshScanSurface();
      showToast("扫描进度获取失败", { detail: err.message || "请重新扫描", tone: "error", icon: "alert" });
    }
  }

  /* ------------------------------------------------------------------ delete */

  function requestDelete() {
    const selected = getSelectedSessions();
    if (selected.length === 0) return;
    const uids = selected.map((session) => session.uid);

    if (state.prefs.confirmBeforeDelete === false) {
      void performDelete(uids, state.prefs.deleteMode);
      return;
    }
    openConfirm(uids);
  }

  function openConfirm(uids) {
    const sessions = uids.map((uid) => state.sessionByUid.get(uid)).filter(Boolean);
    if (sessions.length === 0) return;

    const bytes = sum(sessions, "bytes");
    const counts = new Map();
    for (const session of sessions) {
      counts.set(session.agentId, (counts.get(session.agentId) || 0) + 1);
    }
    const activeCount = sessions.filter((session) => session.active).length;
    const mode = state.prefs.deleteMode === "permanent" ? "permanent" : "trash";

    state.modalReturnFocus = document.activeElement;
    state.pendingDeleteUids = uids.slice();

    dom.confirmTitle.textContent = `清理 ${sessions.length} 条会话？`;
    dom.confirmLede.textContent = "";
    append(dom.confirmLede, [
      document.createTextNode("合计 "),
      el("b", { text: formatBytes(bytes) }),
      document.createTextNode(` · 涉及 ${counts.size} 个 Agent`),
    ]);

    dom.confirmAgents.textContent = "";
    const ordered = [...counts.entries()].sort((a, b) => b[1] - a[1]);
    for (const [agentId, count] of ordered) {
      dom.confirmAgents.append(
        el("span", {
          class: "agent-chip",
          text: `${agentName(agentId)}${count > 1 ? ` × ${count}` : ""}`,
        })
      );
    }

    dom.confirmActive.hidden = activeCount === 0;
    if (activeCount > 0) {
      dom.confirmActive.querySelector("span").textContent =
        `有 ${activeCount} 条会话可能正在使用。清理前建议先退出对应的 Agent。`;
    }

    dom.confirmMode.classList.toggle("callout-warn", mode === "permanent");
    dom.confirmModeText.textContent =
      mode === "permanent"
        ? "直接删除，不进废纸篓，无法撤销。"
        : "默认移入废纸篓，可撤销。索引文件会先备份到 ~/Library/Application Support/AgentSweep/backups/。";

    dom.confirmError.hidden = true;
    dom.confirmError.textContent = "";
    dom.confirmDelete.textContent = confirmButtonLabel(mode);
    dom.confirmDelete.classList.add("btn-danger");
    dom.confirmCancel.disabled = false;
    dom.confirmDelete.disabled = false;

    dom.confirmModal.hidden = false;
    const focusTarget = state.lastInput === "keyboard" ? dom.confirmCancel : dom.confirmDialog;
    focusTarget.focus({ preventScroll: true });
  }

  function confirmButtonLabel(mode) {
    return mode === "permanent" ? "永久删除" : "移入废纸篓";
  }

  function closeConfirm(restoreFocus) {
    if (dom.confirmModal.hidden) return;
    dom.confirmModal.hidden = true;
    state.pendingDeleteUids = null;
    const previous = state.modalReturnFocus;
    state.modalReturnFocus = null;
    if (restoreFocus !== false && previous instanceof HTMLElement && document.contains(previous) && !previous.disabled) {
      previous.focus();
    }
  }

  async function submitPendingDelete() {
    if (state.deleting) return;
    const uids = state.pendingDeleteUids ? state.pendingDeleteUids.slice() : [];
    if (uids.length === 0) {
      closeConfirm();
      return;
    }
    await performDelete(uids, state.prefs.deleteMode);
  }

  async function performDelete(uids, mode) {
    if (state.deleting || uids.length === 0) return;
    state.deleting = true;
    setConfirmBusy(true);

    try {
      const result = await api("POST", "/api/delete", { uids });
      for (const uid of uids) state.selected.delete(uid);
      if (state.focusUid && uids.includes(state.focusUid)) state.focusUid = null;
      validateFocus();
      closeConfirm(false);
      state.deleting = false;
      setConfirmBusy(false);
      await refreshBootstrap({ quiet: true });
      renderStatus();
      showDeleteResult(mode, result || {}, uids.length);
    } catch (err) {
      state.deleting = false;
      setConfirmBusy(false);
      if (!dom.confirmModal.hidden) {
        dom.confirmError.hidden = false;
        dom.confirmError.textContent = err.message || "清理失败，请稍后再试。";
      } else {
        showToast("清理失败", { detail: err.message || "请稍后再试", tone: "error", icon: "alert" });
      }
      renderStatus();
    }
  }

  function setConfirmBusy(busy) {
    dom.confirmCancel.disabled = busy;
    dom.confirmDelete.disabled = busy;
    if (busy) {
      dom.confirmDelete.textContent = "正在清理…";
    } else {
      dom.confirmDelete.textContent = confirmButtonLabel(state.prefs.deleteMode);
    }
  }

  function showDeleteResult(mode, result, requestedCount) {
    const errors = Array.isArray(result.errors) ? result.errors : [];
    const count = Number.isFinite(Number(result.count)) ? Number(result.count) : requestedCount;
    const freed = Number(result.freed) || 0;
    const errorText = errors.length > 0
      ? `${errors.length} 条未能清理${errors[0] && errors[0].message ? `：${errors[0].message}` : ""}`
      : "";

    if (mode === "permanent") {
      showToast(`已释放 ${formatBytes(freed)}`, {
        icon: "check",
        tone: "success",
        detail: errorText || `${count} 条会话已直接删除`,
        duration: 7000,
      });
      return;
    }

    showToast(`已移入废纸篓 · ${count} 条`, {
      icon: "trash",
      detail: errorText || "可撤销，索引已先备份。",
      actionLabel: result.batchId ? "撤销" : null,
      duration: result.batchId ? 12000 : 7000,
      onAction: result.batchId
        ? async () => {
            const ok = await undoBatch(result.batchId);
            return ok ? undefined : false;
          }
        : null,
    });
  }

  async function undoBatch(batchId, button) {
    if (!batchId) return false;
    if (button) {
      button.disabled = true;
      button.textContent = "撤销中…";
    }

    try {
      const result = await api("POST", "/api/undo", { batchId });
      const restored = Number(result && result.restored);
      await refreshBootstrap({ quiet: true });
      if (state.sheet === "settings") renderBatches();
      renderStatus();
      showToast(`已恢复 ${Number.isFinite(restored) ? restored : 0} 条`, {
        icon: "undo",
        tone: "success",
        detail: "会话已回到列表。",
      });
      return true;
    } catch (err) {
      showToast("撤销失败", { detail: err.message || "这个批次可能已经不可撤销", tone: "error", icon: "alert" });
      return false;
    } finally {
      if (button) {
        button.disabled = false;
        button.textContent = "撤销";
      }
    }
  }

  async function revealPath(path, button) {
    if (state.mock) {
      showToast("演示数据没有对应的真实文件", { detail: path, icon: "info" });
      return;
    }

    if (button) button.disabled = true;
    try {
      await api("POST", "/api/reveal", { path });
    } catch (err) {
      showToast("无法在 Finder 中显示", { detail: err.message || path, tone: "error", icon: "alert" });
    } finally {
      if (button) button.disabled = false;
    }
  }

  /* ------------------------------------------------------------------ toasts */

  function showToast(text, options) {
    const opts = options || {};
    const toast = el("div", {
      class: `toast${opts.tone === "error" ? " is-error" : opts.tone === "success" ? " is-success" : ""}`,
    });

    const iconName = opts.icon || (opts.tone === "error" ? "alert" : "info");
    toast.append(icon(iconName, "toast-icon"));

    const copy = el("div", { class: "toast-copy" });
    copy.append(el("div", { class: "toast-text", text }));
    if (opts.detail) copy.append(el("div", { class: "toast-sub", text: opts.detail }));
    toast.append(copy);

    let timer = null;
    const dismiss = () => {
      if (timer !== null) window.clearTimeout(timer);
      toast.remove();
    };

    if (opts.actionLabel) {
      const action = el("button", { class: "toast-action", type: "button", text: opts.actionLabel });
      action.addEventListener("click", async () => {
        if (action.disabled) return;
        action.disabled = true;
        const label = action.textContent;
        action.textContent = "处理中…";
        let keep = false;
        try {
          if (typeof opts.onAction === "function") {
            const result = await opts.onAction();
            keep = result === false;
          }
        } catch (err) {
          keep = true;
        }
        if (!keep) {
          dismiss();
        } else {
          action.disabled = false;
          action.textContent = label;
        }
      });
      toast.append(action);
    }

    const close = el("button", { class: "toast-close", type: "button", "aria-label": "关闭提示" });
    close.append(icon("x"));
    close.addEventListener("click", dismiss);
    toast.append(close);

    dom.toasts.append(toast);
    while (dom.toasts.childElementCount > 3) {
      dom.toasts.firstElementChild.remove();
    }

    timer = window.setTimeout(dismiss, Number(opts.duration) || 6000);
    return { element: toast, dismiss };
  }

  /* ----------------------------------------------------------------- keyboard */

  function onKeydown(event) {
    const key = event.key;
    const meta = event.metaKey || event.ctrlKey;
    const typing = isTypingTarget(event.target);

    if (key === "Escape") {
      if (!dom.confirmModal.hidden) {
        event.preventDefault();
        if (!state.deleting) closeConfirm();
        return;
      }
      if (state.sheet) {
        event.preventDefault();
        closeSheet();
        return;
      }
      if (state.search) {
        event.preventDefault();
        clearSearch({ focus: true });
        return;
      }
      if (state.selected.size > 0) {
        event.preventDefault();
        clearSelection();
        return;
      }
      if (typing && event.target instanceof HTMLElement) event.target.blur();
      return;
    }

    if (meta && key.toLowerCase() === "r") {
      event.preventDefault();
      if (!isOverlayOpen()) void startScan();
      return;
    }

    if (meta && key.toLowerCase() === "f") {
      event.preventDefault();
      if (!isOverlayOpen()) {
        dom.searchInput.focus();
        dom.searchInput.select();
      }
      return;
    }

    if (meta && key.toLowerCase() === "a") {
      if (typing || isOverlayOpen()) return;
      event.preventDefault();
      const visible = getVisibleSessions();
      for (const session of visible) state.selected.add(session.uid);
      if (visible.length > 0 && (!state.focusUid || !state.selected.has(state.focusUid))) {
        state.focusUid = visible[0].uid;
      }
      state.anchorUid = visible.length > 0 ? visible[0].uid : null;
      syncSelectionUi();
    }
  }

  /* ------------------------------------------------------------------- detail */

  function setDetailOpen(open) {
    state.detailOpen = Boolean(open);
    state.userToggledDetail = true;
    syncDetailVisibility();
  }

  function syncDetailVisibility() {
    dom.app.classList.toggle("detail-hidden", !state.detailOpen);
    dom.detailToggle.classList.toggle("is-active", state.detailOpen);
    dom.detailToggle.setAttribute("aria-pressed", state.detailOpen ? "true" : "false");
    dom.detailToggle.setAttribute("aria-label", state.detailOpen ? "隐藏详情面板" : "显示详情面板");
    dom.detailToggle.title = state.detailOpen ? "隐藏详情面板" : "显示详情面板";
  }

  /* --------------------------------------------------------------------- init */

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => {
      void boot();
    });
  } else {
    void boot();
  }
})();
