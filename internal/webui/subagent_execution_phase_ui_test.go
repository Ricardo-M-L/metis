package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestSubagentExecutionPhasePopover(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(source)
	start := strings.Index(js, "function subAgentText(en, zh)")
	end := strings.Index(js, "document.addEventListener('click', e =>")
	if start < 0 || end <= start {
		t.Fatal("cannot locate sub-agent status renderer")
	}
	const harness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const pop = { style: {display:'block'}, innerHTML:'', querySelectorAll:()=>[] };
const c = {
  document: {documentElement:{lang:'zh-CN'},getElementById:id=>id==='statusPopover'?pop:null},
  currentSessionId:'session-a', lastStatusSnapshot:null,
  escHtml:value=>String(value), escAttr:value=>String(value),
  uiText:(en,zh)=>c.document.documentElement.lang==='en'?en:zh,
};
vm.createContext(c);
vm.runInContext(source,c);
const agents = [
  {sessionId:'session-a',agentId:'execute-a',name:'execute-a',status:'running',executionPhase:'executing',holdsExecutionSlot:true},
  {sessionId:'session-a',agentId:'execute-b',name:'execute-b',status:'running',executionPhase:'executing',holdsExecutionSlot:true},
  {sessionId:'session-a',agentId:'bash',name:'bash',status:'running',executionPhase:'waiting_background',holdsExecutionSlot:false},
  {sessionId:'session-a',agentId:'queued',name:'queued',status:'queued',executionPhase:'queued',holdsExecutionSlot:false},
  {sessionId:'other',agentId:'foreign',name:'foreign',status:'running',executionPhase:'executing',holdsExecutionSlot:true},
];
c.lastStatusSnapshot={viewRoster:{sessionId:'session-a',agents,heldExecutionSlots:2,executionSlots:{held:2,total:8}}};
c.renderStatusPopover();
assert.match(pop.innerHTML,/执行槽（采样）.*2\s*\/\s*8/);
assert.match(pop.innerHTML,/当前会话子代理占用.*2/);
assert.equal((pop.innerHTML.match(/运行中 · /g)||[]).length,3,'three live agents can hold only two sampled slots');
for (const phase of ['执行中','等待后台任务','排队中']) assert.match(pop.innerHTML,new RegExp(phase));
assert.doesNotMatch(pop.innerHTML,/foreign/);
assert.match(pop.innerHTML,/运行中/,'lifecycle remains distinct from execution phase');
c.lastStatusSnapshot.viewRoster.agents=agents.concat([
  {sessionId:'session-a',agentId:'children',name:'children',status:'running',executionPhase:'waiting_children',holdsExecutionSlot:false},
  {sessionId:'session-a',agentId:'resume',name:'resume',status:'running',executionPhase:'waiting_slot',holdsExecutionSlot:false},
]);
c.renderStatusPopover();
assert.match(pop.innerHTML,/等待子代理/);assert.match(pop.innerHTML,/等待执行槽/);
c.document.documentElement.lang='en';c.renderStatusPopover();
assert.match(pop.innerHTML,/Execution slots \(sampled\).*2\s*\/\s*8/);
assert.match(pop.innerHTML,/Child slots in this session.*2/);
for (const phase of ['Executing','Waiting for background task','Waiting for child agents','Waiting for execution slot','Queued']) assert.match(pop.innerHTML,new RegExp(phase));
assert.doesNotMatch(pop.innerHTML,/执行槽|等待后台任务/);
c.lastStatusSnapshot={viewRoster:{sessionId:'session-a',agents:[{sessionId:'session-a',agentId:'old',status:'running'}]}};
c.renderStatusPopover();
assert.doesNotMatch(pop.innerHTML,/Execution slots \(sampled\)|Child slots in this session|0\s*\/\s*0/,'missing telemetry is not invented');
assert.match(pop.innerHTML,/Running/,'older worker snapshots retain lifecycle fallback');
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewBufferString(js[start:end])
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sub-agent execution phase UI: %v\n%s", err, output)
	}
}

func TestSubagentDetailExecutionPhaseUpdatesWithoutRedraw(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(source)
	start := strings.Index(js, "function subAgentDetailExecutionPhase(agent)")
	end := strings.Index(js, "function stopSubAgentElapsedTimer()")
	if start < 0 || end <= start {
		t.Fatal("cannot locate sub-agent detail phase updater")
	}
	const harness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0,'utf8');
const phaseEl={textContent:'',hidden:true},subtitle={textContent:''};
let bodyRedraws=0;
const agent={agentId:'child',sessionId:'session-a',status:'running',executionPhase:'executing',elapsedMs:90};
const c={currentSessionId:'session-a',lastStatusSnapshot:null,subAgentDetailState:{data:agent,ownerSessionId:'session-a'},
  document:{getElementById:id=>id==='subAgentDetailPhase'?phaseEl:id==='subAgentDetailDescription'?subtitle:null},
  statusRosterForSelectedSession:snapshot=>snapshot&&snapshot.viewRoster,
  subAgentDetailStatusLabel:item=>item.status==='running'?'Running':item.status==='completed'?'Completed':'Queued',
  subAgentExecutionPhaseLabel:phase=>({executing:'Executing',waiting_background:'Waiting for background task',waiting_children:'Waiting for child agents',waiting_slot:'Waiting for execution slot'})[phase]||'',
  subAgentElapsedLabel:ms=>ms+'ms',subAgentElapsedMilliseconds:item=>item.elapsedMs,
  renderSubAgentDetails:()=>bodyRedraws++,
};
vm.createContext(c);vm.runInContext(source,c);
c.lastStatusSnapshot={viewRoster:{sessionId:'session-a',agents:[{...agent,executionPhase:'waiting_background'}]}};
c.updateSubAgentDetailExecutionPhase();
assert.equal(phaseEl.textContent,'Waiting for background task');
assert.equal(phaseEl.hidden,false);
assert.equal(subtitle.textContent,'Running · Waiting for background task · 90ms');
c.lastStatusSnapshot.viewRoster.agents[0].executionPhase='waiting_children';
c.updateSubAgentDetailExecutionPhase();
assert.equal(subtitle.textContent,'Running · Waiting for child agents · 90ms');
assert.equal(bodyRedraws,0,'status polling changes only the phase text');
c.lastStatusSnapshot=null;c.updateSubAgentDetailExecutionPhase();
assert.equal(subtitle.textContent,'Running · Executing · 90ms','detail stream remains a fallback');
agent.status='completed';c.updateSubAgentDetailExecutionPhase();
assert.equal(phaseEl.hidden,true,'terminal status hides stale execution phase');
assert.equal(subtitle.textContent,'Completed · 90ms');
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewBufferString(js[start:end])
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sub-agent detail execution phase UI: %v\n%s", err, output)
	}
}
