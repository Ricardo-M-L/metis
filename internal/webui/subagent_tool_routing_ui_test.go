package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Child tools run concurrently with the parent's text stream. Reuse the tool
// browser fixture, but exercise the real streaming renderer as well: closing a
// parent's message at a child tool boundary used to split even a single word.
func TestSubagentToolsDoNotSplitParentTranscript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	fixture, _, ok := strings.Cut(toolRowBrowserScript, "async function runScenario() {")
	if !ok {
		t.Fatal("missing tool browser fixture boundary")
	}
	for _, scenario := range []string{"args", "start", "result", "mixed_history", "reused_tool_id", "late_background"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(node, "-e", fixture+subagentToolRoutingScript, scenario)
			cmd.Stdin = bytes.NewReader(source)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("subagent tool routing: %v\n%s", err, output)
			}
		})
	}
}

const subagentToolRoutingScript = `
let attachedActions = 0;
Object.assign(context, {
  streamingEl: null, streamingText: '', streamMsgIdx: -1, streamedTextThisTurn: false,
  turnFirstTokenMs: 0, runningTurnIncompleteReason: '', currentView: 'chat', updateSendBtn() {}, loadSessions() {},
  loadSessionStatsbar() {}, attachMessageActions() { attachedActions++; },
});
vm.runInContext(extract('function handleTextDelta(d)', 'const foregroundRequests ='), context);
vm.runInContext(extract('function startStreamingMessage()', '// A provider turn_end'), context);

const assistantRows = () => area.querySelectorAll('.message-assistant');
const assistantText = () => assistantRows().map(row => {
  const content = row.querySelector('.message-content');
  return content.innerHTML || content.textContent;
});
const parentInput = JSON.stringify({name:'review', prompt:'Inspect the source'});
const parentPresentation = {'metis.agent_started':true, subagent:{
  sessionId:'A', parentToolUseId:'parent-agent', agentId:'child-agent',
  name:'review', background:true, status:'running',
}};
context.handleToolStart({tool:'Agent',id:'parent-agent',traceCallId:'parent-call',input:parentInput});
context.handleToolResult({tool:'Agent',id:'parent-agent',traceCallId:'parent-call',
  output:'Agent started',presentation:parentPresentation});
const agentRow = area.querySelector('.call-row');
assert.equal(agentRow.dataset.subagentId,'child-agent');

const child = {session:'A',subAgentParentId:'parent-agent',tool:'Read',id:'child-read',
  traceCallId:'child-call',input:'{"path":"README.md"}',delta:'{"path":',output:'child output',elapsedMs:1};
const childEvent = kind => {
  if (kind === 'args') context.handleToolArgsDelta(child);
  if (kind === 'start') context.handleToolStart(child);
  if (kind === 'result') context.handleToolResult(child);
};

if (scenario === 'late_background') {
  context.handleTextDelta({delta:'Parent complete.'});
  context.finishUserTurn();
  const originalChildren = [...area.children];
  for (const kind of ['args','start','result']) childEvent(kind);
  assert.ok(area.children.length === originalChildren.length &&
    area.children.every((node,index) => node === originalChildren[index]),
    'late child tools must not reopen a settled parent turn');
  assert.equal(agentRow.dataset.subagentStatus,'running');
  context.handleSubagentLifecycle({sessionId:'A',parentToolUseId:'parent-agent',
    traceCallId:'parent-call',agentId:'child-agent',status:'completed'},'completed');
  assert.equal(agentRow.dataset.subagentStatus,'completed','the owning Agent lifecycle still updates');
} else if (scenario === 'reused_tool_id') {
  context.handleToolArgsDelta({tool:'Read',id:'shared-id',delta:'{"path":'});
  const parentTool = area.querySelectorAll('.call-row').at(-1);
  const parentArgs = parentTool.getAttribute('data-args');
  child.id = 'shared-id'; child.traceCallId = '';
  for (const kind of ['args','start','result']) childEvent(kind);
  assert.equal(parentTool.getAttribute('data-args'),parentArgs,'child args cannot corrupt a same-ID parent preview');
  assert.equal(parentTool.dataset.state,'running','child result cannot complete a same-ID parent tool');
  assert.equal(area.querySelectorAll('.call-row').length,2);
  context.handleToolStart({tool:'Read',id:'shared-id',input:'{"path":"parent.txt"}'});
  context.handleToolResult({tool:'Read',id:'shared-id',output:'parent output'});
  assert.equal(context.toolDetails[parentTool.dataset.rowKey].output,'parent output');
} else {
  context.handleTextDelta({delta:'有个关键'});
  const parentMessage = assistantRows()[0];
  const initialChildren = [...area.children];
  const initialMessages = context.messages.length;
  if (scenario === 'mixed_history') {
    for (const kind of ['args','start','result']) childEvent(kind);
  } else childEvent(scenario);
  assert.equal(context.streaming,true,'a child event must not settle the parent text stream');
  assert.equal(context.streamingEl,parentMessage,'the parent message keeps its stream owner');
  assert.equal(context.messages.length,initialMessages,'child tools cannot commit a partial parent message');
  assert.equal(attachedActions,0,'child tools cannot insert a partial-message timestamp/actions');
  assert.ok(area.children.length === initialChildren.length &&
    area.children.every((node,index) => node === initialChildren[index]),
    'child tools belong outside the parent transcript');
  context.handleTextDelta({delta:'区别，需要分开'});
  child.tool='sub: Glob'; child.id='child-glob'; child.traceCallId='child-call-2';
  for (const kind of ['args','start','result']) childEvent(kind);
  context.handleTextDelta({delta:'评价。'});
  context.endStreamingMessage();
  assert.deepEqual(assistantText(),['有个关键区别，需要分开评价。']);
  assert.equal(attachedActions,1,'only the completed parent message gets actions');
  assert.equal(area.querySelectorAll('.call-row').length,1,'the parent Agent card remains available');
  context.openSubagentFromTool(agentRow.dataset.rowKey,agentRow);
  assert.equal(context.openedAgents[0].id,'child-agent','child details remain reachable');

  if (scenario === 'mixed_history') {
    const liveText=assistantText();
    context.renderHistoryMessages([
      {role:'user',content:[{type:'text',text:'Review'}]},
      {role:'assistant',content:[{type:'tool_use',name:'Agent',tool_use_id:'parent-agent',input:JSON.parse(parentInput)}]},
      {role:'user',content:[{type:'tool_result',tool_use_id:'parent-agent',content:'Agent started',presentation:parentPresentation}]},
      {role:'assistant',content:[{type:'text',text:liveText[0]}]},
    ]);
    assert.deepEqual(assistantText(),liveText,'live and saved parent messages have identical boundaries');
    assert.equal(area.querySelectorAll('.call-row').length,1);
  }
}
`
