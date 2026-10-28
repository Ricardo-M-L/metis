package webui

import "testing"

func TestTraceBrowserToolOrderAndPairing(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const c={console};vm.createContext(c);vm.runInContext(source,c);
const build=events=>c.buildTraceRows(events);
const event=(kind,turn,extra={})=>({kind,turn,toolName:'Read',...extra});

// The screenshot's old completed ToolSearch must occupy its original slot once.
{
  const rows=build([
    event('tool_args',30,{toolName:'ToolSearch',toolUseID:'old',text:'{"query":"select:Read"}',sequence:1359}),
    event('tool_result',30,{toolName:'ToolSearch',toolUseID:'old',text:'loaded',sequence:1361}),
    {kind:'user',turn:49,text:'new question',sequence:1852},
    {kind:'text',turn:49,text:'new answer',sequence:1858},
  ]);
  assert.equal(rows.filter(r=>r.toolName==='ToolSearch').length,1,'completed args must not become a second pending call');
  assert.equal(rows[0].turn,30);assert.equal(rows.at(-1).turn,49);
  assert.equal(rows[0].resultText,'loaded');assert.equal(c.rowState(rows[0]),'complete');
}

// A provider ID can repeat across turns, invocations, and simultaneous calls.
for(const ownership of [
  [{turn:30},{turn:49}],
  [{turn:1,traceInvocationID:'parent'},{turn:1,traceInvocationID:'child'}],
  [{turn:1,traceInvocationID:'same',traceCallID:'a'},{turn:1,traceInvocationID:'same',traceCallID:'b'}],
]){
  const [a,b]=ownership;
  const rows=build([
    event('tool_start',a.turn,{...a,toolUseID:'reused',text:'first'}),
    event('tool_start',b.turn,{...b,toolUseID:'reused',text:'second'}),
    event('tool_result',a.turn,{...a,toolUseID:'reused',text:'first result'}),
    event('tool_result',b.turn,{...b,toolUseID:'reused',text:'second result'}),
  ]);
  assert.equal(rows.length,2);
  assert.equal(rows[0].resultText,'first result');assert.equal(rows[1].resultText,'second result');
}

// An unmatched result in a newer turn must not complete an older same-name tool.
{
  const rows=build([event('tool_start',30,{toolUseID:'old'}),event('tool_result',49,{toolUseID:'new',text:'new result'})]);
  assert.equal(rows.length,2);assert.equal(c.rowState(rows[0]),'running');assert.equal(c.rowState(rows[1]),'complete');
}

// Empty output is completion evidence, and cannot absorb a later legacy result.
{
  const rows=build([
    event('tool_start',1,{toolUseID:'a'}),event('tool_result',1,{toolUseID:'a',text:''}),
    event('tool_start',1,{toolUseID:'b'}),event('tool_result',1,{text:'second result'}),
  ]);
  assert.equal(rows.length,2);assert.equal(c.rowState(rows[0]),'complete');
  assert.equal(rows[0].resultText,'');assert.equal(rows[1].resultText,'second result');
}

// Ambiguous name-only results stay visible rather than choosing an arbitrary call.
{
  const rows=build([event('tool_start',1,{toolUseID:'a'}),event('tool_start',1,{toolUseID:'b'}),event('tool_result',1,{text:'unattributed'})]);
  assert.equal(rows.length,3);assert.equal(c.rowState(rows[0]),'running');assert.equal(c.rowState(rows[1]),'running');
}

// Recovering a tool call must not duplicate or remove the compaction disclosure.
{
  const rows=build([
    event('tool_args',30,{toolUseID:'old',text:'{}'}),
    {kind:'compaction_start',turn:30,text:'auto'},
    {kind:'compaction_progress',turn:30,text:'summarizing'},
    {kind:'context_compacted',turn:30,text:'auto'},
    {kind:'compaction_end',turn:30,text:'auto'},
    event('tool_result',30,{toolUseID:'old',text:'done'}),
    {kind:'user',turn:49,text:'new turn'},
  ]);
  assert.equal(rows.filter(r=>r.kind==='context_compacted').length,1);
  assert.equal(rows.filter(r=>r.toolUseID==='old').length,1);
  assert.equal(c.rowState(rows.find(r=>r.toolUseID==='old')),'complete');
  assert.equal(rows.at(-1).turn,49);
}
`)
}
