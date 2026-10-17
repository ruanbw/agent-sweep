/* AgentSweep — state, rendering and interaction layer.
   Rendering and interaction semantics come from docs/design/prototype.
   Data comes from the local HTTP API documented in docs/api.md.
   Vanilla JS only: no libraries, no build step, no framework, no bindings. */
(function () {
  'use strict';

  var BRAND = (window.AGENT_BRAND && typeof window.AGENT_BRAND === 'object')
    ? window.AGENT_BRAND
    : { icons: {}, iconKeys: {} };
  var doc = document;
  var DAY = 86400000;
  var SPRITE_LINK = 'href';
  var POLL_INTERVAL = 250;
  var LS_APPEARANCE = 'agentsweep.appearance';
  var LS_DENSITY = 'agentsweep.density';
  var APPEARANCE_ORDER = ['auto', 'light', 'dark'];
  var APPEARANCE_LABELS = { auto: '自动', light: '浅色', dark: '深色' };

  /* =========================================================================
     0. Small DOM / data helpers
     ========================================================================= */

  function byId(id) {
    return doc.getElementById(id);
  }

  function $(selector, root) {
    return (root || doc).querySelector(selector);
  }

  function $$(selector, root) {
    return Array.prototype.slice.call((root || doc).querySelectorAll(selector));
  }

  function h(tag, attrs) {
    var node = doc.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (key) {
        var value = attrs[key];
        if (value === null || value === undefined || value === false) return;
        if (key === 'class') node.className = value;
        else if (key === 'text') node.textContent = value;
        else if (key === 'dataset') {
          Object.keys(value).forEach(function (k) { node.dataset[k] = value[k]; });
        } else if (key === 'style') node.setAttribute('style', value);
        else if (key === 'on') {
          Object.keys(value).forEach(function (type) {
            if (typeof value[type] === 'function') node.addEventListener(type, value[type]);
          });
        } else node.setAttribute(key, value === true ? '' : String(value));
      });
    }
    for (var i = 2; i < arguments.length; i++) {
      append(node, arguments[i]);
    }
    return node;
  }

  function append(parent, child) {
    if (child === null || child === undefined || child === false) return;
    if (Array.isArray(child)) {
      child.forEach(function (item) { append(parent, item); });
      return;
    }
    parent.appendChild(child.nodeType ? child : doc.createTextNode(String(child)));
  }

  function clear(node) {
    if (node) node.replaceChildren();
  }

  function setText(node, text) {
    if (node) node.textContent = text === null || text === undefined ? '' : String(text);
  }

  function setHidden(node, hidden) {
    if (node) node.hidden = !!hidden;
  }

  function setClass(node, name, on) {
    if (node) node.classList.toggle(name, !!on);
  }

  function sprite(id, className) {
    var svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', className || 'icon');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var use = doc.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute(SPRITE_LINK, '#' + id);
    svg.appendChild(use);
    return svg;
  }

  function clone(value) {
    if (value === null || value === undefined) return value;
    if (typeof structuredClone === 'function') {
      try { return structuredClone(value); } catch (err) { /* fall through */ }
    }
    return JSON.parse(JSON.stringify(value));
  }

  function safe(fn, label) {
    return function () {
      try {
        return fn.apply(this, arguments);
      } catch (err) {
        reportError(label, err);
        return undefined;
      }
    };
  }

  function reportError(label, err) {
    var detail = err && err.message ? String(err.message) : '未知错误';
    try {
      toast((label ? label + '没有完成：' : '操作没有完成：') + detail, { tone: 'error' });
    } catch (ignored) { /* never throw from an error path */ }
  }

  function bind(node, type, handler, options) {
    if (!node) return;
    node.addEventListener(type, safe(handler), options);
  }

  function isTypingTarget(target) {
    if (!target || target.nodeType !== 1) return false;
    var tag = target.tagName;
    if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
    if (tag === 'INPUT') {
      // 只有文本类输入才吞掉全局快捷键。复选框 / 单选 / 按钮获得焦点时，
      // ⌘A、⌘K、? 仍然应当是列表级快捷键（否则点过一次复选框就失去键盘能力）。
      var type = String(target.getAttribute('type') || 'text').toLowerCase();
      return ['text', 'search', 'email', 'password', 'url', 'tel', 'number',
        'date', 'datetime-local', 'month', 'time', 'week'].indexOf(type) !== -1;
    }
    if (target.isContentEditable) return true;
    var role = target.getAttribute('role');
    return role === 'textbox' || role === 'searchbox' || role === 'combobox';
  }

  function isButtonLike(target) {
    if (!target || target.nodeType !== 1) return false;
    return !!target.closest('button,a,input,select,textarea,label,[role="button"],[contenteditable="true"]');
  }

  function pad2(value) {
    return (value < 10 ? '0' : '') + value;
  }

  function trimFixed(value) {
    return String(Math.round(value * 10) / 10).replace(/\.0$/, '');
  }

  function prefersReducedMotion() {
    if (!window.matchMedia) return false;
    try { return window.matchMedia('(prefers-reduced-motion: reduce)').matches; } catch (err) { return false; }
  }

  /* =========================================================================
     1. Formatting (CONTRACT §6: formatting lives in app.js, not CSS)
     ========================================================================= */

  function formatBytes(bytes) {
    var n = Number(bytes);
    if (!isFinite(n) || n <= 0) return '0 B';
    if (n < 1024) return Math.round(n) + ' B';
    var kb = n / 1024;
    if (kb < 1024) return (kb >= 100 ? String(Math.round(kb)) : trimFixed(kb)) + ' KB';
    var mb = kb / 1024;
    if (mb < 1024) return (mb >= 100 ? String(Math.round(mb)) : trimFixed(mb)) + ' MB';
    return trimFixed(mb / 1024) + ' GB';
  }

  var WEEKDAYS = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];

  function sameLocalDay(a, b) {
    return a.getFullYear() === b.getFullYear() &&
      a.getMonth() === b.getMonth() &&
      a.getDate() === b.getDate();
  }

  function relativeTime(iso) {
    var date = new Date(iso);
    if (isNaN(date.getTime())) return '—';
    var now = new Date();
    var yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
    var hm = pad2(date.getHours()) + ':' + pad2(date.getMinutes());
    if (sameLocalDay(date, now)) return '今天 ' + hm;
    if (sameLocalDay(date, yesterday)) return '昨天 ' + hm;
    var diff = now.getTime() - date.getTime();
    if (diff >= 0 && diff < 7 * DAY) return WEEKDAYS[date.getDay()] + ' ' + hm;
    var md = (date.getMonth() + 1) + '月' + date.getDate() + '日';
    return date.getFullYear() === now.getFullYear() ? md : date.getFullYear() + '年' + md;
  }

  function fullDate(iso) {
    var date = new Date(iso);
    if (isNaN(date.getTime())) return '—';
    return date.getFullYear() + '年' + (date.getMonth() + 1) + '月' + date.getDate() + '日 ' +
      pad2(date.getHours()) + ':' + pad2(date.getMinutes());
  }

  function monthDayTime(iso) {
    var date = new Date(iso);
    if (isNaN(date.getTime())) return '—';
    return (date.getMonth() + 1) + '月' + date.getDate() + '日 ' +
      pad2(date.getHours()) + ':' + pad2(date.getMinutes());
  }

  function homePrefix() {
    return state.home ? String(state.home).replace(/\/+$/, '') : '';
  }

  function baseName(path) {
    var value = path === null || path === undefined ? '' : String(path).replace(/\/+$/, '');
    if (!value) return '';
    var parts = value.split('/');
    return parts[parts.length - 1] || value;
  }

  function abbrevPath(path) {
    var value = path === null || path === undefined ? '' : String(path);
    if (!value) return '—';
    if (value === '~' || value.indexOf('~/') === 0) return value;
    var home = homePrefix();
    if (home && (value === home || value.indexOf(home + '/') === 0)) {
      return '~' + value.slice(home.length);
    }
    return value;
  }

  function hexToRgba(hex, alpha) {
    var value = String(hex || '').replace('#', '');
    if (value.length === 3) {
      value = value.charAt(0) + value.charAt(0) + value.charAt(1) + value.charAt(1) + value.charAt(2) + value.charAt(2);
    }
    if (!/^[0-9a-fA-F]{6}$/.test(value)) return 'rgba(63,101,153,' + alpha + ')';
    var r = parseInt(value.slice(0, 2), 16);
    var g = parseInt(value.slice(2, 4), 16);
    var b = parseInt(value.slice(4, 6), 16);
    return 'rgba(' + r + ',' + g + ',' + b + ',' + alpha + ')';
  }

  function isDarkTheme() {
    var forced = doc.documentElement.dataset.theme;
    if (forced === 'dark') return true;
    if (forced === 'light') return false;
    if (!window.matchMedia) return false;
    try { return window.matchMedia('(prefers-color-scheme: dark)').matches; } catch (err) { return false; }
  }

  /* =========================================================================
     2. State model (CONTRACT §3)
     ========================================================================= */

  var GROUPS = [
    { key: 'cli', label: '命令行' },
    { key: 'ide', label: 'IDE' },
    { key: 'app', label: '桌面应用' },
    { key: 'extension', label: '编辑器扩展' },
    { key: 'other', label: '其他' }
  ];

  var GROUP_LABELS = {
    cli: '命令行', ide: 'IDE', app: '桌面应用', extension: '编辑器扩展', other: '其他'
  };

  var STORAGE_LABELS = { file: '文件', dir: '目录', sqlite: 'SQLite 行' };
  var STORAGE_ICONS = { file: 'i-file', dir: 'i-folder', sqlite: 'i-database' };

  var DEFAULT_PREFS = {
    deleteMode: 'trash',
    confirmBeforeDelete: true,
    includeSnapshots: true,
    autoScan: true,
    autoCheckUpdates: true,
    appearance: 'auto'
  };

  var DEFAULT_FILTER = {
    agentId: 'all',
    query: '',
    storage: 'all',
    age: 'all',
    size: 'all'
  };

  var DEFAULT_SORT = { key: 'updatedAt', dir: 'desc' };

  var state = {
    /* server state (GET /api/bootstrap) */
    version: '',
    home: '',
    platform: '',
    sessions: [],
    agents: [],
    candidates: [],
    batches: [],
    prefs: clone(DEFAULT_PREFS),
    scan: {
      status: 'idle',
      progress: { done: 0, total: 0, current: '' },
      issues: [],
      durationMs: 0
    },
    /* local UI state */
    filter: clone(DEFAULT_FILTER),
    sort: clone(DEFAULT_SORT),
    selected: new Set(),
    focusUid: null,
    focusSuppressed: false,
    anchorUid: null,
    detailOpen: true,
    density: 'comfortable',
    sheet: null,
    dialog: null,
    menu: null,
    menuUid: null,
    menuReturnFocus: null,
    popover: null,
    deleting: false,
    undoBusy: false,
    scanStarting: false,
    autoScanStarted: false,
    prefsSeq: 0,
    overlay: null,
    palette: { query: '', index: 0, entries: [] },
    discover: { loaded: false, loading: false, error: '' },
    issueExpanded: false,
    reviewAck: false,
    reviewError: '',
    iconMisses: {},
    iconPending: {},
    adoptBusy: {},
    highlightAgentId: null,
    lastDelete: { ids: [], count: 0, freed: 0, at: 0 },
    update: {
      data: null,
      loading: false,
      checking: false,
      applying: false,
      notesKey: '',
      applyStartedAt: 0
    },
    boot: {
      status: 'loading',   // loading | ready | error
      error: ''
    }
  };

  var overlayTrigger = null;
  var timers = {
    scanPoll: null,
    highlight: null,
    updatePoll: null
  };
  var toastSeq = 0;

  /* ---------- derived values (CONTRACT §3) ---------- */

  function agentById(agentId) {
    for (var i = 0; i < state.agents.length; i++) {
      if (state.agents[i].id === agentId) return state.agents[i];
    }
    return null;
  }

  function sessionByUid(uid) {
    for (var i = 0; i < state.sessions.length; i++) {
      if (state.sessions[i].uid === uid) return state.sessions[i];
    }
    return null;
  }

  function agentName(agentId) {
    var agent = agentById(agentId);
    return agent ? agent.name : (agentId || '未知 Agent');
  }

  function agentStats(agentId) {
    var count = 0;
    var bytes = 0;
    state.sessions.forEach(function (session) {
      if (session.agentId === agentId) {
        count += 1;
        bytes += Number(session.bytes) || 0;
      }
    });
    return { count: count, bytes: bytes };
  }

  function totalStats() {
    var bytes = 0;
    state.sessions.forEach(function (session) { bytes += Number(session.bytes) || 0; });
    return { count: state.sessions.length, bytes: bytes };
  }

  function searchableText(session) {
    var agent = agentById(session.agentId);
    return [
      session.title || '',
      session.project || '',
      session.snippet || '',
      session.id || '',
      session.uid || '',
      agent ? agent.name : ''
    ].join('\n').toLowerCase();
  }

  function startOfToday() {
    var now = new Date();
    return new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  }

  function sessionAgeMs(session) {
    var time = new Date(session.updatedAt).getTime();
    if (isNaN(time)) return Infinity;
    return Date.now() - time;
  }

  function matchesAge(session, age) {
    if (!age || age === 'all') return true;
    var time = new Date(session.updatedAt).getTime();
    if (isNaN(time)) return false;
    if (age === 'today') return time >= startOfToday();
    if (age === '7d') return time >= Date.now() - 7 * DAY;
    if (age === '30d') return time >= Date.now() - 30 * DAY;
    if (age === 'older') return time < Date.now() - 30 * DAY;
    return true;
  }

  function matchesSize(session, size) {
    var bytes = Number(session.bytes) || 0;
    if (!size || size === 'all') return true;
    if (size === 'small') return bytes < 10 * 1024;
    if (size === 'medium') return bytes >= 10 * 1024 && bytes < 1024 * 1024;
    if (size === 'large') return bytes >= 1024 * 1024;
    return true;
  }

  function sortSessions(list) {
    var key = state.sort.key;
    var sign = state.sort.dir === 'asc' ? 1 : -1;
    list.sort(function (a, b) {
      var result = 0;
      if (key === 'title') {
        result = String(a.title || '').localeCompare(String(b.title || ''), 'zh-Hans');
      } else if (key === 'bytes') {
        result = (Number(a.bytes) || 0) - (Number(b.bytes) || 0);
      } else if (key === 'messages') {
        result = (Number(a.messages) || 0) - (Number(b.messages) || 0);
      } else {
        result = new Date(a.updatedAt).getTime() - new Date(b.updatedAt).getTime();
      }
      if (result === 0) result = String(a.uid).localeCompare(String(b.uid));
      return result * sign;
    });
    return list;
  }

  function visibleSessions() {
    var filter = state.filter;
    var query = String(filter.query || '').trim().toLowerCase();
    var list = state.sessions.filter(function (session) {
      if (filter.agentId !== 'all' && session.agentId !== filter.agentId) return false;
      if (filter.storage !== 'all' && session.storage !== filter.storage) return false;
      if (!matchesAge(session, filter.age)) return false;
      if (!matchesSize(session, filter.size)) return false;
      if (query && searchableText(session).indexOf(query) === -1) return false;
      return true;
    });
    return sortSessions(list);
  }

  function selectedSessions() {
    return state.sessions.filter(function (session) {
      return state.selected.has(session.uid);
    });
  }

  function selectedBytes() {
    var bytes = 0;
    selectedSessions().forEach(function (session) { bytes += Number(session.bytes) || 0; });
    return bytes;
  }

  function trashBytes() {
    return state.prefs.deleteMode === 'trash' ? 0 : selectedBytes();
  }

  function agentsInSelection() {
    var map = {};
    var order = [];
    selectedSessions().forEach(function (session) {
      if (!map[session.agentId]) {
        map[session.agentId] = { agent: agentById(session.agentId), agentId: session.agentId, count: 0, bytes: 0 };
        order.push(session.agentId);
      }
      map[session.agentId].count += 1;
      map[session.agentId].bytes += Number(session.bytes) || 0;
    });
    return order.map(function (id) { return map[id]; });
  }

  function activeInSelection() {
    return selectedSessions().filter(function (session) { return !!session.active; }).length;
  }

  function hiddenSelected() {
    var visible = {};
    visibleSessions().forEach(function (session) { visible[session.uid] = true; });
    var hidden = 0;
    state.selected.forEach(function (uid) {
      if (!visible[uid] && state.sessions.some(function (session) { return session.uid === uid; })) hidden += 1;
    });
    return hidden;
  }

  function activeFilterCount() {
    var count = 0;
    if (state.filter.agentId !== 'all') count += 1;
    if (state.filter.storage !== 'all') count += 1;
    if (state.filter.age !== 'all') count += 1;
    if (state.filter.size !== 'all') count += 1;
    return count;
  }

  function currentFocusSession() {
    return state.focusUid ? sessionByUid(state.focusUid) : null;
  }

  function focusFirstVisible() {
    var visible = visibleSessions();
    state.focusUid = visible.length ? visible[0].uid : null;
    if (state.focusUid) state.focusSuppressed = false;
  }

  function ensureFocusVisible() {
    var visible = visibleSessions();
    if (!visible.length) {
      state.focusUid = null;
      return;
    }
    var stillVisible = visible.some(function (session) { return session.uid === state.focusUid; });
    if (!stillVisible) {
      state.focusUid = visible[0].uid;
      state.focusSuppressed = false;
    }
  }

  /* =========================================================================
     3. Icons (brand SVGs from brand-icons.js, line icons from index.html sprite)
     ========================================================================= */

  function brandIcon(agent) {
    var key = agent && BRAND.iconKeys ? BRAND.iconKeys[agent.id] : null;
    return key && BRAND.icons ? BRAND.icons[key] : null;
  }

  function brandColor(agent) {
    var icon = brandIcon(agent);
    if (!icon) return '#3F6599';
    var dark = isDarkTheme();
    return (dark && icon.dark) ? icon.dark : (icon.color || '#3F6599');
  }

  function initialOf(agent) {
    return agent && agent.name ? String(agent.name).trim().charAt(0).toUpperCase() : '?';
  }

  /* A custom agent has no bundled vector; ask the backend for the icon of its
     installed .app. Fetch (not <img>) so an expected 404 stays out of the
     console; a miss is remembered and never requested twice. */
  function attachBackendIcon(container, initial, agent) {
    if (!agent || !agent.custom || !agent.id) return;
    if (state.platform !== 'darwin') return;
    if (state.iconMisses[agent.id] || state.iconPending[agent.id]) return;
    state.iconPending[agent.id] = true;
    var url = '/api/icon?id=' + encodeURIComponent(agent.id);
    fetch(url, { headers: { Accept: 'image/png' } }).then(function (response) {
      if (!response.ok) throw new Error('no icon');
      return response.blob();
    }).then(function (blob) {
      delete state.iconPending[agent.id];
      var objectUrl = URL.createObjectURL(blob);
      var img = h('img', { alt: '', decoding: 'async' });
      img.style.cssText = 'width:100%;height:100%;object-fit:contain;display:block';
      img.addEventListener('load', function () {
        container.classList.add('has-image');
        container.style.backgroundColor = 'transparent';
        if (initial && initial.parentNode) initial.parentNode.removeChild(initial);
        URL.revokeObjectURL(objectUrl);
      });
      img.addEventListener('error', function () {
        state.iconMisses[agent.id] = true;
        if (img.parentNode) img.parentNode.removeChild(img);
        URL.revokeObjectURL(objectUrl);
      });
      img.src = objectUrl;
      container.appendChild(img);
    }).catch(function () {
      delete state.iconPending[agent.id];
      state.iconMisses[agent.id] = true;
    });
  }

  function bundleAgentIcon(agent, sizeClass) {
    var container = h('span', {
      'class': 'agent-icon' + (sizeClass ? ' ' + sizeClass : ''),
      'aria-hidden': 'true'
    });
    var color = brandColor(agent);
    container.style.setProperty('--agent-color', color);
    container.style.color = color;
    container.style.backgroundColor = hexToRgba(color, isDarkTheme() ? 0.18 : 0.1);

    var icon = brandIcon(agent);
    if (icon && icon.paths && icon.paths.length) {
      var svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
      svg.setAttribute('viewBox', icon.viewBox || '0 0 24 24');
      svg.setAttribute('class', 'agent-svg');
      svg.setAttribute('aria-hidden', 'true');
      svg.setAttribute('focusable', 'false');
      svg.setAttribute('fill', 'currentColor');
      icon.paths.forEach(function (d) {
        var path = doc.createElementNS('http://www.w3.org/2000/svg', 'path');
        path.setAttribute('d', d);
        svg.appendChild(path);
      });
      container.appendChild(svg);
    } else {
      var initial = h('span', { 'class': 'agent-initial', text: initialOf(agent) });
      container.appendChild(initial);
      attachBackendIcon(container, initial, agent);
    }
    return container;
  }

  function spriteIcon(id, className) {
    return sprite(id, className || 'icon');
  }

  /* =========================================================================
     4. Sidebar rendering (CONTRACT §4.3)
     ========================================================================= */

  /* Only agents that actually hold sessions are listed: the catalog has 37
     entries, and 36 empty rows would bury the one that matters. The footer
     line still says how many agents were checked. */
  function renderSidebar() {
    var wrap = byId('agentGroups');
    if (!wrap) return;
    clear(wrap);

    var armed = [];
    var byGroup = {};
    state.agents.forEach(function (agent) {
      var stats = agentStats(agent.id);
      if (stats.count <= 0) return;
      armed.push(agent.id);
      var group = agent.group || 'other';
      if (!byGroup[group]) byGroup[group] = [];
      byGroup[group].push({ agent: agent, stats: stats });
    });

    GROUPS.forEach(function (group) {
      var agents = byGroup[group.key] || [];
      if (!agents.length) return;
      var label = h('div', { 'class': 'agent-group-label' }, GROUP_LABELS[group.key] || group.label);
      label.appendChild(h('span', { text: String(agents.length) }));
      wrap.appendChild(label);

      agents.forEach(function (entry) {
        var agent = entry.agent;
        var stats = entry.stats;
        var active = state.filter.agentId === agent.id;
        var row = h('button', {
          'class': 'agent-row' + (active ? ' is-active' : ''),
          type: 'button',
          'data-agent': agent.id,
          'aria-pressed': active ? 'true' : 'false',
          title: agent.name + ' · ' + stats.count + ' 条会话 · ' + formatBytes(stats.bytes)
        });
        row.appendChild(bundleAgentIcon(agent));
        row.appendChild(h('span', { 'class': 'agent-name', text: agent.name }));
        row.appendChild(h('span', { 'class': 'agent-count', text: String(stats.count) }));
        row.appendChild(h('span', { 'class': 'agent-meta', text: formatBytes(stats.bytes) }));
        bind(row, 'click', function () {
          selectAgent(agent.id);
        }, false);
        wrap.appendChild(row);
      });
    });

    var checked = state.agents.length;
    var without = checked - armed.length;
    if (checked > 0) {
      var noteText = without > 0
        ? '已检查 ' + checked + ' 款 Agent · ' + without + ' 款没有会话'
        : '已检查 ' + checked + ' 款 Agent · 全部有会话';
      var note = h('div', { 'class': 'agent-checked-note' }, noteText);
      note.style.padding = '12px 8px 2px';
      note.style.color = 'var(--ink-3)';
      note.style.fontSize = '10px';
      note.style.lineHeight = '14px';
      wrap.appendChild(note);
    }

    var all = byId('agentAll');
    if (all) {
      setClass(all, 'is-active', state.filter.agentId === 'all');
      all.setAttribute('aria-pressed', state.filter.agentId === 'all' ? 'true' : 'false');
    }
    var allCount = byId('allCount');
    var allMeta = byId('allMeta');
    var totals = totalStats();
    setText(allCount, String(totals.count));
    setText(allMeta, formatBytes(totals.bytes));
    setText(byId('agentTotal'), String(armed.length));
  }

  function selectAgent(agentId) {
    state.filter.agentId = agentId || 'all';
    state.filter.query = '';
    state.anchorUid = null;
    state.focusSuppressed = false;
    syncSearchInput();
    ensureFocusVisible();
    closePopover();
    closeMenu();
    renderSidebar();
    renderToolbar();
    renderTable();
    renderDetail();
    renderStatusbar();
    renderFilterChips();
    scrollFocusIntoView();
  }

  /* =========================================================================
     5. Toolbar / list header rendering (CONTRACT §4.2, §4.4)
     ========================================================================= */

  function syncSearchInput() {
    var input = byId('searchInput');
    if (input && input.value !== state.filter.query) input.value = state.filter.query;
    setHidden(byId('searchClear'), !state.filter.query);
    setHidden(byId('searchKbd'), !!state.filter.query);
  }

  function renderToolbar() {
    syncSearchInput();

    var bootLoading = state.boot.status === 'loading';
    var bootError = state.boot.status === 'error';
    var scanRunning = state.scan.status === 'running';
    setText(byId('scanBtnLabel'), bootLoading ? '连接中' : (scanRunning ? '扫描中' : '扫描'));
    setHidden(byId('scanKbd'), scanRunning || bootLoading);
    var scanBtn = byId('scanBtn');
    if (scanBtn) {
      var scanDisabled = scanRunning || bootLoading;
      scanBtn.disabled = scanDisabled;
      scanBtn.setAttribute('aria-disabled', scanDisabled ? 'true' : 'false');
      scanBtn.title = bootError ? '重新连接本地服务' : (scanRunning ? '扫描进行中' : '扫描本机会话（⌘R）');
    }

    var progress = state.scan.progress || { done: 0, total: 0 };
    var percent = progress.total ? Math.max(0, Math.min(100, Math.round(progress.done / progress.total * 100))) : 0;
    setHidden(byId('scanProgress'), !scanRunning);
    var progressWrap = byId('scanProgress');
    if (progressWrap) progressWrap.setAttribute('aria-valuenow', String(percent));
    var bar = byId('scanProgressBar');
    if (bar) bar.style.transform = 'scaleX(' + (percent / 100) + ')';

    var discoverCount = pendingCandidates().length;
    setText(byId('discoverCount'), String(discoverCount));
    setHidden(byId('discoverCount'), discoverCount === 0);

    var detailToggle = byId('detailToggle');
    if (detailToggle) {
      detailToggle.setAttribute('aria-pressed', state.detailOpen ? 'true' : 'false');
      setClass(detailToggle, 'is-pressed', state.detailOpen);
      detailToggle.setAttribute('aria-label', state.detailOpen ? '隐藏详情面板' : '显示详情面板');
    }

    var densityBtn = byId('densityBtn');
    if (densityBtn) densityBtn.setAttribute('aria-pressed', state.density === 'compact' ? 'true' : 'false');

    var agent = state.filter.agentId === 'all' ? null : agentById(state.filter.agentId);
    setText(byId('listTitle'), agent ? agent.name : '全部会话');

    var visible = visibleSessions();
    var bytes = 0;
    visible.forEach(function (session) { bytes += Number(session.bytes) || 0; });
    var meta = visible.length + ' 条 · ' + formatBytes(bytes);
    if (state.filter.query) meta = '“' + state.filter.query + '” · ' + meta;
    else if (activeFilterCount() > 0) meta = '筛选后 · ' + meta;
    setText(byId('listMeta'), meta);

    var filterBtn = byId('filterBtn');
    var filterCount = byId('filterCount');
    var count = activeFilterCount();
    setText(filterCount, String(count));
    setHidden(filterCount, count === 0);
    if (filterBtn) filterBtn.setAttribute('aria-expanded', state.popover === 'filter' ? 'true' : 'false');

    var quickBtn = byId('quickSelectBtn');
    if (quickBtn) quickBtn.setAttribute('aria-expanded', state.popover === 'quick' ? 'true' : 'false');

    var issuesBtn = byId('scanIssuesBtn');
    var issueCount = state.scan.issues.length;
    setHidden(issuesBtn, issueCount === 0);
    if (issuesBtn) {
      issuesBtn.setAttribute('aria-label', '查看扫描问题（' + issueCount + '）');
      issuesBtn.title = issueCount + ' 个 Agent 扫描失败';
    }
  }

  /* =========================================================================
     6. Session table rendering (CONTRACT §4.4)
     ========================================================================= */

  function appendHighlighted(parent, text, query) {
    var value = text === null || text === undefined ? '' : String(text);
    var needle = String(query || '').trim();
    if (!needle) {
      parent.appendChild(doc.createTextNode(value));
      return;
    }
    var lower = value.toLowerCase();
    var search = needle.toLowerCase();
    var index = lower.indexOf(search);
    if (index === -1) {
      parent.appendChild(doc.createTextNode(value));
      return;
    }
    var cursor = 0;
    while (index !== -1) {
      if (index > cursor) parent.appendChild(doc.createTextNode(value.slice(cursor, index)));
      parent.appendChild(h('mark', { text: value.slice(index, index + search.length) }));
      cursor = index + search.length;
      index = lower.indexOf(search, cursor);
    }
    if (cursor < value.length) parent.appendChild(doc.createTextNode(value.slice(cursor)));
  }

  /* Some adapters report the project path as the snippet. Repeating it under
     every row is pure noise, so treat empty/path-like snippets as "no summary". */
  function snippetIsPathLike(session) {
    var snippet = String(session.snippet || '').trim();
    if (!snippet) return true;
    var project = String(session.project || '').trim();
    if (project && snippet === project) return true;
    if (snippet.charAt(0) !== '/') return false;
    return snippet.indexOf(' ') === -1 &&
      snippet.indexOf('\t') === -1 &&
      snippet.indexOf('\n') === -1;
  }

  function rowSummary(session) {
    var snippet = String(session.snippet || '').trim();
    if (!snippetIsPathLike(session)) return { text: snippet, muted: false };
    if (session.storage === 'sqlite') {
      return { text: 'SQLite 行 · 正文在数据库中', muted: true };
    }
    if (session.storage === 'dir') {
      return { text: '目录会话 · 暂无摘要', muted: true };
    }
    return { text: '暂无摘要', muted: true };
  }

  function buildRow(session) {
    var agent = agentById(session.agentId);
    var selected = state.selected.has(session.uid);
    var focused = state.focusUid === session.uid;
    var row = h('tr', {
      'class': 'session-row' + (selected ? ' is-selected' : '') + (focused ? ' is-focused' : ''),
      'data-uid': session.uid,
      tabindex: focused ? '0' : '-1',
      'aria-selected': selected ? 'true' : 'false',
      'data-active': session.active ? 'true' : 'false'
    });

    var checkCell = h('td', { 'class': 'cell-check' });
    var checkbox = h('input', {
      type: 'checkbox',
      'class': 'row-check',
      'aria-label': '选择会话：' + (session.title || session.id || ''),
      tabindex: '-1'
    });
    checkbox.checked = selected;
    checkCell.appendChild(checkbox);
    row.appendChild(checkCell);

    var titleCell = h('td', { 'class': 'cell-title' });
    var titleLine = h('div', { 'class': 'row-title-line' });
    if (state.filter.agentId === 'all') {
      var rowIcon = bundleAgentIcon(agent, 'is-sm row-agent-icon');
      titleLine.appendChild(rowIcon);
    }
    var title = h('span', {
      'class': 'row-title',
      title: session.title || session.id || ''
    });
    appendHighlighted(title, session.title || session.id || '(无标题)', state.filter.query);
    titleLine.appendChild(title);

    if (session.active) {
      var activeBadge = h('span', { 'class': 'badge badge-warn' });
      activeBadge.appendChild(spriteIcon('i-alert'));
      activeBadge.appendChild(doc.createTextNode('可能正在使用'));
      titleLine.appendChild(activeBadge);
    } else if (session.storage === 'sqlite') {
      titleLine.appendChild(h('span', { 'class': 'badge badge-neutral', text: 'SQLite 行' }));
    } else if (session.storage === 'dir') {
      titleLine.appendChild(h('span', { 'class': 'badge badge-neutral', text: '目录' }));
    }
    titleCell.appendChild(titleLine);

    var summary = rowSummary(session);
    titleCell.appendChild(h('div', {
      'class': 'row-snippet' + (summary.muted ? ' is-muted' : ''),
      text: summary.text,
      title: summary.text
    }));
    row.appendChild(titleCell);

    var projectCell = h('td', { 'class': 'cell-project' });
    var projectFull = session.project || (agent && agent.home) || '';
    projectCell.appendChild(h('span', {
      'class': 'path-text',
      text: baseName(projectFull) || '—',
      title: projectFull
    }));
    row.appendChild(projectCell);

    row.appendChild(h('td', { 'class': 'cell-num', text: String(Number(session.messages) || 0) }));

    var sizeCell = h('td', { 'class': 'cell-num cell-size', text: formatBytes(session.bytes) });
    sizeCell.title = String(Number(session.bytes) || 0) + ' 字节';
    row.appendChild(sizeCell);

    var timeCell = h('td', { 'class': 'cell-time', text: relativeTime(session.updatedAt) });
    timeCell.title = fullDate(session.updatedAt);
    row.appendChild(timeCell);

    var actionCell = h('td', { 'class': 'cell-actions' });
    var more = h('button', {
      'class': 'row-more',
      type: 'button',
      'aria-label': '更多操作：' + (session.title || ''),
      title: '更多操作'
    });
    more.appendChild(spriteIcon('i-more'));
    actionCell.appendChild(more);
    row.appendChild(actionCell);

    return row;
  }

  function renderTable() {
    var tbody = byId('sessionBody');
    if (!tbody) return;
    var table = byId('sessionTable');
    var emptyState = byId('emptyState');
    var skeleton = byId('skeleton');
    var tableWrap = byId('tableWrap');

    /* The static sample in index.html must never stand in for real data. */
    if (state.boot.status === 'loading') {
      clear(tbody);
      setHidden(table, true);
      setHidden(emptyState, true);
      setHidden(skeleton, false);
      updateSelectAll([]);
      return;
    }

    if (state.boot.status === 'error') {
      clear(tbody);
      setHidden(table, true);
      setHidden(skeleton, true);
      setHidden(emptyState, false);
      renderEmptyState();
      updateSelectAll([]);
      return;
    }

    var visible = visibleSessions();
    var scanningEmpty = state.scan.status === 'running' && state.sessions.length === 0;

    if (scanningEmpty) {
      clear(tbody);
      setHidden(table, true);
      setHidden(emptyState, true);
      setHidden(skeleton, false);
      return;
    }

    if (table) {
      table.hidden = false;
      setClass(table, 'hide-agent-icon', state.filter.agentId !== 'all');
    }
    setHidden(skeleton, true);

    if (!visible.length) {
      clear(tbody);
      setHidden(table, true);
      setHidden(emptyState, false);
      renderEmptyState();
      updateSelectAll(visible);
      return;
    }

    setHidden(emptyState, true);
    if (tableWrap) tableWrap.scrollTop = tableWrap.scrollTop; // preserve scroll
    var fragment = doc.createDocumentFragment();
    visible.forEach(function (session) {
      fragment.appendChild(buildRow(session));
    });
    tbody.replaceChildren(fragment);
    updateSelectAll(visible);
  }

  function renderEmptyState() {
    var title = byId('emptyTitle');
    var desc = byId('emptyDesc');
    var action = byId('emptyAction');
    var noSessions = state.sessions.length === 0;
    var query = state.filter.query;
    var hasFilter = activeFilterCount() > 0;
    var agent = state.filter.agentId === 'all' ? null : agentById(state.filter.agentId);
    var agentSessions = agent ? agentStats(agent.id).count : 0;

    var actionText = '重新扫描';
    var actionIcon = 'i-scan';
    var actionKind = 'scan';

    if (state.boot.status === 'error') {
      var hint = state.boot.error || '无法连接本地服务';
      setText(title, '无法连接本地服务');
      setText(desc, hint + '。请确认 AgentSweep 正在运行，然后重试。');
      actionText = '重试连接';
      actionIcon = 'i-refresh';
      actionKind = 'retry';
    } else if (query) {
      setText(title, '没有匹配「' + query + '」的会话');
      setText(desc, '换个关键词，或重置筛选后再看。');
      actionText = '清除搜索';
      actionIcon = 'i-x';
      actionKind = 'clear-search';
    } else if (agent && !agent.installed) {
      setText(title, agent.name + ' 未安装');
      setText(desc, '没有找到它的数据目录。安装并使用后再扫描。');
      actionText = '查看全部会话';
      actionIcon = 'i-layers';
      actionKind = 'all';
    } else if (agent && agentSessions === 0) {
      setText(title, agent.name + ' 还没有会话');
      setText(desc, '它已经安装，但暂时没有可清理的会话记录。');
      actionText = '查看全部会话';
      actionIcon = 'i-layers';
      actionKind = 'all';
    } else if (noSessions && state.scan.status === 'idle') {
      setText(title, '还没有扫描过');
      setText(desc, '点扫描读取本机的 Agent 会话；扫描只读取文件，不会修改。');
      actionText = '扫描本机';
      actionIcon = 'i-scan';
      actionKind = 'scan';
    } else if (noSessions) {
      setText(title, '这台 Mac 上还没有找到会话');
      setText(desc, '扫描只会读取文件，不会修改。装了新 Agent 后再扫一次。');
    } else if (hasFilter) {
      setText(title, '没有符合筛选条件的会话');
      setText(desc, '放宽筛选条件，或切回全部会话看看。');
      actionText = '重置筛选';
      actionIcon = 'i-x';
      actionKind = 'reset-filters';
    } else {
      setText(title, '这台 Mac 上还没有找到会话');
      setText(desc, '扫描只会读取文件，不会修改。装了新 Agent 后再扫一次。');
    }

    if (action) {
      action.dataset.action = actionKind;
      action.replaceChildren(spriteIcon(actionIcon), doc.createTextNode(actionText));
    }
  }

  function updateSelectAll(visible) {
    var selectAll = byId('selectAll');
    if (!selectAll) return;
    var list = visible || visibleSessions();
    var selectedCount = 0;
    list.forEach(function (session) {
      if (state.selected.has(session.uid)) selectedCount += 1;
    });
    var allSelected = list.length > 0 && selectedCount === list.length;
    var someSelected = selectedCount > 0 && !allSelected;
    selectAll.checked = allSelected;
    selectAll.indeterminate = someSelected;
    selectAll.setAttribute('aria-checked', allSelected ? 'true' : (someSelected ? 'mixed' : 'false'));
    selectAll.disabled = list.length === 0;
  }

  function paintRows() {
    var tbody = byId('sessionBody');
    if (!tbody) return;
    var rows = $$('tr.session-row', tbody);
    rows.forEach(function (row) {
      var uid = row.getAttribute('data-uid');
      var selected = state.selected.has(uid);
      var focused = state.focusUid === uid;
      setClass(row, 'is-selected', selected);
      setClass(row, 'is-focused', focused);
      row.setAttribute('aria-selected', selected ? 'true' : 'false');
      row.tabIndex = focused ? 0 : -1;
      var check = $('.row-check', row);
      if (check) check.checked = selected;
    });
    var visible = visibleSessions();
    updateSelectAll(visible);
  }

  function focusRowDom(uid, scroll) {
    if (!uid) return;
    var row = byId('sessionBody') ? byId('sessionBody').querySelector('tr.session-row[data-uid="' + uid.replace(/"/g, '\\"') + '"]') : null;
    if (!row) return;
    try { row.focus({ preventScroll: true }); } catch (err) { row.focus(); }
    if (scroll !== false) {
      try { row.scrollIntoView({ block: 'nearest' }); } catch (err) { row.scrollIntoView(); }
    }
  }

  function scrollFocusIntoView() {
    if (!state.focusUid) return;
    var row = byId('sessionBody') ? byId('sessionBody').querySelector('tr.session-row[data-uid="' + state.focusUid.replace(/"/g, '\\"') + '"]') : null;
    if (row && row.scrollIntoView) {
      try { row.scrollIntoView({ block: 'nearest' }); } catch (err) { row.scrollIntoView(); }
    }
  }

  /* =========================================================================
     7. Detail panel rendering (CONTRACT §4.5, §5.6)
     ========================================================================= */

  function animateDetail() {
    var body = byId('detailBody');
    if (!body || !body.animate || prefersReducedMotion()) return;
    try {
      body.animate(
        [{ opacity: 0.985 }, { opacity: 1 }],
        { duration: 140, easing: 'cubic-bezier(.2,.8,.2,1)' }
      );
    } catch (err) { /* animation is decorative */ }
  }

  function detailStorageBadge(storage) {
    var badge = h('span', { 'class': 'badge badge-storage' });
    badge.appendChild(spriteIcon(STORAGE_ICONS[storage] || 'i-file'));
    badge.appendChild(doc.createTextNode(STORAGE_LABELS[storage] || '文件'));
    return badge;
  }

  function pathRow(path, label) {
    var li = h('li', { 'class': 'path-row' });
    li.appendChild(h('span', {
      'class': 'path-text',
      text: abbrevPath(path),
      title: path
    }));
    var actions = h('span', { 'class': 'path-actions' });
    var copy = h('button', {
      'class': 'icon-btn icon-btn-xs',
      type: 'button',
      'data-path-action': 'copy',
      'data-path': path,
      'aria-label': '复制路径：' + (label || abbrevPath(path)),
      title: '复制路径'
    });
    copy.appendChild(spriteIcon('i-copy'));
    var reveal = h('button', {
      'class': 'icon-btn icon-btn-xs',
      type: 'button',
      'data-path-action': 'reveal',
      'data-path': path,
      'aria-label': '在 Finder 中显示：' + (label || abbrevPath(path)),
      title: '在 Finder 中显示'
    });
    reveal.appendChild(spriteIcon('i-external'));
    actions.appendChild(copy);
    actions.appendChild(reveal);
    li.appendChild(actions);
    return li;
  }

  function detailSection(title, note) {
    var section = h('section', { 'class': 'detail-section' });
    var head = h('div', { 'class': 'detail-section-head' });
    head.appendChild(h('h4', { 'class': 'detail-section-title', text: title }));
    if (note) head.appendChild(h('span', { 'class': 'detail-section-note', text: note }));
    section.appendChild(head);
    return section;
  }

  function renderDetailEmpty() {
    var wrap = h('div', { 'class': 'detail-empty' });
    var icon = h('span', { 'class': 'empty-icon', 'aria-hidden': 'true' });
    icon.appendChild(spriteIcon('i-info'));
    wrap.appendChild(icon);
    wrap.appendChild(h('h3', { 'class': 'detail-empty-title empty-title', text: '选一条会话查看详情' }));
    wrap.appendChild(h('p', {
      'class': 'detail-empty-desc empty-desc',
      text: '点击列表行即可查看；勾选才会选中。'
    }));
    return wrap;
  }

  function renderDetail() {
    var body = byId('detailBody');
    if (!body) return;
    var session = currentFocusSession();
    clear(body);

    if (!session) {
      body.appendChild(renderDetailEmpty());
      syncDetailFoot(null);
      return;
    }

    var agent = agentById(session.agentId);
    var agentLabel = agent ? agent.name : '未知 Agent';

    /* 1. identity */
    var identity = h('div', { 'class': 'detail-identity' });
    identity.appendChild(bundleAgentIcon(agent, 'is-lg'));
    var copy = h('span', { 'class': 'detail-identity-copy' });
    copy.appendChild(h('span', { 'class': 'detail-agent-name', text: agentLabel }));
    copy.appendChild(h('span', { 'class': 'detail-vendor', text: agent && agent.vendor ? agent.vendor : '未知厂商' }));
    identity.appendChild(copy);
    identity.appendChild(detailStorageBadge(session.storage));
    body.appendChild(identity);

    /* 2. title */
    body.appendChild(h('h3', { 'class': 'detail-title', text: session.title || session.id || '(无标题会话)' }));

    /* 3. meta */
    var meta = h('p', { 'class': 'detail-meta' });
    meta.appendChild(h('span', { text: relativeTime(session.updatedAt) }));
    meta.appendChild(h('span', { 'class': 'dot-divider', 'aria-hidden': 'true' }));
    meta.appendChild(h('span', { text: (Number(session.messages) || 0) + ' 条消息' }));
    body.appendChild(meta);

    /* 4. badges */
    var badges = h('div', { 'class': 'detail-badges' });
    if (session.active) {
      var activeBadge = h('span', { 'class': 'badge badge-warn' });
      activeBadge.appendChild(spriteIcon('i-alert'));
      activeBadge.appendChild(doc.createTextNode('可能正在使用'));
      badges.appendChild(activeBadge);
    }
    badges.appendChild(h('span', { 'class': 'badge badge-neutral', text: STORAGE_LABELS[session.storage] || '文件' }));
    if (agent && agent.custom) badges.appendChild(h('span', { 'class': 'badge badge-neutral', text: '自定义' }));
    body.appendChild(badges);

    /* 5. active warning */
    if (session.active) {
      var callout = h('div', { 'class': 'callout callout-warn' });
      callout.appendChild(spriteIcon('i-alert'));
      var calloutCopy = h('div', { 'class': 'callout-copy' });
      calloutCopy.appendChild(h('strong', { text: '这条会话可能正在使用。' }));
      calloutCopy.appendChild(h('span', {
        text: '清理前建议先退出 ' + agentLabel + '。'
      }));
      callout.appendChild(calloutCopy);
      body.appendChild(callout);
    }

    /* 6. stats */
    var stats = h('div', { 'class': 'detail-stats' });
    var statPairs = [
      ['体积', formatBytes(session.bytes)],
      ['消息', String(Number(session.messages) || 0)],
      ['最后更新', relativeTime(session.updatedAt)]
    ];
    statPairs.forEach(function (pair) {
      var stat = h('div', { 'class': 'stat' });
      stat.appendChild(h('span', { 'class': 'stat-label', text: pair[0] }));
      stat.appendChild(h('span', { 'class': 'stat-value', text: pair[1] }));
      stats.appendChild(stat);
    });
    body.appendChild(stats);

    /* 7. impact (first, so all four rows stay above the fold in a 760 window) */
    var isSqlite = session.storage === 'sqlite';
    var paths = (session.paths || []).slice();
    var trash = state.prefs.deleteMode === 'trash';
    var impactSection = detailSection('清理影响', trash ? '可撤销' : '不可撤销');
    var impactList = h('div', { 'class': 'impact-list' });
    var indexValue;
    if (!state.prefs.includeSnapshots) indexValue = '不额外备份';
    else if (isSqlite) indexValue = '整库备份后删行';
    else indexValue = '先备份再裁剪';
    var impactPairs = [
      ['正文去向', trash ? '移入废纸篓' : '永久删除'],
      ['索引处理', indexValue],
      ['撤销窗口', trash ? '可在设置中撤销' : '不可撤销'],
      ['真实释放', trash
        ? '0 B · 清空废纸篓后生效'
        : formatBytes(session.bytes) + ' · 立即生效']
    ];
    impactPairs.forEach(function (pair) {
      var row = h('div', { 'class': 'impact-row' });
      row.appendChild(h('span', { 'class': 'impact-label', text: pair[0] }));
      row.appendChild(h('span', { 'class': 'impact-value', text: pair[1] }));
      impactList.appendChild(row);
    });
    impactSection.appendChild(impactList);
    body.appendChild(impactSection);

    /* 8. file locations (the API only exposes session paths) */
    var locationNote = paths.length
      ? paths.length + ' 个位置'
      : (isSqlite ? 'SQLite 数据库' : '无独立文件');
    var locations = detailSection('文件位置', locationNote);

    if (isSqlite) {
      var sqliteNote = h('p', {
        'class': 'detail-section-hint',
        text: '正文在 SQLite 数据库中，没有独立会话文件；删除前会整库备份，删行后执行 VACUUM。'
      });
      locations.appendChild(sqliteNote);
    }

    var pathList = h('ul', { 'class': 'path-list' });
    if (paths.length) {
      paths.forEach(function (path) {
        pathList.appendChild(pathRow(path, session.title));
      });
      locations.appendChild(pathList);
    } else {
      var fallbackPath = session.project || (agent && agent.home) || '';
      if (fallbackPath) {
        pathList.appendChild(pathRow(fallbackPath, '项目路径'));
        locations.appendChild(pathList);
      } else {
        locations.appendChild(h('p', {
          'class': 'detail-section-hint',
          text: '这个 Agent 没有提供可显示的文件路径。'
        }));
      }
    }
    body.appendChild(locations);

    /* 9. agent note */
    var note = h('details', { 'class': 'detail-note' });
    note.appendChild(h('summary', { text: '关于 ' + agentLabel + ' 的清理说明' }));
    note.appendChild(h('p', {
      text: (agent && agent.note) || '这个 Agent 没有额外的索引说明。清理只会处理会话本身。'
    }));
    body.appendChild(note);

    syncDetailFoot(session);
    animateDetail();
  }

  function syncDetailFoot(session) {
    var reveal = byId('detailReveal');
    var select = byId('detailSelect');
    var hasFocus = !!session;
    if (reveal) {
      reveal.disabled = !hasFocus;
      reveal.setAttribute('aria-disabled', hasFocus ? 'false' : 'true');
    }
    if (select) {
      select.disabled = !hasFocus;
      select.setAttribute('aria-disabled', hasFocus ? 'false' : 'true');
      var selected = hasFocus && state.selected.has(session.uid);
      var label = select.querySelector('span');
      var text = !hasFocus ? '选中此会话' : (selected ? '取消选中' : '选中此会话');
      if (label) setText(label, text);
      else setText(select, text);
      select.setAttribute('aria-pressed', selected ? 'true' : 'false');
    }
  }

  /* =========================================================================
     8. Status bar (CONTRACT §4.6)
     ========================================================================= */

  function statusDot(tone) {
    var dot = h('span', {
      'class': 'status-dot is-' + tone,
      'data-tone': tone === 'ok' ? 'ok' : (tone === 'warn' ? 'warn' : (tone === 'busy' ? 'busy' : 'ok')),
      'aria-hidden': 'true'
    });
    return dot;
  }

  function renderStatusbar() {
    renderScanStatus();
    renderSelectionSummary();
    renderUndoChip();
    renderCleanButton();
  }

  function renderScanStatus() {
    var wrap = byId('scanStatus');
    if (!wrap) return;
    var progress = state.scan.progress || { done: 0, total: 0, current: '' };
    var tone = 'ok';
    var text = '';
    var showProgress = false;

    if (state.boot.status === 'loading') {
      tone = 'busy';
      text = '正在连接本地服务…';
    } else if (state.boot.status === 'error') {
      tone = 'warn';
      var bootMessage = state.boot.error || '未知错误';
      text = bootMessage.indexOf('无法连接') === 0 ? bootMessage : '无法连接本地服务 · ' + bootMessage;
    } else if (state.deleting) {
      tone = 'busy';
      text = '正在清理 ' + state.selected.size + ' 条会话…';
    } else if (state.undoBusy) {
      tone = 'busy';
      text = '正在撤销上次清理…';
    } else if (state.scan.status === 'running') {
      tone = 'busy';
      text = '扫描中 · ' + progress.done + '/' + progress.total + (progress.current ? ' · ' + progress.current : '');
      showProgress = true;
    } else if (state.scan.status === 'error') {
      tone = 'warn';
      text = state.scan.issues.length
        ? state.scan.issues.length + ' 个 Agent 未能扫描'
        : '扫描没有完成';
    } else if (state.sessions.length === 0) {
      tone = 'ok';
      text = state.scan.status === 'idle' ? '就绪 · 还没有扫描' : '就绪 · 没有找到会话';
    } else if (state.scan.status === 'done') {
      var totals = totalStats();
      tone = state.scan.issues.length ? 'warn' : 'ok';
      text = '扫描完成 · ' + totals.count + ' 条会话 · ' + formatBytes(totals.bytes) +
        (state.scan.issues.length ? ' · ' + state.scan.issues.length + ' 个 Agent 失败' : '');
    } else {
      var totalsIdle = totalStats();
      tone = 'ok';
      text = '就绪 · ' + totalsIdle.count + ' 条会话 · ' + formatBytes(totalsIdle.bytes);
    }

    var children = [statusDot(tone), h('span', { 'class': 'status-text', text: text })];
    if (showProgress) {
      var percent = progress.total ? Math.max(0, Math.min(100, Math.round(progress.done / progress.total * 100))) : 0;
      var mini = h('span', {
        'class': 'status-progress',
        role: 'progressbar',
        'aria-label': '扫描进度',
        'aria-valuemin': '0',
        'aria-valuemax': '100',
        'aria-valuenow': String(percent)
      });
      var fill = h('span', { 'class': 'status-progress-fill' });
      fill.style.transform = 'scaleX(' + (percent / 100) + ')';
      mini.appendChild(fill);
      children.push(mini);
    }
    if (state.scan.status !== 'running' && state.scan.issues.length) {
      children.push(h('button', {
        'class': 'btn-link status-link',
        type: 'button',
        'data-status-action': 'issues',
        text: state.issueExpanded ? '收起' : '查看'
      }));
    }
    wrap.replaceChildren.apply(wrap, children);
    wrap.classList.toggle('is-error', state.boot.status === 'error' || state.scan.status === 'error');
  }

  function renderSelectionSummary() {
    var wrap = byId('selectionSummary');
    if (!wrap) return;
    var selected = selectedSessions();
    var line = h('span', { 'class': 'selection-line' });
    var ledger = h('span', { 'class': 'selection-ledger' });

    var hidden = hiddenSelected();
    if (!selected.length) {
      append(line, '已选 0 条');
      wrap.replaceChildren(line);
      wrap.title = '';
      return;
    }
    append(line, ['已选 ', h('b', { text: String(selected.length) }), ' 条 · ',
      h('b', { text: formatBytes(selectedBytes()) })]);
    append(ledger, ['真实释放 ', h('b', { text: formatBytes(trashBytes()) })]);
    if (state.prefs.deleteMode === 'trash' && hidden === 0) {
      append(ledger, ' · 清空废纸篓后生效');
    }
    wrap.replaceChildren(line, ledger);
    if (hidden > 0) {
      wrap.appendChild(h('span', {
        'class': 'selection-hidden',
        text: '其中 ' + hidden + ' 条不在当前筛选'
      }));
    }
    wrap.title = hidden > 0 ? '其中 ' + hidden + ' 条不在当前筛选' : '';
  }

  function latestUndoableBatch() {
    var found = null;
    state.batches.forEach(function (batch) {
      if (!batch.undoable) return;
      var time = new Date(batch.at).getTime();
      if (isNaN(time)) return;
      if (!found || new Date(found.at).getTime() < time) found = batch;
    });
    return found;
  }

  /* A single delete can write one journal per Agent, so "the last cleanup"
     is usually several batch ids. Prefer the ids we just received. */
  function pendingUndoBatchIds() {
    var ids = (state.lastDelete && state.lastDelete.ids) || [];
    if (!ids.length) return [];
    var allUndoable = ids.every(function (id) {
      var found = false;
      state.batches.forEach(function (batch) {
        if (batch.id === id) found = !!batch.undoable;
      });
      return found;
    });
    return allUndoable ? ids.slice() : [];
  }

  function renderUndoChip() {
    var chip = byId('undoChip');
    if (!chip) return;
    var batch = latestUndoableBatch();
    if (!batch || state.undoBusy) {
      setHidden(chip, true);
      chip.dataset.batch = '';
      return;
    }
    setHidden(chip, false);
    chip.dataset.batch = batch.id;
    var ids = pendingUndoBatchIds();
    var count = batch.count;
    if (ids.length) {
      count = 0;
      ids.forEach(function (id) {
        state.batches.forEach(function (item) {
          if (item.id === id) count += Number(item.count) || 0;
        });
      });
    }
    var text = chip.querySelector('.undo-chip-text');
    if (text) setText(text, '已清理 ' + count + ' 条');
    else setText(chip, '已清理 ' + count + ' 条 · 撤销');
    var action = chip.querySelector('.undo-chip-action');
    if (action) setText(action, '撤销');
  }

  function renderCleanButton() {
    var button = byId('cleanBtn');
    if (!button) return;
    var label = byId('cleanBtnLabel');
    var count = selectedSessions().length;
    var disabled = count === 0 || state.deleting || state.undoBusy || state.boot.status !== 'ready';
    button.disabled = disabled;
    button.setAttribute('aria-disabled', disabled ? 'true' : 'false');
    button.title = disabled ? '选择会话后可清理' : '清理选中的会话（⌘⌫）';
    button.classList.toggle('is-danger', state.prefs.deleteMode === 'permanent');
    button.classList.toggle('is-busy', state.deleting);
    button.dataset.mode = state.prefs.deleteMode;

    var text;
    if (state.deleting) text = '正在清理…';
    else if (count === 0) text = '清理';
    else if (state.prefs.deleteMode === 'permanent') text = '永久删除 ' + count + ' 条';
    else text = '清理 ' + count + ' 条';
    if (label) setText(label, text);
    else setText(button, text);
  }

  /* =========================================================================
     9. Filter chips (CONTRACT §4.4 / §5.3)
     ========================================================================= */

  var AGE_LABELS = { today: '今天', '7d': '7 天内', '30d': '30 天内', older: '更早' };
  var SIZE_LABELS = { small: '小于 10 KB', medium: '10 KB–1 MB', large: '大于 1 MB' };

  function chip(label, kind) {
    var wrapper = h('span', { 'class': 'filter-chip' });
    wrapper.appendChild(h('span', { 'class': 'filter-chip-label', text: label }));
    var remove = h('button', {
      'class': 'filter-chip-x',
      type: 'button',
      'data-chip': kind,
      'aria-label': '移除筛选：' + label
    });
    remove.appendChild(spriteIcon('i-x'));
    wrapper.appendChild(remove);
    return wrapper;
  }

  function renderFilterChips() {
    var wrap = byId('filterChips');
    if (!wrap) return;
    var items = [];
    if (state.filter.agentId !== 'all') {
      items.push(chip(agentName(state.filter.agentId), 'agent'));
    }
    if (state.filter.storage !== 'all') items.push(chip(STORAGE_LABELS[state.filter.storage] || '存储', 'storage'));
    if (state.filter.age !== 'all') items.push(chip(AGE_LABELS[state.filter.age] || '时间', 'age'));
    if (state.filter.size !== 'all') items.push(chip(SIZE_LABELS[state.filter.size] || '体积', 'size'));
    wrap.replaceChildren.apply(wrap, items);
    setHidden(wrap, items.length === 0);
  }

  /* =========================================================================
     10. Batch history (settings + status receipt)
     ========================================================================= */

  /* BatchView has no deleteMode: only claim what the API actually reports. */
  function batchMetaLabel(batch) {
    return '真实释放 ' + formatBytes(batch.freed) + ' · ' + batchAgentNames(batch);
  }

  function batchAgentNames(batch) {
    var names = (batch.agents || []).map(function (id) { return agentName(id); });
    return names.length ? names.join('、') : '未知 Agent';
  }

  function renderBatches() {
    var wrap = byId('batchList');
    if (!wrap) return;
    clear(wrap);
    if (!state.batches.length) {
      wrap.appendChild(h('div', { 'class': 'batch-empty', text: '还没有清理记录' }));
      return;
    }
    state.batches.forEach(function (batch) {
      var row = h('div', { 'class': 'batch-row' });
      var copyWrap = h('div', { 'class': 'batch-copy' });
      copyWrap.appendChild(h('span', {
        'class': 'batch-title',
        text: relativeTime(batch.at) + ' · ' + batch.count + ' 条'
      }));
      copyWrap.appendChild(h('span', {
        'class': 'batch-meta',
        text: batchMetaLabel(batch)
      }));
      row.appendChild(copyWrap);

      if (batch.undoable) {
        var undo = h('button', {
          'class': 'btn btn-quiet btn-xs batch-undo',
          type: 'button',
          'data-undo': batch.id,
          text: '撤销'
        });
        if (state.undoBusy) {
          undo.disabled = true;
          undo.setAttribute('aria-disabled', 'true');
        }
        row.appendChild(undo);
      } else {
        row.appendChild(h('span', {
          'class': 'batch-tag',
          text: '不可撤销'
        }));
      }
      wrap.appendChild(row);
    });
  }

  /* =========================================================================
     11. Issue banner (CONTRACT §4.4 / §5.2)
     ========================================================================= */

  function renderIssueBanner() {
    var banner = byId('issueBanner');
    var list = byId('issueList');
    var button = byId('issueBannerBtn');
    var issues = state.scan.issues || [];
    if (!banner) return;

    setHidden(banner, issues.length === 0);
    if (!issues.length) {
      if (list) clear(list);
      return;
    }
    setText(byId('issueBannerText'), issues.length + ' 个 Agent 未能扫描');
    if (button) {
      setText(button, state.issueExpanded ? '收起' : '查看');
      button.setAttribute('aria-expanded', state.issueExpanded ? 'true' : 'false');
    }

    if (list) {
      clear(list);
      issues.forEach(function (issue) {
        var row = h('div', { 'class': 'issue-row' });
        row.appendChild(h('span', { 'class': 'issue-agent', text: agentName(issue.agentId) }));
        row.appendChild(h('span', { 'class': 'issue-msg', text: issue.message }));
        row.appendChild(h('button', {
          'class': 'issue-retry',
          type: 'button',
          'data-retry': issue.agentId,
          text: '重试'
        }));
        list.appendChild(row);
      });
      setHidden(list, !state.issueExpanded);
    }
  }

  function toggleIssues() {
    state.issueExpanded = !state.issueExpanded;
    renderIssueBanner();
    renderScanStatus();
    if (state.issueExpanded) {
      var banner = byId('issueBanner');
      if (banner && banner.scrollIntoView) {
        try { banner.scrollIntoView({ block: 'nearest' }); } catch (err) { banner.scrollIntoView(); }
      }
    }
  }

  /* =========================================================================
     12. Discover sheet (CONTRACT §4.7, §5.7)
     ========================================================================= */

  function isCandidateAdopted(candidate) {
    if (!candidate) return false;
    if (candidate.adopted) return true;
    var suggested = candidate.suggested || {};
    if (suggested.id && agentById(suggested.id)) return true;
    var path = candidate.path || '';
    if (!path) return false;
    for (var i = 0; i < state.agents.length; i++) {
      var agent = state.agents[i];
      if (!agent || !agent.custom) continue;
      if (agent.home === path) return true;
      var roots = agent.roots || [];
      for (var j = 0; j < roots.length; j++) {
        if (roots[j] === path) return true;
      }
    }
    return false;
  }

  function pendingCandidates() {
    return state.candidates.filter(function (candidate) {
      return !isCandidateAdopted(candidate);
    });
  }

  function renderDiscover() {
    var wrap = byId('discoverList');
    if (!wrap) return;
    var sub = byId('discoverSub');
    clear(wrap);

    if (state.discover.loading) {
      if (sub) setText(sub, '正在寻找看起来像会话存储的目录…');
      for (var i = 0; i < 3; i++) {
        var skeleton = h('div', { 'class': 'discover-skeleton-row' });
        skeleton.appendChild(h('span', { 'class': 'sk sk-title' }));
        skeleton.appendChild(h('span', { 'class': 'sk sk-project' }));
        skeleton.appendChild(h('span', { 'class': 'sk sk-size' }));
        wrap.appendChild(skeleton);
      }
      return;
    }

    if (state.discover.error) {
      if (sub) setText(sub, '发现没有完成');
      var errorEmpty = h('div', { 'class': 'discover-empty' });
      errorEmpty.appendChild(h('p', { 'class': 'discover-empty-title', text: '没有完成发现' }));
      errorEmpty.appendChild(h('p', {
        'class': 'discover-empty-desc',
        text: state.discover.error + '。可以点「重新发现」再试一次。'
      }));
      wrap.appendChild(errorEmpty);
      return;
    }

    var pending = pendingCandidates();
    if (sub) {
      setText(sub, pending.length
        ? pending.length + ' 个看起来像会话存储的目录'
        : '没有发现未收录的目录');
    }

    if (!state.candidates.length) {
      var empty = h('div', { 'class': 'discover-empty' });
      empty.appendChild(h('p', { 'class': 'discover-empty-title', text: '没有发现未收录的目录' }));
      empty.appendChild(h('p', {
        'class': 'discover-empty-desc',
        text: 'AgentSweep 只会在已知目录和常见会话目录里查找；新的 Agent 可以先在设置里手动添加路径。'
      }));
      wrap.appendChild(empty);
      return;
    }

    state.candidates.forEach(function (candidate) {
      var adopted = isCandidateAdopted(candidate);
      var busy = !!state.adoptBusy[candidate.path];
      var card = h('article', { 'class': 'candidate-card' + (adopted ? ' is-adopted' : '') });
      var head = h('div', { 'class': 'candidate-head' });
      var idWrap = h('div', { 'class': 'candidate-id' });
      var icon = h('span', { 'class': 'candidate-icon', 'aria-hidden': 'true' });
      icon.appendChild(spriteIcon('i-sparkle'));
      idWrap.appendChild(icon);
      var candidateCopy = h('div', { 'class': 'candidate-copy' });
      candidateCopy.appendChild(h('h3', { 'class': 'candidate-name', text: candidate.name }));
      candidateCopy.appendChild(h('p', {
        'class': 'candidate-path path-text',
        text: abbrevPath(candidate.path),
        title: candidate.path
      }));
      idWrap.appendChild(candidateCopy);
      head.appendChild(idWrap);

      var adopt = h('button', {
        'class': 'btn btn-secondary btn-sm candidate-adopt',
        type: 'button',
        'data-adopt': candidate.suggested && candidate.suggested.id ? candidate.suggested.id : candidate.path,
        'data-candidate-path': candidate.path,
        text: busy ? '收录中…' : (adopted ? '已收录' : '收录为 Agent')
      });
      if (adopted || busy || !candidate.suggested) {
        adopt.disabled = true;
        adopt.setAttribute('aria-disabled', 'true');
        if (!candidate.suggested) adopt.title = '这个候选缺少适配器草稿';
      }
      head.appendChild(adopt);
      card.appendChild(head);

      var metrics = h('div', { 'class': 'candidate-metrics' });
      var confidence = h('div', { 'class': 'candidate-confidence confidence' });
      confidence.appendChild(h('span', { 'class': 'confidence-label', text: '置信度' }));
      var bar = h('span', { 'class': 'confidence-bar' });
      var percent = Math.round((Number(candidate.confidence) || 0) * 100);
      var fill = h('span', { 'class': 'confidence-fill' });
      fill.style.width = percent + '%';
      bar.appendChild(fill);
      confidence.appendChild(bar);
      confidence.appendChild(h('span', { 'class': 'confidence-value', text: percent + '%' }));
      metrics.appendChild(confidence);
      metrics.appendChild(h('span', {
        'class': 'candidate-count',
        text: (Number(candidate.sessionCount) || 0) + ' 条 · ' + formatBytes(candidate.bytes)
      }));
      card.appendChild(metrics);

      var evidence = h('div', { 'class': 'evidence-chips' });
      (candidate.evidence || []).slice(0, 3).forEach(function (item) {
        evidence.appendChild(h('span', { 'class': 'evidence-chip', text: item }));
      });
      card.appendChild(evidence);

      wrap.appendChild(card);
    });
  }

  function startDiscoverLoad(options) {
    var opts = options || {};
    if (state.discover.loading) return;
    state.discover.loading = true;
    state.discover.error = '';
    if (opts.force) state.discover.loaded = false;
    renderDiscover();
    api('GET', '/api/discover').then(function (result) {
      state.discover.loading = false;
      state.discover.loaded = true;
      var list = result && Array.isArray(result.candidates) ? result.candidates : [];
      state.candidates = list.map(function (candidate) {
        candidate.adopted = isCandidateAdopted(candidate);
        return candidate;
      });
      renderDiscover();
      renderToolbar();
    }, function (err) {
      state.discover.loading = false;
      state.discover.loaded = true;
      state.discover.error = err && err.message ? err.message : '未知错误';
      renderDiscover();
      toast('发现没有完成：' + state.discover.error, { tone: 'error' });
    });
  }

  function openDiscover() {
    closePopover();
    closeMenu();
    showOverlay('discover');
    if (!state.discover.loaded) startDiscoverLoad();
    else renderDiscover();
  }

  function adoptCandidate(candidatePath) {
    var candidate = null;
    state.candidates.forEach(function (item) {
      if (item.path === candidatePath) candidate = item;
    });
    if (!candidate) return;
    if (isCandidateAdopted(candidate)) {
      toast('这个目录已经收录');
      return;
    }
    var spec = candidate.suggested;
    if (!spec || !spec.id || !spec.name || !Array.isArray(spec.roots) || !spec.roots.length) {
      toast('这个候选缺少可用的适配器草稿，无法收录', { tone: 'error' });
      return;
    }
    if (state.adoptBusy[candidatePath]) return;
    state.adoptBusy[candidatePath] = true;
    renderDiscover();
    api('POST', '/api/adapters', spec).then(function () {
      delete state.adoptBusy[candidatePath];
      candidate.adopted = true;
      state.discover.loaded = false;
      state.highlightAgentId = spec.id;
      renderDiscover();
      renderToolbar();
      toast('已收录 ' + (spec.name || candidate.name) + '，正在扫描…');
      startScan();
    }, function (err) {
      delete state.adoptBusy[candidatePath];
      renderDiscover();
      toast('收录失败：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  /* =========================================================================
     13. Review dialog content (CONTRACT §4.7 / §5.5)
     ========================================================================= */

  function ledgerRow(label, value, note, total) {
    var row = h('div', { 'class': total ? 'ledger-total' : 'ledger-row' });
    row.appendChild(h('span', { 'class': 'ledger-label', text: label }));
    row.appendChild(h('span', { 'class': 'ledger-value', text: value }));
    row.appendChild(h('span', { 'class': 'ledger-note', text: note }));
    return row;
  }



  function renderReview() {
    var selected = selectedSessions();
    var groups = agentsInSelection();
    var count = selected.length;
    var bytes = selectedBytes();
    var mode = state.prefs.deleteMode;
    var trash = mode === 'trash';
    var activeCount = activeInSelection();

    setText(byId('reviewTitle'), trash
      ? '清理 ' + count + ' 条会话？'
      : '永久删除 ' + count + ' 条会话？');
    setText(byId('reviewLede'), trash
      ? '这 ' + count + ' 条会话会移入废纸篓，可以撤销。' +
        (state.prefs.includeSnapshots ? '索引会先备份。' : '索引不额外备份。')
      : '这 ' + count + ' 条会话会直接从磁盘删除，不进入废纸篓，也无法撤销。');

    var agentsWrap = byId('reviewAgents');
    if (agentsWrap) {
      var chips = [];
      groups.forEach(function (group) {
        var chipNode = h('span', { 'class': 'agent-chip review-agent-chip agent-chip' });
        chipNode.appendChild(bundleAgentIcon(group.agent || { id: group.agentId, name: agentName(group.agentId) }, 'is-xs'));
        chipNode.appendChild(h('span', { 'class': 'agent-chip-name', text: agentName(group.agentId) }));
        chipNode.appendChild(h('span', {
          'class': 'agent-chip-meta',
          text: group.count + ' 条 · ' + formatBytes(group.bytes)
        }));
        chips.push(chipNode);
      });
      agentsWrap.replaceChildren.apply(agentsWrap, chips);
    }

    var ledger = byId('reviewLedger');
    if (ledger) {
      ledger.replaceChildren(
        ledgerRow('会话正文', formatBytes(bytes), trash ? '移入废纸篓 · 不释放' : '永久删除 · 不可恢复'),
        ledgerRow('索引备份', state.prefs.includeSnapshots ? '先备份' : '不额外备份',
          state.prefs.includeSnapshots ? '保留在应用支持目录' : '只处理会话本身'),
        ledgerRow('真实释放', trash ? '0 B' : formatBytes(bytes), trash
          ? '清空废纸篓后生效'
          : '清理完成后生效'),
        ledgerRow('总计', formatBytes(bytes), trash ? '真实释放 0 B' : '真实释放 ' + formatBytes(bytes), true)
      );
    }

    var activeCallout = byId('reviewActive');
    if (activeCallout) {
      setHidden(activeCallout, activeCount === 0);
      if (activeCount > 0) {
        var activeAgents = groups.filter(function (group) {
          return selected.some(function (session) {
            return session.agentId === group.agentId && session.active;
          });
        }).map(function (group) { return agentName(group.agentId); });
        var names = activeAgents.join('、') || 'Agent';
        var strong = $('strong', activeCallout);
        var copy = $('.callout-copy span', activeCallout);
        setText(strong, '其中 ' + activeCount + ' 条可能正在使用。');
        setText(copy, '清理前建议先退出 ' + names + '。');
      }
    }

    var permanentWarn = byId('reviewPermanentWarn');
    setHidden(permanentWarn, trash);
    var ackRow = byId('reviewAckRow');
    setHidden(ackRow, trash);
    var ack = byId('reviewAck');
    if (ack) ack.checked = !!state.reviewAck;

    var seg = byId('reviewModeSeg');
    if (seg) {
      $$('input[name="reviewMode"]', seg).forEach(function (input) {
        input.checked = input.value === mode;
      });
    }

    var confirm = byId('reviewConfirm');
    if (confirm) {
      var disabled = state.deleting || state.undoBusy || (!trash && !state.reviewAck);
      confirm.disabled = disabled;
      confirm.setAttribute('aria-disabled', disabled ? 'true' : 'false');
      setText(confirm, trash ? '移入废纸篓 ' + count + ' 条' : '永久删除 ' + count + ' 条');
      confirm.classList.toggle('is-danger', !trash);
    }

    var error = byId('reviewError');
    if (error) {
      setHidden(error, !state.reviewError);
      setText(error, state.reviewError || '');
    }
  }

  function openReview() {
    var selected = selectedSessions();
    if (!selected.length) {
      toast('先选择要清理的会话');
      return;
    }
    state.reviewAck = false;
    state.reviewError = '';
    renderReview();
    closePopover();
    closeMenu();
    showOverlay('review');
    var cleanTrigger = byId('cleanBtn');
    if (cleanTrigger && !cleanTrigger.disabled) overlayTrigger = cleanTrigger;
  }

  /* =========================================================================
     14. Settings sync (CONTRACT §4.7 / §5.8)
     ========================================================================= */

  var FALLBACK_VERSION = '0.1.0';

  function readStored(key) {
    try {
      return window.localStorage ? window.localStorage.getItem(key) : null;
    } catch (err) {
      return null;
    }
  }

  function writeStored(key, value) {
    try {
      if (window.localStorage) window.localStorage.setItem(key, value);
    } catch (err) { /* storage may be unavailable; appearance still works */ }
  }

  /* Appearance and density are UI-only; the API has no field for them. */
  function loadLocalUi() {
    var appearance = readStored(LS_APPEARANCE);
    state.prefs.appearance = APPEARANCE_ORDER.indexOf(appearance) !== -1 ? appearance : 'auto';
    var density = readStored(LS_DENSITY);
    state.density = density === 'compact' ? 'compact' : 'comfortable';
    state.detailOpen = window.innerWidth >= 1120;
  }

  function syncSettingsControls() {
    var prefs = state.prefs;
    $$('input[name="deleteMode"]').forEach(function (input) {
      input.checked = input.value === prefs.deleteMode;
    });
    $$('input[name="appearance"]').forEach(function (input) {
      input.checked = input.value === prefs.appearance;
    });
    var confirm = byId('prefConfirm');
    if (confirm) confirm.checked = !!prefs.confirmBeforeDelete;
    var snapshots = byId('prefSnapshots');
    if (snapshots) snapshots.checked = !!prefs.includeSnapshots;
    var autoScan = byId('prefAutoScan');
    if (autoScan) autoScan.checked = !!prefs.autoScan;
    var autoCheck = byId('prefAutoCheckUpdates');
    if (autoCheck) autoCheck.checked = prefs.autoCheckUpdates !== false;
    var hint;
    if (prefs.deleteMode === 'trash') {
      hint = '正文进入废纸篓，可撤销；真实释放会在清空废纸篓后生效。' +
        (prefs.includeSnapshots ? '索引会先备份。' : '');
    } else {
      hint = '正文会被永久删除，无法恢复。' +
        (prefs.includeSnapshots ? '索引会先备份到应用支持目录。' : '索引不额外备份。');
    }
    setText(byId('deleteModeHint'), hint);
    var version = state.version || FALLBACK_VERSION;
    setText(byId('settingsVersion'), 'AgentSweep ' + version);
    syncVersionLabel();
  }

  function applyAppearance(appearance) {
    state.prefs.appearance = appearance;
    if (appearance === 'auto') delete doc.documentElement.dataset.theme;
    else doc.documentElement.dataset.theme = appearance;
    $$('input[name="appearance"]').forEach(function (input) {
      input.checked = input.value === appearance;
    });
  }

  function setAppearance(appearance) {
    if (APPEARANCE_ORDER.indexOf(appearance) === -1) return;
    applyAppearance(appearance);
    writeStored(LS_APPEARANCE, appearance);
    renderSidebar();
    renderTable();
    renderDetail();
    syncSettingsControls();
  }

  function syncDensity() {
    var root = doc.documentElement;
    if (root) root.dataset.density = state.density;
    var app = byId('app');
    if (app) app.dataset.density = state.density;
    var button = byId('densityBtn');
    if (button) button.setAttribute('aria-pressed', state.density === 'compact' ? 'true' : 'false');
  }

  function syncDetail() {
    var app = byId('app');
    if (app) app.dataset.detail = state.detailOpen ? 'open' : 'closed';
    var button = byId('detailToggle');
    if (button) {
      button.setAttribute('aria-pressed', state.detailOpen ? 'true' : 'false');
      setClass(button, 'is-pressed', state.detailOpen);
      button.setAttribute('aria-label', state.detailOpen ? '隐藏详情面板' : '显示详情面板');
    }
  }

  function toggleDetail(open) {
    state.detailOpen = typeof open === 'boolean' ? open : !state.detailOpen;
    syncDetail();
  }

  /* =========================================================================
     14b. Update — GET /api/update + POST /api/update/check|apply
     ========================================================================= */

  var UPDATE_POLL_INTERVAL = 1500;
  var UPDATE_STATES = [
    'idle', 'checking', 'available', 'uptodate',
    'downloading', 'installing', 'done', 'error', 'unsupported'
  ];
  var UPDATE_ACTIVE = { checking: true, downloading: true, installing: true };

  function canReachUpdateApi() {
    /* 桌面端由 Wails v3 用 wails:// 提供页面，serve 模式是 http://；
       只有直接用 file:// 打开的静态预览没有后端。 */
    return window.location.protocol !== 'file:';
  }

  function clampProgress(value) {
    var n = Number(value);
    if (!isFinite(n)) return 0;
    if (n < 0) return 0;
    if (n > 100) return 100;
    return n;
  }

  function normalizeUpdate(raw) {
    var source = raw && typeof raw === 'object' ? raw : {};
    return {
      state: UPDATE_STATES.indexOf(source.state) !== -1 ? source.state : 'idle',
      current: typeof source.current === 'string' ? source.current : '',
      latest: typeof source.latest === 'string' ? source.latest : '',
      available: source.available === true,
      notes: typeof source.notes === 'string' ? source.notes : '',
      pageUrl: typeof source.pageUrl === 'string' ? source.pageUrl : '',
      assetName: typeof source.assetName === 'string' ? source.assetName : '',
      assetBytes: typeof source.assetBytes === 'number' && isFinite(source.assetBytes) && source.assetBytes > 0
        ? source.assetBytes
        : 0,
      progress: clampProgress(source.progress),
      checkedAt: typeof source.checkedAt === 'string' ? source.checkedAt : '',
      error: typeof source.error === 'string' ? source.error : '',
      canApply: source.canApply !== false,
      autoCheck: typeof source.autoCheck === 'boolean'
        ? source.autoCheck
        : state.prefs.autoCheckUpdates !== false
    };
  }

  function updateStatusText(data) {
    if (!data) return '启动时自动检查一次更新';
    var size = data.assetBytes ? formatBytes(data.assetBytes) : '';
    if (data.state === 'checking') return '正在检查…';
    if (data.state === 'uptodate') return '已是最新版本';
    if (data.state === 'available') {
      var line = '新版本 v' + (data.latest || '') + ' 可用';
      if (size) line += ' · ' + size;
      if (!data.canApply) line += ' · 需从发布页手动安装';
      return line;
    }
    if (data.state === 'downloading') return '正在下载更新 · ' + Math.round(data.progress) + '%';
    if (data.state === 'installing') return '正在安装，随后会重新启动…';
    if (data.state === 'done') return '安装完成，正在重新启动…';
    if (data.state === 'error') return data.error || '检查更新失败，请稍后重试';
    if (data.state === 'unsupported') {
      return '请从“应用程序”中打开 AgentSweep 再更新，或到发布页手动下载。';
    }
    return '还没有检查过更新';
  }

  function syncVersionLabel() {
    var node = byId('versionLabel');
    if (!node) return;
    var data = state.update.data;
    var hasUpdate = !!(data && data.state === 'available' && data.latest);
    var version = (data && data.current) || state.version || FALLBACK_VERSION;
    setText(node, version);
    setClass(node, 'has-update', hasUpdate);
    node.title = hasUpdate
      ? '新版本 v' + data.latest + ' 可用 · 打开设置'
      : '版本 ' + version + ' · 打开设置';
    node.setAttribute('aria-label', hasUpdate
      ? '版本 ' + version + '，新版本 v' + data.latest + ' 可用，打开设置'
      : '版本 ' + version + '，打开设置');
  }

  function renderUpdate() {
    var data = state.update.data;
    var name = data ? data.state : 'idle';
    var status = byId('updateStatus');
    var current = byId('updateCurrent');
    var check = byId('updateCheckBtn');
    var checkLabel = byId('updateCheckLabel');
    var progress = byId('updateProgressWrap');
    var track = byId('updateProgressTrack');
    var fill = byId('updateProgressFill');
    var value = byId('updateProgressValue');
    var actions = byId('updateActions');
    var install = byId('updateInstallBtn');
    var page = byId('updatePageBtn');
    var notes = byId('updateNotes');
    var notesText = byId('updateNotesText');
    var pref = byId('prefAutoCheckUpdates');

    var version = (data && data.current) || state.version || FALLBACK_VERSION;
    setText(current, '当前 ' + version);

    if (status) {
      status.setAttribute('data-state', name);
      setText(status, updateStatusText(data));
    }

    var busy = !!(state.update.loading || state.update.checking || name === 'checking');
    var locked = name === 'downloading' || name === 'installing' || name === 'done';
    if (check) {
      check.disabled = busy || locked;
      setClass(check, 'is-busy', busy);
      check.setAttribute('aria-busy', busy ? 'true' : 'false');
      setText(checkLabel, name === 'error' ? '重试' : (busy ? '检查中…' : '检查更新'));
    }

    var showProgress = name === 'downloading';
    setHidden(progress, !showProgress);
    if (showProgress && data) {
      var pct = Math.round(data.progress);
      if (track) track.setAttribute('aria-valuenow', String(pct));
      if (fill) fill.style.transform = 'scaleX(' + (pct / 100) + ')';
      setText(value, pct + '%');
    }

    var showInstall = !!data && name === 'available' && data.canApply;
    var showPage = !!data && !!data.pageUrl && (name === 'available' || name === 'unsupported');
    setHidden(actions, !(showInstall || showPage));
    setHidden(install, !showInstall);
    setHidden(page, !showPage);
    if (install) install.disabled = !!state.update.applying;

    var showNotes = !!data && name === 'available' && !!data.notes;
    setHidden(notes, !showNotes);
    if (showNotes && notes) {
      var key = (data.latest || '') + '\n' + data.notes;
      if (state.update.notesKey !== key) {
        state.update.notesKey = key;
        notes.open = false;
        setText(notesText, data.notes);
      }
    }

    if (pref) pref.checked = state.prefs.autoCheckUpdates !== false;
    syncVersionLabel();
  }

  function setUpdateState(name) {
    var base = state.update.data;
    if (!base) {
      state.update.data = normalizeUpdate({ state: name });
    } else {
      var copy = {};
      Object.keys(base).forEach(function (key) { copy[key] = base[key]; });
      copy.state = UPDATE_STATES.indexOf(name) !== -1 ? name : 'idle';
      state.update.data = copy;
    }
    renderUpdate();
  }

  function applyUpdate(raw) {
    var previous = state.update.data;
    var next = normalizeUpdate(raw);
    if (next.state === 'available' && (!previous || previous.state !== 'available')) {
      state.update.notesKey = '';
    }
    /* A GET racing right after apply can still report "available"; keep the
       optimistic downloading state until the backend catches up. */
    if (state.update.applyStartedAt) {
      if (next.state === 'available') {
        if (Date.now() - state.update.applyStartedAt < 20000) {
          next.state = 'downloading';
          next.progress = 0;
        } else {
          state.update.applyStartedAt = 0;
        }
      } else {
        state.update.applyStartedAt = 0;
      }
    }
    state.update.data = next;
    renderUpdate();
    scheduleUpdatePoll();
  }

  function scheduleUpdatePoll() {
    if (timers.updatePoll) {
      clearTimeout(timers.updatePoll);
      timers.updatePoll = null;
    }
    var data = state.update.data;
    if (!data || !UPDATE_ACTIVE[data.state]) return;
    timers.updatePoll = setTimeout(function () {
      timers.updatePoll = null;
      refreshUpdate({ silent: true });
    }, UPDATE_POLL_INTERVAL);
  }

  function refreshUpdate(options) {
    var opts = options || {};
    if (!canReachUpdateApi()) return null;
    if (state.update.loading) return null;
    state.update.loading = true;
    renderUpdate();
    return api('GET', '/api/update').then(function (data) {
      state.update.loading = false;
      /* An explicit check owns the state until its POST resolves. */
      if (state.update.checking) {
        renderUpdate();
        return data;
      }
      if (data && typeof data === 'object' && typeof data.state === 'string') {
        applyUpdate(data);
        return data;
      }
      renderUpdate();
      return null;
    }, function (err) {
      state.update.loading = false;
      renderUpdate();
      scheduleUpdatePoll();
      if (!opts.silent) {
        toast('暂时无法检查更新：' + (err && err.message ? err.message : '未知错误'), {
          tone: 'error',
          actionLabel: '重试',
          onAction: checkForUpdates
        });
      }
      return null;
    });
  }

  function checkForUpdates() {
    if (!canReachUpdateApi()) {
      toast('当前是静态预览，更新功能需要从 AgentSweep 应用内使用');
      return;
    }
    if (state.update.checking) return;
    var previous = state.update.data;
    state.update.checking = true;
    setUpdateState('checking');
    api('POST', '/api/update/check').then(function (data) {
      state.update.checking = false;
      if (data && typeof data === 'object' && typeof data.state === 'string') {
        applyUpdate(data);
      } else {
        renderUpdate();
        refreshUpdate({ silent: true });
      }
    }, function (err) {
      state.update.checking = false;
      state.update.data = previous;
      renderUpdate();
      toast('暂时无法检查更新：' + (err && err.message ? err.message : '未知错误'), {
        tone: 'error',
        actionLabel: '重试',
        onAction: checkForUpdates
      });
    });
  }

  function applyUpdateNow() {
    var data = state.update.data;
    if (!data || data.state !== 'available' || state.update.applying) return;
    if (!data.canApply) {
      openUpdatePage();
      return;
    }
    state.update.applying = true;
    state.update.applyStartedAt = Date.now();
    setUpdateState('downloading');
    if (state.update.data) state.update.data.progress = 0;
    renderUpdate();
    api('POST', '/api/update/apply').then(function () {
      state.update.applying = false;
      scheduleUpdatePoll();
    }, function (err) {
      state.update.applying = false;
      state.update.applyStartedAt = 0;
      setUpdateState('available');
      toast('无法开始安装：' + (err && err.message ? err.message : '未知错误'), {
        tone: 'error',
        actionLabel: '重试',
        onAction: applyUpdateNow
      });
    });
  }

  function openUpdatePage() {
    var data = state.update.data;
    var url = data && data.pageUrl;
    if (!url) {
      toast('这个版本没有发布页地址');
      return;
    }
    api('POST', '/api/open', { path: url }).then(function () {}, function (err) {
      toast('打不开发布页：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  /* =========================================================================
     15. Render everything
     ========================================================================= */

  function renderAll() {
    try {
      renderSidebar();
      renderToolbar();
      renderTable();
      renderDetail();
      renderStatusbar();
      renderFilterChips();
      renderBatches();
      renderIssueBanner();
      applySortUi();
      syncSettingsControls();
      renderUpdate();
      syncDensity();
      syncDetail();
    } catch (err) {
      reportError('渲染界面时', err);
    }
  }

  /* =========================================================================
     16. Overlay manager (sheets, dialogs, palette) — CONTRACT §4.7
     ========================================================================= */

  var OVERLAY_IDS = {
    settings: 'settingsSheet',
    discover: 'discoverSheet',
    review: 'reviewDialog',
    shortcuts: 'shortcutsDialog',
    palette: 'palette'
  };

  var FOCUSABLE_SELECTOR = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

  function overlayElement(kind) {
    return kind ? byId(OVERLAY_IDS[kind]) : null;
  }

  function activeOverlayElement() {
    return overlayElement(state.overlay);
  }

  function isVisibleElement(el) {
    if (!el) return false;
    if (el.hidden) return false;
    return !!(el.getClientRects().length || el.offsetParent !== null);
  }

  function focusablesIn(root) {
    return $$(FOCUSABLE_SELECTOR, root).filter(function (el) {
      return el instanceof HTMLElement && el.tabIndex >= 0 && isVisibleElement(el);
    });
  }

  function initialOverlayFocus(kind) {
    var map = {
      settings: 'closeSettings',
      review: 'reviewCancel',
      palette: 'paletteInput'
    };
    if (map[kind] && byId(map[kind])) return byId(map[kind]);
    var overlay = overlayElement(kind);
    if (!overlay) return null;
    var list = focusablesIn(overlay);
    return list.length ? list[0] : overlay;
  }

  function showOverlay(kind) {
    if (!OVERLAY_IDS[kind]) return;
    if (!state.overlay) overlayTrigger = doc.activeElement;
    closePopover();
    closeMenu();
    Object.keys(OVERLAY_IDS).forEach(function (name) {
      setHidden(byId(OVERLAY_IDS[name]), true);
    });
    state.overlay = kind;
    state.sheet = (kind === 'settings' || kind === 'discover') ? kind : null;
    state.dialog = (kind === 'review' || kind === 'shortcuts') ? kind : null;
    var overlay = overlayElement(kind);
    setHidden(overlay, false);
    setHidden(byId('scrim'), false);
    var app = byId('app');
    if (app) app.classList.add('has-overlay');

    if (kind === 'settings') {
      syncSettingsControls();
      renderBatches();
    } else if (kind === 'discover') {
      renderDiscover();
    } else if (kind === 'review') {
      renderReview();
    } else if (kind === 'shortcuts') {
      renderShortcutsFallback();
    } else if (kind === 'palette') {
      renderPalette();
    }

    var target = initialOverlayFocus(kind);
    if (target) {
      setTimeout(function () {
        try { target.focus(); } catch (err) { /* ignore */ }
        if (kind === 'palette') {
          var input = byId('paletteInput');
          if (input && input.select) { try { input.select(); } catch (err2) { /* ignore */ } }
        }
      }, 0);
    }
    renderToolbar();
  }

  function closeOverlay(options) {
    var opts = options || {};
    if (!state.overlay) return;
    Object.keys(OVERLAY_IDS).forEach(function (name) {
      setHidden(byId(OVERLAY_IDS[name]), true);
    });
    state.overlay = null;
    state.sheet = null;
    state.dialog = null;
    setHidden(byId('scrim'), true);
    var app = byId('app');
    if (app) app.classList.remove('has-overlay');
    var trigger = overlayTrigger;
    overlayTrigger = null;
    renderToolbar();
    if (opts.restoreFocus !== false && trigger) {
      setTimeout(function () {
        try {
          if (trigger.isConnected && !trigger.disabled) trigger.focus();
          else {
            var row = byId('sessionBody') ? byId('sessionBody').querySelector('tr.session-row[tabindex="0"]') : null;
            if (row) row.focus();
            else if (byId('searchInput')) byId('searchInput').focus();
          }
        } catch (err) { /* ignore */ }
      }, 0);
    }
  }

  function trapFocus(event) {
    var overlay = activeOverlayElement();
    if (!overlay) return;
    var focusables = focusablesIn(overlay);
    if (!focusables.length) {
      event.preventDefault();
      overlay.focus();
      return;
    }
    var first = focusables[0];
    var last = focusables[focusables.length - 1];
    var active = doc.activeElement;
    if (event.shiftKey) {
      if (active === first || !overlay.contains(active)) {
        event.preventDefault();
        last.focus();
      }
    } else if (active === last || !overlay.contains(active)) {
      event.preventDefault();
      first.focus();
    }
  }

  function renderShortcutsFallback() {
    var list = byId('shortcutsList');
    if (!list || list.children.length) return;
    var general = [
      ['命令面板', '⌘K'], ['聚焦搜索', '⌘F'], ['扫描', '⌘R'], ['设置', '⌘,'],
      ['发现', '⌘⇧D'], ['切换详情', '⌘\\'], ['撤销最近清理', '⌘Z'],
      ['快捷键表', '?'], ['关闭浮层 / 清空搜索 / 清空选择', 'Esc']
    ];
    var listKeys = [
      ['移动焦点', '↑ ↓ / J K'], ['切换选中', 'Space / X'],
      ['范围选择', 'Shift Space'], ['全选当前列表', '⌘A'],
      ['查看详情', 'Enter'], ['清理选中', '⌘⌫']
    ];
    function group(title, rows) {
      var section = h('section', { 'class': 'shortcut-group' });
      section.appendChild(h('h3', { 'class': 'shortcut-group-title', text: title }));
      rows.forEach(function (row) {
        var line = h('div', { 'class': 'shortcut-row' });
        line.appendChild(h('span', { text: row[0] }));
        line.appendChild(h('kbd', { 'class': 'kbd', text: row[1] }));
        section.appendChild(line);
      });
      return section;
    }
    list.appendChild(group('通用', general));
    list.appendChild(group('列表', listKeys));
  }

  /* =========================================================================
     17. Toasts — CONTRACT §5.12
     ========================================================================= */

  var activeToasts = [];
  var TOAST_LIFETIME = 6000;
  var TOAST_MAX = 3;

  function toast(text, options) {
    var opts = options || {};
    var container = byId('toasts');
    if (!container) return null;

    while (activeToasts.length >= TOAST_MAX) {
      removeToast(activeToasts[0], true);
    }

    var record = {
      el: null,
      timer: null,
      remaining: TOAST_LIFETIME,
      startedAt: 0,
      hovered: false
    };

    var el = h('div', {
      'class': 'toast toast-' + (opts.tone || 'info'),
      role: 'status'
    });
    var icon = h('span', { 'class': 'toast-icon', 'aria-hidden': 'true' });
    icon.appendChild(spriteIcon(opts.tone === 'error' ? 'i-alert' : (opts.tone === 'success' ? 'i-check' : 'i-info')));
    el.appendChild(icon);
    el.appendChild(h('span', { 'class': 'toast-text', text: text }));

    if (opts.actionLabel && typeof opts.onAction === 'function') {
      var action = h('button', {
        'class': 'toast-action',
        type: 'button',
        text: opts.actionLabel
      });
      action.addEventListener('click', safe(function () {
        removeToast(record);
        opts.onAction();
      }));
      el.appendChild(action);
    }

    var close = h('button', {
      'class': 'toast-close',
      type: 'button',
      'aria-label': '关闭通知'
    });
    close.appendChild(spriteIcon('i-x'));
    close.addEventListener('click', function () { removeToast(record); });
    el.appendChild(close);

    el.addEventListener('mouseenter', function () {
      record.hovered = true;
      if (record.timer) {
        clearTimeout(record.timer);
        record.timer = null;
        record.remaining -= Date.now() - record.startedAt;
        if (record.remaining < 800) record.remaining = 800;
      }
    });
    el.addEventListener('mouseleave', function () {
      record.hovered = false;
      scheduleDismiss(record);
    });

    container.appendChild(el);
    record.el = el;
    activeToasts.push(record);
    animateToastIn(el);
    scheduleDismiss(record);
    return record;
  }

  function scheduleDismiss(record) {
    if (!record || record.hovered) return;
    if (record.timer) clearTimeout(record.timer);
    record.startedAt = Date.now();
    record.timer = setTimeout(function () {
      record.timer = null;
      removeToast(record);
    }, Math.max(600, record.remaining));
  }

  function animateToastIn(el) {
    if (!el.animate || prefersReducedMotion()) return;
    try {
      el.animate(
        [{ opacity: 0, transform: 'translateY(8px)' }, { opacity: 1, transform: 'translateY(0)' }],
        { duration: 160, easing: 'cubic-bezier(.2,.8,.2,1)' }
      );
    } catch (err) { /* decorative */ }
  }

  function removeToast(record, immediate) {
    if (!record || !record.el) return;
    if (record.timer) {
      clearTimeout(record.timer);
      record.timer = null;
    }
    var index = activeToasts.indexOf(record);
    if (index !== -1) activeToasts.splice(index, 1);
    var el = record.el;
    record.el = null;
    if (immediate || !el.animate || prefersReducedMotion()) {
      if (el.parentNode) el.parentNode.removeChild(el);
      return;
    }
    try {
      var animation = el.animate(
        [{ opacity: 1, transform: 'translateY(0)' }, { opacity: 0, transform: 'translateY(8px)' }],
        { duration: 160, easing: 'cubic-bezier(.2,.8,.2,1)' }
      );
      animation.onfinish = function () {
        if (el.parentNode) el.parentNode.removeChild(el);
      };
      setTimeout(function () {
        if (el.parentNode) el.parentNode.removeChild(el);
      }, 240);
    } catch (err) {
      if (el.parentNode) el.parentNode.removeChild(el);
    }
  }

  /* =========================================================================
     18. Popovers, context menu — CONTRACT §4.7, §5.3, §5.11
     ========================================================================= */

  function positionFloating(el, anchor, align) {
    if (!el) return;
    var parent = el.offsetParent || byId('app') || byId('window') || doc.body;
    var parentRect = parent.getBoundingClientRect();
    var rect = anchor.getBoundingClientRect();
    var width = el.offsetWidth || 260;
    var height = el.offsetHeight || 300;
    var left;
    if (align === 'left') left = rect.left - parentRect.left;
    else left = rect.right - parentRect.left - width;
    var top = rect.bottom - parentRect.top + 8;
    var margin = 8;
    if (left < margin) left = margin;
    if (left + width > parentRect.width - margin) left = Math.max(margin, parentRect.width - width - margin);
    if (top + height > parentRect.height - margin) {
      top = Math.max(margin, rect.top - parentRect.top - height - 8);
    }
    el.style.position = 'absolute';
    el.style.left = Math.round(left) + 'px';
    el.style.top = Math.round(top) + 'px';
  }

  function positionAtPoint(el, clientX, clientY) {
    if (!el) return;
    var parent = el.offsetParent || byId('app') || byId('window') || doc.body;
    var parentRect = parent.getBoundingClientRect();
    var width = el.offsetWidth || 220;
    var height = el.offsetHeight || 220;
    var margin = 8;
    var left = clientX - parentRect.left;
    var top = clientY - parentRect.top;
    if (left + width > parentRect.width - margin) left = parentRect.width - width - margin;
    if (top + height > parentRect.height - margin) top = parentRect.height - height - margin;
    if (left < margin) left = margin;
    if (top < margin) top = margin;
    el.style.position = 'absolute';
    el.style.left = Math.round(left) + 'px';
    el.style.top = Math.round(top) + 'px';
  }

  function openPopover(kind, anchor) {
    var popover = byId(kind === 'filter' ? 'filterPopover' : 'quickPopover');
    var button = byId(kind === 'filter' ? 'filterBtn' : 'quickSelectBtn');
    if (!popover) return;
    if (state.popover === kind) {
      closePopover();
      return;
    }
    if (state.popover) closePopover();
    closeMenu();
    state.popover = kind;
    setHidden(popover, false);
    if (kind === 'filter') syncFilterPopover();
    else syncQuickPopoverCounts();
    positionFloating(popover, anchor || button);
    renderToolbar();
  }

  function closePopover() {
    if (!state.popover) return;
    var popover = byId(state.popover === 'filter' ? 'filterPopover' : 'quickPopover');
    setHidden(popover, true);
    state.popover = null;
    renderToolbar();
  }

  function syncQuickPopoverCounts() {
    var popover = byId('quickPopover');
    if (!popover) return;
    var visible = visibleSessions();
    var counts = {
      old: visible.filter(function (session) { return sessionAgeMs(session) > 30 * DAY; }).length,
      empty: visible.filter(function (session) { return (Number(session.messages) || 0) === 0; }).length,
      largest: Math.min(10, visible.length),
      all: visible.length,
      clear: state.selected.size
    };
    var baseLabels = {
      old: '超过 30 天',
      empty: '空会话',
      largest: '最大 10 条',
      all: '全部可见',
      clear: '清除选择'
    };
    $$('[data-quick]', popover).forEach(function (item) {
      var kind = item.dataset.quick;
      var label = $('.quick-label', item) || $('.popover-item-label', item);
      if (!label || !baseLabels[kind]) return;
      var count = counts[kind] || 0;
      setText(label, baseLabels[kind] + ' · ' + count + ' 条');
    });
  }

  function syncFilterPopover() {
    var popover = byId('filterPopover');
    if (!popover) return;
    $$('.popover-item[data-filter]', popover).forEach(function (item) {
      var group = item.dataset.filter;
      var value = item.dataset.value;
      var active = state.filter[group] === value;
      item.setAttribute('aria-checked', active ? 'true' : 'false');
      item.classList.toggle('is-active', active);
    });
  }

  function openContextMenu(uid, options) {
    var opts = options || {};
    var session = sessionByUid(uid);
    if (!session) return;
    closePopover();
    state.menu = 'context';
    state.menuUid = uid;
    state.menuReturnFocus = doc.activeElement;
    setFocus(uid);
    var menu = byId('contextMenu');
    if (!menu) return;
    var toggleLabel = $('.menu-item[data-menu="toggle"] span', menu);
    if (toggleLabel) setText(toggleLabel, state.selected.has(uid) ? '取消选中' : '选中此条');
    setHidden(menu, false);
    if (typeof opts.x === 'number' && typeof opts.y === 'number') {
      positionAtPoint(menu, opts.x, opts.y);
    } else {
      var row = byId('sessionBody') ? byId('sessionBody').querySelector('tr.session-row[data-uid="' + uid.replace(/"/g, '\\"') + '"]') : null;
      if (row) {
        var rowMore = row.querySelector('.row-more');
        var anchorRect = rowMore ? rowMore.getBoundingClientRect() : row.getBoundingClientRect();
        positionAtPoint(menu, rowMore ? anchorRect.right : anchorRect.right - 12, rowMore ? anchorRect.bottom : anchorRect.top + Math.min(anchorRect.height, 40));
      }
    }
    var first = focusablesIn(menu)[0];
    if (first) setTimeout(function () { try { first.focus(); } catch (err) { /* ignore */ } }, 0);
  }

  function closeMenu(options) {
    if (!state.menu) return;
    var menu = byId('contextMenu');
    setHidden(menu, true);
    state.menu = null;
    state.menuUid = null;
    if (!options || options.restoreFocus !== false) {
      var target = state.menuReturnFocus;
      state.menuReturnFocus = null;
      if (target && target.isConnected) {
        try { target.focus(); } catch (err) { /* ignore */ }
      }
    }
    state.menuReturnFocus = null;
  }

  function executeMenuAction(action) {
    var uid = state.menuUid;
    var session = sessionByUid(uid);
    if (!session) {
      closeMenu();
      return;
    }
    if (action === 'reveal') {
      revealPath((session.paths && session.paths[0]) || session.project || '');
      closeMenu();
    } else if (action === 'open') {
      openPath((session.paths && session.paths[0]) || session.project || '');
      closeMenu();
    } else if (action === 'copy') {
      copyPath(session.paths && session.paths[0] ? session.paths[0] : session.project);
      closeMenu();
    } else if (action === 'toggle') {
      toggleSelected(uid);
      paintRows();
      renderStatusbar();
      renderDetail();
      closeMenu();
    } else if (action === 'clean') {
      if (!state.selected.has(uid)) {
        state.selected = new Set([uid]);
      }
      closeMenu();
      if (state.prefs.confirmBeforeDelete) openReview();
      else performDelete();
    }
  }

  function handleMenuKey(event) {
    var menu = byId('contextMenu');
    if (!menu) return;
    var items = focusablesIn(menu);
    var index = items.indexOf(doc.activeElement);
    if (event.key === 'Escape') {
      event.preventDefault();
      closeMenu();
      return;
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      if (!items.length) return;
      var delta = event.key === 'ArrowDown' ? 1 : -1;
      var next = index === -1 ? (delta > 0 ? 0 : items.length - 1) : (index + delta + items.length) % items.length;
      items[next].focus();
      return;
    }
    if (event.key === 'Tab') {
      closeMenu({ restoreFocus: false });
      return;
    }
    if (event.key === 'Enter' || event.key === ' ') {
      if (index === -1) return;
      event.preventDefault();
      items[index].click();
    }
  }

  /* =========================================================================
     19. Command palette — CONTRACT §5.9
     ========================================================================= */

  function paletteActions() {
    var appearanceLabel = APPEARANCE_LABELS[state.prefs.appearance] || '自动';
    var actions = [
      { id: 'scan', label: '扫描本机会话', icon: 'i-scan', kbd: '⌘R' },
      { id: 'settings', label: '打开设置', icon: 'i-gear', kbd: '⌘,' },
      { id: 'discover', label: '发现未收录目录', icon: 'i-sparkle', kbd: '⌘⇧D' },
      { id: 'appearance', label: '切换外观（当前：' + appearanceLabel + '）', icon: 'i-shield', kbd: '' },
      { id: 'detail', label: state.detailOpen ? '收起详情面板' : '打开详情面板', icon: 'i-panel', kbd: '⌘\\' },
      { id: 'shortcuts', label: '键盘快捷键', icon: 'i-command', kbd: '?' },
      { id: 'undo', label: '撤销上次清理', icon: 'i-undo', kbd: '⌘Z' }
    ];
    var focused = currentFocusSession();
    if (focused) {
      actions.push({ id: 'open-focus', label: '用默认程序打开当前会话文件', icon: 'i-external', kbd: '' });
    }
    return actions;
  }

  function buildPaletteEntries() {
    var query = String(state.palette.query || '').trim().toLowerCase();
    var entries = [];
    var actions = paletteActions().filter(function (action) {
      return !query || action.label.toLowerCase().indexOf(query) !== -1;
    });
    var sessions = [];
    var agents = [];
    if (query) {
      sessions = state.sessions.filter(function (session) {
        return searchableText(session).indexOf(query) !== -1;
      }).slice(0, 6);
    }
    state.agents.forEach(function (agent) {
      if (agentStats(agent.id).count <= 0) return;
      if (query && agent.name.toLowerCase().indexOf(query) === -1) return;
      agents.push(agent);
    });
    if (!query) {
      agents.unshift(null); // "全部会话"
    }
    return { actions: actions, sessions: sessions, agents: agents, query: query };
  }

  function paletteAgentIcon(agent) {
    var span = h('span', { 'class': 'palette-icon palette-agent-icon', 'aria-hidden': 'true' });
    var color = brandColor(agent);
    span.style.setProperty('--agent-color', color);
    span.style.color = color;
    var icon = brandIcon(agent);
    if (icon && icon.paths) {
      var svg = doc.createElementNS('http://www.w3.org/2000/svg', 'svg');
      svg.setAttribute('viewBox', icon.viewBox || '0 0 24 24');
      svg.setAttribute('class', 'agent-svg');
      svg.setAttribute('aria-hidden', 'true');
      svg.setAttribute('fill', 'currentColor');
      svg.setAttribute('width', '12');
      svg.setAttribute('height', '12');
      icon.paths.forEach(function (d) {
        var path = doc.createElementNS('http://www.w3.org/2000/svg', 'path');
        path.setAttribute('d', d);
        svg.appendChild(path);
      });
      span.appendChild(svg);
    } else {
      span.appendChild(h('span', { 'class': 'agent-initial', text: initialOf(agent) }));
    }
    return span;
  }

  function paletteRow(entry, flatIndex) {
    var row = h('button', {
      'class': 'palette-row',
      type: 'button',
      role: 'option',
      'aria-selected': 'false',
      id: 'paletteRow' + flatIndex
    });
    row.dataset.paletteIndex = String(flatIndex);

    if (entry.type === 'session') {
      var session = entry.session;
      var icon = h('span', { 'class': 'palette-icon', 'aria-hidden': 'true' });
      icon.appendChild(spriteIcon(session.storage === 'sqlite' ? 'i-database' : 'i-file'));
      row.appendChild(icon);
      row.appendChild(h('span', { 'class': 'palette-label', text: session.title || session.id }));
      row.appendChild(h('span', {
        'class': 'palette-meta',
        text: agentName(session.agentId) + ' · ' + relativeTime(session.updatedAt)
      }));
    } else if (entry.type === 'agent') {
      row.appendChild(entry.agent ? paletteAgentIcon(entry.agent) : (function () {
        var span = h('span', { 'class': 'palette-icon', 'aria-hidden': 'true' });
        span.appendChild(spriteIcon('i-layers'));
        return span;
      })());
      row.appendChild(h('span', { 'class': 'palette-label', text: entry.agent ? entry.agent.name : '全部会话' }));
      row.appendChild(h('span', {
        'class': 'palette-meta',
        text: (entry.agent ? agentStats(entry.agent.id).count : state.sessions.length) + ' 条'
      }));
    } else {
      var actionIcon = h('span', { 'class': 'palette-icon', 'aria-hidden': 'true' });
      actionIcon.appendChild(spriteIcon(entry.action.icon));
      row.appendChild(actionIcon);
      row.appendChild(h('span', { 'class': 'palette-label', text: entry.action.label }));
      if (entry.action.kbd) row.appendChild(h('kbd', { 'class': 'kbd', text: entry.action.kbd }));
    }
    return row;
  }

  function renderPalette() {
    var results = byId('paletteResults');
    var empty = byId('paletteEmpty');
    var input = byId('paletteInput');
    if (!results) return;
    if (input && input.value !== state.palette.query) input.value = state.palette.query;

    var built = buildPaletteEntries();
    var entries = [];
    var groups = [];
    if (built.query) {
      if (built.sessions.length) groups.push({ kind: 'session', title: '会话', items: built.sessions });
    }
    if (built.actions.length) groups.push({ kind: 'action', title: '动作', items: built.actions });
    if (built.agents.length) groups.push({ kind: 'agent', title: 'Agent', items: built.agents });

    clear(results);
    var flat = 0;
    groups.forEach(function (group) {
      var groupEl = h('div', { 'class': 'palette-group' });
      groupEl.appendChild(h('div', { 'class': 'palette-group-title', text: group.title }));
      group.items.forEach(function (item) {
        var entry;
        if (group.kind === 'session') entry = { type: 'session', session: item };
        else if (group.kind === 'agent') entry = { type: 'agent', agent: item };
        else entry = { type: 'action', action: item };
        entry.flatIndex = flat;
        entries.push(entry);
        groupEl.appendChild(paletteRow(entry, flat));
        flat += 1;
      });
      results.appendChild(groupEl);
    });

    state.palette.entries = entries;
    setHidden(results, entries.length === 0);
    setHidden(empty, entries.length > 0);
    if (entries.length) setPaletteActive(Math.min(state.palette.index, entries.length - 1));
    else {
      state.palette.index = 0;
      if (input) input.removeAttribute('aria-activedescendant');
    }
  }

  function setPaletteActive(index) {
    var entries = state.palette.entries;
    if (!entries.length) return;
    if (index < 0) index = entries.length - 1;
    if (index >= entries.length) index = 0;
    state.palette.index = index;
    var rows = $$('#paletteResults .palette-row');
    rows.forEach(function (row, rowIndex) {
      var active = rowIndex === index;
      row.classList.toggle('is-active', active);
      row.setAttribute('aria-selected', active ? 'true' : 'false');
      if (active && row.scrollIntoView) {
        try { row.scrollIntoView({ block: 'nearest' }); } catch (err) { row.scrollIntoView(); }
      }
    });
    var input = byId('paletteInput');
    if (input && rows[index]) input.setAttribute('aria-activedescendant', rows[index].id || '');
  }

  function executePaletteEntry(entry) {
    if (!entry) return;
    if (entry.type === 'session') {
      var uid = entry.session.uid;
      closeOverlay({ restoreFocus: false });
      ensureFocusVisible();
      setFocus(uid, { dom: true, scroll: true });
      if (!state.detailOpen) { state.detailOpen = true; syncDetail(); }
      renderDetail();
      return;
    }
    if (entry.type === 'agent') {
      var agentId = entry.agent ? entry.agent.id : 'all';
      closeOverlay({ restoreFocus: false });
      selectAgent(agentId);
      return;
    }
    var id = entry.action.id;
    closeOverlay({ restoreFocus: false });
    if (id === 'scan') {
      startScan();
    } else if (id === 'settings') {
      openSettings();
    } else if (id === 'discover') {
      openDiscover();
    } else if (id === 'appearance') {
      var order = ['auto', 'light', 'dark'];
      var next = order[(order.indexOf(state.prefs.appearance) + 1) % order.length];
      setAppearance(next);
      toast('外观：' + ({ auto: '自动', light: '浅色', dark: '深色' }[next]));
    } else if (id === 'detail') {
      toggleDetail();
    } else if (id === 'shortcuts') {
      openShortcuts();
    } else if (id === 'undo') {
      performUndo();
    } else if (id === 'open-focus') {
      var focusedSession = currentFocusSession();
      if (!focusedSession) {
        toast('先聚焦一条会话');
        return;
      }
      openPath((focusedSession.paths && focusedSession.paths[0]) || focusedSession.project || '');
    }
  }

  function openPalette() {
    state.palette.query = '';
    state.palette.index = 0;
    state.palette.entries = [];
    var input = byId('paletteInput');
    if (input) input.value = '';
    closePopover();
    closeMenu();
    renderPalette();
    showOverlay('palette');
    var field = byId('paletteInput');
    if (field) {
      setTimeout(function () {
        try { field.focus(); field.select(); } catch (err) { /* ignore */ }
      }, 0);
    }
  }

  /* =========================================================================
     20. Scans — POST /api/scan + GET /api/scan polling (CONTRACT §5.2)
     ========================================================================= */

  function normalizeScan(raw) {
    var source = raw && typeof raw === 'object' ? raw : {};
    var status = ['idle', 'running', 'done', 'error'].indexOf(source.status) !== -1
      ? source.status
      : 'idle';
    var progress = source.progress && typeof source.progress === 'object' ? source.progress : {};
    return {
      status: status,
      startedAt: typeof source.startedAt === 'string' ? source.startedAt : '',
      finishedAt: typeof source.finishedAt === 'string' ? source.finishedAt : '',
      durationMs: Number(source.durationMs) || 0,
      progress: {
        done: Number(progress.done) || 0,
        total: Number(progress.total) || 0,
        current: typeof progress.current === 'string' ? progress.current : ''
      },
      issues: Array.isArray(source.issues) ? source.issues : []
    };
  }

  function scanCurrentAgentId() {
    var current = state.scan.progress ? state.scan.progress.current : '';
    if (!current) return null;
    for (var i = 0; i < state.agents.length; i++) {
      var agent = state.agents[i];
      if (agent && (agent.id === current || agent.name === current)) return agent.id;
    }
    return null;
  }

  function paintScanningAgent(agentId) {
    var wrap = byId('agentGroups');
    if (!wrap) return;
    $$('.agent-row', wrap).forEach(function (row) {
      row.classList.toggle('is-scanning', !!agentId && row.dataset.agent === agentId);
    });
  }

  function stopScanPolling() {
    if (timers.scanPoll) {
      clearTimeout(timers.scanPoll);
      timers.scanPoll = null;
    }
  }

  function applyScanStatus(next) {
    state.scan = next;
    paintScanningAgent(next.status === 'running' ? scanCurrentAgentId() : null);
    renderToolbar();
    renderStatusbar();
    renderIssueBanner();
  }

  function scheduleScanPoll(delay) {
    stopScanPolling();
    timers.scanPoll = setTimeout(function () {
      timers.scanPoll = null;
      api('GET', '/api/scan').then(function (raw) {
        var next = normalizeScan(raw);
        applyScanStatus(next);
        if (next.status === 'running') {
          scheduleScanPoll(POLL_INTERVAL);
        } else {
          finishScan();
        }
      }, function (err) {
        state.scan.status = 'error';
        state.scan.issues = [];
        renderToolbar();
        renderStatusbar();
        toast('读取扫描状态失败：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
      });
    }, delay || 0);
  }

  function startScan() {
    if (state.boot.status !== 'ready') {
      reloadBootstrap();
      return;
    }
    if (state.scan.status === 'running' || state.scanStarting) return;
    state.scanStarting = true;
    state.scan.status = 'running';
    state.scan.issues = [];
    state.scan.durationMs = 0;
    state.scan.progress = { done: 0, total: 0, current: '准备中' };
    state.issueExpanded = false;
    renderAll();
    paintScanningAgent(null);
    api('POST', '/api/scan').then(function () {
      state.scanStarting = false;
      scheduleScanPoll(POLL_INTERVAL);
    }, function (err) {
      state.scanStarting = false;
      state.scan.status = 'error';
      state.scan.issues = [];
      renderAll();
      toast('扫描没有启动：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  function finishScan() {
    stopScanPolling();
    paintScanningAgent(null);
    refreshBootstrap({ preserveFocus: true }).then(function () {
      var totals = totalStats();
      toast('扫描完成 · ' + totals.count + ' 条会话 · ' + formatBytes(totals.bytes));
    }, function () { /* refreshBootstrap already surfaced the error */ });
  }

  function retryIssue(agentId) {
    toast('正在重新扫描' + (agentId ? '（重试 ' + agentName(agentId) + '）' : '') + '…');
    startScan();
  }

  /* =========================================================================
     21. Selection helpers — CONTRACT §5.4
     ========================================================================= */

  function addVisibleSelection(uids, appendSelection) {
    if (!appendSelection) state.selected.clear();
    uids.forEach(function (uid) { state.selected.add(uid); });
    state.anchorUid = uids.length ? uids[0] : state.anchorUid;
  }

  function toggleSelected(uid) {
    if (!uid) return;
    if (state.selected.has(uid)) state.selected.delete(uid);
    else state.selected.add(uid);
    state.anchorUid = uid;
  }

  function selectRange(uid) {
    var visible = visibleSessions();
    if (!visible.length) return;
    var anchor = state.anchorUid || state.focusUid || uid;
    var from = visible.findIndex(function (session) { return session.uid === anchor; });
    var to = visible.findIndex(function (session) { return session.uid === uid; });
    if (from === -1 || to === -1) {
      toggleSelected(uid);
      return;
    }
    var start = Math.min(from, to);
    var end = Math.max(from, to);
    for (var i = start; i <= end; i++) state.selected.add(visible[i].uid);
    state.anchorUid = anchor;
  }

  function selectAllVisible() {
    var visible = visibleSessions();
    visible.forEach(function (session) { state.selected.add(session.uid); });
    if (!state.anchorUid && visible.length) state.anchorUid = visible[0].uid;
    paintRows();
    renderStatusbar();
    renderDetail();
    updateSelectAll(visible);
  }

  function clearSelection() {
    state.selected.clear();
    state.anchorUid = null;
    paintRows();
    renderStatusbar();
    renderDetail();
    updateSelectAll(visibleSessions());
  }

  function setFocus(uid, options) {
    if (!sessionByUid(uid)) return;
    var opts = options || {};
    state.focusUid = uid;
    state.focusSuppressed = false;
    paintRows();
    renderDetail();
    if (opts.dom) focusRowDom(uid, opts.scroll !== false);
  }

  function moveFocus(delta) {
    var visible = visibleSessions();
    if (!visible.length) return;
    var index = visible.findIndex(function (session) { return session.uid === state.focusUid; });
    if (index === -1) index = delta > 0 ? -1 : 0;
    var next = Math.max(0, Math.min(visible.length - 1, index + delta));
    setFocus(visible[next].uid, { dom: true, scroll: true });
  }

  function applyQuickSelect(kind) {
    var visible = visibleSessions();
    var uids = [];
    if (kind === 'clear') {
      clearSelection();
      closePopover();
      return;
    }
    if (kind === 'all') {
      uids = visible.map(function (session) { return session.uid; });
    } else if (kind === 'old') {
      uids = visible.filter(function (session) { return sessionAgeMs(session) > 30 * DAY; })
        .map(function (session) { return session.uid; });
    } else if (kind === 'empty') {
      uids = visible.filter(function (session) { return (Number(session.messages) || 0) === 0; })
        .map(function (session) { return session.uid; });
    } else if (kind === 'largest') {
      uids = visible.slice().sort(function (a, b) { return (Number(b.bytes) || 0) - (Number(a.bytes) || 0); })
        .slice(0, 10)
        .map(function (session) { return session.uid; });
    }
    if (!uids.length) {
      toast('没有符合条件的会话');
      closePopover();
      return;
    }
    addVisibleSelection(uids, false);
    paintRows();
    renderStatusbar();
    renderDetail();
    updateSelectAll(visible);
    closePopover();
    toast('已选择 ' + uids.length + ' 条会话');
  }

  /* =========================================================================
     22. Clean / delete / undo — CONTRACT §5.5
     ========================================================================= */

  function reportDeleteErrors(errors, labels) {
    if (!errors || !errors.length) return;
    var shown = 0;
    errors.slice(0, 3).forEach(function (item) {
      var label = (item && labels[item.uid]) || (item && item.uid) || '会话';
      toast('未能清理「' + label + '」：' + ((item && item.message) || '未知错误'), { tone: 'error' });
      shown += 1;
    });
    if (errors.length > shown) {
      toast('另有 ' + (errors.length - shown) + ' 条未能清理', { tone: 'error' });
    }
  }

  function performDelete() {
    if (state.deleting || state.undoBusy) return;
    var selected = selectedSessions();
    if (!selected.length) return;
    var uids = selected.map(function (session) { return session.uid; });
    var labels = {};
    selected.forEach(function (session) {
      labels[session.uid] = session.title || session.id || session.uid;
    });

    var visibleBefore = visibleSessions();
    var focusIndex = visibleBefore.findIndex(function (session) {
      return session.uid === state.focusUid;
    });

    closeOverlay({ restoreFocus: false });
    state.deleting = true;
    state.reviewError = '';
    renderStatusbar();
    renderCleanButton();

    api('POST', '/api/delete', { uids: uids }).then(function (result) {
      var errors = result && Array.isArray(result.errors) ? result.errors : [];
      var failed = {};
      errors.forEach(function (item) {
        if (item && item.uid) failed[item.uid] = true;
      });
      var succeeded = uids.filter(function (uid) { return !failed[uid]; });
      var succeededSet = {};
      succeeded.forEach(function (uid) { succeededSet[uid] = true; });

      /* Reflect the authoritative server result immediately; the refresh
         below only reconciles batches and agent counters. */
      state.sessions = state.sessions.filter(function (session) {
        return !succeededSet[session.uid];
      });
      var nextSelected = new Set();
      uids.forEach(function (uid) {
        if (failed[uid] && sessionByUid(uid)) nextSelected.add(uid);
      });
      state.selected = nextSelected;
      state.anchorUid = null;
      state.deleting = false;

      if (!state.focusUid || !sessionByUid(state.focusUid)) {
        var visibleAfter = visibleSessions();
        if (visibleAfter.length) {
          var nextIndex = focusIndex >= 0 ? Math.min(focusIndex, visibleAfter.length - 1) : 0;
          state.focusUid = visibleAfter[nextIndex].uid;
          state.focusSuppressed = false;
        } else {
          state.focusUid = null;
        }
      }

      var batchIds = result && Array.isArray(result.batchIds)
        ? result.batchIds.filter(function (id) { return !!id; })
        : [];
      var count = Number(result && result.count) || 0;
      var freed = Number(result && result.freed) || 0;
      state.lastDelete = { ids: batchIds, count: count, freed: freed, at: Date.now() };
      renderAll();

      refreshBootstrap({ preserveFocus: true, silent: true }).then(function () {
        var latest = latestUndoableBatch();
        if (count > 0) {
          var text = '已清理 ' + count + ' 条 · 真实释放 ' + formatBytes(freed);
          if (latest && latest.undoable) {
            toast(text, {
              actionLabel: '撤销',
              onAction: function () { performUndo(); }
            });
          } else {
            toast(text);
          }
        } else if (!errors.length) {
          toast('没有清理任何会话');
        }
        reportDeleteErrors(errors, labels);
      }, function () {
        if (count > 0) toast('已清理 ' + count + ' 条 · 真实释放 ' + formatBytes(freed));
        reportDeleteErrors(errors, labels);
      });
    }, function (err) {
      state.deleting = false;
      state.reviewError = err && err.message ? err.message : '未知错误';
      renderAll();
      toast('清理失败：' + state.reviewError, { tone: 'error' });
    });
  }

  function performUndo(batchId) {
    if (state.deleting || state.undoBusy) return;
    var ids = [];
    if (batchId) ids = [batchId];
    else ids = pendingUndoBatchIds();
    if (!ids.length) {
      var latest = latestUndoableBatch();
      if (latest && latest.undoable) ids = [latest.id];
    }
    if (!ids.length) {
      toast('没有可撤销的清理');
      return;
    }
    var allUndoable = ids.every(function (id) {
      var found = false;
      state.batches.forEach(function (batch) {
        if (batch.id === id && batch.undoable) found = true;
      });
      return found;
    });
    if (!allUndoable) {
      toast('这个批次已经不可撤销');
      return;
    }

    state.undoBusy = true;
    renderAll();
    api('POST', '/api/undo', { batchId: ids[0], batchIds: ids }).then(function (result) {
      state.undoBusy = false;
      var restored = Number(result && result.restored) || 0;
      var errors = result && Array.isArray(result.errors) ? result.errors : [];
      state.lastDelete = { ids: [], count: 0, freed: 0, at: 0 };
      refreshBootstrap({ preserveFocus: true, silent: true }).then(function () {
        renderAll();
        toast('已恢复 ' + restored + ' 条会话');
        errors.slice(0, 2).forEach(function (message) {
          toast('撤销时有文件未恢复：' + message, { tone: 'error' });
        });
        /* GET /api/bootstrap 的内存会话不会随撤销更新，重新扫描一次让恢复
           的会话回到列表（扫描只读，安全）。 */
        if (restored > 0) startScan();
      }, function () {
        renderAll();
        toast('已撤销，但列表刷新失败', { tone: 'error' });
      });
    }, function (err) {
      state.undoBusy = false;
      renderAll();
      toast('撤销失败：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  /* =========================================================================
     23. Clipboard + reveal — CONTRACT §5.6
     ========================================================================= */

  function legacyCopy(text) {
    var area = doc.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', 'readonly');
    area.style.position = 'fixed';
    area.style.top = '-1000px';
    area.style.left = '-1000px';
    area.style.opacity = '0';
    doc.body.appendChild(area);
    var copied = false;
    try {
      area.select();
      copied = doc.execCommand('copy');
    } catch (err) {
      copied = false;
    }
    if (area.parentNode) area.parentNode.removeChild(area);
    toast(copied ? '已复制路径' : '剪贴板不可用，请从详情面板手动复制路径');
  }

  function copyPath(path) {
    if (!path) {
      toast('这条会话没有可复制的路径');
      return;
    }
    try {
      if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
        navigator.clipboard.writeText(String(path)).then(function () {
          toast('已复制路径');
        }, function () {
          legacyCopy(String(path));
        });
        return;
      }
    } catch (err) {
      /* fall through to legacy copy */
    }
    legacyCopy(String(path));
  }

  function revealPath(path) {
    if (!path) {
      toast('这条会话没有可显示的文件路径');
      return;
    }
    api('POST', '/api/reveal', { path: String(path) }).then(function () {}, function (err) {
      toast('无法在 Finder 中显示：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  function openPath(path) {
    if (!path) {
      toast('这条会话没有可打开的文件路径');
      return;
    }
    api('POST', '/api/open', { path: String(path) }).then(function () {}, function (err) {
      toast('无法打开：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  /* =========================================================================
     24. HTTP API + bootstrap
     ========================================================================= */

  function apiError(message, status) {
    var err = new Error(message);
    err.name = 'ApiError';
    err.status = status || 0;
    return err;
  }

  function api(method, path, body) {
    var options = { method: method, headers: { Accept: 'application/json' } };
    if (body !== undefined) {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(body);
    }
    return fetch(path, options).then(function (response) {
      var type = response.headers.get('content-type') || '';
      var isJSON = type.indexOf('application/json') !== -1;
      var parsed = isJSON
        ? response.json().catch(function () { return null; })
        : Promise.resolve(null);
      return parsed.then(function (data) {
        if (!response.ok) {
          throw apiError((data && data.error) || ('请求失败（HTTP ' + response.status + '）'), response.status);
        }
        if (!isJSON) {
          throw apiError('本地服务返回了非 JSON 响应（可能没有通过 AgentSweep 启动）', response.status);
        }
        return data;
      });
    }, function () {
      throw apiError('无法连接本地服务', 0);
    });
  }

  function normalizePrefs(raw) {
    var source = raw && typeof raw === 'object' ? raw : {};
    return {
      deleteMode: source.deleteMode === 'permanent' ? 'permanent' : 'trash',
      confirmBeforeDelete: source.confirmBeforeDelete !== false,
      includeSnapshots: source.includeSnapshots !== false,
      autoScan: source.autoScan !== false,
      autoCheckUpdates: source.autoCheckUpdates !== false,
      appearance: state.prefs.appearance
    };
  }

  function highlightAgentRow(agentId) {
    if (!agentId) return;
    var row = byId('agentGroups')
      ? byId('agentGroups').querySelector('.agent-row[data-agent="' + agentId.replace(/"/g, '\\"') + '"]')
      : null;
    if (!row) return;
    row.classList.add('is-new');
    if (timers.highlight) clearTimeout(timers.highlight);
    timers.highlight = setTimeout(function () {
      timers.highlight = null;
      if (row && row.isConnected) row.classList.remove('is-new');
    }, 1600);
    if (row.scrollIntoView) {
      try { row.scrollIntoView({ block: 'nearest' }); } catch (err) { row.scrollIntoView(); }
    }
  }

  function applyBootstrap(data, options) {
    var opts = options || {};
    var appearance = state.prefs.appearance;
    state.prefs = normalizePrefs(data.prefs);
    state.prefs.appearance = appearance;
    state.version = typeof data.version === 'string' ? data.version : state.version;
    state.home = typeof data.home === 'string' ? data.home : '';
    state.platform = typeof data.platform === 'string' ? data.platform : '';
    state.agents = Array.isArray(data.agents) ? data.agents : [];
    state.sessions = Array.isArray(data.sessions) ? data.sessions : [];
    state.batches = Array.isArray(data.batches) ? data.batches : [];
    state.scan = normalizeScan(data.scan);

    if (state.filter.agentId !== 'all') {
      var filteredAgent = agentById(state.filter.agentId);
      if (!filteredAgent || agentStats(filteredAgent.id).count === 0) {
        state.filter.agentId = 'all';
      }
    }

    var existing = {};
    state.sessions.forEach(function (session) { existing[session.uid] = true; });
    var nextSelected = new Set();
    state.selected.forEach(function (uid) {
      if (existing[uid]) nextSelected.add(uid);
    });
    state.selected = nextSelected;
    if (state.anchorUid && !existing[state.anchorUid]) state.anchorUid = null;
    if (state.focusUid && !existing[state.focusUid]) state.focusUid = null;
    /* Focus (not selection) drives the detail pane. After the first scan, the
       sessions arrive via a refresh, so always restore a focus when one is
       missing unless the user explicitly pressed Esc to clear it. */
    if (!state.focusUid && state.sessions.length && !state.focusSuppressed) {
      focusFirstVisible();
    }

    state.boot.status = 'ready';
    state.boot.error = '';
    renderAll();
    if (state.overlay === 'settings') renderBatches();
    if (state.overlay === 'review') renderReview();
    if (state.overlay === 'discover') renderDiscover();

    if (state.highlightAgentId) {
      var highlightId = state.highlightAgentId;
      state.highlightAgentId = null;
      highlightAgentRow(highlightId);
    }

    if (state.scan.status === 'running') {
      scheduleScanPoll(POLL_INTERVAL);
    } else if (state.scan.status === 'idle' && state.prefs.autoScan && !state.autoScanStarted) {
      state.autoScanStarted = true;
      startScan();
    }
  }

  function loadBootstrap(options) {
    var opts = options || {};
    if (opts.initial) {
      state.boot.status = 'loading';
      state.boot.error = '';
      renderAll();
    }
    return api('GET', '/api/bootstrap').then(function (data) {
      if (!data || typeof data !== 'object') throw apiError('本地服务返回了空响应');
      applyBootstrap(data, { preserveFocus: opts.preserveFocus === true });
      return data;
    }, function (err) {
      var message = err && err.message ? err.message : '未知错误';
      if (opts.initial || state.boot.status !== 'ready') {
        state.boot.status = 'error';
        state.boot.error = message;
        renderAll();
        toast('无法连接本地服务：' + message, {
          tone: 'error',
          actionLabel: '重试',
          onAction: reloadBootstrap
        });
      } else if (!opts.silent) {
        toast('刷新列表失败：' + message, { tone: 'error' });
      }
      throw err;
    });
  }

  function refreshBootstrap(options) {
    var opts = options || {};
    return loadBootstrap({
      preserveFocus: opts.preserveFocus !== false,
      silent: opts.silent === true
    });
  }

  function reloadBootstrap() {
    loadBootstrap({ initial: true }).then(function () {}, function () {});
  }

  function setPrefs(patch) {
    var seq = ++state.prefsSeq;
    var previous = {};
    Object.keys(patch).forEach(function (key) {
      previous[key] = state.prefs[key];
      state.prefs[key] = patch[key];
    });
    renderAll();
    api('PUT', '/api/prefs', patch).then(function (prefs) {
      if (seq !== state.prefsSeq) return;
      var appearance = state.prefs.appearance;
      state.prefs = normalizePrefs(prefs);
      state.prefs.appearance = appearance;
      renderAll();
      if (state.overlay === 'review') renderReview();
    }, function (err) {
      if (seq !== state.prefsSeq) return;
      Object.keys(previous).forEach(function (key) {
        state.prefs[key] = previous[key];
      });
      renderAll();
      if (state.overlay === 'review') renderReview();
      toast('设置没有保存：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  function backupDirPath() {
    if (state.platform !== 'darwin' || !state.home) return '';
    return state.home + '/Library/Application Support/AgentSweep/backups';
  }

  function revealBackups() {
    var path = backupDirPath();
    if (!path) {
      toast('找不到备份目录（仅在 macOS 本机上可用）');
      return;
    }
    api('POST', '/api/reveal', { path: path }).then(function () {}, function (err) {
      toast('暂时打不开备份目录：' + (err && err.message ? err.message : '未知错误'), { tone: 'error' });
    });
  }

  function ensureBrandAssets() {
    if (window.AGENT_BRAND) {
      BRAND = window.AGENT_BRAND;
      return;
    }
    var script = doc.createElement('script');
    script.src = 'brand-icons.js';
    script.onload = function () {
      if (window.AGENT_BRAND) {
        BRAND = window.AGENT_BRAND;
        renderAll();
      }
    };
    script.onerror = function () { /* initials are still a valid fallback */ };
    doc.head.appendChild(script);
  }

  function openSettings() {
    syncSettingsControls();
    renderBatches();
    renderUpdate();
    if (state.overlay !== 'settings') showOverlay('settings');
    refreshUpdate({ silent: true });
  }

  function openShortcuts() {
    renderShortcutsFallback();
    showOverlay('shortcuts');
  }

  /* =========================================================================
     25. Search / filters / sort — CONTRACT §5.3
     ========================================================================= */

  function clearSearch(options) {
    var opts = options || {};
    state.filter.query = '';
    syncSearchInput();
    ensureFocusVisible();
    renderToolbar();
    renderTable();
    renderDetail();
    renderStatusbar();
    renderFilterChips();
    if (opts.focus && byId('searchInput')) byId('searchInput').focus();
    else if (opts.blur && doc.activeElement === byId('searchInput')) byId('searchInput').blur();
  }

  function resetFilters(includeAgent) {
    state.filter.storage = 'all';
    state.filter.age = 'all';
    state.filter.size = 'all';
    if (includeAgent) state.filter.agentId = 'all';
    ensureFocusVisible();
    renderToolbar();
    renderTable();
    renderDetail();
    renderFilterChips();
    renderStatusbar();
    syncFilterPopover();
    if (includeAgent) renderSidebar();
  }

  function applyFilterValue(group, value) {
    if (['storage', 'age', 'size'].indexOf(group) === -1) return;
    state.filter[group] = value;
    ensureFocusVisible();
    renderToolbar();
    renderTable();
    renderDetail();
    renderFilterChips();
    renderStatusbar();
    syncFilterPopover();
  }

  function applySort(key) {
    if (state.sort.key === key) {
      state.sort.dir = state.sort.dir === 'asc' ? 'desc' : 'asc';
    } else {
      state.sort.key = key;
      state.sort.dir = key === 'title' ? 'asc' : 'desc';
    }
    applySortUi();
    renderTable();
    renderToolbar();
    scrollFocusIntoView();
  }

  function applySortUi() {
    $$('.sort-btn[data-sort]').forEach(function (button) {
      var th = button.closest('th');
      var active = button.dataset.sort === state.sort.key;
      if (th) {
        th.setAttribute('aria-sort', active ? (state.sort.dir === 'asc' ? 'ascending' : 'descending') : 'none');
      }
      button.classList.toggle('is-sorted', active);
      button.classList.toggle('is-desc', active && state.sort.dir === 'desc');
    });
  }

  function toggleDensity() {
    state.density = state.density === 'compact' ? 'comfortable' : 'compact';
    writeStored(LS_DENSITY, state.density);
    syncDensity();
    renderTable();
  }

  /* =========================================================================
     26. Event wiring
     ========================================================================= */

  function bindEvents() {
    /* top bar */
    bind(byId('searchInput'), 'input', function (event) {
      state.filter.query = event.target.value;
      syncSearchInput();
      ensureFocusVisible();
      renderToolbar();
      renderTable();
      renderDetail();
      renderStatusbar();
    });
    bind(byId('searchClear'), 'click', function () {
      clearSearch({ focus: true });
    });
    bind(byId('brandHome'), 'click', function () {
      state.filter = clone(DEFAULT_FILTER);
      state.sort = clone(DEFAULT_SORT);
      syncSearchInput();
      state.anchorUid = null;
      focusFirstVisible();
      closePopover();
      closeMenu();
      renderAll();
      applySortUi();
    });
    bind(byId('paletteBtn'), 'click', openPalette);
    bind(byId('discoverBtn'), 'click', openDiscover);
    bind(byId('settingsBtn'), 'click', openSettings);
    bind(byId('detailToggle'), 'click', function () { toggleDetail(); });
    bind(byId('scanBtn'), 'click', startScan);
    bind(byId('scanIssuesBtn'), 'click', toggleIssues);
    bind(byId('scanStatus'), 'click', function (event) {
      var link = event.target.closest ? event.target.closest('[data-status-action="issues"]') : null;
      if (link) toggleIssues();
    });
    bind(byId('detailClose'), 'click', function () {
      toggleDetail(false);
      if (state.focusUid) {
        var row = byId('sessionBody') ? byId('sessionBody').querySelector('tr.session-row[data-uid="' + state.focusUid.replace(/"/g, '\\"') + '"]') : null;
        if (row) try { row.focus(); } catch (err) { /* ignore */ }
      }
    });
    bind(byId('undoChip'), 'click', function () {
      performUndo();
    });
    bind(byId('cleanBtn'), 'click', function () {
      if (state.deleting || !selectedSessions().length) return;
      if (state.prefs.confirmBeforeDelete) openReview();
      else performDelete();
    });

    /* list toolbar */
    bind(byId('agentAll'), 'click', function () { selectAgent('all'); });
    bind(byId('filterBtn'), 'click', function () {
      openPopover('filter', byId('filterBtn'));
    });
    bind(byId('quickSelectBtn'), 'click', function () {
      openPopover('quick', byId('quickSelectBtn'));
    });
    bind(byId('densityBtn'), 'click', toggleDensity);
    $$('.sort-btn[data-sort]').forEach(function (button) {
      bind(button, 'click', function () { applySort(button.dataset.sort); });
    });
    bind(byId('selectAll'), 'change', function () {
      var visible = visibleSessions();
      var checked = byId('selectAll').checked;
      visible.forEach(function (session) {
        if (checked) state.selected.add(session.uid);
        else state.selected.delete(session.uid);
      });
      if (checked && visible.length && !state.anchorUid) state.anchorUid = visible[0].uid;
      paintRows();
      renderStatusbar();
      renderDetail();
      updateSelectAll(visible);
    });

    /* table interaction (event delegation) */
    var tbody = byId('sessionBody');
    bind(tbody, 'click', function (event) {
      var row = event.target.closest ? event.target.closest('tr.session-row') : null;
      if (!row) return;
      var uid = row.dataset.uid;
      if (event.target.closest('.row-more')) {
        openContextMenu(uid, {
          x: event.clientX || 0,
          y: event.clientY || 0
        });
        return;
      }
      if (event.target.closest('.row-check')) {
        if (event.shiftKey) selectRange(uid);
        else toggleSelected(uid);
        state.focusUid = uid;
        state.focusSuppressed = false;
        paintRows();
        renderDetail();
        renderStatusbar();
        updateSelectAll(visibleSessions());
        return;
      }
      state.anchorUid = state.anchorUid || uid;
      setFocus(uid);
      var focusedRow = byId('sessionBody').querySelector('tr.session-row[data-uid="' + uid.replace(/"/g, '\\"') + '"]');
      if (focusedRow) {
        try { focusedRow.focus({ preventScroll: true }); } catch (err) { focusedRow.focus(); }
      }
    });
    bind(tbody, 'dblclick', function (event) {
      var row = event.target.closest ? event.target.closest('tr.session-row') : null;
      if (!row) return;
      state.focusUid = row.dataset.uid;
      state.detailOpen = true;
      syncDetail();
      renderDetail();
      paintRows();
    });
    bind(tbody, 'contextmenu', function (event) {
      var row = event.target.closest ? event.target.closest('tr.session-row') : null;
      if (!row) return;
      event.preventDefault();
      openContextMenu(row.dataset.uid, { x: event.clientX, y: event.clientY });
    });

    /* empty state action */
    bind(byId('emptyAction'), 'click', function () {
      var action = byId('emptyAction').dataset.action || 'scan';
      if (action === 'retry') reloadBootstrap();
      else if (action === 'scan') startScan();
      else if (action === 'clear-search') clearSearch({ focus: true });
      else if (action === 'reset-filters') resetFilters(true);
      else if (action === 'all') {
        state.filter = clone(DEFAULT_FILTER);
        state.sort = clone(DEFAULT_SORT);
        syncSearchInput();
        state.anchorUid = null;
        focusFirstVisible();
        renderAll();
        applySortUi();
      }
    });

    /* detail actions */
    bind(byId('detailBody'), 'click', function (event) {
      var button = event.target.closest ? event.target.closest('[data-path-action]') : null;
      if (!button) return;
      var action = button.dataset.pathAction;
      if (action === 'copy') copyPath(button.dataset.path);
      else if (action === 'reveal') revealPath(button.dataset.path);
      else if (action === 'open') openPath(button.dataset.path);
    });
    bind(byId('detailReveal'), 'click', function () {
      var session = currentFocusSession();
      var path = session
        ? ((session.paths && session.paths[0]) || session.project || '')
        : '';
      revealPath(path);
    });
    bind(byId('detailSelect'), 'click', function () {
      if (!state.focusUid) return;
      toggleSelected(state.focusUid);
      paintRows();
      renderStatusbar();
      renderDetail();
      updateSelectAll(visibleSessions());
    });

    /* filter chips + popovers */
    bind(byId('filterChips'), 'click', function (event) {
      var button = event.target.closest ? event.target.closest('[data-chip]') : null;
      if (!button) return;
      var kind = button.dataset.chip;
      if (kind === 'agent') {
        state.filter.agentId = 'all';
        ensureFocusVisible();
        renderSidebar();
      } else {
        state.filter[kind] = 'all';
        ensureFocusVisible();
      }
      renderToolbar();
      renderTable();
      renderDetail();
      renderFilterChips();
      renderStatusbar();
      syncFilterPopover();
    });
    bind(byId('filterPopover'), 'click', function (event) {
      var item = event.target.closest ? event.target.closest('.popover-item[data-filter]') : null;
      if (item) {
        applyFilterValue(item.dataset.filter, item.dataset.value);
        return;
      }
      if (event.target.closest && event.target.closest('#filterReset')) resetFilters(false);
    });
    bind(byId('quickPopover'), 'click', function (event) {
      var item = event.target.closest ? event.target.closest('[data-quick]') : null;
      if (!item) return;
      applyQuickSelect(item.dataset.quick);
    });
    bind(byId('contextMenu'), 'click', function (event) {
      var item = event.target.closest ? event.target.closest('.menu-item[data-menu]') : null;
      if (!item) return;
      executeMenuAction(item.dataset.menu);
    });

    /* batch list, issue banner, discover */
    bind(byId('batchList'), 'click', function (event) {
      var button = event.target.closest ? event.target.closest('[data-undo]') : null;
      if (!button) return;
      performUndo(button.dataset.undo);
    });
    bind(byId('issueBanner'), 'click', function (event) {
      if (event.target.closest && event.target.closest('#issueBannerBtn')) {
        toggleIssues();
        return;
      }
      var retry = event.target.closest ? event.target.closest('[data-retry]') : null;
      if (retry) retryIssue(retry.dataset.retry);
    });
    bind(byId('discoverList'), 'click', function (event) {
      var button = event.target.closest ? event.target.closest('[data-candidate-path]') : null;
      if (!button) return;
      adoptCandidate(button.dataset.candidatePath);
    });
    bind(byId('rediscoverBtn'), 'click', function () {
      startDiscoverLoad({ force: true });
    });

    /* overlays */
    bind(byId('scrim'), 'click', function () { closeOverlay(); });
    doc.addEventListener('click', safe(function (event) {
      var closer = event.target.closest ? event.target.closest('[data-close-sheet],[data-close-dialog]') : null;
      if (closer) closeOverlay();
      var cancel = event.target.closest ? event.target.closest('#reviewCancel') : null;
      if (cancel) closeOverlay();
    }));

    /* settings: server prefs go through PUT /api/prefs, appearance stays local */
    $$('input[name="deleteMode"]').forEach(function (input) {
      bind(input, 'change', function () {
        if (!input.checked) return;
        setPrefs({ deleteMode: input.value });
        if (state.overlay === 'review') renderReview();
      });
    });
    $$('input[name="appearance"]').forEach(function (input) {
      bind(input, 'change', function () {
        if (!input.checked) return;
        setAppearance(input.value);
      });
    });
    bind(byId('prefConfirm'), 'change', function (event) {
      setPrefs({ confirmBeforeDelete: !!event.target.checked });
    });
    bind(byId('prefSnapshots'), 'change', function (event) {
      setPrefs({ includeSnapshots: !!event.target.checked });
    });
    bind(byId('prefAutoScan'), 'change', function (event) {
      setPrefs({ autoScan: !!event.target.checked });
    });
    bind(byId('openBackupsBtn'), 'click', revealBackups);

    /* update */
    bind(byId('versionLabel'), 'click', openSettings);
    bind(byId('settingsVersion'), 'click', openSettings);
    bind(byId('updateCheckBtn'), 'click', checkForUpdates);
    bind(byId('updateInstallBtn'), 'click', applyUpdateNow);
    bind(byId('updatePageBtn'), 'click', openUpdatePage);
    bind(byId('prefAutoCheckUpdates'), 'change', function (event) {
      setPrefs({ autoCheckUpdates: !!event.target.checked });
    });

    /* review */
    bind(byId('reviewModeSeg'), 'change', function (event) {
      var input = event.target;
      if (!input || input.name !== 'reviewMode' || !input.checked) return;
      setPrefs({ deleteMode: input.value });
      renderReview();
    });
    bind(byId('reviewAck'), 'change', function (event) {
      state.reviewAck = !!event.target.checked;
      renderReview();
    });
    bind(byId('reviewConfirm'), 'click', function () {
      if (state.prefs.deleteMode === 'permanent' && !state.reviewAck) return;
      performDelete();
    });

    /* palette */
    bind(byId('paletteInput'), 'input', function (event) {
      state.palette.query = event.target.value;
      state.palette.index = 0;
      renderPalette();
    });
    bind(byId('paletteInput'), 'keydown', function (event) {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        setPaletteActive(state.palette.index + 1);
      } else if (event.key === 'ArrowUp') {
        event.preventDefault();
        setPaletteActive(state.palette.index - 1);
      } else if (event.key === 'Enter') {
        event.preventDefault();
        executePaletteEntry(state.palette.entries[state.palette.index]);
      }
    });
    bind(byId('paletteResults'), 'click', function (event) {
      var row = event.target.closest ? event.target.closest('.palette-row') : null;
      if (!row) return;
      var index = Number(row.dataset.paletteIndex);
      executePaletteEntry(state.palette.entries[index]);
    });
    bind(byId('paletteResults'), 'mouseover', function (event) {
      var row = event.target.closest ? event.target.closest('.palette-row') : null;
      if (!row) return;
      var index = Number(row.dataset.paletteIndex);
      if (!isNaN(index) && index !== state.palette.index) setPaletteActive(index);
    });

    /* global pointer + keyboard */
    doc.addEventListener('pointerdown', safe(function (event) {
      var target = event.target;
      if (!target || target.nodeType !== 1) return;
      if (state.menu) {
        var menu = byId('contextMenu');
        if (menu && !menu.contains(target) && !target.closest('.row-more')) closeMenu({ restoreFocus: false });
      }
      if (state.popover) {
        var popover = byId(state.popover === 'filter' ? 'filterPopover' : 'quickPopover');
        if (popover && !popover.contains(target) && !target.closest('#filterBtn,#quickSelectBtn')) closePopover();
      }
    }));
    doc.addEventListener('keydown', safe(handleGlobalKeyDown));
    window.addEventListener('resize', safe(function () {
      if (state.popover) closePopover();
      if (state.menu) closeMenu({ restoreFocus: false });
    }));
    doc.addEventListener('visibilitychange', safe(function () {
      if (doc.visibilityState === 'visible' && state.scan.status === 'running' && !timers.scanPoll) {
        scheduleScanPoll(0);
      }
    }));

    if (window.matchMedia) {
      try {
        var media = window.matchMedia('(prefers-color-scheme: dark)');
        var onChange = function () {
          if (state.prefs.appearance === 'auto') {
            renderSidebar();
            renderTable();
            renderDetail();
          }
        };
        if (media.addEventListener) media.addEventListener('change', onChange);
        else if (media.addListener) media.addListener(onChange);
      } catch (err) { /* optional */ }
    }
  }

  /* =========================================================================
     27. Keyboard — CONTRACT §5.10
     ========================================================================= */

  function handleGlobalKeyDown(event) {
    if (event.defaultPrevented) return;
    if (event.isComposing) return;
    var key = event.key;
    var lower = key.length === 1 ? key.toLowerCase() : key;
    var meta = event.metaKey || event.ctrlKey;

    if (state.menu) {
      handleMenuKey(event);
      return;
    }

    if (state.overlay) {
      if (key === 'Tab') {
        trapFocus(event);
        return;
      }
      if (key === 'Escape') {
        event.preventDefault();
        closeOverlay();
        return;
      }
      if (meta && !event.altKey && lower === 'k') {
        event.preventDefault();
        if (state.overlay === 'palette') closeOverlay();
        else openPalette();
        return;
      }
      return;
    }

    if (key === 'Escape') {
      if (handleEscape()) event.preventDefault();
      return;
    }

    if (meta && !event.altKey && lower === 'k') {
      event.preventDefault();
      openPalette();
      return;
    }

    if (isTypingTarget(event.target)) return;
    if (event.altKey) return;

    if (meta) {
      if (lower === 'f') {
        event.preventDefault();
        var input = byId('searchInput');
        if (input) {
          input.focus();
          try { input.select(); } catch (err) { /* ignore */ }
        }
        return;
      }
      if (lower === 'r') {
        event.preventDefault();
        startScan();
        return;
      }
      if (key === ',') {
        event.preventDefault();
        openSettings();
        return;
      }
      if (event.shiftKey && lower === 'd') {
        event.preventDefault();
        openDiscover();
        return;
      }
      if (key === '\\' || lower === '\\') {
        event.preventDefault();
        toggleDetail();
        return;
      }
      if (lower === 'z' && !event.shiftKey) {
        event.preventDefault();
        performUndo();
        return;
      }
      if (lower === 'a') {
        event.preventDefault();
        selectAllVisible();
        return;
      }
      if (key === 'Backspace' || key === 'Delete') {
        event.preventDefault();
        if (selectedSessions().length) {
          if (state.prefs.confirmBeforeDelete) openReview();
          else performDelete();
        }
        return;
      }
    }

    if (key === '?') {
      event.preventDefault();
      openShortcuts();
      return;
    }

    if (isButtonLike(event.target)) {
      if (key === 'Enter' || key === ' ' || lower === 'x') return;
    }

    if (key === 'ArrowDown' || lower === 'j') {
      if (state.popover) closePopover();
      event.preventDefault();
      moveFocus(1);
      return;
    }
    if (key === 'ArrowUp' || lower === 'k') {
      if (state.popover) closePopover();
      event.preventDefault();
      moveFocus(-1);
      return;
    }
    if (key === ' ' || key === 'Spacebar' || lower === 'x') {
      if (!state.focusUid) return;
      event.preventDefault();
      if (event.shiftKey) selectRange(state.focusUid);
      else toggleSelected(state.focusUid);
      paintRows();
      renderStatusbar();
      renderDetail();
      updateSelectAll(visibleSessions());
      return;
    }
    if (key === 'Enter') {
      if (!state.focusUid) return;
      event.preventDefault();
      if (!state.detailOpen) {
        state.detailOpen = true;
        syncDetail();
      }
      renderDetail();
      paintRows();
      scrollFocusIntoView();
      return;
    }
    if (key === 'Home') {
      var visible = visibleSessions();
      if (visible.length) {
        event.preventDefault();
        setFocus(visible[0].uid, { dom: true, scroll: true });
      }
      return;
    }
    if (key === 'End') {
      var visibleEnd = visibleSessions();
      if (visibleEnd.length) {
        event.preventDefault();
        setFocus(visibleEnd[visibleEnd.length - 1].uid, { dom: true, scroll: true });
      }
    }
  }

  function handleEscape() {
    if (state.menu) { closeMenu(); return true; }
    if (state.popover) { closePopover(); return true; }
    if (state.overlay) { closeOverlay(); return true; }
    if (state.filter.query) {
      var search = byId('searchInput');
      var focused = doc.activeElement === search;
      clearSearch({ blur: focused });
      return true;
    }
    if (doc.activeElement === byId('searchInput')) {
      byId('searchInput').blur();
      return true;
    }
    if (state.selected.size) {
      clearSelection();
      return true;
    }
    if (state.focusUid) {
      state.focusUid = null;
      state.focusSuppressed = true;
      paintRows();
      renderDetail();
      return true;
    }
    return false;
  }

  /* =========================================================================
     28. Boot
     ========================================================================= */

  function exposeDebugApi() {
    try {
      window.AGENTSWEEP_APP = {
        state: state,
        visibleSessions: visibleSessions,
        selectedSessions: selectedSessions,
        formatBytes: formatBytes,
        relativeTime: relativeTime,
        renderAll: renderAll,
        startScan: startScan,
        reloadBootstrap: reloadBootstrap,
        performUndo: performUndo,
        openReview: openReview,
        setPrefs: setPrefs,
        setAppearance: setAppearance,
        refreshUpdate: refreshUpdate,
        checkForUpdates: checkForUpdates,
        api: api,
        toast: toast
      };
    } catch (err) { /* debug hook is optional */ }
  }

  function boot() {
    try {
      loadLocalUi();
      applyAppearance(state.prefs.appearance);
      syncSearchInput();
      syncDensity();
      syncDetail();
      applySortUi();
      bindEvents();
      ensureBrandAssets();
      renderAll();
      loadBootstrap({ initial: true, preserveFocus: false }).then(function () {
        refreshUpdate({ silent: true });
      }, function () {
        refreshUpdate({ silent: true });
      });
      exposeDebugApi();
    } catch (err) {
      reportError('启动时', err);
    }
  }

  if (doc.readyState === 'loading') {
    doc.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();

