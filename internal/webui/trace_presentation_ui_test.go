package webui

import (
	"strings"
	"testing"
)

func TestTracePresentationAssets(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `href="trace.css?v=__BUILD__"`) {
		t.Fatal("trace presentation stylesheet is not loaded")
	}
	css, err := staticFS.ReadFile("static/trace.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{".tt-image-card", "width: 48px", "height: 48px", "@media (max-width:"} {
		if !strings.Contains(string(css), required) {
			t.Fatalf("trace stylesheet is missing %q", required)
		}
	}
}

// These cases exercise the display projection, leaving the original records
// available for inspection rather than making the persisted trace lossy.
func TestTracePresentationSemanticRows(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", tracePresentationHarness+`
const input=[
  event('text','Hello ',{streamID:'response-1'}),
  event('text','world',{streamID:'response-1'}),
  event('thinking','First ',{streamID:'reasoning-1'}),
  event('thinking','second',{streamID:'reasoning-1'}),
];
const before=JSON.stringify(input), rows=c.buildTraceRows(input);
assert.equal(rows.length,2,'streaming fragments become readable semantic entries');
assert.equal(rows[0].text,'Hello world');
assert.equal(rows[1].text,'First second');
assert.equal(rows[0].rawEvents.length,2);
assert.equal(rows[1].rawEvents.length,2);
assert.equal(JSON.stringify(rows[0].rawEvents),JSON.stringify(input.slice(0,2)));
assert.equal(JSON.stringify(input),before,'display aggregation must not mutate saved records');
assert.match(c.inspectorBody(rows[0],'events'),/Hello /,'original fragments remain inspectable');
assert.match(c.inspectorBody(rows[0],'events'),/world/);

// Transport diagnostics are appended independently of the semantic stream.
const diagnostic=event('request_timing','pre_request_start request=req-1 call=0 attempt=0 elapsed_ms=0',{
  requestTiming:{request_id:'req-1',phase:'pre_request_start',provider_call:0,attempt:0,elapsed_ms:0},
});
const withDiagnostic=c.buildTraceRows([input[0],diagnostic,input[1]]);
assert.equal(withDiagnostic.filter(row=>row.kind==='text').length,1);
assert.equal(withDiagnostic.find(row=>row.kind==='text').text,'Hello world');

for(const kind of ['user','tool_start','tool_result','tokens','turn_end','loop_done','error','compaction_start']){
  const result=c.buildTraceRows([
    event('text','before',{streamID:'same-stream'}),
    event(kind,kind==='tokens'?'input=1 output=2 cache_write=0 cache_read=0':'boundary'),
    event('text','after',{streamID:'same-stream'}),
  ]).filter(row=>row.kind==='text');
  assert.equal(result.length,2,kind+' must terminate the semantic entry even when old stream ids repeat');
  assert.equal(result[0].text,'before');assert.equal(result[1].text,'after');
}

for(const changed of [
  {turn:2}, {sessionId:'B'}, {traceInvocationID:'child'}, {traceParentInvocationID:'other-parent'},
  {subAgentOf:'different-agent'}, {parentID:'different-tool'}, {depth:1}, {source:'different-source'},
  {streamID:'response-2'},
]){
  const result=c.buildTraceRows([
    event('text','first',{streamID:'same'}),
    event('text','second',{streamID:'same',...changed}),
  ]).filter(row=>row.kind==='text');
  assert.equal(result.length,2,'immutable ownership/boundaries must not mix '+JSON.stringify(changed));
}
const interleaved=c.buildTraceRows([
  event('text','parent one',{streamID:'parent'}),
  event('text','child one',{streamID:'child',traceInvocationID:'child',subAgentOf:'agent-1',depth:1}),
  event('text','parent two',{streamID:'parent'}),
]);
assert.equal(interleaved.length,3,'concurrent owners preserve their recorded ordering');
assert.equal(interleaved[1].traceInvocationID,'child');

// Pagination may split a response, and the overlapping page edge is deduped.
const combined=c.mergeTraceEvents(input.slice(0,2),input.slice(1));
assert.equal(combined.length,input.length);
const combinedRows=c.buildTraceRows(combined);
assert.equal(combinedRows[0].text,'Hello world');
assert.equal(combinedRows[0].rawEvents.length,2);
assert.equal(JSON.stringify(combined),before);

// Existing argument/result and compaction projections must retain their input
// records too, otherwise the new Raw events tab would silently lose evidence.
const toolRecords=[
  event('tool_args','{"path":',{toolName:'Read',toolUseID:'read-1'}),
  event('tool_args','"file.txt"}',{toolName:'Read',toolUseID:'read-1'}),
  event('tool_start','{}',{toolName:'Read',toolUseID:'read-1'}),
  event('tool_result','contents',{toolName:'Read',toolUseID:'read-1'}),
];
const toolRows=c.buildTraceRows(toolRecords);
assert.equal(toolRows.length,1);
assert.equal(JSON.stringify(toolRows[0].rawEvents),JSON.stringify(toolRecords),
  'tool argument recovery and result pairing keep every original record');
const compactRecords=[
  event('compaction_start','auto'),event('compaction_progress','summarizing'),
  event('context_compacted','auto'),event('compaction_end','auto'),
];
const compactRows=c.buildTraceRows(compactRecords);
assert.equal(compactRows.length,1);
assert.equal(JSON.stringify(compactRows[0].rawEvents),JSON.stringify(compactRecords),
  'collapsed compaction phases remain inspectable');

const redacted=c.buildTraceRows([event('thinking_redacted','opaque-provider-ciphertext')])[0];
for(const [tab] of c.inspectorTabsFor(redacted)){
  assert.doesNotMatch(c.inspectorBody(redacted,tab),/opaque-provider-ciphertext/,
    'provider-redacted reasoning must remain redacted in every accessible tab');
}
`)
}

func TestTracePresentationRequestDiagnostics(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", tracePresentationHarness+`
const timing=(request,phase,call,attempt,elapsed,extra={})=>event('request_timing',phase,{
  requestTiming:{request_id:request,phase,provider_call:call,attempt,elapsed_ms:elapsed,at:'2026-11-09T10:00:00Z',...extra},
});
const records=[
  timing('req-1','pre_request_start',0,0,0),
  timing('req-1','http_do_start',1,1,10),
  timing('req-1','http_first_response_byte',1,1,125),
  timing('req-1','http_do_start',1,2,300),
  timing('req-1','request_complete',1,2,500,{outcome:'ok',status_code:200}),
  timing('req-2','pre_request_start',0,0,0),
];
const before=JSON.stringify(records),rows=c.buildTraceRows(records);
assert.equal(rows.length,2,'many timing phases collapse to one row per request');
assert(rows.every(row=>row.kind==='request_timing'));
assert.equal(rows[0].rawEvents.length,5);
assert.equal(rows[0].requestTimingRecords.length,5);
assert.equal(JSON.stringify(rows[0].requestTimingRecords),JSON.stringify(records.slice(0,5).map(record=>record.requestTiming)));
assert.equal(JSON.stringify(records),before);
const details=c.inspectorBody(rows[0],'timing');
assert.match(details,/http_first_response_byte/,'phase names are readable inside the collapsed diagnostics');
assert.match(details,/attempt/i,'HTTP attempts stay distinct within a provider call');
assert.match(details,/call/i,'provider-call grouping is visible');
assert.match(c.rowText(rows[0]),/req-1/,'diagnostic summary identifies the real request');
assert.match(c.inspectorBody(rows[0],'events'),/request_complete/,'all original diagnostic records are inspectable');

// Identical request ids cannot join different child invocations.
const child={...records[0],id:'child-event',traceInvocationID:'child',subAgentOf:'agent-1',depth:1};
const owned=c.buildTraceRows([records[0],child,records[1]]);
assert.equal(owned.length,2);
assert.equal(owned.find(row=>row.traceInvocationID==='root').rawEvents.length,2);
assert.equal(owned.find(row=>row.traceInvocationID==='child').rawEvents.length,1);

// Legacy strings remain queryable even without structured TimingRecord fields.
const legacy=[
  event('request_timing','http_do_start request=legacy-1 call=1 attempt=1 elapsed_ms=0'),
  event('request_timing','request_complete request=legacy-1 call=1 attempt=1 elapsed_ms=123 outcome=ok status=200'),
];
const recovered=c.buildTraceRows(legacy);
assert.equal(recovered.length,1);
assert.equal(recovered[0].rawEvents.length,2);
assert.match(c.inspectorBody(recovered[0],'events'),/elapsed_ms=123/);
`)
}

func TestTracePresentationImagesAndContext(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", tracePresentationHarness+`
const hash='a'.repeat(64);
const image={mediaType:'image/png',attachmentHash:hash,url:'/api/trace/image?session_id=A&hash='+hash,
  name:'screenshot <script>alert(1)</script>.png',bytes:1234,width:640,height:480};
assert.equal(c.traceImageSource(image),image.url);
for(const url of [
  'https://outside.invalid/image.png', '//outside.invalid/image.png', 'javascript:alert(1)',
  'data:image/png;base64,aW1hZ2U=', '/api/trace/other?session_id=A&hash='+hash,
])assert.equal(c.traceImageSource({...image,url}),'','only the controlled attachment endpoint is a valid source');
const original=event('user','describe the screenshot',{images:[image]});
const row=c.buildTraceRows([original])[0];
assert.equal(JSON.stringify(row.images),JSON.stringify([image]));
const ledger=c.contentHtml(row);
assert.match(ledger,/<img\b/,'user image cards appear directly in the ledger');
assert.match(ledger,/<button\b/,'the thumbnail can open a larger preview');
assert.doesNotMatch(ledger,/<script>/,'attachment names must be escaped');
for(const tab of ['summary','preview']){
  const html=c.inspectorBody(row,tab);
  assert.match(html,/<img\b/,tab+' includes the image card');
  assert.doesNotMatch(html,/<script>/);
}
const missing=c.buildTraceRows([event('user','[image attachment]')])[0];
const unavailable=c.inspectorBody(missing,'summary');
assert.match(unavailable,/image|attachment/i);
assert.match(unavailable,/unavailable|not (?:recorded|available|recover)|missing|无法|不可用|未记录/i,
  'old image-only trace placeholders need an honest availability explanation');

const snapshot={available:true,source:'logical-request',system:'SYSTEM_SNAPSHOT <unsafe>',
  tools:[{name:'Read',description:'Read the chosen file',inputSchema:{type:'object',properties:{path:{type:'string'}}}}],
  capturedAt:'2026-11-09T09:00:00Z',turn:1,requestID:'first-request',model:'fixture-model'};
const input=[event('user','question'),event('text','answer')];
const originalInput=JSON.stringify(input);
const snapshotRows=c.buildTraceRows(input,snapshot);
assert.equal(snapshotRows[0].kind,'initial_context','the captured context precedes the first user input');
assert.equal(snapshotRows[1].kind,'user');
assert.equal(snapshotRows.filter(row=>row.kind==='initial_context').length,1);
assert.equal(JSON.stringify(snapshotRows[0].contextSnapshot),JSON.stringify(snapshot));
const tabs=c.inspectorTabsFor(snapshotRows[0]).map(tab=>tab[0]);
assert(tabs.includes('system')&&tabs.includes('tools'),'system and tool snapshots have separate tabs');
assert.match(c.inspectorBody(snapshotRows[0],'system'),/SYSTEM_SNAPSHOT &lt;unsafe&gt;/);
assert.match(c.inspectorBody(snapshotRows[0],'tools'),/Read the chosen file/);
assert.match(c.inspectorBody(snapshotRows[0],'tools'),/inputSchema|properties|path/);
assert.equal(JSON.stringify(input),originalInput,'snapshot display leaves original history untouched');

const later=c.buildTraceRows(input,{...snapshot,turn:7})[0];
const laterSummary=c.inspectorBody(later,'summary')+' '+c.rowText(later);
assert.match(laterSummary,/7/,'a snapshot first captured in a later turn identifies that turn');
assert.match(laterSummary,/recorded|captured|已记录|捕获/i,'later capture is not presented as the original initial request');
const absent=c.buildTraceRows(input,{available:false,source:'unavailable',limitation:'No recorded request snapshot'})[0];
assert.equal(absent.kind,'initial_context');
assert.match(c.inspectorBody(absent,'summary'),/not (?:captured|recorded|available)|unavailable|No recorded|未记录|不可用/i);
assert.doesNotMatch(c.inspectorBody(absent,'system'),/SYSTEM_SNAPSHOT/,
  'missing historical snapshots cannot borrow current or previous session configuration');

const limitedRecords=[
  event('text','recorded prefix ',{streamID:'limited-stream'}),
  event('text','retained tail',{streamID:'limited-stream',truncated:true,limitation:'fixture cap'}),
];
const limitedRows=c.buildTraceRows(limitedRecords);
assert.equal(limitedRows.length,1);
const limitedStream=limitedRows[0];
assert.equal(limitedStream.text,'recorded prefix retained tail');
assert.equal(limitedStream.truncated,true,'a later truncated fragment marks the entire semantic capture incomplete');
assert.equal(limitedStream.limitation,'fixture cap');
assert.equal(limitedStream.rawEvents.length,2);
for(const [tab] of c.inspectorTabsFor(limitedStream)){
  const html=c.inspectorBody(limitedStream,tab);
  assert.match(html,/Incomplete capture/i,tab+' must keep the incomplete-record notice visible');
  assert.match(html,/fixture cap/,tab+' retains the limitation from the later fragment');
}
const limitedSnapshot=c.buildTraceRows(input,{...snapshot,truncated:true,limitation:'SNAPSHOT_CAPTURE_LIMIT'})[0];
for(const [tab] of c.inspectorTabsFor(limitedSnapshot)){
  const html=c.inspectorBody(limitedSnapshot,tab);
  assert.match(html,/Incomplete capture/i,tab+' must not imply a complete historical request');
  assert.match(html,/SNAPSHOT_CAPTURE_LIMIT/,tab+' retains the snapshot limitation');
}
`)
}

func TestTracePresentationTimelineAndRequestNumbers(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", tracePresentationHarness+`
const nodes={};
const node=()=>({style:{},innerHTML:'',classList:{toggle(){},remove(){},add(){}}});
c.document.getElementById=id=>nodes[id]||(nodes[id]=node());
c.document.querySelectorAll=()=>[];
const business=[
  event('user','first question',{turn:4,ts:'2026-11-09T10:00:00Z'}),
  event('text','first answer',{turn:4,ts:'2026-11-09T10:00:05Z'}),
  event('tokens','input=10 output=3 cache_write=0 cache_read=0',{turn:4,ts:'2026-11-09T10:00:10Z'}),
  event('user','next question',{turn:5,ts:'2026-11-09T10:00:12Z'}),
  event('text','next answer',{turn:5,ts:'2026-11-09T10:00:20Z'}),
];
const diagnostic=(phase,ts)=>event('request_timing',phase,{turn:4,ts,
  requestTiming:{request_id:'timing-1',phase,provider_call:1,attempt:1,elapsed_ms:0}});
const early=diagnostic('pre_request_start','2026-11-09T09:00:00Z');
const late=diagnostic('request_complete','2026-11-09T11:00:00Z');
const snapshot={available:true,source:'logical-request',system:'captured system',tools:[],
  turn:1,capturedAt:'2026-11-09T08:00:00Z'};
function setRows(events,context=null){
  c.input=events;c.snapshot=context;
  vm.runInContext('traceRows=buildTraceRows(input,snapshot);traceSelectedIdx=-1;traceSelectedReq=-1;',c);
}
function timeline(events,context,duration){
  setRows(events,context);c.duration=duration;
  vm.runInContext('traceDurMode=duration;',c);c.renderTimeline();
  // Click targets follow display indices; compare the visual spans themselves.
  return nodes.traceTrack.innerHTML.replace(/selectTraceRow\(\d+, true\)/g,'selectTraceRow(ROW, true)');
}
for(const duration of [false,true]){
  const baseline=timeline(business,null,duration);
  assert.equal(timeline([early,...business,late],snapshot,duration),baseline,
    'diagnostic occurrence clocks and historical snapshots must not change timeline geometry');
}
setRows(business,snapshot);c.renderTrace();
const ledgerRows=nodes.traceBody.innerHTML.match(/<tr\b[\s\S]*?<\/tr>/g)||[];
assert.match(ledgerRows[0],/data-kind="initial_context"/);
assert.doesNotMatch(ledgerRows[0],/tt-req-dot|Request #/,'the context snapshot is not a model request');
const inputs=ledgerRows.filter(row=>row.includes('data-kind="user"'));
assert.equal(inputs.length,2);
assert.match(inputs[0],/Request #1/,'the first visible user turn retains Request #1');
assert.match(inputs[1],/Request #2/);
`)
}

func TestTracePresentationImagePreviewInteraction(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", tracePresentationHarness+`
class Element {
  constructor(tag='div'){
    this.tagName=tag;this.dataset={};this.attributes={};this.handlers={};this.children=[];
    this.style={};this.hidden=false;this.isConnected=false;this.parentElement=null;this.focusCount=0;
    this.classList={add(){},remove(){},toggle(){}};
  }
  setAttribute(name,value){this.attributes[name]=String(value);}
  getAttribute(name){return this.attributes[name]??null;}
  addEventListener(name,callback){(this.handlers[name]||(this.handlers[name]=[])).push(callback);}
  dispatch(name,extra={}){
    const event={target:this,preventDefault(){this.prevented=true;},stopPropagation(){this.stopped=true;},...extra};
    for(const handler of this.handlers[name]||[])handler(event);
    return event;
  }
  appendChild(child){this.children.push(child);child.parentElement=this;child.isConnected=true;return child;}
  remove(){this.removed=true;this.isConnected=false;if(this.parentElement)this.parentElement.children=this.parentElement.children.filter(child=>child!==this);}
  focus(){this.focusCount++;c.document.activeElement=this;}
  querySelector(selector){
    if(selector==='img')return this.previewImage;
    if(selector==='button')return this.closeButton;
    if(selector==='.tt-image-limitation')return this.children.find(child=>child.className==='tt-image-limitation')||null;
    return null;
  }
  set innerHTML(value){
    this._html=String(value);
    if(this._html.includes('tt-image-dialog')){
      this.previewImage=new Element('img');this.closeButton=new Element('button');
      this.previewImage.parentElement=new Element('div');
    }
  }
  get innerHTML(){return this._html||'';}
}
const body=new Element('body'),nodes={};
c.document.body=body;c.document.createElement=tag=>new Element(tag);
c.document.getElementById=id=>nodes[id]||(nodes[id]=new Element());
c.document.querySelector=()=>new Element();c.document.querySelectorAll=()=>[];
c.updateEmptyLayout=()=>{};
const hash='b'.repeat(64),imageURL='/api/trace/image?session_id=A&hash='+hash;
const trigger=new Element('button');trigger.isConnected=true;
trigger.dataset={traceImageSrc:imageURL,traceImageName:'screen <script>unsafe</script>'};
const overlay=()=>body.children.find(node=>node.className==='tt-image-overlay');
function open(){c.openTraceImagePreview(trigger);const value=overlay();assert(value,'the image opens a dialog');return value;}

let modal=open();
assert.equal(modal.attributes.role,'dialog');assert.equal(modal.attributes['aria-modal'],'true');
assert.equal(modal.previewImage.src,imageURL);assert.equal(c.document.activeElement,modal.closeButton);
assert.doesNotMatch(modal.innerHTML,/<script>/,'the preview title is escaped');
const tab=modal.dispatch('keydown',{key:'Tab',shiftKey:true});
assert(tab.prevented&&tab.stopped);assert.equal(c.document.activeElement,modal.closeButton);
const escapeKey=modal.dispatch('keydown',{key:'Escape'});
assert(escapeKey.prevented&&escapeKey.stopped);assert(modal.removed);
assert.equal(c.document.activeElement,trigger,'Escape restores focus to the connected card');

modal=open();modal.dispatch('click',{target:modal.previewImage});
assert.equal(overlay(),modal,'a click inside the image keeps the preview open');
modal.dispatch('click');assert(modal.removed);assert.equal(c.document.activeElement,trigger);
modal=open();modal.closeButton.dispatch('click');assert(modal.removed);

modal=open();const previousFocuses=trigger.focusCount;
const replacement=new Element('button');replacement.isConnected=true;replacement.dataset=trigger.dataset;
c.openTraceImagePreview(replacement);
assert(modal.removed,'opening another preview replaces the existing dialog');
assert.equal(body.children.filter(node=>node.className==='tt-image-overlay').length,1);
assert.equal(trigger.focusCount,previousFocuses,'replacement does not refocus the previous card');
c.closeTraceImagePreview();assert.equal(c.document.activeElement,replacement);

modal=open();trigger.isConnected=false;const disconnectedFocuses=trigger.focusCount;
c.closeTraceImagePreview();assert.equal(trigger.focusCount,disconnectedFocuses,'removed cards are never refocused');
trigger.isConnected=true;
modal=open();modal.previewImage.dispatch('error');
assert.equal(modal.previewImage.hidden,true);
assert.match(modal.previewImage.parentElement.children[0].textContent,/unavailable/i);
c.closeTraceImagePreview(false);

const thumbnail=new Element('img'),card=new Element('button');
thumbnail.closest=selector=>selector==='.tt-image-card'?card:null;
c.traceImageFailed(thumbnail);c.traceImageFailed(thumbnail);
assert.equal(thumbnail.hidden,true);assert.equal(card.children.length,1,'a failed thumbnail gets one honest placeholder');
assert.match(card.children[0].textContent,/unavailable/i);

modal=open();const beforeReset=trigger.focusCount;c.currentSessionId='B';c.resetTraceForSession();
assert(modal.removed);assert.equal(overlay(),undefined);
assert.equal(trigger.focusCount,beforeReset,'session navigation closes without restoring focus into the old conversation');
c.currentSessionId='A';modal=open();const beforeLeave=trigger.focusCount;
vm.runInContext("currentView='trace';",c);c.switchView('chat');
assert(modal.removed);assert.equal(overlay(),undefined);assert.equal(trigger.focusCount,beforeLeave);

const rejected=new Element('button');rejected.dataset={traceImageSrc:'https://outside.invalid/image.png'};
c.openTraceImagePreview(rejected);assert.equal(overlay(),undefined,'unsafe sources cannot open a preview');
`)
}

func TestTracePresentationStableSelection(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", tracePresentationHarness+`
function select(events,index,tab,context=null){
  c.input=events;c.context=context;c.selectionIndex=index;c.selectionTab=tab;
  vm.runInContext('traceRows=buildTraceRows(input,context);traceSelectedIdx=selectionIndex;traceSelectedReq=-1;traceTab=selectionTab;',c);
  return c.traceSelectionSnapshot();
}
function restore(saved,events,context=null){
  c.input=events;c.context=context;
  vm.runInContext('traceRows=buildTraceRows(input,context);traceSelectedIdx=-1;traceSelectedReq=-1;',c);
  c.restoreTraceSelection(saved);
  return vm.runInContext('({index:traceSelectedIdx,request:traceSelectedReq,tab:traceTab,row:traceRows[traceSelectedIdx]})',c);
}
const first=event('text','earlier ',{streamID:'response-stable'});
const second=event('text','later',{streamID:'response-stable'});
const saved=select([second],0,'preview');
const restored=restore(saved,[first,second]);
assert.equal(restored.index,0);assert.equal(restored.tab,'preview');
assert.equal(restored.row.text,'earlier later');
assert.equal(c.traceRowStableKey(c.buildTraceRows([second])[0]),c.traceRowStableKey(c.buildTraceRows([first,second])[0]),
  'loading older fragments cannot change a recorded response identity');

// Old traces have no stable stream id: the still-present raw id anchors the
// selected row when loading an earlier page changes its first fragment.
const legacyFirst=event('text','old '),legacySecond=event('text','selected');
const legacySaved=select([legacySecond],0,'events');
const legacyRestored=restore(legacySaved,[legacyFirst,legacySecond]);
assert.equal(legacyRestored.index,0);assert.equal(legacyRestored.tab,'events');
assert.equal(legacyRestored.row.text,'old selected');
assert.equal(restore(legacySaved,[event('text','unrelated')]).index,-1,
  'a missing original record closes the inspector instead of selecting a new row at the old index');
const changedOwner={...legacySecond,traceInvocationID:'child',subAgentOf:'agent-2',depth:1};
assert.equal(restore(legacySaved,[changedOwner]).index,-1,
  'shared legacy record ids cannot transfer selection to a different owner');
const streamOwner={...second,traceInvocationID:'child',subAgentOf:'agent-2',depth:1};
assert.equal(restore(saved,[streamOwner]).index,-1,
  'a response id is stable only within its immutable owner');

const phase=(request,text)=>event('request_timing',text,{requestTiming:{request_id:request,
  phase:text,provider_call:1,attempt:1,elapsed_ms:10}});
const early=phase('request-stable','http_do_start'),later=phase('request-stable','request_complete');
const diagnosticSaved=select([early],0,'timing');
const diagnosticRestored=restore(diagnosticSaved,[phase('other-request','pre_request_start'),early,later]);
assert.equal(diagnosticRestored.index,1);assert.equal(diagnosticRestored.tab,'timing');
assert.equal(diagnosticRestored.row.requestTimingRecords.length,2);
assert.equal(diagnosticRestored.row.requestTimingRecords[0].request_id,'request-stable');

const snapshot={available:true,source:'logical-request',system:'recorded system',tools:[],turn:1,
  requestID:'initial-request',capturedAt:'2026-11-09T09:00:00Z'};
const question=event('user','question');
const contextSaved=select([question],0,'tools',snapshot);
const contextRestored=restore(contextSaved,[early,question],{...snapshot});
assert.equal(contextRestored.index,1);assert.equal(contextRestored.tab,'tools');
assert.equal(contextRestored.row.kind,'initial_context');
assert.equal(restore(contextSaved,[question],{...snapshot,requestID:'another-initial-request',capturedAt:'2026-11-09T09:01:00Z'}).index,-1,
  'a different captured snapshot cannot inherit an old selection');
`)
}

const tracePresentationHarness = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const escape=value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;');
const c={console,URL,URLSearchParams,currentSessionId:'A',
  window:{location:{origin:'http://localhost',href:'http://localhost/'}},
  document:{documentElement:{lang:'en'}},escHtml:escape,escAttr:escape,
  uiText:(english)=>english,formatContent:text=>'<p>'+escape(text)+'</p>'};
vm.createContext(c);vm.runInContext(source,c);
let sequence=0;
const event=(kind,text,extra={})=>({id:'event-'+(++sequence),sequence,kind,text,turn:1,
  sessionId:'A',traceInvocationID:'root',traceParentInvocationID:'',subAgentOf:'',parentID:'',depth:0,source:'',
  ts:new Date(Date.UTC(2026,10,9,10,0,sequence)).toISOString(),...extra});
`
