// A child reader owns its requests, viewport and disclosures. It never activates
// the child's runtime or borrows the parent transcript's mutable rendering state.
let subAgentCatalogState = { owner: '', agents: [], nextCursor: null, loading: false, loaded: false, error: '', generation: 0, checkedAt: 0 };
let subAgentConversationState = null;
let subAgentConversationFrame = 0;
const subAgentReadingMemory = new Map();
let subAgentCatalogClock = null;

function subAgentViewCurrent(state) {
  return !!state && state === subAgentConversationState &&
    state.owner === String(currentSessionId || '') &&
    state.owner === subAgentDetailState.ownerSessionId && state.agentId === subAgentDetailState.agentId;
}

function subAgentCatalogAgents() {
  const owner = String(currentSessionId || '');
  const map = new Map();
  if (subAgentCatalogState.owner === owner) subAgentCatalogState.agents.forEach(item => {
    if (String(item.sessionId || '') === owner) map.set(String(item.agentId), item);
  });
  statusSubAgentsForSelectedSession(lastStatusSnapshot || {}).forEach(item => {
    const previous = map.get(String(item.agentId)) || {};
    const priorTerminal = ['completed', 'failed', 'killed'].includes(previous.status);
    const sameRun = !previous.startedAt || !item.startedAt || previous.startedAt === item.startedAt;
    map.set(String(item.agentId), { ...previous, ...item,
      status: priorTerminal && sameRun && ['queued', 'running'].includes(item.status) ? previous.status : item.status });
  });
  if (subAgentDetailState.ownerSessionId === owner && subAgentDetailState.data) {
    const item = subAgentDetailState.data;
    map.set(String(item.agentId), { ...(map.get(String(item.agentId)) || {}), ...item });
  }
  return [...map.values()].filter(item => item.agentId).sort((a, b) => {
    const active = item => ['queued', 'running'].includes(item.status) ? 0 : 1;
    return active(a) - active(b) || (Date.parse(b.startedAt) || 0) - (Date.parse(a.startedAt) || 0) || String(a.agentId).localeCompare(String(b.agentId));
  });
}

function syncSubAgentCatalog() {
  const owner = String(currentSessionId || '');
  if (subAgentCatalogState.owner !== owner) {
    subAgentCatalogState.abort?.abort();
    subAgentCatalogState = { owner, agents: [], nextCursor: null, loading: false, loaded: false,
      error: '', generation: subAgentCatalogState.generation + 1, checkedAt: 0 };
  }
  if (owner && !subAgentCatalogState.loading && Date.now() - subAgentCatalogState.checkedAt >= 3000) void refreshSubAgentCatalog();
}

async function refreshSubAgentCatalog(append = false) {
  const state = subAgentCatalogState, owner = String(currentSessionId || '');
  if (!owner || state.owner !== owner || state.loading || append && !state.nextCursor) return;
  const generation = ++state.generation;
  state.loading = true;
  state.checkedAt = Date.now();
  state.abort = typeof AbortController === 'function' ? new AbortController() : null;
  const cursor = append ? state.nextCursor : null;
  try {
    const response = await fetch('/api/subagents?sessionId=' + encodeURIComponent(owner) + '&limit=50' +
      (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''), { cache: 'no-store', signal: state.abort?.signal });
    const data = await response.json();
    if (state !== subAgentCatalogState || owner !== String(currentSessionId || '') || generation !== state.generation) return;
    if (!response.ok || data.sessionId !== owner || !Array.isArray(data.agents)) throw new Error('catalog unavailable');
    const wasLoaded = state.loaded;
    const entries = new Map(state.agents.map(item => [String(item.agentId), item]));
    data.agents.forEach(item => { if (String(item.sessionId || '') === owner && item.agentId) entries.set(String(item.agentId), item); });
    state.agents = [...entries.values()];
    if (append || !wasLoaded) state.nextCursor = data.hasMore ? data.nextCursor : null;
    state.loaded = true;
    state.error = '';
  } catch (error) {
    if (state !== subAgentCatalogState || generation !== state.generation || error?.name === 'AbortError') return;
    state.error = subAgentText('Unable to load agent history.', '暂时无法读取子代理历史。');
  } finally {
    if (state === subAgentCatalogState && generation === state.generation) {
      state.loading = false;
      if (typeof renderStatusSnapshot === 'function' && lastStatusSnapshot) renderStatusSnapshot(lastStatusSnapshot);
      else renderStatusPopover();
    }
  }
}

function subAgentUsageLabel(usage) {
  if (!usage || usage.available !== true || !Number.isSafeInteger(usage.totalTokens) || usage.totalTokens < 0) return '—';
  const value = usage.totalTokens;
  const compact = value >= 1000000 ? (value / 1000000).toFixed(1).replace(/\.0$/, '') + 'M'
    : value >= 1000 ? (value / 1000).toFixed(1).replace(/\.0$/, '') + 'K' : String(value);
  return compact + ' tok';
}

