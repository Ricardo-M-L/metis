// Metis Desktop - trajectory view (DSH ui-trajectory parity).
// Toolbar (Duration / Turns / Calls + search), a 3-lane overview strip,
// a turn-aware event ledger table, and a tabbed record inspector.
// Shared helpers (escHtml/escAttr/showToast) live in app.js; the
// markdown renderer (formatContent) lives in chat.js.

let currentView = 'chat';
let traceEvents = [];
let traceRows = [];        // display rows derived from traceEvents
let traceSearchQuery = '';
let traceTurnsFolded = false;
let traceCallsFolded = false;
let traceDurMode = false;  // false = equal-width ops (DSH default)
let traceFoldedTurns = new Set();        // turn numbers folded
let traceFoldedAssistants = new Set();   // row idx of assistant rows with folded calls
let traceSelectedIdx = -1;   // selected row (idx into traceRows)
let traceSelectedReq = -1;   // selected request (turn number)
let traceTab = 'summary';
let traceNextCursor = '';
let traceTotalEvents = 0;
let traceLoadingOlder = false;
const TRACE_REDACTED_THINKING_PLACEHOLDER = 'Reasoning redacted by provider';

// DSH kind palette: tag label, tag color class, timeline lane.
const KIND_META = {
  user:               { label: 'USER',       cls: 'k-user',      lane: 0 },
  thinking:           { label: 'THINK',      cls: 'k-thinking',  lane: 1 },
  thinking_redacted:  { label: 'THINK',      cls: 'k-thinking',  lane: 1 },
  text:               { label: 'ASSISTANT',  cls: 'k-assistant', lane: 1 },
  plan:               { label: 'PLAN',       cls: 'k-assistant', lane: 1 },
  tool_start:         { label: 'TOOL',       cls: 'k-tool',      lane: 2 },
  tool_result:        { label: 'TOOL',       cls: 'k-subtool',   lane: 2 },
  subagent_start:     { label: 'SUBAGENT',   cls: 'k-subtool',   lane: 2 },
  subagent_end:       { label: 'SUBAGENT',   cls: 'k-subtool',   lane: 2 },
  subagent_progress:  { label: 'SUBAGENT',   cls: 'k-subtool',   lane: 2 },
  permission_request: { label: 'PERMISSION', cls: 'k-tool',      lane: 2 },
  rate_limit:         { label: 'RATE LIMIT', cls: 'k-tool',      lane: 2 },
  model_fallback:     { label: 'FALLBACK',   cls: 'k-tool',      lane: 2 },
  tokens:             { label: 'USAGE',      cls: 'k-context',   lane: 1 },
  context_warn:       { label: 'CONTEXT',    cls: 'k-context',   lane: 0 },
  compaction_start:   { label: 'COMPACTION', cls: 'k-context',   lane: 0 },
  compaction_progress:{ label: 'COMPACTION', cls: 'k-context',   lane: 0 },
  compaction_end:     { label: 'COMPACTION', cls: 'k-context',   lane: 0 },
  context_compacted:  { label: 'COMPACTED',  cls: 'k-system',    lane: 0 },
  info:               { label: 'INFO',       cls: 'k-system',    lane: 1 },
  error:              { label: 'ERROR',      cls: 'k-error',     lane: 2 },
};

function metaOf(kind) {
  return KIND_META[kind] || { label: String(kind || '?').toUpperCase(), cls: 'k-system', lane: 1 };
}

function switchView(view) {
  if (view !== 'artifacts' && typeof leaveArtifactsPanel === 'function') {
    leaveArtifactsPanel();
  }
  currentView = view;
  document.getElementById('tabChat').classList.toggle('active', view === 'chat');
  document.getElementById('tabTrace').classList.toggle('active', view === 'trace');
  const artifactsTab = document.getElementById('tabArtifacts');
  if (artifactsTab) artifactsTab.classList.toggle('active', view === 'artifacts');
  document.querySelector('.main').classList.toggle('trace-mode', view === 'trace');
  document.querySelector('.app').classList.toggle('trace-mode', view === 'trace');
  document.getElementById('tracePanel').classList.toggle('visible', view === 'trace');
  if (view === 'trace') {
    document.querySelector('.main').classList.remove('empty');
    loadTrace();
  } else {
    updateEmptyLayout();
  }
}

function fmtMs(ms) {
  if (!ms) return '';
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.round((ms % 60000) / 1000) + 's';
}

function fmtRunDur(ms) {
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return sec + '\u79D2';
  return Math.floor(sec / 60) + '\u5206' + String(sec % 60).padStart(2, '0') + '\u79D2';
}

function fmtTokens(n) {
  n = Number(n) || 0;
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
  return String(n);
}

function oneLine(s) {
  return String(s || '').replace(/\s+/g, ' ').trim();
}

