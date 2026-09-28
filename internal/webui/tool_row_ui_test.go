package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

// Keep the tool-row interaction contract under the same live renderer used by
// Desktop. This small DOM shim exercises generated controls and dispatched UI
// events, rather than asserting that particular strings exist in chat.js.
func TestToolRowBrowserInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{
		"keyboard_disclosure",
		"inspect_and_language",
		"subagent_semantic_rows",
		"subagent_stage_lifecycle",
		"subagent_background_lifecycle_across_turns",
		"subagent_reused_provider_id_trace_binding",
		"subagent_trace_miss_does_not_bind_old_row",
		"subagent_known_child_without_trace_cross_turn",
		"subagent_history_replay",
		"subagent_history_terminal_refresh",
		"subagent_history_terminal_race",
		"stopped_tool",
		"failed_tool_icon",
		"completed_without_tool_result",
		"history_stopped_after_metadata",
		"history_error_after_metadata",
		"reused_provider_id_same_turn",
		"reused_provider_id_ambiguous_no_trace",
		"reused_provider_id_trace_out_of_order",
		"interleaved_args_same_provider_id",
		"reused_provider_id_across_turns",
		"reused_provider_id_history_replay",
		"history_rebuild_clears_old_detail",
		"empty_provider_id_provisional_and_ambiguous",
		"provisional_id_different_tool_name",
		"history_empty_provider_id_unique",
		"history_empty_provider_id_ambiguous",
		"result_only_failure_creates_tool_row",
		"result_only_adopts_unique_provisional",
		"result_only_does_not_adopt_other_traced_preview",
		"result_only_multiple_candidates_stays_unattributed",
		"incomplete_provisional_not_reused",
		"legacy_result_tool_name_mismatch",
		"reconnect_adopts_unique_history_row",
		"reconnect_adopts_traced_running_history_row",
		"reconnect_does_not_guess_duplicate_history_rows",
		"reconnect_deduplicates_completed_history_row",
	} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(node, "-e", toolRowBrowserScript, scenario)
			cmd.Stdin = bytes.NewReader(source)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("tool-row browser interaction: %v\n%s", err, output)
			}
		})
	}
}

const toolRowBrowserScript = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const scenario = process.argv[1];
function extract(start, end) {
  const a = source.indexOf(start), b = source.indexOf(end, a + start.length);
  assert(a >= 0 && b > a, 'missing browser function boundary: ' + start);
  return source.slice(a, b);
}

let area;
let context;
const frames = [];
const toasts = [];
const decodeAttribute = value => String(value)
  .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<')
  .replace(/&gt;/g, '>').replace(/&amp;/g, '&');
class Element {
  constructor(tagName = 'div') {
    this.tagName = tagName.toUpperCase();
    this.parentElement = null;
    this.children = [];
    this.dataset = {};
    this.attrs = {};
    this.classes = new Set();
    this._html = '';
    this.textContent = '';
    this.style = {};
    this.scrollTop = this.scrollHeight = this.clientHeight = 0;
    this.listeners = new Map();
    this.inline = {};
    this.classList = {
      add: name => this.classes.add(name),
      remove: name => this.classes.delete(name),
      contains: name => this.classes.has(name),
      toggle: (name, force) => {
        const on = force === undefined ? !this.classes.has(name) : !!force;
        if (on) this.classes.add(name); else this.classes.delete(name);
        return on;
      },
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
  appendChild(child) {
    if (child.parentElement) child.remove();
    child.parentElement = this; this.children.push(child); return child;
  }
  remove() {
    if (!this.parentElement) return;
    const siblings = this.parentElement.children;
    siblings.splice(siblings.indexOf(this), 1);
    this.parentElement = null;
  }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }
  dispatch(type, event = {}) {
    event.type = type;
    event.target ||= this;
    event.defaultPrevented ||= false;
    event.preventDefault ||= () => { event.defaultPrevented = true; };
    event.stopPropagation ||= () => { event.propagationStopped = true; };
    for (let node = this; node; node = node.parentElement) {
      event.currentTarget = node;
      for (const listener of node.listeners.get(type) || []) listener(event);
      if (node.inline[type]) {
        context.__uiTarget = node;
        context.event = event;
        vm.runInContext('(function(){' + node.inline[type] + '}).call(__uiTarget)', context);
      }
      if (event.propagationStopped) break;
    }
    // A real <button> receives a click from Enter or Space without an app
    // keydown handler. Model that browser behavior for the native control.
    if (type === 'keydown' && this.tagName === 'BUTTON' && !event.defaultPrevented &&
        (event.key === 'Enter' || event.key === ' ')) this.dispatch('click');
    return event;
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
  set innerHTML(html) {
    this._html = String(html);
    this.children.forEach(child => child.parentElement = null);
    this.children = [];
    if (this.classList.contains('activity-turn')) {
      const toggle = new Element('button'); toggle.className = 'activity-turn-toggle';
      toggle.setAttribute('aria-expanded', this._html.match(/aria-expanded="(true|false)"/)?.[1] || 'true');
      for (const name of ['activity-turn-duration', 'activity-turn-state']) {
        const child = new Element('span'); child.className = name; toggle.appendChild(child);
      }
      this.appendChild(toggle);
    } else if (this.classList.contains('activity-group')) {
      const summary = new Element('button'); summary.className = 'activity-group-summary';
      summary.setAttribute('aria-expanded', this._html.match(/aria-expanded="(true|false)"/)?.[1] || 'true');
      this.appendChild(summary);
      const body = new Element(); body.className = 'activity-group-body'; this.appendChild(body);
      const items = new Element(); items.className = 'activity-group-items'; body.appendChild(items);
    }
  }
  get innerHTML() { return this._html; }
  insertAdjacentHTML(position, html) {
    assert.equal(position, 'beforeend');
    if (/class="message message-(user|assistant)"/.test(html)) {
      const role = html.match(/class="message message-(user|assistant)"/)?.[1];
      const message = new Element(); message.className = 'message message-' + role;
      const outer = html.match(/<div class="message message-(?:user|assistant)"([^>]*)>/)?.[1] || '';
      for (const [, key, value] of outer.matchAll(/\b(data-[\w-]+)="([^"]*)"/g)) message.setAttribute(key, decodeAttribute(value));
      const content = new Element(); content.className = role === 'user' ? 'message-bubble' : 'message-content';
      const contentName = role === 'user' ? 'message-bubble' : 'message-content';
      content.textContent = html.match(new RegExp('class="' + contentName + '">([\\s\\S]*?)<\\/div>'))?.[1] || '';
      message.appendChild(content); this.appendChild(message); return;
    }
    assert.match(html, /class="call-row"/, 'expected live tool row');
    const row = new Element(); row.className = 'call-row';
    const outer = html.match(/<div class="call-row"([^>]*)>/)?.[1] || '';
    for (const [, key, value] of outer.matchAll(/\b(data-[\w-]+)="([^"]*)"/g)) row.setAttribute(key, decodeAttribute(value));
    const header = new Element(); header.className = 'tc-row'; row.appendChild(header);
    const opening = html.match(/<div class="tc-row"([^>]*)>/)?.[1] || '';
    for (const [, key, value] of opening.matchAll(/\b([\w-]+)="([^"]*)"/g)) {
      header.setAttribute(key, value);
      if (key === 'onclick') header.inline.click = value;
      if (key === 'onkeydown') header.inline.keydown = value;
    }
    const disclosureTag = html.match(/<button class="tc-disclosure"([^>]*)>/);
    const disclosure = disclosureTag ? new Element('button') : header;
    if (disclosureTag) {
      disclosure.className = 'tc-disclosure'; header.appendChild(disclosure);
      for (const [, key, value] of disclosureTag[1].matchAll(/\b([\w-]+)="([^"]*)"/g)) {
        disclosure.setAttribute(key, value);
        if (key === 'onclick') disclosure.inline.click = value;
        if (key === 'onkeydown') disclosure.inline.keydown = value;
      }
    }
    const leading = new Element('span'); leading.className = 'tc-leading'; disclosure.appendChild(leading);
    leading.innerHTML = html.match(/<span class="tc-leading">([\s\S]*?)<\/span>\s*<span class="tc-title">/)?.[1] || '';
    for (const name of ['tc-title', 'tc-summary', 'tc-time']) {
      const child = new Element('span'); child.className = name; disclosure.appendChild(child);
      const raw = html.match(new RegExp('<span class="' + name + '">([\\s\\S]*?)<\\/span>'))?.[1] || '';
      child.textContent = raw;
    }
    const inspectTag = html.match(/<button class="tc-inspect"([^>]*)>([^<]*)<\/button>/);
    if (inspectTag) {
      const inspect = new Element('button'); inspect.className = 'tc-inspect';
      inspect.textContent = inspectTag[2];
      for (const [, key, value] of inspectTag[1].matchAll(/\b([\w-]+)="([^"]*)"/g)) {
        inspect.setAttribute(key, value);
        if (key === 'onclick') inspect.inline.click = value;
      }
      header.appendChild(inspect);
    }
    const agentTag = html.match(/<button class="tc-open-agent"([^>]*)>([^<]*)<\/button>/);
    if (agentTag) {
      const openAgent = new Element('button'); openAgent.className = 'tc-open-agent';
      openAgent.textContent = agentTag[2]; openAgent.hidden = agentTag[1].includes('hidden');
      for (const [, key, value] of agentTag[1].matchAll(/\b([\w-]+)="([^"]*)"/g)) {
        openAgent.setAttribute(key, value);
        if (key === 'onclick') openAgent.inline.click = value;
      }
      header.appendChild(openAgent);
    }
    const body = new Element(); body.className = 'tc-body'; row.appendChild(body);
    for (const section of ['in', 'out']) {
      const sec = new Element(); sec.className = 'tc-io-sec'; body.appendChild(sec);
      const label = new Element('span'); label.className = 'tc-io-label'; sec.appendChild(label);
      const labelRe = section === 'in'
        ? /<span class="tc-io-label">([^<]*)<\/span><span class="tc-io-text" data-in>/
        : /<span class="tc-io-label">([^<]*)<\/span><span class="tc-io-text" data-out>/;
      label.textContent = html.match(labelRe)?.[1] || '';
      const text = new Element('span'); text.className = 'tc-io-text';
      text.setAttribute('data-' + section, ''); sec.appendChild(text);
      text.textContent = html.match(new RegExp('class="tc-io-text" data-' + section + '>([^<]*)<'))?.[1] || '';
    }
    this.appendChild(row);
  }
  matches(selector) {
    if (selector.includes(' ')) return false;
    const klass = selector.match(/^\.([\w-]+)/)?.[1];
    if (klass && !this.classList.contains(klass)) return false;
    for (const [, attr, value] of selector.matchAll(/\[([\w-]+)(?:="([^"]*)")?\]/g)) {
      const current = this.getAttribute(attr);
      if (current === null || (value !== undefined && current !== value)) return false;
    }
    return true;
  }
  querySelectorAll(selector) {
    const selectors = selector.split(',').map(s => s.trim());
    const found = [];
    const walk = node => {
      for (const child of node.children) {
        if (selectors.some(s => child.matches(s))) found.push(child);
        walk(child);
      }
    };
    walk(this); return found;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) {
    for (let node = this; node; node = node.parentElement) if (node.matches(selector)) return node;
    return null;
  }
}

