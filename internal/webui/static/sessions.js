// Metis Desktop - session list management (sidebar).
// Shared state (sessions, currentSessionId) lives in app.js.

// --- Sessions ---
let showArchivedSessions = false;
let openWorkspaceMenuBtn = null;
let sessionsNextCursor = '';
let sessionsTotal = 0;
let sessionsLoading = false;
let sessionSearchTimer = null;
let sessionDeleteDialog = null;

async function loadWorkspaces() {
  try {
    const res = await fetch('/api/workspaces');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'workspaces: ' + res.status);
    workspaces = Array.isArray(data.workspaces) ? data.workspaces : [];
    activeWorkspaceId = data.activeId || '';
    renderSessions();
  } catch (e) {
    workspaces = [];
    showToast('Unable to load workspaces: ' + e.message);
  }
}

function workspaceForSession(s) {
  return workspaces.find(w => w.id === s.workspaceId) || null;
}

async function addWorkspace() {
  const path = prompt('Add workspace path');
  if (!path || !path.trim()) return;
  const name = prompt('Workspace name (optional)', '') || '';
  try {
    const res = await fetch('/api/workspaces', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: path.trim(), name: name.trim() })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'add workspace: ' + res.status);
    await loadWorkspaces();
    showToast('Workspace added. Open it to start an independent Desktop window.');
  } catch (e) {
    showToast('Add workspace failed: ' + e.message);
  }
}

async function openWorkspace(id) {
  if (id === activeWorkspaceId) {
    showToast('This workspace is already open in the current window');
    return;
  }
  try {
    const res = await fetch('/api/workspaces/open', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'open workspace: ' + res.status);
    showToast('Opening workspace in a new Desktop window');
  } catch (e) {
    showToast('Open workspace failed: ' + e.message);
  }
}

function toggleWorkspaceMenu(btn) {
  if (openWorkspaceMenuBtn && openWorkspaceMenuBtn !== btn) {
    const old = openWorkspaceMenuBtn.parentElement.querySelector('.workspace-menu');
    if (old) old.style.display = 'none';
  }
  const menu = btn.parentElement.querySelector('.workspace-menu');
  const show = menu && menu.style.display === 'none';
  if (menu) menu.style.display = show ? 'block' : 'none';
  btn.setAttribute('aria-expanded', show ? 'true' : 'false');
  openWorkspaceMenuBtn = show ? btn : null;
}

async function renameWorkspace(id) {
  const ws = workspaces.find(w => w.id === id);
  if (!ws) return;
  const name = prompt('Rename workspace', ws.name || '');
  if (!name || !name.trim() || name.trim() === ws.name) return;
  try {
    const res = await fetch('/api/workspaces/rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, name: name.trim() })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'rename workspace: ' + res.status);
    await loadWorkspaces();
    showToast('Workspace renamed');
  } catch (e) {
    showToast('Rename workspace failed: ' + e.message);
  }
}

async function removeWorkspace(id) {
  const ws = workspaces.find(w => w.id === id);
  if (!ws) return;
  if (!confirm('Remove "' + ws.name + '" from the Desktop list? Project files and sessions will not be deleted.')) return;
  try {
    const res = await fetch('/api/workspaces/remove', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'remove workspace: ' + res.status);
    await loadWorkspaces();
    await loadSessions();
    showToast('Workspace removed from the list; no files or sessions were deleted');
  } catch (e) {
    showToast('Remove workspace failed: ' + e.message);
  }
}

async function moveWorkspace(id, delta) {
  const from = workspaces.findIndex(w => w.id === id);
  const to = from + delta;
  if (from < 0 || to < 0 || to >= workspaces.length) return;
  const reordered = workspaces.slice();
  const moved = reordered.splice(from, 1)[0];
  reordered.splice(to, 0, moved);
  try {
    const res = await fetch('/api/workspaces/reorder', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids: reordered.map(w => w.id) })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'reorder workspaces: ' + res.status);
    workspaces = Array.isArray(data.workspaces) ? data.workspaces : reordered;
    renderSessions();
  } catch (e) {
    showToast('Reorder failed: ' + e.message);
  }
}

function toggleSessionViewMenu(e) {
  if (e) e.stopPropagation();
  const menu = document.getElementById('sessionViewMenu');
  const btn = document.getElementById('sessionViewBtn');
  const open = menu.style.display === 'none';
  menu.style.display = open ? 'block' : 'none';
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) syncSessionViewMenu();
}

function syncSessionViewMenu() {
  document.querySelectorAll('#sessionViewMenu [data-pref]').forEach(btn => {
    const selected = desktopPreferences[btn.dataset.pref] === btn.dataset.value;
    btn.classList.toggle('selected', selected);
    btn.setAttribute('aria-checked', selected ? 'true' : 'false');
  });
}

async function setSessionPreference(key, value) {
  if (await saveDesktopPreference(key, value)) {
    sessionsExpanded = false;
    renderSessions();
    syncSessionViewMenu();
  }
}

