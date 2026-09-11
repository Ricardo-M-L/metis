package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestTodoPlanAnimationsRequireRunningDock(t *testing.T) {
	css, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{
		`.todo-plan-dock.is-running .todo-plan-trigger-ring`,
		`.todo-plan-dock.is-running .todo-plan-item[data-status="in_progress"] .todo-plan-glyph`,
	} {
		if !strings.Contains(string(css), selector) {
			t.Errorf("animation is missing run ownership selector %q", selector)
		}
	}
}

func TestTodoPlanBrowserRunLifecycle(t *testing.T) {
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
function extract(start, next) {
  const from = source.indexOf(start), to = source.indexOf(next, from + start.length);
  assert(from >= 0 && to > from, start);
  return source.slice(from, to);
}
function element() {
  const classes = new Set(), attrs = {};
  return { hidden: false, textContent: '', innerHTML: '', style: {},
    classList: { toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name), contains: name => classes.has(name) },
    setAttribute: (name, value) => attrs[name] = value };
}
const elements = Object.fromEntries(['todoPlanDock', 'todoPlanPopover', 'todoPlanTrigger', 'todoPlanStepLabel', 'todoPlanCounts', 'todoPlanList', 'sendBtn'].map(id => [id, element()]));
const c = {
  currentSessionId: 'A', runningSessionId: null, turnRunning: false, stopRequestPending: false,
  backgroundContinuationGeneration: 0, backgroundContinuationGenerations: new Map(), pendingForegroundRequest: null,
  queuedTurns: [], queuedSessionId: null, drainingQueuedTurns: false, runningTurnNeedsHistorySync: false, runningTurnIncompleteReason: '', streamedTextThisTurn: false,
  document: { getElementById: id => elements[id] || null },
  escHtml: String, escAttr: String, syncTurnControls() {}, renderSessions() {},
  resumeAutoScroll() {}, addMessage() {}, beginUserTurn() {}, finishUserTurn() {},
  updateSendBtn() {}, loadSessions() {}, loadSessionStatsbar() {}, showError() {}, showToast() {},
  sameSession: d => !d.session || d.session === c.currentSessionId,
};
vm.createContext(c);
vm.runInContext(extract('let todoPlanItems =', 'let pendingAsk ='), c);
vm.runInContext(extract('function setTurnRunning(', '// Detach transient DOM state'), c);
vm.runInContext(extract('function handleBackgroundContinuation(d)', 'async function loadEffort'), c);
vm.runInContext(extract('async function runTurnItem(', 'const MESSAGE_ACTION_ICONS'), c);
const dock = elements.todoPlanDock;
const running = () => dock.classList.contains('is-running');
const complete = () => dock.classList.contains('is-complete');
const activePlan = {todos: [{content:'Inspect failed HTTP/2 request', status:'in_progress'}, {content:'Verify fix', status:'pending'}]};
const savedPlan = [{role:'assistant', content:[{type:'tool_use', name:'TodoWrite', input:activePlan}]}];
function plan() { c.applyTodoSnapshot('TodoWrite', activePlan); }
function assertStopped(label) {
  assert.equal(running(), false, label);
  assert.match(elements.todoPlanStepLabel.textContent, /未完成|已停止/, label);
  assert.doesNotMatch(elements.todoPlanCounts.textContent, /进行中/, label);
  assert.match(elements.todoPlanList.innerHTML, /data-status="in_progress"/, 'stop must preserve unfinished plan state');
}

// Saved plan status alone is not evidence that a model request is alive.
c.applyTodoSnapshot('TodoWrite', {todos:[{content:'Done', status:'completed'}]});
assert.equal(complete(), true, 'completed dock activates the existing checkmark style');
c.restoreTodoPlanFromHistory(savedPlan);
assertStopped('history reload');
c.setTurnRunning(true, 'A');
assert.equal(running(), true, 'selected session is running');
assert.match(elements.todoPlanCounts.textContent, /进行中/);
c.setTurnRunning(false);
assertStopped('authoritative status stopped');

// Even while the run is writing its answer, a completed plan is a static check.
c.setTurnRunning(true, 'A');
c.applyTodoSnapshot('TodoWrite', {todos:[{content:'Done', status:'completed'}]});
assert.equal(complete(), true, 'completed dock activates the existing checkmark style');
assert.equal(running(), false, 'all completed never spins');
c.clearTodoPlan();
assert.equal(dock.hidden, true);
plan();
assert.equal(complete(), false, 'next unfinished plan clears the old completion style');
assert.equal(running(), true);

// A background run must not animate the saved plan of the viewed session.
c.currentSessionId = 'B';
c.restoreTodoPlanFromHistory(savedPlan);
assertStopped('viewing B while A runs');
assert.equal(c.applyStatusPlanSnapshot({activeSessionId:'A', planItems:[{content:'Other session', status:'completed'}]}), false);
assertStopped('unrelated status snapshot');
c.handleBackgroundContinuation({backgroundContinuation:'finished', session:'A', succeeded:true});
assertStopped('offscreen continuation finishes');
c.currentSessionId = 'A';
c.restoreTodoPlanFromHistory(savedPlan);
assertStopped('return to completed background run');
c.handleBackgroundContinuation({backgroundContinuation:'started', session:'A'});
assert.equal(running(), true, 'new continuation reactivates its unfinished plan');
c.handleBackgroundContinuation({backgroundContinuation:'finished', session:'A', succeeded:false});
assertStopped('failed continuation');

// A new chat can begin before the backend assigns its session ID.
c.currentSessionId = null;
c.setTurnRunning(true);
plan();
assert.equal(running(), true, 'new-session foreground run');
c.setTurnRunning(false);
c.currentSessionId = 'A';

(async () => {
  for (const outcome of ['done', 'stopped', 'http-error', 'network-error']) {
    c.fetch = async () => {
      plan();
      assert.equal(running(), true, outcome + ' starts running');
      if (outcome === 'network-error') throw new Error('HTTP/2 stream INTERNAL_ERROR');
      return { ok: outcome !== 'http-error', status: 500,
        json: async () => ({sessionId:'A', stopped:outcome === 'stopped', error:'HTTP/2 stream INTERNAL_ERROR'}) };
    };
    const succeeded = await c.runTurnItem({text:'Continue'});
    assert.equal(succeeded, outcome === 'done' || outcome === 'stopped', outcome);
    assert.equal(c.turnRunning, false, outcome + ' releases the run');
    assertStopped(outcome + ' settles the plan without fabricating completion');
  }
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("todo plan lifecycle harness: %v\n%s", err, out)
	}
}