function subAgentConversationIdentity() {
  return String(subAgentDetailState.ownerSessionId || '') + ':' + String(subAgentDetailState.agentId || '');
}

function beginSubAgentConversationView(sameAgent) {
  if (sameAgent && subAgentViewCurrent(subAgentConversationState)) return;
  cancelSubAgentConversationView();
  const key = subAgentConversationIdentity();
  const remembered = subAgentReadingMemory.get(key);
  const state = subAgentConversationState = {
    key, owner: subAgentDetailState.ownerSessionId, agentId: subAgentDetailState.agentId,
    messages: new Map(), total: remembered?.total || 0, segments: [], liveTools: [], revision: '', requestedRevision: '',
    pending: false, error: '', loaded: false, follow: remembered?.anchor?.follow !== false,
    anchor: remembered?.anchor || null, expanded: remembered?.expanded || new Set(),
    projectionEpoch: 0, syntheticKeys: new Map(),
    generation: 0, viewport: null, units: [], infoOpen: false, requestTimer: null, fallbackTimer: null,
  };
  const body = document.getElementById('subAgentDetailBody');
  if (body) body.replaceChildren();
  const info = document.getElementById('subAgentDetailInfo');
  if (info) { info.hidden = true; info.replaceChildren(); }
  document.getElementById('subAgentInfoToggle')?.setAttribute('aria-expanded', 'false');
  // These handlers belong to this reader generation, including the Latest
  // action's state closure. Never reuse controls from a previous child.
  document.getElementById('subAgentDetailFooterActions')?.replaceChildren();
  syncSubAgentPresentation();
  state.fallbackTimer = setInterval(() => {
    if (subAgentViewCurrent(state) && ['queued', 'running'].includes(subAgentDetailState.data?.status)) scheduleSubAgentConversationFetch('', true);
  }, 2500);
  const before = remembered?.anchor?.follow === false && Number.isInteger(remembered.anchor.turn) && remembered.total > 0
    ? Math.min(remembered.total, Math.max(1, remembered.anchor.turn + 30)) : null;
  void fetchSubAgentConversation(before);
}

function cancelSubAgentConversationView() {
  const state = subAgentConversationState;
  if (state) {
    const anchor = state.viewport?.captureAnchor() || state.anchor;
    if (anchor) subAgentReadingMemory.set(state.key, { anchor, total: state.total, expanded: state.expanded });
    while (subAgentReadingMemory.size > 12) subAgentReadingMemory.delete(subAgentReadingMemory.keys().next().value);
    state.generation++;
    state.abort?.abort();
    clearTimeout(state.requestTimer);
    clearInterval(state.fallbackTimer);
    const body = document.getElementById('subAgentDetailBody');
    if (body && state.onWheel) body.removeEventListener('wheel', state.onWheel);
    if (body && state.onScroll) body.removeEventListener('scroll', state.onScroll);
    state.viewport?.destroy();
  }
  subAgentConversationState = null;
  if (subAgentConversationFrame) cancelAnimationFrame(subAgentConversationFrame);
  subAgentConversationFrame = 0;
}

function scheduleSubAgentConversationFetch(revision = '', force = false) {
  const state = subAgentConversationState;
  if (!subAgentViewCurrent(state)) return;
  if (revision) state.requestedRevision = revision;
  if (!force && state.loaded && (!revision || revision === state.revision)) return;
  if (state.requestTimer != null) return;
  state.requestTimer = setTimeout(() => {
    state.requestTimer = null;
    if (subAgentViewCurrent(state)) void fetchSubAgentConversation();
  }, 160);
}

