package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

// Exercise the Desktop composer and queued-turn functions together. A slash
// command accidentally sent to /api/turns is a user-visible model request,
// so a string-level source assertion would miss the failure.
func TestBusyRenameCommandsStayLocal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", busyRenameBrowserScript)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("busy rename browser interaction: %v\n%s", err, out)
	}
}

const busyRenameBrowserScript = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
function extract(start, end) {
  const a = source.indexOf(start), b = source.indexOf(end, a + start.length);
  assert(a >= 0 && b > a, 'missing function boundary: ' + start);
  return source.slice(a, b);
}
function fixture() {
  const calls = [], toasts = [], timers = [];
  const input = {value:'', selectionStart:0};
  const c = {
    console, setTimeout: fn => { timers.push(fn); return timers.length; },
    document: {getElementById: id => id === 'inputField' ? input : null},
    currentSessionId:'S', runningSessionId:'S', turnRunning:true, pendingAsk:null,
    attachments:[], queuedTurns:[], queuedSessionId:null, queuedTurnMenuIndex:-1,
    drainingQueuedTurns:false, desktopPreferences:{busyEnter:'queue'},
    sessions:[{id:'S', title:'Old title'}, {id:'B', title:'Other title'}],
    COMPOSER_COMMANDS:[{name:'/rename'}, {name:'/title'}],
    parallelTurnsEnabled:()=>false, isViewedTurnRunning:()=>true,
    uiText:(en, zh)=>zh,
    autoResize(){}, closeCommandMenu(){}, renderAttachments(){}, renderQueuedTurns(){},
    saveSessionQueue:()=>({draining:false}),
    prepareDraftSession:async()=>true, showToast:value=>toasts.push(value),
    loadSessions:async()=>{},
    fetch:async (url, options) => {
      calls.push({url, body:options && options.body ? JSON.parse(options.body) : null});
      return {ok:true, json:async()=>({})};
    },
    submitBusyInput:async()=>{throw new Error('rename went through the busy message queue');},
  };
  vm.createContext(c);
  vm.runInContext(extract('async function sendMessage(', 'async function submitBusyInput('), c);
  vm.runInContext(extract('async function drainQueuedTurns(', 'async function syncViewedSessionHistory('), c);
  vm.runInContext(extract('async function runTurnItem(', 'const MESSAGE_ACTION_ICONS'), c);
  vm.runInContext(extract('async function executeComposerCommand(', 'async function runSessionCommand('), c);
  vm.runInContext(extract('async function renameCurrentSessionFromCommand(', 'async function branchCurrentSessionFromCommand('), c);
  return {c, input, calls, toasts, timers};
}

(async () => {
  // During a running turn, rename and title execute locally. They must never
  // become queued/steered text or reach the agent endpoint.
  const live = fixture();
  live.input.value = '/rename New title';
  await live.c.sendMessage();
  assert.equal(live.input.value, '');
  assert.equal(live.c.queuedTurns.length, 0);
  assert.deepEqual(live.calls, [{url:'/api/sessions/rename', body:{id:'S', title:'New title'}}]);
  live.input.value = '/TITLE Another title';
  await live.c.sendMessage('send');
  assert.equal(live.calls.length, 2);
  assert.deepEqual(live.calls[1], {url:'/api/sessions/rename', body:{id:'S', title:'Another title'}});
  live.c.pendingAsk = {id:'model-question'};
  live.input.value = '/rename During question';
  await live.c.sendMessage();
  assert.deepEqual(live.calls[2], {url:'/api/sessions/rename', body:{id:'S', title:'During question'}},
    'a local rename must not be submitted as an answer to an active model question');
  live.c.pendingAsk = null;
  live.c.attachments = [{name:'keep.png'}];
  live.input.value = '/title Keep attachment';
  await live.c.sendMessage();
  assert.equal(live.input.value, '/title Keep attachment', 'an invalid rename preserves the composer draft');
  assert.equal(live.c.attachments.length, 1);
  assert.equal(live.calls.length, 3, 'an attached rename is not sent to any endpoint');
  live.c.attachments = [];

  // A different session may be running when the selected session is renamed.
  live.c.currentSessionId = 'B';
  live.c.runningSessionId = 'S';
  live.input.value = '/rename Side title';
  await live.c.sendMessage();
  assert.deepEqual(live.calls[3], {url:'/api/sessions/rename', body:{id:'B', title:'Side title'}});

  // Pre-existing queued commands belong to the queue owner. Draining them
  // locally must leave the following user turn in order and never send the
  // slash text through /api/turns.
  const queued = fixture();
  queued.c.queuedTurns = [{text:'/title Queued title', images:[]}, {text:'Next real prompt', images:[]}];
  queued.c.queuedSessionId = 'S';
  queued.c.currentSessionId = 'B';
  queued.c.turnRunning = false;
  queued.c.isViewedTurnRunning = () => false;
  await queued.c.drainQueuedTurns();
  assert.equal(queued.c.queuedTurns.length, 2, 'other session cannot drain this queue');
  queued.c.currentSessionId = 'S';
  await queued.c.drainQueuedTurns();
  assert.deepEqual(queued.calls, [{url:'/api/sessions/rename', body:{id:'S', title:'Queued title'}}]);
  assert.equal(queued.c.queuedTurns.length, 1);
  assert.equal(queued.c.queuedTurns[0].text, 'Next real prompt');
  assert.equal(queued.timers.length, 1, 'local rename schedules the next queued item');
  const modelItems = [];
  queued.c.runTurnItem = async item => { modelItems.push(item.text); return true; };
  await queued.c.drainQueuedTurns();
  assert.deepEqual(modelItems, ['Next real prompt']);
  assert.equal(queued.c.queuedTurns.length, 0);

  const attached = fixture();
  attached.c.turnRunning = false;
  attached.c.isViewedTurnRunning = () => false;
  attached.c.queuedTurns = [
    {text:'/rename Queued attachment', images:[{name:'keep.png'}]},
    {text:'Later prompt', images:[]},
  ];
  attached.c.queuedSessionId = 'S';
  await attached.c.drainQueuedTurns();
  assert.deepEqual(attached.calls, [], 'legacy attached rename never reaches the model or rename endpoint');
  assert.equal(attached.c.queuedTurns.length, 2, 'invalid queued item and following prompt remain in order');
  assert.equal(attached.c.queuedTurns[1].text, 'Later prompt');

  // Capture the owner before a native title prompt can change the selection.
  const owner = fixture();
  owner.c.prompt = () => { owner.c.currentSessionId = 'B'; return 'Prompted title'; };
  await owner.c.executeComposerCommand('/rename', 'S');
  assert.deepEqual(owner.calls, [{url:'/api/sessions/rename', body:{id:'S', title:'Prompted title'}}]);
})().catch(error => { console.error(error); process.exitCode = 1; });
`