document.addEventListener('click', e => {
  const menu = document.getElementById('sessionViewMenu');
  const btn = document.getElementById('sessionViewBtn');
  if (!menu || menu.style.display === 'none' || menu.contains(e.target) || btn.contains(e.target)) return;
  menu.style.display = 'none';
  btn.setAttribute('aria-expanded', 'false');
});

document.addEventListener('click', e => {
  if (!openWorkspaceMenuBtn) return;
  const row = openWorkspaceMenuBtn.parentElement;
  if (row && row.contains(e.target)) return;
  const menu = row && row.querySelector('.workspace-menu');
  if (menu) menu.style.display = 'none';
  openWorkspaceMenuBtn.setAttribute('aria-expanded', 'false');
  openWorkspaceMenuBtn = null;
});

async function loadSessions(append) {
	if (sessionsLoading) return;
	sessionsLoading = true;
  try {
	const params = new URLSearchParams({ limit: '50' });
	if (showArchivedSessions) params.set('archived_only', 'true');
	if (sessionFilter) params.set('q', sessionFilter);
	if (append && sessionsNextCursor) params.set('cursor', sessionsNextCursor);
	const url = '/api/sessions?' + params.toString();
    const res = await fetch(url);
    if (!res.ok) throw new Error(`sessions: ${res.status}`);
    const data = await res.json();
	const page = data.sessions || [];
	if (append) {
	  const merged = new Map(sessions.map(s => [s.id, s]));
	  page.forEach(s => merged.set(s.id, s));
	  sessions = Array.from(merged.values());
	} else {
	  sessions = page;
	}
	sessionsNextCursor = data.nextCursor || '';
	sessionsTotal = Number(data.total) || sessions.length;
	renderSessions();
  } catch (e) {
    document.getElementById('sessionList').innerHTML = '<div style="padding:12px;color:var(--red);font-size:13px;">Unable to load sessions</div>';
	} finally { sessionsLoading = false; }
}

async function loadMoreSessions() {
	if (!sessionsNextCursor || sessionsLoading) return;
	await loadSessions(true);
}

async function toggleArchivedSessions() {
  showArchivedSessions = !showArchivedSessions;
  sessionsExpanded = false;
  const btn = document.getElementById('archivedSessionsBtn');
  if (btn) {
    btn.classList.toggle('active', showArchivedSessions);
    btn.title = showArchivedSessions ? 'Show active sessions' : 'Show archived sessions';
    btn.setAttribute('aria-label', btn.title);
  }
  await loadSessions();
}

let sessionFilter = '';

let openMenuBtn = null;
function closeSessionMenu(restoreFocus) {
  if (!openMenuBtn) return;
  const button = openMenuBtn;
  const menu = button.parentElement.querySelector('.session-menu');
  if (menu) menu.style.display = 'none';
  button.setAttribute('aria-expanded', 'false');
  openMenuBtn = null;
  if (restoreFocus && button.isConnected) button.focus();
}

function toggleSessionMenu(btn) {
  if (openMenuBtn && openMenuBtn !== btn) {
    closeSessionMenu(false);
  }
  const menu = btn.parentElement.querySelector('.session-menu');
  const show = !menu || menu.style.display === 'none';
  if (menu) menu.style.display = show ? 'block' : 'none';
  btn.setAttribute('aria-expanded', show ? 'true' : 'false');
  openMenuBtn = show ? btn : null;
  if (show && menu) requestAnimationFrame(() => {
    const first = menu.querySelector('[role="menuitem"]:not(:disabled)');
    if (first) first.focus();
  });
}

function sessionMenuKeydown(event) {
  const menu = event.currentTarget;
  const items = Array.from(menu.querySelectorAll('[role="menuitem"]:not(:disabled)'));
  if (!items.length) return;
  if (event.key === 'Escape') { event.preventDefault(); closeSessionMenu(true); return; }
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
  event.preventDefault();
  let index = items.indexOf(document.activeElement);
  if (event.key === 'Home') index = 0;
  else if (event.key === 'End') index = items.length - 1;
  else index = (Math.max(0, index) + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
  items[index].focus();
}

document.addEventListener('click', event => {
  if (!openMenuBtn) return;
  const row = openMenuBtn.parentElement;
  if (row && row.contains(event.target)) return;
  closeSessionMenu(false);
});

async function renameSession(id, el) {
  const item = el.closest('.session-item');
  const nameEl = item ? item.querySelector('.session-item-name') : null;
  const current = nameEl ? nameEl.textContent : '';
  const next = prompt('Rename session', current);
  if (!next || !next.trim() || next === current) {
    toggleSessionMenu(el.closest('.session-item').querySelector('.session-more'));
    return;
  }
  try {
    const res = await fetch('/api/sessions/rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, title: next })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'rename: ' + res.status);
    if (nameEl) nameEl.textContent = data.title || next;
    showToast('Renamed');
  } catch (e) {
    showToast('Rename failed: ' + e.message);
  }
  const btn = el.closest('.session-item').querySelector('.session-more');
  const m = btn.parentElement.querySelector('.session-menu');
  if (m) m.style.display = 'none';
  openMenuBtn = null;
}

async function branchSessionFromSidebar(id, el) {
  if (typeof turnRunning !== 'undefined' && turnRunning) {
    showToast('Stop the current turn before branching');
    return;
  }
  try {
    // -1 is the API's explicit "branch at the latest message" sentinel.
    const res = await fetch('/api/fork', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: id, messageIndex: -1 })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'fork: ' + res.status);
    showToast('Branched');
    await loadSessions();
    await resumeSession(data.sessionId);
  } catch (e) {
    showToast('Branch failed: ' + e.message);
  }
}

