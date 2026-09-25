package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestConcurrentDesktopTurnLifecycleUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	content, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(string(content))
	const script = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
const extract=(start,next)=>{const a=source.indexOf(start),b=source.indexOf(next,a+start.length);assert(a>=0&&b>a,start);return source.slice(a,b);};
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const ok=data=>({ok:true,json:async()=>data});
const tick=()=>new Promise(resolve=>setImmediate(resolve));
function context(){
 const requests=[],errors=[],added=[],controls=[],elements=new Map();
 const el=id=>{if(!elements.has(id))elements.set(id,{value:'',style:{},disabled:false,textContent:'',innerHTML:'',remove(){},classList:{add(){},remove(){},toggle(){}},setAttribute(){}});return elements.get(id);};
 const c={Map,Set,Array,console,window:{},document:{getElementById:el,querySelector:()=>null,querySelectorAll:()=>[]},
  lastStatusSnapshot:{isolatedTurnsEnabled:true,isolatedTurnSessions:[],turnRunning:false},currentSessionId:'A',runningSessionId:null,turnRunning:false,
  pendingForegroundRequest:null,queuedTurns:[],queuedSessionId:null,queuedTurnMenuIndex:-1,queuedTurnPendingItem:null,drainingQueuedTurns:false,
  sessions:[{id:'A'},{id:'B'}],pendingAsk:null,attachments:[],desktopPreferences:{busyEnter:'queue'},resumeSessionGeneration:0,
  backgroundContinuationGenerations:new Map(),backgroundContinuationGeneration:0,runningTurnIncompleteReason:'',runningTurnNeedsHistorySync:false,
  streamedTextThisTurn:false,messages:[],stopRequestPending:false,
  clearTodoPlan(){},resumeAutoScroll(){},beginUserTurn(){},finishUserTurn(){},syncTurnControls(){},updateSendBtn(){},
  renderSessions(){},renderAttachments(){},autoResize(){},closeCommandMenu(){},executeComposerCommand:async()=>false,
  renderQueuedTurns(){if(typeof c.saveSessionQueue==='function')c.saveSessionQueue();},
  addMessage:(role,text)=>added.push({session:c.currentSessionId,role,text}),
  setTurnRunning:(running,id)=>{c.turnRunning=running;c.runningSessionId=running?id:null;controls.push({running,id});},
  showError:error=>errors.push(error),showToast:error=>errors.push(error),loadSessions:async()=>{},loadSessionStatsbar(){},
  loadArtifactsForSession:async()=>{},loadSessionFiles(){},syncViewedSessionHistory:async()=>true,setTimeout(){},
  invalidateSessionAsyncLoads(){c.resumeSessionGeneration++;},resetTraceForSession(){},resetSessionFiles(){},resetArtifactsForSession(){},
  resetTurnState(){c.pendingAsk=null;c.setTurnRunning(false);},updateScrollFollowUI(){},applyLanguage(){},updateEmptyLayout(){},
 };
 vm.createContext(c);
 vm.runInContext(extract('const foregroundRequests = new Map();','function syncTurnControls()'),c);
 vm.runInContext(extract('function sameSession(d)','let thinkingEl'),c);
 vm.runInContext(extract('async function runTurnItem(item)','const MESSAGE_ACTION_ICONS'),c);
 vm.runInContext(extract('async function sendMessage(busyBehavior)','function queuedTurnIcon('),c);
 vm.runInContext(extract('async function stopTurn()','function startStreamingMessage('),c);
 vm.runInContext(extract('function newChat()','// The POST resolves'),c);
 vm.runInContext(extract('async function drainQueuedTurns()','async function syncViewedSessionHistory('),c);
 c.fetch=(url,options)=>{const pending=deferred();requests.push({url,body:JSON.parse(options?.body||'{}'),pending});return pending.promise;};
 return{c,requests,errors,added,controls,el};
}
(async()=>{
 // Both sends reach the server before either finishes; A cannot clear B's controls.
 {
  const f=context(),{c}=f;
  f.el('inputField').value='A prompt';const a=c.sendMessage();await tick();
  assert.equal(f.requests.length,1);assert.equal(f.requests[0].body.sessionId,'A');
  c.currentSessionId='B';c.restoreSessionQueue('B');c.syncTrackedRunningState();
  f.el('inputField').value='B prompt';const b=c.sendMessage();await tick();
  assert.equal(f.requests.length,2,'second workspace remained blocked behind first');
  assert.equal(f.requests[1].body.sessionId,'B');assert.equal(c.isViewedTurnRunning(),true);
  f.requests[0].pending.resolve(ok({sessionId:'A',text:'A done'}));await a;
  assert.equal(c.turnRunning,true);assert.equal(c.runningSessionId,'B');
  assert(!f.added.some(item=>item.session==='B'&&item.text==='A done'));
  f.requests[1].pending.resolve(ok({sessionId:'B',text:'B done'}));await b;
  assert.equal(c.turnRunning,false);assert.deepEqual(f.errors,[]);
 }
 // A new chat is available while A runs; its stable server ID precedes its first turn.
 {
  const f=context(),{c}=f;const a=c.runTurnItem({text:'A prompt',images:[]});
  c.lastStatusSnapshot={isolatedTurnsEnabled:true,isolatedTurnSessions:['A'],turnRunning:true,runningSessionId:'A'};
  c.newChat();assert.equal(c.currentSessionId,null);assert.equal(c.sameSession({session:'A'}),false);
  f.el('inputField').value='new prompt';const b=c.sendMessage();await tick();
  assert.equal(f.requests[1].url,'/api/sessions');
  f.requests[1].pending.resolve(ok({id:'C'}));await tick();
  assert.equal(c.currentSessionId,'C');assert.equal(f.requests[2].body.sessionId,'C');
  f.requests[0].pending.resolve(ok({sessionId:'A',text:'A done'}));await a;
  assert.equal(c.currentSessionId,'C');assert.equal(c.runningSessionId,'C');
  f.requests[2].pending.resolve(ok({sessionId:'C',text:'C done'}));await b;
 }
 // Completing a background turn never replaces a fresh blank composer.
 {
  const f=context(),{c}=f;const a=c.runTurnItem({text:'A'});c.newChat();
  f.requests[0].pending.resolve(ok({sessionId:'A',text:'done'}));await a;
  assert.equal(c.currentSessionId,null);assert(!f.added.some(item=>item.session===null));
 }
 // Stop targets the selected live session and preserves another session's queue.
 {
  const f=context(),{c}=f;
  c.lastStatusSnapshot={isolatedTurnsEnabled:true,isolatedTurnSessions:['A','B'],turnRunning:true,runningSessionId:'A'};
  c.queuedTurns=[{text:'A next'}];c.queuedSessionId='A';c.saveSessionQueue();
  c.currentSessionId='B';c.restoreSessionQueue('B');c.queuedTurns=[{text:'B next'}];c.queuedSessionId='B';c.saveSessionQueue();
  c.turnRunning=true;c.runningSessionId='A';const stop=c.stopTurn();
  assert.equal(f.requests[0].body.sessionId,'B');
  f.requests[0].pending.resolve(ok({stopped:true}));await stop;
  c.currentSessionId='A';c.restoreSessionQueue('A');assert.equal(c.queuedTurns[0].text,'A next');
  c.currentSessionId='B';c.restoreSessionQueue('B');assert.equal(c.queuedTurns.length,0);
 }
 // Independent stop requests can remain in flight together without clearing each other's state.
 {
  const f=context(),{c}=f;
  c.lastStatusSnapshot={isolatedTurnsEnabled:true,isolatedTurnSessions:['A','B'],turnRunning:true,runningSessionId:'A'};
  c.turnRunning=true;c.runningSessionId='A';const a=c.stopTurn();
  c.currentSessionId='B';const b=c.stopTurn();
  assert.equal(f.requests.length,2);assert.equal(f.requests[1].body.sessionId,'B');
  f.requests[0].pending.resolve(ok({stopped:true}));await a;assert.equal(c.stopRequestPending,true);
  f.requests[1].pending.resolve(ok({stopped:true}));await b;assert.equal(c.stopRequestPending,false);
 }
 // Draining A while switching to B cannot overwrite B's queue/draining flag.
 {
  const f=context(),{c}=f;c.queuedTurns=[{text:'A one'},{text:'A two'}];c.queuedSessionId='A';
  const drain=c.drainQueuedTurns();await tick();
  c.saveSessionQueue();c.currentSessionId='B';c.restoreSessionQueue('B');
  c.queuedTurns=[{text:'B next'}];c.queuedSessionId='B';c.saveSessionQueue();
  f.requests[0].pending.resolve(ok({sessionId:'A',text:'done'}));await drain;
  assert.equal(c.queuedTurns[0].text,'B next');assert.equal(c.drainingQueuedTurns,false);
  c.currentSessionId='A';c.restoreSessionQueue('A');assert.equal(c.queuedTurns[0].text,'A two');assert.equal(c.drainingQueuedTurns,false);
 }
 // A cancelled draft selection cannot steal the later selected session.
 {
  const f=context(),{c}=f;c.currentSessionId=null;
  const create=c.prepareDraftSession();c.resumeSessionGeneration++;c.currentSessionId='B';
  f.requests[0].pending.resolve(ok({id:'C'}));assert.equal(await create,false);assert.equal(c.currentSessionId,'B');
 }
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("concurrent Desktop lifecycle UI checks failed: %v\n%s", err, output)
	}
}
