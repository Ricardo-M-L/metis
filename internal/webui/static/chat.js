// Metis Desktop - live chat: SSE stream, message sending, composer,
// model cycling, approval mode and the settings overlay.
// Shared state and escHtml/escAttr helpers live in app.js.

// --- SSE Real-time Event Stream ---
// The server broadcasts agent events over Server-Sent Events. Events are
// rendered incrementally so the chat updates live instead of waiting for
// the turn to finish.
let eventSource = null;
let streaming = false;
let streamMsgIdx = -1;
let turnRunning = false;
let runningSessionId = null;
let stopRequestPending = false;
let runningTurnNeedsHistorySync = false;
let queuedTurns = [];
let queuedSessionId = null;
let drainingQueuedTurns = false;
let streamingEl = null;
let streamingText = '';
let streamedTextThisTurn = false;
let effortState = { supported: false, effort: 'default', options: [] };
let effortRequestGeneration = 0;
const CHAT_BOTTOM_THRESHOLD = 56;
let followOutput = true;
let programmaticChatScroll = false;
let chatScrollFrame = 0;
const seenLiveEventIDs = new Set();
const seenLiveEventQueue = [];

function acceptLiveEvent(e) {
  const id = String(e.lastEventId || '');
  if (!id) return true;
  if (seenLiveEventIDs.has(id)) return false;
  seenLiveEventIDs.add(id);
  seenLiveEventQueue.push(id);
  if (seenLiveEventQueue.length > 4096) seenLiveEventIDs.delete(seenLiveEventQueue.shift());
  return true;
}

function onLive(type, handler) {
  eventSource.addEventListener(type, e => {
    if (!acceptLiveEvent(e)) return;
    try {
      const data = JSON.parse(e.data);
      if (sameSession(data)) handler(data);
    } catch (_) {}
  });
}

function connectEvents() {
  if (eventSource) { eventSource.close(); eventSource = null; }
  if (!window.EventSource) return;

  eventSource = new EventSource('/api/events');

  eventSource.addEventListener('ready', e => {
    hideReconnectBanner();
    try { if (JSON.parse(e.data).replayReset) showToast('Live event history expired. The saved session will refresh after the turn.'); } catch (_) {}
  });
  onLive('text_delta', handleTextDelta);
  onLive('tool_start', handleToolStart);
  onLive('tool_result', handleToolResult);
  onLive('tool_args_delta', handleToolArgsDelta);
  onLive('thinking_delta', handleThinkingDelta);
  onLive('tokens', handleTokensEvent);
  onLive('context_warn', d => handleContextEvent(d));
  onLive('context_compacted', d => handleContextEvent(d, true));
  onLive('compaction_start', handleCompactionStart);
  onLive('compaction_progress', handleCompactionProgress);
  onLive('compaction_end', handleCompactionEnd);
  onLive('ask_user', handleAskUser);
  onLive('redacted_thinking', () => handleRedactedThinking());
  onLive('permission_request', handlePermissionRequest);
  onLive('turn_end', endStreamingMessage);
  onLive('loop_done', finishUserTurn);
  onLive('agent_error', d => showToast('Agent error: ' + (d.message || 'request failed')));
  eventSource.addEventListener('error', () => {
    showReconnectBanner();
  });
}

async function loadEffort(shouldApply = () => true) {
  const generation = ++effortRequestGeneration;
  try {
    const res = await fetch('/api/effort');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'effort: ' + res.status);
    if (generation !== effortRequestGeneration || !shouldApply()) return;
    effortState = data;
    const btn = document.getElementById('effortBtn');
    if (!btn) return;
    btn.style.display = data.supported ? '' : 'none';
    btn.title = data.supported ? 'Reasoning effort for the next model request' : (data.reason || 'Reasoning effort unavailable');
    const label = document.getElementById('effortName');
    if (label) label.textContent = data.effort === 'default' ? 'Default effort' : data.effort.charAt(0).toUpperCase() + data.effort.slice(1);
    paintEffortMenu();
  } catch (_) {
    if (generation !== effortRequestGeneration || !shouldApply()) return;
    const btn = document.getElementById('effortBtn');
    if (btn) btn.style.display = 'none';
  }
}

function paintEffortMenu() {
  const menu = document.getElementById('effortMenu');
  if (!menu) return;
  const labels = { default: 'Default', low: 'Low', medium: 'Medium', high: 'High' };
  menu.innerHTML = (effortState.options || []).map(value => `<button type="button" role="menuitemradio" aria-checked="${effortState.effort === value ? 'true' : 'false'}" class="${effortState.effort === value ? 'selected' : ''}" onclick="chooseEffort('${value}')">${labels[value] || escHtml(value)}</button>`).join('');
}

function toggleEffortMenu(e) {
  if (e) e.stopPropagation();
  const menu = document.getElementById('effortMenu');
  const btn = document.getElementById('effortBtn');
  if (!menu || !btn) return;
  const open = menu.style.display === 'none';
  menu.style.display = open ? 'block' : 'none';
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

async function chooseEffort(value) {
  try {
    const res = await fetch('/api/effort', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ effort: value }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'effort: ' + res.status);
    effortState.effort = data.effort;
    document.getElementById('effortMenu').style.display = 'none';
    document.getElementById('effortBtn').setAttribute('aria-expanded', 'false');
    await loadEffort();
    showToast('Reasoning effort: ' + data.effort);
  } catch (e) {
    showToast('Effort change failed: ' + e.message);
  }
}

// Events carry the session they belong to; the running session and the
// transcript currently being viewed are intentionally separate. The first
// event also reveals the id assigned to a brand-new session before its POST
// resolves, so sidebar navigation can safely background it.
function sameSession(d) {
  if (turnRunning && d && d.session && !runningSessionId) {
    runningSessionId = d.session;
    renderSessions();
  }
  return !(d && d.session && currentSessionId && d.session !== currentSessionId);
}

let thinkingEl = null;
let thinkingText = '';
let thinkStartMs = 0;

const THINK_ORBIT_ICON = '<svg class="think-orbit-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.1" aria-hidden="true"><ellipse cx="8" cy="8" rx="6.2" ry="2.55"/><ellipse cx="8" cy="8" rx="6.2" ry="2.55" transform="rotate(60 8 8)"/><ellipse cx="8" cy="8" rx="6.2" ry="2.55" transform="rotate(120 8 8)"/><circle cx="8" cy="8" r="1.1" fill="currentColor" stroke="none"/></svg>';
const THINK_LOCK_ICON = '<svg class="think-lock-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3.5" y="7" width="9" height="6.5" rx="1.5"/><path d="M5.5 7V5.3a2.5 2.5 0 0 1 5 0V7"/></svg>';
const REDACTED_THINKING_PLACEHOLDER = 'Reasoning redacted by provider';

function thinkPreview(text, running) {
  const normalized = String(text || '').trim();
  if (!normalized) return '';
  const lines = normalized.split('\n');
  return (running ? lines[lines.length - 1] : lines[0]).trim();
}

// One renderer is shared by live streaming and persisted-history replay so
// the disclosure semantics do not drift between the two paths.
function appendThinkingRow(text, options = {}) {
  const area = document.getElementById('chatArea');
  const running = !!options.running;
  const redacted = !!options.redacted;
  const runningAttr = running ? ' data-running="true"' : '';
  // Use a real button for interactive reasoning rows. WKWebView can expose a
  // div[role=button] correctly to the accessibility tree while failing to
  // paint its nested label during long, composited chat scrolls. A native
  // button avoids that WebKit-only blank-row failure and gives Enter/Space
  // activation without custom keyboard emulation.
  const headTag = redacted ? 'div' : 'button';
  const headAttrs = redacted
    ? ''
    : ' type="button" aria-expanded="false" onclick="toggleThinkRow(this)"';
  area.insertAdjacentHTML('beforeend', `
    <div class="think-row${redacted ? ' redacted' : ''}"${runningAttr}>
      <${headTag} class="think-head"${headAttrs}>
        <span class="think-leading">
          <span class="think-icon-idle">${redacted ? THINK_LOCK_ICON : THINK_ORBIT_ICON}</span>
          ${redacted ? '' : '<svg class="think-chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6.5 5 3 3-3 3"/></svg>'}
        </span>
        <span class="think-title">Think</span>
        <span class="think-summary"></span>
        <span class="think-time"></span>
      </${headTag}>
      ${redacted ? '' : '<div class="think-body"></div>'}
    </div>`);
  const row = area.lastElementChild;
  const body = row.querySelector('.think-body');
  const summary = row.querySelector('.think-summary');
  if (body) body.textContent = String(text || '');
  if (summary) summary.textContent = redacted
    ? REDACTED_THINKING_PLACEHOLDER
    : thinkPreview(text, running);
  return row;
}

// Thinking disclosure (DSH ReasoningRow parity): collapsed "Think" row that
// accumulates reasoning deltas, with a sweep shimmer while running.
function handleThinkingDelta(d) {
  if (turnStartMs && !turnFirstTokenMs) turnFirstTokenMs = Date.now();
  if (!thinkingEl) {
    thinkStartMs = Date.now();
    thinkingEl = appendThinkingRow('', { running: true });
  }
  thinkingText += d.delta || '';
  const body = thinkingEl.querySelector('.think-body');
  if (body) body.textContent = thinkingText;
  // DSH parity: while streaming, the collapsed row previews the LATEST line.
  const summary = thinkingEl.querySelector('.think-summary');
  if (summary) summary.textContent = thinkPreview(thinkingText, true);
  autoScroll();
}

// Safety-filtered reasoning: show the DSH-style locked placeholder row.
function handleRedactedThinking() {
  // Redacted provider blocks are atomic and independent from plaintext
  // reasoning. Never attach their opaque payload to the DOM.
  finishThinking();
  appendThinkingRow('', { redacted: true });
  autoScroll();
}

function toggleThinkRow(head) {
  const row = head.parentElement;
  if (!row.querySelector('.think-body')) return;
  const open = row.classList.toggle('open');
  head.setAttribute('aria-expanded', String(open));
}

function thinkRowKeydown(event, head) {
  if (event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
  event.preventDefault();
  toggleThinkRow(head);
}

function finishThinking() {
  if (thinkingEl) {
    thinkingEl.removeAttribute('data-running');
    // DSH parity: once done, the collapsed preview shows the FIRST line.
    const summary = thinkingEl.querySelector('.think-summary');
    if (summary) summary.textContent = thinkPreview(thinkingText, false);
    if (thinkStartMs) {
      const t = thinkingEl.querySelector('.think-time');
      if (t) t.textContent = fmtMs(Date.now() - thinkStartMs);
      thinkStartMs = 0;
    }
    thinkingEl = null;
    thinkingText = '';
  }
}

let todoPlanItems = [];
let todoPlanOpen = false;

function isTodoWriteTool(name) {
  const normalized = String(name || '').toLowerCase().replace(/[^a-z]/g, '');
  return normalized === 'todowrite';
}

function normalizeTodoItems(value, base = todoPlanItems) {
  let parsed = value;
  if (typeof value === 'string') {
    try { parsed = JSON.parse(value); }
    catch (e) { return null; }
  }
  if (!parsed || typeof parsed !== 'object') return null;

  const clean = (item) => {
    if (!item || typeof item !== 'object') return null;
    const content = String(item.content || '').trim();
    const status = String(item.status || 'pending').toLowerCase();
    if (!content || !['pending', 'in_progress', 'completed'].includes(status)) return null;
    return { content: content, status: status };
  };

  if (Array.isArray(parsed.todos)) {
    const todos = parsed.todos.map(clean);
    return todos.every(Boolean) ? todos : null;
  }

  const single = clean(parsed);
  if (!single) return null;
  const next = base.map(item => ({ content: item.content, status: item.status }));
  const key = single.content.toLocaleLowerCase();
  const existing = next.findIndex(item => item.content.toLocaleLowerCase() === key);
  if (existing >= 0) next[existing] = single;
  else next.push(single);
  return next;
}

function todoPlanMetrics(todos = todoPlanItems) {
  const total = todos.length;
  const completed = todos.filter(item => item.status === 'completed').length;
  const active = todos.filter(item => item.status === 'in_progress').length;
  const pending = Math.max(0, total - completed - active);
  const activeIndex = todos.findIndex(item => item.status === 'in_progress');
  const current = total === 0 ? 0 : (activeIndex >= 0 ? activeIndex + 1 : Math.min(completed + 1, total));
  return { total, completed, active, pending, current };
}

function renderTodoPlan() {
  const dock = document.getElementById('todoPlanDock');
  const popover = document.getElementById('todoPlanPopover');
  const trigger = document.getElementById('todoPlanTrigger');
  const step = document.getElementById('todoPlanStepLabel');
  const counts = document.getElementById('todoPlanCounts');
  const list = document.getElementById('todoPlanList');
  if (!dock || !popover || !trigger || !step || !counts || !list) return;

  const metrics = todoPlanMetrics();
  if (metrics.total === 0) {
    dock.hidden = true;
    popover.hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
    return;
  }

  dock.hidden = false;
  const finished = metrics.completed === metrics.total;
  step.textContent = finished
    ? `已完成 ${metrics.completed} / ${metrics.total}`
    : `第 ${metrics.current} / ${metrics.total} 步`;
  counts.textContent = [
    metrics.completed ? `${metrics.completed} 已完成` : '',
    metrics.active ? `${metrics.active} 进行中` : '',
    metrics.pending ? `${metrics.pending} 待处理` : '',
  ].filter(Boolean).join(' · ');
  list.innerHTML = todoPlanItems.map(item => `
    <li class="todo-plan-item" data-status="${escAttr(item.status)}">
      <span class="todo-plan-glyph" aria-hidden="true"></span>
      <span class="todo-plan-item-text">${escHtml(item.content)}</span>
    </li>`).join('');
  popover.hidden = !todoPlanOpen;
  trigger.setAttribute('aria-expanded', todoPlanOpen ? 'true' : 'false');
  dock.classList.toggle('is-open', todoPlanOpen);
}

function applyTodoSnapshot(name, input) {
  if (!isTodoWriteTool(name)) return false;
  const next = normalizeTodoItems(input);
  if (!next) return false;
  todoPlanItems = next;
  renderTodoPlan();
  return true;
}

function clearTodoPlan() {
  todoPlanItems = [];
  todoPlanOpen = false;
  renderTodoPlan();
}

function toggleTodoPlan() {
  if (!todoPlanItems.length) return;
  todoPlanOpen = !todoPlanOpen;
  renderTodoPlan();
}

function restoreTodoPlanFromHistory(history) {
  const messages = Array.isArray(history) ? history : [];
  const failedToolUses = new Set();
  for (const message of messages) {
    const blocks = Array.isArray(message && message.content) ? message.content : [];
    for (const block of blocks) {
      if (block && block.type === 'tool_result' && block.is_error && block.tool_use_id) {
        failedToolUses.add(String(block.tool_use_id));
      }
    }
  }

  let restored = [];
  for (const message of messages) {
    const blocks = Array.isArray(message && message.content)
      ? message.content
      : (typeof (message && message.content) === 'string' ? [{ type: 'text', text: message.content }] : []);
    const startsUserTurn = message && message.role === 'user' && blocks.some(block =>
      block && block.type === 'text' && String(block.text || block.content || '').trim());
    if (startsUserTurn) restored = [];
    for (const block of blocks) {
      if (!block || block.type !== 'tool_use' || !isTodoWriteTool(block.name)) continue;
      if (block.tool_use_id && failedToolUses.has(String(block.tool_use_id))) continue;
      const next = normalizeTodoItems(block.input, restored);
      if (next) restored = next;
    }
  }
  todoPlanItems = restored;
  todoPlanOpen = false;
  renderTodoPlan();
}

let pendingAsk = null;

// Model's mid-turn question (DSH PendingSteering parity): right-aligned
// pending bubble + the composer flips into answer mode.
function handleAskUser(d) {
  const area = document.getElementById('chatArea');
  pendingAsk = { id: d.askId, question: d.question || '', options: d.options || [], allowFreeform: !!d.allowFreeform };
  const optsHtml = (pendingAsk.options || []).map(o =>
    `<button class="ask-option" onclick="answerAskOption(this)">${escHtml(o)}</button>`).join('');
  area.insertAdjacentHTML('beforeend', `
    <div class="message message-user ask-pending">
      <div class="message-bubble">
        <div class="ask-question">${escHtml(pendingAsk.question)}</div>
        ${optsHtml ? '<div class="ask-options">' + optsHtml + '</div>' : ''}
        <div class="ask-answer"></div>
      </div>
    </div>`);
  const input = document.getElementById('inputField');
  input.placeholder = pendingAsk.allowFreeform ? '\u56DE\u590D\u6A21\u578B\u7684\u95EE\u9898\u2026' : '\u9009\u62E9\u4E00\u4E2A\u9009\u9879';
  document.getElementById('inputField').focus();
  renderSessions();
  autoScroll();
}

function answerAskOption(btn) {
  const card = btn.closest('.ask-pending');
  if (card) card.querySelectorAll('.ask-option').forEach(b => b.disabled = true);
  submitAskAnswer(btn.textContent);
}

async function submitAskAnswer(answer) {
  const ask = pendingAsk;
  if (!ask) return;
  const card = document.querySelector('.ask-pending');
  if (card) card.querySelectorAll('.ask-option').forEach(b => b.disabled = true);
  try {
    const res = await fetch('/api/ask', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: ask.id, answer: answer })
    });
    if (!res.ok) throw new Error('ask: ' + res.status);
    pendingAsk = null;
    if (card) {
      const a = card.querySelector('.ask-answer');
      if (a) a.textContent = answer;
      card.classList.remove('ask-pending');
      card.classList.add('ask-answered');
    }
    renderSessions();
  } catch (e) {
    // Keep the question pending so the user can retry; re-enable options.
    if (card) card.querySelectorAll('.ask-option').forEach(b => b.disabled = false);
    showToast('Failed to answer: ' + e.message);
  }
  const input = document.getElementById('inputField');
  input.placeholder = '\u63CF\u8FF0\u4F60\u60F3\u8981\u6784\u5EFA\u7684\u5185\u5BB9';
}

let turnStartMs = 0;
let turnFirstTokenMs = 0;
let turnInTokens = 0;
let turnOutTokens = 0;
let turnStatusEl = null;
let turnStatusTimer = null;
let compactionInFlight = false;
let compactionStatusEl = null;

// "Deep diving..." turn-level status (DSH TurnStatus parity): rides the
// whole running turn and gains a clock after 15s.
function beginTurnStatus() {
  turnStartMs = Date.now();
  turnFirstTokenMs = 0;
  turnInTokens = 0;
  turnOutTokens = 0;
  const area = document.getElementById('chatArea');
  area.insertAdjacentHTML('beforeend',
    '<div class="turn-status" role="status" aria-live="polite">Deep diving...<span class="ts-clock"></span></div>');
  turnStatusEl = area.lastElementChild;
  clearInterval(turnStatusTimer);
  turnStatusTimer = setInterval(() => {
    const ms = Date.now() - turnStartMs;
    if (ms >= 15000 && turnStatusEl) {
      const clock = turnStatusEl.querySelector('.ts-clock');
      if (clock) clock.textContent = ' ' + fmtMs(ms);
    }
  }, 1000);
  autoScroll();
}

function endTurnStatus() {
  clearInterval(turnStatusTimer);
  if (turnStatusEl) { turnStatusEl.remove(); turnStatusEl = null; }
}

// Per-turn StatsLine (DSH parity): Ran for / Input / Output / tok per s.
function showTurnStatsLine() {
  if (turnStartMs) {
    const ms = Date.now() - turnStartMs;
    const generationMs = turnFirstTokenMs ? Math.max(1, Date.now() - turnFirstTokenMs) : ms;
    const tps = generationMs > 1000 ? Math.round(turnOutTokens / (generationMs / 1000)) : 0;
    const ttft = turnFirstTokenMs ? Math.max(0, turnFirstTokenMs - turnStartMs) : 0;
    const area = document.getElementById('chatArea');
    const rows = area.querySelectorAll('.message-assistant .msg-actions');
    const actions = rows.length ? rows[rows.length - 1] : null;
    const metrics = turnMetricsMarkup({ durationMs: ms, ttftMs: ttft, tokPerSec: tps });
    if (actions) {
      const previous = actions.querySelector('.msg-metrics');
      if (previous) previous.remove();
      actions.insertAdjacentHTML('beforeend', metrics);
      actions.classList.add('with-metrics');
    } else {
      area.insertAdjacentHTML('beforeend', `<div class="stats-line">${metrics}</div>`);
    }
    turnStartMs = 0;
    turnFirstTokenMs = 0;
    turnInTokens = 0;
    turnOutTokens = 0;
    autoScroll();
  }
}

function handleTokensEvent(d) {
  turnInTokens += d.inputTokens || 0;
  turnOutTokens += d.outputTokens || 0;
  pollStatus();
}

function setTurnStatusLabel(label) {
  if (!turnStatusEl) return false;
  if (turnStatusEl.firstChild && turnStatusEl.firstChild.nodeType === 3) {
    turnStatusEl.firstChild.nodeValue = label;
  } else {
    turnStatusEl.insertAdjacentText('afterbegin', label);
  }
  return true;
}

function upsertCompactionRow(title, detail, state) {
  const area = document.getElementById('chatArea');
  const nextState = state || 'running';
  const needsNew = !compactionStatusEl || !compactionStatusEl.isConnected || compactionStatusEl.dataset.state !== 'running';
  if (needsNew) {
    const row = document.createElement('div');
    row.className = 'context-row compaction-row open';
    row.setAttribute('role', 'status');
    row.setAttribute('aria-live', 'polite');
    row.innerHTML = `
      <div class="context-head" onclick="toggleContextRow(this)">
        <span class="context-chevron">\u25BE</span>
        <span class="context-title"></span>
      </div>
      <div class="context-body"></div>`;
    area.appendChild(row);
    compactionStatusEl = row;
  }
  compactionStatusEl.dataset.state = nextState;
  compactionStatusEl.classList.toggle('running', nextState === 'running');
  compactionStatusEl.classList.toggle('complete', nextState === 'complete');
  compactionStatusEl.classList.toggle('failed', nextState === 'failed');
  const titleEl = compactionStatusEl.querySelector('.context-title');
  const bodyEl = compactionStatusEl.querySelector('.context-body');
  if (titleEl) titleEl.textContent = title;
  if (bodyEl) bodyEl.textContent = detail || '';
  autoScroll();
  return compactionStatusEl;
}

function handleCompactionStart() {
  compactionInFlight = true;
  upsertCompactionRow(uiText('Compacting context…', '正在压缩上下文…'),
    uiText('Preserving decisions, active work, and important details.', '正在保留关键决定、进行中的工作和重要细节。'), 'running');
}

function handleCompactionProgress(d) {
  if (!compactionInFlight) handleCompactionStart();
  const bytes = Number(d.progressBytes) || 0;
  const suffix = bytes >= 1024 ? ' · ' + Math.round(bytes / 1024) + ' KB' : '';
  upsertCompactionRow(uiText('Compacting context…', '正在压缩上下文…') + suffix,
    uiText('Preserving decisions, active work, and important details.', '正在保留关键决定、进行中的工作和重要细节。'), 'running');
}

function handleCompactionEnd(d) {
  compactionInFlight = false;
  if (d.error) {
    upsertCompactionRow(uiText('Context compaction failed', '上下文压缩失败'), String(d.error), 'failed');
    showToast(uiText('Context compaction failed: ', '上下文压缩失败：') + d.error);
  } else if (compactionStatusEl && compactionStatusEl.dataset.state === 'running') {
    upsertCompactionRow(uiText('Conversation history compacted', '会话历史已压缩'),
      uiText('Important context was preserved for the next request.', '重要上下文已保留，后续请求可继续使用。'), 'complete');
  }
  pollStatus();
}

// Context injection / compaction disclosure rows (DSH ContextInjectionRow).
function handleContextEvent(d, compacted) {
  const area = document.getElementById('chatArea');
  const text = d.info || '';
  const before = Number(d.previousContextTokens) || 0;
  const after = Number(d.contextTokens) || 0;
  const reduction = compacted && before > 0 && after > 0
    ? fmtTokens(before) + ' → ' + fmtTokens(after) + ' tokens'
    : '';
  const title = compacted
    ? uiText('Conversation history compacted', '会话历史已压缩') + (reduction ? ' · ' + reduction : '')
    : uiText('Context', '上下文');
  const detail = [text, reduction].filter(Boolean).join('\n');
  if (compacted) {
    upsertCompactionRow(title, detail || uiText('Important context was preserved for the next request.', '重要上下文已保留，后续请求可继续使用。'), 'complete');
    compactionStatusEl.classList.add('complete');
  } else {
    area.insertAdjacentHTML('beforeend', `
      <div class="context-row">
        <div class="context-head" onclick="toggleContextRow(this)">
          <span class="context-chevron">\u25B8</span>
          <span class="context-title">${escHtml(title)}</span>
        </div>
        <div class="context-body">${escHtml(detail)}</div>
      </div>`);
  }
  // before/after describe the replaceable conversation history only. The
  // status meter also includes fixed system/state/memory/tool-schema overhead,
  // so never overwrite it with the smaller history number; refresh the
  // authoritative full-request estimate instead.
  if (compacted && after > 0) pollStatus();
  autoScroll();
}

function toggleContextRow(head) {
  const row = head.parentElement;
  const open = row.classList.toggle('open');
  head.querySelector('.context-chevron').textContent = open ? '\u25BE' : '\u25B8';
}

async function restoreCompactionHistory(sessionId = currentSessionId, shouldApply = () => true) {
  const requestedSessionId = String(sessionId || '');
  if (!requestedSessionId || typeof coalesceTraceCompactions !== 'function') return;
  try {
    const res = await fetch('/api/trace?sessionId=' + encodeURIComponent(requestedSessionId) + '&limit=2000');
    if (!res.ok) return;
    const data = await res.json();
    if (!shouldApply() || requestedSessionId !== String(currentSessionId || '')) return;
    const completed = coalesceTraceCompactions(data.events || []).filter(ev => ev.kind === 'context_compacted');
    compactionStatusEl = null;
    for (const ev of completed) {
      const timestamp = ev.ts ? new Date(ev.ts).toLocaleString() : '';
      const detail = [
        uiText('This saved session compacted earlier conversation history.', '此已保存会话曾压缩较早的对话历史。'),
        timestamp,
      ].filter(Boolean).join(' · ');
      upsertCompactionRow(uiText('Previous context compaction', '历史上下文压缩'), detail, 'complete');
    }
  } catch (_) {}
}

