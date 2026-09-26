package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// Exercise the transcript's actual event and history renderers. The DOM shim
// models only the nodes those functions read, so this catches a grouping or
// disclosure regression without depending on a separately installed browser.
func TestActivityGroupBrowserLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
function extract(start, end) {
  const a = source.indexOf(start), b = source.indexOf(end, a + start.length);
  assert(a >= 0 && b > a, 'missing browser function boundary ' + start);
  return source.slice(a, b);
}


let area;
let root;
class Element {
  constructor(tag = 'div') {
    this.tagName = tag; this.parentElement = null; this.children = [];
    this.dataset = {}; this.attrs = {}; this._html = ''; this.textContent = '';
    this.scrollTop = 0; this.scrollHeight = 0; this.clientHeight = 0;
    this.listeners = new Map();
    this.classes = new Set();
    this.classList = {
      add: name => this.classes.add(name), remove: name => this.classes.delete(name),
      contains: name => this.classes.has(name),
      toggle: (name, force) => {
        const on = force === undefined ? !this.classes.has(name) : !!force;
        if (on) this.classes.add(name); else this.classes.delete(name);
        return on;
      }
    };
  }
  set className(value) { this.classes = new Set(String(value).split(/\s+/).filter(Boolean)); }
  get className() { return [...this.classes].join(' '); }
  get isConnected() {
    for (let node = this; node; node = node.parentElement) if (node === area) return true;
    return false;
  }
  get lastElementChild() { return this.children.at(-1) || null; }
  get nextElementSibling() {
    if (!this.parentElement) return null;
    return this.parentElement.children[this.parentElement.children.indexOf(this) + 1] || null;
  }
  setAttribute(key, value) {
    this.attrs[key] = String(value);
    if (key.startsWith('data-')) this.dataset[key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = String(value);
  }
  getAttribute(key) {
    if (key.startsWith('data-')) return this.dataset[key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] ?? null;
    return this.attrs[key] ?? null;
  }
  removeAttribute(key) {
    delete this.attrs[key];
    if (key.startsWith('data-')) delete this.dataset[key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())];
  }
  appendChild(node) {
    if (node.parentElement) node.remove();
    node.parentElement = this; this.children.push(node); return node;
  }
  addEventListener(type, handler) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(handler);
  }
  dispatch(type) { for (const handler of this.listeners.get(type) || []) handler(); }
  insertBefore(node, anchor) {
    if (node.parentElement) node.remove();
    const index = this.children.indexOf(anchor);
    assert(index >= 0);
    node.parentElement = this; this.children.splice(index, 0, node); return node;
  }
  remove() {
    if (!this.parentElement) return;
    const siblings = this.parentElement.children;
    siblings.splice(siblings.indexOf(this), 1); this.parentElement = null;
  }
  set innerHTML(html) {
    this._html = String(html);
    this.children.forEach(child => child.parentElement = null);
    this.children = [];
    if (this.classList.contains('activity-turn')) {
      const expanded = this._html.match(/aria-expanded="(true|false)"/)?.[1] || 'true';
      const toggle = new Element('button'); toggle.className = 'activity-turn-toggle'; toggle.setAttribute('aria-expanded', expanded);
      const duration = new Element('span'); duration.className = 'activity-turn-duration'; toggle.appendChild(duration);
      const state = new Element('span'); state.className = 'activity-turn-state'; toggle.appendChild(state);
      const chevron = new Element('span'); chevron.className = 'activity-turn-chevron'; toggle.appendChild(chevron);
      this.appendChild(toggle);
    }
    if (this.classList.contains('activity-group')) {
      const expanded = this._html.match(/aria-expanded="(true|false)"/)?.[1] || 'false';
      const summary = new Element('button'); summary.className = 'activity-group-summary'; summary.setAttribute('aria-expanded', expanded);
      this.appendChild(summary);
      const body = new Element(); body.className = 'activity-group-body'; this.appendChild(body);
      const items = new Element(); items.className = 'activity-group-items'; body.appendChild(items);
    }
  }
  get innerHTML() { return this._html; }
  insertAdjacentHTML(position, html) {
    assert.equal(position, 'beforeend');
    const node = new Element();
    node.className = html.match(/<div class="([^"]+)"/)?.[1] || '';
    for (const [, key, value] of html.matchAll(/\b(data-[\w-]+)="([^"]*)"/g)) node.setAttribute(key, value);
    if (node.classList.contains('call-row')) {
      for (const name of ['tc-leading', 'tc-title', 'tc-summary', 'tc-time']) {
        const child = new Element(); child.className = name;
        child.textContent = html.match(new RegExp('class="' + name + '">([^<]*)<'))?.[1] || '';
        node.appendChild(child);
      }
      for (const key of ['in', 'out']) {
        const child = new Element(); child.className = 'tc-io-text'; child.setAttribute('data-' + key, ''); node.appendChild(child);
      }
    } else if (node.classList.contains('think-row')) {
      for (const name of ['think-summary', 'think-body', 'think-time']) {
        const child = new Element(); child.className = name; node.appendChild(child);
      }
    } else if (node.classList.contains('message')) {
      const content = new Element(); content.className = node.classList.contains('message-user') ? 'message-bubble' : 'message-content';
      content.innerHTML = html.match(/class="message-content">([\s\S]*?)<\/div>/)?.[1] || '';
      node.appendChild(content);
      if (html.includes('stream-cursor')) { const cursor = new Element('span'); cursor.className = 'stream-cursor'; node.appendChild(cursor); }
    }
    this.appendChild(node);
  }
  matches(selector) {
    if (selector.includes(' ')) return false; // Descendant selectors need the missing child, not the parent.
    const klass = selector.match(/^\.([\w-]+)/)?.[1];
    if (klass && !this.classList.contains(klass)) return false;
    for (const [, attr, value] of selector.matchAll(/\[([\w-]+)="([^"]*)"\]/g)) if (this.getAttribute(attr) !== value) return false;
    return true;
  }
  querySelectorAll(selector) {
    const selectors = selector.split(',').map(part => part.trim());
    const found = [];
    const visit = node => {
      for (const child of node.children) {
        if (selectors.some(part => child.matches(part))) found.push(child);
        visit(child);
      }
    };
    visit(this); return found;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) {
    for (let node = this; node; node = node.parentElement) if (node.matches(selector)) return node;
    return null;
  }
}
area = new Element();
root = new Element('html');
const liveCallbacks = {};
const frames = [];
function flushFrames() {
  for (let count = 0; frames.length; count++) {
    assert(count < 100, 'animation frames must settle');
    frames.shift()();
  }
}
const c = {
  Map, Set, Date, console, queueMicrotask: fn => fn(),
  requestAnimationFrame: fn => frames.push(fn),
  window: {EventSource: function() {}},
  EventSource: function() { this.addEventListener = () => {}; this.close = () => {}; },
  eventSource: null,
  onLive: (name, handler) => { liveCallbacks[name] = handler; }, onInteraction() {},
  handleTokensEvent() {}, handleContextEvent() {}, handleCompactionStart() {},
  handleCompactionProgress() {}, handleCompactionEnd() {}, handleAskUser() {},
  handlePermissionRequest() {}, handleInteractionResolved() {}, showToast() {},
  document: {documentElement: root, getElementById: id => id === 'chatArea' ? area : null,
    createElement: tag => new Element(tag), querySelector: selector => area.querySelector(selector),
    querySelectorAll: selector => area.querySelectorAll(selector)},
  uiText: (en, zh) => zh, escHtml: value => String(value), escAttr: value => String(value), escOnclick: value => String(value),
  fmtMs: ms => ms + 'ms', formatContent: text => String(text),
  messageActionsMarkup: () => '', visibleTranscriptText: value => String(value),
  autoScroll() {}, updateEmptyLayout() {}, updateSendBtn() {}, loadSessions() {}, loadSessionStatsbar() {},
  resumeAutoScroll() {}, restoreTodoPlanFromHistory() {}, restoreHistoryMessageMetadata() {},
  endTurnStatus() {}, showTurnStatsLine() {},
  loadSessionFiles() {}, attachMessageActions() {}, isTodoWriteTool: () => false, isPlanningTool: () => false,
  renderDetailPanel() {}, closeToolDetail() { c.selectedToolId = null; },
  currentView: 'chat', currentSessionId: 'A',
  desktopPreferences: {presentationMode:'standard'},
  pendingForegroundRequest: null, turnRunning: false, runningSessionId: null, renderSessions() {},
  lastStatusSnapshot: null,
  runningTurnIncompleteReason: '',
  toolDetails: {}, selectedToolId: null, messages: [], streaming: false,
  streamingEl: null, streamingText: '', streamMsgIdx: -1, turnStartMs: 0, turnFirstTokenMs: 0,
};
vm.createContext(c);
vm.runInContext(extract('function sameSession(d)', 'let thinkingEl = null;'), c);
vm.runInContext(extract('let thinkingEl = null;', 'const THINK_ORBIT_ICON'), c);
vm.runInContext(extract('const THINK_ORBIT_ICON', 'let todoPlanItems ='), c);
vm.runInContext(extract('function handleTextDelta(d)', 'const foregroundRequests ='), c);
vm.runInContext(extract('const foregroundRequests =', 'function syncTrackedRunningState('), c);
vm.runInContext(extract('function startStreamingMessage()', '// A provider turn_end'), c);
vm.runInContext(extract('const TOOL_VARIANTS =', 'const FILE_DIFF_MAX_LINES'), c);
vm.runInContext(extract('function toolRowsInCurrentTurn()', '// Search card'), c);
vm.runInContext(extract('function addMessage(', '// Attach the hover actions row'), c);
vm.runInContext(extract('function renderHistoryMessages(history)', 'async function restoreHistoryMessageMetadata('), c);
vm.runInContext(extract('async function restoreHistoryMessageMetadata(', 'function showError('), c);
vm.runInContext(extract('function finishUserTurn(', '// Reset every piece of in-flight turn state.'), c);
vm.runInContext(extract('function connectEvents()', 'function handleBackgroundContinuation(d)'), c);

