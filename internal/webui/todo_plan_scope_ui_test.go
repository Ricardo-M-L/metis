package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestTodoPlanScopeAndHistoryIsolation(t *testing.T) {
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
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function extract(source, start, next) {
 const from = source.indexOf(start), to = source.indexOf(next, from + start.length);
 assert(from >= 0 && to > from, start);
 return source.slice(from, to);
}
function element() {
 const classes = new Set();
 return {hidden:false,textContent:'',innerHTML:'',style:{},
  classList:{toggle:(name,on)=>on?classes.add(name):classes.delete(name),contains:name=>classes.has(name)},
  setAttribute(){}};
}
function fixture() {
 const elements = Object.fromEntries(['todoPlanDock','todoPlanPopover','todoPlanTrigger','todoPlanStepLabel','todoPlanCounts','todoPlanList'].map(id=>[id,element()]));
 const c = {currentSessionId:'A',turnRunning:false,runningSessionId:null,resumeSessionGeneration:0,
  statusRequestGeneration:0,renderingActivityHistory:false,
  document:{getElementById:id=>elements[id]||null},escHtml:String,escAttr:String,
  renderStatusSnapshot(){},queueMicrotask,console,
 };
 vm.createContext(c);
 vm.runInContext(extract(sources['chat.js'],'let todoPlanItems =','let pendingAsk ='),c);
 vm.runInContext(extract(sources['chat.js'],'const INTERNAL_TRANSCRIPT_SECTION_RE','// Rebuild the full transcript'),c);
 vm.runInContext(extract(sources['app.js'],'async function pollStatus(','function closeContextMeterPopover('),c);
 return {c,elements,dock:elements.todoPlanDock};
}
const oldPlan = [{content:'Old inspect',status:'completed'},{content:'Old change',status:'completed'},{content:'Old verify',status:'completed'}];
const status = (turn,planItems,running=false) => ({activeSessionId:'A',planTurn:turn,planItems,turnRunning:running,runningSessionId:running?'A':''});
const history = input => [
 {role:'user',content:[{type:'text',text:'Old task'}]},
 {role:'assistant',content:[{type:'tool_use',tool_use_id:'old-plan',name:'TodoWrite',input:{todos:oldPlan}}]},
 {role:'user',content:[{type:'tool_result',tool_use_id:'old-plan',content:'done'}]},
 ...input,
];
(async()=>{
 // A response already in flight when the next turn clears the dock cannot
 // put the previous plan back, even when the selected session did not change.
 {
  const {c,dock}=fixture();
  c.applyStatusPlanSnapshot(status(4,oldPlan));
  let resolve;
  c.fetch=()=>new Promise(done=>resolve=done);
  const pending=c.pollStatus();
  c.clearTodoPlan();
  resolve({ok:true,json:async()=>status(4,oldPlan)});
  await pending;
  assert.equal(dock.hidden,true,'late same-session status resurrected the completed plan');
 }
 // Later polls still carrying the old turn must be rejected, while current
 // plans may progress through pending and all-completed normally.
 {
  const {c,dock,elements}=fixture();
  c.applyStatusPlanSnapshot(status(4,oldPlan));
  c.startTodoPlanTurn('A');
  assert.equal(c.applyStatusPlanSnapshot(status(4,oldPlan,true)),false);
  assert.equal(dock.hidden,true);
  assert.equal(c.applyStatusPlanSnapshot(status(5,[{content:'New task',status:'pending'}],true)),true);
  assert.equal(dock.hidden,false);
  assert.match(elements.todoPlanList.innerHTML,/New task/);
  assert.doesNotMatch(elements.todoPlanList.innerHTML,/Old inspect/);
  c.applyStatusPlanSnapshot(status(5,[{content:'New task',status:'completed'}]));
  assert.match(elements.todoPlanStepLabel.textContent,/已完成 1 \/ 1/);
  assert.equal(c.applyStatusPlanSnapshot(status(4,oldPlan)),false);
  c.currentSessionId='B';
  assert.equal(c.applyStatusPlanSnapshot(status(5,oldPlan)),false,'cross-session status applied');
 }
 // Before the first scoped status arrives, a new task cannot accept a stale
 // completed legacy snapshot. A live plan is enough to establish freshness.
 {
  const {c,dock}=fixture();
  c.startTodoPlanTurn('A');
  assert.equal(c.applyStatusPlanSnapshot({activeSessionId:'A',planItems:oldPlan}),false);
  assert.equal(dock.hidden,true);
  c.applyTodoSnapshot('TodoWrite',{todos:[{content:'Current',status:'completed'}]});
  assert.equal(c.applyStatusPlanSnapshot(status(1,[{content:'Current',status:'completed'}])),true);
  assert.equal(dock.hidden,false);
 }
 // An unknown first snapshot is only a baseline. An empty or unfinished
 // old snapshot cannot certify that the following completed snapshot is new.
 for (const first of [[], [{content:'Old unfinished',status:'in_progress'}]]) {
  const {c}=fixture();
  c.startTodoPlanTurn('A');
  c.applyStatusPlanSnapshot(status(4,first));
  assert.equal(c.applyStatusPlanSnapshot(status(4,oldPlan)),false,'unknown baseline unlocked old completed snapshot');
  assert.equal(c.applyStatusPlanSnapshot(status(5,[{content:'New completed',status:'completed'}])),true,'advancing beyond the baseline confirms a new turn');
 }
 // Unfinished plans survive pause/resume; a resume does not call the new-turn
 // boundary and must not require the backend to increment its turn number.
 {
  const {c,dock,elements}=fixture();
  const unfinished=[{content:'Resume this',status:'in_progress'}];
  c.applyStatusPlanSnapshot(status(7,unfinished));
  c.turnRunning=true;c.runningSessionId='A';
  assert.equal(c.applyStatusPlanSnapshot(status(7,unfinished,true)),true);
  assert.equal(dock.hidden,false);
  assert.match(elements.todoPlanList.innerHTML,/Resume this/);
 }
 // A genuine image/file-only message starts a new task. Runtime context,
 // tool results and a mid-turn user steer must not erase the active checklist.
 for(const type of ['image','image_url','document','file','audio','input_audio','video']) {
  const {c,dock}=fixture();
  c.restoreTodoPlanFromHistory(history([{role:'user',content:[{type,data:'fixture'}]}]));
  assert.equal(dock.hidden,true,type+' did not retire the prior plan');
 }
 for(const block of [{type:'text',text:'<system-reminder>internal</system-reminder>'},{type:'text',text:'hidden',synthetic:true},{type:'text',text:'[user steer mid-turn] more detail'},{type:'tool_result',tool_use_id:'other',content:'result'}]) {
  const {c,dock}=fixture();
  c.restoreTodoPlanFromHistory(history([{role:'user',content:[block]}]));
  assert.equal(dock.hidden,false,'non-human runtime input cleared the plan');
 }
 {
  const {c,dock}=fixture();
  c.restoreTodoPlanFromHistory(history([
   {role:'user',content:[{type:'text',text:'New task'}]},
   {role:'assistant',content:[{type:'tool_use',tool_use_id:'failed',name:'TodoWrite',input:{todos:oldPlan}}]},
   {role:'user',content:[{type:'tool_result',tool_use_id:'failed',is_error:true,content:'failed'}]},
  ]));
  assert.equal(dock.hidden,true,'failed planning tool revived a checklist');
 }
 // Re-mounting a historical tool row must only paint that row. It cannot
 // mutate the live dock or queue a poll that later overwrites it.
 {
  const {c,elements}=fixture();
  let polls=0;
  c.pollStatus=()=>{polls++};
  c.applyTodoSnapshot('TodoWrite',{todos:[{content:'Current task',status:'in_progress'}]});
  const chip={dataset:{tool:'TodoWrite',rowKey:'old',state:'running'},getAttribute:()=>JSON.stringify({todos:oldPlan}),querySelector:()=>null,closest:()=>null};
  Object.assign(c,{isChildToolEvent:()=>false,pendingToolRows:()=>[chip],toolRowsInCurrentTurn:()=>[chip],
   setToolRowVisualState:(row,state)=>{row.dataset.state=state},isSubagentTool:()=>false,
   updateToolRowAccessibleLabel(){},isWebSearchTool:()=>false,toolDetails:{},activityGroupStats:new Map(),selectedToolId:null});
  vm.runInContext(extract(sources['chat.js'],'function handleToolResult(d) {','function parseGrepOutput(text)'),c);
  c.renderingActivityHistory=true;
  c.handleToolResult({tool:'TodoWrite',id:'old-plan',output:'done'});
  await Promise.resolve();
  assert.match(elements.todoPlanList.innerHTML,/Current task/);
  assert.doesNotMatch(elements.todoPlanList.innerHTML,/Old inspect/);
  assert.equal(polls,0,'historical tool scheduled a live status poll');
 }
 // A deferred history restore cannot cross either session navigation or the
 // next explicit task boundary in the same session.
 {
  const {c,dock}=fixture();
  c.scheduleTodoPlanHistoryRestore(history([]));
  c.currentSessionId='B';
  c.clearTodoPlan();
  await Promise.resolve();
  assert.equal(dock.hidden,true,'history restore crossed sessions');
  c.currentSessionId='A';
  c.scheduleTodoPlanHistoryRestore(history([]));
  c.startTodoPlanTurn('A');
  await Promise.resolve();
  assert.equal(dock.hidden,true,'old history restore crossed task boundary');
 }
})().catch(error=>{console.error(error);process.exitCode=1});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("todo scope harness: %v\n%s", err, out)
	}
}