function handleTextDelta(d) {
  // The first answer token settles the preceding provider reasoning row.
  finishThinking();
  if (turnStartMs && !turnFirstTokenMs) turnFirstTokenMs = Date.now();
  streamedTextThisTurn = true;
  if (!streaming) startStreamingMessage();
  streamingText += d.delta || '';
  if (streamingEl) {
    const box = streamingEl.querySelector('.message-content');
    box.innerHTML = formatContent(visibleTranscriptText(streamingText)) + '<span class="stream-cursor"></span>';
  }
  autoScroll();
}

function syncTurnControls() {
  const sendBtn = document.getElementById('sendBtn');
  const stopBtn = document.getElementById('stopBtn');
  if (sendBtn) sendBtn.style.display = turnRunning ? 'none' : '';
  if (stopBtn) {
    stopBtn.style.display = turnRunning ? '' : 'none';
    stopBtn.disabled = stopRequestPending;
    stopBtn.classList.toggle('stopping', stopRequestPending);
    stopBtn.title = stopRequestPending ? 'Stopping the running turn…' : 'Stop the running turn';
  }
}

function setTurnRunning(running, sessionId) {
  turnRunning = running;
  if (running) {
    if (sessionId) runningSessionId = sessionId;
    else if (!runningSessionId && currentSessionId) runningSessionId = currentSessionId;
  } else {
    runningSessionId = null;
    stopRequestPending = false;
  }
  syncTurnControls();
  renderSessions();
}

// Detach transient DOM state when the user views another transcript. The
// agent turn remains alive and tracked by runningSessionId; only its old live
// rendering pointers are discarded so completion cannot write into session B.
function detachRunningTurnView() {
  if (turnRunning && currentSessionId && currentSessionId === runningSessionId) {
    runningTurnNeedsHistorySync = true;
  }
  if (streamingEl) {
    const caret = streamingEl.querySelector('.stream-cursor');
    if (caret) caret.remove();
  }
  streaming = false;
  streamingEl = null;
  streamingText = '';
  endTurnStatus();
  finishThinking();
  pendingAsk = null;
  toolDetails = {};
  selectedToolId = null;
  closeToolDetail();
}

async function stopTurn() {
  if (!turnRunning || stopRequestPending) return;
  stopRequestPending = true;
  syncTurnControls();
  try {
    const res = await fetch('/api/stop', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: runningSessionId })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'stop: ' + res.status);
    if (data.stopping) showToast('Stopping the running turn…');
    const dropped = queuedTurns.length;
    queuedTurns = [];
    queuedSessionId = null;
    renderQueuedTurns();
    if (dropped) showToast('Stopped; cleared ' + dropped + ' queued message' + (dropped === 1 ? '' : 's'));
  } catch (e) {
    showToast('Stop failed: ' + e.message);
  } finally {
    if (turnRunning) {
      stopRequestPending = false;
      syncTurnControls();
    }
  }
}

function startStreamingMessage() {
  streaming = true;
  streamMsgIdx = messages.length;
  const area = document.getElementById('chatArea');
  // No id here: getElementById returns the FIRST match, so a leftover id
  // from a previous turn would make later turns write into the old
  // message. lastElementChild is the node inserted right above.
  area.insertAdjacentHTML('beforeend', `
    <div class="message message-assistant">
      <div class="message-avatar">M</div>
      <div class="message-body">
        <div class="message-content"><span class="stream-cursor"></span></div>
      </div>
    </div>`);
  streamingEl = area.lastElementChild;
  autoScroll();
}

function endStreamingMessage() {
  finishThinking();
  const visibleText = visibleTranscriptText(streamingText);
  if (streamingEl) {
    const caret = streamingEl.querySelector('.stream-cursor');
    if (caret) caret.remove();
    const box = streamingEl.querySelector('.message-content');
    if (visibleText.trim()) {
      if (box) box.innerHTML = formatContent(visibleText);
      attachMessageActions(streamingEl, streamMsgIdx);
    } else {
      streamingEl.remove();
    }
  }
  if (visibleText.trim()) messages.push({ role: 'assistant', content: visibleText, time: new Date() });
  streaming = false;
  streamingEl = null;
  streamingText = '';
  updateSendBtn();
  loadSessions();
  loadSessionStatsbar();
  if (currentView === 'trace') loadTrace();
}

// A provider turn_end closes one assistant response, but an agent turn can
// continue through tool execution and another model request. Keep the
// TurnStatus alive until loop_done (or the authoritative POST completion).
function finishUserTurn() {
  endStreamingMessage();
  endTurnStatus();
  showTurnStatsLine();
}

// Called at the start of each user turn to reset per-turn stream state.
function beginUserTurn() {
  streamedTextThisTurn = false;
  // If the previous turn's turn_end never arrived (SSE drop/reconnect),
  // its streaming flag is still set and the next answer would stream INTO
  // the previous assistant bubble. Close it first - idempotent when the
  // turn already ended normally.
  finishUserTurn();
  beginTurnStatus();
}

// Reset every piece of in-flight turn state. Called on session switch,
// new chat, and SSE reconnect so nothing from a previous session leaks
// into the next (timer, pending question, tool details, thinking row).
function resetTurnState() {
  clearTodoPlan();
  compactionInFlight = false;
  compactionStatusEl = null;
  endTurnStatus();
  finishThinking();
  if (streamingEl) {
    const caret = streamingEl.querySelector('.stream-cursor');
    if (caret) caret.remove();
  }
  streaming = false;
  streamingEl = null;
  streamingText = '';
  pendingAsk = null;
  toolDetails = {};
  selectedToolId = null;
  turnStartMs = 0;
  turnFirstTokenMs = 0;
  turnInTokens = 0;
  turnOutTokens = 0;
  setTurnRunning(false);
  closeToolDetail();
}

// Details column state: structured inspection of the selected tool row.
let toolDetails = {};     // id -> {name, input, output, elapsed, error}
let selectedToolId = null;
let detailTab = 'summary';

function openToolDetail(id) {
  if (selectedToolId === id) { closeToolDetail(); return; }
  selectedToolId = id;
  detailTab = 'summary';
  document.querySelector('.app').classList.remove('details-closed');
  document.querySelectorAll('.tool-event.selected').forEach(c => c.classList.remove('selected'));
  const card = document.querySelector('.tool-event[data-id="' + escAttr(id) + '"]');
  if (card) card.classList.add('selected');
  document.getElementById('detailsPlaceholder').style.display = 'none';
  document.getElementById('detailsTabs').style.display = 'flex';
  renderDetailPanel();
}

function closeToolDetail() {
  selectedToolId = null;
  document.querySelectorAll('.tool-event.selected').forEach(c => c.classList.remove('selected'));
  document.getElementById('detailsPlaceholder').style.display = '';
  document.getElementById('detailsTabs').style.display = 'none';
  document.getElementById('detailsBody').innerHTML = '';
  document.querySelector('.app').classList.add('details-closed');
}

function switchDetailTab(tab, btn) {
  detailTab = tab;
  document.querySelectorAll('.detail-tab').forEach(b => b.classList.remove('active'));
  if (btn) btn.classList.add('active');
  renderDetailPanel();
}

function renderDetailPanel() {
  const body = document.getElementById('detailsBody');
  const d = toolDetails[selectedToolId];
  if (!d) return;
  if (detailTab === 'summary') {
    const lines = [
      ['Tool', d.name],
      ['Status', d.error ? 'Failed' : 'Done'],
      ['Duration', d.elapsed ? fmtMs(d.elapsed) : '-'],
      ['Input', d.input ? '(see Input tab)' : '(none)'],
      ['Output', d.output ? '(see Output tab)' : '(none)'],
    ];
    body.innerHTML = '<div class="insp-kv">' + lines.map(([k, v]) =>
      '<span class="k">' + escHtml(k) + '</span><span class="v">' + escHtml(v) + '</span>').join('') + '</div>';
    document.querySelectorAll('.detail-tab').forEach(b => b.classList.remove('active'));
    const t = document.querySelectorAll('.detail-tab')[0];
    if (t) t.classList.add('active');
  } else if (detailTab === 'input') {
    body.innerHTML = '<div class="details-pre">' + escHtml(d.input || '(no input)') + '</div>';
  } else {
    body.innerHTML = '<div class="details-pre">' + escHtml(d.output || '(no output)') + '</div>';
  }
}

// DSH ToolRow parity: per-tool variant title/icon and the summary
// derivation (first preferred string arg, first line only).
const TOOL_VARIANTS = {
  bash:      { title: 'Bash',   keys: ['description', 'command'] },
  pwsh:      { title: 'Pwsh',   keys: ['description', 'command'] },
  read:      { title: 'Read',   keys: ['path', 'file_path', 'url'] },
  web_fetch: { title: 'Read',   keys: ['path', 'file_path', 'url'] },
  web_search:{ title: 'Search', keys: ['query', 'pattern', 'url'] },
  grep:      { title: 'Grep',   keys: ['query', 'pattern', 'url'] },
  glob:      { title: 'Glob',   keys: ['pattern', 'path'] },
  write:     { title: 'Write',  keys: ['path', 'file_path'] },
  edit:      { title: 'Edit',   keys: ['path', 'file_path'] },
  run_code:  { title: 'Code',   keys: ['description'] },
  todo_write:{ title: '\u66F4\u65B0\u4EFB\u52A1\u6E05\u5355', keys: [] },
  todowrite: { title: '\u66F4\u65B0\u4EFB\u52A1\u6E05\u5355', keys: [] },
};
const TOOL_ICONS = {
  search: '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><circle cx="7" cy="7" r="4.5"/><path d="M10.5 10.5L14 14"/></svg>',
  read:   '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.3"><path d="M2 4.5A1.5 1.5 0 0 1 3.5 3h2.6l1.5 1.8h4.9A1.5 1.5 0 0 1 14 6.3v5.2a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 2 11.5z"/></svg>',
  bash:   '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M6 4.5 2.8 8 6 11.5M10 4.5 13.2 8 10 11.5"/></svg>',
  write:  '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"><path d="M11.7 2.3a1.6 1.6 0 0 1 2.3 2.3L6 12.6 2.5 13.5l.9-3.5z"/></svg>',
  code:   '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M6 4 2.5 8 6 12M10 4l3.5 4L10 12"/></svg>',
  sparkle:'<svg viewBox="0 0 16 16" fill="currentColor"><path d="M8 1.6l1 2.9a2.6 2.6 0 0 0 1.5 1.5l2.9 1-2.9 1a2.6 2.6 0 0 0-1.5 1.5l-1 2.9-1-2.9a2.6 2.6 0 0 0-1.5-1.5l-2.9-1 2.9-1a2.6 2.6 0 0 0 1.5-1.5z"/></svg>',
};
function iconKeyOf(name) {
  if (name === 'bash' || name === 'pwsh') return 'bash';
  if (name === 'read' || name === 'web_fetch') return 'read';
  if (name === 'web_search' || name === 'grep' || name === 'glob') return 'search';
  if (name === 'write' || name === 'edit') return 'write';
  if (name === 'run_code') return 'code';
  return 'sparkle';
}
function toolVariantOf(name) {
  const v = TOOL_VARIANTS[name];
  return { title: v ? v.title : 'Tool call', keys: v ? v.keys : [], icon: TOOL_ICONS[iconKeyOf(name)] };
}
function firstLine(s) {
  const t = String(s || '');
  const i = t.indexOf('\n');
  return (i === -1 ? t : t.slice(0, i)).trim();
}
function toolSummaryText(name, input) {
  if (isTodoWriteTool(name)) {
    const todos = normalizeTodoItems(input);
    if (todos) {
      const metrics = todoPlanMetrics(todos);
      const active = todos.filter(item => item.status === 'in_progress').map(item => item.content);
      const suffix = active.length ? ` · ${active[0]}${active.length > 1 ? ` +${active.length - 1}` : ''}` : '';
      return `完成 ${metrics.completed}/${metrics.total}${suffix}`;
    }
  }
  let args;
  try { args = JSON.parse(input); } catch (e) { args = undefined; }
  const v = TOOL_VARIANTS[name];
  if (args === undefined) return firstLine(input);
  if (typeof args !== 'object' || args === null || Array.isArray(args)) return firstLine(input);
  let val = '';
  for (const k of (v ? v.keys : [])) {
    if (typeof args[k] === 'string' && args[k].trim()) { val = args[k]; break; }
  }
  if (!val) {
    for (const k of Object.keys(args)) {
      if (typeof args[k] === 'string' && args[k].trim()) { val = args[k]; break; }
    }
  }
  const base = firstLine(val || input);
  if (v === undefined && name && name !== 'tool') return name + ' \u00B7 ' + base;
  return base;
}

const FILE_DIFF_MAX_LINES = 500;
const FILE_DIFF_MAX_SOURCE_LINES = 1000;
const FILE_DIFF_MAX_SOURCE_CHARS = 256 * 1024;
const FILE_DIFF_MAX_CELLS = 80000;

function fileDiffLines(value) {
  if (value === '') return { lines: [], truncated: false };
  let text = String(value == null ? '' : value).replace(/\r\n/g, '\n');
  let truncated = text.length > FILE_DIFF_MAX_SOURCE_CHARS;
  if (truncated) text = text.slice(0, FILE_DIFF_MAX_SOURCE_CHARS);
  let lines = text.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  if (lines.length > FILE_DIFF_MAX_SOURCE_LINES) {
    lines = lines.slice(0, FILE_DIFF_MAX_SOURCE_LINES);
    truncated = true;
  }
  return { lines: lines, truncated: truncated };
}

function parseFileEditInput(name, input) {
  const kind = String(name || '').toLowerCase();
  if (kind !== 'edit' && kind !== 'write') return null;
  let args = input;
  if (typeof args === 'string') {
    try { args = JSON.parse(args); } catch (e) { return null; }
  }
  if (!args || typeof args !== 'object' || Array.isArray(args)) return null;
  const path = typeof args.path === 'string' ? args.path : (typeof args.file_path === 'string' ? args.file_path : 'file');
  if (kind === 'edit') {
    const before = typeof args.old === 'string' ? args.old : args.old_string;
    const after = typeof args.new === 'string' ? args.new : args.new_string;
    if (typeof before !== 'string' || typeof after !== 'string') return null;
    return { kind: kind, path: path, before: before, after: after };
  }
  if (typeof args.content !== 'string') return null;
  return { kind: kind, path: path, before: '', after: args.content };
}

