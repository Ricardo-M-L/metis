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
let traceLoadGeneration = 0;
let traceSessionId = '';
let traceInitialContext = null;
let traceImagePreview = null;
const TRACE_REDACTED_THINKING_PLACEHOLDER = 'Reasoning redacted by provider';

// DSH kind palette: tag label, tag color class, timeline lane.
const KIND_META = {
  user:               { label: 'USER',       cls: 'k-user',      lane: 0 },
  context:            { label: 'CONTEXT',    cls: 'k-context',   lane: 0 },
  initial_context:    { label: 'CONTEXT',    cls: 'k-context',   lane: 0 },
  request_timing:     { label: 'DIAGNOSTIC', cls: 'k-system',    lane: 1 },
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
  if (view !== 'trace') closeTraceImagePreview(false);
  if (currentView === 'chat' && view !== 'chat' && typeof virtualHistorySaveViewport === 'function') virtualHistorySaveViewport();
  if (view !== 'artifacts' && typeof leaveArtifactsPanel === 'function') {
    leaveArtifactsPanel();
  }
  currentView = view;
  if (window.metisNavigation) window.metisNavigation.recordView(view);
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
    invalidateTraceLoads();
    updateEmptyLayout();
    if (view === 'chat' && typeof virtualHistoryViewportChanged === 'function') virtualHistoryViewportChanged();
  }
}

function invalidateTraceLoads() {
  traceLoadGeneration++;
  traceLoadingOlder = false;
}

function resetTraceForSession() {
  closeTraceImagePreview(false);
  invalidateTraceLoads();
  traceSessionId = String(currentSessionId || '');
  traceEvents = [];
  traceRows = [];
  traceInitialContext = null;
  traceNextCursor = '';
  traceTotalEvents = 0;
  traceSelectedIdx = -1;
  traceSelectedReq = -1;
  traceTab = 'summary';
  traceTurnsFolded = false;
  traceCallsFolded = false;
  traceFoldedTurns = new Set();
  traceFoldedAssistants = new Set();
  const body = document.getElementById('traceBody');
  const track = document.getElementById('traceTrack');
  if (body) body.innerHTML = '';
  if (track) track.innerHTML = '';
  closeTraceInspector(false);
  if (body && track && document.getElementById('btnFoldTurns') && document.getElementById('btnFoldCalls')) renderTrace();
}

function fmtMs(ms) {
  if (!ms) return '';
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.round((ms % 60000) / 1000) + 's';
}

function fmtRunDur(ms) {
  const sec = Math.floor(ms / 1000);
  const chinese = document.documentElement.lang === 'zh-CN';
  if (sec < 60) return sec + (chinese ? '\u79D2' : 's');
  return Math.floor(sec / 60) + (chinese ? '\u5206' : 'm') +
    String(sec % 60).padStart(2, '0') + (chinese ? '\u79D2' : 's');
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

function traceText(en, zh) {
  return typeof document !== 'undefined' && document.documentElement?.lang === 'zh-CN' ? zh : en;
}

function traceOwnerKey(ev) {
  return JSON.stringify([ev.sessionId || '', ev.turn || 0, ev.traceInvocationID || '',
    ev.traceParentInvocationID || '', ev.subAgentOf || '', ev.parentID || '', ev.depth || 0]);
}

function addTraceRawEvents(target, source) {
  target.rawEvents = [...(target.rawEvents || []), ...(source.rawEvents || [source])];
  if (source.truncated) target.truncated = true;
  if (source.limitation && !target.limitation) target.limitation = source.limitation;
}

function traceTimingRecord(ev) {
  if (ev.requestTiming && typeof ev.requestTiming === 'object') return { ...ev.requestTiming };
  const text = String(ev.text || '');
  try {
    const value = JSON.parse(text);
    if (value && typeof value === 'object' && value.phase) return value;
  } catch (_) { /* Older traces used a fixed, readable diagnostic string. */ }
  const record = { phase: text.split(/\s+/)[0] || 'unknown', at: ev.ts };
  const fields = { request: 'request_id', call: 'provider_call', attempt: 'attempt', elapsed_ms: 'elapsed_ms', outcome: 'outcome', status: 'status_code' };
  for (const [legacy, field] of Object.entries(fields)) {
    const match = new RegExp('(?:^|\\s)' + legacy + '=([^\\s]*)').exec(text);
    if (match) record[field] = ['provider_call', 'attempt', 'elapsed_ms', 'status_code'].includes(field) ? Number(match[1]) : match[1];
  }
  return record;
}

// Project streaming fragments without rewriting the raw ledger. Diagnostics
// are buffered independently by the runtime, so they cannot split a response.
function coalesceTraceSemanticEvents(events) {
  const display = [], diagnostics = new Map();
  let stream = null;
  for (const original of events) {
    const ev = { ...original };
    if (ev.kind === 'request_timing') {
      const timing = traceTimingRecord(ev);
      const key = traceOwnerKey(ev) + '|' + String(timing.request_id || 'legacy-unattributed');
      let group = diagnostics.get(key);
      if (!group) {
        group = { ...ev, requestTimingRecords: [], rawEvents: [] };
        diagnostics.set(key, group);
        display.push(group);
      }
      group.requestTimingRecords.push(timing);
      addTraceRawEvents(group, ev);
      continue;
    }
    if (ev.kind === 'text' || ev.kind === 'thinking') {
      const key = traceOwnerKey(ev) + '|' + ev.kind + '|' + (ev.source || '') + '|' + (ev.streamID || '') + '|' + (ev.requestID || '');
      if (stream && stream.key === key) {
        stream.event.text = String(stream.event.text || '') + String(ev.text || '');
        stream.event.endTS = ev.endTS || ev.ts;
        stream.event.sequenceEnd = ev.sequence;
        stream.event.legacyMerged = !ev.streamID;
        addTraceRawEvents(stream.event, ev);
        const duration = Date.parse(stream.event.endTS) - Date.parse(stream.event.ts);
        if (Number.isFinite(duration) && duration >= 0) stream.event.elapsedMs = duration;
      } else {
        display.push(ev);
        stream = { key, event: ev };
      }
      continue;
    }
    // Keep even invisible turn_end/loop_done records until after this pass:
    // they are real response boundaries for legacy traces without stream IDs.
    stream = null;
    display.push(ev);
  }
  return display;
}

// Runtime compaction can emit dozens of byte-progress samples. Keep those
// events in the saved trace, but present each lifecycle as one readable row.
function coalesceTraceCompactions(events) {
  const display = [];
  const active = new Map();
  const completed = new Map();
  const keyOf = traceOwnerKey;
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
      addTraceRawEvents(aggregate, ev);
      continue;
    }
    if (ev.kind === 'context_compacted') {
      let aggregate = active.get(key);
      const text = !ev.text || ev.text === 'auto' ? completeText : ev.text;
      if (aggregate) {
        const rawEvents = aggregate.rawEvents;
        Object.assign(aggregate, ev, { kind: 'context_compacted', text, rawEvents });
        addTraceRawEvents(aggregate, ev);
      }
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
        const rawEvents = aggregate.rawEvents;
        if (ev.isError) Object.assign(aggregate, ev, { kind: 'error', text: ev.text || 'Context compaction failed' });
        else {
          Object.assign(aggregate, ev, { kind: 'context_compacted', text: completeText });
          completed.set(key, aggregate);
        }
        aggregate.rawEvents = rawEvents;
        addTraceRawEvents(aggregate, ev);
        active.delete(key);
      } else if (ev.isError) {
        display.push(Object.assign({}, ev, { kind: 'error', text: ev.text || 'Context compaction failed' }));
      } else if (completed.has(key)) {
        addTraceRawEvents(completed.get(key), ev);
      }
      continue;
    }
    if (ev.kind === 'info' && /^context compacted\b/i.test(String(ev.text || ''))) {
      const aggregate = completed.get(key);
      if (aggregate) {
        const summary = String(ev.text || '').replace(/^context compacted\s*(?:\(auto\))?\s*:\s*/i, '');
        aggregate.text = completeText + (summary ? ' · ' + summary : '');
        addTraceRawEvents(aggregate, ev);
        continue;
      }
    }
    display.push(ev);
  }
  return display;
}

