package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestDraftTurnNavigationKeepsResponseInOriginatingView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	content, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(string(content))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const start = source.indexOf('async function runTurnItem(item)');
const end = source.indexOf('const MESSAGE_ACTION_ICONS', start);
assert(start >= 0 && end > start, 'runTurnItem must be extracted from the actual Desktop source');
const turnSource = source.slice(start, end);
const deferred = () => {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
};
function fixture() {
  const request = deferred();
  const messages = [], errors = [], toasts = [], finishes = [], navigation = [];
  const c = {
    currentSessionId: null, resumeSessionGeneration: 0,
    backgroundContinuationGenerations: new Map(), backgroundContinuationGeneration: 0,
    pendingForegroundRequest: null, runningSessionId: null, runningTurnIncompleteReason: '',
    runningTurnNeedsHistorySync: false, streamedTextThisTurn: false,
    queuedTurns: [], drainingQueuedTurns: false, queuedSessionId: null,
    window: { metisNavigation: { recordSession(id, options) { navigation.push({ id, options }); } } },
    document: { getElementById(id) { return id === 'welcomeScreen' ? null : { disabled: false }; } },
    clearTodoPlan() {}, resumeAutoScroll() {}, beginUserTurn() {},
    addMessage(role, text) { messages.push({ view: c.currentSessionId, role, text }); },
    finishUserTurn(outcome) { finishes.push({ view: c.currentSessionId, outcome }); },
    setTurnRunning(running, id) { c.runningSessionId = running ? id : null; },
    updateSendBtn() {}, loadSessionStatsbar() {}, loadSessionFiles() {},
    loadArtifactsForSession: async () => {}, loadSessions: async () => {},
    showError(message) { errors.push({ view: c.currentSessionId, message }); },
    showToast(message) { toasts.push({ view: c.currentSessionId, message }); },
    fetch(url, options) {
      assert.equal(url, '/api/turns');
      assert.equal(JSON.parse(options.body).sessionId, null);
      return request.promise;
    },
  };
  vm.createContext(c);
  vm.runInContext(turnSource, c);
  return { c, request, messages, errors, toasts, finishes, navigation };
}
const ok = data => ({ ok: true, json: async () => data });
const failed = data => ({ ok: false, status: 500, json: async () => data });
(async () => {
  // A blank draft submits, then another saved session becomes visible before failure.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    f.c.currentSessionId = 'B';
    f.c.resumeSessionGeneration++;
    f.request.resolve(failed({ error: 'A failed' }));
    assert.equal(await turn, false);
    assert.deepEqual(f.errors, [], 'draft A failure was rendered as an error in B');
    assert.deepEqual(f.finishes, [], 'draft A finished B\'s visible turn');
    assert.equal(f.c.currentSessionId, 'B');
    assert(f.toasts.some(item => item.message.includes('A failed')), 'background failure must remain visible as a toast');
  }
  // Navigating through B and back creates a new blank composer, even though its ID is also null.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    f.c.currentSessionId = 'B';
    f.c.resumeSessionGeneration++;
    f.c.currentSessionId = null;
    f.c.resumeSessionGeneration++;
    f.request.resolve(ok({ sessionId: 'A', text: 'A answered' }));
    assert.equal(await turn, true);
    assert.equal(f.c.currentSessionId, null, 'old response claimed a new blank composer');
    assert(!f.messages.some(item => item.role === 'assistant'), 'old response inserted text into a new blank composer');
    assert.deepEqual(f.finishes, [], 'old response finished the new composer');
    assert.deepEqual(f.navigation, [], 'old response replaced the current navigation entry');
  }
  // Without navigation, errors stay attached to the draft that started them.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    f.request.resolve(failed({ error: 'A failed' }));
    assert.equal(await turn, false);
    assert.equal(f.errors.length, 1);
    assert.equal(f.errors[0].message, 'A failed');
    assert.equal(f.finishes.length, 1);
    assert.equal(f.finishes[0].outcome, 'error');
  }
  // A successful response claims its own unchanged blank composer and renders its answer.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    f.request.resolve(ok({ sessionId: 'A', text: 'A answered' }));
    assert.equal(await turn, true);
    assert.equal(f.c.currentSessionId, 'A');
    assert(f.messages.some(item => item.view === 'A' && item.role === 'assistant' && item.text === 'A answered'));
    assert.equal(f.finishes.length, 1);
    assert.equal(f.finishes[0].outcome, 'completed');
    assert.equal(f.navigation.length, 1);
    assert.equal(f.navigation[0].id, 'A');
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("draft turn navigation UI checks failed: %v\n%s", err, output)
	}
}
