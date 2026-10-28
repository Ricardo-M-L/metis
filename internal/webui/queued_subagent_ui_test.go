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
  context.handleSubagentLifecycle({...child, status:'running', executionPhase:'executing'});
  assert.equal(stage.dataset.state, 'running');
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Running · Executing');
  context.reconcileSubagentToolRows({sessionId:'A',agents:[{...child,status:'running',executionPhase:'waiting_background'}]});
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Running · Waiting for background task');
  context.reconcileSubagentToolRows({sessionId:'A',agents:[{...child,status:'running',executionPhase:'waiting_children'}]});
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Running · Waiting for child agents');
  context.reconcileSubagentToolRows({sessionId:'A',agents:[{...child,status:'running',executionPhase:'waiting_slot'}]});
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Running · Waiting for execution slot');
  context.lang = 'zh';context.refreshActivityGroupLanguage();
  assert.equal(row.querySelector('.tc-subagent-state').textContent, '运行中 · 等待执行槽');
  context.lang = 'en';context.refreshActivityGroupLanguage();
  context.attachSubagentIdentity(row, child, 'queued');
  assert.equal(row.dataset.subagentStatus, 'running', 'late queued launch receipt cannot regress a started child');
  context.handleSubagentLifecycle({...child, status:'killed'});
  assert.equal(row.querySelector('.tc-subagent-state').textContent, 'Stopped');
  assert.equal(row.dataset.subagentExecutionPhase, undefined, 'terminal lifecycle clears stale execution phase');
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
	runSubagentConversationUI(t, "queued_cancel")
}
