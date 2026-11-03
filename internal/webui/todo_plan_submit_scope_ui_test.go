package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestTodoPlanRejectedAndUncertainSubmission(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"chat.js", "app.js"} {
		data, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(data)
	}
	input, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
function extract(source,start,next){const a=source.indexOf(start),b=source.indexOf(next,a+start.length);assert(a>=0&&b>a,start);return source.slice(a,b)}
function element(){const classes=new Set();return {hidden:false,textContent:'',innerHTML:'',style:{},classList:{toggle:(n,on)=>on?classes.add(n):classes.delete(n),contains:n=>classes.has(n)},setAttribute(){}}}
const unfinished=[{content:'Existing unfinished task',status:'in_progress'}];
const completed=[{content:'Existing unfinished task',status:'completed'}];
function status(turn,items,running=false){return {activeSessionId:'A',planTurn:turn,planItems:items,turnRunning:running,runningSessionId:running?'A':''}}
function response(statusCode,data){return {ok:statusCode>=200&&statusCode<300,status:statusCode,json:async()=>data}}
function fixture(){
 const elements=Object.fromEntries(['todoPlanDock','todoPlanPopover','todoPlanTrigger','todoPlanStepLabel','todoPlanCounts','todoPlanList','sendBtn'].map(id=>[id,element()]));
 const c={currentSessionId:'A',turnRunning:false,runningSessionId:null,stopRequestPending:false,
  backgroundContinuationGeneration:0,backgroundContinuationGenerations:new Map(),pendingForegroundRequest:null,resumeSessionGeneration:0,
  queuedTurns:[],queuedSessionId:null,drainingQueuedTurns:false,runningTurnNeedsHistorySync:false,runningTurnIncompleteReason:'',streamedTextThisTurn:false,
  statusRequestGeneration:0,renderingActivityHistory:false,pausedQueueSessions:new Set(),
  document:{getElementById:id=>elements[id]||null},escHtml:String,escAttr:String,renderStatusSnapshot(){},queueMicrotask,console,
  syncTurnControls(){},renderSessions(){},resumeAutoScroll(){},addMessage(){},beginUserTurn(){},finishUserTurn(){},
  updateSendBtn(){},loadSessions(){},loadSessionStatsbar(){},showError(){},showToast(){},canResumeCurrentTurn:()=>true,
 };
 vm.createContext(c);
 vm.runInContext(extract(sources['chat.js'],'let todoPlanItems =','let pendingAsk ='),c);
 vm.runInContext(extract(sources['chat.js'],'function setTurnRunning(','// Detach transient DOM state'),c);
 vm.runInContext(extract(sources['chat.js'],'function closeMismatchedSubAgentDetails()','function newChat()'),c);
 vm.runInContext(extract(sources['chat.js'],'async function runTurnItem(','const MESSAGE_ACTION_ICONS'),c);
 vm.runInContext(extract(sources['app.js'],'async function pollStatus(','function closeContextMeterPopover('),c);
 c.applyStatusPlanSnapshot(status(7,unfinished));
 return {c,elements};
}
(async()=>{
 for(const code of [400,403,409]){
  const {c,elements}=fixture();
  c.fetch=async(url,options)=>options?.method==='POST'?response(code,{error:'request not accepted'}):response(200,status(7,unfinished));
  assert.equal(await c.runTurnItem({text:'Start a different task'}),false);
  assert.equal(c.applyStatusPlanSnapshot(status(7,unfinished)),true,code+' rejection stranded the old task behind an incremented floor');
  assert.equal(elements.todoPlanDock.hidden,false);
  c.fetch=async(url,options)=>options?.method==='POST'?response(200,{sessionId:'A',text:''}):response(200,status(7,unfinished));
  assert.equal(await c.runTurnItem({resume:true,resumeToken:'existing-checkpoint',skipClearTodoPlan:true}),true,'resume after rejected submission');
  assert.match(elements.todoPlanList.innerHTML,/Existing unfinished task/);
 }
 // Historical TaskCreate activity does not confirm this pending POST.
 // A late preflight rejection must restore the original scope even while
 // that original scope is still running, when idle reconciliation cannot help.
 {
  const {c}=fixture();
  c.fetch=async(url,options)=>{
   if(options?.method==='POST') {
    c.restoreTodoPlanFromHistory([
     {role:'user',content:[{type:'text',text:'Existing task'}]},
     {role:'assistant',content:[{type:'tool_use',tool_use_id:'old-create',name:'TaskCreate',input:{subject:'Existing unfinished task'}}]},
     {role:'user',content:[{type:'tool_result',tool_use_id:'old-create',content:'created'}]},
    ]);
    return response(409,{error:'original turn is still running'});
   }
   return response(200,status(7,unfinished,true));
  };
  assert.equal(await c.runTurnItem({text:'Rejected overlapping task'}),false);
  assert.equal(c.todoPlanScope('A').minimumTurn,7,'old history prevented preflight rollback while original turn runs');
  assert.equal(c.applyStatusPlanSnapshot(status(7,unfinished,true)),true);
 }
 // Replaying completed Todo history while the new POST remains unconfirmed
 // cannot put the old checkmark back or certify the new task as planned.
 {
  const {c,elements}=fixture();
  c.startTodoPlanTurn('A');
  c.restoreTodoPlanFromHistory([
   {role:'user',content:[{type:'text',text:'Completed old task'}]},
   {role:'assistant',content:[{type:'tool_use',tool_use_id:'old-todo',name:'TodoWrite',input:{todos:completed}}]},
   {role:'user',content:[{type:'tool_result',tool_use_id:'old-todo',content:'done'}]},
  ]);
  assert.equal(elements.todoPlanDock.hidden,true,'pending new submission restored the old completed dock');
  assert.equal(c.todoPlanScope('A').awaitingFresh,true);
  assert.equal(c.todoPlanScope('A').livePlan,false,'historical plan confirmed the pending submission');
 }
 // A client error that arrives after an observed live plan cannot undo
 // the newer scope, and an older POST cannot roll back a newer submission.
 for(const observed of ['live-plan','new-submission']) {
  const {c}=fixture();
  c.fetch=async(url,options)=>{
   if(options?.method==='POST') {
    if(observed==='live-plan') c.applyTodoSnapshot('TodoWrite',{todos:[{content:'Current live task',status:'pending'}]});
    else c.startTodoPlanTurn('A');
    return response(409,{error:'late rejection'});
   }
   return response(200,status(7,unfinished));
  };
  assert.equal(await c.runTurnItem({text:'New task'}),false);
  assert.equal(c.todoPlanScope('A').minimumTurn,8,observed+' allowed an old response to roll back the scope');
  assert.equal(c.applyStatusPlanSnapshot(status(7,unfinished)),false);
 }
 // Network failure does not prove whether the server accepted the task. It
 // must keep the tentative floor until a later status reconciles ownership.
 {
  const {c}=fixture();
  let floorAtStatusRequest;
  c.fetch=async(url,options)=>{
   if(options?.method==='POST') throw new Error('connection lost');
   floorAtStatusRequest=c.todoPlanScope('A').minimumTurn;
   return response(200,status(7,unfinished));
  };
  assert.equal(await c.runTurnItem({text:'Maybe accepted'}),false);
  assert.equal(floorAtStatusRequest,8,'network exception blindly rolled back before status reconciliation');
  assert.equal(c.applyStatusPlanSnapshot(status(7,unfinished)),true,'idle authoritative old scope should allow continuation');
  assert.equal(c.applyStatusPlanSnapshot(status(7,completed)),false,'uncertain old completed snapshot must stay hidden');
 }
 // Restoring the old structured-plan history after navigation does not
 // prove the uncertain new submission ran. Its idle baseline may reconcile.
 {
  const {c}=fixture();
  c.fetch=async(url,options)=>{
   if(options?.method==='POST') throw new Error('connection lost before acceptance is known');
   return response(200,status(7,unfinished,true));
  };
  assert.equal(await c.runTurnItem({text:'Uncertain new task'}),false);
  assert.equal(c.todoPlanScope('A').minimumTurn,8);
  c.restoreTodoPlanFromHistory([
   {role:'user',content:[{type:'text',text:'Existing task'}]},
   {role:'assistant',content:[{type:'tool_use',tool_use_id:'existing',name:'TaskCreate',input:{subject:'Existing unfinished task'}}]},
   {role:'user',content:[{type:'tool_result',tool_use_id:'existing',content:'created'}]},
  ]);
  assert.equal(c.applyStatusPlanSnapshot(status(7,unfinished)),true,'historical planning blocked uncertain idle reconciliation');
  assert.equal(c.todoPlanScope('A').minimumTurn,7);
 }
 {
  const {c}=fixture();
  c.fetch=async(url,options)=>{
   if(options?.method==='POST') throw new Error('connection lost after submission');
   return response(200,status(7,unfinished,true));
  };
  assert.equal(await c.runTurnItem({text:'Still running on server'}),false);
  assert.equal(c.todoPlanScope('A').minimumTurn,8,'running ambiguous request rolled back');
  assert.equal(c.applyStatusPlanSnapshot(status(7,completed)),false);
  assert.equal(c.applyStatusPlanSnapshot(status(8,[{content:'Accepted new task',status:'pending'}],true)),true);
  assert.equal(c.applyStatusPlanSnapshot(status(8,[{content:'Accepted new task',status:'completed'}])),true);
 }
})().catch(error=>{console.error(error);process.exitCode=1});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("todo submit scope harness: %v\n%s", err, out)
	}
}