async function archiveSession(id, el) {
  if (typeof turnRunning !== 'undefined' && turnRunning && id === currentSessionId) {
    showToast('Stop the current turn before archiving this session');
    return;
  }
  const item = el.closest('.session-item');
  const name = item && item.querySelector('.session-item-name')
    ? item.querySelector('.session-item-name').textContent : 'this session';
  if (!window.confirm('Archive "' + name + '"? You can restore it later.')) return;
  try {
    const res = await fetch('/api/sessions/archive', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, archived: true })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'archive: ' + res.status);
    if (id === currentSessionId) newChat();
    await loadSessions();
    showToast('Session archived');
  } catch (e) {
    showToast('Archive failed: ' + e.message);
  }
}

async function restoreSession(id) {
  try {
    const res = await fetch('/api/sessions/archive', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, archived: false })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'restore: ' + res.status);
    await loadSessions();
    showToast('Session restored');
  } catch (e) {
    showToast('Restore failed: ' + e.message);
  }
}

function closeSessionDeleteDialog(force) {
  if (!sessionDeleteDialog || sessionDeleteDialog.pending && !force) return;
  const state = sessionDeleteDialog;
  sessionDeleteDialog = null;
  state.overlay.remove();
  if (state.trigger && state.trigger.isConnected) state.trigger.focus();
}