const turns = () => area.querySelectorAll('.activity-turn');
const groups = () => area.querySelectorAll('.activity-group');
const rows = group => group.querySelectorAll('.call-row');
const summary = group => group.querySelector('.activity-group-summary');
const turnToggle = turn => turn.querySelector('.activity-turn-toggle');

(async () => {
// A live turn owns multiple process sections and one duration. An Artifact
// remains a peer between the sections, rather than becoming hidden detail.
c.handleThinkingDelta({delta:'Inspect source'});
c.handleToolStart({tool:'Bash', id:'live-command', input:'{"description":"Check repository"}'});
c.handleToolResult({tool:'Bash', id:'live-command', output:'clean', elapsedMs:11});
c.handleToolStart({tool:'Read', id:'live-read', input:'{"path":"README.md"}'});
c.handleToolResult({tool:'Read', id:'live-read', output:'contents', elapsedMs:9});
assert.equal(turns().length, 1);
assert.equal(turns()[0].dataset.state, 'running');
assert.equal(groups().length, 1);
assert.equal(groups()[0].querySelectorAll('.think-row').length, 1);
assert.equal(rows(groups()[0]).length, 2);
assert.match(summary(groups()[0]).textContent, /执行了命令/);
assert.match(summary(groups()[0]).textContent, /已读取文件/);
const firstGroup = groups()[0];
c.setActivityGroupOpen(firstGroup, true);
flushFrames();
const liveItems = firstGroup.querySelector('.activity-group-items');
liveItems.scrollHeight = 500; liveItems.clientHeight = 100; liveItems.scrollTop = 370;
liveItems.dispatch('scroll');
liveItems.scrollHeight = 560;
c.handleToolStart({tool:'Read', id:'follow-read', input:'{"path":"next"}'});
flushFrames();
assert.equal(liveItems.scrollTop, 560, 'near-bottom process output follows new rows');
c.handleToolResult({tool:'Read', id:'follow-read', output:'next', elapsedMs:4});
liveItems.scrollTop = 100;
liveItems.dispatch('scroll');
liveItems.scrollHeight = 620;
c.handleToolStart({tool:'Read', id:'scroll-up-read', input:'{"path":"last"}'});
flushFrames();
assert.equal(liveItems.scrollTop, 100, 'manual upward scroll is preserved during streaming');
c.handleToolResult({tool:'Read', id:'scroll-up-read', output:'last', elapsedMs:4});
c.finishActivityGroup();
const artifact = new Element('article'); artifact.className = 'artifact-chat-card'; area.appendChild(artifact);
c.handleToolStart({tool:'Bash', id:'live-second-command', input:'{"description":"Verify"}'});
c.handleToolResult({tool:'Bash', id:'live-second-command', output:'ok', elapsedMs:7});
assert.equal(groups().length, 2);
assert.equal(groups()[1].dataset.turnId, firstGroup.dataset.turnId);
assert.equal(artifact.parentElement, area);
c.handleTextDelta({delta:'Final answer'});
c.endStreamingMessage();
c.finishUserTurn();
const liveTurn = turns()[0];
assert.equal(liveTurn.dataset.state, 'completed');
assert.equal(liveTurn.classList.contains('open'), false, 'successful turn with an answer folds by default');
assert.equal(liveTurn.querySelectorAll('.activity-turn-duration').length, 1);
assert.equal(groups().some(group => group.querySelector('.activity-group-duration')), false,
  'turn duration must never be repeated on an inner process group');
assert.equal(area.lastElementChild.classList.contains('message-assistant'), true);
assert.equal(area.lastElementChild.querySelector('.message-content').innerHTML, 'Final answer');
assert.equal(groups().some(group => group.querySelector('.message-assistant')), false);

// Outer disclosure hides only the process sections and keeps their own
// disclosure state. The Artifact and answer remain visible at top level.
c.toggleActivityGroup(summary(firstGroup));
assert.equal(firstGroup.classList.contains('open'), true);
c.toggleActivityTurn(turnToggle(liveTurn));
assert.equal(turnToggle(liveTurn).getAttribute('aria-expanded'), 'true');
assert.equal(firstGroup.getAttribute('data-turn-collapsed'), null);
assert.equal(firstGroup.classList.contains('open'), true);
c.toggleActivityTurn(turnToggle(liveTurn));
assert.equal(turnToggle(liveTurn).getAttribute('aria-expanded'), 'false');
assert.equal(firstGroup.getAttribute('data-turn-collapsed'), 'true');
assert.equal(firstGroup.classList.contains('open'), true);
assert.equal(artifact.parentElement, area);
assert.equal(artifact.getAttribute('data-turn-collapsed'), null);
c.toggleActivityTurn(turnToggle(liveTurn));
assert.equal(turnToggle(liveTurn).getAttribute('aria-expanded'), 'true');
assert.equal(firstGroup.classList.contains('open'), true, 'inner disclosure survives outer fold');
c.toggleActivityGroup(summary(firstGroup));
assert.equal(firstGroup.classList.contains('open'), false);
assert.equal(liveTurn.classList.contains('open'), true, 'inner fold does not close outer turn');
c.uiText = (en, zh) => en;
c.refreshActivityGroupLanguage();
assert.match(summary(firstGroup).textContent, /Ran commands/);
assert.equal(liveTurn.querySelector('.activity-turn-duration').textContent.startsWith('Took '), true);
c.uiText = (en, zh) => zh;
c.refreshActivityGroupLanguage();
assert.match(summary(firstGroup).textContent, /执行了命令/);

// Saved JSONL replay has a single turn header even when an intermediate
// answer separates two process sections. Trace metrics belong to that header.
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Please inspect README'}]},
  {role:'assistant', content:[{type:'thinking', text:'Plan the read'}, {type:'tool_use', name:'Read', tool_use_id:'history-read', input:{path:'README.md'}}]},
  {role:'user', content:[{type:'tool_result', tool_use_id:'history-read', content:'saved output'}]},
  {role:'assistant', content:[{type:'text', text:'Intermediate answer'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'history-command', input:{command:'check'}}]},
  {role:'user', content:[{type:'tool_result', tool_use_id:'history-command', content:'check passed'}]},
  {role:'assistant', content:[{type:'text', text:'Saved final answer'}]},
]);
assert.equal(turns().length, 1, 'replay must not retain the previous live turn');
assert.equal(groups().length, 2);
assert.equal(groups()[0].dataset.turnId, groups()[1].dataset.turnId);
assert.equal(groups()[0].dataset.turnId, turns()[0].dataset.turnId);
assert.equal(groups()[0].querySelector('.activity-group-duration'), null);
assert.equal(groups()[1].querySelector('.activity-group-duration'), null);
assert.equal(rows(groups()[0])[0].getAttribute('data-state'), 'ok');
assert.equal(rows(groups()[1])[0].getAttribute('data-state'), 'ok');
assert.equal(area.lastElementChild.querySelector('.message-content').innerHTML, 'Saved final answer');
c.fetch = async () => ({ok:true, json:async () => ({turnMetrics:[{turn:1,durationMs:107000}]})});
await c.restoreHistoryMessageMetadata('A');
assert.match(turns()[0].querySelector('.activity-turn-duration').textContent, /1 分 47 秒/);
assert.equal(area.querySelectorAll('.activity-turn-duration').length, 1);
c.fetch = async () => ({ok:false});