// Mirror the server's occurrence-aware legacy matching. Provider IDs and tool
// names can repeat; neither may attach a result to a different turn or owner.
function traceToolMatchRank(a, b) {
  if ((a.turn || 0) !== (b.turn || 0) ||
      (a.traceInvocationID && b.traceInvocationID && a.traceInvocationID !== b.traceInvocationID)) return 0;
  for (const field of ['sessionId', 'traceParentInvocationID', 'subAgentOf']) {
    if (a[field] && b[field] && a[field] !== b[field]) return 0;
  }
  if (a.traceCallID && b.traceCallID) return a.traceCallID === b.traceCallID ? 4 : 0;
  const aID = a.toolUseID || '';
  const bID = b.toolUseID || (b.kind === 'tool_result' ? b.parentID : '') || '';
  if (aID && bID) return aID === bID ? 3 : 0;
  if (a.toolName && a.toolName === b.toolName) return 2;
  if (!a.toolName && !aID) return 1;
  return 0;
}

function traceMatchTool(candidates, ev, eventOf = value => value) {
  let best = null, bestRank = 0, tied = false;
  for (const candidate of candidates) {
    const rank = traceToolMatchRank(eventOf(candidate), ev);
    if (rank > bestRank) { best = candidate; bestRank = rank; tied = false; }
    else if (rank > 0 && rank === bestRank) tied = true;
  }
  return tied ? null : best;
}

function inheritTraceToolIdentity(dst, src) {
  for (const key of ['toolName', 'toolUseID', 'traceInvocationID', 'traceCallID', 'sessionId', 'traceParentInvocationID', 'subAgentOf']) {
    if (!dst[key] && src[key]) dst[key] = src[key];
  }
}

// Older servers/transports can still expose argument fragments. Recover them
// in place, including calls which already have a result but never emitted a start.
function coalesceTraceToolArguments(events) {
  const active = new Set(), slots = [];
  for (const original of events) {
    const ev = Object.assign({}, original);
    if (ev.kind === 'tool_args' || ev.kind === 'tool_args_delta') {
      let partial = traceMatchTool(active, ev, value => value.event);
      if (!partial) {
        partial = { event: ev, text: '', consumed: false, resultSeen: false };
        active.add(partial);
        slots.push({ partial });
      } else addTraceRawEvents(partial.event, ev);
      partial.text += ev.text || '';
      inheritTraceToolIdentity(partial.event, ev);
      continue;
    }
    if (ev.kind === 'tool_start' || ev.kind === 'tool_result') {
      const partial = traceMatchTool(active, ev, value => value.event);
      if (partial) {
        active.delete(partial);
        inheritTraceToolIdentity(ev, partial.event);
        if (ev.kind === 'tool_start') {
          partial.consumed = true;
          addTraceRawEvents(ev, partial.event);
          if (partial.text && ['', '{}', 'null'].includes(String(ev.text || '').trim())) ev.text = partial.text;
        } else {
          partial.resultSeen = true;
          inheritTraceToolIdentity(partial.event, ev);
        }
      }
    }
    slots.push({ event: ev });
  }
  return slots.flatMap(slot => {
    const p = slot.partial;
    if (!p) return [slot.event];
    if (p.consumed || (!p.text && !p.resultSeen)) return [];
    return [Object.assign({}, p.event, { kind: 'tool_start', text: p.text })];
  });
}