area = new Element();
const app = new Element(); app.className = 'app details-closed';
const details = Object.fromEntries(['detailsPlaceholder', 'detailsTabs', 'detailsBody'].map(id => [id, new Element()]));
context = {
  Map, Set, WeakMap, WeakSet, Date, console,
  requestAnimationFrame: callback => frames.push(callback),
  queueMicrotask: callback => callback(),
  document: {
    getElementById: id => id === 'chatArea' ? area : details[id] || null,
    createElement: tag => new Element(tag),
    querySelector: selector => selector === '.app' ? app : area.querySelector(selector),
    querySelectorAll: selector => area.querySelectorAll(selector),
  },
  lang: 'zh', uiText: (en, zh) => context.lang === 'zh' ? zh : en,
  escHtml: value => String(value),
  escAttr: value => String(value).replace(/&/g, '&amp;').replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;').replace(/</g, '&lt;').replace(/>/g, '&gt;'),
  escOnclick: value => String(value),
  fmtMs: ms => ms + 'ms',
  isTodoWriteTool: () => false, isPlanningTool: () => false,
  renderSearchCard() {}, renderFileEditCard() {},
  autoScroll() {}, finishThinking() {}, endStreamingMessage() {}, endTurnStatus() {}, showTurnStatsLine() {}, beginTurnStatus() {}, refreshTurnStatusLanguage() {}, refreshTurnMetricsLanguage() {},
  updateEmptyLayout() {}, resumeAutoScroll() {}, restoreTodoPlanFromHistory() {}, loadSessionFiles() {},
  messageActionsMarkup: () => '', formatContent: value => String(value),
  showToast: message => toasts.push(String(message)),
  turnStartMs: 0, streaming: false, toolDetails: {}, selectedToolId: null, detailTab: 'summary',
  currentSessionId: 'A', turnRunning: false, runningSessionId: null, pendingForegroundRequest: null,
  messages: [], fetch: async () => ({ok:false}),
  openedAgents: [],
  openSubAgentDetails: (id, trigger, owner) => context.openedAgents.push({id, trigger, owner}),
};
vm.createContext(context);
vm.runInContext(extract('let thinkingEl = null;', 'const THINK_ORBIT_ICON'), context);
vm.runInContext(extract('function openToolDetail(id)', '// DSH ToolRow parity'), context);
vm.runInContext(extract('const TOOL_VARIANTS =', 'const FILE_DIFF_MAX_LINES'), context);
vm.runInContext(extract('function toolRowsInCurrentTurn(', '// Search card'), context);
vm.runInContext(extract('function finishUserTurn(', '// Called at the start of each user turn'), context);
vm.runInContext(extract('function beginUserTurn()', '// Reset every piece of in-flight turn state.'), context);
vm.runInContext(extract('function addMessage(', '// Attach the hover actions row'), context);
vm.runInContext(extract('const INTERNAL_TRANSCRIPT_SECTION_RE', '// Rebuild the full transcript'), context);
vm.runInContext(extract('function refreshHistoricalSubagentStatuses(', 'function restoreHistoryTurnState('), context);
vm.runInContext(extract('function restoreHistoryTurnState(', 'function showError('), context);
function flushFrames() {
  for (let i = 0; frames.length; i++) {
    assert(i < 100, 'animation frames must settle');
    const callback = frames.shift();
    assert.equal(typeof callback, 'function');
    callback();
  }
}
function newRow(id, description = 'Check repository', traceCallId = '') {
  const before = area.querySelectorAll('.call-row').length;
  context.handleToolStart({tool:'Bash', id, traceCallId, input:JSON.stringify({description})});
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, before + 1, 'each tool_start creates one independent row');
  const row = rows.at(-1);
  assert(row, 'live tool row was not rendered');
  return row;
}
function rowKey(row) {
  return row.getAttribute('data-row-key') || row.getAttribute('data-id');
}
function inspectRow(row) {
  const button = row.querySelector('.tc-inspect');
  assert(button, 'tool row has Inspect');
  button.dispatch('click');
  assert.equal(context.selectedToolId, rowKey(row), 'Inspect selects the clicked row');
}
function visibleToolText(row) {
  const visit = node => node.textContent + node.children.map(visit).join(' ');
  return visit(row) + ' ' + row.querySelector('.tc-leading').innerHTML;
}
function visibleText(node) {
  return node.textContent + node.children.map(visibleText).join(' ');
}
function assertNeutralNoResult(row, message) {
  const state = row.getAttribute('data-state');
  assert.notEqual(state, 'running', message + ': stale running state');
  assert.notEqual(state, 'stopped', message + ': must not invent an interruption');
  assert.notEqual(state, 'error', message + ': must not invent a failure');
  assert.match(visibleToolText(row), /未收到工具结果|未收到结果|未记录工具结果|结果未知|无结果|No result|Result unavailable/i,
    message + ': explain the unknown result in the visible row');
}