async function fetchSubAgentConversation(before = null) {
  const state = subAgentConversationState;
  if (!subAgentViewCurrent(state) || state.pending) return;
  const requestedAgent = subAgentDetailState.data;
  const generation = ++state.generation;
  state.pending = true;
  state.abort = typeof AbortController === 'function' ? new AbortController() : null;
  renderSubAgentConversationView();
  try {
    const response = await fetch('/api/subagents/' + encodeURIComponent(state.agentId) + '/conversation?sessionId=' +
      encodeURIComponent(state.owner) + '&limit=100' + (Number.isInteger(before) ? '&before=' + before : ''),
      { cache: 'no-store', signal: state.abort?.signal });
    const payload = await response.json();
    if (!subAgentViewCurrent(state) || generation !== state.generation) return;
    if (!response.ok || !payload.agent || payload.agent.agentId !== state.agentId || payload.agent.sessionId !== state.owner ||
        !Array.isArray(payload.messages) || !Number.isInteger(payload.startIndex) || payload.startIndex < 0) {
      throw new Error(subAgentText('Unable to read this agent conversation.', '无法读取这个子代理的会话。'));
    }
    state.anchor = state.viewport?.captureAnchor() || state.anchor;
    payload.messages.forEach((message, index) => state.messages.set(payload.startIndex + index, message));
    state.total = Number(payload.totalMessages) || 0;
    for (const index of state.messages.keys()) if (index >= state.total) state.messages.delete(index);
    state.segments = Array.isArray(payload.segments) ? payload.segments : [];
    state.liveTools = Array.isArray(payload.liveTools) ? payload.liveTools : [];
    state.completeness = payload.completeness;
    state.activity = payload.activity;
    state.projectionEpoch++;
    state.revision = String(payload.revision || '');
    state.loaded = true;
    state.error = '';
    // SSE may advance while this request reads disk. Keep that newer sample,
    // including its output prefix, so the next delta cannot lose intervening
    // text. A terminal sample must also survive an older running response.
    const current = subAgentDetailState.data;
    const terminal = current && ['completed', 'failed', 'killed'].includes(current.status);
    const preserve = current && current !== requestedAgent ||
      terminal && current.startedAt === payload.agent.startedAt && ['queued', 'running'].includes(payload.agent.status);
    subAgentDetailState.data = preserve ? { ...payload.agent, ...current } : { ...current, ...payload.agent };
    state.usage = preserve && current.usage || payload.usage;
  } catch (error) {
    if (subAgentViewCurrent(state) && generation === state.generation && error?.name !== 'AbortError') state.error = error.message || subAgentText('Unable to read the conversation.', '暂时无法读取会话。');
  } finally {
    if (subAgentViewCurrent(state) && generation === state.generation) {
      state.pending = false;
      renderSubAgentConversationView();
      if (state.requestedRevision && state.requestedRevision !== state.revision) {
        state.requestedRevision = '';
        scheduleSubAgentConversationFetch('', true);
      }
    }
  }
}

function loadEarlierSubAgentMessages(before) { void fetchSubAgentConversation(before); }

function subAgentBlockText(block) {
  const value = block && block.type === 'tool_result' ? block.content : block?.text;
  return typeof value === 'string' ? value : value == null ? '' : JSON.stringify(value, null, 2);
}

// Pair occurrences, never a global provider-id map. Ambiguous old records keep
// their result separate rather than assigning it to an arbitrary tool call.
function buildSubAgentViewUnits(entries, liveTools = [], segments = []) {
  const units = [], byTrace = new Map(), pending = new Map();
  const segmentStarts = new Set(segments.filter(item => item.reason === 'context_reset').map(item => item.startIndex));
  let previousIndex = -1;
  for (const [index, message] of entries) {
    if (segmentStarts.has(index) || previousIndex >= 0 && index !== previousIndex + 1) { byTrace.clear(); pending.clear(); }
    const unit = { index, role: message.role, blocks: [], reset: segmentStarts.has(index), gapBefore: previousIndex >= 0 && index > previousIndex + 1 ? index - previousIndex - 1 : 0 };
    previousIndex = index;
    const content = Array.isArray(message.content) ? message.content : [{ type: 'text', text: String(message.content || '') }];
    content.forEach((block, ordinal) => {
      const key = index + ':' + ordinal;
      if (block.type === 'tool_use') {
        const tool = { id: key, traceCallId: block.trace_call_id || '', providerToolUseId: block.tool_use_id || '',
          name: block.name || 'Tool', input: block.input || {}, output: '', status: 'running', isError: false, recorded: true };
        const item = { kind: 'tool', key, tool };
        unit.blocks.push(item);
        if (tool.traceCallId) byTrace.set(tool.traceCallId, item);
        if (tool.providerToolUseId) {
          const queue = pending.get(tool.providerToolUseId) || []; queue.push(item); pending.set(tool.providerToolUseId, queue);
        }
      } else if (block.type === 'tool_result') {
        const candidates = (pending.get(String(block.tool_use_id || '')) || []).filter(item => item.tool.status === 'running');
        const match = block.trace_call_id ? byTrace.get(block.trace_call_id) : candidates.length === 1 ? candidates[0] : null;
        if (match && match.tool.status === 'running') Object.assign(match.tool, { output: subAgentBlockText(block), isError: !!block.is_error, status: block.is_error ? 'failed' : 'completed' });
        else unit.blocks.push({ kind: 'tool', key, tool: { id: key, name: block.name || subAgentText('Tool result', '工具结果'),
          providerToolUseId: block.tool_use_id || '', input: null, output: subAgentBlockText(block), isError: !!block.is_error,
          status: block.is_error ? 'failed' : 'completed', recorded: true } });
      } else if (['text', 'thinking', 'redacted_thinking', 'image'].includes(block.type)) unit.blocks.push({ kind: block.type, key, block });
    });
    units.push(unit);
  }
  liveTools.forEach(tool => {
    const existing = tool.traceCallId && byTrace.get(tool.traceCallId);
    if (existing) { if (existing.tool.status === 'running') Object.assign(existing.tool, tool); }
    else units.push({ index: 'live-' + tool.id, role: 'assistant', blocks: [{ kind: 'tool', key: 'live-' + tool.id, tool }] });
  });
  return units.filter(unit => unit.blocks.length || unit.reset || unit.gapBefore);
}