function buildTraceRows(events, initialContext = null) {
  const originals = Array.isArray(events) ? events : [];
  const order = new Map(originals.map((ev, index) => [ev, index]));
  const decorated = originals.map(ev => ({ ...ev, rawEvents: [ev] }));
  const displayEvents = coalesceTraceToolArguments(coalesceTraceCompactions(coalesceTraceSemanticEvents(decorated)));
  const rows = [];
  const pendingTool = new Set();
  for (let i = 0; i < displayEvents.length; i++) {
    const ev = Object.assign({}, displayEvents[i]);
    if (ev.kind === 'turn_end' || ev.kind === 'loop_done') continue; // boundaries only

    if (ev.kind === 'tool_result') {
      const target = traceMatchTool(pendingTool, ev);
      if (target) {
        target.resultText = ev.text || '';
        target.resultIsError = !!ev.isError;
        target.hasResult = true;
        addTraceRawEvents(target, ev);
        target.resultIdx = i;
        if (ev.elapsedMs) target.elapsedMs = ev.elapsedMs;
        inheritTraceToolIdentity(target, ev);
        pendingTool.delete(target);
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
      source: ev.source || '',
      elapsedMs: ev.elapsedMs || 0,
      isError: !!ev.isError,
      depth: ev.depth || 0,
      parentID: ev.parentID || '',
      toolName: ev.toolName || '',
      toolUseID: ev.toolUseID || '',
      traceInvocationID: ev.traceInvocationID || '',
      traceParentInvocationID: ev.traceParentInvocationID || '',
      traceCallID: ev.traceCallID || '',
      sessionId: ev.sessionId || '',
      subAgentOf: ev.subAgentOf || '',
      streamID: ev.streamID || '',
      requestID: ev.requestID || '',
      endTS: ev.endTS || '',
      sequenceEnd: ev.sequenceEnd,
      legacyMerged: !!ev.legacyMerged,
      truncated: !!ev.truncated,
      limitation: ev.limitation || '',
      rawEvents: (ev.rawEvents || []).slice(),
      requestTimingRecords: ev.requestTimingRecords || [],
      images: Array.isArray(ev.images) ? ev.images.slice() : [],
      text: ev.kind === 'thinking_redacted'
        ? TRACE_REDACTED_THINKING_PLACEHOLDER
        : (ev.kind === 'tool_result' ? '' : (ev.text || '')),
      resultText: ev.kind === 'tool_result' ? (ev.text || '') : '',
      resultIsError: ev.kind === 'tool_result' && !!ev.isError,
      hasResult: ev.kind === 'tool_result',
      lane: meta.lane,
      tokens: null,
    };
    if (ev.kind === 'error' && ev.toolName) row.lane = 2;
    if (ev.kind === 'tool_start') {
      pendingTool.add(row);
    }
    if (ev.kind === 'tokens') {
      const m = /input=(\d+) output=(\d+)(?: cache_write=(\d+))?(?: cache_read=(\d+))?/.exec(ev.text || '');
      row.tokens = m
        ? { input: +m[1], output: +m[2], cacheWrite: +(m[3] || 0), cacheRead: +(m[4] || 0) }
        : { input: 0, output: 0, cacheWrite: 0, cacheRead: 0 };
    }
    rows.push(row);
  }
  for (const row of rows) {
    row.rawEvents = [...new Set(row.rawEvents)].sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0))
      .map(ev => ev.kind === 'thinking_redacted'
        ? { id: ev.id, kind: ev.kind, turn: ev.turn, sequence: ev.sequence, ts: ev.ts, text: TRACE_REDACTED_THINKING_PLACEHOLDER }
        : ev);
  }
  if (initialContext && typeof initialContext === 'object') {
    const snapshot = { kind: 'initial_context', turn: null, contextSnapshot: initialContext, rawEvents: [], images: [],
      text: traceContextLabel(initialContext), lane: 0, ts: '', elapsedMs: 0, tokens: null, source: initialContext.source || '' };
    const firstUser = rows.findIndex(row => row.kind === 'user');
    rows.splice(firstUser < 0 ? 0 : firstUser, 0, snapshot);
  }
  rows.forEach((row, index) => { row.idx = index; });
  return rows;
}

function isToolRow(r) {
  return r.kind === 'tool_start' || r.kind === 'tool_result';
}

function traceContextLabel(snapshot = {}) {
  if (!snapshot.available) return traceText('Request context unavailable', '请求上下文未记录');
  const turn = Number(snapshot.turn) || 1;
  return turn > 1
    ? traceText('First recorded request · Turn ', '首个已记录请求 · 第 ') + turn + traceText('', ' 轮')
    : traceText('Initial request · System and tools', '初始请求 · System 与 Tools');
}

function traceOwnerLabel(row) {
  if (row.subAgentOf) return traceText('Child ', '子 Agent ') + row.subAgentOf;
  if (row.depth > 0 && row.traceInvocationID) return traceText('Child ', '子 Agent ') + row.traceInvocationID;
  if (row.depth > 0) return traceText('Nested event · depth ', '嵌套事件 · 深度 ') + row.depth;
  return '';
}

function traceMilliseconds(value) {
  return Number(value) === 0 ? '0ms' : fmtMs(Number(value));
}

function traceTimingMetrics(row) {
  const records = row.requestTimingRecords || [];
  const first = phase => records.filter(record => record.phase === phase && Number.isFinite(Number(record.elapsed_ms)))
    .sort((a, b) => Number(a.elapsed_ms) - Number(b.elapsed_ms))[0];
  return { text: first('first_nonempty_text_delta'), reasoning: first('first_reasoning_delta'),
    byte: first('http_first_response_byte'), duration: Math.max(0, ...records.map(record => Number(record.elapsed_ms) || 0)) };
}