function buildFileLineDiff(before, after) {
  const leftSource = fileDiffLines(before);
  const rightSource = fileDiffLines(after);
  const left = leftSource.lines;
  const right = rightSource.lines;
  const rows = [];
  const cells = left.length * right.length;
  let i = 0;
  let j = 0;

  if (cells <= FILE_DIFF_MAX_CELLS) {
    const dp = Array.from({ length: left.length + 1 }, () => new Uint16Array(right.length + 1));
    for (i = left.length - 1; i >= 0; i--) {
      for (j = right.length - 1; j >= 0; j--) {
        dp[i][j] = left[i] === right[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
    i = 0;
    j = 0;
    while (i < left.length || j < right.length) {
      if (i < left.length && j < right.length && left[i] === right[j]) {
        rows.push({ kind: 'context', oldLine: i + 1, newLine: j + 1, text: left[i] });
        i++;
        j++;
      } else if (j < right.length && (i === left.length || dp[i][j + 1] >= dp[i + 1][j])) {
        rows.push({ kind: 'add', oldLine: '', newLine: j + 1, text: right[j] });
        j++;
      } else {
        rows.push({ kind: 'delete', oldLine: i + 1, newLine: '', text: left[i] });
        i++;
      }
    }
  } else {
    left.forEach((text, index) => rows.push({ kind: 'delete', oldLine: index + 1, newLine: '', text: text }));
    right.forEach((text, index) => rows.push({ kind: 'add', oldLine: '', newLine: index + 1, text: text }));
  }

  return {
    rows: rows,
    added: rows.filter(row => row.kind === 'add').length,
    removed: rows.filter(row => row.kind === 'delete').length,
    truncated: leftSource.truncated || rightSource.truncated || rows.length > FILE_DIFF_MAX_LINES,
  };
}

function renderFileEditCard(chip, name, input) {
  const edit = parseFileEditInput(name, input);
  if (!edit) return false;
  const diff = buildFileLineDiff(edit.before, edit.after);
  const shown = diff.rows.slice(0, FILE_DIFF_MAX_LINES);
  const leaf = edit.path.split(/[\\/]/).pop() || edit.path;
  const summary = chip.querySelector('.tc-summary');
  if (summary) summary.textContent = leaf + ' \u00B7 +' + diff.added + ' -' + diff.removed;
  const body = chip.querySelector('.tc-body');
  if (!body) return false;
  const rows = shown.map(row => {
    const mark = row.kind === 'add' ? '+' : (row.kind === 'delete' ? '\u2212' : ' ');
    return '<div class="tc-diff-line" data-kind="' + row.kind + '">' +
      '<span class="tc-diff-no">' + row.oldLine + '</span>' +
      '<span class="tc-diff-no">' + row.newLine + '</span>' +
      '<span class="tc-diff-mark">' + mark + '</span>' +
      '<code class="tc-diff-code">' + escHtml(row.text || ' ') + '</code></div>';
  }).join('');
  const writeNote = edit.kind === 'write'
    ? '<span class="tc-diff-note">Written content; the previous full body is not stored in this tool event.</span>'
    : '<span class="tc-diff-note">Before / after</span>';
  const omitted = diff.truncated
    ? '<div class="tc-diff-omitted">Diff truncated after ' + FILE_DIFF_MAX_LINES + ' rows. Inspect the tool input for the complete change.</div>'
    : '';
  body.innerHTML = '<div class="tc-file-diff">' +
    '<div class="tc-diff-head"><span class="tc-diff-path" title="' + escAttr(edit.path) + '">' + escHtml(leaf) + '</span>' +
    writeNote + '<span class="tc-diff-count add">+' + diff.added + '</span><span class="tc-diff-count delete">-' + diff.removed + '</span></div>' +
    '<div class="tc-diff-full-path">' + escHtml(edit.path) + '</div>' +
    '<div class="tc-diff-lines">' + rows + '</div>' + omitted + '</div>';
  chip.classList.add('file-edit-row');
  return true;
}

// Provider-streamed partial tool args (tool_input_delta): route each
// chunk to the in-flight row so the args JSON builds up live.
function handleToolArgsDelta(d) {
  const id = d.id || '';
  let card = id ? document.querySelector('.call-row[data-id="' + escAttr(id) + '"]') : null;
  if (!card && d.tool) {
    // Args deltas stream BEFORE the provider's final tool_use_stop, so the
    // in-flight row does not exist yet: create it and let the args build up.
    handleToolStart({ tool: d.tool, id: id, input: '' });
    card = id ? document.querySelector('.call-row[data-id="' + escAttr(id) + '"]') : null;
  }
  if (!card || card.getAttribute('data-state') !== 'running') return;
  const next = (card.getAttribute('data-args') || '') + (d.delta || '');
  card.setAttribute('data-args', next);
  if (toolDetails[id]) toolDetails[id].input = next;
  const name = card.getAttribute('data-tool') || 'tool';
  const summary = card.querySelector('.tc-summary');
  const text = toolSummaryText(name, next);
  if (summary && text) summary.textContent = text;
  const box = card.querySelector('.tc-io-text[data-in]');
  if (box) {
    try { box.textContent = JSON.stringify(JSON.parse(next), null, 2); }
    catch (e) { box.textContent = next; }
  }
}

function handleToolStart(d) {
  const area = document.getElementById('chatArea');
  const name = d.tool || 'tool';
  const id = d.id || '';
  const input = d.input || '';
  const existing = id ? document.querySelector('.call-row[data-id="' + escAttr(id) + '"]') : null;
  if (existing) {
    // The row was created early by tool_args_delta; adopt the provider's
    // authoritative full args instead of appending a duplicate row.
    const full = input || existing.getAttribute('data-args') || '';
    existing.setAttribute('data-args', full);
    if (!toolDetails[id]) toolDetails[id] = { name: name, input: full, output: '', elapsed: 0, error: false };
    toolDetails[id].input = full;
    const summary = existing.querySelector('.tc-summary');
    const text = toolSummaryText(name, full);
    if (summary && text) summary.textContent = text;
    const box = existing.querySelector('.tc-io-text[data-in]');
    if (box) {
      try { box.textContent = JSON.stringify(JSON.parse(full), null, 2); }
      catch (e) { box.textContent = full; }
    }
    return;
  }
  // A new tool call is a provider event boundary: settle reasoning before
  // inserting the tool so the visible timeline keeps source order.
  finishThinking();
  toolDetails[id] = { name: name, input: input, output: '', elapsed: 0, error: false };
  const v = toolVariantOf(name);
  const summary = toolSummaryText(name, input);
  const pretty = (() => {
    try { return JSON.stringify(JSON.parse(input), null, 2); }
    catch (e) { return input; }
  })();
  area.insertAdjacentHTML('beforeend', `
    <div class="call-row" data-tool="${escAttr(name)}" data-id="${escAttr(id)}" data-variant="${escAttr(iconKeyOf(name))}" data-state="running" data-args="${escAttr(input)}">
      <div class="tc-row" role="button" tabindex="0" onclick="toggleToolInline('${escOnclick(id)}')">
        <span class="tc-leading">
          <span class="tc-icon-idle">${v.icon}</span>
          <svg class="tc-chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M5 6.5 8 9.5l3-3"/></svg>
        </span>
        <span class="tc-title">${escHtml(v.title)}</span>
        <span class="tc-sep" aria-hidden="true"></span>
        <span class="tc-summary">${escHtml(summary) || '\u2014'}</span>
        <span class="tc-time"></span>
        <button class="tc-inspect" onclick="event.stopPropagation();openToolDetail('${escOnclick(id)}')">Inspect</button>
      </div>
      <div class="tc-body">
        <div class="tc-io">
          <div class="tc-io-sec"><span class="tc-io-label">IN</span><span class="tc-io-text" data-in>${escHtml(pretty || '\u2014')}</span></div>
          <div class="tc-io-div"></div>
          <div class="tc-io-sec"><span class="tc-io-label">OUT</span><span class="tc-io-text" data-out>\u2026</span></div>
        </div>
      </div>
    </div>`);
  autoScroll();
}

function toggleToolInline(id) {
  const card = document.querySelector('.call-row[data-id="' + escAttr(id) + '"]');
  if (card) card.classList.toggle('open');
}

function handleToolResult(d) {
  const name = d.tool || '';
  const id = d.id || '';
  // Prefer the exact tool_use_id; the old name+last-of-type selector
  // failed whenever another element was appended between start and
  // result, leaving the chip stuck on "Running".
  let chip = null;
  if (id) chip = document.querySelector('.call-row[data-id="' + escAttr(id) + '"]');
  if (!chip && name) {
    const chips = document.querySelectorAll('.call-row[data-tool="' + escAttr(name) + '"]:not([data-state="ok"]):not([data-state="error"])');
    chip = chips.length ? chips[chips.length - 1] : null;
  }
  if (chip) {
    const ok = !d.isError;
    chip.setAttribute('data-state', ok ? 'ok' : 'error');
    if (ok && isTodoWriteTool(name)) {
      applyTodoSnapshot(name, chip.getAttribute('data-args') || '');
    }
    const leading = chip.querySelector('.tc-leading');
    if (!ok && leading) {
      leading.innerHTML = '<span class="tc-state-dot" data-state="error" aria-hidden="true"></span>';
    }
    const t = chip.querySelector('.tc-time');
    if (t) t.textContent = d.elapsedMs ? fmtMs(d.elapsedMs) : '';
    const summary = chip.querySelector('.tc-summary');
    if (!ok && summary && d.output) {
      summary.classList.add('err');
      summary.textContent = firstLine(d.output) || summary.textContent;
    }
    const out = chip.querySelector('.tc-io-text[data-out]');
    if (out) {
      out.textContent = d.output || '(no output)';
      if (!ok) out.setAttribute('data-error', 'true');
    }
    // DSH SearchBlock parity: a successful Grep renders as a structured
    // search card (summary banner + per-file groups + line rows) instead
    // of the raw text dump. Details panel keeps the raw output.
    if (ok && name === 'Grep' && d.output) {
      renderSearchCard(chip, d.output, id);
    }
    if (ok && (String(name).toLowerCase() === 'edit' || String(name).toLowerCase() === 'write')) {
      renderFileEditCard(chip, name, chip.getAttribute('data-args') || '');
    }
    // Rich results use structured presentation metadata persisted beside the
    // tool result. Never infer an Artifact from model-authored output text.
    if (ok && d.presentation && typeof renderArtifactPresentation === 'function') {
      renderArtifactPresentation(chip, d.presentation);
    }
    if (toolDetails[id]) {
      toolDetails[id].output = d.output || '';
      toolDetails[id].elapsed = d.elapsedMs || 0;
      toolDetails[id].error = !!d.isError;
    }
    if (selectedToolId === id) renderDetailPanel();
  }
}

// ============================================================
// Search card (DSH SearchBlock parity): parses grep's
// `path:lineno:line` rows into per-file groups behind a summary
// banner. Geometry mirrors CodeBlock: monospace rows, no soft wrap
// (long lines scroll horizontally), 4 visible rows per file with an
// expand toggle (matches DSH's "… 其余 N 行" affordance).
// ============================================================
function parseGrepOutput(text) {
  const groups = [];           // [{path, matches:[{lineNumber, line}]}]
  const byPath = new Map();
  const footers = [];
  let matched = 0;
  for (const raw of String(text).split('\n')) {
    const m = raw.match(/^(.+?):(\d+):(.*)$/);
    if (!m) {
      const t = raw.trim();
      // grep.go appends bracketed pagination/walk footers; "(no matches)"
      // style lines mean the card should not render at all.
      if (t) footers.push(t);
      continue;
    }
    let g = byPath.get(m[1]);
    if (!g) { g = { path: m[1], matches: [] }; byPath.set(m[1], g); groups.push(g); }
    g.matches.push({ lineNumber: parseInt(m[2], 10), line: m[3] });
    matched++;
  }
  if (!groups.length) return null;
  // Truncation footer reveals the pre-cap total: "[truncated at 200
  // matches; …]" — fold it into the banner ("显示 X / 共 N").
  let total = matched;
  const trunc = footers.find(f => /^\[truncated at (\d+) matches/.test(f));
  if (trunc) {
    const n = trunc.match(/\d+/);
    if (n) total = Math.max(matched, parseInt(n[0], 10));
  }
  return { groups, matched, total, truncated: total > matched, footers };
}

function searchCardLineHtml(m) {
  return '<div class="sc-line"><span class="sc-lineno">' + m.lineNumber + '</span><span class="sc-linetext">' + escHtml(m.line) + '</span></div>';
}

function renderSearchCard(chip, output, id) {
  const parsed = parseGrepOutput(output);
  if (!parsed) return; // "(no matches)" / unparsable → keep the raw view
  const sec = chip.querySelector('.tc-io-sec:last-child');
  if (!sec) return;

  const fileRows = parsed.groups.map(g => {
    const hidden = g.matches.length - 4;
    const rows = g.matches.slice(0, 4).map(searchCardLineHtml).join('');
    const more = hidden > 0
      ? '<button class="sc-more" onclick="toggleSearchGroup(this)">\u2026 \u5176\u4F59 ' + hidden + ' \u884C</button>' : '';
    const rest = hidden > 0
      ? '<div class="sc-lines sc-hidden">' + g.matches.slice(4).map(searchCardLineHtml).join('') + '</div>' : '';
    return '<div class="sc-group">' +
      '<button class="sc-file" onclick="toggleSearchFiles(this)">' +
      '<svg class="sc-chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 6.5 8 9.5l3-3"/></svg>' +
      '<span class="sc-path">' + escHtml(g.path) + '</span>' +
      '<span class="sc-count">' + g.matches.length + '</span></button>' +
      '<div class="sc-lines">' + rows + '</div>' + rest + more + '</div>';
  }).join('');

  const banner = parsed.truncated
    ? '\u663E\u793A ' + parsed.matched + ' / \u5171 ' + parsed.total + ' \u5904\u5339\u914D \u00B7 ' + parsed.groups.length + ' \u4E2A\u6587\u4EF6'
    : parsed.matched + ' \u5904\u5339\u914D \u00B7 ' + parsed.groups.length + ' \u4E2A\u6587\u4EF6';

  sec.innerHTML =
    '<div class="sc-card" data-id="' + escAttr(id) + '">' +
    '<div class="sc-banner"><span class="sc-summary">' + escHtml(banner) + '</span>' +
    '<button class="sc-copy" onclick="copySearchCard(this)" title="Copy raw result">&#128203;</button></div>' +
    '<div class="sc-body">' + fileRows + '</div>' +
    (parsed.footers.length ? '<div class="sc-recovery">' + escHtml(parsed.footers.join(' \u00B7 ')) + '</div>' : '') +
    '</div>';
}

function toggleSearchFiles(btn) {
  const group = btn.closest('.sc-group');
  if (group) group.classList.toggle('collapsed');
}

function toggleSearchGroup(btn) {
  const wrap = btn.previousElementSibling;
  if (wrap) wrap.classList.toggle('sc-hidden');
  btn.remove();
}

function copySearchCard(btn) {
  const card = btn.closest('.sc-card');
  if (!card) return;
  const id = card.getAttribute('data-id');
  const raw = (toolDetails[id] || {}).output || '';
  navigator.clipboard.writeText(raw).then(() => {
    btn.textContent = '\u2713';
    setTimeout(() => { btn.textContent = '\uD83D\uDCCB'; }, 1200);
  }).catch(() => {});
}

// Permission approval card (DSH human-in-the-loop parity).
function handlePermissionRequest(d) {
  const area = document.getElementById('chatArea');
  const id = d.permId || '';
  if (!id) return;
  area.insertAdjacentHTML('beforeend', `
    <div class="perm-card" data-perm="${escAttr(id)}" data-tool="${escAttr(d.tool || 'tool')}">
      <span class="tool-icon running">&#128274;</span>
      <div class="tool-body">
        <div class="tool-name">${escHtml(d.tool || 'tool')} needs approval</div>
        <div class="tool-status">${escHtml(d.reason || '')}</div>
      </div>
      <div class="perm-actions">
        <button class="perm-btn allow" onclick="resolvePermission('${escOnclick(id)}', true, this)">&#20801;&#35768;</button>
        <button class="perm-btn deny" onclick="resolvePermission('${escOnclick(id)}', false, this)">&#25298;&#32477;</button>
      </div>
    </div>`);
  renderSessions();
  autoScroll();
}

async function resolvePermission(id, approve, btn) {
  const card = btn.closest('.perm-card');
  try {
    const res = await fetch('/api/permission', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, approve: approve })
    });
    if (!res.ok) throw new Error('permission: ' + res.status);
    if (card) {
      card.classList.add(approve ? 'approved' : 'denied');
      const tool = card.getAttribute('data-tool') || 'tool';
      const name = card.querySelector('.tool-name');
      if (name) name.textContent = tool + (approve ? ' allowed' : ' denied');
      const status = card.querySelector('.tool-status');
      if (status) status.textContent = approve ? 'Allowed' : 'Denied';
      const actions = card.querySelector('.perm-actions');
      if (actions) actions.remove();
      const icon = card.querySelector('.tool-icon');
      icon.className = 'tool-icon ' + (approve ? 'done' : 'failed');
      icon.innerHTML = approve ? '&#10003;' : '&#10007;';
    }
    renderSessions();
  } catch (e) {
    showToast('Failed to resolve permission: ' + e.message);
  }
}

function isChatNearBottom(area) {
  if (!area) return true;
  return area.scrollHeight - area.scrollTop - area.clientHeight <= CHAT_BOTTOM_THRESHOLD;
}

function updateScrollFollowUI() {
  const button = document.getElementById('jumpLatestBtn');
  const area = document.getElementById('chatArea');
  if (button) button.hidden = followOutput || isChatNearBottom(area);
}

function onChatScroll() {
  if (programmaticChatScroll) return;
  const area = document.getElementById('chatArea');
  followOutput = isChatNearBottom(area);
  updateScrollFollowUI();
}

function pauseAutoScrollOnWheel(event) {
  if (event.deltaY >= 0) return;
  programmaticChatScroll = false;
  followOutput = false;
  updateScrollFollowUI();
}

function initChatScroll() {
  const area = document.getElementById('chatArea');
  if (!area || area.dataset.scrollFollowReady === 'true') return;
  area.dataset.scrollFollowReady = 'true';
  area.addEventListener('scroll', onChatScroll, { passive: true });
  // A trackpad/wheel-up gesture must win even if it lands in the same frame as
  // a live delta that just moved the transcript programmatically.
  area.addEventListener('wheel', pauseAutoScrollOnWheel, { passive: true });
  updateScrollFollowUI();
}

function autoScroll(force = false) {
  const area = document.getElementById('chatArea');
  if (!area) return;
  // Harness renders TurnStatus after every conversation node. METIS appends
  // live nodes imperatively, so move the stable status element back to the
  // tail whenever a thinking/tool/message row arrives.
  if (turnStatusEl && turnStatusEl.parentElement === area && area.lastElementChild !== turnStatusEl) {
    area.appendChild(turnStatusEl);
  }
  if (force) followOutput = true;
  if (!followOutput) {
    updateScrollFollowUI();
    return;
  }
  programmaticChatScroll = true;
  area.scrollTop = area.scrollHeight;
  if (chatScrollFrame) cancelAnimationFrame(chatScrollFrame);
  chatScrollFrame = requestAnimationFrame(() => {
    chatScrollFrame = 0;
    programmaticChatScroll = false;
    followOutput = isChatNearBottom(area);
    updateScrollFollowUI();
  });
}

function resumeAutoScroll() {
  autoScroll(true);
}


// --- Chat ---
function newChat() {
  if (turnRunning) {
    showToast('Stop the current turn before starting a new session');
    return;
  }
  if (typeof invalidateSessionAsyncLoads === 'function') invalidateSessionAsyncLoads();
  currentSessionId = null;
  if (typeof resetArtifactsForSession === 'function') resetArtifactsForSession();
  queuedTurns = [];
  renderQueuedTurns();
  resetTurnState();
  messages = [];
  followOutput = true;
  updateScrollFollowUI();
  document.getElementById('chatArea').innerHTML = `
    <div class="welcome" id="welcomeScreen">
      <div class="welcome-line">
        <span class="welcome-text" data-i18n="welcome">\u4ECE\u60F3\u6CD5\uFF0C\u5230\u5B8C\u6210</span>
      </div>
    </div>`;
	applyLanguage(desktopPreferences.language);
  const bar = document.getElementById('sessionStatsbar');
  if (bar) { bar.style.display = 'none'; bar.textContent = ''; }
  renderSessions();
  updateEmptyLayout();
}

// The POST resolves when the turn completes; while it is in flight the
// assistant response is rendered live via the /api/events SSE stream.
// If nothing was streamed (error turn, empty reply), fall back to the
// text returned by the POST so the chat is never left blank.
// --- Image attachments (DSH attachment parity) ---
// Pasted images ride the same ContentBlock{type:"image"} channel the TUI
// uses; the server forwards them into AppendUserBlocks.
let attachments = [];

function attachImage(dataUrl, mediaType) {
  if (attachments.length >= 6) { showToast('At most 6 images per message'); return; }
  const base64 = dataUrl.split(',')[1] || '';
  if (!base64) return;
  if (base64.length > 11 * 1024 * 1024) { showToast('Image too large (max ~8MB)'); return; }
  attachments.push({ mediaType, data: base64 });
  renderAttachments();
}

function removeAttachment(i) {
  attachments.splice(i, 1);
  renderAttachments();
}

function renderAttachments() {
  let strip = document.getElementById('attachStrip');
  if (!strip) {
    const box = document.querySelector('.input-box');
    if (!box) return;
    strip = document.createElement('div');
    strip.id = 'attachStrip';
    strip.className = 'attach-strip';
    box.parentElement.insertBefore(strip, box);
  }
  strip.innerHTML = attachments.map((a, i) =>
    '<span class="attach-chip">' +
    '<img class="attach-thumb" alt="Attached image preview" src="data:' + a.mediaType + ';base64,' + a.data + '">' +
    '<span class="attach-label">' + (a.mediaType === 'image/png' ? 'PNG' : 'IMG') + '</span>' +
    '<button class="attach-remove" title="Remove attachment" aria-label="Remove attachment" onclick="removeAttachment(' + i + ')">\u00D7</button>' +
    '</span>').join('');
  updateEmptyLayout();
  updateSendBtn();
}

function openAttachmentPicker() {
  const input = document.getElementById('attachmentInput');
  if (input) input.click();
}

function attachFile(file) {
  if (!file || !String(file.type || '').startsWith('image/')) {
    showToast('Only image attachments are supported');
    return;
  }
  if (file.size > 8 * 1024 * 1024) {
    showToast('Image too large (max 8MB)');
    return;
  }
  const reader = new FileReader();
  reader.onload = () => attachImage(reader.result, file.type);
  reader.onerror = () => showToast('Unable to read image');
  reader.readAsDataURL(file);
}

function onAttachmentFiles(files) {
  for (const file of Array.from(files || [])) attachFile(file);
}

function initAttachmentDrop() {
  const container = document.querySelector('.input-container');
  const overlay = document.getElementById('dropOverlay');
  if (!container || !overlay) return;
  let depth = 0;
  container.addEventListener('dragenter', e => {
    if (!e.dataTransfer || !Array.from(e.dataTransfer.types || []).includes('Files')) return;
    e.preventDefault();
    depth++;
    overlay.classList.add('visible');
  });
  container.addEventListener('dragover', e => {
    if (!e.dataTransfer) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  container.addEventListener('dragleave', e => {
    e.preventDefault();
    depth = Math.max(0, depth - 1);
    if (depth === 0) overlay.classList.remove('visible');
  });
  container.addEventListener('drop', e => {
    e.preventDefault();
    depth = 0;
    overlay.classList.remove('visible');
    onAttachmentFiles(e.dataTransfer && e.dataTransfer.files);
  });
}
document.addEventListener('DOMContentLoaded', initAttachmentDrop);

function insertComposerText(input, text) {
  if (!input || !text) return;
  const start = Number.isInteger(input.selectionStart) ? input.selectionStart : input.value.length;
  const end = Number.isInteger(input.selectionEnd) ? input.selectionEnd : start;
  const before = input.value.slice(0, start);
  const after = input.value.slice(end);
  const prefix = before && !/[\s\n]$/.test(before) ? '\n' : '';
  const suffix = after && !/^[\s\n]/.test(after) ? '\n' : '';
  input.setRangeText(prefix + text + suffix, start, end, 'end');
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.focus();
}

async function pasteClipboardFilePaths(files, input) {
  const names = Array.from(files || []).map(file => file.name).filter(Boolean);
  if (!names.length) return;
  try {
    const res = await fetch('/api/clipboard/files', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ names })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'Unable to resolve clipboard file paths');
    const resolved = Array.isArray(data.files) ? data.files : [];
    if (!resolved.length) throw new Error('Clipboard does not contain readable local paths');
    const references = resolved.map(file =>
      uiText(file.isDir ? 'Folder' : 'File', file.isDir ? '\u6587\u4ef6\u5939' : '\u6587\u4ef6') + ': ' + JSON.stringify(String(file.path || ''))
    ).join('\n');
    insertComposerText(input, references);
    showToast(uiText(
      resolved.length === 1 ? 'Local path added' : resolved.length + ' local paths added',
      resolved.length === 1 ? '\u5df2\u6dfb\u52a0\u672c\u5730\u8def\u5f84' : '\u5df2\u6dfb\u52a0 ' + resolved.length + ' \u4e2a\u672c\u5730\u8def\u5f84'
    ));
  } catch (err) {
    showToast(uiText(
      'Unable to read the Finder path. Copy the item again, or use Finder\u2019s Copy as Pathname.',
      '\u65e0\u6cd5\u8bfb\u53d6 Finder \u8def\u5f84\uff0c\u8bf7\u91cd\u65b0\u590d\u5236\uff0c\u6216\u5728 Finder \u4e2d\u4f7f\u7528\u201c\u62f7\u8d1d\u4e3a\u8def\u5f84\u540d\u201d\u3002'
    ));
  }
}

async function pasteAllClipboardFilePaths() {
  const input = document.getElementById('inputField');
  try {
    const res = await fetch('/api/clipboard/files', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ all: true })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'Unable to read Finder clipboard paths');
    const resolved = Array.isArray(data.files) ? data.files : [];
    if (!resolved.length) throw new Error(uiText('Copy files or folders in Finder first.', '\u8bf7\u5148\u5728 Finder \u4e2d\u590d\u5236\u6587\u4ef6\u6216\u6587\u4ef6\u5939\u3002'));
    const references = resolved.map(file =>
      uiText(file.isDir ? 'Folder' : 'File', file.isDir ? '\u6587\u4ef6\u5939' : '\u6587\u4ef6') + ': ' + JSON.stringify(String(file.path || ''))
    ).join('\n');
    insertComposerText(input, references);
    showToast(uiText(
      resolved.length === 1 ? 'Local path added' : resolved.length + ' local paths added',
      resolved.length === 1 ? '\u5df2\u6dfb\u52a0\u672c\u5730\u8def\u5f84' : '\u5df2\u6dfb\u52a0 ' + resolved.length + ' \u4e2a\u672c\u5730\u8def\u5f84'
    ));
  } catch (err) {
    showToast(err.message || uiText('Unable to read Finder clipboard.', '\u65e0\u6cd5\u8bfb\u53d6 Finder \u526a\u8d34\u677f\u3002'));
  }
}

// Finder folders arrive in WKWebView as zero-byte File objects containing
// only the basename. Images remain data attachments; every other File item is
// resolved through the native clipboard endpoint and inserted as an absolute
// path so the agent can actually read it.
document.addEventListener('paste', (e) => {
  const files = Array.from((e.clipboardData && e.clipboardData.files) || []);
  if (!files.length) return;
  const images = files.filter(file => String(file.type || '').startsWith('image/'));
  const pathFiles = files.filter(file => !String(file.type || '').startsWith('image/'));
  const input = document.getElementById('inputField');
  const composerFocused = input && (e.target === input || document.activeElement === input);
  if (!images.length && (!pathFiles.length || !composerFocused)) return;
  e.preventDefault();
  for (const file of images) attachFile(file);
  if (pathFiles.length && composerFocused) pasteClipboardFilePaths(pathFiles, input);
});

async function sendMessage(busyBehavior) {
  const input = document.getElementById('inputField');
  const text = input.value.trim();
  if (!text && !attachments.length) return;
  if (pendingAsk) {
    input.value = '';
    autoResize(input);
    await submitAskAnswer(text);
    return;
  }

  if (turnRunning && runningSessionId && currentSessionId !== runningSessionId) {
    showToast('Another session is still running. Return to it or stop it before sending.');
    return;
  }

  if (!turnRunning && await executeComposerCommand(text)) {
    input.value = '';
    autoResize(input);
    closeCommandMenu();
    return;
  }

  const item = { text: text, images: attachments.slice() };
  input.value = '';
  autoResize(input);
  attachments = [];
  renderAttachments();

  if (turnRunning) {
    await submitBusyInput(item, busyBehavior || desktopPreferences.busyEnter || 'queue');
    return;
  }
  await runTurnItem(item);
}

async function submitBusyInput(item, behavior) {
  if (behavior === 'send' && item.images.length === 0) {
    try {
      const res = await fetch('/api/steer', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sessionId: currentSessionId, input: item.text })
      });
      const data = await res.json();
      if (res.ok) {
        currentSessionId = data.sessionId || currentSessionId;
        addMessage('user', item.text);
        showToast('Sent to the current turn');
        await loadSessions();
        return;
      }
      if (res.status !== 409) throw new Error(data.error || 'steer: ' + res.status);
      showToast('Current turn closed; queued for the next turn');
    } catch (e) {
      showToast('Unable to steer; queued instead (' + e.message + ')');
    }
  } else if (behavior === 'send' && item.images.length) {
    showToast('Image follow-ups are queued for the next turn');
  }
  if (!queuedSessionId) queuedSessionId = runningSessionId || currentSessionId;
  queuedTurns.push(item);
  renderQueuedTurns();
}

function renderQueuedTurns() {
  const wrap = document.getElementById('queuedTurns');
  if (!wrap) return;
  if (!queuedTurns.length) {
    wrap.style.display = 'none';
    wrap.innerHTML = '';
    return;
  }
  wrap.style.display = 'flex';
  wrap.innerHTML = '<span class="queued-count">Queued ' + queuedTurns.length + '</span>' + queuedTurns.map((item, i) => {
    const preview = item.text || (item.images.length + ' image' + (item.images.length === 1 ? '' : 's'));
    return '<span class="queued-item"><span>' + escHtml(preview) + '</span><button type="button" aria-label="Remove queued message" onclick="removeQueuedTurn(' + i + ')">\u00D7</button></span>';
  }).join('');
}

function removeQueuedTurn(index) {
  queuedTurns.splice(index, 1);
  if (!queuedTurns.length) queuedSessionId = null;
  renderQueuedTurns();
}

async function drainQueuedTurns() {
  if (turnRunning || drainingQueuedTurns || pendingAsk || !queuedTurns.length) return;
  if (queuedSessionId && currentSessionId !== queuedSessionId) return;
  drainingQueuedTurns = true;
  const item = queuedTurns.shift();
  renderQueuedTurns();
  try {
    await runTurnItem(item);
  } finally {
    drainingQueuedTurns = false;
    if (!queuedTurns.length) queuedSessionId = null;
    else if (currentSessionId === queuedSessionId) setTimeout(drainQueuedTurns, 0);
  }
}

async function syncViewedSessionHistory(sessionId) {
  if (!sessionId || currentSessionId !== sessionId) return false;
  try {
    const res = await fetch('/api/sessions/' + encodeURIComponent(sessionId), { method: 'GET' });
    if (!res.ok) return false;
    const data = await res.json();
    if (currentSessionId !== sessionId) return false;
    messages = [];
    streamedTextThisTurn = false;
    renderHistoryMessages(data.messages);
    await restoreCompactionHistory();
    return true;
  } catch (e) {
    return false;
  }
}

async function runTurnItem(item) {
  clearTodoPlan();
  const text = item.text || '';
  const images = item.images || [];
  const turnSessionId = currentSessionId;
  let resolvedTurnSessionId = turnSessionId;

  // Hide welcome
  const welcome = document.getElementById('welcomeScreen');
  if (welcome) welcome.remove();

  // Add user message (image attachments render as placeholder thumbnails)
  resumeAutoScroll();
  addMessage('user', text || '(image attached)');
  beginUserTurn();
  setTurnRunning(true, turnSessionId);
  document.getElementById('sendBtn').disabled = true;

  try {
    const res = await fetch('/api/turns', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({sessionId: turnSessionId, input: text, images: images})
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || `turn: ${res.status}`);
    resolvedTurnSessionId = data.sessionId || turnSessionId;
    if (!runningSessionId && resolvedTurnSessionId) runningSessionId = resolvedTurnSessionId;
    if (!currentSessionId || currentSessionId === turnSessionId) {
      currentSessionId = resolvedTurnSessionId;
    }
    const viewingTurn = currentSessionId === resolvedTurnSessionId;
    if (viewingTurn && typeof loadArtifactsForSession === 'function') {
      await loadArtifactsForSession(resolvedTurnSessionId, { rebuildCards: true, silent: true });
    }
    // If the SSE stream rendered nothing this turn (e.g. a text-less reply),
    // fall back to the returned text only while still viewing this session.
    let historySynced = false;
    if (viewingTurn && runningTurnNeedsHistorySync) {
      historySynced = await syncViewedSessionHistory(resolvedTurnSessionId);
      if (historySynced) runningTurnNeedsHistorySync = false;
    }
    if (viewingTurn && !historySynced && !streamedTextThisTurn && data.text) {
      addMessage('assistant', data.text);
    }
    if (viewingTurn && data.stopped) showToast('Turn stopped');
    await loadSessions();
  } catch (e) {
    const viewingTurn = !resolvedTurnSessionId || currentSessionId === resolvedTurnSessionId || currentSessionId === turnSessionId;
    if (viewingTurn) showError(e.message || 'The request failed.');
    else showToast('Background turn failed: ' + (e.message || 'request failed'));
  } finally {
    // The POST resolving is the definitive end-of-turn signal. Never let a
    // background turn's cleanup mutate the transcript currently being viewed.
    const viewingTurn = !resolvedTurnSessionId || currentSessionId === resolvedTurnSessionId || currentSessionId === turnSessionId;
    if (viewingTurn) finishUserTurn();
    setTurnRunning(false);
    runningTurnNeedsHistorySync = false;
    updateSendBtn();
    loadSessionStatsbar();
    if (queuedTurns.length && !drainingQueuedTurns) {
      if (!queuedSessionId || currentSessionId === queuedSessionId) setTimeout(drainQueuedTurns, 0);
      else showToast('Queued messages are waiting in the completed session');
    }
  }
}

const MESSAGE_ACTION_ICONS = {
  copy: '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.55" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="5" width="11" height="11" rx="2.5"/><path d="M7 5V4.5A2.5 2.5 0 0 1 9.5 2H15a3 3 0 0 1 3 3v5.5a2.5 2.5 0 0 1-2.5 2.5H14"/></svg>',
  branch: '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.55" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="5" cy="4" r="1.6"/><circle cx="15" cy="5" r="1.6"/><circle cx="15" cy="15" r="1.6"/><path d="M5 5.6v3.1A6.3 6.3 0 0 0 11.3 15h2.1M6.6 4h2.7A5.7 5.7 0 0 1 15 9.7v3.7"/></svg>',
  feedback: '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 4.5h14v9H9l-4.5 3v-3H3z"/><path d="M6.5 8h7M6.5 10.5h4.5"/></svg>',
  up: '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7.2 16.5H4.7A1.7 1.7 0 0 1 3 14.8V9.3a1.7 1.7 0 0 1 1.7-1.7h2.5zM7.2 8l3.3-5a1.5 1.5 0 0 1 2.7 1v3.1h2.3A1.8 1.8 0 0 1 17.2 9l-1 5.5a2.4 2.4 0 0 1-2.4 2H7.2z"/></svg>',
  down: '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7.2 3.5H4.7A1.7 1.7 0 0 0 3 5.2v5.5a1.7 1.7 0 0 0 1.7 1.7h2.5zM7.2 12l3.3 5a1.5 1.5 0 0 0 2.7-1v-3.1h2.3A1.8 1.8 0 0 0 17.2 11l-1-5.5a2.4 2.4 0 0 0-2.4-2H7.2z"/></svg>',
  done: '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m4.5 10 3.4 3.4L15.8 5.8"/></svg>',
};

function messageActionButton(icon, label, onclick) {
  return `<button type="button" class="msg-btn" data-icon="${icon}" onclick="${onclick}" title="${escAttr(label)}" aria-label="${escAttr(label)}">${MESSAGE_ACTION_ICONS[icon]}</button>`;
}

const MESSAGE_TIME_ZONE = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'Local';
  } catch (_) {
    return 'Local';
  }
})();

function messageUTCOffset(date) {
  const offsetMinutes = -date.getTimezoneOffset();
  const sign = offsetMinutes >= 0 ? '+' : '-';
  const absolute = Math.abs(offsetMinutes);
  const hours = String(Math.floor(absolute / 60)).padStart(2, '0');
  const minutes = String(absolute % 60).padStart(2, '0');
  return `UTC${sign}${hours}:${minutes}`;
}

function messageActionTime(date = new Date()) {
  if (!(date instanceof Date) || Number.isNaN(date.getTime())) return '';
  const lang = resolvedLanguage(desktopPreferences.language);
  const options = {
    year: 'numeric', month: lang === 'en' ? 'short' : 'long', day: 'numeric',
    hour: '2-digit', minute: '2-digit', hour12: false
  };
  const localDateTime = new Intl.DateTimeFormat(lang === 'en' ? 'en-US' : 'zh-CN', options).format(date);
  return `${localDateTime} · ${messageUTCOffset(date)} · ${MESSAGE_TIME_ZONE}`;
}

function turnMetricsMarkup(metric) {
  const durationMs = Number(metric && metric.durationMs) || 0;
  const ttftMs = Number(metric && metric.ttftMs) || 0;
  const tokPerSec = Math.round(Number(metric && metric.tokPerSec) || 0);
  if (durationMs <= 0 && ttftMs <= 0 && tokPerSec <= 0) return '';
  return `<span class="msg-metrics"><span class="msg-sep">\u00B7</span><span>${uiText('Ran for ', '用时 ')}${fmtRunDur(durationMs)}</span>${ttftMs ? `<span class="msg-sep">\u00B7</span><span>${uiText('First token ', '首 token ')}${fmtMs(ttftMs)}</span>` : ''}${tokPerSec ? `<span class="msg-sep">\u00B7</span><span>${tokPerSec} tok/s</span>` : ''}</span>`;
}

