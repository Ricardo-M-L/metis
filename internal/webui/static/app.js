
// ============================================================
// Metis Desktop — Client Logic
// ============================================================

let approvalMode = 'default';
let messages = [];
let sessions = [];
let workspaces = [];
let activeWorkspaceId = '';
let currentSessionId = null;
let desktopPreferences = { busyEnter: 'queue', sidebarView: 'grouped', sidebarSort: 'recent', sessionOrder: [], defaultPreset: 'standard', language: 'zh-CN' };
let lastStatusSnapshot = null;

// --- Init ---
document.addEventListener('DOMContentLoaded', () => {
  initDesktopPreferences().then(loadWorkspaces).finally(loadSessions);
  detectProject();
  connectEvents();
  initTheme();
  updateEmptyLayout();
  initLayout(); // DSH AppFrame parity: resizable sidebar/details + auto-collapse
  fetch('/api/config').then(r => r.json()).then(c => {
    if (c && c.model) {
      cfgModel = c.model;
      document.getElementById('modelName').textContent = c.model.split('/').pop();
    }
	const preset = document.getElementById('presetName');
	if (preset) preset.textContent = presetDisplayName(c.preset || 'standard');
  }).catch(() => {});
	if (typeof loadEffort === 'function') loadEffort();
  document.getElementById('inputField').focus();
});

async function initDesktopPreferences() {
  try {
    const res = await fetch('/api/preferences');
    const data = await res.json();
    if (res.ok) desktopPreferences = Object.assign({}, desktopPreferences, data);
	applyLanguage(desktopPreferences.language);
  } catch (_) { /* server defaults remain authoritative on next save */ }
}

const DESKTOP_I18N = {
  en: {
    newSession: 'New session', workspaces: 'Workspaces', searchSessions: 'Search sessions…', settings: 'Settings',
    searchSessionsLabel: 'Search sessions', collapseSidebar: 'Collapse sidebar', expandSidebar: 'Expand sidebar',
    context: 'Context', autoCompactAt: 'auto-compact at', subAgents: 'sub-agents', backgroundTasks: 'background tasks',
    chat: 'Chat', trajectory: 'Trajectory', welcome: 'From idea to done', preview: 'METIS Desktop', sessionLog: 'Session log',
    composerPlaceholder: 'Describe what you want to build', details: 'Details', detailsPlaceholder: 'Select a tool row to inspect details',
    backToApp: 'Back to app', searchSettings: 'Search settings…', personal: 'Personal', general: 'General', appearance: 'Appearance',
    modelProviders: 'Model Providers', agentPresets: 'Agent Presets', plugins: 'Plugins', smartRouting: 'Smart Routing', configuration: 'Configuration'
  },
  'zh-CN': {
    newSession: '新会话', workspaces: '工作区', searchSessions: '搜索会话…', settings: '设置',
    searchSessionsLabel: '搜索会话', collapseSidebar: '收起侧栏', expandSidebar: '展开侧栏',
    context: '上下文', autoCompactAt: '自动压缩阈值', subAgents: '子代理', backgroundTasks: '后台任务',
    chat: '对话', trajectory: '轨迹', welcome: '从想法，到完成', preview: 'METIS Desktop', sessionLog: '会话日志',
    composerPlaceholder: '描述你想要构建的内容', details: '详情', detailsPlaceholder: '点击消息流中的工具行查看详情',
    backToApp: '返回应用', searchSettings: '搜索设置…', personal: '个人', general: '通用', appearance: '外观',
    modelProviders: '模型提供商', agentPresets: '代理预设', plugins: '插件', smartRouting: '智能路由', configuration: '配置'
  }
};