function subAgentMarkdown(node, text, streaming = false) {
  text = String(text || '');
  if (node._subagentText === text) return;
  node._subagentText = text;
  if (streaming && typeof renderStreamingMarkdown === 'function') renderStreamingMarkdown(node, text);
  else {
    const html = typeof formatContent === 'function' ? formatContent(text) : escHtml(text).replace(/\n/g, '<br>');
    if (node.innerHTML !== html) node.innerHTML = html;
  }
}

function subAgentToolNode(item, state) {
  const details = document.createElement('details');
  details.className = 'subagent-tool';
  details.dataset.toolKey = item.key;
  details.dataset.traceCallId = item.tool.traceCallId || '';
  details._subagentItem = item;
  const summary = document.createElement('summary');
  const label = document.createElement('span'); label.className = 'subagent-tool-title';
  const status = document.createElement('span'); status.className = 'subagent-tool-status';
  summary.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true"><path d="m6 3 5 5-5 5"/></svg>';
  summary.append(label, status); details.appendChild(summary);
  const content = document.createElement('div'); content.className = 'subagent-tool-body'; details.appendChild(content);
  const update = () => {
    const tool = details._subagentItem.tool;
    label.textContent = tool.name || 'Tool';
    status.textContent = ({ incomplete: subAgentText('Interrupted', '已中断'), result_only: subAgentText('Tool result', '工具结果') })[tool.status] || subAgentStatusLabel(tool.status === 'error' ? 'failed' : tool.status);
    details.dataset.state = tool.isError ? 'failed' : tool.status;
    if (!details.open) return;
    const signature = JSON.stringify([tool.input, tool.output, tool.isError, tool.status]);
    if (content._subagentSignature === signature) return;
    content._subagentSignature = signature;
    content.replaceChildren();
    const append = (heading, value) => {
      const h = document.createElement('div'); h.className = 'subagent-tool-section-label'; h.textContent = heading;
      const pre = document.createElement('pre'); pre.textContent = value; content.append(h, pre);
    };
    if (tool.input != null) append(subAgentText('Input', '输入'), typeof tool.input === 'string' ? tool.input : JSON.stringify(tool.input, null, 2));
    if (tool.output !== '' && tool.output != null) append(subAgentText('Output', '输出'), String(tool.output));
    else if (['running', 'queued'].includes(tool.status)) {
      const note = document.createElement('p'); note.textContent = subAgentText('Waiting for the tool result…', '正在等待工具结果…'); content.appendChild(note);
    }
  };
  details.addEventListener('toggle', () => {
    if (details.open) state.expanded.add(item.key); else state.expanded.delete(item.key);
    update();
    state.viewport?.schedule();
  });
  details._subagentUpdate = update;
  details.open = state.expanded.has(item.key);
  update();
  return details;
}

function subAgentUpdateUnit(unit, nodes, state) {
  const element = nodes[0];
  if (!element) return;
  const wanted = [];
  const reuse = new Map(Array.from(element.children).map(node => [node.dataset.blockKey, node]));
  if (unit.model.notice) {
    const note = reuse.get('notice') || document.createElement('p');
    note.dataset.blockKey = 'notice'; note.className = unit.model.error ? 'subagent-detail-error' : 'subagent-history-note';
    note.textContent = unit.model.notice; wanted.push(note);
  }
  if (unit.model.reset) {
    let note = reuse.get('reset') || document.createElement('p'); note.dataset.blockKey = 'reset'; note.className = 'subagent-history-note';
    note.textContent = subAgentText('Context checkpoint · Earlier recorded messages remain above.', '上下文检查点 · 更早的已记录消息保留在上方。'); wanted.push(note);
  }
  unit.model.blocks.forEach(item => {
    let node = reuse.get(item.key);
    if (item.kind === 'tool') {
      if (!node) node = subAgentToolNode(item, state);
      node._subagentItem = item; node._subagentUpdate?.();
    } else if (item.kind === 'thinking' || item.kind === 'redacted_thinking') {
      if (!node) {
        node = document.createElement('details'); node.className = 'subagent-thinking';
        const summary = document.createElement('summary'); summary.textContent = subAgentText('Thinking', '思考过程');
        const body = document.createElement('div'); body.className = 'subagent-thinking-body message-content'; node.append(summary, body);
        node.open = state.expanded.has(item.key);
        node.addEventListener('toggle', () => { if (node.open) state.expanded.add(item.key); else state.expanded.delete(item.key); state.viewport?.schedule(); });
      }
      subAgentMarkdown(node.lastElementChild, item.kind === 'redacted_thinking' ? subAgentText('The model did not expose this reasoning.', '模型未公开这段思考内容。') : subAgentBlockText(item.block));
    } else if (item.kind === 'image') {
      if (!node) { node = document.createElement('p'); node.className = 'subagent-history-note'; }
      node.textContent = subAgentText('Image attachment', '图像附件');
    } else {
      if (!node) { node = document.createElement('div'); node.className = 'message-body message-content' + (unit.model.role === 'user' ? ' subagent-task-content' : ''); }
      subAgentMarkdown(node, subAgentBlockText(item.block), !!unit.live);
    }
    node.dataset.blockKey = item.key; wanted.push(node);
  });
  const desired = new Set(wanted);
  Array.from(element.children).forEach(node => { if (!desired.has(node)) node.remove(); });
  wanted.forEach((node, index) => { if (element.children[index] !== node) element.insertBefore(node, element.children[index] || null); });
}