function messageActionsMarkup(role, date = new Date()) {
  const copy = messageActionButton('copy', uiText('Copy', '复制'), 'copyMessage(this)');
  const branch = messageActionButton('branch', uiText('Branch from here', '从此处分支'), 'branchMessage(this)');
  const feedback = messageActionButton('feedback', uiText('Add private feedback', '添加私密反馈'), 'feedbackMessage(this)');
  const ratings = role === 'assistant'
    ? messageActionButton('up', uiText('Good reply', '回答很好'), "rateMessage(this,'up')") + messageActionButton('down', uiText('Bad reply', '回答不好'), "rateMessage(this,'down')")
    : '';
  const time = date instanceof Date && !Number.isNaN(date.getTime())
    ? `<span class="msg-time">${escHtml(messageActionTime(date))}</span>`
    : '';
  return `<div class="msg-actions">${copy}${ratings}${branch}${feedback}${time}</div>`;
}

function addMessage(role, content, remember = true, idx = -1, historyTurn = 0) {
  const now = remember ? new Date() : null;
  if (remember) messages.push({ role, content, time: now });
  const index = idx >= 0 ? idx : (remember ? messages.length - 1 : -1);
  const area = document.getElementById('chatArea');
  const idxAttr = index >= 0 ? ` data-idx="${index}"` : '';
  const turnAttr = historyTurn > 0 ? ` data-history-turn="${historyTurn}"` : '';

  if (role === 'user') {
    area.insertAdjacentHTML('beforeend', `
      <div class="message message-user"${idxAttr}${turnAttr}>
        <div class="message-bubble">${escHtml(content)}</div>
        ${messageActionsMarkup('user', now)}
      </div>`);
  } else {
    area.insertAdjacentHTML('beforeend', `
      <div class="message message-assistant"${idxAttr}${turnAttr}>
        <div class="message-avatar">M</div>
        <div class="message-body">
          <div class="message-content">${formatContent(content)}</div>
          ${messageActionsMarkup('assistant', now)}
        </div>
      </div>`);
  }

  autoScroll();
  updateEmptyLayout();
}

// Attach the hover actions row to a streamed assistant message.
function attachMessageActions(el, idx) {
  if (!el || idx < 0) return;
  el.setAttribute('data-idx', String(idx));
  const body = el.querySelector('.message-body');
  if (!body || body.querySelector('.msg-actions')) return;
  body.insertAdjacentHTML('beforeend', messageActionsMarkup('assistant'));
}

function copyMessage(btn) {
  const msg = btn.closest('.message');
  const src = msg.querySelector('.message-content') || msg.querySelector('.message-bubble');
  if (!src) return;
  const text = src.textContent;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(() => {
      btn.innerHTML = MESSAGE_ACTION_ICONS.done;
      setTimeout(() => { btn.innerHTML = MESSAGE_ACTION_ICONS[btn.dataset.icon] || MESSAGE_ACTION_ICONS.copy; }, 1200);
    }, () => {});
  }
}

// Feedback (DSH command-feedback parity): records a log-only remark on
// the session. Never enters model context — it lands in the JSONL as a
// "feedback" entry the resume path ignores.
async function feedbackMessage(btn) {
  if (!currentSessionId) { showToast('Start a session before leaving feedback'); return; }
  const msg = btn.closest('.message');
  const idx = msg ? msg.dataset.idx : '';
  const text = (window.prompt('\u53cd\u9988\u5907\u6ce8\uFF08\u4ec5\u8bb0\u5f55\uFF0C\u4e0d\u53d1\u7ed9\u6a21\u578b\uFF09:', '') || '').trim();
  if (!text) return;
  try {
    const res = await fetch('/api/feedback', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId, kind: 'remark', text: (idx ? '(msg ' + idx + ') ' : '') + text }),
    });
    if (!res.ok) throw new Error('feedback: ' + res.status);
    btn.innerHTML = MESSAGE_ACTION_ICONS.done;
    setTimeout(() => { btn.innerHTML = MESSAGE_ACTION_ICONS.feedback; }, 1200);
    showToast('Feedback recorded');
  } catch (e) {
    showToast('Feedback failed: ' + e.message);
  }
}

// rateMessage records a message-level 👍/👎 (DSH message-feedback parity).
// Log-only like remarks; the button gets a .rated class so the user sees
// which way they voted.
async function rateMessage(btn, rating) {
  if (!currentSessionId) { showToast('Start a session before rating'); return; }
  const msg = btn.closest('.message');
  const idx = msg ? msg.dataset.idx : '';
  try {
    const res = await fetch('/api/feedback', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId, kind: 'rating', rating: rating, msgIdx: idx || '' }),
    });
    if (!res.ok) throw new Error('rating: ' + res.status);
    const row = btn.closest('.msg-actions');
    row.querySelectorAll('.msg-btn.rated').forEach(b => b.classList.remove('rated'));
    btn.classList.add('rated');
    showToast(rating === 'up' ? '\u8bb0\u5f55\u4e86 \ud83d\udc4d' : '\u8bb0\u5f55\u4e86 \ud83d\udc4e');
  } catch (e) {
    showToast('Rating failed: ' + e.message);
  }
}

async function branchMessage(btn) {
  const msg = btn.closest('.message');
  const idx = msg ? parseInt(msg.dataset.idx, 10) : NaN;
  if (!currentSessionId || !Number.isFinite(idx)) { showToast('Cannot branch this message'); return; }
  try {
    const res = await fetch('/api/fork', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId, messageIndex: idx })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'fork: ' + res.status);
    showToast('Branched to a new session');
    await loadSessions();
    await resumeSession(data.sessionId);
  } catch (e) {
    showToast('Branch failed: ' + e.message);
  }
}

function messageText(content) {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return '';
  return content.filter(b => b && b.type === 'text').map(b => b.text || '').join('\n');
}

// Runtime instructions are transported as user-role text because provider
// APIs have no mid-conversation system role. They are never chat content.
// Filter complete and currently-streaming envelopes so a model that echoes an
// internal rescue prompt cannot expose it in Desktop (screenshot regression).
const INTERNAL_TRANSCRIPT_SECTION_RE = /<(system-reminder|memory-context|auto-retrieve|peer_message|task-context|project-context|job_notification|sub_agent_idle|memory_consolidation_done|monitor_event|post_compact_context|metis-internal-review)(?:\s[^>]*)?>[\s\S]*?<\/\1\s*>/gi;
const UNTERMINATED_INTERNAL_SECTION_RE = /<(?:system-reminder|memory-context|auto-retrieve|peer_message|task-context|project-context|job_notification|sub_agent_idle|memory_consolidation_done|monitor_event|post_compact_context|metis-internal-review)(?:\s[^>]*)?>[\s\S]*$/i;

function visibleTranscriptText(value) {
  return String(value == null ? '' : value)
    .replace(INTERNAL_TRANSCRIPT_SECTION_RE, '')
    .replace(UNTERMINATED_INTERNAL_SECTION_RE, '');
}

// Rebuild the full transcript from session history (DSH parity: reloading
// or resuming a session reconstructs tool call rows, not just text).
// Shapes come from the JSONL: assistant messages carry {type:'tool_use',
// tool_use_id, name, input} blocks; the following user message carries
// {type:'tool_result', tool_use_id, content, is_error?} blocks.
function renderHistoryMessages(history) {
  queueMicrotask(() => restoreTodoPlanFromHistory(history));
  const area = document.getElementById('chatArea');
  // Avoid forcing a layout/scroll for every reconstructed history block. The
  // selected transcript is pinned once, after the complete history exists.
  followOutput = false;
  area.innerHTML = '';
  let i = 0;
  let historyTurn = 0;
  (history || []).forEach(m => {
    const role = m.role === 'user' ? 'user' : 'assistant';
    const blocks = Array.isArray(m.content)
      ? m.content
      : (m.content != null ? [{ type: 'text', text: String(m.content) }] : []);
    if (role === 'user' && blocks.some(b => {
      if (!b || b.type !== 'text') return false;
      const text = String(b.text || '');
      return text.trim() && !text.startsWith('[user steer mid-turn] ') && visibleTranscriptText(text).trim();
    })) historyTurn++;
    blocks.forEach(b => {
      if (!b || typeof b !== 'object') return;
      if (b.type === 'text' && (b.text || '').trim()) {
        const rawShown = role === 'user' && String(b.text).startsWith('[user steer mid-turn] ')
          ? String(b.text).slice('[user steer mid-turn] '.length) : b.text;
        const shown = visibleTranscriptText(rawShown);
        if (shown.trim()) addMessage(role, shown, false, i++, historyTurn);
      } else if (role === 'assistant' && b.type === 'thinking' && (b.text || '').trim()) {
        try {
          appendThinkingRow(b.text);
        } catch (e) { /* skip malformed block */ }
      } else if (role === 'assistant' && b.type === 'redacted_thinking') {
        // Provider ciphertext lives in b.data for request round-tripping;
        // history UI intentionally renders only the safe placeholder.
        try { appendThinkingRow('', { redacted: true }); }
        catch (e) { /* skip malformed block */ }
      } else if (b.type === 'tool_use' && b.tool_use_id) {
        let input = '';
        try { input = JSON.stringify(b.input || {}); } catch (e) { input = ''; }
        try { handleToolStart({ tool: b.name || 'tool', id: b.tool_use_id, input: input }); }
        catch (e) { /* malformed history entry: skip the row */ }
      } else if (b.type === 'tool_result' && b.tool_use_id) {
        const out = typeof b.content === 'string' ? b.content
          : (b.content != null ? JSON.stringify(b.content) : '');
        const det = toolDetails[b.tool_use_id] || {};
        try {
          handleToolResult({
            tool: det.name || 'tool',
            id: b.tool_use_id,
            output: out,
            isError: !!b.is_error,
            elapsedMs: 0,
            display: b.display,
            presentation: b.presentation
          });
        } catch (e) { /* skip */ }
      }
    });
  });
  updateEmptyLayout();
  resumeAutoScroll();
  void restoreHistoryMessageMetadata(currentSessionId);
}

async function restoreHistoryMessageMetadata(sessionId) {
  if (!sessionId) return;
  try {
    const res = await fetch('/api/trace?sessionId=' + encodeURIComponent(sessionId) + '&limit=1');
    if (!res.ok) return;
    const data = await res.json();
    if (currentSessionId !== sessionId) return;
    (data.turnMetrics || []).forEach(metric => {
      const turn = Number(metric.turn) || 0;
      if (turn <= 0) return;

      const userActions = document.querySelector(`.message-user[data-history-turn="${turn}"] .msg-actions`);
      const startedAt = new Date(metric.startedAt || '');
      if (userActions && !Number.isNaN(startedAt.getTime())) {
        userActions.querySelector('.msg-time')?.remove();
        userActions.insertAdjacentHTML('beforeend', `<span class="msg-time">${escHtml(messageActionTime(startedAt))}</span>`);
      }

      const assistantRows = document.querySelectorAll(`.message-assistant[data-history-turn="${turn}"] .msg-actions`);
      const assistantActions = assistantRows.length ? assistantRows[assistantRows.length - 1] : null;
      if (!assistantActions) return;
      assistantActions.querySelector('.msg-time')?.remove();
      assistantActions.querySelector('.msg-metrics')?.remove();
      const completedAt = new Date(metric.completedAt || '');
      if (!Number.isNaN(completedAt.getTime())) {
        assistantActions.insertAdjacentHTML('beforeend', `<span class="msg-time">${escHtml(messageActionTime(completedAt))}</span>`);
      }
      const metrics = turnMetricsMarkup(metric);
      if (metrics) {
        assistantActions.insertAdjacentHTML('beforeend', metrics);
        assistantActions.classList.add('with-metrics');
      }
    });
  } catch (_) {}
}

function showError(text) {
  addMessage('assistant', `Error: ${text}`);
}