function traceDiagnosticSummary(row) {
  const records = row.requestTimingRecords || [];
  const request = records.find(record => record.request_id)?.request_id;
  const metrics = traceTimingMetrics(row);
  let text = request ? traceText('Request ', '请求 ') + request : traceText('Legacy timing · request identity unavailable', '旧版诊断 · 请求身份未记录');
  text += ' · ' + records.length + traceText(' phases', ' 个阶段');
  if (metrics.text) text += ' · TTFT ' + traceMilliseconds(metrics.text.elapsed_ms);
  else if (metrics.byte) text += ' · ' + traceText('First byte ', '首字节 ') + traceMilliseconds(metrics.byte.elapsed_ms);
  if (metrics.duration) text += ' · ' + traceMilliseconds(metrics.duration);
  return text;
}

function traceDiagnosticsHtml(row) {
  const records = row.requestTimingRecords || [];
  const groups = new Map();
  for (const record of records) {
    const key = (Number(record.provider_call) || 0) + ':' + (Number(record.attempt) || 0);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(record);
  }
  const metrics = traceTimingMetrics(row);
  let html = '<div class="tt-diagnostic-note">' + escHtml(traceText(
    'Local request diagnostics. TTFT is request start to first text; first-byte and phase offsets share the request clock. These records do not change turn duration or usage.',
    '本地请求诊断。TTFT 从请求开始计至首段文字；首字节与各阶段偏移使用同一请求时钟。这些记录不改变轮次耗时和用量。')) + '</div>';
  if (metrics.text) html += '<div class="tt-diagnostic-metric">TTFT ' + escHtml(traceMilliseconds(metrics.text.elapsed_ms)) + '</div>';
  for (const records of groups.values()) {
    const first = records[0];
    const label = Number(first.provider_call) > 0
      ? traceText('Call ', '调用 ') + first.provider_call + ' · ' + traceText('Attempt ', '尝试 ') + (Number(first.attempt) || 0)
      : traceText('Request preparation', '请求准备');
    html += '<details class="tt-diagnostic-call"><summary>' + escHtml(label) + ' · ' + records.length + traceText(' phases', ' 个阶段') + '</summary>' +
      '<ol class="tt-diagnostic-phases">' + records.map(record => '<li><code>' + escHtml(record.phase || 'unknown') + '</code><span>+' +
        escHtml(traceMilliseconds(record.elapsed_ms || 0)) + '</span>' +
        (record.outcome || record.status_code ? '<small>' + escHtml([record.outcome, record.status_code || ''].filter(Boolean).join(' · ')) + '</small>' : '') + '</li>').join('') + '</ol></details>';
  }
  return html;
}

function traceRawEventsHtml(row) {
  if (!(row.rawEvents || []).length) return '<p class="tt-no-payload">' + escHtml(traceText('No original event recorded', '没有原始事件记录')) + '</p>';
  return '<div class="tt-raw-events">' + row.rawEvents.map(event => '<details><summary>' +
    escHtml('#' + (event.sequence ?? '?') + ' · ' + event.kind + (event.ts ? ' · ' + fmtTimestamp(event.ts) : '')) +
    '</summary><pre class="tt-payload">' + escHtml(JSON.stringify(event, null, 2)) + '</pre></details>').join('') + '</div>';
}

function traceImageSource(image) {
  if (!image || image.available === false) return '';
  const source = String(image.url || '');
  if (!source.startsWith('/api/trace/image?') || /[\\\u0000-\u0020]/.test(source)) return '';
  try {
    const url = new URL(source, 'http://metis.local');
    if (url.origin !== 'http://metis.local' || url.pathname !== '/api/trace/image' || url.hash ||
        !url.searchParams.get('session_id') || !url.searchParams.get('hash')) return '';
    return source;
  } catch (_) { return ''; }
}

function traceImagesHtml(row) {
  const images = Array.isArray(row.images) ? row.images : [];
  if (!images.length) return row.kind === 'user' && /\[image attachment\]/i.test(row.text || '')
    ? '<p class="tt-attachment-note">' + escHtml(traceText('The image preview is unavailable in this older trace; the original attachment could not be uniquely recovered.', '这条旧轨迹无法唯一恢复原附件，图片预览不可用。')) + '</p>' : '';
  return '<div class="tt-image-gallery">' + images.map((image, index) => {
    const source = traceImageSource(image), name = image.name || traceText('Image ', '图片 ') + (index + 1);
    const dimensions = Number(image.width) > 0 && Number(image.height) > 0 ? image.width + ' × ' + image.height : '';
    const metadata = [dimensions, Number(image.bytes) > 0 ? Math.ceil(image.bytes / 1024) + ' KB' : '', image.mediaType || ''].filter(Boolean).join(' · ');
    const caption = '<span class="tt-image-caption"><span>' + escHtml(name) + '</span><small>' + escHtml(metadata) + '</small></span>';
    if (!source) return '<span class="tt-image-card tt-image-unavailable"><span class="tt-image-placeholder" aria-hidden="true">▧</span>' + caption +
      '<small class="tt-image-limitation">' + escHtml(image.limitation || traceText('Preview unavailable', '预览不可用')) + '</small></span>';
    return '<button type="button" class="tt-image-card" data-trace-image-src="' + escAttr(source) + '" data-trace-image-name="' + escAttr(name) +
      '" aria-label="' + escAttr(traceText('Preview ', '预览 ') + name) + '" onclick="event.stopPropagation();openTraceImagePreview(this)">' +
      '<img src="' + escAttr(source) + '" alt="' + escAttr(name) + '" width="48" height="48" loading="lazy" decoding="async" onerror="traceImageFailed(this)">' + caption + '</button>';
  }).join('') + '</div>';
}