function subAgentReadingAnchor(body) {
  const bounds = body.getBoundingClientRect();
  const candidates = body.querySelectorAll('.message-content p, .message-content pre, .message-content li, .subagent-tool > summary');
  for (const element of candidates) {
    const rect = element.getBoundingClientRect();
    if (rect.bottom > bounds.top && rect.top < bounds.bottom && element.textContent.trim()) {
      return { element, text: element.textContent, offset: rect.top - bounds.top };
    }
  }
  return null;
}

function restoreSubAgentReadingAnchor(body, anchor) {
  if (!anchor) return;
  let element = anchor.element;
  if (!element.isConnected) element = [...body.querySelectorAll('.message-content p, .message-content pre, .message-content li, .subagent-tool > summary')]
    .find(node => node.textContent === anchor.text);
  if (!element) return;
  const delta = element.getBoundingClientRect().top - body.getBoundingClientRect().top - anchor.offset;
  if (Math.abs(delta) > .5) body.scrollTop += delta;
}

function renderSubAgentConversationView() {
  if (subAgentConversationFrame) return;
  subAgentConversationFrame = requestAnimationFrame(() => { subAgentConversationFrame = 0; commitSubAgentConversationView(); });
}

function subAgentConversationModels(state, agent) {
  if (state.cachedEpoch !== state.projectionEpoch) {
    const entries = [...state.messages.entries()].sort((a, b) => a[0] - b[0]);
    state.cachedEntries = entries;
    state.cachedModels = buildSubAgentViewUnits(entries, state.liveTools, state.segments).flatMap(model => model.gapBefore
      ? [{ index: 'gap-' + model.index, role: 'system', before: model.index, blocks: [], gap: true }, model] : [model]);
    state.committedText = entries.filter(([, message]) => message.role === 'assistant').flatMap(([, message]) => message.content || [])
      .filter(block => block.type === 'text').map(block => String(block.text || '')).join('');
    state.cachedEpoch = state.projectionEpoch;
  }
  const entries = state.cachedEntries || [], models = [...(state.cachedModels || [])], committed = state.committedText || '';
  const output = String(agent.output || '');
  let tail = '', preview = !!agent.outputTruncated;
  if (!state.messages.size) tail = output || String(agent.result || '');
  else if (!agent.outputTruncated && output.startsWith(committed)) tail = output.slice(committed.length);
  else if (['queued', 'running'].includes(agent.status) && output) {
    // A paged suffix or a context reset cannot be matched to the run's text
    // accumulator safely. Keep its bounded live preview visible until the
    // recorded conversation catches up; never guess a tool/message pairing.
    tail = output; preview = true;
  }
  if (tail.trim()) models.push({ index: 'tail', role: 'assistant', live: true,
    notice: preview ? subAgentText('Live output preview · Recorded messages remain in the conversation.', '实时输出预览 · 完整消息会继续保存到对话中。') : '',
    blocks: [{ kind: 'text', key: 'tail', block: { text: tail } }] });
  if (entries.length && entries[0][0] > 0) models.unshift({ index: 'earlier', role: 'system', before: entries[0][0], blocks: [] });
  if (!models.length) models.push({ index: 'empty', role: 'system', blocks: [], notice: agent.status === 'queued'
    ? subAgentText('Waiting for an execution slot. This agent has not started yet.', '正在等待执行名额，子代理尚未开始执行。')
    : agent.status === 'running' ? subAgentText('The agent is working. Its task and tool activity will appear here.', '子代理正在执行，任务与工具活动会在这里更新。')
      : subAgentText('This agent has no recorded messages.', '这个子代理没有保存会话消息。') });
  const error = state.error || subAgentDetailState.error || agent.exitError;
  if (error) models.push({ index: 'error', role: 'system', blocks: [], notice: error, error: true });
  if (state.completeness?.state === 'unavailable' && state.messages.size === 0) models.unshift({ index: 'unavailable', role: 'system', blocks: [],
    notice: subAgentText('Only the retained output is available for this older agent.', '这个子代理的完整会话记录不可用，目前展示已保留的输出。') });
  return models;
}