function formatContent(text) {
  // Compact markdown renderer: fenced code blocks first (with a language
  // label + copy button), then block-level elements, then inline marks.
  const blocks = [];
  const src0 = String(text).replace(/```([\w+-]*)\n?([\s\S]*?)```/g, (m, lang, code) => {
    blocks.push({ lang: lang || '', code: code.replace(/\n$/, '') });
    return '\nCODEBLOCK' + (blocks.length - 1) + '\n';
  });

  // Display math ($$...$$ and \[...\]) — extracted to whole-line tokens so
  // multi-line TeX bodies work and never touch the inline pipeline.
  const mathBlocks = [];
  const src = src0
    .replace(/\$\$([\s\S]+?)\$\$/g, (m, tex) => {
      mathBlocks.push(tex.trim());
      return '\nMATHDISPLAY' + (mathBlocks.length - 1) + '\n';
    })
    .replace(/\\\[([\s\S]+?)\\\]/g, (m, tex) => {
      mathBlocks.push(tex.trim());
      return '\nMATHDISPLAY' + (mathBlocks.length - 1) + '\n';
    });

  let html = '';
  let inUl = false, inOl = false, inQuote = false, inPre = false;
  const closeAll = () => {
    if (inUl) { html += '</ul>'; inUl = false; }
    if (inOl) { html += '</ol>'; inOl = false; }
    if (inQuote) { html += '</blockquote>'; inQuote = false; }
  };

  const raws = src.split('\n');
  for (let index = 0; index < raws.length; index++) {
    const raw = raws[index];
    if (raw === '\u0000') continue;
    const t = raw.trim();
    if (/^CODEBLOCK\d+$/.test(t)) {
      closeAll();
      html += codeBlockHtml(blocks[parseInt(t.slice(9), 10)]);
      continue;
    }
    if (/^MATHDISPLAY\d+$/.test(t)) {
      closeAll();
      html += renderMathDisplay(mathBlocks[parseInt(t.slice(11), 10)]);
      continue;
    }
    if (inPre) {
      if (t === '```') { html += '</pre>'; inPre = false; continue; }
      html += escHtml(raw) + '\n';
      continue;
    }
    if (!t) { closeAll(); continue; }
    if (t === '```') { closeAll(); html += '<pre class="md-plain-pre">'; inPre = true; continue; }
    const h = t.match(/^(#{1,6})\s/);
    if (h) { closeAll(); html += '<h' + h[1].length + '>' + inlineMd(t.slice(h[1].length).trim()) + '</h' + h[1].length + '>'; continue; }
    if (/^[-*]\s/.test(t)) { if (!inUl) { closeAll(); html += '<ul>'; inUl = true; } html += '<li>' + inlineMd(t.slice(2)) + '</li>'; continue; }
    if (/^\d+\.\s/.test(t)) { if (!inOl) { closeAll(); html += '<ol>'; inOl = true; } html += '<li>' + inlineMd(t.replace(/^\d+\.\s/, '')) + '</li>'; continue; }
    if (t.startsWith('>')) { if (!inQuote) { closeAll(); html += '<blockquote>'; inQuote = true; } html += '<p>' + inlineMd(t.replace(/^>\s?/, '')) + '</p>'; continue; }
    if (/^(-{3,}|\*{3,})$/.test(t)) { closeAll(); html += '<hr>'; continue; }
    if (raw.startsWith('|') && raw.trim().endsWith('|')) {
      // Collect the contiguous table block and render it directly.
      closeAll();
      html += mdTableBlock(raws, index);
      continue;
    }
    closeAll();
    html += '<p>' + inlineMd(t) + '</p>';
  }
  closeAll();
  if (inPre) html += '</pre>';
  return html;
}

// Table support: consecutive | separated rows become <table>. Operates on
// the RAW source lines (paragraph rendering joins lines, so a post-hoc
// pass on HTML can neither find tables nor spare code blocks).
function mdTableBlock(raws, startIdx) {
  const rows = [];
  let i = startIdx;
  while (i < raws.length && raws[i].trim().startsWith('|') && raws[i].trim().endsWith('|')) {
    rows.push(raws[i].trim().replace(/^\|/, '').replace(/\|$/, ''));
    i++;
  }
  // Consume the collected lines from the main loop.
  for (let k = startIdx + 1; k < i; k++) raws[k] = '\u0000';
  const valid = rows.filter(r => r.includes('|'));
  if (valid.length < 2) {
    return rows.map(r => '<p>' + inlineMd(r) + '</p>').join('');
  }
  const cells = valid.map(r => r.split('|').map(c => c.trim()));
  const sepRow = /^:?-{2,}:?$/.test(cells[1][0]) ? 1 : -1;
  let t = '<table class="md-table"><thead><tr>' +
    cells[0].map(c => '<th>' + inlineMd(c) + '</th>').join('') + '</tr></thead><tbody>';
  for (let r = Math.max(1, sepRow + 1); r < cells.length; r++) {
    t += '<tr>' + cells[r].map(c => '<td>' + inlineMd(c) + '</td>').join('') + '</tr>';
  }
  return t + '</tbody></table>';
}

function renderMdTablesLegacy(html) {
  const lines = html.split('\n');
  const out = [];
  let table = [];
  const flush = () => {
    if (table.length === 0) return;
    const rows = table.map(row => row.trim().replace(/^\|/, '').replace(/\|$/, ''));
    if (rows.every(r => r.includes('|')) && rows.length >= 2) {
      const cells = rows.map(r => r.split('|').map(c => c.trim()));
      let t = '<table class="md-table"><thead><tr>' +
        cells[0].map(c => '<th>' + inlineMd(c) + '</th>').join('') + '</tr></thead><tbody>';
      const bodyRows = /^:?-{2,}:?$/.test(cells[1][0]) ? cells.slice(2) : cells.slice(1);
      t += bodyRows.map(r => '<tr>' + r.map(c => '<td>' + inlineMd(c) + '</td>').join('') + '</tr>').join('');
      t += '</tbody></table>';
      out.push(t);
    } else {
      out.push(...table.map(line => '<p>' + inlineMd(line) + '</p>'));
    }
    table = [];
  };
  for (const line of lines) {
    if (line.trim().startsWith('|') && line.trim().endsWith('|')) { table.push(line); continue; }
    flush();
    out.push(line);
  }
  flush();
  return out.join('\n');
}

function inlineMd(s) {
  // Code spans first (raw stash): a `$` inside backticks must never become
  // math. Then math is stashed behind alphanumeric placeholders so it
  // survives escHtml; everything is re-substituted at the end.
  const codeStash = [];
  let out = String(s).replace(/`([^`]+)`/g, (m, code) => {
    codeStash.push(code);
    return 'CODESPAN' + (codeStash.length - 1) + 'XQ';
  });
  const stash = [];
  // \( ... \) backslash delimiters (DSH parity: "inline dollar and backslash")
  out = out.replace(/\\\((.+?)\\\)/g, (_m, tex) => {
    stash.push(tex);
    return 'MATHINLINE' + (stash.length - 1) + 'ZQ';
  });
  // $ ... $ — pandoc-style guards: no space at either TeX end, no
  // word-char/digit adjacency (so "costs $5 and $6" stays text).
  out = out.replace(/(^|[^\w\\$])\$([^$\n]+)\$([^\w$]|$)/g, (m, pre, tex, post) => {
    if (/^\s|\s$/.test(tex) || !tex.trim()) return m;
    stash.push(tex);
    return pre + 'MATHINLINE' + (stash.length - 1) + 'ZQ' + post;
  });
  out = escHtml(out)
    .replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|[\s(])\*([^*\s][^*\n]*?)\*/g, '$1<em>$2</em>')
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, text, url) => {
      // Protocol whitelist: never let a javascript:/data: URL into href.
      const safe = /^(https?:|mailto:)/i.test(url) ? url : '#';
      return '<a href="' + escAttr(safe) + '" target="_blank" rel="noopener">' + text + '</a>';
    });
  for (let i = 0; i < codeStash.length; i++) {
    out = out.split('CODESPAN' + i + 'XQ').join('<code class="md-inline-code">' + escHtml(codeStash[i]) + '</code>');
  }
  for (let i = 0; i < stash.length; i++) {
    out = out.split('MATHINLINE' + i + 'ZQ').join(renderMathInline(stash[i]));
  }
  return out;
}

// KaTeX wrapper: strict-ish render with throwOnError:false covers DSH's
// three-arm chain (strict → strict:ignore → error span) for vanilla JS.
function katexHtml(tex, displayMode) {
  try {
    if (window.katex) {
      return window.katex.renderToString(tex, { displayMode, throwOnError: false, strict: 'ignore', trust: false });
    }
  } catch (_) { /* fall through to source fallback */ }
  return '<code class="md-math-fallback">' + escHtml(tex) + '</code>';
}

function renderMathInline(tex) {
  return '<span class="md-math md-math-inline">' + katexHtml(tex, false) + '</span>';
}

function renderMathDisplay(tex) {
  return '<div class="md-math md-math-block">' + katexHtml(tex, true) + '</div>';
}

function codeBlockHtml(b) {
  const label = b.lang ? escHtml(b.lang) : 'code';
  return '<div class="md-codeblock">' +
    '<div class="md-code-head"><span>' + label + '</span>' +
    '<button class="md-copy-btn" onclick="copyCodeBlock(this)">Copy</button></div>' +
    '<pre><code>' + escHtml(b.code) + '</code></pre></div>';
}

function copyCodeBlock(btn) {
  const pre = btn.closest('.md-codeblock').querySelector('pre');
  if (!pre) return;
  const text = pre.textContent;
  const done = () => { btn.textContent = 'Copied'; setTimeout(() => { btn.textContent = 'Copy'; }, 1500); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, () => {});
  }
}

// --- Input ---
const COMPOSER_COMMANDS = [
  { name: '/help', label: 'Command help', hint: 'Browse the complete Desktop command catalog', category: 'System' },
  { name: '/keybindings', label: 'Keyboard shortcuts', hint: 'Show command-palette navigation keys', category: 'System' },
  { name: '/new', label: 'New session', hint: 'Start a fresh conversation', category: 'Session' },
  { name: '/clear', label: 'New session', hint: 'Start a fresh conversation', category: 'Session' },
  { name: '/sessions', label: 'Find sessions', hint: 'Search and resume a saved session', category: 'Session' },
  { name: '/resume', label: 'Resume session', hint: 'Search and resume a saved conversation', category: 'Session' },
  { name: '/history', label: 'Conversation history', hint: 'Return to the current conversation transcript', category: 'Session' },
  { name: '/rename', label: 'Rename session', hint: 'Rename the current conversation', category: 'Session' },
  { name: '/title', label: 'Set session title', hint: 'Rename the current conversation', category: 'Session' },
  { name: '/branch', label: 'Branch session', hint: 'Fork the current conversation at its latest message', category: 'Session' },
  { name: '/clear-history', label: 'Clear history', hint: 'Remove conversation messages but keep this session', category: 'Session' },
  { name: '/retry', label: 'Retry response', hint: 'Remove and rerun the latest user turn', category: 'Session' },
  { name: '/undo', label: 'Undo last turn', hint: 'Remove the latest exchange and restore its prompt', category: 'Session' },
  { name: '/edit', label: 'Edit last prompt', hint: 'Undo the latest exchange and restore its prompt', category: 'Session' },
  { name: '/save', label: 'Save session', hint: 'Flush the current transcript to disk', category: 'Session' },
  { name: '/compact', label: 'Compact history', hint: 'Summarize older context now', category: 'Session' },
  { name: '/export', label: 'Export session log', hint: 'Write the current transcript to a file', category: 'Session' },
  { name: '/feedback', label: 'Record feedback', hint: 'Save a private log-only note', category: 'Session' },
  { name: '/artifact', label: 'Create or update artifact', hint: 'Ask the agent to build a durable local HTML artifact', category: 'Artifact' },
  { name: '/artifacts', label: 'Open artifacts', hint: 'Browse artifacts saved with this session', category: 'Artifact' },
  { name: '/goal', label: 'Set or view goal', hint: 'Track a durable long-running objective', category: 'Workflow' },
  { name: '/plan', label: 'Plan mode', hint: 'Enter or leave read-only planning', category: 'Mode' },
  { name: '/permission', label: 'Permission mode', hint: 'Choose the execution approval policy', category: 'Mode' },
  { name: '/permissions', label: 'Permission rules', hint: 'Review or change the execution approval policy', category: 'Mode' },
  { name: '/mode', label: 'Execution mode', hint: 'Review or change the execution approval policy', category: 'Mode' },
  { name: '/default', label: 'Default permissions', hint: 'Ask before state-changing actions', category: 'Mode' },
  { name: '/acceptEdits', label: 'Accept edits', hint: 'Allow file edits; ask for other state changes', category: 'Mode' },
  { name: '/dontAsk', label: 'Do not ask', hint: 'Deny actions that would require approval', category: 'Mode' },
  { name: '/bypassPermissions', label: 'Bypass permissions', hint: 'Run without approval prompts — use with care', category: 'Mode' },
  { name: '/model', label: 'Choose model', hint: 'Switch the model used by this session', category: 'Model' },
  { name: '/effort', label: 'Reasoning effort', hint: 'Choose default, low, medium, or high', category: 'Model' },
  { name: '/providers', label: 'Model providers', hint: 'Open provider configuration', category: 'Model' },
  { name: '/provider', label: 'Model provider', hint: 'Open provider configuration', category: 'Model' },
  { name: '/presets', label: 'Agent presets', hint: 'Open agent preset configuration', category: 'Agent' },
  { name: '/skills', label: 'Skills', hint: 'Browse installed skill-providing plugins', category: 'Agent' },
  { name: '/plugins', label: 'Plugin marketplace', hint: 'Browse, install, or remove plugins', category: 'Agent' },
  { name: '/agents', label: 'Sub-agents', hint: 'Show active sub-agents and background work', category: 'Agent' },
  { name: '/tasks', label: 'Background tasks', hint: 'Show active jobs for this session', category: 'Agent' },
  { name: '/todos', label: 'Session checklist', hint: 'Show task and checklist activity for this session', category: 'Agent' },
  { name: '/tools', label: 'Available tools', hint: 'Show tools registered in the active runtime', category: 'Agent' },
  { name: '/routing', label: 'Smart routing', hint: 'Open model routing configuration', category: 'Configure' },
  { name: '/config', label: 'Configuration', hint: 'Open effective METIS configuration', category: 'Configure' },
  { name: '/settings', label: 'Desktop settings', hint: 'Configure Desktop behavior', category: 'Configure' },
  { name: '/appearance', label: 'Appearance', hint: 'Open theme and display settings', category: 'Configure' },
  { name: '/theme', label: 'Color theme', hint: 'Open appearance settings or apply dark, light, or auto', category: 'Configure' },
  { name: '/thinking', label: 'Thinking display', hint: 'Set reasoning rows to show, hide, or auto', category: 'Configure' },
  { name: '/trace', label: 'Open trajectory', hint: 'Inspect the current session timeline', category: 'Inspect' },
  { name: '/status', label: 'Session status', hint: 'Show workspace, agents, tasks, and context', category: 'Inspect' },
  { name: '/session-info', label: 'Session information', hint: 'Show the active session runtime summary', category: 'Inspect' },
  { name: '/stats', label: 'Runtime statistics', hint: 'Show workspace, agents, tasks, and context', category: 'Inspect' },
  { name: '/context', label: 'Context usage', hint: 'Show current token-window utilization', category: 'Inspect' },
  { name: '/cost', label: 'Token usage', hint: 'Show current context-token usage', category: 'Inspect' },
  { name: '/usage', label: 'Token usage', hint: 'Show current context-token usage', category: 'Inspect' },
  { name: '/doctor', label: 'Desktop health', hint: 'Check the local Desktop server and active runtime', category: 'System' },
  { name: '/version', label: 'METIS version', hint: 'Show the running Desktop and METIS version', category: 'System' },
  { name: '/abort', label: 'Stop current turn', hint: 'Cancel the running response', category: 'Run' },
  { name: '/stop', label: 'Stop current turn', hint: 'Cancel the running response', category: 'Run' },
];

const COMPOSER_ADD_ACTIONS = [
  { section: 'add', action: 'attachment', label: 'Image attachments', labelZh: '\u56fe\u7247\u9644\u4ef6', hint: 'Keep the original image upload flow', hintZh: '\u4fdd\u7559\u539f\u6709\u56fe\u7247\u4e0a\u4f20\u529f\u80fd' },
  { section: 'add', action: 'clipboard', label: 'Files and folders', labelZh: '\u6587\u4ef6\u548c\u6587\u4ef6\u5939', hint: 'Insert absolute paths copied in Finder', hintZh: '\u63d2\u5165 Finder \u4e2d\u5df2\u590d\u5236\u9879\u76ee\u7684\u7edd\u5bf9\u8def\u5f84' },
  { section: 'workflow', action: 'goal', label: 'Goal', labelZh: '\u76ee\u6807', hint: 'Set or view a long-running objective', hintZh: '\u8bbe\u7f6e\u6216\u67e5\u770b\u957f\u65f6\u95f4\u4efb\u52a1\u76ee\u6807' },
  { section: 'workflow', action: 'plan', label: 'Plan mode', labelZh: '\u8ba1\u5212\u6a21\u5f0f', hint: 'Enter or leave planning mode', hintZh: '\u8fdb\u5165\u6216\u9000\u51fa\u89c4\u5212\u6a21\u5f0f' },
  { section: 'command', action: 'commands', label: 'All commands', labelZh: '\u5168\u90e8\u547d\u4ee4', hint: 'Open the independent slash-command palette', hintZh: '\u6253\u5f00\u72ec\u7acb\u7684\u659c\u6760\u547d\u4ee4\u9762\u677f' },
  { section: 'command', action: 'plugins', label: 'Skills and plugins', labelZh: '\u6280\u80fd\u4e0e\u63d2\u4ef6', hint: 'Open installed extensions and marketplace', hintZh: '\u6253\u5f00\u5df2\u5b89\u88c5\u6269\u5c55\u548c\u63d2\u4ef6\u5546\u573a' },
];
let commandMatches = [];
let commandSelection = 0;
let commandTotalMatches = 0;
let composerActionDialog = null;

function composerActionIcon(action) {
  const paths = {
    attachment: '<path d="M6.5 8.8 10.9 4.4a2.2 2.2 0 0 1 3.1 3.1L8.3 13.2a3.5 3.5 0 0 1-5-5l6-6"/>',
    clipboard: '<path d="M2.5 4.5h4l1.2 1.4h5.8v7.6h-11z"/><path d="M2.5 5.9h11"/>',
    goal: '<circle cx="8" cy="8" r="5.5"/><circle cx="8" cy="8" r="2.4"/><path d="M11.8 4.2 8 8"/>',
    plan: '<path d="M3 4h10M3 8h7M3 12h5"/><path d="m11 11 1.2 1.2L14.5 10"/>',
    commands: '<path d="M4 5.5 2.5 8 4 10.5M7 11h5"/>',
    compact: '<path d="M5.5 2.5v3h-3M10.5 13.5v-3h3"/><path d="M3 5.5a5.5 5.5 0 0 1 9.6-1.4M13 10.5a5.5 5.5 0 0 1-9.6 1.4"/>',
    export: '<path d="M8 2.5v7M5.5 7 8 9.5 10.5 7"/><path d="M3 11v2.5h10V11"/>',
    feedback: '<path d="M2.5 3.5h11v7h-6L4 13v-2.5H2.5z"/>',
    permission: '<path d="M8 2.2 13 4v3.6c0 3-2 5.1-5 6.2-3-1.1-5-3.2-5-6.2V4z"/><path d="m5.8 8 1.4 1.4 3-3"/>',
    model: '<circle cx="8" cy="8" r="2.2"/><path d="M8 1.8c1.7 0 3 2.8 3 6.2s-1.3 6.2-3 6.2S5 11.4 5 8s1.3-6.2 3-6.2Z"/><path d="M2.6 4.9c.9-1.5 4-1.2 7 .5s4.9 4.2 4 5.7-4 1.2-7-.5-4.9-4.2-4-5.7Z"/>',
    plugins: '<path d="M6.2 2.5h3.6v3.1h3.1v3.6H9.8v3.1H6.2V9.2H3.1V5.6h3.1z"/>'
  };
  return '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.35" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + (paths[action] || paths.commands) + '</svg>';
}

function renderComposerAddMenu() {
  const menu = document.getElementById('composerAddMenu');
  if (!menu) return;
  let section = '';
  menu.innerHTML = COMPOSER_ADD_ACTIONS.map(item => {
    let divider = '';
    if (item.section !== section) {
      if (section) divider = '<div class="composer-add-divider" role="separator"></div>';
      section = item.section;
    }
    return divider + '<button type="button" class="composer-add-option" role="menuitem" data-action="' + escAttr(item.action) + '" onclick="runComposerAddAction(\'' + escOnclick(item.action) + '\',this)">' +
      '<span class="composer-add-icon">' + composerActionIcon(item.action) + '</span><span class="composer-add-copy"><strong>' + escHtml(uiText(item.label, item.labelZh)) + '</strong><small>' + escHtml(uiText(item.hint, item.hintZh)) + '</small></span></button>';
  }).join('');
}

function toggleComposerAddMenu(event) {
  if (event) event.stopPropagation();
  const menu = document.getElementById('composerAddMenu');
  const button = document.getElementById('attachmentBtn');
  if (!menu || !button) return;
  const opening = menu.hidden;
  closeCommandMenu();
  menu.hidden = !opening;
  button.setAttribute('aria-expanded', opening ? 'true' : 'false');
  if (opening) {
    renderComposerAddMenu();
    // The empty-state composer can sit near the middle of a short window.
    // Constrain the catalog to the real room above it so the first section is
    // never clipped by the WebView edge; the rest remains internally scrollable.
    const inputBox = document.querySelector('.input-box');
    const roomAbove = inputBox ? Math.floor(inputBox.getBoundingClientRect().top - 12) : 350;
    menu.style.maxHeight = Math.max(140, Math.min(350, roomAbove)) + 'px';
    menu.scrollTop = 0;
    const first = menu.querySelector('button');
    if (first) requestAnimationFrame(() => first.focus());
  }
}

function closeComposerAddMenu(restoreFocus) {
  const menu = document.getElementById('composerAddMenu');
  const button = document.getElementById('attachmentBtn');
  if (menu) menu.hidden = true;
  if (button) {
    button.setAttribute('aria-expanded', 'false');
    if (restoreFocus) button.focus();
  }
}

async function runComposerAddAction(action, trigger) {
  closeComposerAddMenu(false);
  switch (action) {
    case 'attachment': openAttachmentPicker(); break;
    case 'clipboard': await pasteAllClipboardFilePaths(); break;
    case 'commands': {
      const input = document.getElementById('inputField');
      input.value = '/';
      onComposerInput(input);
      input.focus();
      break;
    }
    case 'goal': openComposerActionDialog('goal', trigger); break;
    case 'plan': await executeComposerCommand('/plan'); break;
    case 'compact': await executeComposerCommand('/compact'); break;
    case 'export': await executeComposerCommand('/export'); break;
    case 'feedback': openComposerActionDialog('feedback', trigger); break;
    case 'permission': openComposerActionDialog('permission', trigger); break;
    case 'model': toggleModelMenu(); break;
    case 'plugins': await openPluginSettings(); break;
  }
}

document.addEventListener('pointerdown', event => {
  const menu = document.getElementById('composerAddMenu');
  const button = document.getElementById('attachmentBtn');
  if (!menu || menu.hidden || menu.contains(event.target) || button && button.contains(event.target)) return;
  closeComposerAddMenu(false);
});
document.addEventListener('keydown', event => {
  const menu = document.getElementById('composerAddMenu');
  if (!menu || menu.hidden) return;
  if (event.key === 'Escape') {
    event.preventDefault();
    closeComposerAddMenu(true);
    return;
  }
  if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
  const items = Array.from(menu.querySelectorAll('.composer-add-option'));
  if (!items.length) return;
  event.preventDefault();
  const current = items.indexOf(document.activeElement);
  const delta = event.key === 'ArrowDown' ? 1 : -1;
  items[(current + delta + items.length) % items.length].focus();
});

function onComposerInput(el) {
  autoResize(el);
  updateCommandMenu(el.value);
}

function updateCommandMenu(value) {
  const q = String(value || '').trim().toLowerCase();
  if (!q.startsWith('/') || q.includes(' ')) {
    closeCommandMenu();
    return;
  }
  const needle = q.slice(1);
  const matches = COMPOSER_COMMANDS.filter(c =>
    c.name.toLowerCase().startsWith(q) ||
    c.label.toLowerCase().includes(needle) ||
    c.hint.toLowerCase().includes(needle) ||
    c.category.toLowerCase().includes(needle)).sort((a, b) => {
      const score = c => {
        const command = c.name.toLowerCase();
        if (command === q) return 0;
        if (command.startsWith(q)) return 1;
        if (c.label.toLowerCase().startsWith(needle)) return 2;
        if (c.label.toLowerCase().includes(needle)) return 3;
        if (c.category.toLowerCase().includes(needle)) return 4;
        return 5;
      };
      return score(a) - score(b);
    });
  commandTotalMatches = matches.length;
  commandMatches = matches;
  commandSelection = Math.min(commandSelection, Math.max(0, commandMatches.length - 1));
  closeComposerAddMenu(false);
  renderCommandMenu();
}

function renderCommandMenu() {
  const menu = document.getElementById('commandMenu');
  if (!commandMatches.length) { closeCommandMenu(); return; }
  menu.innerHTML = commandMatches.map((c, i) =>
    '<button type="button" role="option" aria-selected="' + (i === commandSelection ? 'true' : 'false') + '" class="command-option' + (i === commandSelection ? ' selected' : '') + '" onclick="chooseComposerCommand(\'' + escOnclick(c.name) + '\')">' +
    '<span class="command-name">' + escHtml(c.name.slice(1)) + '</span><span class="command-label">' + escHtml(c.label) + '</span><small>' + escHtml(c.hint) + '</small><span class="command-category">' + escHtml(c.category) + '</span></button>'
  ).join('') + '<div class="command-menu-foot">' + commandTotalMatches + ' commands · type to filter</div>';
  menu.style.display = 'block';
  const inputBox = document.querySelector('.input-box');
  const roomAbove = inputBox ? Math.floor(inputBox.getBoundingClientRect().top - 12) : 420;
  menu.style.maxHeight = Math.max(160, Math.min(420, roomAbove)) + 'px';
  const selected = menu.querySelector('.selected');
  if (selected) requestAnimationFrame(() => selected.scrollIntoView({ block: 'nearest' }));
}

function closeCommandMenu() {
  const menu = document.getElementById('commandMenu');
  if (menu) { menu.style.display = 'none'; menu.innerHTML = ''; }
  commandMatches = [];
  commandSelection = 0;
  commandTotalMatches = 0;
}

function chooseComposerCommand(name) {
  const input = document.getElementById('inputField');
  input.value = name + ' ';
  autoResize(input);
  closeCommandMenu();
  input.focus();
  input.setSelectionRange(input.value.length, input.value.length);
}

async function executeComposerCommand(text) {
  const match = String(text || '').trim().match(/^(\/[^\s]+)(?:\s+([\s\S]*))?$/);
  if (!match) return false;
  const name = match[1].toLowerCase();
  const input = String(match[2] || '').trim();
  if (!COMPOSER_COMMANDS.some(c => c.name.toLowerCase() === name)) return false;
  switch (name) {
    case '/help':
    case '/keybindings': openFullCommandCatalog(); break;
    case '/new':
    case '/clear': newChat(); break;
    case '/sessions':
    case '/resume': openSessionFinder(); break;
    case '/history': switchView('chat'); showToast('Showing conversation history'); break;
    case '/rename':
    case '/title': await renameCurrentSessionFromCommand(input); break;
    case '/branch': await branchCurrentSessionFromCommand(); break;
    case '/clear-history': await runSessionCommand('clear-history'); break;
    case '/undo':
    case '/edit': await runSessionCommand('undo'); break;
    case '/retry': await runSessionCommand('retry'); break;
    case '/save': await runSessionCommand('save'); break;
    case '/settings': await openSettings(); break;
    case '/appearance': await openComposerSettingsTab('appearance', 'settingsAppearanceBtn'); break;
    case '/theme': await runThemeCommand(input); break;
    case '/thinking': await runThinkingDisplayCommand(input); break;
    case '/providers':
    case '/provider': await openComposerSettingsTab('providers', 'settingsProvidersBtn'); break;
    case '/presets': await openComposerSettingsTab('presets', 'settingsPresetsBtn'); break;
    case '/routing': await openComposerSettingsTab('routing', 'settingsRoutingBtn'); break;
    case '/config': await openComposerSettingsTab('config', 'settingsConfigBtn'); break;
    case '/trace':
      if (!currentSessionId) showToast('Open a session before viewing its trajectory');
      else switchView('trace');
      break;
    case '/compact': await compactCurrentSession(input); break;
    case '/export': await exportSession(); break;
    case '/artifacts': openArtifactsPanel(); break;
    case '/artifact':
      // A request body intentionally continues through the normal model turn:
      // the model-facing Artifact tool owns creation and versioning. With no
      // body, use the local gallery as the most useful read-only default.
      if (!input) openArtifactsPanel();
      else return false;
      break;
    case '/feedback':
      if (input) await recordComposerFeedback(input);
      else openComposerActionDialog('feedback', document.getElementById('inputField'));
      break;
    case '/goal':
      if (input) await createComposerGoal(input, 'medium');
      else openComposerActionDialog('goal', document.getElementById('inputField'));
      break;
    case '/permission':
    case '/permissions':
    case '/mode':
      if (input) await setPermissionMode(input);
      else openComposerActionDialog('permission', document.getElementById('inputField'));
      break;
    case '/plan': await setPermissionMode(input === 'off' || approvalMode === 'plan' ? 'default' : 'plan'); break;
    case '/default': await setPermissionMode('default'); break;
    case '/acceptedits': await setPermissionMode('acceptEdits'); break;
    case '/dontask': await setPermissionMode('dontAsk'); break;
    case '/bypasspermissions': await setPermissionMode('bypassPermissions'); break;
    case '/model': toggleModelMenu(); break;
    case '/effort':
      if (input) await chooseEffort(input.toLowerCase());
      else { await loadEffort(); toggleEffortMenu(); }
      break;
    case '/skills':
    case '/plugins': await openPluginSettings(); break;
    case '/agents': showRuntimeSummary('agents'); break;
    case '/tasks':
    case '/todos': showRuntimeSummary('tasks'); break;
    case '/tools': showRuntimeSummary('tools'); break;
    case '/status':
    case '/session-info':
    case '/stats': showRuntimeSummary('status'); break;
    case '/context':
    case '/cost':
    case '/usage': showRuntimeSummary('context'); break;
    case '/doctor': await showDesktopHealth(false); break;
    case '/version': await showDesktopHealth(true); break;
    case '/abort':
    case '/stop': await stopTurn(); break;
    default: return false;
  }
  return true;
}

async function runSessionCommand(command) {
  if (!currentSessionId) { showToast('Open a session before running /' + command); return false; }
  try {
    const res = await fetch('/api/commands/session', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId, command: command })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || command + ': ' + res.status);
    if (command === 'save') {
      showToast('Session saved to disk');
      return true;
    }
    if (!data.changed) {
      showToast(data.message || 'Nothing to undo');
      return false;
    }
    messages = [];
    renderHistoryMessages(data.messages || []);
    await loadSessions();
    loadSessionStatsbar();
    if (command === 'clear-history') {
      showToast('Conversation history cleared');
      return true;
    }
    const prefill = String(data.prefill || '');
    if (command === 'retry' && prefill) {
      await runTurnItem({ text: prefill, images: [] });
      return true;
    }
    showToast('Last turn restored to the composer');
    requestAnimationFrame(() => {
      const composer = document.getElementById('inputField');
      composer.value = prefill;
      autoResize(composer);
      composer.focus();
      composer.setSelectionRange(prefill.length, prefill.length);
    });
    return true;
  } catch (error) {
    showToast('Command failed: ' + error.message);
    return false;
  }
}

async function runThemeCommand(input) {
  const value = String(input || '').trim().toLowerCase();
  if (!value) { await openComposerSettingsTab('appearance', 'settingsAppearanceBtn'); return; }
  if (!['auto', 'dark', 'light', 'dark-daltonized', 'nord', 'solarized-dark'].includes(value)) {
    showToast('Theme must be auto, dark, light, dark-daltonized, nord, or solarized-dark');
    return;
  }
  await saveSettingValue('ui.theme', value);
}

async function runThinkingDisplayCommand(input) {
  const value = String(input || '').trim().toLowerCase();
  if (!value) { await openComposerSettingsTab('appearance', 'settingsAppearanceBtn'); return; }
  if (!['show', 'hide', 'auto'].includes(value)) {
    showToast('Thinking display must be show, hide, or auto');
    return;
  }
  await saveSettingValue('ui.thinking_display', value);
}

async function showDesktopHealth(versionOnly) {
  try {
    const [healthRes, configRes, statusRes] = await Promise.all([fetch('/api/health'), fetch('/api/config'), fetch('/api/status')]);
    const health = await healthRes.json();
    const config = await configRes.json();
    const status = await statusRes.json();
    if (!healthRes.ok || !configRes.ok || !statusRes.ok) throw new Error('health check unavailable');
    if (versionOnly) showToast('METIS ' + (config.version || 'unknown') + ' · Desktop build ' + (health.build || 'unknown'));
    else showToast('Desktop healthy · ' + (status.toolCount || 0) + ' tools · build ' + (health.build || 'unknown'));
  } catch (error) { showToast('Desktop health check failed: ' + error.message); }
}

function openFullCommandCatalog() {
  const input = document.getElementById('inputField');
  input.value = '/';
  onComposerInput(input);
  input.focus();
}

function openSessionFinder() {
  const wrap = document.getElementById('sbSearch');
  const input = document.getElementById('sessionSearchInput');
  if (wrap && !wrap.classList.contains('open')) toggleSessionSearch();
  else if (input) input.focus();
}

async function renameCurrentSessionFromCommand(title) {
  if (!currentSessionId) { showToast('Open a session before renaming it'); return false; }
  const current = sessions.find(session => session.id === currentSessionId);
  const next = (title || prompt('Rename session', current ? current.title : '') || '').trim();
  if (!next) return false;
  try {
    const res = await fetch('/api/sessions/rename', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: currentSessionId, title: next }) });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'rename: ' + res.status);
    await loadSessions();
    showToast('Session renamed');
    return true;
  } catch (error) { showToast('Rename failed: ' + error.message); return false; }
}

async function branchCurrentSessionFromCommand() {
  if (!currentSessionId) { showToast('Open a session before branching it'); return false; }
  if (turnRunning) { showToast('Stop the current turn before branching'); return false; }
  try {
    const res = await fetch('/api/fork', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sessionId: currentSessionId, messageIndex: -1 }) });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'branch: ' + res.status);
    await loadSessions();
    await resumeSession(data.sessionId);
    showToast('Branched to a new session');
    return true;
  } catch (error) { showToast('Branch failed: ' + error.message); return false; }
}

async function openComposerSettingsTab(tab, buttonId) {
  await openSettings();
  showSettingsTab(tab, document.getElementById(buttonId));
}

function showRuntimeSummary(kind) {
  const d = lastStatusSnapshot || {};
  const used = Number(d.contextUsed) || 0, limit = Number(d.contextWindow) || 0;
  if (kind === 'agents') { showToast((d.subAgents || 0) + ' active sub-agents'); return; }
  if (kind === 'tasks') { showToast((d.backgroundTasks || 0) + ' active background tasks'); return; }
  if (kind === 'tools') {
    const names = Array.isArray(d.tools) ? d.tools : [];
    showToast((d.toolCount || names.length || 0) + ' tools' + (names.length ? ': ' + names.slice(0, 8).join(', ') + (names.length > 8 ? '…' : '') : ''));
    return;
  }
  if (kind === 'context') { showToast('Context: ' + fmtTokens(used) + (limit ? ' / ' + fmtTokens(limit) + ' tokens' : ' tokens')); return; }
  const contextPercent = limit ? Math.max(0, Math.min(100, Math.round(used / limit * 100))) : 0;
  showToast((d.workspace || 'metis') + ' · ' + (d.subAgents || 0) + ' sub-agents · ' + (d.backgroundTasks || 0) + ' tasks' + (limit ? ' · context ' + contextPercent + '%' : ''));
}

async function compactCurrentSession(instructions) {
  if (!currentSessionId) {
    showToast(uiText('Open a session before compacting it.', '\u8bf7\u5148\u6253\u5f00\u4e00\u4e2a\u4f1a\u8bdd\u518d\u538b\u7f29\u3002'));
    return false;
  }
  showToast(uiText('Compacting older conversation history\u2026', '\u6b63\u5728\u538b\u7f29\u8f83\u65e9\u7684\u4f1a\u8bdd\u5386\u53f2\u2026'));
  try {
    const res = await fetch('/api/compact', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId, instructions: instructions || '' })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'compact: ' + res.status);
    showToast(data.compacted
      ? uiText('History compacted: ', '\u4f1a\u8bdd\u5386\u53f2\u5df2\u538b\u7f29\uff1a') + data.before + ' \u2192 ' + data.after
      : uiText(data.message || 'No compactable history yet.', data.message || '\u6682\u65e0\u53ef\u538b\u7f29\u7684\u5386\u53f2\u3002'));
    return true;
  } catch (error) {
    showToast(uiText('Compaction failed: ', '\u538b\u7f29\u5931\u8d25\uff1a') + error.message);
    return false;
  }
}

async function recordComposerFeedback(text) {
  if (!currentSessionId) {
    showToast(uiText('Open a session before recording feedback.', '\u8bf7\u5148\u6253\u5f00\u4e00\u4e2a\u4f1a\u8bdd\u518d\u8bb0\u5f55\u53cd\u9988\u3002'));
    return false;
  }
  try {
    const res = await fetch('/api/feedback', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId, kind: 'remark', text: String(text || '').trim() })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'feedback: ' + res.status);
    showToast(uiText('Feedback recorded for this session.', '\u5df2\u8bb0\u5f55\u672c\u4f1a\u8bdd\u53cd\u9988\u3002'));
    return true;
  } catch (error) {
    showToast(uiText('Feedback failed: ', '\u53cd\u9988\u8bb0\u5f55\u5931\u8d25\uff1a') + error.message);
    return false;
  }
}

async function createComposerGoal(objective, priority) {
  try {
    const res = await fetch('/api/goals', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ objective: String(objective || '').trim(), priority: priority || 'medium' })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'goal: ' + res.status);
    showToast(uiText('Goal created: ', '\u76ee\u6807\u5df2\u521b\u5efa\uff1a') + (data.title || objective));
    return true;
  } catch (error) {
    showToast(uiText('Goal creation failed: ', '\u76ee\u6807\u521b\u5efa\u5931\u8d25\uff1a') + error.message);
    return false;
  }
}

function normalizedPermissionMode(value) {
  const key = String(value || '').trim().replace(/[\s_-]/g, '').toLowerCase();
  return ({ default: 'default', ask: 'default', acceptedits: 'acceptEdits', plan: 'plan', dontask: 'dontAsk', bypass: 'bypassPermissions', bypasspermissions: 'bypassPermissions' })[key] || '';
}

async function setPermissionMode(value) {
  const mode = normalizedPermissionMode(value);
  if (!mode) {
    showToast(uiText('Unknown permission mode.', '\u672a\u77e5\u7684\u6743\u9650\u6a21\u5f0f\u3002'));
    return false;
  }
  try {
    const res = await fetch('/api/settings', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ changes: [{ key: 'permission.mode', value: mode }] })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'mode: ' + res.status);
    syncApprovalChip(mode);
    const setting = settingByKey('permission.mode');
    if (setting) setting.value = mode;
    showToast(uiText('Permission mode: ', '\u6743\u9650\u6a21\u5f0f\uff1a') + (document.documentElement.lang === 'zh-CN' ? PERMISSION_LABELS_ZH[mode] : PERMISSION_LABELS[mode]));
    return true;
  } catch (error) {
    showToast(uiText('Mode change failed: ', '\u6743\u9650\u6a21\u5f0f\u5207\u6362\u5931\u8d25\uff1a') + error.message);
    return false;
  }
}

async function openPluginSettings() {
  await openSettings();
  const button = document.getElementById('settingsPluginsBtn');
  showSettingsTab('plugins', button);
}

function composerActionDialogMarkup(kind) {
  if (kind === 'goal') {
    return '<div class="composer-action-heading"><span class="composer-action-symbol">' + composerActionIcon('goal') + '</span><div><h2 id="composerActionTitle">' + uiText('Set a goal', '\u8bbe\u7f6e\u76ee\u6807') + '</h2><p id="composerActionDescription">' + uiText('Create a durable objective that METIS can track across sessions.', '\u521b\u5efa\u4e00\u4e2a METIS \u53ef\u4ee5\u8de8\u4f1a\u8bdd\u8ddf\u8e2a\u7684\u6301\u4e45\u76ee\u6807\u3002') + '</p></div></div>' +
      '<div class="composer-goal-list" id="composerGoalList" aria-live="polite">' + uiText('Loading current goals\u2026', '\u6b63\u5728\u52a0\u8f7d\u5f53\u524d\u76ee\u6807\u2026') + '</div>' +
      '<label class="composer-action-field"><span>' + uiText('Objective', '\u76ee\u6807') + '</span><input id="composerActionInput" maxlength="240" placeholder="' + escAttr(uiText('What should METIS keep working toward?', 'METIS \u9700\u8981\u6301\u7eed\u8ffd\u8e2a\u4ec0\u4e48\uff1f')) + '"></label>' +
      '<label class="composer-action-field"><span>' + uiText('Priority', '\u4f18\u5148\u7ea7') + '</span><select id="composerActionPriority"><option value="high">' + uiText('High', '\u9ad8') + '</option><option value="medium" selected>' + uiText('Medium', '\u4e2d') + '</option><option value="low">' + uiText('Low', '\u4f4e') + '</option></select></label>';
  }
  if (kind === 'permission') {
    const modes = ['default', 'acceptEdits', 'plan', 'dontAsk', 'bypassPermissions'];
    const labels = document.documentElement.lang === 'zh-CN' ? PERMISSION_LABELS_ZH : PERMISSION_LABELS;
    const descs = document.documentElement.lang === 'zh-CN' ? PERMISSION_DESCS_ZH : PERMISSION_DESCS;
    return '<div class="composer-action-heading"><span class="composer-action-symbol">' + composerActionIcon('permission') + '</span><div><h2 id="composerActionTitle">' + uiText('Permission mode', '\u6743\u9650\u6a21\u5f0f') + '</h2><p id="composerActionDescription">' + uiText('Choose what METIS may execute without asking.', '\u9009\u62e9 METIS \u54ea\u4e9b\u64cd\u4f5c\u53ef\u4ee5\u4e0d\u8be2\u95ee\u76f4\u63a5\u6267\u884c\u3002') + '</p></div></div><div class="composer-permission-options">' + modes.map(mode =>
      '<label class="composer-permission-option"><input type="radio" name="composerPermission" value="' + mode + '"' + (approvalMode === mode ? ' checked' : '') + '><span><strong>' + escHtml(labels[mode]) + '</strong><small>' + escHtml(descs[mode]) + '</small></span></label>'
    ).join('') + '</div>';
  }
  return '<div class="composer-action-heading"><span class="composer-action-symbol">' + composerActionIcon('feedback') + '</span><div><h2 id="composerActionTitle">' + uiText('Session feedback', '\u4f1a\u8bdd\u53cd\u9988') + '</h2><p id="composerActionDescription">' + uiText('This note is saved to the session log and is not sent to the model.', '\u8fd9\u6761\u5907\u6ce8\u53ea\u4fdd\u5b58\u5230\u4f1a\u8bdd\u65e5\u5fd7\uff0c\u4e0d\u4f1a\u53d1\u9001\u7ed9\u6a21\u578b\u3002') + '</p></div></div>' +
    '<label class="composer-action-field"><span>' + uiText('Feedback', '\u53cd\u9988') + '</span><textarea id="composerActionInput" maxlength="2000" rows="5" placeholder="' + escAttr(uiText('What should be recorded about this session?', '\u9700\u8981\u4e3a\u672c\u4f1a\u8bdd\u8bb0\u5f55\u4ec0\u4e48\uff1f')) + '"></textarea></label>';
}

function openComposerActionDialog(kind, trigger) {
  closeComposerActionDialog(true);
  const overlay = document.createElement('div');
  overlay.className = 'composer-action-overlay';
  overlay.innerHTML = '<form class="composer-action-dialog" role="dialog" aria-modal="true" aria-labelledby="composerActionTitle" aria-describedby="composerActionDescription">' + composerActionDialogMarkup(kind) +
    '<p class="composer-action-error" role="alert" hidden></p><div class="composer-action-buttons"><button type="button" class="composer-action-cancel">' + uiText('Cancel', '\u53d6\u6d88') + '</button><button type="submit" class="composer-action-confirm">' + (kind === 'permission' ? uiText('Apply mode', '\u5e94\u7528\u6a21\u5f0f') : kind === 'goal' ? uiText('Create goal', '\u521b\u5efa\u76ee\u6807') : uiText('Save feedback', '\u4fdd\u5b58\u53cd\u9988')) + '</button></div></form>';
  document.body.appendChild(overlay);
  const fallback = document.getElementById('attachmentBtn');
  composerActionDialog = { kind, trigger: trigger && !trigger.closest('.composer-add-menu') ? trigger : fallback, overlay, pending: false };
  overlay.querySelector('.composer-action-cancel').addEventListener('click', () => closeComposerActionDialog(false));
  overlay.querySelector('form').addEventListener('submit', confirmComposerAction);
  overlay.addEventListener('click', event => { if (event.target === overlay) closeComposerActionDialog(false); });
  overlay.addEventListener('keydown', composerActionDialogKeydown);
  if (kind === 'goal') void loadComposerGoals();
  requestAnimationFrame(() => {
    const focus = overlay.querySelector('#composerActionInput, input[type="radio"]:checked, input[type="radio"], button');
    if (focus) focus.focus();
  });
}

async function loadComposerGoals() {
  const target = composerActionDialog && composerActionDialog.overlay.querySelector('#composerGoalList');
  if (!target) return;
  try {
    const res = await fetch('/api/goals');
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'goals: ' + res.status);
    const goals = Array.isArray(data.goals) ? data.goals.slice(0, 4) : [];
    target.innerHTML = goals.length ? '<div class="composer-goal-list-title">' + uiText('Current goals', '\u5f53\u524d\u76ee\u6807') + '</div>' + goals.map(goal =>
      '<div class="composer-goal-row"><span class="composer-goal-status ' + escAttr(goal.status || 'pending') + '"></span><strong>' + escHtml(goal.title || '') + '</strong><small>' + escHtml(goal.priority || 'medium') + '</small></div>'
    ).join('') : '<span class="composer-goal-empty">' + uiText('No goals yet.', '\u8fd8\u6ca1\u6709\u76ee\u6807\u3002') + '</span>';
  } catch (error) {
    target.textContent = uiText('Unable to load goals: ', '\u65e0\u6cd5\u52a0\u8f7d\u76ee\u6807\uff1a') + error.message;
  }
}

function closeComposerActionDialog(force) {
  if (!composerActionDialog || composerActionDialog.pending && !force) return;
  const state = composerActionDialog;
  composerActionDialog = null;
  state.overlay.remove();
  if (state.trigger && state.trigger.isConnected) state.trigger.focus();
}

function composerActionDialogKeydown(event) {
  if (!composerActionDialog || composerActionDialog.pending) return;
  if (event.key === 'Escape') { event.preventDefault(); closeComposerActionDialog(false); return; }
  if (event.key !== 'Tab') return;
  const focusable = Array.from(composerActionDialog.overlay.querySelectorAll('button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled)'));
  if (!focusable.length) return;
  const first = focusable[0], last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}

async function confirmComposerAction(event) {
  event.preventDefault();
  const state = composerActionDialog;
  if (!state || state.pending) return;
  const input = state.overlay.querySelector('#composerActionInput');
  const value = input ? input.value.trim() : '';
  const error = state.overlay.querySelector('.composer-action-error');
  if (state.kind !== 'permission' && !value) {
    error.hidden = false;
    error.textContent = uiText('Enter a value first.', '\u8bf7\u5148\u8f93\u5165\u5185\u5bb9\u3002');
    input.focus();
    return;
  }
  state.pending = true;
  const buttons = state.overlay.querySelectorAll('button');
  buttons.forEach(button => { button.disabled = true; });
  let ok = false;
  if (state.kind === 'goal') ok = await createComposerGoal(value, state.overlay.querySelector('#composerActionPriority').value);
  else if (state.kind === 'feedback') ok = await recordComposerFeedback(value);
  else {
    const selected = state.overlay.querySelector('input[name="composerPermission"]:checked');
    ok = await setPermissionMode(selected ? selected.value : approvalMode);
  }
  if (ok) {
    closeComposerActionDialog(true);
    return;
  }
  state.pending = false;
  buttons.forEach(button => { button.disabled = false; });
  error.hidden = false;
  error.textContent = uiText('The action could not be completed. Review the message above and try again.', '\u64cd\u4f5c\u672a\u5b8c\u6210\uff0c\u8bf7\u6839\u636e\u63d0\u793a\u540e\u91cd\u8bd5\u3002');
}

function handleKeydown(e) {
  // WebKit can report keyCode 229 while an IME candidate is being committed.
  // Let the input method consume Enter and navigation keys without sending.
  if (e.isComposing || e.keyCode === 229) return;

  const menuOpen = commandMatches.length > 0 && document.getElementById('commandMenu').style.display !== 'none';
  if (menuOpen && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
    e.preventDefault();
    const delta = e.key === 'ArrowDown' ? 1 : -1;
    commandSelection = (commandSelection + delta + commandMatches.length) % commandMatches.length;
    renderCommandMenu();
    return;
  }
  if (menuOpen && e.key === 'Escape') {
    e.preventDefault();
    closeCommandMenu();
    return;
  }
  if (menuOpen && e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
    e.preventDefault();
    chooseComposerCommand(commandMatches[commandSelection].name);
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    let behavior = desktopPreferences.busyEnter || 'queue';
    if (turnRunning && (e.metaKey || e.ctrlKey)) behavior = behavior === 'queue' ? 'send' : 'queue';
    sendMessage(behavior);
  }
  updateSendBtn();
}

function autoResize(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 200) + 'px';
  updateSendBtn();
}

function updateEmptyLayout() {
  const main = document.querySelector('.main');
  if (!main) return;
  const hasContent = document.querySelectorAll('.message').length > 0
    || !!document.querySelector('.call-row')
    || !!document.querySelector('.think-row')
    || !!document.querySelector('.perm-card');
  main.classList.toggle('empty', !hasContent);
}

function updateSendBtn() {
  const val = document.getElementById('inputField').value.trim();
  document.getElementById('sendBtn').disabled = !val && !attachments.length;
}

// --- Model ---
function cycleModel() {
  currentModel = (currentModel + 1) % models.length;
  document.getElementById('modelName').textContent = models[currentModel];
}

// --- Approval ---
let modelList = [];
let modelMenuOpen = false;

async function toggleModelMenu() {
  const menu = document.getElementById('modelMenu');
  if (modelMenuOpen) { menu.style.display = 'none'; modelMenuOpen = false; return; }
  if (!modelList.length) {
    try {
      const res = await fetch('/api/models');
      const d = await res.json();
      if (res.ok) modelList = d.models || [];
    } catch (_) {}
  }
  if (modelList.length < 2) { showToast('No other models configured'); return; }
  menu.innerHTML = modelList.map(m =>
    `<div class="model-option" onclick="switchModel('${escOnclick(m.provider)}','${escOnclick(m.model)}')">${escHtml(m.label)}</div>`).join('');
  menu.style.display = 'block';
  modelMenuOpen = true;
}

async function switchModel(provider, model) {
  document.getElementById('modelMenu').style.display = 'none';
  modelMenuOpen = false;
  try {
    const res = await fetch('/api/models', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ provider: provider, model: model })
    });
    const d = await res.json();
    if (!res.ok) throw new Error(d.error || 'model: ' + res.status);
    const name = document.getElementById('modelName');
    if (name) name.textContent = (d.model || model).split('/').pop();
	await loadEffort();
    showToast('Model: ' + (d.model || model));
  } catch (e) {
    showToast('Model switch failed: ' + e.message);
  }
}

async function toggleApproval() {
  const modes = ['default', 'acceptEdits', 'plan', 'dontAsk', 'bypassPermissions'];
  const next = modes[(modes.indexOf(approvalMode) + 1) % modes.length];
  await setPermissionMode(next);
}

// --- Settings ---
// The settings panel is backed by the same config file the TUI /config
// panel edits: GET /api/settings lists editable entries, POST persists
// validated changes through config.SaveUserSettingsAndLoad.
let settingsCache = null;
let providersCache = null;
let presetsCache = null;
let pluginsCache = null;
let pluginCatalogCache = { ecosystems: [], marketplaces: [], plugins: [], needsSync: false };
let pluginCatalogQuery = '';
let pluginEcosystemFilter = 'all';
const PLUGIN_CATALOG_PAGE_SIZE = 60;
let pluginCatalogLimit = PLUGIN_CATALOG_PAGE_SIZE;
let pluginCatalogSyncAttempted = false;
let pluginCatalogSyncing = false;
let pluginActionDialog = null;
let routingCache = null;

function uiText(en, zh) {
  return document.documentElement.lang === 'zh-CN' ? zh : en;
}

const SETTINGS_ZH = {
  'permission.mode': ['权限模式', '选择工具调用与文件修改何时需要确认'],
  'session.max_iterations': ['最大迭代次数', '单个 turn 允许的最大代理循环次数'],
  'session.auto_compact_threshold': ['自动压缩阈值', '上下文窗口达到该比例时自动压缩'],
  'session.auto_compact_minimum_tokens': ['最小压缩 token', '触发自动压缩前需要的最小 token 数'],
  'loop_detection.disabled': ['禁用循环检测', '关闭重复工具调用与停滞检测'],
  'ui.theme': ['界面主题', '选择 Desktop 的颜色方案'],
  'ui.thinking_display': ['思考展示', '控制推理内容默认展开、隐藏或自动折叠'],
};

function settingTitle(s) {
  return document.documentElement.lang === 'zh-CN' && SETTINGS_ZH[s.key] ? SETTINGS_ZH[s.key][0] : s.label;
}

function settingDescription(s) {
  return document.documentElement.lang === 'zh-CN' && SETTINGS_ZH[s.key] ? SETTINGS_ZH[s.key][1] : s.description;
}
let settingsTab = 'general';
let themeMedia = null;

async function openSettings() {
  document.getElementById('settingsOverlay').classList.add('visible');
  if (!settingsCache) {
    try {
      const res = await fetch('/api/settings');
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'settings: ' + res.status);
      settingsCache = data;
      applyTheme(currentThemeSetting());
    } catch (e) {
      showToast('Failed to load settings: ' + e.message);
    }
  }
  renderSettingsTab();
}

function closeSettings(e) {
  if (!e || e.target === document.getElementById('settingsOverlay')) {
    document.getElementById('settingsOverlay').classList.remove('visible');
  }
}

function showSettingsTab(tab, el) {
  settingsTab = tab;
  document.querySelectorAll('.settings-item').forEach(i => i.classList.remove('active'));
  if (el) el.classList.add('active');
  const content = document.getElementById('settingsContent');
  if (content) content.scrollTop = 0;
  renderSettingsTab();
}

function filterSettings(query) {
  const q = String(query || '').trim().toLowerCase();
  document.querySelectorAll('#settingsContent .settings-section').forEach(section => {
    section.style.display = !q || section.textContent.toLowerCase().includes(q) ? '' : 'none';
  });
}

function settingByKey(key) {
  return (settingsCache && settingsCache.settings || []).find(x => x.key === key);
}

function currentThemeSetting() {
  const t = settingByKey('ui.theme');
  return t ? t.value : 'auto';
}

// Resolve the config value (auto follows the OS) onto the root element;
// the palettes live in style.css as [data-theme=...] variable sets.
function applyTheme(theme) {
  let resolved = theme || 'auto';
  if (resolved === 'auto') {
    if (!themeMedia) {
      themeMedia = window.matchMedia('(prefers-color-scheme: light)');
      themeMedia.addEventListener('change', () => {
        if (currentThemeSetting() === 'auto') applyTheme('auto');
      });
    }
    resolved = themeMedia.matches ? 'light' : 'dark';
  }
  document.documentElement.dataset.theme = resolved;
}

// Boot hook: fetch settings once so the persisted theme applies on page
// load (openSettings reuses the same cache).
async function initTheme() {
  try {
    const res = await fetch('/api/settings');
    const data = await res.json();
    if (res.ok && data.settings) {
      settingsCache = data;
      applyTheme(currentThemeSetting());
      const permission = settingByKey('permission.mode');
      syncApprovalChip(permission ? permission.value : 'default');
    }
  } catch (_) { /* fall back to the default dark palette */ }
}

function renderSettingsTab() {
  const content = document.getElementById('settingsContent');
  if (!settingsCache) {
    content.innerHTML = '<p style="color:var(--text-secondary);margin-top:12px;">' + uiText('Loading settings...', '正在加载设置…') + '</p>';
    return;
  }
  switch (settingsTab) {
    case 'general': content.innerHTML = '<h2>' + uiText('General', '通用') + '</h2>' + renderGeneralTab(); break;
    case 'appearance': content.innerHTML = '<h2>' + uiText('Appearance', '外观') + '</h2>' + renderAppearanceTab(); break;
    case 'providers': content.innerHTML = renderProvidersTab(); loadProviders(); break;
    case 'presets': content.innerHTML = renderPresetsTab(); loadPresets(); break;
    case 'plugins': content.innerHTML = renderPluginsTab(); loadPlugins(); loadPluginCatalog(); break;
    case 'routing': content.innerHTML = renderRoutingTab(); loadRouting(); break;
    case 'config': content.innerHTML = renderConfigTab(); fillConfigFiles(); break;
  }
  const search = document.querySelector('.settings-search');
  if (search && search.value) filterSettings(search.value);
}

function lockBadge(s) {
  return s.lockedReason ? ` <span class="settings-lock">&#128274; ${escHtml(s.lockedReason)}</span>` : '';
}

function renderEnumSetting(s, labels, descs) {
  const cards = s.options.map(opt => {
    const sel = opt === s.value ? ' selected' : '';
    const lock = s.lockedReason ? ' locked' : '';
    return `<button type="button" aria-pressed="${opt === s.value ? 'true' : 'false'}" class="radio-card${sel}${lock}" data-key="${escAttr(s.key)}" data-value="${escAttr(opt)}" onclick="chooseEnum(this)">
      <div class="radio-card-title">${escHtml(labels[opt] || opt)}</div>
      <div class="radio-card-desc">${escHtml(descs[opt] || '')}</div>
    </button>`;
  }).join('');
  return `<div class="settings-section">
    <div class="settings-section-title">${escHtml(settingTitle(s))}${lockBadge(s)}</div>
    <div class="settings-section-desc">${escHtml(settingDescription(s))}</div>
    <div class="radio-cards">${cards}</div>
  </div>`;
}

function renderNumberSetting(s) {
  const disabled = s.lockedReason ? ' disabled' : '';
  return `<div class="settings-section">
    <div class="settings-section-title">${escHtml(settingTitle(s))}${lockBadge(s)}</div>
    <div class="settings-section-desc">${escHtml(settingDescription(s))}${s.restartRequired ? uiText(' - takes effect after restart', ' - 重启后生效') : ''}</div>
    <input type="number" class="settings-number" data-key="${escAttr(s.key)}" value="${escAttr(s.value)}" onchange="saveSetting(this)"${disabled}>
  </div>`;
}

function renderBoolSetting(s) {
  const on = s.value === 'true';
  const lock = s.lockedReason ? ' locked' : '';
  return `<div class="settings-section">
    <div class="settings-section-title">${escHtml(settingTitle(s))}${lockBadge(s)}</div>
    <div class="settings-section-desc">${escHtml(settingDescription(s))}${s.restartRequired ? uiText(' - takes effect after restart', ' - 重启后生效') : ''}</div>
    <button type="button" aria-pressed="${on ? 'true' : 'false'}" class="settings-switch${on ? ' on' : ''}${lock}" data-key="${escAttr(s.key)}" data-value="${on ? 'false' : 'true'}" onclick="toggleSetting(this)" title="${escAttr(s.lockedReason || '')}"${s.lockedReason ? ' disabled' : ''}>
      <span class="settings-switch-knob"></span>
    </button>
  </div>`;
}

const PERMISSION_LABELS = {
  default: 'Default',
  acceptEdits: 'Accept Edits',
  plan: 'Plan Mode',
  dontAsk: "Don't Ask",
  bypassPermissions: 'Bypass Permissions',
};
const PERMISSION_DESCS = {
  default: 'Allow read-only work; ask before changes',
  acceptEdits: 'Auto-accept file edits; ask before other state changes',
  plan: 'Explore and plan first; no changes without approval',
  dontAsk: 'Never prompt: allow pre-approved and read-only work, deny the rest',
  bypassPermissions: 'Auto-approve ordinary tool calls; critical destructive checks remain',
};
const PERMISSION_LABELS_ZH = {
  default: '默认确认', acceptEdits: '自动接受编辑', plan: '计划模式', dontAsk: '不询问', bypassPermissions: '跳过权限检查',
};
const PERMISSION_DESCS_ZH = {
  default: '只读操作直接执行，修改前询问',
  acceptEdits: '自动接受文件编辑，其他状态变更继续询问',
  plan: '先探索和规划，未经确认不修改',
  dontAsk: '从不弹窗：只执行已允许和只读操作，其余直接拒绝',
  bypassPermissions: '普通工具调用自动执行，严重破坏性操作仍会拦截',
};

function syncApprovalChip(mode) {
  const valid = Object.prototype.hasOwnProperty.call(PERMISSION_LABELS, mode) ? mode : 'default';
  approvalMode = valid;
  const labels = document.documentElement.lang === 'zh-CN' ? PERMISSION_LABELS_ZH : PERMISSION_LABELS;
  const descs = document.documentElement.lang === 'zh-CN' ? PERMISSION_DESCS_ZH : PERMISSION_DESCS;
  const label = labels[valid];
  const btn = document.getElementById('approvalBtn');
  const text = document.getElementById('approvalLabel');
  if (text) text.textContent = label;
  if (btn) {
    btn.classList.toggle('active', valid !== 'default');
    btn.setAttribute('aria-label', uiText('Permission mode: ', '权限模式：') + label);
    btn.title = descs[valid] || label;
  }
}
const THEME_LABELS = {
  auto: 'Auto', dark: 'Dark', light: 'Light',
  'dark-daltonized': 'Dark (Colorblind)', nord: 'Nord', 'solarized-dark': 'Solarized Dark',
};
const THEME_DESCS = {
  auto: 'Follow the system light/dark preference',
  dark: 'Default dark palette',
  light: 'Bright palette for daytime',
  'dark-daltonized': 'Cyan/magenta signals for red-green colorblindness',
  nord: 'Arctic cool-toned palette',
  'solarized-dark': 'Warm amber and green tones',
};
const THEME_LABELS_ZH = {
  auto: '自动', dark: '深色', light: '浅色', 'dark-daltonized': '深色（色觉优化）', nord: 'Nord', 'solarized-dark': 'Solarized 深色',
};
const THEME_DESCS_ZH = {
  auto: '跟随系统深色/浅色设置', dark: '默认深色配色', light: '适合白天的明亮配色',
  'dark-daltonized': '使用青色/品红信号优化红绿色觉', nord: '冰原风冷色调', 'solarized-dark': '温暖的琥珀与绿色调',
};

function renderGeneralTab() {
  const parts = [];
  parts.push(renderBusyEnterPreference());
  const perm = settingByKey('permission.mode');
  if (perm) parts.push(renderEnumSetting(perm,
    document.documentElement.lang === 'zh-CN' ? PERMISSION_LABELS_ZH : PERMISSION_LABELS,
    document.documentElement.lang === 'zh-CN' ? PERMISSION_DESCS_ZH : PERMISSION_DESCS));
  for (const key of ['session.max_iterations', 'session.auto_compact_threshold', 'session.auto_compact_minimum_tokens']) {
    const st = settingByKey(key);
    if (st) parts.push(renderNumberSetting(st));
  }
  const loop = settingByKey('loop_detection.disabled');
  if (loop) parts.push(renderBoolSetting(loop));
  return parts.join('');
}

function renderBusyEnterPreference() {
  const value = desktopPreferences.busyEnter || 'queue';
  const choices = document.documentElement.lang === 'zh-CN' ? [
    { value: 'queue', title: '排队', desc: 'Enter 加入后续队列；Cmd/Ctrl+Enter 发送到当前 turn' },
    { value: 'send', title: '立即发送', desc: 'Enter 引导当前 turn；Cmd/Ctrl+Enter 加入后续队列' },
  ] : [
    { value: 'queue', title: 'Queue', desc: 'Enter queues a follow-up; Cmd/Ctrl+Enter sends it into the current turn' },
    { value: 'send', title: 'Send now', desc: 'Enter steers the current turn; Cmd/Ctrl+Enter queues a follow-up' },
  ];
  return `<div class="settings-section">
    <div class="settings-section-title">${uiText('Busy Enter behavior', '运行中 Enter 行为')}</div>
    <div class="settings-section-desc">${uiText('Choose what Enter does while the agent is working. Image follow-ups always queue.', '选择代理运行时 Enter 的行为。带图片的后续消息始终排队。')}</div>
    <div class="radio-cards">${choices.map(c => `<button type="button" aria-pressed="${value === c.value ? 'true' : 'false'}" class="radio-card desktop-pref${value === c.value ? ' selected' : ''}" onclick="chooseBusyEnter('${c.value}')">
      <div class="radio-card-title">${c.title}</div><div class="radio-card-desc">${c.desc}</div>
    </button>`).join('')}</div>
  </div>`;
}

async function chooseBusyEnter(value) {
  if (await saveDesktopPreference('busyEnter', value)) {
    renderSettingsTab();
    showToast('Busy Enter: ' + (value === 'queue' ? 'Queue' : 'Send now'));
  }
}

function renderAppearanceTab() {
  const theme = settingByKey('ui.theme');
  if (!theme) return '<p style="color:var(--text-secondary);margin-top:12px;">Theme setting unavailable.</p>';
  const thinking = settingByKey('ui.thinking_display');
  let html = renderEnumSetting(theme,
    document.documentElement.lang === 'zh-CN' ? THEME_LABELS_ZH : THEME_LABELS,
    document.documentElement.lang === 'zh-CN' ? THEME_DESCS_ZH : THEME_DESCS);
  if (thinking) html += renderEnumSetting(thinking,
    { show: 'Show', hide: 'Hide', auto: 'Auto' },
    { show: 'Expand provider reasoning by default', hide: 'Hide provider reasoning rows', auto: 'Use compact live and history previews' });
  return html + renderLanguagePreference();
}

function renderLanguagePreference() {
  const value = desktopPreferences.language || 'zh-CN';
  const choices = [
    { value: 'auto', title: 'Auto', desc: 'Follow the operating system language' },
    { value: 'en', title: 'English', desc: 'Use English for the Desktop shell' },
    { value: 'zh-CN', title: '简体中文', desc: '桌面主界面使用简体中文' },
  ];
  return `<div class="settings-section"><div class="settings-section-title">${uiText('Interface language', '界面语言')}</div>
    <div class="settings-section-desc">${uiText('Applies immediately to Desktop navigation, settings, and composer.', '立即应用到 Desktop 导航、设置与输入区。')}</div>
    <div class="radio-cards">${choices.map(c => `<button type="button" aria-pressed="${value === c.value ? 'true' : 'false'}" class="radio-card desktop-pref${value === c.value ? ' selected' : ''}" onclick="chooseLanguage('${c.value}')"><div class="radio-card-title">${c.title}</div><div class="radio-card-desc">${c.desc}</div></button>`).join('')}</div></div>`;
}

async function chooseLanguage(value) {
  if (await saveDesktopPreference('language', value)) {
    applyLanguage(value);
    renderSettingsTab();
    showToast(value === 'zh-CN' ? '界面语言：简体中文' : 'Interface language: ' + (value === 'en' ? 'English' : 'Auto'));
  }
}

function renderProvidersTab() {
  return `<h2>${uiText('Model Providers', '模型提供商')}</h2>
    <div class="settings-section">
      <div class="settings-section-title">${uiText('Configured providers', '已配置的提供商')}</div>
      <div class="settings-section-desc">${uiText('Credentials are stored separately in auth.json and are never displayed. Validate checks local configuration only and sends no model request.', '凭据单独保存在 auth.json，绝不回显。“验证”只检查本地配置，不发送模型请求。')}</div>
      <div id="providerList" class="provider-list"><div class="provider-empty">${uiText('Loading providers...', '正在加载提供商…')}</div></div>
      <button type="button" class="provider-add-btn" onclick="showProviderForm()">+ ${uiText('Add custom provider', '添加自定义提供商')}</button>
    </div>
    <form id="providerForm" class="provider-form" style="display:none" onsubmit="saveCustomProvider(event)">
      <div class="settings-section-title" id="providerFormTitle">${uiText('Add custom provider', '添加自定义提供商')}</div>
      <label>${uiText('Provider ID', '提供商 ID')}<input id="providerId" required pattern="[a-z0-9][a-z0-9_-]*" placeholder="my-provider" autocomplete="username"></label>
      <label>${uiText('Transport', '传输协议')}<select id="providerTransport" required>
        <option value="openai_chat">OpenAI Chat</option>
        <option value="anthropic_messages">Anthropic Messages</option>
        <option value="gemini_native">Gemini Native</option>
      </select></label>
      <label>Base URL<input id="providerBaseUrl" required type="url" placeholder="https://api.example.com/v1" autocomplete="url"></label>
      <label>${uiText('Model', '模型')}<input id="providerModel" required placeholder="model-id" autocomplete="off"></label>
      <label>${uiText('API key (optional)', 'API 密钥（可选）')}<input id="providerApiKey" type="password" autocomplete="new-password" placeholder="${uiText('Leave blank to keep the existing key', '留空以保留现有密钥')}"></label>
      <label class="provider-clear"><input id="providerClearCredential" type="checkbox"> ${uiText('Remove the saved credential', '移除已保存的凭据')}</label>
      <div class="provider-form-actions"><button type="button" onclick="hideProviderForm()">${uiText('Cancel', '取消')}</button><button type="submit" class="primary">${uiText('Save provider', '保存提供商')}</button></div>
    </form>`;
}

async function loadProviders() {
  try {
    const res = await fetch('/api/providers');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'providers: ' + res.status);
    providersCache = Array.isArray(data.providers) ? data.providers : [];
    paintProviders();
  } catch (e) {
    const list = document.getElementById('providerList');
    if (list) list.innerHTML = '<div class="provider-empty">Unable to load providers: ' + escHtml(e.message) + '</div>';
  }
}

