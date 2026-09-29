package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestQueuedSubagentToolRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const scenarios = `if (scenario === 'queued_lifecycle') {
  context.handleToolStart({tool:'Agent', id:'queued-parent', session:'A', input:JSON.stringify({description:'Review code'})});
  flushFrames();
  const row = area.querySelector('.call-row');
  const stage = area.querySelector('.subagent-stage');
  const child = {sessionId:'A', parentToolUseId:'queued-parent', agentId:'queued-child', status:'queued'};
  context.handleSubagentLifecycle(child, 'running');
  assert.equal(row.dataset.subagentStatus, 'queued', 'payload status must override the event default');
  assert.equal(stage.dataset.state, 'queued');
  assert.match(stage.textContent, /子代理排队中/);
  assert.doesNotMatch(stage.textContent, /正在协调|已协调|已启动/);
  assert.equal(row.querySelector('.tc-subagent-state').textContent, '排队中');
  assert.match(row.querySelector('.tc-disclosure').getAttribute('aria-label'), /排队中/);
  row.querySelector('.tc-open-agent').dispatch('click');
  assert.equal(context.openedAgents.at(-1).id, 'queued-child', 'queued identity opens the details');
  context.lang = 'en'; context.refreshActivityGroupLanguage();
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Queued');
  assert.match(stage.textContent, /Sub-agents queued/);
  assert.doesNotMatch(stage.textContent, /子代理|排队中/);
  context.handleSubagentLifecycle({...child, status:'running'});
  assert.equal(stage.dataset.state, 'running');
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Running');
  context.attachSubagentIdentity(row, child, 'queued');
  assert.equal(row.dataset.subagentStatus, 'running', 'late queued launch receipt cannot regress a started child');
  context.handleSubagentLifecycle({...child, status:'killed'});
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Stopped');
  assert.notEqual(stage.dataset.state, 'completed');
  context.handleSubagentLifecycle({...child, status:'running'});
  assert.equal(row.dataset.subagentStatus, 'killed', 'late start cannot revive a terminal child');
} else if (scenario === 'queued_background_history') {
  const history = [
    {role:'assistant', content:[{type:'tool_use', tool_use_id:'queued-parent', name:'Agent', input:JSON.stringify({description:'Review later', run_in_background:true})}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'queued-parent', content:'Agent queued', presentation:{'metis.agent_started':true, subagent:{sessionId:'A', parentToolUseId:'queued-parent', agentId:'queued-child', background:true, status:'queued'}}}]},
  ];
  let resolveDetail;
  context.fetch = url => url.startsWith('/api/subagents/') ? new Promise(resolve => {resolveDetail = resolve;}) : Promise.resolve({ok:false});
  context.renderHistoryMessages(history); flushFrames();
  const row = area.querySelector('.call-row');
  const stage = area.querySelector('.subagent-stage');
  assert.equal(row.dataset.state, 'ok', 'launch acknowledgement is separate from child status');
  assert.equal(stage.dataset.state, 'queued', 'launch acknowledgement must not finish queued work');
  assert.equal(row.querySelector('.tc-subagent-state').textContent, '排队中');
  assert.equal(typeof resolveDetail, 'function', 'history refresh must query queued children');
  resolveDetail({ok:true,json:async()=>({agent:{agentId:'queued-child',sessionId:'A',status:'running'}})});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(row.dataset.subagentStatus, 'running');
  assert.equal(row.querySelector('.tc-subagent-state').textContent, '运行中');
  context.handleToolStart({tool:'Agent', id:'second-parent', session:'A', input:'{}'}); flushFrames();
  context.handleSubagentLifecycle({sessionId:'A',parentToolUseId:'second-parent',agentId:'second-child',status:'queued'});
  context.handleToolStart({tool:'Agent', id:'third-parent', session:'A', input:'{}'}); flushFrames();
  context.handleSubagentLifecycle({sessionId:'A',parentToolUseId:'third-parent',agentId:'third-child',status:'running'});
  assert.match(area.querySelectorAll('.subagent-stage').at(-1).textContent, /1 个排队中/, 'mixed running and queued agents retain the queue count');
  context.renderHistoryMessages(history); flushFrames();
  const queuedRow=area.querySelector('.call-row');
  context.handleSubagentLifecycle({sessionId:'A',parentToolUseId:'queued-parent',agentId:'queued-child',status:'running'});
  resolveDetail({ok:true,json:async()=>({agent:{agentId:'queued-child',sessionId:'A',status:'queued'}})});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(queuedRow.dataset.subagentStatus,'running','stale history response cannot undo a live start');
} else if (scenario === 'roster_terminal_reconciliation') {
  const agents=[];
  for(let i=0;i<12;i++) {
    const parent='parent-'+i;
    const agent={sessionId:'A',parentToolUseId:parent,agentId:'child-'+i,background:true,status:i<8?'running':'queued'};
    agents.push(agent);
    context.handleToolStart({tool:'Agent',id:parent,session:'A',input:JSON.stringify({description:'Review '+i,run_in_background:true})});
    context.handleToolResult({tool:'Agent',id:parent,output:'Original launch receipt '+agent.status,presentation:{subagent:agent}});
  }
  flushFrames();
  const rows=area.querySelectorAll('.call-row');
  const stage=area.querySelector('.subagent-stage');
  assert.match(stage.textContent,/已启动后台子代理.*12 个.*4 个排队中/);
  inspectRow(rows.at(-1));
  assert.match(details.detailsBody.innerHTML,/排队中/,'Inspect reads child status rather than completed handshake');
  const originalOutput=context.toolDetails[rowKey(rows.at(-1))].output;
  context.finishUserTurn('stopped');context.beginUserTurn();
  const terminal=agents.map(agent=>({...agent,status:'killed'}));
  context.reconcileSubagentToolRows({sessionId:'B',agents:terminal});
  assert.equal(rows[0].dataset.subagentStatus,'running','foreign roster cannot stop this session');
  context.reconcileSubagentToolRows({sessionId:'A',agents:terminal.map(agent=>({...agent,sessionId:'B'}))});
  assert.equal(rows[0].dataset.subagentStatus,'running','foreign child cannot stop this session');
  context.reconcileSubagentToolRows({sessionId:'A',agents:[]});
  assert.equal(rows[0].dataset.subagentStatus,'running','missing sampled child is not proof of termination');
  context.reconcileSubagentToolRows({sessionId:'A',agents:terminal});
  for(const row of rows) {
    assert.equal(row.dataset.subagentStatus,'killed','roster reconciles prior-turn live rows after missing SSE');
    assert.equal(row.querySelector('.tc-subagent-state').textContent,'已停止');
    assert.equal(row.dataset.state,'ok','original tool handshake remains an immutable successful result');
  }
  assert.equal(stage.dataset.state,'stopped','user cancellation must not be rendered as an error');
  assert.match(stage.textContent,/子代理已停止.*12 个.*12 个已停止/);
  assert.doesNotMatch(stage.textContent,/已启动|排队中|失败/);
  assert.match(details.detailsBody.innerHTML,/已停止/,'open inspector follows current child state');
  assert.equal(context.toolDetails[rowKey(rows.at(-1))].output,originalOutput,'terminal refresh does not rewrite the historical output');
  context.reconcileSubagentToolRows({sessionId:'A',agents});
  assert.equal(rows[0].dataset.subagentStatus,'killed','late active snapshot cannot revive stopped work');
  context.lang='en';context.refreshActivityGroupLanguage();
  assert.match(stage.textContent,/Sub-agents stopped.*12 agents.*12 stopped/);
  assert.doesNotMatch(stage.textContent,/failed|queued|子代理/);
  const history=[
    {role:'assistant',content:[{type:'tool_use',tool_use_id:'saved-parent',name:'Agent',input:{description:'Saved background',run_in_background:true}}]},
    {role:'user',content:[{type:'tool_result',tool_use_id:'saved-parent',content:'Original queued receipt',presentation:{subagent:{sessionId:'A',parentToolUseId:'saved-parent',agentId:'saved-child',background:true,status:'queued'}}}]},
  ];
  context.lastStatusSnapshot={viewRoster:{sessionId:'A',agents:[{sessionId:'A',agentId:'saved-child',status:'killed'}]}};
  context.statusRosterForSelectedSession=snapshot=>snapshot.viewRoster;
  context.renderHistoryMessages(history);flushFrames();
  assert.equal(area.querySelector('.call-row').dataset.subagentStatus,'killed','history replay immediately consumes the known terminal roster');
  assert.match(area.querySelector('.subagent-stage').textContent,/Sub-agents stopped/);
} else if (scenario === 'keyboard_disclosure') {`
	harness := strings.Replace(toolRowBrowserScript, "if (scenario === 'keyboard_disclosure') {", scenarios, 1)
	for _, scenario := range []string{"queued_lifecycle", "queued_background_history", "roster_terminal_reconciliation"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(node, "-e", harness, scenario)
			cmd.Stdin = bytes.NewReader(source)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("queued tool-row interaction: %v\n%s", err, out)
			}
		})
	}
}

