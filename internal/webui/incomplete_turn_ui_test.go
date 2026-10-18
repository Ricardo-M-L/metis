package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestIncompleteTurnLifecycleUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict'), vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const extract = (start, next) => {
  const a = source.indexOf(start), b = source.indexOf(next, a + start.length);
  assert(a >= 0 && b > a, start);
  return source.slice(a, b);
};
function fixture(lang = 'en') {
  const toasts = [], errors = [], finishes = [], timers = [], callbacks = {}, added = [];
  const noop = () => {};
  const c = {console, Map, Set, Array, AbortController,
    window: {EventSource: true}, EventSource: class { addEventListener(type, handler) { callbacks['raw:'+type] = handler; } }, eventSource: null,
    document: {getElementById: () => ({disabled:false, remove(){}})},
    currentSessionId:'A', runningSessionId:null, resumeSessionGeneration:0,
    backgroundContinuationGenerations:new Map(), backgroundContinuationGeneration:0,
    runningTurnIncompleteReason:'', runningTurnNeedsHistorySync:false,
    pendingForegroundRequest:null, streamedTextThisTurn:false, pausedQueueSessions:new Set(),
    queuedTurns:[{text:'later'}], queuedSessionId:'A', drainingQueuedTurns:false,
    uiText:(en,zh) => lang === 'en' ? en : zh,
    acceptLiveEvent:()=>true, sameSession:data=>data.session==='A',
    onLive:(kind,handler) => callbacks[kind] = handler, onInteraction:noop,
    showToast:text => toasts.push(text), showError:text => errors.push(text),
    finishUserTurn:(state,reason) => finishes.push({state,reason}),
    beginUserTurn:() => { c.runningTurnIncompleteReason = ''; },
    setTurnRunning:(running,id) => { c.runningSessionId = running ? id : null; },
    addMessage:(role,text) => added.push({role,text}), loadSessions:async()=>{},
    isSessionQueuePaused:()=>false, setTimeout:handler => timers.push(handler),
  };
  for (const key of ['handleTextDelta','handleToolStart','handleToolResult','handleToolArgsDelta',
    'handleSubagentLifecycle','handleThinkingDelta','handleTokensEvent','handleContextEvent',
    'handleCompactionStart','handleCompactionProgress','handleCompactionEnd','handleAskUser',
    'handleRedactedThinking','handlePermissionRequest','handleInteractionResolved','endStreamingMessage',
    'handleBackgroundContinuation','clearTodoPlan','resumeAutoScroll','closeMismatchedSubAgentDetails',
    'updateSendBtn','loadSessionStatsbar','drainQueuedTurns']) c[key] = noop;
  vm.createContext(c);
  vm.runInContext(extract('function connectEvents()', 'function handleBackgroundContinuation(d)'),c);
  vm.runInContext(extract('async function runTurnItem(item)', 'const MESSAGE_ACTION_ICONS'),c);
  c.connectEvents();
  return {c,toasts,errors,finishes,timers,callbacks,added};
}
const localReasons = [
  ['budget_usd', /spending limit/i, /费用上限/],
  ['turn_wall_clock', /time limit/i, /时间上限/],
  ['max_iterations', /iteration limit/i, /迭代上限/],
  ['loop_detected', /repeated tool calls/i, /重复工具调用/],
  ['stuck_after_reset', /progress/i, /进展/],
  ['diminishing_returns', /progress/i, /进展/],
];
(async()=>{
  for (const lang of ['en','zh']) {
    const checkpoint=fixture(lang);
    checkpoint.callbacks['raw:info']({data:JSON.stringify({source:'iteration_continuation',session:'A'})});
    assert.equal(checkpoint.toasts.length,1,'one notification for the checkpoint Info');
    assert.match(checkpoint.toasts[0],lang==='en'?/continuing the current task/i:/继续执行当前任务/);
    assert.equal(checkpoint.finishes.length,0,'a checkpoint must not finish the task');
    checkpoint.callbacks['raw:info']({data:JSON.stringify({source:'iteration_continuation',session:'B'})});
    assert.equal(checkpoint.toasts.length,1,'offscreen checkpoints do not replace current session feedback');
    for (const [reason,en,zh] of localReasons) {
      const f=fixture(lang);
      f.callbacks.loop_done({incomplete:true,stopReason:reason});
      assert.equal(f.finishes.at(-1).state,'incomplete',reason);
      assert.match(f.toasts.at(-1),lang==='en'?en:zh,reason);
      assert(!/APIError|Agent error|Error:/.test(f.toasts.join(' ')),reason);
      assert.equal(f.errors.length,0);
    }
    // The typed HTTP body remains authoritative when no SSE terminal arrives.
    const f=fixture(lang);
    f.c.fetch=async()=>({ok:false,status:502,json:async()=>({incomplete:true,stopReason:'budget_usd',error:'task incomplete: budget_usd'})});
    assert.equal(await f.c.runTurnItem({text:'work'}),false);
    assert.equal(f.finishes.at(-1).state,'incomplete');
    assert.equal(f.finishes.at(-1).reason,'budget_usd');
    assert.match(f.toasts.join(' '),lang==='en'?/spending limit/i:/费用上限/);
    assert.equal(f.errors.length,0,'local exhaustion must not render a request error');
    assert.equal(f.timers.length,0,'incomplete turns must not drain queued messages');
    assert.equal(f.c.queuedTurns.length,1);
  }
  // A late successful HTTP response cannot turn an incomplete SSE outcome into success.
  {
    const f=fixture();
    f.c.fetch=async()=>{
      f.callbacks.loop_done({incomplete:true,stopReason:'turn_wall_clock'});
      return {ok:true,json:async()=>({sessionId:'A'})};
    };
    assert.equal(await f.c.runTurnItem({text:'work'}),false);
    assert.equal(f.finishes.at(-1).state,'incomplete');
    assert.equal(f.errors.length,0);
    assert.equal(f.timers.length,0);
  }
  // A real HTTP failure keeps error semantics even after a local SSE terminal.
  {
    const f=fixture();
    f.c.fetch=async()=>{
      f.callbacks.loop_done({incomplete:true,stopReason:'budget_usd'});
      return {ok:false,status:503,json:async()=>({error:'HTTP 503 upstream unavailable'})};
    };
    assert.equal(await f.c.runTurnItem({text:'work'}),false);
    assert.equal(f.finishes.at(-1).state,'error');
    assert.deepEqual(f.errors,['HTTP 503 upstream unavailable']);
  }
  // Explicit stops, successful turns and provider failures retain their meaning.
  {
    const f=fixture();
    f.callbacks.loop_done({incomplete:true,stopReason:'stopped'});
    assert.equal(f.finishes.at(-1).state,'stopped');
    f.callbacks.agent_error({message:'provider failed'});
    assert.equal(f.finishes.at(-1).state,'error');
    f.c.queuedTurns=[];
    f.c.fetch=async()=>({ok:true,json:async()=>({sessionId:'A',text:'done'})});
    assert.equal(await f.c.runTurnItem({text:'work'}),true);
    assert.equal(f.finishes.at(-1).state,'completed');
  }
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("incomplete turn UI: %v\n%s", err, output)
	}
}