function paintProviders() {
  const list = document.getElementById('providerList');
  if (!list) return;
  const providers = providersCache || [];
  list.innerHTML = providers.map(p => `<div class="provider-card">
    <div class="provider-main"><div class="provider-title">${escHtml(p.id)}${p.default ? ' <span class="provider-badge">' + uiText('Default', '默认') + '</span>' : ''}</div>
      <div class="provider-meta">${escHtml(p.transport || uiText('unknown', '未知'))} · ${escHtml(p.model || uiText('model not set', '未设置模型'))}</div>
      <div class="provider-url">${escHtml(p.baseUrl || uiText('Default endpoint', '默认端点'))}</div>
    </div>
    <span class="provider-credential ${p.credentialConfigured ? 'ready' : ''}">${p.credentialConfigured ? uiText('Credential set', '已配置凭据') : uiText('No credential', '无凭据')}</span>
    <div class="provider-actions">
      <button type="button" onclick="setDefaultProvider('${escOnclick(p.id)}')"${p.default ? ' disabled' : ''}>${uiText('Set default', '设为默认')}</button>
      <button type="button" onclick="validateProvider('${escOnclick(p.id)}')">${uiText('Validate', '验证')}</button>
	  <button type="button" onclick="probeProvider('${escOnclick(p.id)}')">${uiText('Test connection', '测试连接')}</button>
      ${p.custom ? `<button type="button" onclick="editProvider('${escOnclick(p.id)}')">${uiText('Edit', '编辑')}</button><button type="button" class="danger" onclick="deleteProvider('${escOnclick(p.id)}')"${p.default ? ' disabled title="' + uiText('Select another default first', '请先选择其他默认项') + '"' : ''}>${uiText('Delete', '删除')}</button>` : ''}
    </div>
  </div>`).join('') || '<div class="provider-empty">' + uiText('No providers configured', '未配置提供商') + '</div>';
  const search = document.querySelector('.settings-search');
  if (search && search.value) filterSettings(search.value);
}