func TestQueuedSubagentDetailsAndCancellation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(source)
	start := strings.Index(js, "function renderStatusSnapshot(d)")
	end := strings.Index(js, "// ============================================================\n// Layout")
	if start < 0 || end <= start {
		t.Fatal("cannot locate sub-agent detail renderer")
	}
	const harness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
class Element {
  constructor(){ this.style={display:'none'}; this.dataset={}; this.attrs={}; this.hidden=true; this.textContent=''; this.children=[]; this.listeners={}; this.classList={add(){},remove(){},toggle(){}}; }
  set innerHTML(value){
    this.html=value; this.children=[]; this.renderCount=(this.renderCount||0)+1;
    for (const match of value.matchAll(/<button[^>]*data-subagent-id="([^"]+)"[^>]*data-subagent-session-id="([^"]+)"[^>]*>/g)) {
      const row=new Element(); row.dataset={subagentId:match[1],subagentSessionId:match[2]}; this.children.push(row);
    }
    const elapsed=/<dd id="subAgentDetailElapsed">([^<]*)<\/dd>/.exec(value);
    if (elapsed) {const value=new Element(); value.textContent=elapsed[1]; byId.set('subAgentDetailElapsed',value); this.children.push(value);}
  }
  get innerHTML(){return this.html||'';}
  setAttribute(k,v){this.attrs[k]=String(v);}
  appendChild(v){this.children.push(v);return v;}
  append(...vs){this.children.push(...vs);}
  addEventListener(k,fn){(this.listeners[k] ||= []).push(fn);}
  click(){if (!this.disabled) for (const fn of this.listeners.click||[]) fn({target:this});}
  querySelector(){return this.dialog ||= new Element();}
  querySelectorAll(selector){return selector==='[data-subagent-id]' ? this.children.filter(v=>v.dataset.subagentId) : [];}
  focus(){c.document.activeElement=this;}
}
const byId = new Map(['statusPopover','statusChip','subAgentDetailOverlay','subAgentDetailTitle','subAgentDetailDescription','subAgentDetailBody'].map(id=>[id,new Element()]));
let now=Date.parse('2026-09-29T08:00:00Z');
const timers=new Map();let nextTimer=0;
const detail = {agentId:'child',sessionId:'A',name:'reviewer',status:'queued',background:true,startedAt:new Date(now-90000).toISOString(),elapsedMs:105,output:''};
const calls=[];
const reconciledRosters=[];
let stopResponse = async()=>({ok:true,json:async()=>({agent:{...detail,status:'killed'}})});
const c={
  console,Promise,Map,Set,encodeURIComponent,requestAnimationFrame:fn=>fn(),currentSessionId:'A',lastStatusSnapshot:null,
  Date:{parse:Date.parse,now:()=>now},setInterval:(fn,ms)=>{assert.equal(ms,1000);timers.set(++nextTimer,fn);return nextTimer;},clearInterval:id=>timers.delete(id),
  reconcileSubagentToolRows:roster=>reconciledRosters.push(roster),
  DESKTOP_I18N:{'zh-CN':{subAgents:'子代理',backgroundTasks:'后台任务'},en:{subAgents:'sub-agents',backgroundTasks:'background tasks'}},
  subAgentDetailState:{agentId:'',ownerSessionId:'',data:null,loading:false,error:'',requestGeneration:0},subAgentDetailStream:null,subAgentDetailStreamGeneration:0,
  document:{documentElement:{lang:'zh-CN'},body:new Element(),getElementById:id=>byId.get(id)||null,createElement:()=>new Element(),addEventListener(){}},
  uiText:(en,zh)=>c.document.documentElement.lang==='en'?en:zh,escHtml:v=>String(v),escAttr:v=>String(v),
  fetch:async(url,options)=>{calls.push({url,options});return options?.method==='POST'?stopResponse():{ok:true,json:async()=>({agent:{...detail}})};},
};
c.window=c;vm.createContext(c);vm.runInContext(source,c);
c.pollStatus=()=>{};
const body=byId.get('subAgentDetailBody');
const text=node=>node.textContent+' '+node.innerHTML+' '+node.children.map(text).join(' ');
const stopButton=()=>body.children.find(node=>node.className==='subagent-detail-stop');
const tick=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
  const snapshot={viewRoster:{sessionId:'A',subAgents:1,backgroundTasks:0,agents:[{...detail}],jobs:[]}};
  c.lastStatusSnapshot=snapshot;c.renderStatusSnapshot(snapshot);c.renderStatusPopover();
  assert.equal(reconciledRosters.at(-1),snapshot.viewRoster,'status polling forwards the selected roster to tool rows');
  assert.equal(byId.get('statusChip').style.display,'');
  assert.match(byId.get('statusChip').innerHTML,/1 子代理/);
  const pop=byId.get('statusPopover');
  assert.match(pop.innerHTML,/排队中/);assert.doesNotMatch(pop.innerHTML,/运行中|已完成/);
  pop.querySelectorAll('[data-subagent-id]')[0].click();await tick();
  assert.equal(byId.get('subAgentDetailOverlay').hidden,false,'queued row opens its detail');
  assert.match(byId.get('subAgentDetailDescription').textContent,/排队中/);
  assert.match(text(body),/尚未开始执行/);assert.match(text(body),/已等待/);
  assert.doesNotMatch(text(body),/已运行|没有产生文字输出|may still be/);
  assert.equal(stopButton().textContent,'取消排队');
  assert.match(byId.get('subAgentDetailDescription').textContent,/1分30秒/,'active elapsed derives from startedAt instead of stale elapsedMs');
  assert.equal(byId.get('subAgentDetailElapsed').textContent,'1分30秒');
  assert.equal(timers.size,1);
  const buttonBeforeTick=stopButton();buttonBeforeTick.focus();
  const rendersBeforeTick=body.renderCount;
  now+=1000;for(const fn of timers.values())fn();
  assert.equal(byId.get('subAgentDetailElapsed').textContent,'1分31秒');
  assert.equal(body.renderCount,rendersBeforeTick,'elapsed clock must not redraw the detail body');
  assert.equal(stopButton(),buttonBeforeTick);assert.equal(c.document.activeElement,buttonBeforeTick,'timer preserves keyboard focus');
  c.document.documentElement.lang='en';c.renderStatusPopover();c.renderSubAgentDetails();
  assert.match(pop.innerHTML,/Queued/);assert.doesNotMatch(pop.innerHTML,/排队中|查看详情/);
  assert.match(text(body),/has not started yet/);assert.doesNotMatch(text(body),/排队|子代理|已等待/);
  assert.equal(stopButton().textContent,'Cancel queued agent');
  assert.match(byId.get('subAgentDetailDescription').textContent,/1m 31s/);assert.equal(timers.size,1,'language rerender cannot create duplicate timers');
  let releaseStop;
  stopResponse=()=>new Promise(resolve=>{releaseStop=resolve;});
  stopButton().click();
  assert.equal(stopButton().disabled,true,'pending request prevents duplicate cancellation');
  const post=calls.at(-1);assert.equal(post.url,'/api/subagents/child/stop?sessionId=A');assert.equal(post.options.method,'POST');
  assert.equal(c.subAgentDetailState.data.status,'queued','cancel must not fabricate terminal success');
  stopButton().click();assert.equal(calls.at(-1),post);
  releaseStop({ok:true,json:async()=>({agent:{...detail,status:'killed'}})});await tick();
  assert.match(byId.get('subAgentDetailDescription').textContent,/Stopped/);assert.equal(stopButton(),undefined);
  assert.equal(timers.size,0,'terminal status stops the clock');
  assert.equal(byId.get('subAgentDetailElapsed').textContent,'0s','terminal duration remains the server elapsedMs');

  c.subAgentDetailState.data={...detail,status:'killed',exitError:'context canceled',stopHint:'cancelled',elapsedMs:92000};
  c.document.documentElement.lang='zh-CN';c.renderSubAgentDetails();
  assert.match(text(body),/已取消/);assert.match(text(body),/用时/);assert.match(text(body),/1分32秒/);
  assert.doesNotMatch(text(body),/已运行|错误|context canceled|status-dot killed/);
  assert.equal(body.children.some(node=>node.className==='subagent-detail-error'),false);
  c.document.documentElement.lang='en';

  c.subAgentDetailState.data={...detail};c.renderSubAgentDetails();
  stopResponse=async()=>({ok:false,json:async()=>({error:'cancel unavailable'})});
  stopButton().click();await tick();
  assert.equal(c.subAgentDetailState.data.status,'queued');assert.equal(stopButton().disabled,false);
  assert.match(text(body),/Unable to stop this sub-agent/);

  const streams=[];
  c.EventSource=class {constructor(url){this.url=url;this.listeners={};this.closed=false;streams.push(this);}addEventListener(name,fn){this.listeners[name]=fn;}close(){this.closed=true;}emit(name,payload){this.listeners[name]({data:JSON.stringify(payload)});}};
  c.startSubAgentDetailStream('child');const stream=streams.at(-1);
  stream.emit('snapshot',{agent:{...detail}});assert.equal(stream.closed,false);
  stopResponse=()=>new Promise(resolve=>{releaseStop=resolve;});stopButton().click();
  stream.emit('snapshot',{agent:{...detail,status:'running'}});
  releaseStop({ok:true,json:async()=>({agent:{...detail,status:'queued'}})});await tick();
  assert.equal(stopButton().textContent,'Stop sub-agent');assert.match(text(body),/may still be inspecting/);
  assert.equal(c.subAgentDetailState.data.status,'running','late stop response cannot regress a newer SSE snapshot');
  now+=1000;for(const fn of timers.values())fn();
  assert.match(byId.get('subAgentDetailDescription').textContent,/Running · 1m 32s/,'running shares the local startedAt clock');
  assert.doesNotMatch(text(body),/has not started yet/);

  c.subAgentDetailState.data={...detail};c.renderSubAgentDetails();
  stopResponse=()=>new Promise(resolve=>{releaseStop=resolve;});stopButton().click();
  c.closeSubAgentDetails(false);c.currentSessionId='B';
  assert.equal(timers.size,0,'closing detail clears the clock');
  c.subAgentDetailState={agentId:'other',ownerSessionId:'B',data:{agentId:'other',sessionId:'B',status:'queued'},stopping:false,stopGeneration:99};
  releaseStop({ok:true,json:async()=>({agent:{...detail,status:'killed'}})});await tick();
  assert.equal(c.subAgentDetailState.agentId,'other','old cancellation cannot update a new dialog');
  assert.equal(c.subAgentDetailState.data.status,'queued');
  c.currentSessionId='A';c.openSubAgentDetails('child',null,'A');
  streams.at(-1).emit('snapshot',{agent:{...detail}});assert.equal(timers.size,1);
  c.currentSessionId='B';c.renderStatusSnapshot(snapshot);assert.equal(timers.size,0,'session switch clears the clock');
  c.currentSessionId='A';c.openSubAgentDetails('child',null,'A');
  streams.at(-1).emit('snapshot',{agent:{...detail}});assert.equal(timers.size,1);
  c.openSubAgentDetails('other',null,'A');assert.equal(timers.size,0,'switching agents clears the old clock while loading');
  c.closeSubAgentDetails(false);
  assert.equal(c.subAgentElapsedMilliseconds({...detail,startedAt:'invalid'}),105,'bad timestamps use server duration');
  assert.equal(c.subAgentElapsedMilliseconds({...detail,startedAt:new Date(now+1000).toISOString()}),0,'future clocks clamp to zero');
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(js[start:end], "setInterval(pollStatus, 3000);\npollStatus();", ""))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued detail interaction: %v\n%s", err, out)
	}
}
