
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
let statusRequestGeneration = 0;

// The browser UI lives in a loopback iframe inside the Wails shell. This
// narrow request bridge intentionally exposes three named native actions;
// arbitrary commands, paths, or Wails bindings never cross the frame.
let nativeRequestSequence = 0;
const nativeRequests = new Map();

window.addEventListener('message', event => {
  const response = event.data || {};
  if (event.source !== window.parent || response.channel !== 'metis-native' || response.kind !== 'response') return;
  const pending = nativeRequests.get(response.id);
  if (!pending) return;
  nativeRequests.delete(response.id);
  clearTimeout(pending.timer);
  if (response.error) pending.reject(new Error(response.error));
  else pending.resolve(response.value);
});

function requestNative(action, payload = {}, timeoutMs = 30000) {
  if (window.parent === window) return Promise.reject(new Error('This action requires the native METIS Desktop app.'));
  const id = 'native-' + Date.now() + '-' + (++nativeRequestSequence);
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      nativeRequests.delete(id);
      reject(new Error('The native Desktop action timed out.'));
    }, timeoutMs);
    nativeRequests.set(id, { resolve, reject, timer });
    window.parent.postMessage({ channel: 'metis-native', kind: 'request', id, action, payload }, '*');
  });
}

// --- Init ---
document.addEventListener('DOMContentLoaded', () => {
  initDesktopPreferences().then(loadWorkspaces).finally(loadSessions);
  detectProject();
  connectEvents();
  initTheme();
  initChatScroll();
  updateEmptyLayout();
  initLayout(); // DSH AppFrame parity: resizable sidebar/details + auto-collapse
  fetch('/api/config').then(r => r.json()).then(c => {
    if (c && c.model) {
      cfgModel = c.model;
      document.getElementById('modelName').textContent = c.model.split('/').pop();
    }
  }).catch(() => {});
	if (typeof loadEffort === 'function') loadEffort();
  document.getElementById('inputField').focus();
	setTimeout(() => checkDesktopUpdate(false), 250);
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
    newSession: 'New session', workspaces: 'Workspaces', searchSessions: 'Search sessions…', settings: 'Settings', checkUpdates: 'Check for updates',
    searchSessionsLabel: 'Search sessions', collapseSidebar: 'Collapse sidebar', expandSidebar: 'Expand sidebar',
    context: 'Context', autoCompactAt: 'auto-compact at', subAgents: 'sub-agents', backgroundTasks: 'background tasks',
    chat: 'Chat', trajectory: 'Trajectory', welcome: 'From idea to done', preview: 'METIS Desktop', sessionLog: 'Session log',
    composerPlaceholder: 'Describe what you want to build', jumpLatest: 'Jump to latest', details: 'Details', detailsPlaceholder: 'Select a tool row to inspect details',
    backToApp: 'Back to app', searchSettings: 'Search settings…', personal: 'Personal', general: 'General', appearance: 'Appearance',
    modelProviders: 'Model Providers', agentPresets: 'Agent Presets', plugins: 'Plugins', computerUse: 'Computer Use', smartRouting: 'Smart Routing', configuration: 'Configuration'
  },
  'zh-CN': {
    newSession: '新会话', workspaces: '工作区', searchSessions: '搜索会话…', settings: '设置', checkUpdates: '检查更新',
    searchSessionsLabel: '搜索会话', collapseSidebar: '收起侧栏', expandSidebar: '展开侧栏',
    context: '上下文', autoCompactAt: '自动压缩阈值', subAgents: '子代理', backgroundTasks: '后台任务',
    chat: '对话', trajectory: '轨迹', welcome: '从想法，到完成', preview: 'METIS Desktop', sessionLog: '会话日志',
    composerPlaceholder: '描述你想要构建的内容', jumpLatest: '回到最新', details: '详情', detailsPlaceholder: '点击消息流中的工具行查看详情',
    backToApp: '返回应用', searchSettings: '搜索设置…', personal: '个人', general: '通用', appearance: '外观',
    modelProviders: '模型提供商', agentPresets: '代理预设', plugins: '插件', computerUse: '电脑操作', smartRouting: '智能路由', configuration: '配置'
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

let desktopUpdateStatus = null;
let desktopUpdateChecking = false;
let desktopUpdateDialog = null;

function paintDesktopUpdateButton() {
  const button = document.getElementById('desktopUpdateBtn');
  if (!button) return;
  button.classList.toggle('checking', desktopUpdateChecking);
  button.classList.toggle('available', !!(desktopUpdateStatus && desktopUpdateStatus.available));
  button.classList.toggle('unsupported', !!(desktopUpdateStatus && !desktopUpdateStatus.canUpdate));
  const title = desktopUpdateChecking
    ? uiText('Checking for updates…', '正在检查更新…')
    : desktopUpdateStatus && desktopUpdateStatus.available
      ? uiText('Update to METIS Desktop ', '更新到 METIS Desktop ') + desktopUpdateStatus.latestVersion
      : uiText('Check for updates', '检查更新');
  button.title = title;
  button.setAttribute('aria-label', title);
}

async function checkDesktopUpdate(notify = true) {
  if (desktopUpdateChecking) return desktopUpdateStatus;
  desktopUpdateChecking = true;
  paintDesktopUpdateButton();
  try {
    desktopUpdateStatus = await requestNative('check-update');
    paintDesktopUpdateButton();
    if (notify && desktopUpdateStatus && !desktopUpdateStatus.available) {
      showToast(desktopUpdateStatus.message || uiText('METIS Desktop is up to date.', 'METIS Desktop 已是最新版本。'));
    }
    return desktopUpdateStatus;
  } catch (error) {
    // Browser development mode intentionally has no native bridge. Keep the
    // icon useful there by surfacing the limitation only after a user click.
    if (notify) showToast(error.message || uiText('Unable to check for updates.', '无法检查更新。'));
    return null;
  } finally {
    desktopUpdateChecking = false;
    paintDesktopUpdateButton();
  }
}

async function openDesktopUpdateDialog() {
  const status = await checkDesktopUpdate(false);
  if (!status) {
    showToast(uiText('Updates are available in the native METIS Desktop app.', '请在原生 METIS Desktop 客户端中检查更新。'));
    return;
  }
  closeDesktopUpdateDialog(true);
  const available = !!status.available;
  const canUpdate = !!status.canUpdate;
  const overlay = document.createElement('div');
  overlay.className = 'update-overlay';
  overlay.innerHTML = `<section class="update-dialog" role="alertdialog" aria-modal="true" aria-labelledby="updateDialogTitle" aria-describedby="updateDialogDescription">
    <div class="update-dialog-icon" aria-hidden="true"><svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M16.5 7.2A7 7 0 1 0 16.2 13"/><path d="M16.5 3.5v3.7h-3.7"/></svg></div>
    <div class="update-dialog-copy"><h2 id="updateDialogTitle">${available ? uiText('Update available', '发现新版本') : uiText('METIS Desktop is current', 'METIS Desktop 已是最新版本')}</h2>
      <p id="updateDialogDescription">${escHtml(status.message || '')}</p>
      <div class="update-version-row"><span>${uiText('Current', '当前')} ${escHtml(status.currentVersion || '-')}</span><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3 8h10M9 4l4 4-4 4"/></svg><strong>${escHtml(status.latestVersion || status.currentVersion || '-')}</strong></div>
      <p class="update-safety">${uiText('The release archive, SHA-256 checksum, and candidate app are verified before replacement. Nothing changes until you choose Update and restart.', '替换应用前会验证发布归档、SHA-256 校验值和候选应用。只有点击“更新并重启”才会修改当前版本。')}</p>
      <p class="update-error" role="alert" hidden></p>
    </div>
    <div class="update-dialog-actions"><button type="button" class="update-later">${available ? uiText('Later', '稍后') : uiText('Close', '关闭')}</button>${available ? `<button type="button" class="update-confirm" ${canUpdate ? '' : 'disabled'}>${uiText('Update and restart', '更新并重启')}</button>` : ''}</div>
  </section>`;
  document.body.appendChild(overlay);
  const trigger = document.getElementById('desktopUpdateBtn');
  desktopUpdateDialog = { overlay, trigger, pending: false };
  overlay.querySelector('.update-later').addEventListener('click', () => closeDesktopUpdateDialog(false));
  const confirm = overlay.querySelector('.update-confirm');
  if (confirm) confirm.addEventListener('click', installDesktopUpdate);
  overlay.addEventListener('click', event => { if (event.target === overlay) closeDesktopUpdateDialog(false); });
  overlay.addEventListener('keydown', event => {
    if (event.key === 'Escape' && !desktopUpdateDialog.pending) { event.preventDefault(); closeDesktopUpdateDialog(false); }
  });
  requestAnimationFrame(() => (confirm && !confirm.disabled ? confirm : overlay.querySelector('.update-later')).focus());
}

function closeDesktopUpdateDialog(force) {
  if (!desktopUpdateDialog || (desktopUpdateDialog.pending && !force)) return;
  const state = desktopUpdateDialog;
  desktopUpdateDialog = null;
  state.overlay.remove();
  if (state.trigger) state.trigger.focus();
}

async function installDesktopUpdate() {
  const state = desktopUpdateDialog;
  if (!state || state.pending) return;
  state.pending = true;
  const confirm = state.overlay.querySelector('.update-confirm');
  const later = state.overlay.querySelector('.update-later');
  const error = state.overlay.querySelector('.update-error');
  confirm.disabled = true;
  later.disabled = true;
  confirm.textContent = uiText('Downloading and verifying…', '正在下载并验证…');
  try {
    desktopUpdateStatus = await requestNative('install-update', {}, 15 * 60 * 1000);
    confirm.textContent = uiText('Restarting…', '正在重启…');
  } catch (err) {
    state.pending = false;
    confirm.disabled = false;
    later.disabled = false;
    confirm.textContent = uiText('Try again', '重试');
    error.hidden = false;
    error.textContent = err.message || uiText('Update failed.', '更新失败。');
  }
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
async function pollStatus(shouldApply = () => true) {
  const generation = ++statusRequestGeneration;
  try {
    const res = await fetch('/api/status');
    if (!res.ok) return;
    const d = await res.json();
    if (generation !== statusRequestGeneration || !shouldApply()) return;
    lastStatusSnapshot = d;
    renderStatusSnapshot(d);
    if (typeof applyStatusPlanSnapshot === 'function') applyStatusPlanSnapshot(d);
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
    }
    const meter = document.getElementById('contextMeter');
    if (meter) {
      const used = Number(d.contextUsed) || 0;
      const limit = Number(d.contextWindow) || 0;
      const activeSessionId = String(d.activeSessionId || '');
      const selectedSessionId = String(currentSessionId || (turnRunning ? runningSessionId : '') || '');
      const viewingNoSession = !selectedSessionId;
      const viewingInactiveSession = !!(selectedSessionId && activeSessionId && selectedSessionId !== activeSessionId);
      if (viewingNoSession) {
        // The blank new-session composer does not own the backend Loop's
        // previous context. Keep the meter hidden until a turn starts or the
        // user selects a saved transcript.
        meter.style.display = 'none';
        meter.classList.remove('warn');
      } else if (viewingInactiveSession) {
        // /api/status reports the one active Loop. Do not attribute that
        // background session's pressure to a different transcript that the
        // user is only viewing.
        meter.style.display = '';
        meter.textContent = dict.context + ' —';
        meter.title = dict.context;
        meter.classList.remove('warn');
      } else if (limit > 0) {
        const fraction = used / limit;
        // Context pressure can temporarily estimate above the provider limit
        // while compaction is running or when fixed tool/system overhead is
        // irreducible. A progress badge must remain a percentage, not display
        // values such as 318%; preserve the raw token counts in the tooltip.
        const percent = Math.max(0, Math.min(100, Math.round(fraction * 100)));
        const percentLabel = used > 0 && fraction < 0.01 ? '<1%' : percent + '%';
        const compactAtTokens = Number(d.compactAtTokens) || 0;
        const compactAt = compactAtTokens > 0
          ? compactAtTokens / limit
          : Number(d.compactThreshold) || 0;
        meter.style.display = '';
        meter.textContent = dict.context + ' ' + percentLabel;
        meter.title = dict.context + ' ' + fmtTokens(used) + ' / ' + fmtTokens(limit) + ' tokens' +
          (compactAtTokens > 0
            ? ' \u00B7 ' + dict.autoCompactAt + ' ' + fmtTokens(compactAtTokens) + ' tokens (' + Math.round(compactAt * 100) + '%)'
            : compactAt > 0 ? ' \u00B7 ' + dict.autoCompactAt + ' ' + Math.round(compactAt * 100) + '%' : '');
        meter.classList.toggle('warn', compactAt > 0 && used >= Math.max(0, compactAtTokens > 0 ? compactAtTokens * 0.9 : limit * (compactAt - 0.1)));
      } else {
        meter.style.display = 'none';
      }
    }
    if (typeof setTurnRunning === 'function') {
      const statusRunning = !!d.turnRunning;
      const statusSession = String(d.runningSessionId || '');
      if (statusRunning !== turnRunning || (statusRunning && statusSession && statusSession !== runningSessionId)) {
        setTurnRunning(statusRunning, statusSession);
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

// Computer Use settings only read status on entry. Installation, launch and
// opening operating-system settings require a named button action.
let computerUseStatus = null;
let computerUseBusy = false;
let computerUseError = '';
let computerUsePendingAction = '';
let computerUseOperationID = 0;

function renderComputerUseTab() {
  return `<h2>${uiText('Computer Use', '电脑操作')}</h2>
    <section class="settings-section computer-use-settings">
      <p class="settings-section-desc">${uiText('Let Metis use the screen, mouse and keyboard on this computer. Installation, a running connection and operating-system permissions are shown separately.', '让 Metis 使用这台电脑的屏幕、鼠标和键盘。下方分别显示安装状态、运行连接和操作系统权限。')}</p>
      <div id="computerUsePanel" aria-live="polite">${computerUseMarkup()}</div>
    </section>`;
}

function computerUsePermissionLabel(value) {
  const labels = {
    granted: uiText('Granted', '已授权'),
    denied: uiText('Not granted', '未授权'),
    notGranted: uiText('Not granted', '未授权'),
    'not-granted': uiText('Not granted', '未授权'),
    'not-determined': uiText('Not yet authorized', '尚未授权'),
    unknown: uiText('Unknown', '未知'),
    unsupported: uiText('Unsupported', '不支持'),
    unavailable: uiText('Unavailable', '不可用'),
    'not-required': uiText('Not required', '无需授权')
  };
  return labels[value] || value || uiText('Unknown — refresh after installing', '未知，请安装后刷新');
}

function computerUseMarkup() {
  const status = computerUseStatus;
  const description = status && status.description || {};
  const permissions = description.permissions || {};
  const disabled = computerUseBusy ? ' disabled' : '';
  const canInterruptEnable = computerUsePendingAction === 'enable';
  const canStop = canInterruptEnable || (!computerUseBusy && status && status.running);
  const row = (label, value) => `<div class="settings-card-row"><span class="settings-card-label">${escHtml(label)}</span><span class="computer-use-value">${escHtml(value)}</span></div>`;
  const source = status && status.source === 'local' ? uiText('Local build (experimental)', '本地构建（实验性）')
    : status && status.source === 'official' ? uiText('Official component', '官方组件')
    : uiText('Not installed', '未安装');
  const installation = status ? (status.installed ? uiText('Installed', '已安装') : uiText('Not installed', '未安装')) : uiText('Unknown', '未知');
  const state = status ? (status.enabled ? uiText('Enabled', '已启用') : uiText('Disabled', '已停用')) : uiText('Unknown', '未知');
  const connection = status ? (status.running ? uiText('Running', '运行中') : uiText('Stopped', '已停止')) : uiText('Unknown', '未知');
  const permissionRow = (label, key, action) => `<div class="settings-card-row"><div><div class="settings-card-label">${escHtml(label)}</div><div class="settings-card-desc">${escHtml(computerUsePermissionLabel(permissions[key]))}</div></div>${description.platform === 'darwin' ? `<button type="button" class="computer-use-button" onclick="computerUseAction('${action}')"${disabled}>${uiText('Open System Settings', '打开系统设置')}</button>` : ''}</div>`;
  return `<div class="computer-use-status" role="status">${computerUseBusy ? uiText('Working…', '正在处理…') : ''}</div>
    ${computerUseError ? `<p class="computer-use-error" role="alert">${escHtml(computerUseError)}</p>` : ''}
    ${computerUseError && status ? `<p class="settings-section-desc">${uiText('Last known status. Refresh to check again.', '以下为上次获取的状态，请刷新确认。')}</p>` : ''}
    <div class="settings-card">
      ${row(uiText('Installation', '安装'), installation)}
      ${row(uiText('Computer Use', '电脑操作'), state)}
      ${row(uiText('Connection', '连接'), connection)}
      ${row(uiText('Version', '版本'), status && status.version || '—')}
      ${row(uiText('Source', '来源'), status ? source : '—')}
    </div>
    ${status && status.message ? `<p class="computer-use-note">${escHtml(status.message)}</p>` : ''}
    <div class="computer-use-actions">
      <button type="button" class="computer-use-button" onclick="loadComputerUse()"${disabled}>${uiText('Refresh', '刷新')}</button>
      <button type="button" class="computer-use-button primary" onclick="computerUseAction('enable')"${disabled}${!status || status.running ? ' disabled' : ''}>${status && status.installed ? uiText('Enable', '启用') : uiText('Install & enable', '安装并启用')}</button>
      <button type="button" class="computer-use-button" onclick="computerUseAction('stop')"${canStop ? '' : ' disabled'}>${uiText('Stop', '停止')}</button>
      <button type="button" class="computer-use-button" onclick="computerUseAction('disable')"${disabled}${!status || !status.enabled ? ' disabled' : ''}>${uiText('Disable', '停用')}</button>
    </div>
    <h3 class="settings-section-title">${uiText('Operating-system permissions', '操作系统权限')}</h3>
    <p class="settings-section-desc">${uiText('Installing or enabling Computer Use does not grant these permissions. Open System Settings, grant access yourself, then refresh. An installed component may still be unable to see or control your screen.', '安装或启用电脑操作不会授予这些权限。请打开系统设置，自行授权后刷新。组件已安装时，仍可能无法查看或控制屏幕。')}</p>
    <div class="settings-card">
      ${permissionRow(uiText('Accessibility', '辅助功能'), 'accessibility', 'permissions-accessibility')}
      ${permissionRow(uiText('Screen Recording', '屏幕录制'), 'screenRecording', 'permissions-screen-recording')}
    </div>
    ${status && status.path ? `<details class="computer-use-location"><summary>${uiText('Component location', '组件位置')}</summary><code>${escHtml(status.path)}</code></details>` : ''}`;
}

function paintComputerUse() {
  const panel = document.getElementById('computerUsePanel');
  if (!panel) return;
  panel.setAttribute('aria-busy', String(computerUseBusy));
  panel.innerHTML = computerUseMarkup();
}

async function loadComputerUse() {
  if (computerUseBusy) return;
  const operationID = ++computerUseOperationID;
  computerUseBusy = true;
  computerUsePendingAction = 'status';
  computerUseError = '';
  paintComputerUse();
  try {
    const response = await fetch('/api/computer-use', {cache: 'no-store'});
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || uiText('Unable to read Computer Use status.', '无法读取电脑操作状态。'));
    if (operationID === computerUseOperationID) computerUseStatus = data;
  } catch (error) {
    if (operationID === computerUseOperationID) computerUseError = error.message || String(error);
  } finally {
    if (operationID === computerUseOperationID) {
      computerUseBusy = false;
      computerUsePendingAction = '';
      paintComputerUse();
    }
  }
}

async function computerUseAction(action) {
  if (!['enable', 'stop', 'disable', 'permissions-accessibility', 'permissions-screen-recording'].includes(action)) return;
  const interruptsEnable = action === 'stop' && computerUsePendingAction === 'enable';
  if (computerUseBusy && !interruptsEnable) return;
  // Stop cancels the runtime's installation/launch ticket. Its response owns
  // the UI even if the earlier enable request resolves or rejects afterward.
  const operationID = ++computerUseOperationID;
  computerUseBusy = true;
  computerUsePendingAction = action;
  computerUseError = '';
  paintComputerUse();
  try {
    const response = await fetch('/api/computer-use', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({action})});
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || uiText('Computer Use action failed.', '电脑操作请求失败。'));
    if (operationID === computerUseOperationID) computerUseStatus = data;
  } catch (error) {
    if (operationID === computerUseOperationID) computerUseError = error.message || String(error);
  } finally {
    if (operationID === computerUseOperationID) {
      computerUseBusy = false;
      computerUsePendingAction = '';
      paintComputerUse();
    }
  }
}