// Tool failure keeps the inner details available. It does not swallow the
// assistant's explanation, and its inner disclosure remains independent.
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Run the check'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'failed-command', input:{command:'check'}}]},
  {role:'user', content:[{type:'tool_result', tool_use_id:'failed-command', content:'permission denied', is_error:true}]},
  {role:'assistant', content:[{type:'text', text:'The check could not run'}]},
]);
const failed = groups()[0];
assert.equal(rows(failed)[0].getAttribute('data-state'), 'error');
assert.equal(failed.dataset.hasError, 'true');
assert.equal(summary(failed).textContent, '执行了命令', 'visible group title stays about the work');
assert.match(summary(failed).getAttribute('aria-label'), /执行了命令 · 1 项失败/);
assert.equal(failed.classList.contains('open'), true, 'errors stay expanded');
c.toggleActivityGroup(summary(failed));
assert.equal(failed.classList.contains('open'), false);
assert.equal(summary(failed).getAttribute('aria-expanded'), 'false');
c.refreshActivityGroupLanguage();
assert.equal(summary(failed).textContent, '执行了命令');
assert.match(summary(failed).getAttribute('aria-label'), /1 项失败/);
c.uiText = (en, zh) => en;
c.refreshActivityGroupLanguage();
assert.equal(summary(failed).textContent, 'Ran commands');
assert.match(summary(failed).getAttribute('aria-label'), /1 failed/);
c.uiText = (en, zh) => zh;
c.refreshActivityGroupLanguage();
c.applyActivityPresentationMode('detailed');
assert.equal(failed.classList.contains('open'), false,
  'language and density changes preserve a deliberate failure-group fold');