function updateSubAgentConversationChrome() {
  if (!subAgentViewCurrent(subAgentConversationState)) return;
  const state = subAgentConversationState, agent = subAgentDetailState.data || {};
  const description = document.getElementById('subAgentDetailDescription');
  if (description) description.textContent = agent.status ? subAgentDetailHeading(agent) : subAgentText('Loading agent activity…', '正在读取子代理活动…');
  const metrics = document.getElementById('subAgentDetailMetrics');
  if (metrics) {
    let usage = metrics.querySelector('.subagent-detail-token-value');
    let elapsed = metrics.querySelector('#subAgentDetailElapsed');
    if (!usage || !elapsed) {
      metrics.replaceChildren();
      usage = document.createElement('span'); usage.className = 'subagent-detail-token-value';
      elapsed = document.createElement('span'); elapsed.id = 'subAgentDetailElapsed';
      metrics.append(usage, elapsed);
    }
    usage.textContent = subAgentUsageLabel(state.usage || agent.usage);
    const receipt = state.usage || agent.usage;
    usage.title = receipt?.available && Number.isSafeInteger(receipt.totalTokens)
      ? receipt.totalTokens.toLocaleString() + ' tokens' : subAgentText('No provider receipt yet', '尚无供应商用量回执');
    elapsed.textContent = agent.startedAt || agent.elapsedMs != null ? subAgentElapsedLabel(subAgentElapsedMilliseconds(agent)) : '';
  }
}

