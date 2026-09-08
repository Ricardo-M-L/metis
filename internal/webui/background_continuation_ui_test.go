package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestBackgroundContinuationBrowserLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	// Execute the actual browser functions with a minimal UI harness. This
	// checks interleavings, not just the presence of a source-code marker.
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
function extract(start, next) {
  const from = source.indexOf(start), to = source.indexOf(next, from + start.length);
  assert(from >= 0 && to > from, start);
  return source.slice(from, to);
}
let began = 0, finished = 0, rendered = 0;
const c = {
  currentSessionId: 'B', runningSessionId: null, turnRunning: false,
  backgroundContinuationGeneration: 0, backgroundContinuationGenerations: new Map(), pendingForegroundRequest: null,
  queuedTurns: [], drainingQueuedTurns: false, messages: ['new transcript'], streamedTextThisTurn: true,
  sameSession: d => !d.session || d.session === c.currentSessionId,
  beginUserTurn: () => began++, finishUserTurn: () => finished++,
  setTurnRunning: (running, sid) => { c.turnRunning = running; c.runningSessionId = running ? sid : null; },
  updateSendBtn() {}, loadSessions() {}, loadSessionStatsbar() {}, setTimeout() {}, drainQueuedTurns() {},
  renderHistoryMessages() { rendered++; }, restoreCompactionHistory: async () => {},
};
vm.createContext(c);
vm.runInContext(extract('function handleBackgroundContinuation(d)', 'async function loadEffort'), c);
vm.runInContext(extract('async function syncViewedSessionHistory(', 'async function runTurnItem('), c);

// A starts while B is viewed: global ownership must update without editing B.
c.handleBackgroundContinuation({backgroundContinuation:'started', session:'A'});
assert.equal(c.turnRunning, true); assert.equal(c.runningSessionId, 'A'); assert.equal(began, 0);
c.handleBackgroundContinuation({backgroundContinuation:'finished', session:'A', succeeded:true});
assert.equal(c.turnRunning, false); assert.equal(finished, 0);

// A starts visibly; navigation to B must not strand global running state.
c.currentSessionId = 'A';
c.handleBackgroundContinuation({backgroundContinuation:'started', session:'A'});
assert.equal(began, 1);
c.currentSessionId = 'B';
c.handleBackgroundContinuation({backgroundContinuation:'finished', session:'A', succeeded:true});
assert.equal(c.turnRunning, false); assert.equal(finished, 0);

// A wins the backend slot while the foreground POST for B is queued. When
// A finishes, B still owns a pending request and must retain a Stop control.
c.pendingForegroundRequest = {sessionId:'B'};
c.handleBackgroundContinuation({backgroundContinuation:'started', session:'A'});
c.handleBackgroundContinuation({backgroundContinuation:'finished', session:'A', succeeded:true});
assert.equal(c.turnRunning, true); assert.equal(c.runningSessionId, 'B');
assert.equal(c.backgroundContinuationGenerations.get('B') || 0, 0);
assert.equal(began, 2);

// A delayed history response cannot overwrite a newer streamed continuation.
let release, allowed = true;
c.fetch = () => new Promise(resolve => { release = resolve; });
(async () => {
  const pending = c.syncViewedSessionHistory('B', () => allowed);
  allowed = false;
  release({ok:true, json:async () => ({messages:['stale transcript']})});
  assert.equal(await pending, false); assert.equal(rendered, 0);
  assert.deepEqual(c.messages, ['new transcript']);
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser lifecycle harness: %v\n%s", err, out)
	}
}