c.toggleActivityGroup(summary(failed));
assert.equal(failed.classList.contains('open'), true);
assert.equal(area.lastElementChild.querySelector('.message-content').innerHTML, 'The check could not run');

// Request terminal states are distinct from a normal completed turn. The
// actual SSE lifecycle maps the backend's "stopped" reason to stopped.
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Bash', id:'stopped-command', input:'{}'});
c.connectEvents();
liveCallbacks.loop_done({incomplete:true, stopReason:'stopped'});
assert.equal(turns()[0].dataset.state, 'stopped');
assert.match(turns()[0].querySelector('.activity-turn-state').textContent, /停止/);
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Bash', id:'error-command', input:'{}'});
liveCallbacks.agent_error({message:'provider failed'});
assert.equal(turns()[0].dataset.state, 'error');
assert.match(turns()[0].querySelector('.activity-turn-state').textContent, /失败/);

// SSE can report completion before the HTTP response supplies fallback text.
// Once that visible answer arrives, the completed process should fold.
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Read', id:'fallback-read', input:'{}'});
c.handleToolResult({tool:'Read', id:'fallback-read', output:'read', elapsedMs:3});
liveCallbacks.loop_done({incomplete:false});
const fallbackTurn = turns()[0];
assert.equal(fallbackTurn.dataset.state, 'completed');
assert.equal(fallbackTurn.classList.contains('open'), true, 'answerless completion remains inspectable');
c.addMessage('assistant', 'Fallback answer');
c.finishUserTurn();
assert.equal(fallbackTurn.classList.contains('open'), false, 'POST fallback answer folds the settled turn');
assert.equal(area.lastElementChild.querySelector('.message-content').innerHTML, 'Fallback answer');

// A later pure-text turn creates no process header. Its answer must not be
// mistaken for the preceding answerless turn when changing display density.
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Read', id:'answerless-read', input:'{}'});
c.handleToolResult({tool:'Read', id:'answerless-read', output:'read', elapsedMs:3});
c.finishUserTurn();
const answerlessTurn = turns()[0];
assert.equal(answerlessTurn.classList.contains('open'), true);
c.addMessage('user', 'New question');
c.addMessage('assistant', 'Pure text answer');
c.applyActivityPresentationMode('compact');
c.applyActivityPresentationMode('standard');
assert.equal(answerlessTurn.classList.contains('open'), true,
  'next pure-text answer cannot fold an earlier answerless process');

// A steer is still part of the current turn, so an answer following it can
// legitimately close that turn's process disclosure.
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Read', id:'steered-read', input:'{}'});
c.handleToolResult({tool:'Read', id:'steered-read', output:'read', elapsedMs:3});
c.addMessage('user', 'Adjust this run', true, -1, 0, true);
c.addMessage('assistant', 'Adjusted answer');
c.finishUserTurn();
assert.equal(turns()[0].classList.contains('open'), false,
  'a marked steer must not break its own turn answer boundary');

// If the user inspects an inner process section after SSE completion, the
// late POST answer must not hide that section by folding the outer turn.
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Read', id:'inspected-read', input:'{}'});
c.handleToolResult({tool:'Read', id:'inspected-read', output:'read', elapsedMs:3});
liveCallbacks.loop_done({incomplete:false});
const inspectedTurn = turns()[0];
const inspectedGroup = groups()[0];
assert.equal(inspectedTurn.classList.contains('open'), true);
assert.equal(inspectedGroup.classList.contains('open'), false);
c.toggleActivityGroup(summary(inspectedGroup));
assert.equal(inspectedGroup.classList.contains('open'), true);
c.addMessage('assistant', 'Late answer');
c.finishUserTurn();
assert.equal(inspectedTurn.classList.contains('open'), true, 'late answer preserves the inner inspection');
assert.equal(inspectedGroup.classList.contains('open'), true);