function openSessionDeleteDialog(id, title, trigger) {
  if (sessionDeleteDialog) {
    if (sessionDeleteDialog.pending) return;
    closeSessionDeleteDialog(false);
  }
  const record = sessions.find(s => s.id === id);
  title = title || record && record.title || uiText('Untitled session', '\u672a\u547d\u540d\u4f1a\u8bdd');
  if (openMenuBtn) {
    const menu = openMenuBtn.parentElement.querySelector('.session-menu');
    if (menu) menu.style.display = 'none';
    openMenuBtn.setAttribute('aria-expanded', 'false');
    openMenuBtn = null;
  }
  hideSessionDetail();

  const overlay = document.createElement('div');
  overlay.className = 'session-delete-overlay';
  overlay.innerHTML = `
    <div class="session-delete-dialog" role="alertdialog" aria-modal="true" aria-labelledby="sessionDeleteTitle" aria-describedby="sessionDeleteDescription">
      <div class="session-delete-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none"><path d="M4 7h16M9 7V4h6v3m3 0-1 13H7L6 7m4 4v5m4-5v5" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>
      </div>
      <div class="session-delete-copy">
        <h2 id="sessionDeleteTitle">${uiText('Delete session?', '\u5220\u9664\u4f1a\u8bdd\uff1f')}</h2>
        <p class="session-delete-name">${escHtml(title)}</p>
        <p id="sessionDeleteDescription">${uiText('This permanently deletes the conversation, tool history, tasks, checkpoints, traces, and related session data. This action cannot be undone.', '\u8fd9\u5c06\u6c38\u4e45\u5220\u9664\u5bf9\u8bdd\u3001\u5de5\u5177\u5386\u53f2\u3001\u4efb\u52a1\u3001\u68c0\u67e5\u70b9\u3001\u8f68\u8ff9\u53ca\u5176\u4ed6\u4f1a\u8bdd\u6570\u636e\u3002\u6b64\u64cd\u4f5c\u65e0\u6cd5\u64a4\u9500\u3002')}</p>
        <p class="session-delete-error" role="alert" aria-live="assertive" hidden></p>
      </div>
      <div class="session-delete-actions">
        <button type="button" class="session-delete-cancel">${uiText('Cancel', '\u53d6\u6d88')}</button>
        <button type="button" class="session-delete-confirm">${uiText('Delete permanently', '\u6c38\u4e45\u5220\u9664')}</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);

  const state = { id, title, trigger, overlay, pending: false };
  sessionDeleteDialog = state;
  const dialog = overlay.querySelector('.session-delete-dialog');
  const cancel = overlay.querySelector('.session-delete-cancel');
  const confirm = overlay.querySelector('.session-delete-confirm');
  cancel.addEventListener('click', () => closeSessionDeleteDialog(false));
  confirm.addEventListener('click', () => confirmSessionDeletion(state));
  overlay.addEventListener('click', e => {
    if (e.target === overlay) closeSessionDeleteDialog(false);
  });
  dialog.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeSessionDeleteDialog(false);
      return;
    }
    if (e.key !== 'Tab') return;
    const focusable = Array.from(dialog.querySelectorAll('button:not(:disabled)'));
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  });
  requestAnimationFrame(() => cancel.focus());
}

async function confirmSessionDeletion(state) {
  if (!state || sessionDeleteDialog !== state || state.pending) return;
  state.pending = true;
  const cancel = state.overlay.querySelector('.session-delete-cancel');
  const confirm = state.overlay.querySelector('.session-delete-confirm');
  const error = state.overlay.querySelector('.session-delete-error');
  cancel.disabled = true;
  confirm.disabled = true;
  confirm.classList.add('pending');
  confirm.textContent = uiText('Deleting…', '\u6b63\u5728\u5220\u9664…');
  error.hidden = true;
  error.textContent = '';

  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(state.id), { method: 'DELETE' });
    const raw = await res.text();
    let data = {};
    if (raw) {
      try { data = JSON.parse(raw); } catch (_) { data = {}; }
    }
    if (!res.ok) {
      const failure = new Error(data.error || uiText('Delete failed (HTTP ', '\u5220\u9664\u5931\u8d25\uff08HTTP ') + res.status + ')');
      // Active deletion crosses to a fresh replacement before removing
      // sidecars. Even if a later sidecar fails, the browser must follow the
      // backend's authoritative active session instead of posting to the old
      // (partially deleted) transcript.
      failure.activeSessionId = data.activeSessionId || null;
      throw failure;
    }

    const deletedActive = state.id === currentSessionId;
    sessions = sessions.filter(s => s.id !== state.id);
    const oldOrder = Array.isArray(desktopPreferences.sessionOrder) ? desktopPreferences.sessionOrder : [];
    const nextOrder = oldOrder.filter(id => id !== state.id);
    if (nextOrder.length !== oldOrder.length) {
      const saved = await saveDesktopPreferencesPatch({ sessionOrder: nextOrder });
      // The deleted id must not remain in this window even if preference
      // persistence is temporarily unavailable.
      if (!saved) desktopPreferences.sessionOrder = nextOrder;
    }

    if (deletedActive) {
      // The backend may atomically activate a fresh replacement. Build the
      // clean new-chat surface first, then keep that replacement selected so
      // the next message is attached to the server's active session.
      currentSessionId = null;
      newChat();
      currentSessionId = data.activeSessionId || null;
      renderSessions();
    } else {
      renderSessions();
    }
    await loadSessions(false);
    closeSessionDeleteDialog(true);
    showToast(uiText('Session deleted permanently', '\u4f1a\u8bdd\u5df2\u6c38\u4e45\u5220\u9664'));
  } catch (e) {
    if (e.activeSessionId && state.id === currentSessionId) {
      currentSessionId = null;
      newChat();
      currentSessionId = e.activeSessionId;
      renderSessions();
      loadSessions(false).catch(() => {});
    }
    state.pending = false;
    cancel.disabled = false;
    confirm.disabled = false;
    confirm.classList.remove('pending');
    confirm.textContent = uiText('Delete permanently', '\u6c38\u4e45\u5220\u9664');
    error.textContent = e.message || uiText('Unable to delete this session.', '\u65e0\u6cd5\u5220\u9664\u8be5\u4f1a\u8bdd\u3002');
    error.hidden = false;
    confirm.focus();
  }
}

// Render the DSH-style bottom stats bar (turns/steps/time/tokens).
function renderSessionStatsbar(data) {
  const bar = document.getElementById('sessionStatsbar');
  if (!bar) return;
  const st = data.stats || {};
  if (!st.turns && !st.toolCalls && !st.inputTokens) {
    bar.style.display = 'none';
    return;
  }
  const parts = [];
  parts.push({ text: st.turns + uiText(' turns \u00B7 ', '\u8F6E \u00B7 ') + (st.steps || 0) + uiText(' steps', '\u6B65'), title: uiText('Conversation turns and recorded user/model/tool steps', '\u4F1A\u8BDD\u8F6E\u6570\u4E0E\u8BB0\u5F55\u7684\u7528\u6237/\u6A21\u578B/\u5DE5\u5177\u6B65\u9AA4\u6570') });
  const durParts = [];
  if (st.llmMs > 0) durParts.push('LLM ' + fmtMs(st.llmMs));
  if (st.toolMs > 0) durParts.push(uiText('Tools ', '\u5DE5\u5177\u8C03\u7528 ') + fmtMs(st.toolMs));
  if (durParts.length) parts.push({ text: durParts.join(' \u00B7 '), title: uiText('Recorded cumulative LLM and tool-call time', 'LLM \u4E0E\u5DE5\u5177\u8C03\u7528\u8BB0\u5F55\u7684\u7D2F\u8BA1\u8017\u65F6') });
  const speedParts = [];
  if (st.ttftAverageMs > 0) {
    speedParts.push(uiText('Avg first token ', '\u9996 token \u5E73\u5747 ') + fmtMs(st.ttftAverageMs));
  }
  const tokPerSec = Number(st.tokPerSec) || 0;
  if (tokPerSec > 0) {
    const formattedTokPerSec = tokPerSec >= 10 ? Math.round(tokPerSec) : Math.round(tokPerSec * 10) / 10;
    speedParts.push(formattedTokPerSec + ' tok/s');
  }
  if (speedParts.length) parts.push({ text: speedParts.join(' \u00B7 '), title: uiText('TTFT is request-to-first-text-token latency; tok/s is output generation speed', 'TTFT \u662F\u8BF7\u6C42\u5230\u9996\u4E2A\u6587\u672C token \u7684\u5E73\u5747\u65F6\u95F4\uFF1Btok/s \u662F\u8F93\u51FA\u751F\u6210\u901F\u7387') });
  if (st.cacheHitRate > 0) {
    parts.push({ text: uiText('Cache hit ', '\u7F13\u5B58\u547D\u4E2D ') + st.cacheHitRate.toFixed(0) + '%', title: uiText('Cache-read tokens as a share of total input tokens', '\u7F13\u5B58\u8BFB\u53D6 token \u5360\u8F93\u5165\u4E0E\u7F13\u5B58 token \u7684\u6BD4\u4F8B') });
  }
  if (st.inputTokens || st.outputTokens) {
    parts.push({ text: uiText('Input ', '\u8F93\u5165 ') + fmtTokens(st.inputTokens) + ' tok \u00B7 ' + uiText('Output ', '\u8F93\u51FA ') + fmtTokens(st.outputTokens) + ' tok', title: uiText('Cumulative conversation token usage (tok)', '\u4F1A\u8BDD\u7D2F\u8BA1 token \u7528\u91CF\uFF08tok\uFF09') });
  }
  const text = parts.map(p => p.text).join(' | ');
  bar.innerHTML = parts.map((p, i) => `${i ? '<span class="stats-separator" aria-hidden="true">|</span>' : ''}<span class="stats-metric" title="${escAttr(p.title)}">${escHtml(p.text)}</span>`).join('');
  bar.setAttribute('aria-label', text);
  bar.style.display = 'block';
}

async function loadSessionStatsbar() {
  if (!currentSessionId) return;
  try {
    const res = await fetch('/api/trace?sessionId=' + encodeURIComponent(currentSessionId));
    const data = await res.json();
    if (res.ok) renderSessionStatsbar(data);
  } catch (_) { /* stats are best-effort */ }
}

function toggleSessionSearch() {
  const wrap = document.getElementById('sbSearch');
  const input = document.getElementById('sessionSearchInput');
  if (!wrap) return;
  const open = wrap.classList.toggle('open');
  if (open) {
    input.value = '';
    onSessionSearch('');
    input.focus();
  } else {
    input.value = '';
    onSessionSearch('');
  }
}

// Collapse the search capsule when it loses focus while empty.
document.addEventListener('click', function (e) {
  const wrap = document.getElementById('sbSearch');
  if (!wrap || !wrap.classList.contains('open')) return;
  if (wrap.contains(e.target)) return;
  const input = document.getElementById('sessionSearchInput');
  if (input && !input.value.trim()) {
    wrap.classList.remove('open');
    onSessionSearch('');
  }
});

function onSessionSearch(q) {
  sessionFilter = (q || '').trim().toLowerCase();
	clearTimeout(sessionSearchTimer);
	sessionSearchTimer = setTimeout(() => loadSessions(false), 180);
}

function sessionMatches(s) {
  if (!sessionFilter) return true;
  const ws = workspaceForSession(s);
  return [s.title, s.id, s.model, s.workDir, ws && ws.name, ws && ws.path]
    .some(v => String(v || '').toLowerCase().includes(sessionFilter));
}

function relativeTime(ts) {
  const diff = Date.now() - new Date(ts).getTime();
  if (diff < 60000) return '\u521A\u521A';
  if (diff < 3600000) return Math.floor(diff / 60000) + '\u5206\u949F';
  if (diff < 86400000) return Math.floor(diff / 3600000) + '\u5C0F\u65F6';
  return Math.floor(diff / 86400000) + '\u5929';
}

let sessionsExpanded = false;

function sortSessionItems(items) {
  const out = items.slice();
	if (desktopPreferences.sidebarSort === 'manual') {
	  const positions = new Map((desktopPreferences.sessionOrder || []).map((id, i) => [id, i]));
	  return out.sort((a, b) => {
		const ai = positions.has(a.id) ? positions.get(a.id) : Number.MAX_SAFE_INTEGER;
		const bi = positions.has(b.id) ? positions.get(b.id) : Number.MAX_SAFE_INTEGER;
		if (ai !== bi) return ai - bi;
		return new Date(b.updatedAt || b.createdAt) - new Date(a.updatedAt || a.createdAt);
	  });
	}
  if (desktopPreferences.sidebarSort === 'name') {
    return out.sort((a, b) => String(a.title || '').localeCompare(String(b.title || ''), undefined, { sensitivity: 'base' }));
  }
  return out.sort((a, b) => new Date(b.updatedAt || b.createdAt) - new Date(a.updatedAt || a.createdAt));
}

function workspaceLabel(workDir) {
  const clean = String(workDir || '').replace(/[\\/]+$/, '');
  if (!clean) return (lastStatusSnapshot && lastStatusSnapshot.workspace) || 'metis';
  const parts = clean.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || clean;
}

function renderWorkspaceHeader(ws, count, index) {
  const unavailable = ws.available === false;
  const active = ws.id === activeWorkspaceId || ws.active;
  const pathTitle = unavailable ? ws.path + ' (unavailable)' : ws.path;
  return `<div class="ws-group-head${active ? ' active' : ''}${unavailable ? ' unavailable' : ''}" title="${escAttr(pathTitle)}">
    <button type="button" class="ws-group-open" aria-label="${active ? 'Current workspace ' : 'Open workspace '}${escAttr(ws.name)}${active ? '' : ' in a new window'}" onclick="openWorkspace('${escOnclick(ws.id)}')">
      <svg class="ws-folder" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M2 4.5A1.5 1.5 0 0 1 3.5 3h2.6l1.5 1.8h4.9A1.5 1.5 0 0 1 14 6.3v5.2a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 2 11.5z"/></svg>
      <span class="ws-group-name">${escHtml(ws.name)}</span>
      <span class="ws-group-count">${count}</span>
    </button>
    <button type="button" class="ws-more" aria-label="Workspace actions for ${escAttr(ws.name)}" aria-expanded="false" onclick="event.stopPropagation();toggleWorkspaceMenu(this)">&#8943;</button>
    <div class="workspace-menu" style="display:none">
      <button type="button" onclick="event.stopPropagation();openWorkspace('${escOnclick(ws.id)}')"${active || unavailable ? ' disabled' : ''}>Open in new window</button>
      <button type="button" onclick="event.stopPropagation();renameWorkspace('${escOnclick(ws.id)}')">Rename</button>
      <button type="button" onclick="event.stopPropagation();moveWorkspace('${escOnclick(ws.id)}',-1)"${index === 0 ? ' disabled' : ''}>Move up</button>
      <button type="button" onclick="event.stopPropagation();moveWorkspace('${escOnclick(ws.id)}',1)"${index === workspaces.length - 1 ? ' disabled' : ''}>Move down</button>
      <div class="session-menu-sep"></div>
      <button type="button" class="danger" onclick="event.stopPropagation();removeWorkspace('${escOnclick(ws.id)}')"${active ? ' disabled' : ''}>Remove from list</button>
    </div>
  </div>`;
}

function sessionState(s) {
  if (s.archived || showArchivedSessions) return { name: 'archived', label: 'Archived' };
  if (s.id !== currentSessionId) {
    if (s.status === 'running') return { name: 'running', label: 'Interrupted while running' };
    if (s.status === 'failed') return { name: 'failed', label: 'Last turn failed' };
    if (s.status === 'stopped') return { name: 'stopped', label: 'Last turn stopped' };
    if (s.mode === 'plan') return { name: 'plan', label: s.status === 'completed' ? 'Plan session completed' : 'Plan session' };
    if (s.status === 'completed') return { name: 'done', label: 'Completed' };
    return { name: 'done', label: 'Idle' };
  }
  if (typeof pendingAsk !== 'undefined' && pendingAsk) return { name: 'waiting', label: 'Waiting for your answer' };
  if (document.querySelector('.perm-card:not(.approved):not(.denied)')) return { name: 'approval', label: 'Waiting for approval' };
  if (typeof turnRunning !== 'undefined' && turnRunning) {
    if (lastStatusSnapshot && Number(lastStatusSnapshot.subAgents) > 0) return { name: 'delegating', label: 'Sub-agents running' };
    return { name: 'running', label: 'Running' };
  }
  if (s.status === 'running') return { name: 'running', label: 'Interrupted while running' };
  if (s.status === 'failed') return { name: 'failed', label: 'Last turn failed' };
  if (s.status === 'stopped') return { name: 'stopped', label: 'Last turn stopped' };
  if (s.mode === 'plan') return { name: 'plan', label: 'Plan session' };
  if (s.status === 'completed') return { name: 'done', label: 'Completed' };
  return { name: 'done', label: 'Idle' };
}

function sessionItemKeydown(e, id) {
  if (e.target !== e.currentTarget) return;
  if (e.key !== 'Enter' && e.key !== ' ') return;
  e.preventDefault();
  resumeSession(id);
}

function renderSessionItem(s) {
  const time = relativeTime(s.updatedAt || s.createdAt);
  const state = sessionState(s);
		const detail = `data-detail-title="${escAttr(s.title || 'Untitled')}" data-detail-path="${escAttr(s.workDir || '')}" data-detail-model="${escAttr(s.model || '')}" data-detail-meta="${escAttr([s.status || 'idle', s.preset || 'standard', s.mode || '', s.effort || 'default effort', new Date(s.updatedAt || s.createdAt).toLocaleString()].filter(Boolean).join(' · '))}"`;
  if (showArchivedSessions) {
	return `<div class="session-item archived" ${detail} tabindex="0">
      <span class="session-item-status archived" title="Archived" aria-label="Archived"></span>
      <span class="session-item-name">${escHtml(s.title)}</span>
      <span class="session-item-time">${time}</span>
    <button class="session-more" aria-label="Session actions for ${escAttr(s.title)}" aria-haspopup="menu" aria-expanded="false" onclick="event.stopPropagation();toggleSessionMenu(this)">&#8943;</button>
      <div class="session-menu" role="menu" aria-label="${uiText('Session actions', '会话操作')}" style="display:none" onkeydown="sessionMenuKeydown(event)">
        <button type="button" role="menuitem" class="session-menu-item" onclick="event.stopPropagation();restoreSession('${escOnclick(s.id)}')">&#8634; ${uiText('Restore session', '\u6062\u590d\u4f1a\u8bdd')}</button>
        <div class="session-menu-sep" role="separator"></div>
        <button type="button" role="menuitem" class="session-menu-item danger" onclick="event.stopPropagation();openSessionDeleteDialog('${escOnclick(s.id)}','',this)">&#128465; ${uiText('Delete session', '\u5220\u9664\u4f1a\u8bdd')}</button>
      </div>
    </div>`;
  }
  return `<div class="session-item${s.id === currentSessionId ? ' active' : ''}" role="button" tabindex="0" aria-label="Open session ${escAttr(s.title)}" ${detail} data-state="${state.name}" onclick="resumeSession('${escOnclick(s.id)}')" onkeydown="sessionItemKeydown(event,'${escOnclick(s.id)}')">
    <span class="session-item-status ${state.name}" title="${escAttr(state.label)}" aria-label="${escAttr(state.label)}"></span>
    <span class="session-item-name">${escHtml(s.title)}</span>
    <span class="session-item-time">${time}</span>
    <button class="session-more" aria-label="Session actions for ${escAttr(s.title)}" aria-haspopup="menu" aria-expanded="false" onclick="event.stopPropagation();toggleSessionMenu(this)">&#8943;</button>
    <div class="session-menu" role="menu" aria-label="${uiText('Session actions', '会话操作')}" style="display:none" onkeydown="sessionMenuKeydown(event)">
      <button type="button" role="menuitem" class="session-menu-item" onclick="event.stopPropagation();renameSession('${escOnclick(s.id)}', this)">&#9998;&#65039; ${uiText('Rename', '重命名')}</button>
      <button type="button" role="menuitem" class="session-menu-item" onclick="event.stopPropagation();branchSessionFromSidebar('${escOnclick(s.id)}', this)">&#9850; ${uiText('Branch session', '分叉会话')}</button>
	  <button type="button" role="menuitem" class="session-menu-item" onclick="event.stopPropagation();moveSession('${escOnclick(s.id)}',-1)">${uiText('Move up', '上移')}</button>
	  <button type="button" role="menuitem" class="session-menu-item" onclick="event.stopPropagation();moveSession('${escOnclick(s.id)}',1)">${uiText('Move down', '下移')}</button>
      <div class="session-menu-sep" role="separator"></div>
      <button type="button" role="menuitem" class="session-menu-item danger" onclick="event.stopPropagation();archiveSession('${escOnclick(s.id)}', this)">&#128230; ${uiText('Archive session', '归档会话')}</button>
      <button type="button" role="menuitem" class="session-menu-item danger" onclick="event.stopPropagation();openSessionDeleteDialog('${escOnclick(s.id)}','',this)">&#128465; ${uiText('Delete session', '\u5220\u9664\u4f1a\u8bdd')}</button>
    </div>
  </div>`;
}

function renderSessions() {
  const list = document.getElementById('sessionList');
  if (!sessions.length && (desktopPreferences.sidebarView !== 'grouped' || !workspaces.length)) {
    list.innerHTML = '<div style="padding:12px;color:var(--text-muted);font-size:13px;">' +
      (showArchivedSessions ? 'No archived sessions' : 'No sessions yet') + '</div>';
    return;
  }
  const visibleWorkspaceIDs = workspaces.length ? new Set(workspaces.map(w => w.id)) : null;
  const sorted = sortSessionItems(sessions.filter(s => {
    if (visibleWorkspaceIDs && s.workspaceId && !visibleWorkspaceIDs.has(s.workspaceId)) return false;
    return sessionMatches(s);
  }));
  if (!sorted.length && (desktopPreferences.sidebarView !== 'grouped' || sessionFilter)) {
    list.innerHTML = '<div style="padding:12px;color:var(--text-muted);font-size:13px;">No matching sessions</div>';
    return;
  }

  const COLLAPSE = 5;
  const grouped = desktopPreferences.sidebarView === 'grouped';
  const visible = grouped || sessionsExpanded ? sorted : sorted.slice(0, COLLAPSE);
  let hiddenCount = grouped ? 0 : Math.max(0, sorted.length - visible.length);
  let hasCollapsibleGroup = false;
  let html = '';
  if (grouped) {
    const groups = new Map();
    workspaces.forEach(ws => groups.set(ws.id, { workspace: ws, items: [] }));
    visible.forEach(s => {
      const key = s.workspaceId || s.workDir || '';
      if (!groups.has(key)) {
        groups.set(key, { workspace: {
          id: key, path: s.workDir || '', name: workspaceLabel(s.workDir),
          available: true, active: false, registered: false
        }, items: [] });
      }
      groups.get(key).items.push(s);
    });
    let groupIndex = 0;
    groups.forEach(group => {
      if (sessionFilter && !group.items.length) return;
      if (group.items.length > COLLAPSE) hasCollapsibleGroup = true;
      const shown = sessionsExpanded ? group.items : group.items.slice(0, COLLAPSE);
      hiddenCount += Math.max(0, group.items.length - shown.length);
      html += renderWorkspaceHeader(group.workspace, group.items.length, groupIndex++);
      shown.forEach(s => { html += renderSessionItem(s); });
    });
  } else {
    visible.forEach(s => { html += renderSessionItem(s); });
  }
  if (hiddenCount > 0 || sessionsExpanded && (grouped ? hasCollapsibleGroup : sorted.length > COLLAPSE)) {
    html += `<button type="button" class="session-expand" onclick="toggleSessionsExpand()">${sessionsExpanded ? '\u6536\u8D77' : '\u5C55\u5F00\u5176\u4F59 ' + hiddenCount + ' \u4E2A\u4F1A\u8BDD'}</button>`;
  }
	if (sessionsNextCursor) {
	  html += `<button type="button" class="session-load-more" onclick="loadMoreSessions()">Load more (${Math.max(0, sessionsTotal - sessions.length)} remaining)</button>`;
	}
  list.innerHTML = html;
}

async function moveSession(id, delta) {
	const ordered = sortSessionItems(sessions).map(s => s.id);
	const from = ordered.indexOf(id), to = from + delta;
	if (from < 0 || to < 0 || to >= ordered.length) return;
	const moved = ordered.splice(from, 1)[0];
	ordered.splice(to, 0, moved);
	if (await saveDesktopPreferencesPatch({ sidebarSort: 'manual', sessionOrder: ordered })) {
	  sessionsExpanded = true;
	  renderSessions();
	  syncSessionViewMenu();
	}
}

let sessionDetailCard = null;
function showSessionDetail(item) {
	if (!item || !item.dataset.detailTitle) return;
	if (!sessionDetailCard) {
	  sessionDetailCard = document.createElement('div');
	  sessionDetailCard.className = 'session-detail-card';
	  document.body.appendChild(sessionDetailCard);
	}
	sessionDetailCard.innerHTML = `<strong>${escHtml(item.dataset.detailTitle)}</strong><span>${escHtml(item.dataset.detailPath || 'No workspace path')}</span><span>${escHtml(item.dataset.detailModel || 'No model')}</span><small>${escHtml(item.dataset.detailMeta || '')}</small>`;
	const rect = item.getBoundingClientRect();
	const left = Math.min(window.innerWidth - 330, rect.right + 8);
	sessionDetailCard.style.left = Math.max(8, left) + 'px';
	sessionDetailCard.style.top = Math.max(8, Math.min(window.innerHeight - 150, rect.top)) + 'px';
	sessionDetailCard.classList.add('show');
}
function hideSessionDetail() { if (sessionDetailCard) sessionDetailCard.classList.remove('show'); }
document.addEventListener('mouseover', e => { const item = e.target.closest && e.target.closest('.session-item[data-detail-title]'); if (item) showSessionDetail(item); });
document.addEventListener('mouseout', e => { if (e.target.closest && e.target.closest('.session-item[data-detail-title]')) hideSessionDetail(); });
document.addEventListener('focusin', e => { const item = e.target.closest && e.target.closest('.session-item[data-detail-title]'); if (item) showSessionDetail(item); });
document.addEventListener('focusout', e => { if (e.target.closest && e.target.closest('.session-item[data-detail-title]')) hideSessionDetail(); });

function toggleSessionsExpand() {
  sessionsExpanded = !sessionsExpanded;
  renderSessions();
}

async function resumeSession(id) {
  if (typeof turnRunning !== 'undefined' && turnRunning && id !== currentSessionId) {
    showToast('Stop the current turn before switching sessions');
    return;
  }
  try {
    const res = await fetch('/api/sessions/activate', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id })
    });
    if (!res.ok) throw new Error(`resume: ${res.status}`);
    const data = await res.json();
    currentSessionId = id;
    if (currentView === 'trace') loadTrace();
    loadSessionStatsbar();
    messages = [];
    streamedTextThisTurn = false;
    renderHistoryMessages(data.messages);
    document.getElementById('topbarTitle').textContent = data.session.title || 'Session';
	const preset = document.getElementById('presetName');
	if (preset) preset.textContent = presetDisplayName(data.session.preset || 'standard');
	if (typeof loadEffort === 'function') await loadEffort();
    renderSessions();
  } catch (e) {
    showError('Unable to resume this session.');
  }
}