function showProviderForm(provider) {
  const form = document.getElementById('providerForm');
  if (!form) return;
  form.style.display = 'grid';
  document.getElementById('providerFormTitle').textContent = provider ? uiText('Edit custom provider', '编辑自定义提供商') : uiText('Add custom provider', '添加自定义提供商');
  document.getElementById('providerId').value = provider ? provider.id : '';
  document.getElementById('providerId').readOnly = !!provider;
  document.getElementById('providerTransport').value = provider ? provider.transport : 'openai_chat';
  document.getElementById('providerBaseUrl').value = provider ? provider.baseUrl : '';
  document.getElementById('providerModel').value = provider ? provider.model : '';
  document.getElementById('providerApiKey').value = '';
  document.getElementById('providerClearCredential').checked = false;
  document.getElementById('providerId').focus();
}

function hideProviderForm() {
  const form = document.getElementById('providerForm');
  if (form) form.style.display = 'none';
}

function editProvider(id) {
  const provider = (providersCache || []).find(p => p.id === id && p.custom);
  if (provider) showProviderForm(provider);
}

async function saveCustomProvider(e) {
  e.preventDefault();
  const payload = {
    id: document.getElementById('providerId').value.trim(),
    transport: document.getElementById('providerTransport').value,
    baseUrl: document.getElementById('providerBaseUrl').value.trim(),
    model: document.getElementById('providerModel').value.trim(),
    apiKey: document.getElementById('providerApiKey').value,
    clearCredential: document.getElementById('providerClearCredential').checked,
  };
  try {
    const res = await fetch('/api/providers', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'save provider: ' + res.status);
    hideProviderForm();
    await loadProviders();
    showToast('Provider saved. It is now the default for the next launch.');
  } catch (err) {
    showToast('Save provider failed: ' + err.message);
  }
}

async function setDefaultProvider(id) {
  try {
    const res = await fetch('/api/providers/default', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'set default: ' + res.status);
    await loadProviders();
    showToast('Default provider saved for the next launch');
  } catch (e) {
    showToast('Set default failed: ' + e.message);
  }
}

async function validateProvider(id) {
  try {
    const res = await fetch('/api/providers/validate', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'validate: ' + res.status);
    showToast('Provider configuration is valid; no model request was sent');
  } catch (e) {
    showToast('Provider validation failed: ' + e.message);
  }
}

async function probeProvider(id) {
  if (!confirm('Test the metadata connection for "' + id + '"? This sends its credential to the configured provider endpoint, but sends no prompt and uses no model tokens.')) return;
  try {
    const res = await fetch('/api/providers/probe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id, confirm: true }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'connection probe: ' + res.status);
    showToast('Provider metadata endpoint is reachable; no model request was sent');
  } catch (e) {
    showToast('Connection test failed: ' + e.message);
  }
}

async function deleteProvider(id) {
  if (!confirm('Delete custom provider "' + id + '" and its saved credential?')) return;
  try {
    const res = await fetch('/api/providers', { method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'delete provider: ' + res.status);
    await loadProviders();
    showToast('Custom provider and its saved credential were deleted');
  } catch (e) {
    showToast('Delete provider failed: ' + e.message);
  }
}

function renderPresetsTab() {
  return `<h2>${uiText('Agent Presets', '代理预设')}</h2>
    <div class="settings-section">
      <div class="settings-section-title">${uiText('Default preset', '默认预设')}</div>
      <div class="settings-section-desc">${uiText('A preset is applied atomically when Desktop restarts, including its system prompt, model defaults, permission mode, effort, and tool filters.', 'Desktop 重启时会原子应用预设，包括系统提示词、默认模型、权限模式、推理强度和工具过滤。')}</div>
      <div id="presetList" class="preset-list"><div class="provider-empty">${uiText('Loading presets...', '正在加载预设…')}</div></div>
      <button type="button" class="provider-add-btn" onclick="openPresetDirectory()">${uiText('Open preset directory', '打开预设目录')}</button>
    </div>`;
}

async function loadPresets() {
  try {
    const res = await fetch('/api/presets');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'presets: ' + res.status);
    presetsCache = Array.isArray(data.presets) ? data.presets : [];
    paintPresets();
  } catch (e) {
    const list = document.getElementById('presetList');
    if (list) list.innerHTML = '<div class="provider-empty">' + uiText('Unable to load presets: ', '无法加载预设：') + escHtml(e.message) + '</div>';
  }
}

function paintPresets() {
  const list = document.getElementById('presetList');
  if (!list) return;
  list.innerHTML = (presetsCache || []).map(p => {
    const facts = [p.source, p.model, p.permissionMode, p.effort ? uiText('effort ', '推理强度 ') + p.effort : '', p.maxTurns ? p.maxTurns + uiText(' turns', ' 轮') : ''].filter(Boolean).join(' · ');
    const tools = Array.isArray(p.tools) && p.tools.length ? '<div class="preset-tools">' + uiText('Tools: ', '工具：') + escHtml(p.tools.join(', ')) + '</div>' : '';
    return `<div class="provider-card preset-card">
      <div class="provider-main"><div class="provider-title">${escHtml(p.name || p.id)}${p.default ? ' <span class="provider-badge">' + uiText('Default', '默认') + '</span>' : ''}</div>
        <div class="provider-meta">${escHtml(facts || uiText('inherits runtime defaults', '继承运行时默认值'))}</div>
        <div class="preset-description">${escHtml(p.description || p.promptPreview || uiText('No description', '无描述'))}</div>${tools}
      </div>
      <div class="provider-actions">
        <button type="button" onclick="setDefaultPreset('${escOnclick(p.id)}')"${p.default ? ' disabled' : ''}>${uiText('Set default', '设为默认')}</button>
        ${p.id !== 'standard' ? `<button type="button" onclick="copyPreset('${escOnclick(p.id)}')">${uiText('Copy', '复制')}</button>` : ''}
        ${p.custom ? `<button type="button" class="danger" onclick="deletePreset('${escOnclick(p.id)}')"${p.default ? ' disabled title="' + uiText('Select another default first', '请先选择其他默认项') + '"' : ''}>${uiText('Delete', '删除')}</button>` : ''}
      </div>
    </div>`;
  }).join('') || '<div class="provider-empty">' + uiText('No presets found', '未找到预设') + '</div>';
}

async function setDefaultPreset(id) {
  try {
    const res = await fetch('/api/presets/default', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'default preset: ' + res.status);
    desktopPreferences.defaultPreset = id;
    await loadPresets();
    showToast('Default preset saved. Restart Desktop to apply it.');
  } catch (e) {
    showToast('Default preset failed: ' + e.message);
  }
}

async function copyPreset(source) {
  const suggested = source + '-copy';
  const target = prompt('Name for the custom preset', suggested);
  if (!target || !target.trim()) return;
  try {
    const res = await fetch('/api/presets', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ source, target: target.trim() }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'copy preset: ' + res.status);
    await loadPresets();
    showToast('Preset copied to ' + data.path);
  } catch (e) {
    showToast('Copy preset failed: ' + e.message);
  }
}

async function deletePreset(id) {
  if (!confirm('Remove custom preset "' + id + '"? It will be moved to the preset trash folder.')) return;
  try {
    const res = await fetch('/api/presets', { method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'delete preset: ' + res.status);
    await loadPresets();
    showToast('Preset moved to ' + data.recoverableAt);
  } catch (e) {
    showToast('Delete preset failed: ' + e.message);
  }
}

async function openPresetDirectory() {
  try {
    const res = await fetch('/api/presets/open', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'open directory: ' + res.status);
    showToast('Opened ' + data.path);
  } catch (e) {
    showToast('Open preset directory failed: ' + e.message);
  }
}

function renderPluginsTab() {
  return `<h2 id="pluginsSettingsTitle">${uiText('Plugins', '插件')}</h2>
    <div class="plugin-settings" aria-labelledby="pluginsSettingsTitle">
      <div class="plugin-intro">${uiText('METIS connects plugin ecosystems through explicit compatibility bridges. Package formats and runtime components are inspected separately from catalog sources, so a foreign runtime is never presented as a native plugin.', 'METIS 通过明确的兼容桥接接入不同插件生态。包格式、运行时组件与内容来源会分开检查，不再把外部运行时伪装成原生插件。')}</div>
      <div class="plugin-tabs" role="tablist" aria-label="${uiText('Plugin views', '插件视图')}">
        <button type="button" class="plugin-tab active" id="pluginMarketplaceTab" role="tab" aria-selected="true" aria-controls="pluginMarketplacePanel" onclick="switchPluginView('marketplace', this)" onkeydown="pluginTabKeydown(event)">${uiText('Marketplace', '插件商场')}</button>
        <button type="button" class="plugin-tab" id="pluginInstalledTab" role="tab" aria-selected="false" aria-controls="pluginInstalledPanel" tabindex="-1" onclick="switchPluginView('installed', this)" onkeydown="pluginTabKeydown(event)">${uiText('Installed', '已安装')} <span class="plugin-count" id="installedPluginCount">0</span></button>
      </div>
      <section class="plugin-panel" id="pluginMarketplacePanel" role="tabpanel" aria-labelledby="pluginMarketplaceTab">
        <div class="plugin-ecosystem-heading"><div><h3>${uiText('Ecosystem compatibility', '生态兼容层')}</h3><p>${uiText('Choose an ecosystem to inspect what METIS can run, translate, or must leave in its original runtime.', '选择一个生态，查看 METIS 可以原生运行、转换，或必须留在原运行时中的组件。')}</p></div></div>
        <div id="pluginEcosystemGrid" class="plugin-ecosystem-grid" aria-live="polite"></div>
        <div class="plugin-toolbar">
          <label class="plugin-search">
            <span aria-hidden="true">&#8981;</span>
            <span class="visually-hidden">${uiText('Search marketplace plugins', '搜索商场插件')}</span>
            <input type="search" name="plugin-marketplace-search" autocomplete="off" aria-label="${uiText('Search marketplace plugins', '搜索商场插件')}" placeholder="${uiText('Search plugins…', '搜索插件…')}" oninput="filterPluginCatalog(this.value)">
          </label>
          <label class="visually-hidden" for="pluginEcosystemSelect">${uiText('Plugin ecosystem', '插件生态')}</label>
          <select id="pluginEcosystemSelect" class="plugin-market-select" name="plugin-ecosystem" aria-label="${uiText('Plugin ecosystem', '插件生态')}" onchange="choosePluginEcosystem(this.value)">
            <option value="all">${uiText('All ecosystems', '全部生态')}</option>
          </select>
          <button type="button" class="plugin-refresh" id="pluginRefreshBtn" onclick="refreshPluginCatalog()" aria-label="${uiText('Sync marketplace catalogs', '同步插件商场目录')}"><span aria-hidden="true">&#8635;</span> ${uiText('Sync', '同步')}</button>
        </div>
        <details class="plugin-source-details"><summary>${uiText('Catalog sources', '内容来源')}</summary><div id="pluginMarketplaceSources" class="plugin-sources" aria-label="${uiText('Registered catalog sources', '已注册内容来源')}"></div></details>
        <div id="pluginCatalogStatus" class="plugin-status" aria-live="polite"></div>
        <div class="plugin-catalog-heading"><h3>${uiText('Available plugins', '可用插件')}</h3><span id="pluginCatalogCount">0</span></div>
        <div id="pluginCatalogList" class="plugin-grid" aria-busy="true"><div class="plugin-empty">${uiText('Loading marketplace…', '正在加载插件商场…')}</div></div>
      </section>
      <section class="plugin-panel" id="pluginInstalledPanel" role="tabpanel" aria-labelledby="pluginInstalledTab" hidden>
        <div class="plugin-installed-head">
          <div><h3>${uiText('Installed extensions', '已安装的扩展')}</h3><p>${uiText('Install and removal changes take effect after Desktop restarts.', '安装和移除操作会在 Desktop 重启后生效。')}</p></div>
          <button type="button" class="plugin-refresh" onclick="loadPlugins()"><span aria-hidden="true">&#8635;</span> ${uiText('Refresh', '刷新')}</button>
        </div>
        <div id="pluginList" class="plugin-grid" aria-live="polite" aria-busy="true"><div class="plugin-empty">${uiText('Loading installed plugins…', '正在加载已安装插件…')}</div></div>
      </section>
    </div>`;
}

async function loadPlugins() {
  try {
    const res = await fetch('/api/plugins');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'plugins: ' + res.status);
    pluginsCache = Array.isArray(data.plugins) ? data.plugins : [];
    paintPlugins();
  } catch (e) {
    const list = document.getElementById('pluginList');
    if (list) {
      list.setAttribute('aria-busy', 'false');
      list.innerHTML = '<div class="plugin-empty error" role="alert">' + uiText('Unable to load installed plugins. ', '无法加载已安装插件。') + '<button type="button" onclick="loadPlugins()">' + uiText('Retry', '重试') + '</button></div>';
    }
  }
}

function paintPlugins() {
  const list = document.getElementById('pluginList');
  if (!list) return;
  list.setAttribute('aria-busy', 'false');
  const count = document.getElementById('installedPluginCount');
  if (count) count.textContent = String((pluginsCache || []).length);
  list.innerHTML = (pluginsCache || []).map(p => `<article class="plugin-card installed-card">
    <div class="plugin-card-top"><div class="plugin-title-wrap"><h4 title="${escAttr(p.name || p.id)}">${escHtml(p.name || p.id)}</h4>${p.version ? '<span class="plugin-version">' + escHtml(p.version) + '</span>' : ''}</div>
      <span class="plugin-state ${p.loaded ? 'ready' : (p.error ? 'error' : 'pending')}">${p.loaded ? uiText('Loaded', '已加载') : (p.error ? uiText('Invalid', '无效') : uiText('Restart required', '需要重启'))}</span></div>
    <p class="plugin-description">${escHtml(p.description || p.error || uiText('No description provided.', '暂无插件说明。'))}</p>
    <div class="plugin-meta">${escHtml((p.capabilities || []).join(' · ') || uiText('Manifest only', '仅清单'))}</div>
    <div class="plugin-card-footer"><code title="${escAttr(p.source || '')}">${escHtml(p.source || '')}</code><button type="button" class="plugin-remove" onclick="removePlugin('${escOnclick(p.id)}', this)">${uiText('Remove', '移除')}</button></div>
  </article>`).join('') || '<div class="plugin-empty">' + uiText('No plugins installed yet. Browse the marketplace to add one.', '尚未安装插件，可前往插件商场选择安装。') + '</div>';
}

function switchPluginView(view, button) {
  const marketplace = view === 'marketplace';
  document.querySelectorAll('.plugin-tab').forEach(tab => {
    const selected = tab === button;
    tab.classList.toggle('active', selected);
    tab.setAttribute('aria-selected', selected ? 'true' : 'false');
    tab.tabIndex = selected ? 0 : -1;
  });
  const marketPanel = document.getElementById('pluginMarketplacePanel');
  const installedPanel = document.getElementById('pluginInstalledPanel');
  if (marketPanel) marketPanel.hidden = !marketplace;
  if (installedPanel) installedPanel.hidden = marketplace;
}

function pluginTabKeydown(event) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
  const tabs = Array.from(document.querySelectorAll('.plugin-tab'));
  if (!tabs.length) return;
  event.preventDefault();
  let index = tabs.indexOf(event.currentTarget);
  if (event.key === 'Home') index = 0;
  else if (event.key === 'End') index = tabs.length - 1;
  else index = (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
  const next = tabs[index];
  next.focus();
  switchPluginView(next.id === 'pluginMarketplaceTab' ? 'marketplace' : 'installed', next);
}

async function loadPluginCatalog() {
  const list = document.getElementById('pluginCatalogList');
  if (list) list.setAttribute('aria-busy', 'true');
  try {
    const res = await fetch('/api/plugins/catalog');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'catalog: ' + res.status);
    pluginCatalogCache = data;
    paintPluginCatalog();
    if (pluginCatalogCache.needsSync && !pluginCatalogSyncAttempted) {
      pluginCatalogSyncAttempted = true;
      await refreshPluginCatalog({ all: true, automatic: true });
    }
  } catch (e) {
    if (list) {
      list.setAttribute('aria-busy', 'false');
      list.innerHTML = '<div class="plugin-empty error" role="alert">' + uiText('Unable to load the marketplace. ', '无法加载插件商场。') + '<button type="button" onclick="loadPluginCatalog()">' + uiText('Retry', '重试') + '</button></div>';
    }
  }
}

function filterPluginCatalog(value) {
  pluginCatalogQuery = String(value || '').trim().toLocaleLowerCase();
  pluginCatalogLimit = PLUGIN_CATALOG_PAGE_SIZE;
  paintPluginCatalog();
}

function choosePluginEcosystem(value) {
  pluginEcosystemFilter = value || 'all';
  pluginCatalogLimit = PLUGIN_CATALOG_PAGE_SIZE;
  paintPluginCatalog();
}

function pluginMarketplaceMark(market) {
  const ecosystem = String(market && market.ecosystem || '').toLocaleLowerCase();
  const label = ecosystem === 'codex' ? 'O' : (ecosystem === 'claude' ? 'A' : String(market && (market.displayName || market.name) || '?').charAt(0).toLocaleUpperCase());
  return `<span class="plugin-ecosystem-mark ${escAttr(ecosystem || 'other')}" aria-hidden="true">${escHtml(label)}</span>`;
}

function pluginEcosystemOptions(ecosystems) {
  return ecosystems.map(ecosystem => `<option value="${escAttr(ecosystem.id)}">${escHtml(ecosystem.displayName || ecosystem.id)}${Number(ecosystem.packageCount || 0) ? ' · ' + Number(ecosystem.packageCount).toLocaleString() : ''}</option>`).join('');
}

function pluginEcosystemClass(id) {
  if (id === 'deepseek-harness') return 'deepseek';
  return id || 'other';
}

function pluginEcosystemLetter(id) {
  if (id === 'codex') return 'O';
  if (id === 'deepseek-harness') return 'D';
  if (id === 'metis') return 'M';
  if (id === 'claude') return 'A';
  return '?';
}

function renderPluginEcosystems(ecosystems) {
  const grid = document.getElementById('pluginEcosystemGrid');
  if (!grid) return;
  grid.innerHTML = ecosystems.map(ecosystem => {
    const active = pluginEcosystemFilter === ecosystem.id;
    const components = (ecosystem.components || []).map(component => `<span class="plugin-component ${escAttr(component.support || 'external')}" title="${escAttr(component.detail || '')}"><strong>${escHtml(component.kind || '')}</strong><small>${escHtml(component.support || '')}</small></span>`).join('');
    return `<button type="button" class="plugin-ecosystem-card${active ? ' active' : ''}" onclick="choosePluginEcosystem('${escOnclick(ecosystem.id)}')" aria-pressed="${active ? 'true' : 'false'}">
      <span class="plugin-ecosystem-mark ${escAttr(pluginEcosystemClass(ecosystem.id))}" aria-hidden="true">${escHtml(pluginEcosystemLetter(ecosystem.id))}</span>
      <span class="plugin-ecosystem-copy"><span class="plugin-ecosystem-title"><strong>${escHtml(ecosystem.displayName || ecosystem.id)}</strong><small class="${escAttr(ecosystem.status || '')}">${escHtml(ecosystem.status || '')}</small></span><span>${escHtml(ecosystem.description || '')}</span><span class="plugin-component-row">${components}</span></span>
    </button>`;
  }).join('');
}

function pluginCardIcon(plugin) {
  const displayName = plugin.displayName || plugin.name || '?';
  const fallback = escHtml(displayName.charAt(0).toLocaleUpperCase());
  const style = plugin.brandColor ? ` style="--plugin-brand:${escAttr(plugin.brandColor)}"` : '';
  return `<span class="plugin-card-icon"${style}>${plugin.icon ? `<img src="${escAttr(plugin.icon)}" alt="" loading="lazy" onerror="this.remove()">` : ''}<span aria-hidden="true">${fallback}</span></span>`;
}

function pluginCompatibilityLabel(plugin) {
  if (plugin.compatibility === 'native') return uiText('METIS native', 'METIS 原生');
  if (plugin.compatibility === 'skills') return uiText('Skills compatible', 'Skills 兼容');
  if (plugin.compatibility === 'translated') return uiText('Runtime translated', '运行时已转换');
  if (plugin.compatibility === 'partial') return uiText('Portable parts only', '仅导入可移植部分');
  if (plugin.compatibility === 'remote') return uiText('Checked on install', '安装时检查');
  if (plugin.compatibility === 'external') return uiText('Original runtime', '保留在原运行时');
  return uiText('Runtime required', '需要原运行时');
}

function pluginSearchText(plugin) {
  let text = [plugin.name, plugin.displayName, plugin.packageName, plugin.description, plugin.developer, plugin.category, plugin.marketplace, plugin.ecosystem, (plugin.keywords || []).join(' '), (plugin.capabilities || []).join(' '), (plugin.skills || []).join(' ')].join(' ').toLocaleLowerCase();
  if (/(ppt|pptx|powerpoint|presentation|slide|deck)/.test(text)) text += ' ppt pptx powerpoint presentation slides deck 幻灯片 演示文稿';
  return text;
}