// A second completion must not undo a deliberate manual expansion.
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Read', id:'streamed-read', input:'{}'});
c.handleToolResult({tool:'Read', id:'streamed-read', output:'read', elapsedMs:3});
c.handleTextDelta({delta:'Streamed answer'});
c.endStreamingMessage();
liveCallbacks.loop_done({incomplete:false});
const manuallyOpenedTurn = turns()[0];
assert.equal(manuallyOpenedTurn.classList.contains('open'), false);
c.toggleActivityTurn(turnToggle(manuallyOpenedTurn));
assert.equal(manuallyOpenedTurn.classList.contains('open'), true);
c.finishUserTurn();
assert.equal(manuallyOpenedTurn.classList.contains('open'), true, 'later POST completion preserves manual expansion');

// Replay settles each turn separately: an earlier successful answer folds,
// while the current answerless turn stays open for inspection.
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'First task'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'first-read', input:{path:'one'}}]},
  {role:'user', content:[{type:'tool_result', tool_use_id:'first-read', content:'one'}]},
  {role:'assistant', content:[{type:'text', text:'First answer'}]},
  {role:'user', content:[{type:'text', text:'Second task'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'second-command', input:{command:'two'}}]},
]);
assert.equal(turns().length, 2);
assert.equal(turns()[0].dataset.state, 'finished', 'replay without terminal evidence is neutral');
assert.notEqual(turns()[0].querySelector('.activity-turn-state').textContent, '已完成');
assert.equal(turns()[1].classList.contains('open'), true);
assert.equal(groups()[1].getAttribute('data-turn-collapsed'), null);

// Reopening a stopped session must not turn its last process into a success.
c.currentSessionId = 'status-stopped';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Stop this run'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'stopped-history-command', input:{command:'long'}}]},
]);
assert.equal(turns()[0].dataset.state, 'finished');
assert.equal(turns()[0].querySelector('.activity-turn-duration').textContent, '执行过程');
c.fetch = async url => url.startsWith('/api/trace')
  ? {ok:true, json:async () => ({events:[{kind:'loop_done',turn:1,depth:0,parentID:''}],turnMetrics:[]})}
  : {ok:true, json:async () => ({session:{status:'stopped'}})};
await c.restoreHistoryMessageMetadata('status-stopped');
assert.equal(turns()[0].dataset.state, 'stopped');
assert.equal(turns()[0].classList.contains('open'), true);
assert.match(turns()[0].querySelector('.activity-turn-state').textContent, /已停止/);
c.fetch = async () => ({ok:false});

// The one latest root error marks its own turn. Older turns stay neutral: a
// trace page with limit=1 cannot prove their terminal outcome.
c.currentSessionId = 'status-error';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'First task'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'earlier-read', input:{path:'one'}}]},
  {role:'assistant', content:[{type:'text', text:'First answer'}]},
  {role:'user', content:[{type:'text', text:'Second failed task'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'latest-error', input:{command:'fail'}}]},
  {role:'assistant', content:[{type:'text', text:'Second explanation'}]},
]);
assert.equal(turns().length, 2);
assert.equal(turns()[0].dataset.state, 'finished');
assert.equal(turns()[1].dataset.state, 'finished');
c.fetch = async url => url.startsWith('/api/trace')
  ? {ok:true, json:async () => ({events:[{kind:'error',turn:2,depth:0,parentID:''}],turnMetrics:[]})}
  : {ok:true, json:async () => ({session:{status:'failed'}})};
await c.restoreHistoryMessageMetadata('status-error');
assert.equal(turns()[0].dataset.state, 'finished');
assert.equal(turns()[1].dataset.state, 'error');
assert.equal(turns()[1].classList.contains('open'), true);
c.fetch = async () => ({ok:false});

c.currentSessionId = 'status-completed';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Completed task'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'completed-read', input:{path:'ok'}}]},
  {role:'assistant', content:[{type:'text', text:'Completed answer'}]},
]);
c.fetch = async url => url.startsWith('/api/trace')
  ? {ok:true, json:async () => ({events:[{kind:'loop_done',turn:1,depth:0,parentID:''}],turnMetrics:[]})}
  : {ok:true, json:async () => ({session:{status:'completed'}})};
await c.restoreHistoryMessageMetadata('status-completed');
assert.equal(turns()[0].dataset.state, 'completed');
assert.equal(turns()[0].classList.contains('open'), false);
c.fetch = async () => ({ok:false});

// The session status describes the last user turn even if it has no process
// row; it must not be applied to the preceding process header.
c.currentSessionId = 'status-no-process';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'First process'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'earlier-read', input:{path:'one'}}]},
  {role:'assistant', content:[{type:'text', text:'First answer'}]},
  {role:'user', content:[{type:'text', text:'Last turn'}]},
  {role:'assistant', content:[{type:'text', text:'No process needed'}]},
]);
assert.equal(turns().length, 1);
c.fetch = async url => url.startsWith('/api/trace')
  ? {ok:true, json:async () => ({events:[{kind:'loop_done',turn:1,depth:0,parentID:''}],turnMetrics:[]})}
  : {ok:true, json:async () => ({session:{status:'stopped'}})};
await c.restoreHistoryMessageMetadata('status-no-process');
assert.equal(turns()[0].dataset.state, 'finished', 'last-turn stop does not overwrite an earlier process');
c.fetch = async () => ({ok:false});