function resolvedLanguage(value) {
  if (value === 'auto') return String(navigator.language || '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';
  return value === 'en' ? 'en' : 'zh-CN';
}

function applyLanguage(value) {
  const lang = resolvedLanguage(value || 'zh-CN');
  const dict = DESKTOP_I18N[lang];
  document.documentElement.lang = lang;
  document.querySelectorAll('[data-i18n]').forEach(el => { const text = dict[el.dataset.i18n]; if (text) el.textContent = text; });
  document.querySelectorAll('[data-i18n-placeholder]').forEach(el => { const text = dict[el.dataset.i18nPlaceholder]; if (text) el.placeholder = text; });
  document.querySelectorAll('[data-i18n-label]').forEach(el => { const text = dict[el.dataset.i18nLabel]; if (text) el.setAttribute('aria-label', text); });
  document.querySelectorAll('[data-i18n-title]').forEach(el => { const text = dict[el.dataset.i18nTitle]; if (text) el.title = text; });
  applyLayout();
  if (typeof syncApprovalChip === 'function') syncApprovalChip(approvalMode);
  if (lastStatusSnapshot) renderStatusSnapshot(lastStatusSnapshot);
}

function presetDisplayName(id) {
  if (!id || id === 'standard') return 'Standard';
  return String(id).split(/[-_.]+/).filter(Boolean).map(p => p.charAt(0).toUpperCase() + p.slice(1)).join(' ');
}

async function saveDesktopPreference(key, value) {
	return saveDesktopPreferencesPatch({ [key]: value });
}

async function saveDesktopPreferencesPatch(patch) {
  const previous = Object.assign({}, desktopPreferences, { sessionOrder: (desktopPreferences.sessionOrder || []).slice() });
  Object.assign(desktopPreferences, patch);
  try {
    const res = await fetch('/api/preferences', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(patch)
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'preferences: ' + res.status);
    desktopPreferences = Object.assign({}, desktopPreferences, data);
    return true;
  } catch (e) {
	desktopPreferences = previous;
    showToast('Preference save failed: ' + e.message);
    return false;
  }
}


function escAttr(s) {
  return escHtml(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// For onclick="f('...')" string literals: escHtml's &#39; decodes back to
// a quote BEFORE the inline script compiles, so a JS-string context needs
// a real backslash escape instead.
function escOnclick(s) {
  return escHtml(s).replace(/'/g, "\\'");
}
// --- Helpers ---
function escHtml(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function detectProject() {
  const el = document.getElementById('wsGroupName');
  if (!el) return;
  const parts = window.location.pathname.split('/').filter(Boolean);
  el.textContent = parts[0] || 'metis';
}

function toggleSearch() {
  // Placeholder for search functionality
}

// Transient toast for desktop actions (exports, errors).
function showToast(msg) {
  let el = document.getElementById('toastBanner');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toastBanner';
    el.className = 'toast-banner';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    el.setAttribute('aria-atomic', 'true');
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.classList.add('show');
  clearTimeout(el._hideTimer);
  el._hideTimer = setTimeout(() => el.classList.remove('show'), 6000);
}

// Live task progress chip ("N sub-agents ~ M background tasks"),
// polled every 3s like the harness GUI status bar.
async function pollStatus() {
  try {
    const res = await fetch('/api/status');
    if (!res.ok) return;
    const d = await res.json();
    lastStatusSnapshot = d;
    renderStatusSnapshot(d);
  } catch (_) { /* status is best-effort */ }
}

function renderStatusSnapshot(d) {
  try {
    const chip = document.getElementById('statusChip');
    if (!chip) return;
    const dict = DESKTOP_I18N[document.documentElement.lang] || DESKTOP_I18N['zh-CN'];
    const n = d.subAgents || 0, m = d.backgroundTasks || 0;
    if (n === 0 && m === 0) {
      chip.style.display = 'none';
    } else {
      chip.style.display = '';
      chip.textContent = n + ' ' + dict.subAgents + ' ~ ' + m + ' ' + dict.backgroundTasks;
    }
    if (document.getElementById('statusPopover').style.display !== 'none') renderStatusPopover();
    if (d.workspace) {
      const pn = document.getElementById('wsGroupName');
      if (pn) pn.textContent = d.workspace;
      if (!currentSessionId) {
        const tt = document.getElementById('topbarTitle');
        if (tt) tt.textContent = d.workspace;
      }
    }
    const meter = document.getElementById('contextMeter');
    if (meter) {
      const used = Number(d.contextUsed) || 0;
      const limit = Number(d.contextWindow) || 0;
      if (limit > 0) {
        const fraction = used / limit;
        const percent = Math.max(0, Math.min(999, Math.round(fraction * 100)));
        const compactAt = Number(d.compactThreshold) || 0;
        meter.style.display = '';
        meter.textContent = dict.context + ' ' + percent + '%';
        meter.title = dict.context + ' ' + fmtTokens(used) + ' / ' + fmtTokens(limit) + ' tokens' +
          (compactAt > 0 ? ' \u00B7 ' + dict.autoCompactAt + ' ' + Math.round(compactAt * 100) + '%' : '');
        meter.classList.toggle('warn', compactAt > 0 && fraction >= Math.max(0, compactAt - 0.1));
      } else {
        meter.style.display = 'none';
      }
    }
  } catch (_) { /* status is best-effort */ }
}
setInterval(pollStatus, 3000);
pollStatus();

function toggleStatusPopover(e) {
  if (e) e.stopPropagation();
  const pop = document.getElementById('statusPopover');
  const chip = document.getElementById('statusChip');
  const open = pop.style.display === 'none';
  pop.style.display = open ? 'block' : 'none';
  chip.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) renderStatusPopover();
}

function renderStatusPopover() {
  const pop = document.getElementById('statusPopover');
  const d = lastStatusSnapshot || {};
  const agents = Array.isArray(d.agents) ? d.agents : [];
  const jobs = Array.isArray(d.jobs) ? d.jobs : [];
  const rows = [];
  if (agents.length) {
    rows.push('<div class="status-popover-label">Sub-agents</div>');
    agents.forEach(a => rows.push('<div class="status-popover-row"><span class="status-dot ' + escAttr(a.status || '') + '"></span><span>' + escHtml(a.name || a.agentId || 'agent') + '</span><small>' + escHtml(a.status || '') + '</small></div>'));
  }
  if (jobs.length) {
    rows.push('<div class="status-popover-label">Background tasks</div>');
    jobs.forEach(j => rows.push('<div class="status-popover-row"><span class="status-dot ' + escAttr(j.status || '') + '"></span><span>' + escHtml(j.description || j.id || 'task') + '</span><small>' + escHtml(j.status || '') + '</small></div>'));
  }
  pop.innerHTML = rows.join('') || '<div class="status-popover-empty">No active agents or tasks</div>';
}

document.addEventListener('click', e => {
  const pop = document.getElementById('statusPopover');
  const chip = document.getElementById('statusChip');
  if (!pop || pop.style.display === 'none' || pop.contains(e.target) || chip.contains(e.target)) return;
  pop.style.display = 'none';
  chip.setAttribute('aria-expanded', 'false');
});

// ============================================================
// Layout — resizable / collapsible columns (DSH AppFrame parity).
// Constants mirror ui-layout columns.ts: SIDEBAR 264..420 (default
// 280, rail 56, auto-collapse < 1024), DETAILS 300..520 (default
// 360). Drag gestures use pointer capture + rAF throttle; widths ride
// CSS vars on .app (see the Layout section of style.css).
// ============================================================
const LAYOUT_KEY = 'metis.layout.v1';
const SIDEBAR_MIN = 264, SIDEBAR_MAX = 420, SIDEBAR_DEFAULT = 280, SIDEBAR_RAIL = 56;
const SIDEBAR_AUTO_COLLAPSE = 1024;
const DETAILS_MIN = 300, DETAILS_MAX = 520, DETAILS_DEFAULT = 360;

let layoutState = { sidebar: SIDEBAR_DEFAULT, collapsed: false, details: DETAILS_DEFAULT };
let narrowExpanded = false; // narrow-viewport manual re-expand (DSH panels.narrowExpanded)

function initLayout() {
  loadLayout();
  const app = document.querySelector('.app');
  if (!app) return;
  applyLayout();
  const sbHandle = document.getElementById('sidebarHandle');
  const dtHandle = document.getElementById('detailsHandle');
  if (sbHandle) attachDrag(sbHandle, 'sidebar');
  if (dtHandle) attachDrag(dtHandle, 'details');
  window.addEventListener('resize', onWindowResize);
  onWindowResize();
}

function loadLayout() {
  try {
    const raw = localStorage.getItem(LAYOUT_KEY);
    if (raw) layoutState = Object.assign({}, layoutState, JSON.parse(raw));
  } catch (_) { /* corrupt state → defaults */ }
}

function saveLayout() {
  try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layoutState)); } catch (_) {}
}

// Effective collapsed = narrow ? !manualExpand : user preference —
// the same concession AppFrame computes (narrow ? !narrowExpanded :
// panels.sidebar === 0).
function effectiveCollapsed() {
  const app = document.querySelector('.app');
  if (!app) return layoutState.collapsed;
  const narrow = app.clientWidth < SIDEBAR_AUTO_COLLAPSE;
  return narrow ? !narrowExpanded : layoutState.collapsed;
}

function applyLayout() {
  const app = document.querySelector('.app');
  if (!app) return;
  if (effectiveCollapsed()) {
    app.classList.add('sb-rail');
  } else {
    app.classList.remove('sb-rail');
    app.style.setProperty('--sb-w', layoutState.sidebar + 'px');
  }
  app.style.setProperty('--dt-w', layoutState.details + 'px');
  const btn = document.getElementById('sidebarCollapseBtn');
  if (btn) {
    const dict = DESKTOP_I18N[document.documentElement.lang] || DESKTOP_I18N['zh-CN'];
    const label = effectiveCollapsed() ? dict.expandSidebar : dict.collapseSidebar;
    btn.title = label;
    btn.setAttribute('aria-label', label);
  }
}

function toggleSidebar() {
  const app = document.querySelector('.app');
  const narrow = app && app.clientWidth < SIDEBAR_AUTO_COLLAPSE;
  if (narrow) {
    // Narrow toggle flips the manual re-expand override (DSH stores.ts).
    narrowExpanded = !narrowExpanded;
  } else {
    layoutState.collapsed = !layoutState.collapsed;
    saveLayout();
  }
  applyLayout();
}

function openRailSearch() {
  const app = document.querySelector('.app');
  if (app && effectiveCollapsed()) {
    if (app.clientWidth < SIDEBAR_AUTO_COLLAPSE) narrowExpanded = true;
    else layoutState.collapsed = false;
    saveLayout();
    applyLayout();
  }
  const wrap = document.getElementById('sbSearch');
  const input = document.getElementById('sessionSearchInput');
  if (wrap) wrap.classList.add('open');
  if (input) requestAnimationFrame(() => input.focus());
}

// One drag gesture: pointer capture + rAF-throttled deltas against the
// drag-start origin; the base width freezes for the whole gesture so dx
// deltas never compound (AppFrame DragHandle). Move/up listeners ride the
// WINDOW once a gesture starts — pointer capture alone is unreliable
// across synthetic/embedded pointer event sources, and a drag must keep
// tracking even when the cursor leaves the 8px strip.
function attachDrag(handle, side) {
  let origin = 0, base = 0, raf = null, active = false;

  function onMove(e) {
    if (!active) return;
    if (raf !== null) return;
    raf = requestAnimationFrame(() => {
      raf = null;
      const dx = e.clientX - origin;
      if (side === 'sidebar') {
        layoutState.sidebar = Math.max(SIDEBAR_MIN, Math.min(SIDEBAR_MAX, base + dx));
        layoutState.collapsed = false; // dragging a rail-expanded sidebar writes the width
      } else {
        // details: dragging LEFT widens the right-hand column
        layoutState.details = Math.max(DETAILS_MIN, Math.min(DETAILS_MAX, base - dx));
      }
      applyLayout();
    });
  }

  function onUp() {
    if (!active) return;
    active = false;
    if (raf !== null) { cancelAnimationFrame(raf); raf = null; }
    window.removeEventListener('pointermove', onMove);
    window.removeEventListener('pointerup', onUp);
    window.removeEventListener('pointercancel', onUp);
    const app = document.querySelector('.app');
    if (app) delete app.dataset.dragging;
    delete handle.dataset.dragging;
    if (side === 'sidebar') {
      if (effectiveCollapsed()) narrowExpanded = true; // resized while narrow → keep open
    }
    saveLayout();
  }

  handle.addEventListener('pointerdown', function (e) {
    e.preventDefault();
    try { handle.setPointerCapture(e.pointerId); } catch (_) { /* capture best-effort */ }
    active = true;
    origin = e.clientX;
    base = side === 'sidebar' ? layoutState.sidebar : layoutState.details;
    const app = document.querySelector('.app');
    if (app) app.dataset.dragging = 'true';
    handle.dataset.dragging = 'true';
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', onUp);
  });
}

function onWindowResize() {
  const app = document.querySelector('.app');
  if (!app) return;
  applyLayout();
}