async function runScenario() {
if (scenario === 'keyboard_disclosure') {
  const row = newRow('keyboard');
  const control = row.querySelector('.tc-disclosure') || row.querySelector('.tc-row');
  assert(control.tagName === 'BUTTON' ||
    (control.getAttribute('role') === 'button' && control.getAttribute('tabindex') === '0'),
    'tool disclosure is keyboard focusable');
  assert.equal(control.getAttribute('aria-expanded'), 'false', 'collapsed tool exposes its disclosure state');
  control.dispatch('keydown', {key:'Enter'});
  assert.equal(row.classList.contains('open'), true, 'Enter expands the tool output');
  assert.equal(control.getAttribute('aria-expanded'), 'true');
  control.dispatch('keydown', {key:' '});
  assert.equal(row.classList.contains('open'), false, 'Space collapses the tool output');
  assert.equal(control.getAttribute('aria-expanded'), 'false');
} else if (scenario === 'inspect_and_language') {
  const row = newRow('inspect');
  const control = row.querySelector('.tc-disclosure') || row.querySelector('.tc-row');
  const inspect = row.querySelector('.tc-inspect');
  assert(inspect, 'tool row exposes a separate Inspect control');
  assert.match(inspect.textContent, /[\u4e00-\u9fff]/, 'Chinese UI translates Inspect');
  let labels = row.querySelectorAll('.tc-io-label').map(label => label.textContent);
  assert.deepEqual(labels, ['输入', '输出'], 'Chinese UI translates tool input and output labels');
  inspect.dispatch('click');
  assert.equal(context.selectedToolId, rowKey(row), 'Inspect opens the selected detail');
  assert.equal(row.classList.contains('open'), false, 'Inspect does not expand the inline body');
  assert.equal(control.getAttribute('aria-expanded'), 'false');
  assert.equal(app.classList.contains('details-closed'), false);
  context.lang = 'en';
  context.refreshActivityGroupLanguage();
  assert.match(inspect.textContent, /Inspect|View details/, 'English UI translates Inspect');
  labels = row.querySelectorAll('.tc-io-label').map(label => label.textContent);
  assert.deepEqual(labels, ['IN', 'OUT'], 'English UI uses short tool input/output labels');
} else if (scenario === 'subagent_semantic_rows') {
  const add = (tool, id, input) => {
    context.handleToolStart({tool, id, input:JSON.stringify(input)});
    flushFrames();
    return area.querySelectorAll('.call-row').at(-1);
  };
  const secret = 'DO_NOT_SHOW_PRIVATE_PROMPT_OR_MESSAGE_BODY';
  const agent = add('Agent', 'agent-1', {description:'Inspect chart', prompt:secret, name:'reviewer'});
  assert.match(visibleToolText(agent), /启动子代理.*Inspect chart/);
  assert.doesNotMatch(agent.querySelector('.tc-summary').textContent, /DO_NOT_SHOW/);
  const unnamed = add('Agent', 'agent-2', {prompt:secret});
  assert.match(unnamed.querySelector('.tc-summary').textContent, /准备任务/);
  const teammate = add('MessageTeammate', 'message-1', {to:'reviewer', body:secret});
  assert.match(visibleToolText(teammate), /给子代理发消息.*reviewer/);
  assert.doesNotMatch(teammate.querySelector('.tc-summary').textContent, /DO_NOT_SHOW/);
  const external = add('SendMessage', 'external-1', {channel:'slack', text:secret});
  assert.match(visibleToolText(external), /发送外部消息.*外部频道/);
  assert.doesNotMatch(external.querySelector('.tc-summary').textContent, /DO_NOT_SHOW/);
  context.handleToolResult({tool:'MessageTeammate', id:'message-1', isError:true, output:'private ' + secret});
  assert.match(teammate.querySelector('.tc-summary').textContent, /消息发送失败/);
  assert.doesNotMatch(teammate.querySelector('.tc-summary').textContent, /DO_NOT_SHOW/);
  const list = add('SubAgentList', 'list-1', {});
  assert.match(visibleToolText(list), /查看子代理.*当前代理/);
  context.lang = 'en';
  context.refreshActivityGroupLanguage();
  assert.match(visibleToolText(agent), /Start sub-agent.*Inspect chart/);
  assert.match(visibleToolText(teammate), /Message sub-agent.*Message failed/);
  assert.match(visibleToolText(external), /Send external message.*External channel/);
  assert.match(visibleToolText(list), /View sub-agents.*Current agents/);
  assert.match(teammate.querySelector('.tc-summary').textContent, /Message failed/);
} else if (scenario === 'subagent_stage_lifecycle') {
  const input = JSON.stringify({description:'Review code', prompt:'private task body'});
  context.handleToolStart({tool:'Agent', id:'parent-1', session:'A', input});
  flushFrames();
  const row = area.querySelector('.call-row');
  const stage = area.querySelector('.subagent-stage');
  assert(stage, 'Agent call inserts one compact stage into its process group');
  assert.match(stage.textContent, /正在协调子代理.*1 个/);
  const button = row.querySelector('.tc-open-agent');
  assert(button.hidden, 'no link is shown before stable backend identity arrives');
  context.handleSubagentLifecycle({session:'A', sessionId:'A', parentToolUseId:'parent-1',
    agentId:'child-1', name:'reviewer', background:false}, 'running');
  assert.equal(button.hidden, false, 'structured lifecycle reveals direct detail link');
  button.dispatch('click');
  assert.equal(context.openedAgents.length, 1);
  assert.equal(context.openedAgents[0].id, 'child-1');
  assert.equal(context.openedAgents[0].owner, 'A');
  context.handleToolResult({tool:'Agent', id:'parent-1', output:'review complete', presentation:{subagent:{
    sessionId:'A', parentToolUseId:'parent-1', agentId:'child-1', name:'reviewer', status:'completed'}}});
  assert.match(stage.textContent, /已协调子代理.*1 个/);
  assert.equal(area.querySelectorAll('.subagent-stage').length, 1, 'start and result share one stage');
  context.handleToolStart({tool:'Agent', id:'parent-2', session:'A', input});
  flushFrames();
  assert.equal(area.querySelectorAll('.subagent-stage').length, 1, 'second Agent in same process section reuses heading');
  assert.match(stage.textContent, /正在协调子代理.*2 个/);
  const second = area.querySelectorAll('.call-row').at(-1);
  context.handleSubagentLifecycle({session:'B', sessionId:'B', parentToolUseId:'parent-2', agentId:'foreign'}, 'running');
  assert.equal(second.querySelector('.tc-open-agent').hidden, true, 'another session cannot bind a detail link');
  context.handleToolResult({tool:'Agent', id:'parent-2', output:'failed', isError:true});
  assert.match(stage.textContent, /1 项失败/);
  context.finishActivityGroup();
  const artifact = new Element(); artifact.className = 'artifact-chat-card'; area.appendChild(artifact);
  context.handleToolStart({tool:'Agent', id:'parent-3', session:'A', input});
  flushFrames();
  const groups = area.querySelectorAll('.activity-group');
  assert.equal(groups.length, 2, 'an Artifact splits process sections without changing the turn');
  assert.equal(groups[0].dataset.turnId, groups[1].dataset.turnId);
  assert.equal(artifact.parentElement, area, 'Artifact stays a peer in transcript order');
  assert(groups[1].querySelector('.subagent-stage'), 'later Agent stays in the later process section');
  context.lang = 'en';
  context.refreshActivityGroupLanguage();
  assert.match(stage.textContent, /Sub-agent work.*2 agents.*1 failed/);
} else if (scenario === 'subagent_background_lifecycle_across_turns') {
  context.handleToolStart({tool:'Agent', id:'background-parent', session:'A', traceCallId:'background-trace',
    input:JSON.stringify({description:'Review later', run_in_background:true})});
  flushFrames();
  const oldRow = area.querySelector('.call-row');
  const oldStage = area.querySelector('.subagent-stage');
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'background-parent',
    traceCallId:'background-trace', agentId:'background-child', background:true}, 'running');
  context.handleToolResult({tool:'Agent', id:'background-parent', traceCallId:'background-trace',
    output:'Agent started', presentation:{subagent:{sessionId:'A', parentToolUseId:'background-parent',
      agentId:'background-child', background:true, status:'running'}}});
  context.finishUserTurn('completed');
  context.beginUserTurn();
  assert.match(oldStage.textContent, /已启动后台子代理/, 'old turn remains visibly in progress');
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'background-parent',
    traceCallId:'background-trace', agentId:'background-child', background:true}, 'completed');
  assert.equal(oldRow.dataset.subagentStatus, 'completed', 'terminal event updates the prior turn row');
  assert.match(oldStage.textContent, /已协调子代理/, 'prior turn heading reflects terminal status');
  context.currentSessionId = 'B';
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'background-parent',
    traceCallId:'background-trace', agentId:'background-child', background:true}, 'failed');
  assert.equal(oldRow.dataset.subagentStatus, 'completed', 'offscreen lifecycle cannot mutate viewed transcript');
} else if (scenario === 'subagent_reused_provider_id_trace_binding') {
  const input = JSON.stringify({description:'Independent child', run_in_background:true});
  context.handleToolStart({tool:'Agent', id:'reused-agent-id', session:'A', traceCallId:'agent-trace-1', input});
  context.handleToolStart({tool:'Agent', id:'reused-agent-id', session:'A', traceCallId:'agent-trace-2', input});
  flushFrames();
  const [first, second] = area.querySelectorAll('.call-row');
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'reused-agent-id',
    traceCallId:'agent-trace-2', agentId:'child-2', background:true}, 'running');
  assert.equal(first.dataset.subagentId, undefined, 'same provider ID must not bind the first row');
  assert.equal(second.dataset.subagentId, 'child-2', 'trace identity binds the second invocation');
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'reused-agent-id',
    traceCallId:'agent-trace-1', agentId:'child-1', background:true}, 'running');
  assert.equal(first.dataset.subagentId, 'child-1');
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'reused-agent-id',
    traceCallId:'unknown-trace', agentId:'unknown-child', background:true}, 'running');
  assert.equal(first.dataset.subagentId, 'child-1', 'unknown trace never reassigns a known row');
  assert.equal(second.dataset.subagentId, 'child-2');
} else if (scenario === 'subagent_trace_miss_does_not_bind_old_row') {
  const input = JSON.stringify({description:'Previous child'});
  context.handleToolStart({tool:'Agent', id:'reused-parent', session:'A', input});
  flushFrames();
  const oldRow = area.querySelector('.call-row');
  context.handleToolResult({tool:'Agent', id:'reused-parent', output:'old result'});
  context.finishUserTurn('completed');
  context.beginUserTurn();
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'reused-parent',
    traceCallId:'new-call-trace', agentId:'new-child', background:true}, 'running');
  assert.equal(oldRow.dataset.subagentId, undefined,
    'a new traced child must not attach to the only untraced Agent row from a previous turn');
  context.handleToolStart({tool:'Agent', id:'reused-parent', session:'A', input});
  flushFrames();
  const currentRow = area.querySelectorAll('.call-row').at(-1);
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'reused-parent',
    traceCallId:'new-call-trace', agentId:'new-child', background:true}, 'running');
  assert.equal(oldRow.dataset.subagentId, undefined, 'fallback never changes an old unbound row');
  assert.equal(currentRow.dataset.subagentId, 'new-child',
    'the unique untraced row in the current turn can receive its lifecycle identity');
} else if (scenario === 'subagent_known_child_without_trace_cross_turn') {
  context.handleToolStart({tool:'Agent', id:'known-parent', session:'A',
    input:JSON.stringify({run_in_background:true})});
  flushFrames();
  const row = area.querySelector('.call-row');
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'known-parent',
    agentId:'known-child', background:true}, 'running');
  context.handleToolResult({tool:'Agent', id:'known-parent', output:'Agent started'});
  context.finishUserTurn('completed');
  context.beginUserTurn();
  context.handleSubagentLifecycle({sessionId:'A', parentToolUseId:'known-parent',
    traceCallId:'late-trace', agentId:'known-child', background:true}, 'completed');
  assert.equal(row.dataset.subagentStatus, 'completed',
    'a known child can complete its untraced prior-turn row');
} else if (scenario === 'subagent_history_replay') {
  const presentation = {subagent:{sessionId:'A', parentToolUseId:'saved-parent', agentId:'saved-child',
    name:'reviewer', background:false, status:'completed'}};
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Review this file'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Agent', tool_use_id:'saved-parent',
      input:{description:'Review source', prompt:'private child task'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'saved-parent',
      content:'Child returned its findings', presentation}]},
    {role:'assistant', content:[{type:'text', text:'My final answer'}]},
  ]);
  flushFrames();
  const stage = area.querySelector('.subagent-stage');
  assert(stage, 'saved tool blocks recreate the inline sub-agent stage');
  assert.match(stage.textContent, /已协调子代理.*1 个/);
  const row = area.querySelector('.call-row');
  const open = row.querySelector('.tc-open-agent');
  assert.equal(open.hidden, false, 'persisted structured identity restores direct detail link');
  open.dispatch('click');
  assert.equal(context.openedAgents.at(-1).id, 'saved-child');
  assert.equal(area.querySelectorAll('.message-assistant').length, 1, 'child output does not become an extra parent answer');
  context.handleSubagentLifecycle({session:'A', sessionId:'A', parentToolUseId:'saved-parent',
    agentId:'saved-child', status:'completed', text:'PRIVATE_CHILD_STREAM'}, 'completed');
  assert.doesNotMatch(visibleText(area), /PRIVATE_CHILD_STREAM/,
    'child lifecycle updates status only; child text stays in the independent detail stream');
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Review after restart'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Agent', tool_use_id:'saved-parent-blank',
      input:{description:'Review source', prompt:'private child task'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'saved-parent-blank', content:'Child returned its findings',
      presentation:{'metis.agent_started':true, subagent:{sessionId:'', parentToolUseId:'saved-parent-blank',
        agentId:'saved-child-blank', name:'reviewer', background:false, status:'completed'}}}]},
  ]);
  const restored = area.querySelector('.tc-open-agent');
  assert.equal(restored.hidden, false, 'empty owner in a structured saved result adopts the loaded session');
  restored.dispatch('click');
  assert.equal(context.openedAgents.at(-1).id, 'saved-child-blank');
  assert.equal(context.openedAgents.at(-1).owner, 'A');
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Untrusted older data'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Agent', tool_use_id:'unmarked-parent', input:{prompt:'hidden'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'unmarked-parent', content:'done', presentation:{subagent:{
      sessionId:'', parentToolUseId:'unmarked-parent', agentId:'unmarked-child', status:'completed'}}}]},
  ]);
  assert.equal(area.querySelector('.tc-open-agent').hidden, true, 'unmarked empty owner stays non-clickable');
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'No trusted identity'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Agent', tool_use_id:'foreign-parent', input:{prompt:'hidden'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'foreign-parent', content:'done', presentation:{subagent:{
      sessionId:'B', parentToolUseId:'foreign-parent', agentId:'foreign-child', status:'completed'}}}]},
  ]);
  assert.equal(area.querySelector('.tc-open-agent').hidden, true, 'foreign history identity stays non-clickable');
} else if (scenario === 'subagent_history_terminal_refresh') {
  const history = status => [
    {role:'user', content:[{type:'text', text:'Run reviewer in background'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Agent', tool_use_id:'bg-parent',
      input:{description:'Review source', run_in_background:true}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'bg-parent', content:'Agent started',
      presentation:{'metis.agent_started':true, subagent:{sessionId:'A', parentToolUseId:'bg-parent',
        agentId:'bg-child', name:'reviewer', background:true, status}}}]},
  ];
  const requested=[];
  context.fetch=async url=>{
    if (!url.startsWith('/api/subagents/')) return {ok:false};
    requested.push(url);
    return {ok:true,json:async()=>({agent:{sessionId:'A',agentId:'bg-child',status:'completed'}})};
  };
  context.renderHistoryMessages(history('running'));
  await new Promise(resolve=>setImmediate(resolve));
  assert.deepEqual(requested,['/api/subagents/bg-child?sessionId=A']);
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'completed');
  assert.match(area.querySelector('.subagent-stage').textContent,/已协调子代理/);
  requested.length=0;
  context.renderHistoryMessages(history('completed'));
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(requested.length,0,'terminal history does not need another detail request');
  context.fetch=async url=>url.startsWith('/api/subagents/')
    ? {ok:true,json:async()=>({agent:{sessionId:'B',agentId:'bg-child',status:'completed'}})}
    : {ok:false};
  context.renderHistoryMessages(history('running'));
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'running','foreign owner cannot complete this child');
  context.fetch=async () => ({ok:false});
  context.renderHistoryMessages(history('running'));
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'running','failed read keeps original running state');
} else if (scenario === 'subagent_history_terminal_race') {
  const history = owner => [
    {role:'user', content:[{type:'text', text:'Run background reviewer'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Agent', tool_use_id:'bg-parent', input:{run_in_background:true}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'bg-parent', content:'Agent started',
      presentation:{'metis.agent_started':true, subagent:{sessionId:owner, parentToolUseId:'bg-parent',
        agentId:'bg-child', background:true, status:'running'}}}]},
  ];
  const pending=[];
  context.fetch=url=>url.startsWith('/api/subagents/')
    ? new Promise(resolve=>pending.push({url,resolve})) : Promise.resolve({ok:false});
  context.renderHistoryMessages(history('A'));
  assert.equal(pending.length,1);
  const staleRow=area.querySelector('.call-row');
  context.renderHistoryMessages(history('A'));
  assert.equal(pending.length,2);
  pending[0].resolve({ok:true,json:async()=>({agent:{sessionId:'A',agentId:'bg-child',status:'completed'}})});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'running','earlier render cannot update replacement row');
  assert.equal(staleRow.dataset.subagentStatus,'running','detached earlier row also stays unchanged');
  context.currentSessionId='B';
  context.renderHistoryMessages(history('B'));
  assert.equal(pending.length,3);
  pending[1].resolve({ok:true,json:async()=>({agent:{sessionId:'A',agentId:'bg-child',status:'completed'}})});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'running','old session response cannot alter new transcript');
  pending[2].resolve({ok:true,json:async()=>({agent:{sessionId:'B',agentId:'bg-child',status:'failed'}})});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'failed','current session terminal response can update');
} else if (scenario === 'stopped_tool') {
  const row = newRow('stopped');
  assert.equal(row.getAttribute('data-state'), 'running');
  inspectRow(row);
  assert.match(details.detailsBody.innerHTML, /运行中|进行中/, 'running tool detail says it is running');
  assert.doesNotMatch(details.detailsBody.innerHTML, /Done|已完成/, 'running tool detail must not claim completion');
  context.finishUserTurn('stopped', 'user stopped');
  flushFrames();
  assert.equal(area.querySelector('.activity-turn').dataset.state, 'stopped');
  assert.equal(row.getAttribute('data-state'), 'stopped', 'an interrupted tool is no longer running');
  assert.match(row.querySelector('.tc-leading').innerHTML, /<svg/, 'interrupted tool keeps its icon');
  assert.match(row.querySelector('.tc-leading').innerHTML, /warning|stopped/, 'interrupted tool has a warning marker');
  assert.match(details.detailsBody.innerHTML, /已中断/, 'selected detail updates when the tool is interrupted');
} else if (scenario === 'failed_tool_icon') {
  const row = newRow('failed');
  context.handleToolResult({tool:'Bash', id:'failed', output:'permission denied', isError:true, elapsedMs:7});
  flushFrames();
  assert.equal(row.getAttribute('data-state'), 'error');
  assert.match(row.querySelector('.tc-leading').innerHTML, /<svg/, 'failed tool keeps the command icon');
  assert.match(row.querySelector('.tc-leading').innerHTML, /error/, 'failure has a distinct error marker');
  assert.equal(row.querySelector('.tc-io-text[data-out]').getAttribute('data-error'), 'true');
} else if (scenario === 'completed_without_tool_result') {
  const row = newRow('unanswered-tool');
  context.finishUserTurn('completed');
  flushFrames();
  assert.equal(area.querySelector('.activity-turn').dataset.state, 'completed');
  assertNeutralNoResult(row, 'completed turn missing tool_result');
  assert.match(row.querySelector('.tc-leading').innerHTML, /<svg/, 'unknown-result tool keeps its icon');
} else if (scenario === 'history_stopped_after_metadata' || scenario === 'history_error_after_metadata') {
  vm.runInContext('renderingActivityHistory = true; activityHistoryTurn = 1; activityHistoryLastTurn = 1;', context);
  const row = newRow('saved-tool');
  context.finishActivityGroup();
  context.finishActivityTurn('finished');
  vm.runInContext('renderingActivityHistory = false;', context);
  flushFrames();
  const turn = area.querySelector('.activity-turn');
  assert.equal(turn.dataset.state, 'finished', 'saved turn starts neutral without authoritative metadata');
  assertNeutralNoResult(row, 'saved tool before metadata');
  const stopped = scenario === 'history_stopped_after_metadata';
  context.fetch = async url => url.startsWith('/api/trace')
    ? {ok:true, json:async () => ({events:[{kind:stopped ? 'loop_done' : 'error',turn:1,depth:0,parentID:''}],turnMetrics:[]})}
    : {ok:true, json:async () => ({session:{status:stopped ? 'stopped' : 'failed'}})};
  await context.restoreHistoryMessageMetadata('A');
  flushFrames();
  assert.equal(turn.dataset.state, stopped ? 'stopped' : 'error');
  assert.equal(row.getAttribute('data-state'), stopped ? 'stopped' : 'error',
    'authoritative session result updates the previously neutral tool row');
  assert.match(row.querySelector('.tc-leading').innerHTML, /<svg/, 'terminal tool keeps its icon');
  assert.match(row.querySelector('.tc-leading').innerHTML, stopped ? /warning|stopped/ : /error/,
    'interrupted and failed tools remain visually distinct');
} else if (scenario === 'reused_provider_id_same_turn') {
  const first = newRow('reused', 'First invocation');
  inspectRow(first);
  context.switchDetailTab('input');
  assert.match(details.detailsBody.innerHTML, /First invocation/);
  context.handleToolResult({tool:'Bash', id:'reused', output:'first output'});
  flushFrames();
  assert.equal(first.getAttribute('data-state'), 'ok');
  const second = newRow('reused', 'Second invocation');
  assert.notEqual(rowKey(first), rowKey(second), 'same provider id gets distinct stable row keys');
  assert.equal(first.getAttribute('data-state'), 'ok');
  assert.equal(second.getAttribute('data-state'), 'running');
  context.renderDetailPanel();
  assert.match(details.detailsBody.innerHTML, /First invocation/, 'new start does not replace selected older detail');
  assert.doesNotMatch(details.detailsBody.innerHTML, /Second invocation/);
  context.handleToolResult({tool:'Bash', id:'reused', output:'second output'});
  flushFrames();
  assert.equal(second.getAttribute('data-state'), 'ok', 'second result belongs to the only unfinished live row');
  assert.equal(first.getAttribute('data-state'), 'ok');
  assert.equal(second.querySelector('.tc-io-text[data-out]').textContent, 'second output');
  assert.equal(first.querySelector('.tc-io-text[data-out]').textContent, 'first output');
  context.switchDetailTab('output');
  assert.match(details.detailsBody.innerHTML, /first output/, 'first Inspect still belongs to first row');
  inspectRow(second);
  context.switchDetailTab('output');
  assert.match(details.detailsBody.innerHTML, /second output/, 'second Inspect belongs to second row');
} else if (scenario === 'reused_provider_id_ambiguous_no_trace') {
  const first = newRow('ambiguous', 'First unresolved');
  const second = newRow('ambiguous', 'Second unresolved');
  context.handleToolResult({tool:'Bash', id:'ambiguous', output:'orphan result'});
  flushFrames();
  assert.notEqual(first.getAttribute('data-state'), 'ok', 'ambiguous result cannot certify the first invocation');
  assert.notEqual(second.getAttribute('data-state'), 'ok', 'ambiguous result cannot certify the second invocation');
  assert.doesNotMatch(first.querySelector('.tc-io-text[data-out]').textContent, /orphan result/);
  assert.doesNotMatch(second.querySelector('.tc-io-text[data-out]').textContent, /orphan result/);
  assert.match(visibleText(area) + ' ' + toasts.join(' '), /归属不明|无法确定|无法归属|ambiguous|cannot match/i,
    'ambiguous tool result leaves a user-visible explanation');
} else if (scenario === 'reused_provider_id_trace_out_of_order') {
  const first = newRow('trace-reused', 'Trace first', 'call-A');
  const second = newRow('trace-reused', 'Trace second', 'call-B');
  assert.notEqual(rowKey(first), rowKey(second));
  context.handleToolResult({tool:'Bash', id:'trace-reused', traceCallId:'call-A', output:'A arrived first'});
  flushFrames();
  assert.equal(first.getAttribute('data-state'), 'ok', 'traceCallId selects the older in-flight row');
  assert.equal(second.getAttribute('data-state'), 'running', 'newer row stays running until its own result');
  assert.equal(first.querySelector('.tc-io-text[data-out]').textContent, 'A arrived first');
  context.handleToolResult({tool:'Bash', id:'trace-reused', traceCallId:'call-B', output:'B arrived second'});
  flushFrames();
  assert.equal(second.getAttribute('data-state'), 'ok');
  assert.equal(second.querySelector('.tc-io-text[data-out]').textContent, 'B arrived second');
} else if (scenario === 'interleaved_args_same_provider_id') {
  context.handleToolArgsDelta({tool:'Bash', id:'shared-stream', traceCallId:'stream-A', delta:'{"description":"A'});
  context.handleToolArgsDelta({tool:'Bash', id:'shared-stream', traceCallId:'stream-B', delta:'{"description":"B'});
  context.handleToolArgsDelta({tool:'Bash', id:'shared-stream', traceCallId:'stream-A', delta:' one"}'});
  context.handleToolArgsDelta({tool:'Bash', id:'shared-stream', traceCallId:'stream-B', delta:' two"}'});
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2, 'interleaved deltas create distinct call previews');
  const first = rows.find(row => row.dataset.traceCallId === 'stream-A');
  const second = rows.find(row => row.dataset.traceCallId === 'stream-B');
  assert.equal(first.getAttribute('data-args'), '{"description":"A one"}');
  assert.equal(second.getAttribute('data-args'), '{"description":"B two"}');
  context.handleToolStart({tool:'Bash', id:'shared-stream', traceCallId:'stream-B', input:'{"description":"B two"}'});
  context.handleToolStart({tool:'Bash', id:'shared-stream', traceCallId:'stream-A', input:'{"description":"A one"}'});
  assert.equal(area.querySelectorAll('.call-row').length, 2, 'authoritative starts adopt their own previews');
  assert.equal(first.dataset.provisional, 'false');
  assert.equal(second.dataset.provisional, 'false');
  context.handleToolResult({tool:'Bash', id:'shared-stream', traceCallId:'stream-A', output:'A done'});
  context.handleToolResult({tool:'Bash', id:'shared-stream', traceCallId:'stream-B', output:'B done'});
  assert.equal(first.querySelector('.tc-io-text[data-out]').textContent, 'A done');
  assert.equal(second.querySelector('.tc-io-text[data-out]').textContent, 'B done');
} else if (scenario === 'reused_provider_id_across_turns') {
  const first = newRow('turn-reused', 'Old turn');
  context.handleToolResult({tool:'Bash', id:'turn-reused', output:'old result'});
  context.finishUserTurn('completed');
  context.beginUserTurn();
  const second = newRow('turn-reused', 'New turn');
  assert.notEqual(first.closest('.activity-group').dataset.turnId,
    second.closest('.activity-group').dataset.turnId, 'new tool is owned by new turn');
  assert.notEqual(rowKey(first), rowKey(second));
  context.handleToolResult({tool:'Bash', id:'turn-reused', output:'new result'});
  flushFrames();
  assert.equal(first.querySelector('.tc-io-text[data-out]').textContent, 'old result');
  assert.equal(second.querySelector('.tc-io-text[data-out]').textContent, 'new result');
  inspectRow(first);
  context.switchDetailTab('input');
  assert.match(details.detailsBody.innerHTML, /Old turn/);
} else if (scenario === 'reused_provider_id_history_replay') {
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Replay duplicate tools'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'saved-reused', input:{description:'Historical first'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'saved-reused', content:'historical first output'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'saved-reused', input:{description:'Historical second'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'saved-reused', content:'historical second output'}]},
    {role:'assistant', content:[{type:'text', text:'History answer'}]},
  ]);
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2, 'saved repeated tool_use blocks render independently');
  assert.notEqual(rowKey(rows[0]), rowKey(rows[1]));
  assert.equal(rows[0].querySelector('.tc-summary').textContent, 'Historical first');
  assert.equal(rows[1].querySelector('.tc-summary').textContent, 'Historical second');
  assert.equal(rows[0].querySelector('.tc-io-text[data-out]').textContent, 'historical first output',
    'history without traceCallId keeps the first unambiguous result');
  assert.equal(rows[1].querySelector('.tc-io-text[data-out]').textContent, 'historical second output');
} else if (scenario === 'history_rebuild_clears_old_detail') {
  const old = newRow('old-detail', 'Old private input');
  const oldKey = rowKey(old);
  inspectRow(old);
  context.switchDetailTab('input');
  assert.match(details.detailsBody.innerHTML, /Old private input/);
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'New saved task'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'new-detail', input:{description:'New saved input'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'new-detail', content:'New saved output'}]},
    {role:'assistant', content:[{type:'text', text:'New saved answer'}]},
  ]);
  flushFrames();
  assert.equal(old.isConnected, false, 'transcript rebuild removes old row');
  assert.equal(context.selectedToolId, null, 'old Inspect selection is released');
  assert.equal(context.toolDetails[oldKey], undefined, 'old detail data is released');
  assert.equal(app.classList.contains('details-closed'), true);
  assert.doesNotMatch(details.detailsBody.innerHTML, /Old private input/);
} else if (scenario === 'empty_provider_id_provisional_and_ambiguous') {
  context.handleToolArgsDelta({tool:'Bash', id:'', delta:'{"description":"Partial input"}'});
  flushFrames();
  let rows = area.querySelectorAll('.call-row');
  assert(rows.length <= 1, 'empty ID args delta may buffer or create one provisional row');
  const provisional = rows[0] || null;
  const key = provisional ? rowKey(provisional) : '';
  context.handleToolStart({tool:'Bash', id:'', traceCallId:'empty-trace', input:'{"description":"Authoritative input"}'});
  flushFrames();
  rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1, 'authoritative empty ID start does not leave a ghost row');
  if (provisional) assert.equal(rowKey(rows[0]), key, 'existing provisional row is adopted');
  assert.match(rows[0].querySelector('.tc-io-text[data-in]').textContent, /Authoritative input/);
  context.handleToolResult({tool:'Bash', id:'', traceCallId:'empty-trace', output:'empty ID result'});
  flushFrames();
  assert.equal(rows[0].getAttribute('data-state'), 'ok');
  assert.equal(rows[0].querySelector('.tc-io-text[data-out]').textContent, 'empty ID result');

  const first = newRow('', 'Anonymous first');
  const second = newRow('', 'Anonymous second');
  context.handleToolResult({tool:'Bash', id:'', output:'anonymous orphan'});
  flushFrames();
  assert.notEqual(first.getAttribute('data-state'), 'ok');
  assert.notEqual(second.getAttribute('data-state'), 'ok');
  assert.doesNotMatch(first.querySelector('.tc-io-text[data-out]').textContent, /anonymous orphan/);
  assert.doesNotMatch(second.querySelector('.tc-io-text[data-out]').textContent, /anonymous orphan/);
  assert.match(visibleText(area) + ' ' + toasts.join(' '), /归属不明|无法确定|无法归属|ambiguous|cannot match/i,
    'anonymous overlapping results are visibly unattributed');
} else if (scenario === 'provisional_id_different_tool_name') {
  context.handleToolArgsDelta({tool:'Read', id:'shared-provider', delta:'{"path":"draft.txt"}'});
  flushFrames();
  const provisional = area.querySelectorAll('.call-row')[0];
  assert(provisional, 'Read args delta creates a provisional row');
  assert.equal(provisional.dataset.tool, 'Read');
  const command = newRow('shared-provider', 'Build project', 'command-trace');
  assert.notEqual(rowKey(command), rowKey(provisional), 'Bash start cannot adopt a Read provisional');
  assert.equal(provisional.dataset.tool, 'Read');
  assert.equal(command.dataset.tool, 'Bash');
  assert.equal(command.getAttribute('data-variant'), 'bash', 'authoritative tool keeps its icon variant');
  assert.equal(command.querySelector('.tc-title').textContent, '运行命令');
  assert.match(command.querySelector('.tc-io-text[data-in]').textContent, /Build project/);
  assert.doesNotMatch(command.querySelector('.tc-io-text[data-in]').textContent, /draft.txt/);
  assert.equal(context.toolDetails[rowKey(command)].name, 'Bash');
  context.handleToolResult({tool:'Bash', id:'shared-provider', traceCallId:'command-trace', output:'built'});
  flushFrames();
  assert.equal(command.getAttribute('data-state'), 'ok');
  assert.equal(provisional.getAttribute('data-state'), 'running');
  context.handleToolStart({tool:'Read', id:'shared-provider', traceCallId:'read-trace', input:'{"path":"final.txt"}'});
  flushFrames();
  assert.equal(area.querySelectorAll('.call-row').length, 2, 'matching Read start adopts its own provisional');
  assert.match(provisional.querySelector('.tc-io-text[data-in]').textContent, /final.txt/);
} else if (scenario === 'history_empty_provider_id_unique') {
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Replay anonymous tool'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'', input:{description:'Anonymous saved command'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'', content:'anonymous saved output'}]},
    {role:'assistant', content:[{type:'text', text:'Saved answer'}]},
  ]);
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1, 'saved tool_use with empty ID stays visible');
  assert.equal(rows[0].getAttribute('data-state'), 'ok');
  assert.equal(rows[0].querySelector('.tc-io-text[data-out]').textContent, 'anonymous saved output');
} else if (scenario === 'history_empty_provider_id_ambiguous') {
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Replay anonymous parallel tools'}]},
    {role:'assistant', content:[
      {type:'tool_use', name:'Bash', tool_use_id:'', input:{description:'Saved anonymous first'}},
      {type:'tool_use', name:'Bash', tool_use_id:'', input:{description:'Saved anonymous second'}},
    ]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'', content:'unattributed saved output'}]},
  ]);
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2);
  assert.notEqual(rows[0].getAttribute('data-state'), 'ok');
  assert.notEqual(rows[1].getAttribute('data-state'), 'ok');
  assert.match(visibleText(area) + ' ' + toasts.join(' '), /归属不明|无法确定|无法归属|ambiguous|cannot match/i,
    'history shows the anonymous result separately');
} else if (scenario === 'result_only_failure_creates_tool_row') {
  context.handleToolResult({tool:'Bash', id:'no-start', traceCallId:'trace-denied',
    output:'permission denied before tool start', isError:true, elapsedMs:3});
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1, 'result-only failure is a visible tool row, not an orphan note');
  const row = rows[0];
  assert.equal(row.dataset.tool, 'Bash');
  assert.equal(row.dataset.traceCallId, 'trace-denied');
  assert.equal(row.getAttribute('data-state'), 'error');
  assert.match(row.querySelector('.tc-leading').innerHTML, /<svg/, 'failure retains its tool icon');
  assert.match(row.querySelector('.tc-io-text[data-out]').textContent, /permission denied before tool start/);
  inspectRow(row);
  assert.match(details.detailsBody.innerHTML, /失败/, 'result-only failure is inspectable');
} else if (scenario === 'result_only_adopts_unique_provisional') {
  context.handleToolArgsDelta({tool:'Bash', id:'provisional-only', delta:'{"command":"bad arguments"}'});
  flushFrames();
  const row = area.querySelectorAll('.call-row')[0];
  assert(row, 'args delta creates a provisional row');
  assert.equal(row.getAttribute('data-state'), 'running');
  const key = rowKey(row);
  context.handleToolResult({tool:'Bash', id:'provisional-only', traceCallId:'trace-invalid',
    output:'malformed arguments', isError:true});
  flushFrames();
  assert.equal(area.querySelectorAll('.call-row').length, 1, 'result adopts the one provisional row');
  assert.equal(rowKey(row), key);
  assert.equal(row.dataset.traceCallId, 'trace-invalid');
  assert.equal(row.getAttribute('data-state'), 'error', 'provisional tool must terminate');
  assert.match(row.querySelector('.tc-io-text[data-out]').textContent, /malformed arguments/);
  assert.equal(area.querySelectorAll('.tool-unattributed-result').length, 0);
} else if (scenario === 'result_only_does_not_adopt_other_traced_preview') {
  context.handleToolArgsDelta({tool:'Bash', id:'same-provider', traceCallId:'preview-Y',
    delta:'{"description":"Y input"}'});
  const previewY = area.querySelectorAll('.call-row')[0];
  assert.equal(previewY.dataset.traceCallId, 'preview-Y');
  context.handleToolResult({tool:'Bash', id:'same-provider', traceCallId:'result-X',
    output:'X was denied', isError:true});
  let rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2, 'result X gets a separate row instead of taking Y preview');
  assert.equal(previewY.getAttribute('data-state'), 'running');
  assert.equal(previewY.getAttribute('data-args'), '{"description":"Y input"}');
  assert.equal(rows[1].dataset.traceCallId, 'result-X');
  assert.equal(rows[1].getAttribute('data-state'), 'error');
  context.handleToolStart({tool:'Bash', id:'same-provider', traceCallId:'preview-Y',
    input:'{"description":"Y input"}'});
  context.handleToolResult({tool:'Bash', id:'same-provider', traceCallId:'preview-Y', output:'Y done'});
  rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2);
  assert.equal(previewY.getAttribute('data-state'), 'ok');
  assert.equal(previewY.querySelector('.tc-io-text[data-out]').textContent, 'Y done');
} else if (scenario === 'result_only_multiple_candidates_stays_unattributed') {
  context.handleToolStart({tool:'Bash', id:'multiple-provisional', input:'{"command":"first"}', provisional:true});
  context.handleToolStart({tool:'Bash', id:'multiple-provisional', input:'{"command":"second"}', provisional:true});
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2);
  context.handleToolResult({tool:'Bash', id:'multiple-provisional', traceCallId:'trace-unknown',
    output:'cannot attribute', isError:true});
  flushFrames();
  assert.notEqual(rows[0].getAttribute('data-state'), 'error', 'first unproven row is not mislabeled');
  assert.notEqual(rows[1].getAttribute('data-state'), 'error', 'second unproven row is not mislabeled');
  assert.equal(area.querySelectorAll('.tool-unattributed-result').length, 1);
  assert.match(visibleText(area), /归属不明|无法确定|无法归属|ambiguous|cannot match/i);
} else if (scenario === 'incomplete_provisional_not_reused') {
  context.handleToolArgsDelta({tool:'Bash', id:'reused-provisional', delta:'{"description":"Old partial"}'});
  flushFrames();
  const old = area.querySelector('.call-row');
  assert(old, 'old provisional row exists');
  const oldKey = rowKey(old);
  context.finishActivityGroup();
  context.finishActivityTurn('finished');
  flushFrames();
  assert.equal(old.getAttribute('data-state'), 'incomplete');
  context.handleToolArgsDelta({tool:'Bash', id:'reused-provisional', delta:'{"description":"New partial"}'});
  flushFrames();
  let rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2, 'new args delta does not append to older incomplete provisional');
  const fresh = rows[1];
  assert.notEqual(rowKey(fresh), oldKey);
  assert.equal(old.getAttribute('data-state'), 'incomplete');
  assert.doesNotMatch(old.querySelector('.tc-io-text[data-in]').textContent, /New partial/);
  context.handleToolStart({tool:'Bash', id:'reused-provisional', traceCallId:'new-provisional-trace',
    input:'{"description":"New authoritative"}'});
  flushFrames();
  rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2, 'new authoritative start adopts only the fresh provisional');
  assert.equal(rowKey(rows[1]), rowKey(fresh));
  assert.match(fresh.querySelector('.tc-io-text[data-in]').textContent, /New authoritative/);
  assert.equal(old.getAttribute('data-state'), 'incomplete');
} else if (scenario === 'legacy_result_tool_name_mismatch') {
  context.handleToolStart({tool:'Read', id:'legacy-reused', input:'{"path":"README.md"}'});
  flushFrames();
  const read = area.querySelector('.call-row');
  assert(read);
  assert.equal(read.dataset.tool, 'Read');
  context.handleToolResult({tool:'Bash', id:'legacy-reused', output:'bash permission denied', isError:true});
  flushFrames();
  assert.equal(read.getAttribute('data-state'), 'running', 'Bash result must not finish a Read call');
  assert.doesNotMatch(read.querySelector('.tc-io-text[data-out]').textContent, /bash permission denied/);
  assert(area.querySelectorAll('.call-row[data-tool="Bash"]').length > 0 ||
    area.querySelectorAll('.tool-unattributed-result').length > 0,
    'the unmatched Bash result remains visible separately');
} else if (scenario === 'reconnect_adopts_unique_history_row') {
  context.turnRunning = true;
  context.runningSessionId = 'A';
  const input = {description:'Resume repository check'};
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Check repository'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'resume-id', input}]},
  ]);
  flushFrames();
  let rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1);
  const historical = rows[0];
  const key = rowKey(historical);
  assert.equal(historical.getAttribute('data-state'), 'running', 'last saved tool belongs to an active turn');
  context.handleToolStart({tool:'Bash', id:'resume-id', traceCallId:'resume-trace', input:JSON.stringify(input)});
  flushFrames();
  rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1, 'buffered start adopts the unique identical saved tool_use');
  assert.equal(rowKey(rows[0]), key);
  assert.equal(rows[0].dataset.traceCallId, 'resume-trace');
  context.handleToolResult({tool:'Bash', id:'resume-id', traceCallId:'resume-trace', output:'repository clean'});
  flushFrames();
  assert.equal(rows[0].getAttribute('data-state'), 'ok');
  assert.equal(rows[0].querySelector('.tc-io-text[data-out]').textContent, 'repository clean');
} else if (scenario === 'reconnect_adopts_traced_running_history_row') {
  context.turnRunning = true;
  context.runningSessionId = 'A';
  const input = {description:'Trace saved command'};
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Resume traced task'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'traced-running',
      trace_call_id:'running-trace', input}]},
  ]);
  flushFrames();
  let rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1);
  const historical = rows[0];
  assert.equal(historical.dataset.traceCallId, 'running-trace', 'saved trace ID is projected onto the row');
  const key = rowKey(historical);
  context.handleToolStart({tool:'Bash', id:'traced-running', traceCallId:'running-trace', input:JSON.stringify(input)});
  flushFrames();
  rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1, 'buffered start with matching trace ID reuses the saved row');
  assert.equal(rowKey(rows[0]), key);
  context.handleToolResult({tool:'Bash', id:'traced-running', traceCallId:'running-trace', output:'traced running result'});
  flushFrames();
  assert.equal(rows[0].getAttribute('data-state'), 'ok');
  assert.equal(rows[0].querySelector('.tc-io-text[data-out]').textContent, 'traced running result');
} else if (scenario === 'reconnect_does_not_guess_duplicate_history_rows') {
  context.turnRunning = true;
  context.runningSessionId = 'A';
  const input = {description:'Same command'};
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Check twice'}]},
    {role:'assistant', content:[
      {type:'tool_use', name:'Bash', tool_use_id:'duplicate-history', input},
      {type:'tool_use', name:'Bash', tool_use_id:'duplicate-history', input},
    ]},
  ]);
  flushFrames();
  let rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 2);
  const oldKeys = rows.map(rowKey);
  context.handleToolStart({tool:'Bash', id:'duplicate-history', traceCallId:'new-distinct-trace', input:JSON.stringify(input)});
  flushFrames();
  rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 3, 'ambiguous historical matches cannot silently adopt either older row');
  assert.equal(rowKey(rows[0]), oldKeys[0]);
  assert.equal(rowKey(rows[1]), oldKeys[1]);
  assert.equal(rows[0].dataset.traceCallId, '');
  assert.equal(rows[1].dataset.traceCallId, '');
  assert.equal(rows[2].dataset.traceCallId, 'new-distinct-trace');
  context.handleToolResult({tool:'Bash', id:'duplicate-history', traceCallId:'new-distinct-trace', output:'third result'});
  flushFrames();
  assert.notEqual(rows[0].getAttribute('data-state'), 'ok');
  assert.notEqual(rows[1].getAttribute('data-state'), 'ok');
  assert.equal(rows[2].getAttribute('data-state'), 'ok');
  assert.equal(rows[2].querySelector('.tc-io-text[data-out]').textContent, 'third result');
} else if (scenario === 'reconnect_deduplicates_completed_history_row') {
  context.turnRunning = true;
  context.runningSessionId = 'A';
  const input = {description:'Completed saved command'};
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Replay completed tool'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Bash', tool_use_id:'completed-history',
      trace_call_id:'completed-trace', input}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'completed-history',
      trace_call_id:'completed-trace', content:'saved result'}]},
  ]);
  flushFrames();
  const rows = area.querySelectorAll('.call-row');
  assert.equal(rows.length, 1);
  const historical = rows[0];
  assert.equal(historical.getAttribute('data-state'), 'ok');
  assert.equal(historical.dataset.traceCallId, 'completed-trace', 'completed history keeps its persisted trace ID');
  const key = rowKey(historical);
  inspectRow(historical);
  context.switchDetailTab('output');
  const savedDetail = details.detailsBody.innerHTML;
  assert.match(savedDetail, /saved result/);
  context.handleToolArgsDelta({tool:'Bash', id:'completed-history', traceCallId:'completed-trace',
    delta:'{"description":"Completed saved command"}'});
  assert.equal(area.querySelectorAll('.call-row').length, 1,
    'buffered args delta does not recreate a completed saved invocation');
  context.handleToolStart({tool:'Bash', id:'completed-history', traceCallId:'completed-trace', input:JSON.stringify(input)});
  flushFrames();
  assert.equal(area.querySelectorAll('.call-row').length, 1, 'buffered duplicate start does not redraw completed history row');
  assert.equal(rowKey(historical), key);
  context.handleToolResult({tool:'Bash', id:'completed-history', traceCallId:'completed-trace',
    output:'saved result', elapsedMs:9876});
  flushFrames();
  assert.equal(area.querySelectorAll('.call-row').length, 1);
  assert.equal(area.querySelectorAll('.tool-unattributed-result').length, 0, 'duplicate result does not create an orphan note');
  assert.equal(historical.getAttribute('data-state'), 'ok');
  assert.equal(historical.querySelector('.tc-io-text[data-out]').textContent, 'saved result');
  assert.equal(context.toolDetails[key].elapsed, 0, 'duplicate result does not overwrite saved details');
  assert.equal(details.detailsBody.innerHTML, savedDetail);
} else {
  throw new Error('unknown scenario: ' + scenario);
}
}
runScenario().catch(error => { console.error(error); process.exitCode = 1; });
`