// If B is running while the user views idle A, A's replay must not show a
// running process solely because a global turn flag is true.
c.currentSessionId = 'A';
c.turnRunning = true;
c.runningSessionId = 'B';
c.lastStatusSnapshot = {isolatedTurnsEnabled:false};
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Idle A'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'idle-a-read', input:{path:'A'}}]},
]);
assert.equal(turns()[0].dataset.state, 'finished');
assert.notEqual(turns()[0].dataset.state, 'running');
c.fetch = async url => url.startsWith('/api/trace')
  ? {ok:true, json:async () => ({events:[],turnMetrics:[]})}
  : {ok:true, json:async () => ({session:{status:'running'}})};
await c.restoreHistoryMessageMetadata('A');
assert.equal(turns()[0].dataset.state, 'finished', 'B running cannot make idle A look active');
c.fetch = async () => ({ok:false});
c.turnRunning = false;
c.runningSessionId = null;
c.lastStatusSnapshot = null;

// A delayed trace response must not rewrite the same turn number after the
// user changes sessions while the metadata request is in flight.
c.currentSessionId = 'late-A';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Late A'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'late-a-read', input:{path:'A'}}]},
]);
let resolveLateTrace;
c.fetch = url => url.startsWith('/api/trace')
  ? new Promise(resolve => { resolveLateTrace = resolve; })
  : Promise.resolve({ok:true, json:async () => ({session:{status:'stopped'}})});
const pendingLateA = c.restoreHistoryMessageMetadata('late-A');
c.fetch = async () => ({ok:false});
c.currentSessionId = 'late-B';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Late B'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'late-b-command', input:{command:'B'}}]},
]);
resolveLateTrace({ok:true, json:async () => ({events:[{kind:'error',turn:1,depth:0,parentID:''}],turnMetrics:[]})});
await pendingLateA;
assert.equal(turns()[0].dataset.state, 'finished');
assert.equal(rows(groups()[0])[0].getAttribute('data-id'), 'late-b-command');

// Replaying another session discards the old DOM owner. A late SSE event for
// the old session must be rejected before it reaches a renderer.
c.currentSessionId = 'A';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Session A'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'a-read', input:{path:'A'}}]},
  {role:'assistant', content:[{type:'text', text:'A answer'}]},
]);
const oldTurn = turns()[0];
c.currentSessionId = 'B';
c.renderHistoryMessages([
  {role:'user', content:[{type:'text', text:'Session B'}]},
  {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'b-command', input:{command:'B'}}]},
  {role:'assistant', content:[{type:'text', text:'B answer'}]},
]);
assert.equal(oldTurn.isConnected, false);
assert.equal(turns().length, 1);
assert.equal(groups().length, 1);
assert.equal(rows(groups()[0])[0].getAttribute('data-id'), 'b-command');
assert.equal(c.sameSession({session:'A'}), false);
assert.equal(c.sameSession({session:'B'}), true);

// A preference switch applies to the conversation already on screen and to
// the same saved history after a session switch. None of the four densities
// may delete a tool, reasoning row, Artifact, or final answer.
const presentationHistory = [
  {role:'user', content:[{type:'text', text:'Inspect the project'}]},
  {role:'assistant', content:[
    {type:'thinking', text:'Read the source first'},
    {type:'tool_use', name:'Read', tool_use_id:'presentation-read', input:{path:'README.md'}},
  ]},
  {role:'user', content:[{type:'tool_result', tool_use_id:'presentation-read', content:'README contents'}]},
  {role:'assistant', content:[{type:'text', text:'The project is ready'}]},
];
c.fetch = async () => ({ok:false});
c.currentSessionId = 'presentation-history';
for (const mode of ['compact', 'standard', 'detailed', 'verbose']) {
  c.applyActivityPresentationMode(mode);
  c.renderHistoryMessages(presentationHistory);
  const turn = turns()[0], group = groups()[0];
  assert.equal(root.dataset.presentationMode, mode, mode + ' preference reaches the transcript');
  assert.equal(turns().length, 1);
  assert.equal(groups().length, 1);
  assert.equal(rows(group).length, 1, mode + ' keeps the saved tool call');
  assert.equal(group.querySelectorAll('.think-row').length, 1, mode + ' keeps saved reasoning');
  assert.equal(area.lastElementChild.querySelector('.message-content').innerHTML, 'The project is ready');
  assert.equal(turn.classList.contains('open'), mode === 'verbose', mode + ' completed-turn disclosure');
  assert.equal(group.dataset.presentationFlat === 'true', mode === 'verbose', mode + ' saved-process layout');
  assert.equal(summary(group).getAttribute('aria-hidden'), mode === 'verbose' ? 'true' : 'false');
  assert.equal(group.classList.contains('open'), false, mode + ' saved process starts compact');
}

// Compact hides a running tool's raw detail from its summary, but keeps its
// row and every other artifact of the run. Standard restores the live detail;
// detailed and verbose flatten the running activity without deleting it.
c.renderHistoryMessages([]);
c.currentSessionId = 'presentation-live';
c.applyActivityPresentationMode('compact');
c.handleThinkingDelta({delta:'Consider the README'});
c.handleToolStart({tool:'Bash', id:'presentation-live-command', input:'{"description":"VISIBLE_LIVE_DETAIL"}'});
const presentationLiveTurn = turns()[0];
const presentationLiveGroup = groups()[0];
const presentationArtifact = new Element('article');
presentationArtifact.className = 'artifact-chat-card';
area.appendChild(presentationArtifact);
assert.equal(rows(presentationLiveGroup).length, 1);
assert.equal(presentationLiveGroup.querySelectorAll('.think-row').length, 1);
assert.equal(summary(presentationLiveGroup).textContent.includes('VISIBLE_LIVE_DETAIL'), false,
  'compact running summary omits command detail');
assert.equal(presentationLiveTurn.classList.contains('open'), true);
c.applyActivityPresentationMode('standard');
assert.match(summary(presentationLiveGroup).textContent, /VISIBLE_LIVE_DETAIL/,
  'changing preference updates the current live summary');