function fmtTimestamp(ts) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return 'Not available';
  const two = v => String(v).padStart(2, '0');
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`;
}

function onTraceSearch(q) {
  traceSearchQuery = (q || '').trim().toLowerCase();
  renderTrace();
}

function toggleDurMode() {
  traceDurMode = !traceDurMode;
  document.getElementById('btnDurMode').setAttribute('aria-pressed', String(traceDurMode));
  document.getElementById('btnDurMode').title = traceDurMode ? 'Use equal-width operations' : 'Use actual duration';
  renderTrace();
}

function toggleFoldTurns() {
  traceTurnsFolded = !traceTurnsFolded;
  if (traceTurnsFolded) {
    for (const r of traceRows) if (r.turn !== null) traceFoldedTurns.add(r.turn);
  } else {
    traceFoldedTurns = new Set();
  }
  renderTrace();
}

function toggleFoldCalls() {
  traceCallsFolded = !traceCallsFolded;
  if (traceCallsFolded) {
    for (const r of traceRows) {
      if (r.kind === 'text' && toolChildren(r).length > 0) traceFoldedAssistants.add(r.idx);
    }
  } else {
    traceFoldedAssistants = new Set();
  }
  renderTrace();
}

function toggleTraceTurn(turn) {
  if (traceFoldedTurns.has(turn)) traceFoldedTurns.delete(turn);
  else traceFoldedTurns.add(turn);
  renderTrace();
}

function toggleTraceAssistant(idx) {
  if (traceFoldedAssistants.has(idx)) traceFoldedAssistants.delete(idx);
  else traceFoldedAssistants.add(idx);
  renderTrace();
}

// --- Row model ----------------------------------------------------------

// Runtime compaction can emit dozens of byte-progress samples. Keep those
// events in the saved trace, but present each lifecycle as one readable row.
function coalesceTraceCompactions(events) {
  const display = [];
  const active = new Map();
  const completed = new Map();
  const keyOf = ev => String(ev.turn || 0);
  const runningText = 'Compacting context…';
  const completeText = 'Conversation history compacted';

  for (const source of events || []) {
    const ev = Object.assign({}, source);
    const key = keyOf(ev);
    if (ev.kind === 'compaction_start') {
      const aggregate = Object.assign({}, ev, { kind: 'compaction_progress', text: runningText });
      display.push(aggregate);
      active.set(key, aggregate);
      completed.delete(key);
      continue;
    }
    if (ev.kind === 'compaction_progress') {
      let aggregate = active.get(key);
      if (!aggregate) {
        aggregate = Object.assign({}, ev, { kind: 'compaction_progress', text: runningText });
        display.push(aggregate);
        active.set(key, aggregate);
      }
      aggregate.ts = ev.ts || aggregate.ts;
      aggregate.elapsedMs = ev.elapsedMs || aggregate.elapsedMs;
      continue;
    }
    if (ev.kind === 'context_compacted') {
      let aggregate = active.get(key);
      const text = !ev.text || ev.text === 'auto' ? completeText : ev.text;
      if (aggregate) Object.assign(aggregate, ev, { kind: 'context_compacted', text });
      else {
        aggregate = Object.assign({}, ev, { text });
        display.push(aggregate);
      }
      active.delete(key);
      completed.set(key, aggregate);
      continue;
    }
    if (ev.kind === 'compaction_end') {
      const aggregate = active.get(key);
      if (aggregate) {
        if (ev.isError) Object.assign(aggregate, ev, { kind: 'error', text: ev.text || 'Context compaction failed' });
        else {
          Object.assign(aggregate, ev, { kind: 'context_compacted', text: completeText });
          completed.set(key, aggregate);
        }
        active.delete(key);
      } else if (ev.isError) {
        display.push(Object.assign({}, ev, { kind: 'error', text: ev.text || 'Context compaction failed' }));
      }
      continue;
    }
    if (ev.kind === 'info' && /^context compacted\b/i.test(String(ev.text || ''))) {
      const aggregate = completed.get(key);
      if (aggregate) {
        const summary = String(ev.text || '').replace(/^context compacted\s*(?:\(auto\))?\s*:\s*/i, '');
        aggregate.text = completeText + (summary ? ' · ' + summary : '');
        continue;
      }
    }
    display.push(ev);
  }
  return display;
}

function buildTraceRows(events) {
  const displayEvents = coalesceTraceCompactions(events);
  const rows = [];
  const pendingTool = new Map();   // toolUseID -> tool row awaiting result
  const pendingByName = [];
  const partialArgs = new Map();   // toolUseID/name -> interrupted arg stream
  const partialOrder = [];
  const toolKey = ev => ev.toolUseID ? 'id:' + ev.toolUseID : 'name:' + (ev.toolName || '');
  for (let i = 0; i < displayEvents.length; i++) {
    const ev = Object.assign({}, displayEvents[i]);
    if (ev.kind === 'turn_end' || ev.kind === 'loop_done') continue; // boundaries only

    if (ev.kind === 'tool_args' || ev.kind === 'tool_args_delta') {
      const key = toolKey(ev);
      if (!partialArgs.has(key)) {
        partialArgs.set(key, { ev: Object.assign({}, ev), idx: i, text: '' });
        partialOrder.push(key);
      }
      partialArgs.get(key).text += ev.text || '';
      continue;
    }

    if (ev.kind === 'tool_start') {
      const key = toolKey(ev);
      let partialKey = key;
      let partial = partialArgs.get(partialKey);
      if (!partial && ev.toolName) {
        partialKey = 'name:' + ev.toolName;
        partial = partialArgs.get(partialKey);
      }
      if (!partial) {
        partialKey = 'name:';
        partial = partialArgs.get(partialKey);
      }
      if (partial) {
        const current = String(ev.text || '').trim();
        if (!current || current === '{}' || current === 'null') ev.text = partial.text;
        partialArgs.delete(partialKey);
      }
    }

    if (ev.kind === 'tool_result') {
      const key = ev.toolUseID || '';
      let target = key ? pendingTool.get(key) : null;
      if (!target && ev.toolName) {
        target = pendingByName.find(r => r.toolName === ev.toolName && !r.resultText) || null;
      }
      if (target) {
        target.resultText = ev.text || '';
        target.resultIsError = !!ev.isError;
        target.resultIdx = i;
        if (!target.toolUseID) target.toolUseID = key;
        continue;
      }
    }

    const meta = metaOf(ev.kind);
    const row = {
      idx: i,
      kind: ev.kind,
      turn: ev.turn || 0,
      seq: ev.sequence,
      ts: ev.ts,
      elapsedMs: ev.elapsedMs || 0,
      isError: !!ev.isError,
      depth: ev.depth || 0,
      parentID: ev.parentID || '',
      toolName: ev.toolName || '',
      toolUseID: ev.toolUseID || '',
      text: ev.kind === 'thinking_redacted'
        ? TRACE_REDACTED_THINKING_PLACEHOLDER
        : (ev.text || ''),
      resultText: '',
      resultIsError: false,
      lane: meta.lane,
      tokens: null,
    };
    if (ev.kind === 'error' && ev.toolName) row.lane = 2;
    if (ev.kind === 'tool_start') {
      if (row.toolUseID) pendingTool.set(row.toolUseID, row);
      pendingByName.push(row);
    }
    if (ev.kind === 'tokens') {
      const m = /input=(\d+) output=(\d+)(?: cache_write=(\d+))?(?: cache_read=(\d+))?/.exec(ev.text || '');
      row.tokens = m
        ? { input: +m[1], output: +m[2], cacheWrite: +(m[3] || 0), cacheRead: +(m[4] || 0) }
        : { input: 0, output: 0, cacheWrite: 0, cacheRead: 0 };
    }
    rows.push(row);
  }
  // Preserve an interrupted provider stream as one pending tool row.
  for (const key of partialOrder) {
    const partial = partialArgs.get(key);
    if (!partial || !partial.text) continue;
    const ev = Object.assign({}, partial.ev, { kind: 'tool_start', text: partial.text });
    const meta = metaOf(ev.kind);
    rows.push({
      idx: partial.idx, kind: ev.kind, turn: ev.turn || 0, seq: ev.sequence,
      ts: ev.ts, elapsedMs: ev.elapsedMs || 0, isError: !!ev.isError,
      depth: ev.depth || 0, parentID: ev.parentID || '', toolName: ev.toolName || '',
      toolUseID: ev.toolUseID || '', text: ev.text || '', resultText: '',
      resultIsError: false, lane: meta.lane, tokens: null,
    });
  }
  rows.sort((a, b) => a.idx - b.idx);
  return rows;
}

function isToolRow(r) {
  return r.kind === 'tool_start' || r.kind === 'tool_result';
}

function toolChildren(r) {
  // Tool rows immediately following this assistant row in the same turn.
  const out = [];
  const at = traceRows.indexOf(r);
  if (at === -1) return out;
  for (let i = at + 1; i < traceRows.length; i++) {
    const n = traceRows[i];
    if (!isToolRow(n)) break;
    out.push(n);
  }
  return out;
}

function rowText(r) {
  if (r.kind === 'thinking_redacted') return TRACE_REDACTED_THINKING_PLACEHOLDER;
  if (isToolRow(r)) {
    const name = r.toolName || r.kind.toUpperCase();
    const args = oneLine(r.text);
    if (r.kind === 'tool_result' && !r.toolName) return '';
    return args ? name + ' ' + args : name;
  }
  if (r.kind === 'tokens' && r.tokens) {
    const t = r.tokens;
    return `input=${t.input} output=${t.output} cache_read=${t.cacheRead} cache_write=${t.cacheWrite}`;
  }
  return oneLine(r.text) || metaOf(r.kind).label;
}

function rowMatches(r) {
  if (!traceSearchQuery) return true;
  const hay = (rowText(r) + ' ' + (r.toolName || '') + ' ' + metaOf(r.kind).label + ' ' + (r.text || '')).toLowerCase();
  return hay.includes(traceSearchQuery);
}

function rowState(r) {
  if (r.isError || r.resultIsError) return 'error';
  if (isToolRow(r) && r.kind === 'tool_start' && !r.resultText && !r.resultIsError) return 'running';
  return 'complete';
}

function statusLabel(state) {
  return state === 'error' ? 'Failed' : state === 'running' ? 'Pending' : 'Completed';
}

// --- Loading ------------------------------------------------------------

function traceEventKey(ev) {
  if (ev.id) return 'id:' + ev.id;
  return [ev.sequence, ev.ts, ev.kind, ev.toolUseID, ev.parentID].join('|');
}

function mergeTraceEvents(older, current) {
  const seen = new Set();
  const merged = [];
  [...older, ...current].forEach(ev => {
    const key = traceEventKey(ev);
    if (seen.has(key)) return;
    seen.add(key);
    merged.push(ev);
  });
  return merged;
}

async function loadTrace(loadOlder = false) {
  if (loadOlder && (!traceNextCursor || traceLoadingOlder)) return;
  const body = document.getElementById('traceBody');
  const track = document.getElementById('traceTrack');
  if (!loadOlder) {
    traceNextCursor = '';
    traceTotalEvents = 0;
  }
  traceLoadingOlder = loadOlder;
  try {
    const params = new URLSearchParams({ limit: '500' });
    if (currentSessionId) params.set('sessionId', currentSessionId);
    if (loadOlder) params.set('cursor', traceNextCursor);
    const url = '/api/trace?' + params.toString();
    const res = await fetch(url);
    if (!res.ok) throw new Error('trace: ' + res.status);
    const data = await res.json();
    if (!data.enabled) {
      track.innerHTML = '';
      body.innerHTML = '<tr><td colspan="2"><div class="tt-empty">Session tracing is not enabled for this process.</div></td></tr>';
      closeTraceInspector(false);
      return;
    }
    traceEvents = loadOlder
      ? mergeTraceEvents(data.events || [], traceEvents)
      : (data.events || []);
    traceNextCursor = data.nextCursor || '';
    traceTotalEvents = Number(data.totalEvents) || traceEvents.length;
    traceRows = buildTraceRows(traceEvents);
    traceSelectedIdx = -1;
    traceSelectedReq = -1;
    renderTrace();
  } catch (e) {
    track.innerHTML = '';
    body.innerHTML = '<tr><td colspan="2"><div class="tt-empty">Failed to load trajectory: ' + escHtml(e.message) + '</div></td></tr>';
    closeTraceInspector(false);
  } finally {
    traceLoadingOlder = false;
  }
}

// --- Timeline strip -----------------------------------------------------

function spanKindOf(r) {
  if (r.isError) return 'model';
  const m = metaOf(r.kind);
  if (r.kind === 'user') return 'user';
  if (r.kind === 'context_warn' || r.kind === 'context_compacted') return 'context';
  if (m.lane === 2) return 'tool';
  return 'model';
}

function renderTimeline() {
  const track = document.getElementById('traceTrack');
  if (!traceRows.length) {
    track.innerHTML = '<span class="tt-track-empty">No timing data</span>';
    return;
  }
  let html = '';
  const times = traceRows.map(r => Date.parse(r.ts));
  const finite = times.filter(Number.isFinite);
  const min = finite.length ? Math.min(...finite) : 0;
  const max = finite.length ? Math.max(...finite) : 1;
  const span = Math.max(max - min, 1);

  traceRows.forEach((r, i) => {
    const isErr = r.isError || r.resultIsError;
    const kind = spanKindOf(r);
    let left, width;
    if (!traceDurMode) {
      left = (i / traceRows.length) * 100;
      width = 8;
    } else {
      const t = times[i];
      if (!Number.isFinite(t)) return;
      left = ((t - min) / span) * 100;
      width = Math.max((r.elapsedMs / span) * 100, 0.25);
    }
    const cur = i === traceSelectedIdx ? 'true' : undefined;
    html += `<span class="tt-span" data-kind="${kind}"${isErr ? ' data-error="true"' : ''}${cur ? ' data-current="true"' : ''} title="${escAttr(metaOf(r.kind).label + (r.toolName ? ' ' + r.toolName : '') + (r.elapsedMs ? ' (' + fmtMs(r.elapsedMs) + ')' : ''))}" style="--lane:${r.lane};left:${left.toFixed(2)}%;width:${Math.max(width, 2).toFixed(2)}px" onclick="selectTraceRow(${i}, true)"></span>`;
  });

  // Turn boundaries: a 1px tick where each turn starts.
  let lastTurn = null;
  traceRows.forEach((r, i) => {
    if (r.turn !== lastTurn) {
      lastTurn = r.turn;
      let left;
      if (!traceDurMode) left = (i / traceRows.length) * 100;
      else {
        const t = Date.parse(r.ts);
        if (!Number.isFinite(t)) return;
        left = ((t - min) / span) * 100;
      }
      html += `<span class="tt-turn-boundary" style="left:${left.toFixed(2)}%"></span>`;
    }
  });
  track.innerHTML = html;
}

// --- Ledger table -------------------------------------------------------

function renderTrace() {
  const body = document.getElementById('traceBody');
  document.getElementById('btnFoldTurns').classList.toggle('active', traceTurnsFolded);
  document.getElementById('btnFoldCalls').classList.toggle('active', traceCallsFolded);
  const foldTurnsIcon = traceTurnsFolded ? '\u229E' : '\u229F'; // ⊞ / ⊟
  const foldCallsIcon = traceCallsFolded ? '\u229E' : '\u229F';
  const bt = document.getElementById('btnFoldTurns');
  const bc = document.getElementById('btnFoldCalls');
  if (bt) bt.innerHTML = `<span class="tt-action-icon">${foldTurnsIcon}</span>Turns`;
  if (bc) bc.innerHTML = `<span class="tt-action-icon">${foldCallsIcon}</span>Calls`;

  renderTimeline();

  if (!traceRows.length) {
    body.innerHTML = '<tr><td colspan="2"><div class="tt-empty">(no trajectory recorded for this session yet)</div></td></tr>';
    closeTraceInspector(false);
    return;
  }

  const activeTurn = traceSelectedIdx >= 0 && traceRows[traceSelectedIdx]
    ? traceRows[traceSelectedIdx].turn
    : (traceSelectedReq >= 0 ? traceSelectedReq : null);

  // Determine which rows are visible under the current fold state.
  const visible = new Set();
  let turn = null;
  for (let i = 0; i < traceRows.length; i++) {
    const r = traceRows[i];
    if (r.turn !== turn) turn = r.turn;
    if (!rowMatches(r)) continue;
    if (traceTurnsFolded && traceFoldedTurns.has(turn) && !traceSearchQuery) {
      // Keep the first row of a folded turn only (summary row is added separately).
      const firstOfTurn = traceRows.findIndex(x => x.turn === turn);
      if (i !== firstOfTurn) continue;
    }
    if (traceCallsFolded && traceFoldedAssistants.has(r.idx)) {
      visible.add(i);
      continue;
    }
    if (traceCallsFolded && isToolRow(r)) {
      // A tool row following a folded assistant is hidden.
      let hiddenByAssistant = false;
      for (let j = i - 1; j >= 0; j--) {
        const p = traceRows[j];
        if (!isToolRow(p)) { hiddenByAssistant = traceFoldedAssistants.has(p.idx); break; }
      }
      if (hiddenByAssistant) continue;
    }
    visible.add(i);
  }

  let html = '';
  let curTurn = null;
  let reqNo = 0;
  for (let i = 0; i < traceRows.length; i++) {
    const r = traceRows[i];
    const turnStart = r.turn !== curTurn;
    if (turnStart) { curTurn = r.turn; reqNo++; }
    if (!visible.has(i) && !turnStart) continue;

    // Collapsed-turn summary row.
    if (turnStart && traceTurnsFolded && traceFoldedTurns.has(r.turn) && !traceSearchQuery) {
      const tr = traceRows.filter(x => x.turn === r.turn);
      const steps = tr.filter(x => x.kind === 'text' || x.kind === 'user' || x.kind === 'plan').length;
      const calls = tr.filter(isToolRow).length;
      html += collapsedRowHtml(r, `${steps} steps \u00B7 ${calls} tool calls`, 'turn', r.turn);
      continue;
    }

    // Folded-assistant summary row.
    if (traceCallsFolded && traceFoldedAssistants.has(r.idx) && !traceSearchQuery) {
      const calls = toolChildren(r);
      const names = [...new Set(calls.map(c => c.toolName).filter(Boolean))];
      const summary = `${calls.length} tool ${calls.length === 1 ? 'call' : 'calls'}${names.length ? ' \u00B7 ' + names.join(', ') : ''}`;
      html += collapsedRowHtml(r, summary, 'assistant', r.idx);
      continue;
    }

    if (!visible.has(i)) continue;

    const meta = metaOf(r.kind);
    const sel = i === traceSelectedIdx;
    const sub = isToolRow(r) && (r.depth > 0 || !!r.parentID);
    const turnActive = activeTurn !== null && r.turn === activeTurn;
    const isErr = rowState(r) === 'error';

    html += `<tr data-kind="${isToolRow(r) ? (sub ? 'subtool' : 'tool') : r.kind}"${sub ? ' data-sub="true"' : ''}${turnStart ? ' data-turn-start="true"' : ''}${isErr ? ' data-error="true"' : ''}${sel ? ' data-selected="true"' : ''} tabindex="0" onclick="selectTraceRow(${i}, false)">
      <td class="tt-event">
        ${turnStart ? `<button class="tt-req-dot${traceSelectedReq === r.turn ? ' active' : ''}" data-label="Request #${reqNo}" title="Request #${reqNo}" onclick="event.stopPropagation();selectTraceRequest(${r.turn}, ${reqNo})"></button>` : ''}
        ${turnActive ? `<span class="tt-turn-rail"></span>` : ''}
        ${sel ? `<span class="tt-sel-rail"></span>` : ''}
        ${turnStart ? `<span class="tt-turn-label${turnActive ? ' active' : ''}">Turn ${r.turn}</span>` : ''}
        <div class="tt-event-inner"><span class="tt-kind-slot"><span class="tt-kind ${meta.cls}"><span class="k-label">${meta.label}</span></span></span></div>
      </td>
      <td class="tt-content">${contentHtml(r)}</td>
    </tr>`;
  }
  const earlier = traceNextCursor
    ? `<tr class="tt-load-earlier"><td colspan="2"><button type="button" onclick="loadTrace(true)">Load earlier events (${traceEvents.length} / ${traceTotalEvents})</button></td></tr>`
    : '';
  body.innerHTML = earlier + (html || '<tr><td colspan="2"><div class="tt-empty">(no events match the search)</div></td></tr>');
  renderTraceInspector();
}

function collapsedRowHtml(r, summary, kind, key) {
  const click = kind === 'turn'
    ? `onclick="toggleTraceTurn(${r.turn})"`
    : `onclick="toggleTraceAssistant(${key})"`;
  return `<tr data-collapsed="true" ${click} title="${escAttr(summary)}">
    <td class="tt-event"></td>
    <td class="tt-content"><span class="tt-collapsed-content"><span class="tt-collapsed-ellipsis">\u2026</span><span class="tt-collapsed-text">${escHtml(summary)}</span></span></td>
  </tr>`;
}

function contentHtml(r) {
  if (isToolRow(r)) {
    const name = r.toolName ? `<span class="tt-tool-name">${escHtml(r.toolName)}</span>` : '';
    const args = oneLine(r.text);
    const argsHtml = args ? `<span class="tt-tool-args">${escHtml(args)}</span>` : '';
    const head = `<span class="tt-content-text">${name}${argsHtml}</span>`;
    const hasResult = r.resultText || r.resultIsError;
    if (!hasResult) return head;
    const resText = oneLine(r.resultText) || '(no output)';
    return `<span class="tt-result-preview"><span class="tt-result-request">${head}</span><span class="tt-inline-result${r.resultIsError ? ' err' : ''}"><span class="arrow">\u2192</span><span class="tt-inline-result-text">${escHtml(resText)}</span></span></span>`;
  }
  const text = rowText(r);
  return `<span class="tt-content-text${r.isError ? ' is-error' : ''}">${escHtml(text)}</span>`;
}

// --- Selection / inspector ----------------------------------------------

function selectTraceRow(idx, scroll) {
  traceSelectedIdx = idx;
  traceSelectedReq = -1;
  const row = document.querySelector(`.tt-table tbody tr[data-selected="true"]`);
  if (row) row.classList.remove('selected');
  renderTrace();
  if (scroll) {
    const rows = document.querySelectorAll('.tt-table tbody tr:not([data-collapsed])');
    for (const el of rows) {
      if (el.onclick && el.textContent.indexOf('') === 0) continue;
    }
    const target = [...document.querySelectorAll('.tt-table tbody tr')].find(el => el.onclick);
    // Scroll the selected row into view via its data attrs: rebuild marks it.
    const sel = document.querySelector('.tt-table tbody tr[data-selected="true"]');
    if (sel && sel.scrollIntoView) sel.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
}

function selectTraceRequest(turn, reqNo) {
  traceSelectedReq = turn;
  traceSelectedIdx = -1;
  renderTrace();
}

function closeTraceInspector(shouldRender = true) {
  traceSelectedIdx = -1;
  traceSelectedReq = -1;
  const insp = document.getElementById('traceInspector');
  if (insp) { insp.style.display = 'none'; insp.innerHTML = ''; }
  document.querySelectorAll('.tt-table tbody tr[data-selected="true"]').forEach(el => el.classList.remove('selected'));
  if (shouldRender) renderTrace();
}

function inspectorTabsFor(r) {
  if (isToolRow(r)) return [['summary', 'Summary'], ['payload', 'Payload'], ['result', 'Result'], ['timing', 'Timing']];
  if (r.kind === 'thinking_redacted') return [['summary', 'Summary']];
  if (r.kind === 'user' || r.kind === 'thinking' || r.kind === 'text' || r.kind === 'plan') {
    return [['summary', 'Summary'], ['preview', 'Preview'], ['raw', 'Raw']];
  }
  if (r.kind === 'tokens') return [['summary', 'Summary'], ['raw', 'Raw']];
  return [['summary', 'Summary'], ['raw', 'Raw']];
}

function renderTraceInspector() {
  const insp = document.getElementById('traceInspector');
  if (traceSelectedReq >= 0) { renderRequestInspector(insp); return; }
  if (traceSelectedIdx < 0 || !traceRows[traceSelectedIdx]) {
    insp.style.display = 'none';
    insp.innerHTML = '';
    return;
  }
  const r = traceRows[traceSelectedIdx];
  const meta = metaOf(r.kind);
  const tabs = inspectorTabsFor(r);
  if (!tabs.some(t => t[0] === traceTab)) traceTab = tabs[0][0];
  const location = `Turn ${r.turn}${r.toolName ? ' \u00B7 ' + r.toolName : ''}`;
  const title = isToolRow(r)
    ? `<span class="tt-kind ${meta.cls}"><span class="k-label">${r.toolName ? 'TOOL' : meta.label}</span></span>`
    : `<span class="tt-kind ${meta.cls}"><span class="k-label">${meta.label}</span></span>`;
  insp.style.display = 'flex';
  insp.innerHTML = `
    <div class="tt-details-head">
      <div class="tt-details-title">${title}<span class="tt-details-location">${escHtml(location)}</span></div>
      <div class="tt-details-actions">
        <button type="button" class="tt-details-copy" onclick="copyTraceInspector()" aria-label="Copy trace details">Copy</button>
        <button type="button" class="tt-details-close" onclick="closeTraceInspector()" aria-label="Close details">\u00D7</button>
      </div>
    </div>
    <div class="tt-details-tabs">${tabs.map(t => `<button class="tt-detail-tab${t[0] === traceTab ? ' active' : ''}" onclick="switchTraceTab('${t[0]}')">${t[1]}</button>`).join('')}</div>
    <div class="tt-details-body">${inspectorBody(r, traceTab)}</div>`;
}

function switchTraceTab(tab) {
  traceTab = tab;
  renderTraceInspector();
}

function kvRow(k, v, extra) {
  return `<div${extra ? ' class="sub"' : ''}><dt>${k}</dt><dd>${v}</dd></div>`;
}

function formatTracePayload(value) {
  const text = String(value || '');
  if (!text.trim()) return '';
  try { return JSON.stringify(JSON.parse(text), null, 2); } catch (_) { return text; }
}

async function copyTraceInspector() {
  let payload;
  if (traceSelectedReq >= 0) {
    payload = JSON.stringify(traceRows.filter(x => x.turn === traceSelectedReq), null, 2);
  } else {
    const row = traceRows[traceSelectedIdx];
    if (!row) return;
    if (traceTab === 'result') payload = formatTracePayload(row.resultText);
    else if (['raw', 'payload', 'preview'].includes(traceTab)) payload = formatTracePayload(row.text);
    else payload = JSON.stringify(row, null, 2);
  }
  try {
    await navigator.clipboard.writeText(payload || '');
    showToast('Trace details copied');
  } catch (_) {
    showToast('Copy unavailable');
  }
}

function inspectorBody(r, tab) {
  const state = rowState(r);
  if (tab === 'summary') {
    let html = `<dl class="tt-overview">`;
    html += kvRow('Status', `<span class="${state === 'error' ? 'err' : ''}">${statusLabel(state)}</span>`);
    html += kvRow('Turn', String(r.turn));
    html += kvRow('Started', fmtTimestamp(r.ts));
    html += kvRow('Duration', r.elapsedMs ? fmtMs(r.elapsedMs) : '\u2014');
    html += kvRow('Sequence', '#' + r.seq);
    if (r.toolName) html += kvRow('Tool', escHtml(r.toolName));
    if (r.tokens) {
      const t = r.tokens;
      html += kvRow('Tokens', `${t.input + t.cacheRead + t.cacheWrite} + ${t.output} tok`);
      html += kvRow('Input', `${t.input} tok`, true);
      html += kvRow('Cached read', `${t.cacheRead} tok`, true);
      html += kvRow('Cache created', `${t.cacheWrite} tok`, true);
      html += kvRow('Output', `${t.output} tok`, true);
    }
    if (state === 'error') html += kvRow('Error', '<span class="err">' + escHtml(oneLine(r.resultText || r.text)) + '</span>');
    html += `</dl>`;
    return html;
  }
  if (tab === 'preview') {
    return `<div class="tt-markdown">${formatContent(r.text)}</div>`;
  }
  if (tab === 'raw') {
    return `<pre class="tt-payload${state === 'error' ? ' err' : ''}">${escHtml(formatTracePayload(r.text))}</pre>`;
  }
  if (tab === 'payload') {
    if (!r.text) return '<p class="tt-no-payload">No payload captured</p>';
    return `<pre class="tt-payload${r.isError ? ' err' : ''}">${escHtml(formatTracePayload(r.text))}</pre>`;
  }
  if (tab === 'result') {
    if (!r.resultText && !r.resultIsError) return '<p class="tt-no-payload">No result captured</p>';
    const cls = r.resultIsError ? ' err' : (r.resultText === 'No output' ? ' no-output' : '');
    return `<pre class="tt-payload${cls}">${escHtml(formatTracePayload(r.resultText))}</pre>`;
  }
  if (tab === 'timing') {
    return `<dl class="tt-overview">
      ${kvRow('Started', fmtTimestamp(r.ts))}
      ${kvRow('Duration', r.elapsedMs ? fmtMs(r.elapsedMs) : '\u2014')}
      ${kvRow('Timing source', r.elapsedMs ? 'Session timestamps' : 'Not available')}
    </dl>`;
  }
  return '';
}

// Request inspector: per-turn model request (DSH request dots).
function renderRequestInspector(insp) {
  const turn = traceSelectedReq;
  const tr = traceRows.filter(x => x.turn === turn);
  if (!tr.length) { closeTraceInspector(false); return; }
  const tokens = tr.filter(x => x.tokens).reduce((acc, x) => {
    acc.input += x.tokens.input; acc.output += x.tokens.output;
    acc.cacheWrite += x.tokens.cacheWrite; acc.cacheRead += x.tokens.cacheRead;
    return acc;
  }, { input: 0, output: 0, cacheWrite: 0, cacheRead: 0 });
  const calls = tr.filter(isToolRow).length;
  const ts = tr.map(x => Date.parse(x.ts)).filter(Number.isFinite);
  const started = ts.length ? new Date(Math.min(...ts)).toISOString() : null;
  const dur = ts.length > 1 ? Math.max(...ts) - Math.min(...ts) : 0;
  const hasError = tr.some(x => rowState(x) === 'error');
  const state = hasError ? 'error' : (tr.some(x => rowState(x) === 'running') ? 'running' : 'complete');
  const reqNo = [...new Set(traceRows.map(x => x.turn))].indexOf(turn) + 1;
  if (!['summary', 'usage', 'timing'].includes(traceTab)) traceTab = 'summary';
  insp.style.display = 'flex';
  insp.innerHTML = `
    <div class="tt-details-head">
      <div class="tt-details-title">
        <span class="tt-kind k-context"><span class="k-label">REQUEST</span></span>
        <span class="tt-details-location">Request #${reqNo} \u00B7 Turn ${turn}</span>
      </div>
      <div class="tt-details-actions">
        <button type="button" class="tt-details-copy" onclick="copyTraceInspector()" aria-label="Copy request details">Copy</button>
        <button type="button" class="tt-details-close" onclick="closeTraceInspector()" aria-label="Close details">\u00D7</button>
      </div>
    </div>
    <div class="tt-details-tabs">
      <button class="tt-detail-tab${traceTab === 'summary' ? ' active' : ''}" onclick="switchTraceTab('summary')">Summary</button>
      <button class="tt-detail-tab${traceTab === 'usage' ? ' active' : ''}" onclick="switchTraceTab('usage')">Usage</button>
      <button class="tt-detail-tab${traceTab === 'timing' ? ' active' : ''}" onclick="switchTraceTab('timing')">Timing</button>
    </div>
    <div class="tt-details-body">${requestBody(turn, tr, tokens, calls, started, dur, state)}</div>`;
}

function requestBody(turn, tr, tokens, calls, started, dur, state) {
  if (traceTab === 'usage') {
    return `<div class="tt-usage-group">
        <h4 class="tt-usage-head">This request</h4>
        <dl class="tt-overview">
          ${kvRow('Input', `${tokens.input + tokens.cacheRead + tokens.cacheWrite} tok`)}
          ${kvRow('Cached', `${tokens.cacheRead} tok`, true)}
          ${kvRow('Cache created', `${tokens.cacheWrite} tok`, true)}
          ${kvRow('Output', `${tokens.output} tok`)}
        </dl>
      </div>`;
  }
  if (traceTab === 'timing') {
    return `<dl class="tt-overview">
      ${kvRow('Started', started ? fmtTimestamp(started) : 'Not available')}
      ${kvRow('Duration', dur ? fmtMs(dur) : '\u2014')}
      ${kvRow('Timing source', 'Session timestamps')}
    </dl>`;
  }
  return `<dl class="tt-overview">
    ${kvRow('Status', `<span class="${state === 'error' ? 'err' : ''}">${statusLabel(state)}</span>`)}
    ${kvRow('Turn', String(turn))}
    ${kvRow('Tool calls', String(calls))}
    ${kvRow('Tokens', `${tokens.input + tokens.cacheRead + tokens.cacheWrite} + ${tokens.output} tok`)}
    ${kvRow('Input', `${tokens.input + tokens.cacheRead + tokens.cacheWrite} tok`, true)}
    ${kvRow('Output', `${tokens.output} tok`, true)}
  </dl>`;
}

// --- Export -------------------------------------------------------------

// Export the session's trajectory as a readable txt into ~/.metis/exports.
async function exportTrace(sessionId) {
  const sid = sessionId || currentSessionId;
  if (!sid) { showToast('No active session to export'); return; }
  try {
    const res = await fetch('/api/trace/export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: sid })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'trace export: ' + res.status);
    showToast('Trajectory exported: ' + data.path);
  } catch (e) {
    showToast('Trajectory export failed: ' + e.message);
  }
}