function traceImageFailed(image) {
  if (!image) return;
  image.hidden = true;
  const card = image.closest('.tt-image-card');
  if (card && !card.querySelector('.tt-image-limitation')) {
    const note = document.createElement('small');
    note.className = 'tt-image-limitation';
    note.textContent = traceText('Preview unavailable', '预览不可用');
    card.appendChild(note);
  }
}

function openTraceImagePreview(trigger) {
  const source = traceImageSource({ url: trigger?.dataset.traceImageSrc });
  if (!source) return;
  closeTraceImagePreview(false);
  const overlay = document.createElement('div');
  overlay.className = 'tt-image-overlay';
  overlay.setAttribute('role', 'dialog');
  overlay.setAttribute('aria-modal', 'true');
  overlay.setAttribute('aria-label', traceText('Image preview', '图片预览'));
  const name = trigger.dataset.traceImageName || traceText('Attached image', '附加图片');
  overlay.innerHTML = '<div class="tt-image-dialog"><header><span>' + escHtml(name) + '</span><button type="button" aria-label="' +
    escAttr(traceText('Close image preview', '关闭图片预览')) + '">×</button></header><div class="tt-image-stage"><img alt=""></div></div>';
  const image = overlay.querySelector('img'), close = overlay.querySelector('button');
  image.src = source;
  image.alt = name;
  image.addEventListener('error', () => {
    image.hidden = true;
    const note = document.createElement('p');
    note.textContent = traceText('The image preview is unavailable.', '图片预览不可用。');
    image.parentElement.appendChild(note);
  }, { once: true });
  close.addEventListener('click', () => closeTraceImagePreview());
  overlay.addEventListener('click', event => { if (event.target === overlay) closeTraceImagePreview(); });
  overlay.addEventListener('keydown', event => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); closeTraceImagePreview(); }
    if (event.key === 'Tab') { event.preventDefault(); event.stopPropagation(); close.focus(); }
  });
  traceImagePreview = { overlay, trigger, sessionId: String(currentSessionId || '') };
  document.body.appendChild(overlay);
  close.focus();
}

function closeTraceImagePreview(restoreFocus = true) {
  if (!traceImagePreview) return;
  const { overlay, trigger } = traceImagePreview;
  traceImagePreview = null;
  overlay.remove();
  if (restoreFocus && trigger?.isConnected) trigger.focus();
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
  if (r.kind === 'initial_context') return traceContextLabel(r.contextSnapshot);
  if (r.kind === 'request_timing') return traceDiagnosticSummary(r);
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
  const hay = (rowText(r) + ' ' + (r.toolName || '') + ' ' + metaOf(r.kind).label + ' ' + (r.text || '') + ' ' +
    traceOwnerLabel(r) + ' ' + traceOwnerKey(r) + ' ' + JSON.stringify(r.requestTimingRecords || []) + ' ' + JSON.stringify(r.contextSnapshot || {})).toLowerCase();
  return hay.includes(traceSearchQuery);
}

function traceRowHasResult(r) {
  return !!(r.hasResult || r.kind === 'tool_result' || r.resultText || r.resultIsError);
}

function rowState(r) {
  if (r.kind === 'request_timing') return 'complete'; // diagnostics never decide business outcome
  if (r.isError || r.resultIsError) return 'error';
  if (isToolRow(r) && r.kind === 'tool_start' && !traceRowHasResult(r)) return 'running';
  return 'complete';
}

function statusLabel(state) {
  return state === 'error' ? 'Failed' : state === 'running' ? 'Pending' : 'Completed';
}

// --- Loading ------------------------------------------------------------

function traceEventKey(ev) {
  if (ev.id) return 'id:' + ev.id;
  return [traceOwnerKey(ev), ev.sequence, ev.ts, ev.kind, ev.streamID, ev.toolUseID, ev.parentID, ev.text].join('|');
}

function traceRowStableKey(row) {
  if (!row) return '';
  const owner = traceOwnerKey(row);
  if (row.kind === 'initial_context') {
    const snapshot = row.contextSnapshot || {};
    return 'context|' + [snapshot.available, snapshot.requestID || '', snapshot.capturedAt || '', snapshot.turn || 0].join('|');
  }
  if (row.kind === 'request_timing') return owner + '|timing|' + ((row.requestTimingRecords || []).find(record => record.request_id)?.request_id || 'legacy-unattributed');
  if (row.streamID) return owner + '|' + row.kind + '|stream:' + row.streamID;
  if (row.traceCallID) return owner + '|tool:' + row.traceCallID;
  const first = row.rawEvents?.[0];
  return first ? owner + '|' + row.kind + '|' + traceEventKey(first) : '';
}

function traceSelectionSnapshot() {
  const describe = row => row ? { key: traceRowStableKey(row), owner: traceOwnerKey(row), kind: row.kind,
    records: new Set((row.rawEvents || []).map(traceEventKey)) } : null;
  return { sessionId: String(traceSessionId || currentSessionId || ''), request: traceSelectedReq,
    row: describe(traceRows[traceSelectedIdx]),
    folded: traceRows.filter(row => traceFoldedAssistants.has(row.idx)).map(describe) };
}