assert.equal(presentationLiveGroup.dataset.presentationFlat, 'false');
c.applyActivityPresentationMode('detailed');
assert.equal(presentationLiveGroup.dataset.presentationFlat, 'true',
  'detailed shows the live process inline');
assert.equal(summary(presentationLiveGroup).getAttribute('aria-hidden'), 'true');
c.applyActivityPresentationMode('verbose');
assert.equal(presentationLiveGroup.dataset.presentationFlat, 'true');
assert.equal(presentationLiveTurn.classList.contains('open'), true);
assert.equal(presentationArtifact.parentElement, area, 'mode changes keep the Artifact outside process folds');
c.handleToolResult({tool:'Bash', id:'presentation-live-command', output:'ok', elapsedMs:2});
c.finishThinking();
c.addMessage('assistant', 'Live answer');
c.finishUserTurn();
assert.equal(presentationLiveTurn.classList.contains('open'), true,
  'verbose leaves the completed process visible');
assert.equal(rows(presentationLiveGroup).length, 1);
assert.equal(presentationArtifact.parentElement, area);
c.applyActivityPresentationMode('compact');
assert.equal(presentationLiveTurn.classList.contains('open'), false,
  'compact folds an answered turn when the preference changes in place');
assert.equal(presentationLiveGroup.dataset.presentationFlat, 'false');
assert.equal(presentationLiveGroup.querySelectorAll('.think-row').length, 1,
  'compact presentation retains reasoning for later inspection');
assert.equal(area.lastElementChild.querySelector('.message-content').innerHTML, 'Live answer');

// Detailed mode lays out a live run in place, then returns it to the same
// compact historical process section once the final answer has arrived.
c.applyActivityPresentationMode('detailed');
c.renderHistoryMessages([]);
c.handleToolStart({tool:'Read', id:'detailed-lifecycle', input:'{"path":"README.md"}'});
const detailedTurn = turns()[0], detailedGroup = groups()[0];
assert.equal(detailedGroup.dataset.presentationFlat, 'true');
c.handleToolResult({tool:'Read', id:'detailed-lifecycle', output:'ok', elapsedMs:2});
c.addMessage('assistant', 'Detailed answer');
c.finishUserTurn();
assert.equal(detailedTurn.classList.contains('open'), false);
assert.equal(detailedGroup.dataset.presentationFlat, 'false');
assert.equal(detailedGroup.classList.contains('open'), false);
assert.equal(rows(detailedGroup).length, 1);

// A user-expanded saved turn remains expanded when the density setting
// changes; a preference update must not erase an explicit inspection.
c.applyActivityPresentationMode('standard');
c.renderHistoryMessages(presentationHistory);
const inspectedPresentationTurn = turns()[0];
c.toggleActivityTurn(turnToggle(inspectedPresentationTurn));
assert.equal(inspectedPresentationTurn.classList.contains('open'), true);
for (const mode of ['compact', 'detailed', 'verbose', 'standard']) {
  c.applyActivityPresentationMode(mode);
  assert.equal(inspectedPresentationTurn.classList.contains('open'), true,
    mode + ' preserves manually expanded saved activity');
}

// A failure or stop always exposes its evidence, including after a preference
// change. A deliberate expansion survives the SSE terminal event and the
// later HTTP fallback answer in each compacting mode.
for (const mode of ['compact', 'standard', 'detailed', 'verbose']) {
  c.applyActivityPresentationMode(mode);
  c.renderHistoryMessages([]);
  c.handleToolStart({tool:'Bash', id:'failed-' + mode, input:'{}'});
  liveCallbacks.agent_error({message:'provider failed'});
  assert.equal(turns()[0].dataset.state, 'error');
  assert.equal(turns()[0].classList.contains('open'), true, mode + ' failure stays inspectable');
  assert.equal(groups()[0].classList.contains('open'), true, mode + ' failure exposes the command');
  c.renderHistoryMessages([]);
  c.handleToolStart({tool:'Bash', id:'stopped-' + mode, input:'{}'});
  liveCallbacks.loop_done({incomplete:true, stopReason:'stopped'});
  assert.equal(turns()[0].dataset.state, 'stopped');
  assert.equal(turns()[0].classList.contains('open'), true, mode + ' stop stays inspectable');
  assert.equal(groups()[0].classList.contains('open'), true, mode + ' stop exposes the command');
}
for (const mode of ['compact', 'standard', 'detailed']) {
  c.applyActivityPresentationMode(mode);
  c.renderHistoryMessages([]);
  c.handleToolStart({tool:'Read', id:'race-' + mode, input:'{}'});
  c.handleToolResult({tool:'Read', id:'race-' + mode, output:'read', elapsedMs:2});
  liveCallbacks.loop_done({incomplete:false});
  const turn = turns()[0], group = groups()[0];
  assert.equal(turn.classList.contains('open'), true, mode + ' answerless SSE completion stays visible');
  c.toggleActivityGroup(summary(group));
  assert.equal(group.classList.contains('open'), true);
  c.addMessage('assistant', 'HTTP fallback answer');
  c.finishUserTurn();
  assert.equal(turn.classList.contains('open'), true,
    mode + ' later POST answer preserves deliberate process inspection');
  assert.equal(group.classList.contains('open'), true);
}
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("activity-group browser lifecycle: %v\n%s", err, output)
	}
}