function commitSubAgentConversationView() {
  const state = subAgentConversationState;
  if (!subAgentViewCurrent(state)) return;
  const overlay = document.getElementById('subAgentDetailOverlay'), body = document.getElementById('subAgentDetailBody');
  if (!overlay || overlay.hidden || !body) return;
  const agent = subAgentDetailState.data || {};
  const label = subAgentText;
  document.getElementById('subAgentDetailTitle').textContent = agent.name || label('Agent conversation', '子代理会话');
  document.getElementById('subAgentDetailDescription').textContent = agent.status ? subAgentDetailHeading(agent) : label('Loading agent activity…', '正在读取子代理活动…');
  body.setAttribute('aria-busy', state.pending && !state.loaded ? 'true' : 'false');
  updateSubAgentConversationChrome();
  const back = document.getElementById('subAgentBack');
  if (back) { back.setAttribute('aria-label', label('Back to parent conversation', '返回主对话')); const span = back.querySelector('span'); if (span) span.textContent = label('Parent conversation', '主对话'); }
  const readOnly = document.getElementById('subAgentReadOnlyLabel');
  if (readOnly) {
    const parentOwned = agent.ownership === 'parent' || agent.canAcceptDirectInput !== true;
    readOnly.textContent = parentOwned ? label('Managed by parent agent', '由主代理管理') : label('Agent conversation', '子代理会话');
    readOnly.title = parentOwned ? label('Continue assigning this agent in the parent conversation.', '请在主对话中继续指派这个子代理。') : '';
  }
  const infoToggle = document.getElementById('subAgentInfoToggle');
  if (infoToggle) { infoToggle.setAttribute('aria-label', label('Details', '详情')); const span = infoToggle.querySelector('span'); if (span) span.textContent = label('Details', '详情'); }
  const close = document.getElementById('subAgentDetailClose');
  if (close) { close.title = label('Close', '关闭'); close.setAttribute('aria-label', close.title); }
  const info = document.getElementById('subAgentDetailInfo');
  if (info && state.infoOpen) {
    const usage = state.usage || agent.usage;
    const fields = [[label('Task', '任务'), agent.task], [label('Model', '模型'), agent.model],
      [label('Agent ID', '代理 ID'), state.agentId], [label('Parent session', '父会话'), state.owner],
      [label('Mode', '执行方式'), agent.background ? label('Background', '后台执行') : label('Foreground', '前台执行')],
      [label('Reported tokens', '已回报用量'), usage?.available ? subAgentUsageLabel(usage) : label('No provider receipt yet', '尚无供应商回执')]];
    const html = '<dl class="subagent-detail-meta">' + fields.filter(([, value]) => value != null && value !== '').map(([key, value]) => '<div><dt>' + escHtml(key) + '</dt><dd>' + escHtml(String(value)) + '</dd></div>').join('') + '</dl>';
    if (info.innerHTML !== html) info.innerHTML = html;
  }
  const footer = document.getElementById('subAgentDetailFooterActions');
  if (footer) {
    let stop = footer.querySelector('.subagent-detail-stop');
    const active = ['queued', 'running'].includes(agent.status);
    if (active && !stop) { stop = document.createElement('button'); stop.className = 'subagent-detail-stop'; stop.type = 'button'; stop.addEventListener('click', stopSubAgent); footer.appendChild(stop); }
    if (stop) { stop.hidden = !active; stop.disabled = !!subAgentDetailState.stopping; stop.textContent = subAgentDetailState.stopping ? label('Stopping…', '正在停止…') : agent.status === 'queued' ? label('Cancel queued agent', '取消排队') : label('Stop agent', '停止子代理'); }
    let latest = footer.querySelector('.subagent-return-latest');
    if (!latest) { latest = document.createElement('button'); latest.type = 'button'; latest.className = 'subagent-return-latest subagent-detail-action'; latest.textContent = label('Latest', '回到最新'); latest.addEventListener('click', () => { state.follow = true; state.viewport?.restoreAnchor({ sessionId: state.key, follow: true }); state.viewport?.flush(); }); footer.appendChild(latest); }
    latest.hidden = state.follow;
  }
  // A remembered position refers to recorded message keys. A preview-only
  // viewport has none of those keys and would overwrite the saved position
  // before the initial conversation request can restore it.
  if (!state.loaded && !state.messages.size && (!agent.output || state.anchor?.follow === false)) {
    let note = body.querySelector('.subagent-detail-loading, .subagent-detail-error');
    if (!note) { body.replaceChildren(); note = document.createElement('p'); body.appendChild(note); }
    note.className = state.error ? 'subagent-detail-error' : 'subagent-detail-loading';
    note.textContent = state.error || label('Reading the conversation…', '正在读取子代理会话…');
    return;
  }
  const reading = !state.follow ? subAgentReadingAnchor(body) : null;
  const anchor = state.viewport?.captureAnchor() || state.anchor;
  const models = subAgentConversationModels(state, agent);
  const ordinal = model => {
    if (typeof model.index === 'number') return model.index + 1;
    if (model.index === 'earlier') return 0;
    if (!state.syntheticKeys.has(model.index)) state.syntheticKeys.set(model.index, 1000000000 + state.syntheticKeys.size);
    return state.syntheticKeys.get(model.index);
  };
  const units = models.map(model => ({
    key: state.key + ':history-' + ordinal(model),
    turn: ordinal(model),
    signature: String(model.index) + ':' + model.role + ':' + model.blocks.map(item => item.kind + ':' + item.key).join('|') +
      (model.before != null ? ':before=' + model.before + ':pending=' + state.pending : ''),
    estimatedHeight: model.before != null ? 50 : 48 + model.blocks.reduce((sum, item) => sum + (item.kind === 'text' ? Math.min(1600, Math.ceil(subAgentBlockText(item.block).length / 80) * 24) : 44), 0),
    model, live: !!model.live,
  }));
  state.units = units;
  const render = unit => {
    const element = document.createElement('article'); element.className = 'subagent-message'; element.dataset.role = unit.model.role; element.dataset.sourceIndex = String(unit.model.index);
    if (unit.model.before != null) {
      const earlier = document.createElement('button'); earlier.type = 'button'; earlier.className = 'subagent-load-earlier';
      earlier.textContent = unit.model.gap ? label('Load omitted messages', '加载中间的消息') : label('Load earlier messages', '加载更早的消息'); earlier.disabled = state.pending;
      earlier.addEventListener('click', () => loadEarlierSubAgentMessages(unit.model.before)); element.appendChild(earlier);
    } else subAgentUpdateUnit(unit, [element], state);
    body.appendChild(element);
    return [element];
  };
  if (typeof HistoryVirtualViewport === 'function' && body.getBoundingClientRect && body.insertBefore) {
    if (!state.viewport) {
      body.replaceChildren();
      state.viewport = new HistoryVirtualViewport({ area: body, sessionId: state.key, units, anchor: anchor || { sessionId: state.key, follow: state.follow },
        renderTurn: render, isFollowing: () => state.follow, onFollow: value => { state.follow = value; },
        updateMetadata: (unit, nodes) => { if (unit.model.before == null) subAgentUpdateUnit(unit, nodes, state); },
        onViewport: () => { const latest = document.querySelector('#subAgentDetailFooterActions .subagent-return-latest'); if (latest) latest.hidden = state.follow; },
      });
      state.onWheel = event => { if (event.deltaY < 0) state.follow = false; };
      state.onScroll = () => { if (subAgentViewCurrent(state) && !state.viewport?._restoring) state.follow = body.scrollHeight - body.scrollTop - body.clientHeight <= 48; };
      body.addEventListener('wheel', state.onWheel, { passive: true });
      body.addEventListener('scroll', state.onScroll, { passive: true });
    } else state.viewport.setUnits(units, { anchor: anchor || undefined });
    units.forEach(unit => state.viewport.updateMetadata(unit.key, unit.model));
    state.viewport.flush();
    if (!state.follow) restoreSubAgentReadingAnchor(body, reading);
  } else {
    const fragment = document.createDocumentFragment(); units.forEach(unit => render(unit).forEach(node => fragment.appendChild(node))); body.replaceChildren(fragment);
    if (state.follow) body.scrollTop = body.scrollHeight; else restoreSubAgentReadingAnchor(body, reading);
  }
}