function restoreTraceSelection(snapshot) {
  traceSelectedIdx = -1;
  traceSelectedReq = -1;
  if (!snapshot || snapshot.sessionId !== String(traceSessionId || currentSessionId || '')) return;
  const find = saved => {
    if (!saved) return -1;
    const exact = saved.key ? traceRows.findIndex(row => traceRowStableKey(row) === saved.key) : -1;
    if (exact >= 0) return exact;
    // Loading an earlier page can extend a legacy row backwards and change
    // its first ID. Shared original records still identify the same object.
    return traceRows.findIndex(row => row.kind === saved.kind && traceOwnerKey(row) === saved.owner &&
      (row.rawEvents || []).some(event => saved.records.has(traceEventKey(event))));
  };
  traceSelectedIdx = find(snapshot.row);
  if (traceSelectedIdx < 0 && snapshot.request >= 0 && traceRows.some(row => row.turn === snapshot.request)) traceSelectedReq = snapshot.request;
  traceFoldedAssistants = new Set((snapshot.folded || []).map(find).filter(index => index >= 0).map(index => traceRows[index].idx));
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

async function loadTrace(loadOlder = false, sessionId = currentSessionId, shouldApply = () => true) {
  const requestedSessionId = String(sessionId || '');
  if (!shouldApply() || requestedSessionId !== String(currentSessionId || '')) return;
  if (traceSessionId !== requestedSessionId) resetTraceForSession();
  if (loadOlder && (!traceNextCursor || traceLoadingOlder)) return;
  const generation = loadOlder ? traceLoadGeneration : ++traceLoadGeneration;
  const requestedCursor = loadOlder ? traceNextCursor : '';
  const isLatest = () => generation === traceLoadGeneration &&
    shouldApply() && requestedSessionId === String(currentSessionId || '');
  const body = document.getElementById('traceBody');
  const track = document.getElementById('traceTrack');
  traceLoadingOlder = loadOlder;
  if (!loadOlder && !traceRows.length && body) {
    body.innerHTML = '<tr><td colspan="2"><div class="tt-empty">Loading trajectory…</div></td></tr>';
  }
  try {
    const params = new URLSearchParams({ limit: '500' });
    if (requestedSessionId) params.set('sessionId', requestedSessionId);
    if (loadOlder) params.set('cursor', requestedCursor);
    const url = '/api/trace?' + params.toString();
    const res = await fetch(url);
    if (!res.ok) throw new Error('trace: ' + res.status);
    const data = await res.json();
    if (!isLatest()) return;
    const selection = traceSelectionSnapshot();
    if (!loadOlder) {
      traceNextCursor = '';
      traceTotalEvents = 0;
    }
    if (!data.enabled) {
      resetTraceForSession();
      track.innerHTML = '';
      body.innerHTML = '<tr><td colspan="2"><div class="tt-empty">Session tracing is not enabled for this process.</div></td></tr>';
      closeTraceInspector(false);
      return;
    }
    traceEvents = loadOlder
      ? mergeTraceEvents(data.events || [], traceEvents)
      : (data.events || []);
    traceInitialContext = data.initialContext || traceInitialContext || { available: false,
      limitation: traceText('No initial system or tool snapshot was recorded for this session.', '此会话未记录初始 System 或 Tools 快照。') };
    traceNextCursor = data.nextCursor || '';
    traceTotalEvents = Number(data.totalEvents) || traceEvents.length;
    traceRows = buildTraceRows(traceEvents, traceInitialContext);
    restoreTraceSelection(selection);
    renderTrace();
  } catch (e) {
    if (!isLatest()) return;
    if (traceRows.length) {
      renderTrace();
      if (typeof showToast === 'function') showToast(traceText('Unable to refresh trajectory: ', '无法刷新轨迹：') + e.message);
      return;
    }
    if (!loadOlder) resetTraceForSession();
    track.innerHTML = '';
    body.innerHTML = '<tr><td colspan="2"><div class="tt-empty">Failed to load trajectory: ' + escHtml(e.message) + '</div></td></tr>';
    closeTraceInspector(false);
  } finally {
    if (generation === traceLoadGeneration) traceLoadingOlder = false;
  }
}

// --- Timeline strip -----------------------------------------------------

function spanKindOf(r) {
  if (r.isError) return 'model';
  const m = metaOf(r.kind);
  if (r.kind === 'user') return 'user';
  if (r.kind === 'context' || r.kind === 'context_warn' || r.kind === 'context_compacted') return 'context';
  if (m.lane === 2) return 'tool';
  return 'model';
}

function renderTimeline() {
  const track = document.getElementById('traceTrack');
  const timedRows = traceRows.map((row, index) => ({ row, index }))
    .filter(({ row }) => row.kind !== 'request_timing' && row.kind !== 'initial_context');
  if (!timedRows.length) {
    track.innerHTML = '<span class="tt-track-empty">No timing data</span>';
    return;
  }
  let html = '';
  const times = timedRows.map(({ row }) => Date.parse(row.ts));
  const finite = timedRows.flatMap(({ row }) => [Date.parse(row.ts), Date.parse(row.endTS)]).filter(Number.isFinite);
  const min = finite.length ? Math.min(...finite) : 0;
  const max = finite.length ? Math.max(...finite) : 1;
  const span = Math.max(max - min, 1);

  timedRows.forEach(({ row: r, index }, i) => {
    const isErr = r.isError || r.resultIsError;
    const kind = spanKindOf(r);
    let left, width;
    if (!traceDurMode) {
      left = (i / timedRows.length) * 100;
      width = 8;
    } else {
      const t = times[i];
      if (!Number.isFinite(t)) return;
      left = ((t - min) / span) * 100;
      width = Math.max((r.elapsedMs / span) * 100, 0.25);
    }
    const cur = index === traceSelectedIdx ? 'true' : undefined;
    html += `<span class="tt-span" data-kind="${kind}"${isErr ? ' data-error="true"' : ''}${cur ? ' data-current="true"' : ''} title="${escAttr(metaOf(r.kind).label + (r.toolName ? ' ' + r.toolName : '') + (r.elapsedMs ? ' (' + fmtMs(r.elapsedMs) + ')' : ''))}" style="--lane:${r.lane};left:${left.toFixed(2)}%;width:${Math.max(width, 2).toFixed(2)}px" onclick="selectTraceRow(${index}, true)"></span>`;
  });

  // Turn boundaries: a 1px tick where each turn starts.
  let lastTurn = null;
  timedRows.forEach(({ row: r }, i) => {
    if (r.turn !== lastTurn) {
      lastTurn = r.turn;
      let left;
      if (!traceDurMode) left = (i / timedRows.length) * 100;
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
    const turnStart = r.kind !== 'initial_context' && r.turn !== curTurn;
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

    html += `<tr data-kind="${isToolRow(r) ? (sub ? 'subtool' : 'tool') : r.kind}"${sub ? ' data-sub="true"' : ''}${turnStart ? ' data-turn-start="true"' : ''}${isErr ? ' data-error="true"' : ''}${sel ? ' data-selected="true"' : ''} tabindex="0" onclick="selectTraceRow(${i}, false)" onkeydown="if(event.target===this&&event.key==='Enter'){event.preventDefault();selectTraceRow(${i}, false)}">
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
  if (r.kind === 'request_timing') return '<details class="tt-diagnostics" onclick="event.stopPropagation()"><summary>' + escHtml(traceDiagnosticSummary(r)) + '</summary>' + traceDiagnosticsHtml(r) + '</details>';
  if (r.kind === 'initial_context') return '<span class="tt-content-text tt-context-label">' + escHtml(traceContextLabel(r.contextSnapshot)) + '</span>' +
    '<small class="tt-context-hint">' + escHtml(r.contextSnapshot.available
      ? traceText('Captured logical request · inspect System and Tools', '已捕获逻辑请求 · 点击查看 System 与 Tools')
      : r.contextSnapshot.limitation || traceText('No historical snapshot was recorded', '未记录历史快照')) + '</small>';
  if (isToolRow(r)) {
    const name = r.toolName ? `<span class="tt-tool-name">${escHtml(r.toolName)}</span>` : '';
    const args = oneLine(r.text);
    const argsHtml = args ? `<span class="tt-tool-args">${escHtml(args)}</span>` : '';
    const head = `<span class="tt-content-text">${name}${argsHtml}</span>`;
    const hasResult = traceRowHasResult(r);
    if (!hasResult) return head;
    const resText = oneLine(r.resultText) || '(no output)';
    return `<span class="tt-result-preview"><span class="tt-result-request">${head}</span><span class="tt-inline-result${r.resultIsError ? ' err' : ''}"><span class="arrow">\u2192</span><span class="tt-inline-result-text">${escHtml(resText)}</span></span></span>`;
  }
  const text = rowText(r);
  const owner = traceOwnerLabel(r);
  return (owner ? '<small class="tt-owner-label">' + escHtml(owner) + '</small>' : '') +
    `<span class="tt-content-text${r.isError ? ' is-error' : ''}">${escHtml(text)}</span>` + traceImagesHtml(r);
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
  const events = ['events', traceText('Raw events', '原始事件')];
  if (r.kind === 'initial_context') return [['summary', traceText('Summary', '概览')], ['system', 'System'], ['tools', 'Tools'], ['raw', traceText('Raw', '原始数据')]];
  if (r.kind === 'request_timing') return [['summary', traceText('Summary', '概览')], ['timing', traceText('Phases', '阶段')], events];
  if (isToolRow(r)) return [['summary', 'Summary'], ['payload', 'Payload'], ['result', 'Result'], ['timing', 'Timing'], events];
  if (r.kind === 'thinking_redacted') return [['summary', 'Summary']];
  if (r.kind === 'user' || r.kind === 'thinking' || r.kind === 'text' || r.kind === 'plan') {
    return [['summary', 'Summary'], ['preview', 'Preview'], ['raw', 'Raw'], events];
  }
  if (r.kind === 'tokens') return [['summary', 'Summary'], ['raw', 'Raw'], events];
  return [['summary', 'Summary'], ['raw', 'Raw'], events];
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
  const location = r.kind === 'initial_context' ? traceContextLabel(r.contextSnapshot)
    : `Turn ${r.turn}${traceOwnerLabel(r) ? ' · ' + traceOwnerLabel(r) : ''}${r.source ? ' \u00B7 ' + r.source : ''}${r.toolName ? ' \u00B7 ' + r.toolName : ''}`;
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
    if (row.kind === 'initial_context') payload = traceTab === 'system' ? row.contextSnapshot.system || '' : JSON.stringify(traceTab === 'tools' ? row.contextSnapshot.tools || [] : row.contextSnapshot, null, 2);
    else if (traceTab === 'events') payload = JSON.stringify(row.rawEvents, null, 2);
    else if (row.kind === 'thinking_redacted') payload = TRACE_REDACTED_THINKING_PLACEHOLDER;
    else if (traceTab === 'result') payload = formatTracePayload(row.resultText);
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
  const snapshot = r.kind === 'initial_context' ? r.contextSnapshot || {} : null;
  const incomplete = !!(r.truncated || snapshot?.truncated);
  const limitation = snapshot?.available ? snapshot.limitation : r.limitation;
  const note = incomplete
    ? traceText('Incomplete capture. ', '记录不完整。') + (limitation || traceText('The saved stream exceeded its capture limit; retained records do not contain the omitted content.', '已保存的流超过捕获上限，保留的记录不含被省略的内容。'))
    : limitation || '';
  return (note ? '<p class="tt-capture-limitation" role="note">' + escHtml(note) + '</p>' : '') + traceInspectorBody(r, tab);
}

function traceInspectorBody(r, tab) {
  const state = rowState(r);
  if (tab === 'events') return traceRawEventsHtml(r);
  if (r.kind === 'initial_context') {
    const snapshot = r.contextSnapshot || {};
    const unavailable = snapshot.limitation || traceText('No request snapshot was recorded. Current settings cannot reconstruct historical system prompts or tools.', '未记录请求快照，当前配置无法还原当时的 System 与 Tools。');
    if (tab === 'system') return snapshot.available
      ? '<pre class="tt-payload">' + escHtml(snapshot.system || traceText('(empty system prompt)', '（System 为空）')) + '</pre>'
      : '<p class="tt-no-payload">' + escHtml(unavailable) + '</p>';
    if (tab === 'tools') return snapshot.available
      ? '<div class="tt-snapshot-tools">' + (snapshot.tools || []).map(tool => '<details><summary>' + escHtml(tool.name || 'Tool') + '</summary><p>' +
          escHtml(tool.description || '') + '</p><pre class="tt-payload">' + escHtml(JSON.stringify(tool.inputSchema || {}, null, 2)) + '</pre></details>').join('') +
          (!(snapshot.tools || []).length ? '<p class="tt-no-payload">' + escHtml(traceText('No tools in this recorded request', '此已记录请求未包含 Tools')) + '</p>' : '') + '</div>'
      : '<p class="tt-no-payload">' + escHtml(unavailable) + '</p>';
    if (tab === 'raw') return '<pre class="tt-payload">' + escHtml(JSON.stringify(snapshot, null, 2)) + '</pre>';
    return '<dl class="tt-overview">' + kvRow(traceText('Snapshot', '快照'), escHtml(traceContextLabel(snapshot))) +
      kvRow(traceText('Source', '来源'), escHtml(snapshot.source || traceText('Not recorded', '未记录'))) +
      (snapshot.available ? kvRow(traceText('Captured', '捕获时间'), snapshot.capturedAt ? fmtTimestamp(snapshot.capturedAt) : traceText('Not recorded', '未记录')) +
        kvRow(traceText('Recorded tools', '已记录 Tools'), String((snapshot.tools || []).length)) : '') +
      (snapshot.model ? kvRow(traceText('Model', '模型'), escHtml(snapshot.model)) : '') + '</dl>' +
      '<p class="tt-diagnostic-note">' + escHtml(snapshot.available
        ? traceText('Captured logical request before transport serialization; this is not a saved HTTP wire body.', '这是传输序列化前捕获的逻辑请求快照，并非保存的 HTTP 请求体。')
        : unavailable) + '</p>' +
      (snapshot.turn > 1 ? '<p class="tt-diagnostic-note">' + escHtml(traceText('Earlier request snapshots were not recorded. This is the first available capture, from turn ', '更早轮次未记录快照；这里展示第一个可用记录，捕获于第 ') + snapshot.turn + traceText('.', ' 轮。')) + '</p>' : '');
  }
  if (r.kind === 'request_timing' && (tab === 'timing' || tab === 'summary')) {
    return '<h4 class="tt-diagnostic-heading">' + escHtml(traceDiagnosticSummary(r)) + '</h4>' + traceDiagnosticsHtml(r);
  }
  if (tab === 'summary') {
    let html = `<dl class="tt-overview">`;
    html += kvRow('Status', `<span class="${state === 'error' ? 'err' : ''}">${statusLabel(state)}</span>`);
    html += kvRow('Turn', String(r.turn));
    if (r.source) html += kvRow('Source', escHtml(r.source));
    if (traceOwnerLabel(r)) html += kvRow(traceText('Owner', '归属'), escHtml(traceOwnerLabel(r)));
    if (r.traceInvocationID) html += kvRow('Invocation', escHtml(r.traceInvocationID));
    if (r.streamID) html += kvRow(traceText('Response', '响应'), escHtml(r.streamID));
    if (r.source && r.source.startsWith('history-reconstructed')) html += kvRow('Record', 'Recovered from saved history; original timestamp unavailable');
    html += kvRow('Started', fmtTimestamp(r.ts));
    html += kvRow('Duration', r.elapsedMs ? fmtMs(r.elapsedMs) : '\u2014');
    html += kvRow('Sequence', '#' + r.seq + (r.sequenceEnd && r.sequenceEnd !== r.seq ? '–#' + r.sequenceEnd : ''));
    if ((r.rawEvents || []).length > 1) html += kvRow(traceText('Original records', '原始记录'), String(r.rawEvents.length));
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
    if (r.legacyMerged) html += '<p class="tt-diagnostic-note">' + escHtml(traceText('Adjacent legacy fragments were grouped. Their original response identity was not recorded; the raw events remain available.', '已合并相邻旧版片段；原始响应身份未记录，可在原始事件中查看各片段。')) + '</p>';
    html += traceImagesHtml(r);
    return html;
  }
  if (tab === 'preview') {
    return traceImagesHtml(r) + `<div class="tt-markdown">${formatContent(r.text)}</div>`;
  }
  if (tab === 'raw') {
    return `<pre class="tt-payload${state === 'error' ? ' err' : ''}">${escHtml(formatTracePayload(r.text))}</pre>`;
  }
  if (tab === 'payload') {
    if (!r.text) return '<p class="tt-no-payload">No payload captured</p>';
    return `<pre class="tt-payload${r.isError ? ' err' : ''}">${escHtml(formatTracePayload(r.text))}</pre>`;
  }
  if (tab === 'result') {
    if (!traceRowHasResult(r)) return '<p class="tt-no-payload">No result captured</p>';
    const cls = r.resultIsError ? ' err' : (r.resultText === 'No output' ? ' no-output' : '');
    return `<pre class="tt-payload${cls}">${escHtml(formatTracePayload(r.resultText || '(no output)'))}</pre>`;
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
  const ts = tr.filter(x => x.kind !== 'request_timing').flatMap(x => [Date.parse(x.ts), Date.parse(x.endTS)]).filter(Number.isFinite);
  const started = ts.length ? new Date(Math.min(...ts)).toISOString() : null;
  const dur = ts.length > 1 ? Math.max(...ts) - Math.min(...ts) : 0;
  const hasError = tr.some(x => rowState(x) === 'error');
  const state = hasError ? 'error' : (tr.some(x => rowState(x) === 'running') ? 'running' : 'complete');
  const reqNo = [...new Set(traceRows.filter(x => x.turn !== null).map(x => x.turn))].indexOf(turn) + 1;
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