// The preference API returns a full document, while settings buttons save
// individual fields. Exercise the real client save/selection functions with
// delayed responses so an older reply cannot undo the last visible choice.
func TestActivityPresentationPreferenceRace(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	appSource, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	chatSource, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"app": string(appSource), "chat": string(chatSource)})
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function extract(source, start, end) {
  const a = source.indexOf(start), b = source.indexOf(end, a + start.length);
  assert(a >= 0 && b > a, 'missing browser function boundary ' + start);
  return source.slice(a, b);
}
function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return {promise, resolve};
}
const root = {dataset:{}, lang:'zh-CN'};
const c = {
  document: {documentElement:root, querySelectorAll:() => []},
  desktopPreferences: {presentationMode:'standard', language:'zh-CN', sessionOrder:[]},
  uiText:(en, zh) => zh, escHtml:value => String(value), escAttr:value => String(value),
  showToast() {}, applyLanguage(value) { root.lang = value; },
  renderSettingsTab() {},
};
vm.createContext(c);
vm.runInContext('const desktopPreferenceKeysEditedDuringInitialLoad = new Set();', c);
vm.runInContext(extract(sources.app, 'async function initDesktopPreferences()', 'const DESKTOP_I18N'), c);
vm.runInContext(extract(sources.app, 'async function saveDesktopPreference(', 'function escAttr('), c);
vm.runInContext(extract(sources.chat, 'const ACTIVITY_PRESENTATION_POLICIES', '// Keep process events'), c);
vm.runInContext(extract(sources.chat, 'function renderPresentationModePreference()', 'function renderDesktopParallelismPreference()'), c);
vm.runInContext(extract(sources.chat, 'async function chooseLanguage(', 'function renderProvidersTab()'), c);
c.renderSettingsTab = () => { c.settingsMarkup = c.renderPresentationModePreference(); };
function expectVisible(mode) {
  assert.equal(root.dataset.presentationMode, mode, 'active transcript density');
  const checked = c.settingsMarkup.match(/aria-pressed="true"[^>]*onclick="choosePresentationMode\('([^']+)'\)"/);
  assert.equal(checked?.[1], mode, 'selected settings button');
}
function fakeResponse(snapshot, ok = true) {
  return {ok, json:async () => snapshot};
}

(async () => {
  let persisted = {presentationMode:'standard', language:'zh-CN'};
  const staleInitialGet = deferred();
  c.fetch = async (_url, options) => options?.method === 'POST'
    ? (persisted = {...persisted, ...JSON.parse(options.body)}, fakeResponse({...persisted}))
    : {ok:true, json:() => staleInitialGet.promise};
  const initialLoad = c.initDesktopPreferences();
  await c.choosePresentationMode('verbose');
  staleInitialGet.resolve({presentationMode:'standard', language:'zh-CN', sidebarView:'flat'});
  await initialLoad;
  expectVisible('verbose');
  assert.equal(c.desktopPreferences.presentationMode, 'verbose', 'late initial GET cannot undo a saved mode');
  assert.equal(c.desktopPreferences.sidebarView, 'flat', 'untouched fields still load from initial GET');
  c.applyActivityPresentationMode('standard');
  vm.runInContext("savedPresentationMode = 'standard'", c);
  persisted.presentationMode = 'standard';
  const writes = [];
  const firstResponse = deferred();
  c.fetch = async (_url, options) => {
    const patch = JSON.parse(options.body);
    writes.push(patch);
    persisted = {...persisted, ...patch};
    const snapshot = {...persisted};
    return writes.length === 1
      ? {ok:true, json:() => firstResponse.promise}
      : fakeResponse(snapshot);
  };
  const firstClick = c.choosePresentationMode('compact');
  const lastClick = c.choosePresentationMode('verbose');
  expectVisible('verbose');
  assert.equal(writes.length, 1, 'second mode write waits for the first response');
  firstResponse.resolve({presentationMode:'compact', language:'zh-CN'});
  await firstClick;
  await lastClick;
  expectVisible('verbose');
  assert.equal(c.desktopPreferences.presentationMode, 'verbose');
  assert.equal(persisted.presentationMode, 'verbose', 'last click is the persisted value');
  assert.deepEqual(writes.map(write => write.presentationMode), ['compact', 'verbose']);

  // A language save began while compact was current. Its old full-document
  // response arrives after a newer mode save and must not restore compact.
  c.fetch = async (_url, options) => {
    const patch = JSON.parse(options.body);
    persisted = {...persisted, ...patch};
    return fakeResponse({...persisted});
  };
  await c.choosePresentationMode('compact');
  expectVisible('compact');
  const oldLanguageResponse = deferred();
  c.fetch = async (_url, options) => {
    const patch = JSON.parse(options.body);
    persisted = {...persisted, ...patch};
    const snapshot = {...persisted};
    return patch.language
      ? {ok:true, json:() => oldLanguageResponse.promise}
      : fakeResponse(snapshot);
  };
  const languageSave = c.chooseLanguage('en');
  await c.choosePresentationMode('verbose');
  expectVisible('verbose');
  assert.equal(persisted.presentationMode, 'verbose');
  oldLanguageResponse.resolve({presentationMode:'compact', language:'en'});
  await languageSave;
  expectVisible('verbose');
  assert.equal(c.desktopPreferences.presentationMode, 'verbose', 'old language response cannot roll back density');
  assert.equal(c.desktopPreferences.language, 'en');
  assert.equal(persisted.presentationMode, 'verbose');

  // A failed optimistic click must restore the last confirmed preference,
  // both in the settings control and in the active conversation.
  c.fetch = async () => fakeResponse({error:'rejected'}, false);
  await c.choosePresentationMode('compact');
  expectVisible('verbose');
  assert.equal(c.desktopPreferences.presentationMode, 'verbose', 'HTTP 500 restores saved mode');
  c.fetch = async () => { throw new Error('offline'); };
  await c.choosePresentationMode('detailed');
  expectVisible('verbose');
  assert.equal(c.desktopPreferences.presentationMode, 'verbose', 'network failure restores saved mode');
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("activity presentation preference race: %v\n%s", err, output)
	}
}