function toggleSubAgentInfo() {
  const state = subAgentConversationState;
  if (!subAgentViewCurrent(state)) return;
  state.infoOpen = !state.infoOpen;
  const info = document.getElementById('subAgentDetailInfo'), button = document.getElementById('subAgentInfoToggle');
  if (info) info.hidden = !state.infoOpen;
  if (button) button.setAttribute('aria-expanded', String(state.infoOpen));
  renderSubAgentConversationView();
}

function syncSubAgentPresentation() {
  const overlay = document.getElementById('subAgentDetailOverlay'), button = document.getElementById('subAgentPresentationToggle');
  if (!overlay) return;
  const side = overlay.dataset.presentation === 'side';
  const label = side ? subAgentText('Open in main pane', '在主区域查看') : subAgentText('Open in side pane', '在侧边查看');
  if (button) { button.title = label; button.setAttribute('aria-label', label); button.setAttribute('aria-pressed', String(side)); }
}

function toggleSubAgentPresentation() {
  const overlay = document.getElementById('subAgentDetailOverlay');
  if (!overlay) return;
  overlay.dataset.presentation = overlay.dataset.presentation === 'side' ? 'page' : 'side';
  syncSubAgentPresentation();
  subAgentConversationState?.viewport?.schedule();
}

function startSubAgentCatalogClock() {
  stopSubAgentCatalogClock();
  subAgentCatalogClock = setInterval(() => {
    const pop = document.getElementById('statusPopover');
    if (!pop || pop.style.display === 'none') { stopSubAgentCatalogClock(); return; }
    const agents = new Map(subAgentCatalogAgents().map(item => [String(item.agentId), item]));
    pop.querySelectorAll('[data-subagent-id]').forEach(row => {
      const agent = agents.get(row.dataset.subagentId), value = row.querySelector('.status-agent-metrics small');
      if (agent && value) value.textContent = subAgentElapsedLabel(subAgentElapsedMilliseconds(agent));
    });
  }, 1000);
}

function stopSubAgentCatalogClock() {
  if (subAgentCatalogClock != null) clearInterval(subAgentCatalogClock);
  subAgentCatalogClock = null;
}

function initSubAgentViewInteractions() {
  const chip = document.getElementById('statusChip'), pop = document.getElementById('statusPopover');
  if (chip) chip.addEventListener('keydown', event => {
    if (event.key !== 'ArrowDown') return;
    event.preventDefault();
    if (pop && pop.style.display === 'none') toggleStatusPopover(event);
    pop?.querySelector('[data-subagent-id]')?.focus();
  });
  if (pop) pop.addEventListener('keydown', event => {
    const rows = [...pop.querySelectorAll('[data-subagent-id]')];
    const index = rows.indexOf(document.activeElement);
    if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
      event.preventDefault();
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? rows.length - 1
        : event.key === 'ArrowDown' ? (index + 1) % rows.length : (index - 1 + rows.length) % rows.length;
      rows[next]?.focus();
    } else if (event.key === 'Escape' || event.key === 'ArrowLeft') {
      event.preventDefault(); closeStatusPopover(); chip?.focus();
    } else if (event.key === 'ArrowRight' && index >= 0) {
      event.preventDefault(); const row = rows[index]; openSubAgentDetails(row.dataset.subagentId, row, row.dataset.subagentSessionId, 'side');
    }
  });
  const main = document.querySelector('.main');
  if (main && typeof MutationObserver === 'function') {
    const observer = new MutationObserver(() => {
      if (main.dataset.page && main.dataset.page !== 'session' && subAgentDetailState.agentId) closeSubAgentDetails(false);
    });
    observer.observe(main, { attributes: true, attributeFilter: ['data-page'] });
  }
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', initSubAgentViewInteractions, { once: true });
else initSubAgentViewInteractions();