function pluginSearchScore(plugin, query) {
  if (!query) return 0;
  const name = String(plugin.name || '').toLocaleLowerCase();
  const displayName = String(plugin.displayName || '').toLocaleLowerCase();
  const description = String(plugin.description || '').toLocaleLowerCase();
  const keywords = (plugin.keywords || []).join(' ').toLocaleLowerCase();
  const skills = (plugin.skills || []).join(' ').toLocaleLowerCase();
  let score = name === query ? 600 : (name.startsWith(query) ? 500 : (name.includes(query) ? 420 : 0));
  if (displayName === query) score += 500;
  else if (displayName.includes(query)) score += 360;
  if (keywords.split(/\s+/).includes(query)) score += 320;
  else if (keywords.includes(query)) score += 260;
  if (skills.includes(query)) score += 180;
  if (description.includes(query)) score += 140;
  if (/^(ppt|pptx|powerpoint|presentation|presentations|slide|slides|deck|decks|幻灯片|演示文稿)$/.test(query)) {
    const officeTerms = /(ppt|pptx|powerpoint|presentation|slide|deck)/;
    if (officeTerms.test(name)) score += 520;
    if (officeTerms.test(displayName)) score += 400;
    if (officeTerms.test(keywords)) score += 300;
    if (officeTerms.test(description)) score += 160;
    if (officeTerms.test(skills)) score += 120;
  }
  return score;
}

function paintPluginCatalog() {
  const catalog = pluginCatalogCache || { ecosystems: [], marketplaces: [], plugins: [] };
  const ecosystems = Array.isArray(catalog.ecosystems) ? catalog.ecosystems : [];
  const markets = Array.isArray(catalog.marketplaces) ? catalog.marketplaces : [];
  const plugins = Array.isArray(catalog.plugins) ? catalog.plugins : [];
  const select = document.getElementById('pluginEcosystemSelect');
  if (select) {
    const current = pluginEcosystemFilter;
    select.innerHTML = '<option value="all">' + uiText('All ecosystems', '全部生态') + '</option>' + pluginEcosystemOptions(ecosystems);
    select.value = ecosystems.some(item => item.id === current) ? current : 'all';
    pluginEcosystemFilter = select.value;
  }
  renderPluginEcosystems(ecosystems);
  const sources = document.getElementById('pluginMarketplaceSources');
  if (sources) sources.innerHTML = markets.map(m => `<span class="plugin-source" title="${escAttr(m.description || m.name)}">${pluginMarketplaceMark(m)}<span>${escHtml(m.displayName || m.name)}</span><small>${m.synced ? Number(m.pluginCount || 0).toLocaleString() : (pluginCatalogSyncing ? uiText('Syncing…', '同步中…') : uiText('Not synced', '未同步'))}</small></span>`).join('');
  const filtered = plugins.filter(p => {
    if (pluginEcosystemFilter !== 'all' && p.ecosystem !== pluginEcosystemFilter) return false;
    if (!pluginCatalogQuery) return true;
    return pluginSearchText(p).includes(pluginCatalogQuery);
  }).sort((a, b) => {
    if (pluginCatalogQuery) {
      const relevance = pluginSearchScore(b, pluginCatalogQuery) - pluginSearchScore(a, pluginCatalogQuery);
      if (relevance) return relevance;
    }
    if (Boolean(a.installable) !== Boolean(b.installable)) return a.installable ? -1 : 1;
    return String(a.name || '').localeCompare(String(b.name || '')) || String(a.marketplace || '').localeCompare(String(b.marketplace || ''));
  });
  const visible = filtered.slice(0, pluginCatalogLimit);
  const installableCount = filtered.filter(p => p.installable && !p.installed).length;
  const count = document.getElementById('pluginCatalogCount');
  if (count) count.textContent = (visible.length < filtered.length
    ? visible.length.toLocaleString() + ' / ' + filtered.length.toLocaleString()
    : filtered.length.toLocaleString()) + (installableCount ? ' · ' + installableCount.toLocaleString() + ' ' + uiText('installable', '可安装') : '');
  const list = document.getElementById('pluginCatalogList');
  if (!list) return;
  list.setAttribute('aria-busy', 'false');
  if (!filtered.length) {
    const unsynced = markets.some(m => !m.synced);
    list.innerHTML = `<div class="plugin-empty">${pluginCatalogQuery ? uiText('No plugins match this search.', '没有与搜索条件匹配的插件。') : (unsynced ? uiText('Sync marketplace catalogs to load available plugins.', '同步商场目录后即可加载可用插件。') : uiText('No plugins are published by this marketplace.', '此商场暂无可用插件。'))}</div>`;
    return;
  }
  const cards = visible.map(p => {
    const skillCount = Array.isArray(p.skills) ? p.skills.length : 0;
    const disabled = p.installed || !p.installable;
    const installLabel = p.compatibility === 'remote' ? uiText('Inspect & install', '检查并安装') : ((p.compatibility === 'skills' || p.compatibility === 'partial') ? uiText('Import portable parts', '导入可移植部分') : uiText('Install', '安装'));
    const label = p.installed ? uiText('Installed', '已安装') : (p.installable ? installLabel : uiText('Unavailable', '暂不可用'));
    const reason = p.unavailableReason || '';
    const ecosystem = ecosystems.find(item => item.id === p.ecosystem);
    const origin = (markets.find(m => m.name === p.marketplace) || {}).displayName || (p.packageName ? p.packageName : p.marketplace);
    const components = (p.components || []).map(component => `<span class="plugin-mini-component ${escAttr(component.support || '')}" title="${escAttr(component.detail || '')}">${escHtml(component.kind || '')}</span>`).join('');
    return `<article class="plugin-card marketplace-card">
      ${pluginCardIcon(p)}<div class="plugin-card-main"><div class="plugin-card-top"><div class="plugin-title-wrap"><h4 title="${escAttr(p.name)}">${escHtml(p.displayName || p.name)}</h4>${p.version ? '<span class="plugin-version">' + escHtml(p.version) + '</span>' : ''}</div><span class="plugin-market-badge">${escHtml((ecosystem || {}).displayName || p.ecosystem || 'METIS')}</span></div>
      <p class="plugin-description">${escHtml(p.description || uiText('No description provided.', '暂无插件说明。'))}</p>
      <div class="plugin-meta">${p.category ? escHtml(p.category) + ' · ' : ''}${pluginCompatibilityLabel(p)}${skillCount ? ' · ' + skillCount + ' ' + uiText(skillCount === 1 ? 'skill' : 'skills', '个技能') : ''}${p.developer ? ' · ' + escHtml(p.developer) : ''}</div>
      <div class="plugin-component-mini-row">${components}</div>
      <div class="plugin-card-footer"><span class="plugin-availability" title="${escAttr(reason || origin)}">${disabled && !p.installed ? escHtml(reason) : escHtml(origin)}</span><button type="button" class="plugin-install" ${disabled ? 'disabled' : ''} onclick="installPlugin('${escOnclick(p.name)}','${escOnclick(p.marketplace)}',this)">${label}</button></div>
      </div></article>`;
  }).join('');
  const remaining = filtered.length - visible.length;
  list.innerHTML = cards + (remaining > 0 ? `<div class="plugin-catalog-more"><button type="button" onclick="showMorePluginCatalog()">${uiText('Show more', '显示更多')}</button><span>${remaining.toLocaleString()} ${uiText('remaining', '个待显示')}</span></div>` : '');
}

function showMorePluginCatalog() {
  pluginCatalogLimit += PLUGIN_CATALOG_PAGE_SIZE;
  paintPluginCatalog();
}

async function refreshPluginCatalog(options) {
  options = options || {};
  const button = document.getElementById('pluginRefreshBtn');
  const status = document.getElementById('pluginCatalogStatus');
  if (button && button.disabled) return;
  if (button) { button.disabled = true; button.classList.add('pending'); }
  pluginCatalogSyncing = true;
  paintPluginCatalog();
  if (status) status.textContent = options.automatic
    ? uiText('Loading all registered marketplaces…', '正在加载全部已注册插件商场…')
    : uiText('Syncing marketplace catalogs…', '正在同步插件商场目录…');
  try {
    const requested = options.all || pluginEcosystemFilter === 'all' ? [] : (pluginCatalogCache.marketplaces || []).filter(market => market.ecosystem === pluginEcosystemFilter).map(market => market.name);
    if (!options.all && pluginEcosystemFilter !== 'all' && requested.length === 0) {
      const local = await fetch('/api/plugins/catalog');
      const localCatalog = await local.json();
      if (!local.ok) throw new Error(localCatalog.error || 'local ecosystem refresh failed');
      pluginCatalogCache = localCatalog;
      pluginCatalogLimit = PLUGIN_CATALOG_PAGE_SIZE;
      paintPluginCatalog();
      if (status) status.textContent = uiText('Local ecosystem profiles rescanned.', '已重新扫描本地生态配置。');
      return;
    }
    const res = await fetch('/api/plugins/catalog/refresh', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ marketplaces: requested }) });
    const data = await res.json();
    if (data.catalog) {
      pluginCatalogCache = data.catalog;
      pluginCatalogLimit = PLUGIN_CATALOG_PAGE_SIZE;
    }
    paintPluginCatalog();
    const failures = (data.results || []).filter(item => item.error);
    if (!res.ok || failures.length) throw new Error(failures.map(item => item.marketplace + ': ' + item.error).join('; ') || data.error || 'sync failed');
    if (status) status.textContent = Number((pluginCatalogCache.plugins || []).length).toLocaleString() + ' ' + uiText('plugins loaded from registered marketplaces.', '个插件已从注册商场加载。');
  } catch (e) {
    if (status) status.textContent = uiText('Some marketplaces could not be synced: ', '部分商场同步失败：') + e.message;
  } finally {
    pluginCatalogSyncing = false;
    paintPluginCatalog();
    if (button) { button.disabled = false; button.classList.remove('pending'); }
  }
}

function installPlugin(name, marketplace, trigger) {
  openPluginActionDialog('install', name, marketplace, trigger);
}

function removePlugin(name, trigger) {
  openPluginActionDialog('remove', name, '', trigger);
}

function openPluginActionDialog(action, name, marketplace, trigger) {
  closePluginActionDialog(true);
  const installing = action === 'install';
  const catalogPlugin = installing ? (pluginCatalogCache.plugins || []).find(p => p.name === name && p.marketplace === marketplace) : null;
  const partial = catalogPlugin && catalogPlugin.compatibility === 'partial';
  const remote = catalogPlugin && catalogPlugin.compatibility === 'remote';
  const installDescription = partial
    ? uiText('METIS will import the compatible skill files only. This plugin references Codex apps or runtime tools, so some workflows may remain unavailable after restart.', 'METIS 只会导入兼容的 Skill 文件。该插件引用了 Codex App 或专用运行时工具，重启后仍可能有部分流程不可用。')
    : (remote
      ? uiText('METIS will fetch the pinned HTTPS Git source, inspect it, reject unsafe paths or symlinks, and install only if it contains a native manifest or compatible skills.', 'METIS 将获取已锁定的 HTTPS Git 源，检查内容并拒绝越界路径或符号链接；只有包含原生清单或兼容 Skills 时才会安装。')
      : uiText('METIS will copy this extension from the registered marketplace. Review the source before enabling it; installed plugins may add tools, skills, hooks, or subprocesses after restart.', 'METIS 将从已注册商场复制此扩展。启用前请确认来源；重启后，插件可能增加工具、技能、钩子或子进程。'));
  const overlay = document.createElement('div');
  overlay.className = 'plugin-action-overlay';
  overlay.innerHTML = `<div class="plugin-action-dialog" role="alertdialog" aria-modal="true" aria-labelledby="pluginActionTitle" aria-describedby="pluginActionDescription">
    <div class="plugin-action-icon" aria-hidden="true">${installing ? '&#129513;' : '&#128465;'}</div>
    <div class="plugin-action-copy"><h3 id="pluginActionTitle">${installing ? uiText('Install plugin?', '安装这个插件？') : uiText('Remove plugin?', '移除这个插件？')}</h3>
      <p class="plugin-action-name">${escHtml(name)}</p>
      <p id="pluginActionDescription">${installing ? installDescription : uiText('The plugin will be moved to METIS trash and stop loading after Desktop restarts. It can be recovered manually from the returned trash path.', '插件将移动到 METIS 回收目录，并在 Desktop 重启后停止加载；仍可从返回的回收路径手动恢复。')}</p>
      <p class="plugin-action-source">${installing ? uiText('Source: ', '来源：') + escHtml(marketplace) : ''}</p><p class="plugin-action-error" role="alert" aria-live="assertive" hidden></p></div>
    <div class="plugin-action-buttons"><button type="button" class="plugin-action-cancel">${uiText('Cancel', '取消')}</button><button type="button" class="plugin-action-confirm ${installing ? '' : 'danger'}">${installing ? uiText('Install plugin', '安装插件') : uiText('Remove plugin', '移除插件')}</button></div>
  </div>`;
  document.body.appendChild(overlay);
  const cancel = overlay.querySelector('.plugin-action-cancel');
  const confirm = overlay.querySelector('.plugin-action-confirm');
  pluginActionDialog = { action, name, marketplace, trigger, overlay, pending: false };
  cancel.addEventListener('click', () => closePluginActionDialog(false));
  confirm.addEventListener('click', confirmPluginAction);
  overlay.addEventListener('click', event => { if (event.target === overlay) closePluginActionDialog(false); });
  overlay.addEventListener('keydown', pluginActionDialogKeydown);
  requestAnimationFrame(() => cancel.focus());
}

function pluginActionDialogKeydown(event) {
  if (!pluginActionDialog || pluginActionDialog.pending) return;
  if (event.key === 'Escape') { event.preventDefault(); closePluginActionDialog(false); return; }
  if (event.key !== 'Tab') return;
  const focusable = Array.from(pluginActionDialog.overlay.querySelectorAll('button:not(:disabled)'));
  if (!focusable.length) return;
  const first = focusable[0], last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}

function closePluginActionDialog(force) {
  if (!pluginActionDialog || (pluginActionDialog.pending && !force)) return;
  const state = pluginActionDialog;
  pluginActionDialog = null;
  state.overlay.remove();
  if (state.trigger && state.trigger.isConnected) state.trigger.focus();
}

async function confirmPluginAction() {
  const state = pluginActionDialog;
  if (!state || state.pending) return;
  const cancel = state.overlay.querySelector('.plugin-action-cancel');
  const confirm = state.overlay.querySelector('.plugin-action-confirm');
  const error = state.overlay.querySelector('.plugin-action-error');
  state.pending = true;
  cancel.disabled = true;
  confirm.disabled = true;
  confirm.textContent = state.action === 'install' ? uiText('Installing…', '正在安装…') : uiText('Removing…', '正在移除…');
  try {
    const installing = state.action === 'install';
    const res = await fetch(installing ? '/api/plugins/install' : '/api/plugins/remove', {
      method: installing ? 'POST' : 'DELETE', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(installing ? { name: state.name, marketplace: state.marketplace } : { id: state.name })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'plugin action: ' + res.status);
    closePluginActionDialog(true);
    await Promise.all([loadPlugins(), loadPluginCatalog()]);
    showToast(installing ? uiText('Plugin installed. Restart Desktop to load it.', '插件已安装，重启 Desktop 后加载。') : uiText('Plugin moved to trash. Restart Desktop to unload it.', '插件已移至回收目录，重启 Desktop 后停止加载。'));
  } catch (e) {
    state.pending = false;
    cancel.disabled = false;
    confirm.disabled = false;
    confirm.textContent = state.action === 'install' ? uiText('Install plugin', '安装插件') : uiText('Remove plugin', '移除插件');
    error.hidden = false;
    error.textContent = e.message || uiText('Plugin operation failed.', '插件操作失败。');
    confirm.focus();
  }
}

function renderRoutingTab() {
  return `<h2>${uiText('Smart Routing', '智能路由')}</h2>
    <div class="settings-section">
      <div class="settings-section-title">${uiText('Routing overview', '路由概览')}</div>
      <div class="settings-section-desc">${uiText('Inspect the current model, routing constraints, and cached capabilities. Automatic model switching is not enabled.', '查看当前模型、路由约束和已缓存能力。自动模型切换尚未启用。')}</div>
      <div id="routingOverview" class="preset-list"><div class="provider-empty">${uiText('Loading routing state...', '正在加载路由状态…')}</div></div>
    </div>`;
}

async function loadRouting() {
  try {
    const res = await fetch('/api/routing');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'routing: ' + res.status);
    routingCache = data;
    paintRouting();
  } catch (e) {
    const list = document.getElementById('routingOverview');
    if (list) list.innerHTML = '<div class="provider-empty">' + uiText('Unable to load routing overview: ', '无法加载路由概览：') + escHtml(e.message) + '</div>';
  }
}

function capabilityText(value) {
  if (value === true) return uiText('yes', '是');
  if (value === false) return uiText('no', '否');
  return uiText('unknown', '未知');
}

const ROUTING_ZH = {
  'Pinned model': ['固定模型', 'Desktop 会保持当前选中的提供商/模型，直到用户主动更改。'],
  'Attachments': ['附件', '图像轮次需要支持附件的模型；未知的私有网关可自行判定。'],
  'Reasoning effort': ['推理强度', '仅当模型和传输层声明安全映射时显示推理强度控件。'],
  'Tool use': ['工具使用', '代理轮次需要支持工具调用的模型；目录事实仅用于查看。'],
};

function paintRouting() {
  const list = document.getElementById('routingOverview');
  if (!list || !routingCache) return;
  const rules = (routingCache.rules || []).map(rule => {
    const translated = document.documentElement.lang === 'zh-CN' ? ROUTING_ZH[rule.name] : null;
    return `<div class="routing-rule"><strong>${escHtml(translated ? translated[0] : rule.name)}</strong><span>${escHtml(translated ? translated[1] : rule.description)}</span></div>`;
  }).join('');
  const models = (routingCache.models || []).map(m => `<div class="provider-card">
    <div class="provider-main"><div class="provider-title">${escHtml(m.provider)} · ${escHtml(m.model)}${m.current ? ' <span class="provider-badge">' + uiText('Current', '当前') + '</span>' : ''}</div>
      <div class="provider-meta">${uiText('Reasoning', '推理')} ${capabilityText(m.reasoning)} · ${uiText('Attachments', '附件')} ${capabilityText(m.attachment)} · ${uiText('Tools', '工具')} ${capabilityText(m.toolCall)}${m.contextWindow ? ' · ' + fmtTokens(m.contextWindow) + uiText(' context', ' 上下文') : ''}</div>
      <div class="preset-description">${escHtml(m.capabilityNote || '')}</div>
    </div>
  </div>`).join('') || '<div class="provider-empty">' + uiText('No configured models', '没有已配置的模型') + '</div>';
  const note = document.documentElement.lang === 'zh-CN'
    ? '未启用自动最低成本模型切换；此概览为只读，且绝不暴露凭据。'
    : (routingCache.note || '');
  list.innerHTML = `<div class="routing-rules">${rules}</div>${models}<div class="settings-section-desc">${escHtml(note)}</div>`;
}

let configFileCache = null;

function renderConfigTab() {
  const model = settingsCache && settingsCache.model ? settingsCache.model.split('/').pop() : '-';
  let html = `<h2>${uiText('Configuration', '配置')}</h2>
    <div class="settings-section">
      <div class="settings-section-title">${uiText('Config Files', '配置文件')}</div>
      <div class="settings-section-desc">${uiText('Click a file to view its contents', '点击文件查看内容')}</div>
      <div class="settings-card">
        <div class="config-file-block" id="cfUser" onclick="toggleConfigFile('cfUser')">
          <div class="config-file-row">
            <span class="cf-chevron">\u25B8</span>
            <div style="flex:1;min-width:0">
              <div class="cf-path">~/.metis/config.toml</div>
              <div class="cf-desc">${uiText('User configuration - edited by this panel', '用户配置 — 由此面板编辑')}</div>
            </div>
          </div>
          <div class="config-file-content"></div>
        </div>
        <div class="config-file-block" id="cfProject" onclick="toggleConfigFile('cfProject')">
          <div class="config-file-row">
            <span class="cf-chevron">\u25B8</span>
            <div style="flex:1;min-width:0">
              <div class="cf-path">./.metis/config.toml</div>
              <div class="cf-desc">${uiText('Project configuration (overrides user settings)', '项目配置（覆盖用户设置）')}</div>
            </div>
          </div>
          <div class="config-file-content"></div>
        </div>
      </div>
    </div>
    <div class="settings-section">
      <div class="settings-section-title">${uiText('Runtime', '运行时')}</div>
      <div class="settings-card">
        <div class="settings-card-row"><div><div class="settings-card-label">${uiText('Model', '模型')}</div><div class="settings-card-desc">${escHtml(model)}</div></div></div>
        <div class="settings-card-row"><div><div class="settings-card-label">Web UI</div><div class="settings-card-desc">${uiText('Settings marked "restart required" apply to the next metis launch', '标记为“需要重启”的设置会在下次启动 metis 时生效')}</div></div></div>
      </div>
    </div>`;
  return html;
}

function toggleConfigFile(id) {
  document.getElementById(id).classList.toggle('open');
  const chev = document.querySelector('#' + id + ' .cf-chevron');
  if (chev) chev.textContent = document.getElementById(id).classList.contains('open') ? '\u25BE' : '\u25B8';
}

async function fillConfigFiles() {
  if (configFileCache) { paintConfigFiles(configFileCache); return; }
  try {
    const res = await fetch('/api/config/file');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'config file: ' + res.status);
    configFileCache = data;
    paintConfigFiles(data);
  } catch (e) {
    document.querySelectorAll('.config-file-content').forEach(el => {
      el.textContent = uiText('Failed to load: ', '加载失败：') + e.message;
    });
  }
}

function paintConfigFiles(d) {
  const user = document.querySelector('#cfUser .config-file-content');
  if (user) user.textContent = d.userContent || uiText('(empty file)', '（空文件）');
  const proj = document.querySelector('#cfProject .config-file-content');
  if (proj) proj.textContent = d.projectContent || uiText('(no project config)', '（无项目配置）');
}

async function saveSettingValue(key, value) {
  try {
    const res = await fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ changes: [{ key: key, value: value }] })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'save: ' + res.status);
    const st = settingByKey(key);
    if (st) st.value = String(value);
    if (key === 'ui.theme') applyTheme(value);
    if (key === 'permission.mode') syncApprovalChip(String(value));
    const live = (data.liveApplied || []).includes(key);
    showToast(live ? 'Saved: ' + key + ' (applied immediately)' : 'Saved: ' + key + ' (applies after restart)');
  } catch (e) {
    showToast('Save failed: ' + e.message);
    renderSettingsTab(); // restore UI to server truth
  }
}

function chooseEnum(el) {
  if (el.classList.contains('locked')) return;
  const key = el.dataset.key;
  const value = el.dataset.value;
  document.querySelectorAll('.radio-card[data-key="' + escAttr(key) + '"]').forEach(c => {
    c.classList.remove('selected');
    c.setAttribute('aria-pressed', 'false');
  });
  el.classList.add('selected');
  el.setAttribute('aria-pressed', 'true');
  saveSettingValue(key, value);
}

function saveSetting(input) {
  saveSettingValue(input.dataset.key, input.value);
}

async function toggleSetting(el) {
  if (el.classList.contains('locked')) return;
  const value = el.dataset.value;
  const on = value === 'true';
  el.classList.toggle('on', on);
  el.setAttribute('aria-pressed', on ? 'true' : 'false');
  el.dataset.value = on ? 'false' : 'true';
  await saveSettingValue(el.dataset.key, value);
}

// Transient banner shown when the SSE stream drops; EventSource
// reconnects on its own.
function showReconnectBanner() {
  let el = document.getElementById('reconnectBanner');
  if (!el) {
    el = document.createElement('div');
    el.id = 'reconnectBanner';
    el.className = 'reconnect-banner';
    el.textContent = 'Reconnecting...';
    document.body.appendChild(el);
  }
  el.classList.add('show');
}

function hideReconnectBanner() {
  const el = document.getElementById('reconnectBanner');
  if (el) el.classList.remove('show');
}

// Export the session transcript: the server writes the same glyph-led
// txt the CLI /export command produces and returns its path.
async function exportSession() {
  if (!currentSessionId) { showToast('No active session to export'); return; }
  try {
    const res = await fetch('/api/export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: currentSessionId })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'export: ' + res.status);
    showExportResult(data.path);
  } catch (e) {
    showToast('Export failed: ' + e.message);
  }
}

function exportFileName(path) {
  const parts = String(path || '').replace(/\\/g, '/').split('/').filter(Boolean);
  return parts[parts.length - 1] || uiText('Conversation export', '会话导出文件');
}

function closeExportResult() {
  const el = document.getElementById('exportResultBanner');
  if (el) el.classList.remove('show');
}

function showExportResult(path) {
  let el = document.getElementById('exportResultBanner');
  if (!el) {
    el = document.createElement('div');
    el.id = 'exportResultBanner';
    el.className = 'export-result-banner';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    document.body.appendChild(el);
  }
  const filename = exportFileName(path);
  el.dataset.path = String(path || '');
  el.innerHTML = '<span class="export-result-icon" aria-hidden="true">&#10003;</span>' +
    '<span class="export-result-copy"><strong>' + uiText('Session exported', '会话已导出') + '</strong><small title="' + escAttr(filename) + '">' + escHtml(filename) + '</small></span>' +
    '<span class="export-result-actions"><button type="button" onclick="copyExportPath(this)">' + uiText('Copy path', '复制路径') + '</button><button type="button" onclick="openExportsFolder(this)">' + uiText('Open folder', '打开文件夹') + '</button><button type="button" class="export-result-close" onclick="closeExportResult()" aria-label="' + uiText('Dismiss export result', '关闭导出提示') + '">&#215;</button></span>';
  el.classList.add('show');
  clearTimeout(el._hideTimer);
  el._hideTimer = setTimeout(closeExportResult, 12000);
}

async function copyExportPath(button) {
  const banner = document.getElementById('exportResultBanner');
  const path = banner ? banner.dataset.path : '';
  try {
    await navigator.clipboard.writeText(path);
    button.textContent = uiText('Copied', '已复制');
  } catch (e) {
    showToast(uiText('Unable to copy the export path.', '无法复制导出路径。'));
  }
}

async function openExportsFolder(button) {
  button.disabled = true;
  try {
    const res = await fetch('/api/exports/open', { method: 'POST' });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'open exports: ' + res.status);
    closeExportResult();
  } catch (e) {
    button.disabled = false;
    showToast(uiText('Unable to open the exports folder: ', '无法打开导出文件夹：') + e.message);
  }
}

document.addEventListener('click', function(e) {
  const menu = document.getElementById('modelMenu');
  if (menu && !e.target.closest('.model-chip')) { menu.style.display = 'none'; modelMenuOpen = false; }
  const effort = document.getElementById('effortMenu');
  const effortBtn = document.getElementById('effortBtn');
  if (effort && effortBtn && !effort.contains(e.target) && !effortBtn.contains(e.target)) {
    effort.style.display = 'none'; effortBtn.setAttribute('aria-expanded', 'false');
  }
});
