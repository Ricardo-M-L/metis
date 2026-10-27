package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Keep this independent of the browser acceptance runner: the first paint
// must not parse every old assistant message and then discard its DOM. Native
// row measurements, image loads and real scrolling are exercised separately.
func TestHistoryVirtualizationFirstPaint(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-first-paint", historyVirtualFirstPaintCase)
}

func TestHistoryVirtualBranchUsesSourceMessageIndex(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-branch-index", historyVirtualBranchIndexCase)
}

func TestHistoryVirtualTurnGrouping(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-turn-index", historyVirtualTurnIndexCase)
}

func TestHistoryVirtualRange(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-range", historyVirtualRangeCase)
}

func TestHistoryVirtualAnchorLifecycle(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-anchor-lifecycle", historyVirtualAnchorCase)
}

func TestHistoryVirtualMetadataReentry(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-metadata-reentry", historyVirtualMetadataCase)
}

func TestHistoryVirtualReflowCannotOverrideUserPause(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-reflow-user-pause", historyVirtualReflowPauseCase)
}

func TestHistoryVirtualRunningToolKeepsOwner(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-running-tool-owner", historyVirtualRunningToolCase)
}

func TestHistoryVirtualNativeScrollDuringRestoreKeepsReading(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-native-scroll-restore", historyVirtualNativeRestoreCase)
}

func TestHistoryVirtualLogicalContentKeepsLayoutNonempty(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-logical-layout", historyVirtualLogicalLayoutCase)
}

func TestHistoryVirtualCanonicalCommitIsVisibleBeforeNextFrame(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-canonical-atomic", historyVirtualCanonicalAtomicCase)
}

func TestHistoryVirtualFirstDraftOwnsStreamBeforeCanonical(t *testing.T) {
	runHistoryVirtualBrowserCase(t, "history-virtual-first-draft-order", historyVirtualFirstDraftCase)
}

func runHistoryVirtualBrowserCase(t *testing.T, scenario, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	virtual, err := staticFS.ReadFile("static/history_virtual.js")
	if err != nil {
		t.Fatalf("read the production history virtualizer: %v", err)
	}
	if scenario == "history-virtual-first-paint" {
		index, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			t.Fatal(err)
		}
		virtualScript, chatScript := bytes.Index(index, []byte("history_virtual.js")), bytes.Index(index, []byte("chat.js"))
		if virtualScript < 0 || chatScript < virtualScript {
			t.Fatal("the production document must load the virtualizer before its chat adapter")
		}
	}
	source = append(append(virtual, '\n'), source...)
	script := strings.Replace(streamingRenderBrowserHarness,
		"} else throw Error('unknown scenario '+scenario);", "} else {\n"+body+"\n}", 1)
	script = strings.Replace(script, "vm.createContext(c);", historyVirtualDOMSetup+"\nvm.createContext(c);", 1)
	if scenario == "history-virtual-logical-layout" {
		// Preserve the production function before the common performance shim
		// replaces it; the layout regression must exercise the real predicate.
		script = strings.Replace(script, "let layoutUpdates=0;", "const productionUpdateEmptyLayout=c.updateEmptyLayout;\nlet layoutUpdates=0;", 1)
	}
	cmd := exec.Command(node, "-e", script, scenario)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("virtual history %s: %v\n%s", scenario, err, output)
	}
}

const historyVirtualFirstPaintCase = `
assert.equal(typeof c.buildHistoryTurnIndex,'function','first paint must load the real virtualizer module');
let parsed=0;
const originalFormat=c.formatContent;
c.formatContent=(...args)=>{parsed++;return originalFormat(...args);};
const history=Array.from({length:2000},(_,n)=>({role:n%2?'assistant':'user',content:'virtual-message-'+n}));
c.renderHistoryMessages(history);
flushAll();
assert(parsed>0,'the visible tail must actually be rendered');
assert(parsed<200,'first paint must not parse all thousand historical assistant messages before discarding their DOM; parsed='+parsed);
assert(area.children.some(node=>node.classList.contains('message-assistant')&&node.innerHTML.includes('virtual-message-1999')),'first paint must include the newest answer');
assert(area.children.filter(node=>node.classList.contains('message')).length<400,'the first-paint message DOM remains bounded');
assert(!area.innerHTML.includes('virtual-message-1<'),'the offscreen oldest answer must not remain in first-paint markup');
`

const historyVirtualBranchIndexCase = `
// A displayed row index and a persisted message index diverge as soon as
// one source message contains multiple text blocks or a hidden instruction.
// Exercise the real branch action and inspect the HTTP contract it emits.
c.handleToolStart=()=>{};c.handleToolResult=()=>{};
const history=[
  {role:'user',content:'first question'},
  {role:'assistant',content:[{type:'text',text:'working one'},{type:'text',text:'working two'},
    {type:'tool_use',tool_use_id:'call-1',name:'Read',input:{path:'fixture.txt'}}]},
  {role:'user',content:[{type:'tool_result',tool_use_id:'call-1',content:'fixture contents'}]},
  {role:'assistant',content:'first answer'},
  {role:'user',content:'<system-reminder>private runtime instruction</system-reminder>'},
  {role:'assistant',content:'late answer'},
  {role:'user',content:'[user steer mid-turn] a correction'},
  {role:'assistant',content:'steered answer'},
];
for(let n=0;n<100;n++)history.push({role:'user',content:'padding question '+n},{role:'assistant',content:'padding answer '+n});
c.renderHistoryMessages(history);flushAll();
const viewport=evaluate('chatHistoryViewport');
assert(viewport&&viewport.stats().total>50,'the branch fixture must exercise the real virtual history path');
const target=viewport.units.find(unit=>unit.entries.includes(history[5]));assert(target);
viewport.reveal(target.key);flushAll();
viewport.reveal(viewport.units[90].key);flushAll();
assert(!viewport.mounted.has(target.key),'the target turn must actually be released before reentry');
viewport.reveal(target.key);flushAll();
const row=area.children.find(node=>node.classList.contains('message-assistant')&&node.innerHTML.includes('late answer'));
assert(row,'the requested answer must be present in the mounted fixture');
const calls=[];
c.fetch=async(url,options)=>{calls.push({url,body:JSON.parse(options.body)});return {ok:true,json:async()=>({sessionId:'forked'})};};
c.showToast=()=>{};c.resumeSession=async()=>{};
c.branchMessage({closest:()=>row}).then(()=>{
  assert.equal(calls.length,1,'one branch click must issue one request');
  assert.equal(calls[0].url,'/api/fork');
  assert.equal(calls[0].body.sessionId,'A');
  assert.equal(calls[0].body.messageIndex,5,'branch must identify the original persisted message, not the visible text-block ordinal');
}).catch(error=>{console.error(error);process.exitCode=1;});
`

const historyVirtualTurnIndexCase = `
assert.equal(typeof c.buildHistoryTurnIndex,'function','the real history data index must be loaded');
const history=[
  {role:'user',content:'<system-reminder>private</system-reminder>'},
  {role:'user',content:[{type:'text',text:'first A'},{type:'text',text:'first B'}]},
  {role:'assistant',content:[{type:'text',text:'working'},{type:'tool_use',tool_use_id:'t1',name:'Read',input:{}}]},
  {role:'user',content:[{type:'tool_result',tool_use_id:'t1',content:'result'}]},
  {role:'assistant',content:'first answer'},
  {role:'user',content:'[user steer mid-turn] correction'},
  {role:'assistant',content:'steered answer'},
  {role:'user',content:'second question'},
  {role:'assistant',content:'second answer'},
];
const build=(source,session)=>c.buildHistoryTurnIndex(source,session,{visibleText:c.visibleTranscriptText});
const units=build(history,'A');
assert(Array.isArray(units));
const first=units.find(unit=>unit.turn===1),second=units.find(unit=>unit.turn===2);
assert(first&&second,'two real user questions define two turns');
assert.equal(units.filter(unit=>unit.turn>0).length,2,'tool_result, hidden instructions and steer cannot create spurious turns');
assert.equal(first.sourceStart,1);assert.equal(second.sourceStart,7);
assert.equal(first.textIndexStart,0);assert.equal(second.textIndexStart,6,'visible text counting must stay distinct from persisted source indices');
assert.equal(first.entries.length,6,'the entire tool process, result, answer and steer belong to their original turn');
for(let n=1;n<=6;n++)assert(first.entries.includes(history[n]),'turn one must retain original source message '+n);
assert(second.entries.includes(history[7])&&second.entries.includes(history[8]));
const flattened=units.flatMap(unit=>unit.entries);
assert.equal(flattened.length,history.length,'no source message is dropped while building the display index');
for(let n=0;n<history.length;n++)assert.equal(flattened[n],history[n],'source order and identity must remain intact');
assert.equal(new Set(units.map(unit=>unit.key)).size,units.length);
const appended=build([...history,{role:'user',content:'third question'},{role:'assistant',content:'third answer'}],'A');
for(const unit of units){const next=appended.find(item=>item.turn===unit.turn);assert.equal(next.key,unit.key,'appending later turns must preserve earlier anchors');assert.equal(next.signature,unit.signature,'completed prefixes retain their content identity');}
const other=build(history,'B');
assert(other.every(unit=>!units.some(old=>old.key===unit.key)),'another session cannot share a reading or disclosure key');
`

const historyVirtualRangeCase = `
assert.equal(typeof c.historyVirtualRange,'function','the real virtual range function must be loaded');
const units=Array.from({length:100},(_,n)=>({key:'A:'+n,turn:n+1,estimatedHeight:100}));
const range=(top,height=300,overscan=0)=>c.historyVirtualRange(units,top,height,overscan);
const empty=c.historyVirtualRange([],0,300,0);
assert.equal(empty.start,0);assert.equal(empty.end,0);
const top=range(0);assert.equal(top.start,0);assert(top.end>0&&top.end<50,'the first range covers a bounded viewport');
const middle=range(5000);assert(middle.start<=50&&middle.end>50,'middle scroll positions must map to their logical turn');assert(middle.end-middle.start<50,'middle scroll must not mount the full history');
const tail=range(9700);assert(tail.start>50&&tail.end===100,'the last viewport must include the last logical turn');
const expanded=range(5000,300,400);assert(expanded.start<=middle.start&&expanded.end>=middle.end,'overscan extends rather than replaces the visible range');
for(const value of [range(-500),range(1e9),range(0,0)])assert(value.start>=0&&value.end<=units.length&&value.start<=value.end,'all range bounds remain valid');
const giant=[{key:'giant',estimatedHeight:10000},{key:'tail',estimatedHeight:100}];
const inside=c.historyVirtualRange(giant,5000,300,0);assert.equal(inside.start,0);assert(inside.end>=1,'a single very tall turn must remain reachable while scrolling within it');
const measured=units.map((unit,n)=>({...unit,measuredHeight:n<50?200:100}));
const afterMeasurement=c.historyVirtualRange(measured,10000,300,0);
assert(afterMeasurement.start<=50&&afterMeasurement.end>50,'known row heights must supersede the initial estimate when locating a viewport');
`

const historyVirtualAnchorCase = `
const Viewport=evaluate('HistoryVirtualViewport');
const makeUnits=session=>Array.from({length:100},(_,n)=>({key:session+':history-'+(n+1),turn:n+1,signature:'s'+n,
  estimatedHeight:100,entries:[{role:'assistant',content:'private-message-do-not-persist'}],pinReasons:[]}));
const units=makeUnits('A');
let following=false;
const options=(session,index,anchor)=>({area,sessionId:session,units:index,anchor,overscan:0,
  isFollowing:()=>following,onFollow:value=>following=value,
  renderTurn:()=>{const row=new Element();row._height=100;area.appendChild(row);return [row];}});
const anchor={sessionId:'A',key:units[40].key,turn:41,signature:units[40].signature,offset:-25,follow:false};
const first=new Viewport(options('A',units,anchor));flushAll();
let captured=first.captureAnchor();assert.equal(captured.key,anchor.key);assert.equal(captured.offset,-25);
assert.equal(first.restoreAnchor({...anchor,key:'B:history-41'}),false,'a foreign row key cannot be relabeled as the current session');
const saved=first.save();assert.equal(saved.sessionId,'A');assert.equal(saved.key,anchor.key);assert.equal(saved.offset,-25);
assert(!storage.get('metis.history-anchors.v1').includes('private-message-do-not-persist'),'position persistence must not contain conversation content');
area.clientWidth=0;
const hidden=first.save();assert.equal(hidden.key,anchor.key);assert.equal(hidden.offset,-25,'saving a hidden view must preserve its last real reading position');
area.clientWidth=980;
first.schedule();const late=frames.get(first.frame);assert.equal(typeof late,'function');first.destroy();
const b=new Viewport(options('B',makeUnits('B')));flushAll();
const bTop=area.scrollTop,bNodes=[...area.children];
late();assert.equal(area.scrollTop,bTop,'an already delivered old-view callback cannot move the new session');
assert.deepEqual(area.children,bNodes,'an already delivered old-view callback cannot mount old-session rows');
assert.equal(b.restoreAnchor(anchor),false,'another session cannot restore a foreign anchor');
b.destroy();
following=false;
const restored=new Viewport(options('A',makeUnits('A')));flushAll();
captured=restored.captureAnchor();assert.equal(captured.key,anchor.key);assert.equal(captured.offset,-25,'returning to a session restores its semantic row and within-row position');
const changed=makeUnits('A');changed[40].signature='changed-content';
restored.setUnits(changed,{anchor:saved});flushAll();
captured=restored.captureAnchor();assert.equal(captured.key,anchor.key);assert.equal(captured.offset,0,'changed source content cannot inherit the old generation pixel offset');
restored.destroy();
const defaultUnits=makeUnits('default');
const defaultViewport=new Viewport({area,sessionId:'default',units:defaultUnits,
  renderTurn:()=>{const row=new Element();row._height=100;area.appendChild(row);return [row];}});
flushAll();assert.equal(defaultViewport.captureAnchor().follow,true,'a controller without host callbacks still starts in tail-follow mode');
defaultViewport.restoreAnchor({sessionId:'default',key:defaultUnits[20].key,turn:21,signature:defaultUnits[20].signature,offset:0,follow:false});
flushAll();assert.equal(defaultViewport.captureAnchor().follow,false,'explicit reading mode also works without host callbacks');
defaultViewport.destroy();
for(let n=0;n<35;n++){
  const session='bounded-'+n,index=makeUnits(session);
  const viewport=new Viewport(options(session,index,{sessionId:session,key:index[0].key,turn:1,signature:index[0].signature,offset:0,follow:false}));
  viewport.save();viewport.destroy();
}
const persisted=JSON.parse(storage.get('metis.history-anchors.v1'));
assert.equal(persisted.schema,1);assert(persisted.items.length<=32,'reading-position persistence is bounded across sessions');
`

const historyVirtualMetadataCase = `
const Viewport=evaluate('HistoryVirtualViewport');
const units=Array.from({length:100},(_,n)=>({key:'A:history-'+(n+1),turn:n+1,signature:'s'+n,estimatedHeight:100,entries:[],pinReasons:[]}));
let following=false;const applied=[];
const viewport=new Viewport({area,sessionId:'A',units,overscan:0,
  anchor:{sessionId:'A',key:units[50].key,turn:51,signature:units[50].signature,offset:0,follow:false},
  isFollowing:()=>following,onFollow:value=>following=value,
  renderTurn:()=>{const row=new Element();row._height=100;area.appendChild(row);return [row];},
  updateMetadata:(unit,nodes,value)=>{applied.push({key:unit.key,value});nodes.forEach(node=>node.dataset.metadataVersion=String(value.version));},
});flushAll();
const old=units[5].key;
assert(!viewport.mounted.has(old));
viewport.updateMetadata(old,{version:2,state:'completed'});flushAll();
assert(!viewport.mounted.has(old),'an offscreen metadata update must not materialize old history');
assert(!applied.some(item=>item.key===old));
viewport.reveal(old);flushAll();
assert(viewport.mounted.has(old));
assert.equal(viewport.mounted.get(old).nodes[0].dataset.metadataVersion,'2','a remounted turn receives its latest metadata');
assert.equal(applied.filter(item=>item.key===old).at(-1).value.state,'completed');
const background=units[70].key;
viewport.setPinned(background,'worker',true);flushAll();assert(viewport.mounted.has(background),'an active background turn is retained outside the reading window');
viewport.setPinned(background,'worker',false);flushAll();assert(!viewport.mounted.has(background),'a settled background turn becomes releasable');
assert(viewport.mounted.has(units.at(-1).key),'the mutable live tail remains available outside the reading window');
viewport.setPinned(old,'pending-old-content',true);flushAll();
const changed=units.map(unit=>({...unit}));changed[5].signature='replaced-source';
viewport.setUnits(changed);flushAll();
assert(!viewport.metadata.has(old),'metadata from replaced source cannot attach to the same ordinal in a new generation');
assert(!viewport.pins.has(old),'an old-generation activity pin must not keep changed source alive');
assert.equal(viewport.mounted.get(old).nodes[0].dataset.metadataVersion,undefined);
viewport.reveal(units[50].key);flushAll();assert(!viewport.mounted.has(old),'the replaced turn is releasable after its old pin is invalidated');
viewport.destroy();
`

const historyVirtualReflowPauseCase = `
const Viewport=evaluate('HistoryVirtualViewport');
for(const gesture of ['wheel','scrollbar']){
  const viewport=new Viewport({area,units:[],sessionId:'A',
    isFollowing:()=>evaluate('followOutput'),onFollow:value=>{c.__follow=value;evaluate('followOutput=__follow');},renderTurn:()=>[]});
  frames.clear();viewport.frame=0;
  const unit={key:'A:history-1',turn:1,signature:'sig',estimatedHeight:1000};
  const row=new Element();row._height=1000;row.dataset.virtualTurnKey=unit.key;area.replaceChildren(row);area.scrollTop=700;
  Object.assign(viewport,{units:[unit],byKey:new Map([[unit.key,unit]]),mounted:new Map([[unit.key,{unit,nodes:[row]}]]),
    lastAnchor:{sessionId:'A',key:unit.key,turn:1,signature:'sig',offset:-700,follow:true},pendingAnchor:null});
  // Isolate ownership from geometry reconciliation. Scheduling, pause, frame
  // execution, follow decisions and anchor application stay production code.
  viewport._measure=()=>false;viewport._syncDOM=()=>{};
  c.__viewport=viewport;evaluate('chatHistoryViewport=__viewport;followOutput=true');
  viewport._reflow();
  if(gesture==='wheel')c.pauseAutoScrollOnWheel({deltaY:-1});
  else{
    c.beginChatPointerScroll({button:0,pointerType:'mouse',pointerId:10,clientX:995,clientY:100,target:area});
    area.scrollTop=300;
  }
  const top=area.scrollTop;flushAll();
  assert.equal(evaluate('followOutput'),false,'a queued reflow must not revoke the newer '+gesture+' reading intent');
  assert.equal(area.scrollTop,top,'a queued reflow must not force bottom over the newer '+gesture+' gesture');
  c.cancelChatScroll();viewport.destroy();
}
`

const historyVirtualRunningToolCase = `
c.viewedHistoryTurnIsRunning=()=>true;
c.applyHistoryMessageMetadata=()=>{};
let starts=0;
// Only construction of the tool's visual controls is replaced. The actual
// history adapter, mount ownership, pending-row lookup, args and result
// handlers remain active, including the ordinary result fallback path.
c.handleToolStart=d=>{
  starts++;
  const group=new Element();group.className='activity-group';group.dataset.turnId=evaluate('activityTurnKey()');
  const row=new Element();row.className='call-row';
  for(const [key,value]of Object.entries({id:d.id||'',tool:d.tool||'Read',state:'running',rowKey:'pending-'+starts,
    ownerSession:'A',traceCallId:d.traceCallId||'',provisional:String(!!d.provisional),args:d.input||''}))row.setAttribute('data-'+key.replace(/[A-Z]/g,x=>'-'+x.toLowerCase()),value);
  group.appendChild(row);area.appendChild(group);
  c.__group=group;c.__row=row;
  evaluate('activityGroupsByTurn.set(activityTurnKey(),[__group]);activityGroupEl=__group;toolDetails[__row.dataset.rowKey]={name:"Read",input:__row.dataset.args,state:"running"}');
};
const event={tool:'Read',id:'pending-tool',traceCallId:'pending-trace',input:'{"path":"fixture.txt"}'};
c.handleToolStart(event);
c.renderHistoryMessages([{role:'user',content:'question'},{role:'assistant',content:[{type:'tool_use',name:'Read',tool_use_id:'pending-tool',trace_call_id:'pending-trace',input:{path:'fixture.txt'}}]}]);
flushAll();
assert.equal(starts,2,'history reconstruction replaces the previous tool row once');
assert.equal(evaluate('activityCurrentTurnId'),'history-1','the resumed live owner must be the reconstructed last history turn');
const row=c.pendingToolRows('pending-tool')[0];assert(row,'the reconstructed pending call must remain addressable');
assert.equal(c.pendingToolRows('pending-tool').length,1);
c.handleToolArgsDelta({tool:'Read',id:'pending-tool',traceCallId:'pending-trace',delta:' replayed'});
c.handleToolResult({tool:'Read',id:'pending-tool',traceCallId:'pending-trace',output:'verified result',isError:false});
assert.equal(starts,2,'args and result after resume must not create another live tool row');
assert.equal(c.toolRowsInCurrentTurn().length,1);assert.equal(c.toolRowsInCurrentTurn()[0],row);
assert.equal(row.dataset.state,'ok');
c.__key=row.dataset.rowKey;assert.equal(evaluate('toolDetails[__key].output'),'verified result','the final result updates the reconstructed row');
assert.equal(c.pendingToolRows('pending-tool').length,0);
`

const historyVirtualNativeRestoreCase = `
const history=Array.from({length:1000},(_,n)=>({role:n%2?'assistant':'user',content:'restore-message-'+n}));
c.renderHistoryMessages(history);flushAll();
let viewport=evaluate('chatHistoryViewport');
viewport.reveal(viewport.units[50].key);flushAll();
c.pauseAutoScrollOnWheel({deltaY:-1});
const anchor=c.virtualHistoryCaptureAnchor();
assert(anchor&&!anchor.follow,'the fixture must start from an explicit reading intent');
// Same-session refresh may retain its mounted rows. Explicitly detach the
// view to exercise the full unload/restore path and its native clamp event.
c.cancelChatScroll();c.virtualHistoryDetach();
c.renderHistoryMessages(history);
assert.equal(area.children.length,0,'the history rebuild must expose the native empty-area scroll interval before its animation frame');
assert.equal(area._metisScrollIntent,undefined,'cancel/rebuild cleared the old wheel marker, reproducing the native-scroll race');
// Clearing the old DOM clamps scrollTop to zero and can emit a native scroll
// before the new virtual rows mount. That event has no downward user intent.
c.onChatScroll();
assert.equal(evaluate('followOutput'),false,'an empty-area native scroll cannot turn a history restore into tail following');
flushAll();
viewport=evaluate('chatHistoryViewport');
const restored=viewport.captureAnchor();
assert.equal(restored.key,anchor.key,'the rebuild must restore the original semantic reading row');
assert.equal(restored.offset,anchor.offset,'the rebuild must preserve the original within-row reading position');
assert.equal(evaluate('followOutput'),false);
// Requiring explicit downward intent must still allow the user to resume
// following by actually scrolling back to the physical bottom.
c.pauseAutoScrollOnWheel({deltaY:1});area.scrollTop=area.scrollHeight;c.onChatScroll();
assert.equal(evaluate('followOutput'),true,'a downward gesture reaching bottom can explicitly resume following');
`

const historyVirtualLogicalLayoutCase = `
c.updateEmptyLayout=productionUpdateEmptyLayout;
const main=new Element();main.className='main';
const query=c.document.querySelector;
c.document.querySelector=selector=>selector==='.main'?main:query(selector);
function reset(history){
  c.virtualHistoryDetach();area.replaceChildren();c.renderHistoryMessages(history);
}
function expectEmpty(expected,label){c.updateEmptyLayout();assert.equal(main.classList.contains('empty'),expected,label);}
reset([]);expectEmpty(true,'a genuinely empty history keeps the welcome layout');
reset([{role:'user',content:'<system-reminder>private runtime text</system-reminder>'}]);
expectEmpty(true,'an internal-only source unit is not user-visible conversation content');
reset([{role:'assistant',content:[{type:'thinking',text:'   '}]}]);
expectEmpty(true,'whitespace-only reasoning remains visually empty');
reset([{role:'user',content:[{type:'tool_result',tool_use_id:'orphan',content:'orphan output'}]}]);
expectEmpty(true,'an unrendered orphan result cannot by itself suppress the welcome layout');
reset([{role:'user',content:'question'},{role:'assistant',content:'answer'}]);
const viewport=evaluate('chatHistoryViewport');
assert(viewport.units.length>0,'the production index retains the canonical history');
const spacer=new Element();spacer.className='history-virtual-spacer';spacer.dataset.historyVirtualSpacer='all';area.replaceChildren(spacer);
assert.equal(area.querySelectorAll('.message').length,0,'canonical replacement leaves no mounted message in this interval');
expectEmpty(false,'logical conversation content must retain the chat layout while its DOM window contains only a spacer');
viewport.destroy();expectEmpty(true,'a destroyed viewport cannot resurrect old conversation content');
reset([{role:'assistant',content:[{type:'thinking',text:'visible reasoning'}]}]);
expectEmpty(false,'renderable reasoning keeps the chat layout before its row mounts');
reset([{role:'assistant',content:[{type:'redacted_thinking',data:'provider ciphertext'}]}]);
expectEmpty(false,'a redacted reasoning placeholder is still visible content');
reset([{role:'assistant',content:[{type:'tool_use',name:'Read',tool_use_id:'call',input:{path:'fixture'}}]}]);
expectEmpty(false,'a persisted tool start keeps the chat layout before its row mounts');
c.currentSessionId='B';expectEmpty(true,'a stale-session viewport cannot make the new conversation nonempty');c.currentSessionId='A';
const getById=c.document.getElementById,otherArea=new Element();
c.document.getElementById=id=>id==='chatArea'?otherArea:getById(id);
expectEmpty(true,'a viewport attached to a replaced chat area is not the current layout owner');
c.document.getElementById=id=>id==='chatArea'?null:getById(id);
expectEmpty(true,'a missing chat area has no current logical layout owner');
c.document.getElementById=getById;
reset([]);c.virtualHistoryBeginLiveTurn('new live question');
expectEmpty(false,'a live user question keeps the chat layout before its first row mounts');
c.virtualHistoryDetach();
`

const historyVirtualCanonicalAtomicCase = `
c.renderHistoryMessages([{role:'user',content:'cold question'}]);
let viewport=evaluate('chatHistoryViewport');
assert.equal(viewport.units.length,1);
assert.equal(viewport.mounted.size,0,'a cold history visit remains lazy until its first frame');
assert.equal(area.querySelectorAll('.message').length,0);
c.virtualHistoryDetach();
c.renderHistoryMessages([]);flushFrame();
c.addMessage('user','canonical question');flushAll();
viewport=evaluate('chatHistoryViewport');
assert.equal(viewport.units.length,1);assert.equal(viewport.units[0].live,true);
assert.equal(viewport.mounted.size,1,'the sole old live turn must actually be materialized');
c.pauseAutoScrollOnWheel({deltaY:-1});
const anchor=c.virtualHistoryCaptureAnchor();assert(anchor&&!anchor.follow);
const oldUser=area.querySelector('.message-user');assert(oldUser);
const canonical=[{role:'user',content:'canonical question'},{role:'assistant',content:'canonical saved answer'}];
c.renderHistoryMessages(canonical);
// Deliberately do not deliver an animation frame: the old live signature
// was just retired, and the same-session commit must already be visible.
assert.equal(evaluate('chatHistoryViewport'),viewport,'canonical commit reuses the current viewport');
assert.equal(viewport.units[0].live,undefined);
assert(viewport.mounted.size>0,'retiring the sole live record must not leave a spacer-only paint');
const rows=area.querySelectorAll('.message');
assert.equal(rows.length,2,'the canonical user and answer are mounted before the next frame');
assert(rows.some(row=>row.classList.contains('message-assistant')&&row.innerHTML.includes('canonical saved answer')));
assert(!oldUser.isConnected,'the old live row was genuinely replaced rather than hiding the remount gap');
const committed=c.virtualHistoryCaptureAnchor();
assert.equal(committed.key,anchor.key);assert.equal(committed.offset,anchor.offset);
assert.equal(evaluate('followOutput'),false,'an atomic canonical commit preserves the user reading pause');
const nodes=[...rows];flushFrame();
assert.deepEqual(area.querySelectorAll('.message'),nodes,'a stale queued frame cannot replace the completed canonical commit');
area.clientWidth=0;viewport.restoreAnchor(committed);
const pending=viewport.pendingAnchor;
assert.equal(viewport.flush(),false,'a hidden viewport does not consume its pending restore');
assert.equal(viewport.pendingAnchor,pending);
area.clientWidth=980;assert.equal(viewport.flush(),true);
assert.equal(evaluate('followOutput'),false);
c.virtualHistoryDetach();
`

const historyVirtualFirstDraftCase = `
let complete=false;
const watchdog=setTimeout(()=>{if(!complete){console.error('draft flow did not reach canonical assertions');process.exitCode=1;}},2000);
(async()=>{
  c.currentSessionId=null;c.resumeSessionGeneration=0;c.activeWorkspaceId='workspace';c.sessions=[];
  const order=[],errors=[];const sendButton=new Element();
  const getById=c.document.getElementById;
  c.document.getElementById=id=>id==='sendBtn'?sendButton:getById(id);
  c.parallelTurnsEnabled=()=>true;c.renderSessions=()=>{};c.beginUserTurn=()=>{};
  c.permissionModeForTurn=()=>'plan';c.pollStatus=async()=>{};c.syncTrackedRunningState=()=>{};
  c.finishUserTurn=()=>{};c.finishForegroundRequest=()=>{};c.rememberForegroundRequest=()=>{};
  c.applyResumeSnapshot=()=>{};c.applyStatusPermissionSnapshot=()=>{};c.restoreCompactionHistory=async()=>{};
  c.showToast=text=>errors.push(text);c.showError=text=>{throw new Error(text);};
  const answer='DRAFT_STREAM_ANSWER '+ 'long answer '.repeat(100);
  const canonical=[{role:'user',content:'draft question'},{role:'assistant',content:answer}];
  let releaseTurn,enteredResolve,canonicalResolve,sawPost=false;
  const turnResponse=new Promise(resolve=>releaseTurn=resolve),entered=new Promise(resolve=>enteredResolve=resolve);
  const canonicalApplied=new Promise(resolve=>canonicalResolve=resolve);
  c.fetch=(url,options)=>{
    if(url==='/api/sessions'){
      order.push('draft-http');return Promise.resolve({ok:true,json:async()=>({id:'draft-A',permissionMode:'plan'})});
    }
    if(url==='/api/turns'){
      sawPost=true;order.push('turn-http');const body=JSON.parse(options.body);
      assert.equal(body.sessionId,'draft-A','the turn request must already have its allocated draft ID');
      assert.equal(body.expectedPermissionMode,'plan','draft allocation cannot replace the permission snapshot');
      enteredResolve();return turnResponse;
    }
    if(url==='/api/sessions/draft-A'){
      order.push('canonical-http');return Promise.resolve({ok:true,json:async()=>({session:{id:'draft-A',status:'completed'},messages:canonical})});
    }
    throw new Error('unexpected draft-flow request '+url);
  };
  const prepare=c.prepareDraftSession;
  c.prepareDraftSession=async(...args)=>{order.push('prepare-start');const ok=await prepare(...args);order.push('prepare-ready');return ok;};
  const add=c.addMessage;
  c.addMessage=(role,text,...args)=>{
    if(role==='user'){
      order.push('user-message');assert.equal(c.currentSessionId,'draft-A');
      const viewport=evaluate('chatHistoryViewport');
      assert(viewport&&viewport.sessionId==='draft-A','the allocated draft must own an empty viewport before its first user row');
    }
    return add(role,text,...args);
  };
  const sync=c.syncViewedSessionHistory;
  c.syncViewedSessionHistory=async(...args)=>{const result=await sync(...args);canonicalResolve(result);return result;};
  const start=c.startStreamingMessage;
  c.startStreamingMessage=()=>{start();const row=rendered.at(-1).row;row.className='message message-assistant';row._height=1000;area.appendChild(row);};
  // Match the composer path: allocate the real draft asynchronously before
  // the actual turn function renders its first user message.
  assert.equal(await c.prepareDraftSession({force:true}),true);
  const turn=c.runTurnItem({text:'draft question',images:[]});
  await Promise.race([entered,turn]);assert(sawPost,'the real draft flow must reach its turn request: '+JSON.stringify({order,errors,session:c.currentSessionId}));
  assert.equal(order.filter(item=>item==='draft-http').length,1,'the prepared composer draft must not be allocated twice');
  assert(order.indexOf('prepare-ready')<order.indexOf('user-message')&&order.indexOf('user-message')<order.indexOf('turn-http'));
  const viewport=evaluate('chatHistoryViewport');assert(viewport.units.at(-1).live);
  c.handleTextDelta({delta:answer});flushAll();
  c.pauseAutoScrollOnWheel({deltaY:-1});area.scrollTop=400;viewport._scroll();c.onChatScroll();flushFrame();
  const anchor=c.virtualHistoryCaptureAnchor();assert(anchor&&!anchor.follow);
  c.endStreamingMessage(false);assert.equal(evaluate('followOutput'),false);
  releaseTurn({ok:true,json:async()=>({sessionId:'draft-A',text:answer})});
  assert.equal(await turn,true);assert.equal(await canonicalApplied,true);
  assert.equal(evaluate('chatHistoryViewport'),viewport,'the first canonical response must reuse the draft-owned viewport');
  assert.equal(evaluate('followOutput'),false,'the first canonical response cannot turn paused reading into tail following');
  assert.equal(c.virtualHistoryCaptureAnchor().key,anchor.key);
  assert.equal(viewport.units.at(-1).live,undefined);assert(order.includes('canonical-http'));assert.deepEqual(errors,[]);
  c.virtualHistoryDetach();
})().then(()=>{complete=true;clearTimeout(watchdog);}).catch(error=>{complete=true;clearTimeout(watchdog);console.error(error);process.exitCode=1;});
`

// This small DOM models lifecycle, insertion and geometry, not HTML/CSS
// layout. Expensive history formatting remains the production path.
const historyVirtualDOMSetup = `
c.window=c;
const storage=new Map();
c.sessionStorage={getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,String(v)),removeItem:k=>storage.delete(k)};
c.window.sessionStorage=c.sessionStorage;
c.window.addEventListener=()=>{};c.window.removeEventListener=()=>{};
c.window.requestAnimationFrame=c.requestAnimationFrame;c.window.cancelAnimationFrame=c.cancelAnimationFrame;
c.window.getSelection=()=>({isCollapsed:true,rangeCount:0});
c.window.innerHeight=800;c.window.innerWidth=1000;
c.getComputedStyle=()=>({marginTop:'0',marginBottom:'0',paddingTop:'0',paddingBottom:'0',direction:'ltr'});
c.ResizeObserver=class{observe(){}unobserve(){}disconnect(){}};
c.MutationObserver=class{observe(){}disconnect(){}};
Object.defineProperties(Element.prototype,{
  childNodes:{get(){return this.children;}},
  firstChild:{get(){return this.children[0]||null;}},
  lastChild:{get(){return this.children.at(-1)||null;}},
  firstElementChild:{get(){return this.firstChild;}},
  lastElementChild:{get(){return this.lastChild;}},
  parentNode:{get(){return this.parentElement;}},
  nextSibling:{get(){const p=this.parentElement;return p?p.children[p.children.indexOf(this)+1]||null:null;}},
  nextElementSibling:{get(){return this.nextSibling;}},
  previousSibling:{get(){const p=this.parentElement;return p?p.children[p.children.indexOf(this)-1]||null:null;}},
  isConnected:{get(){for(let n=this;n;n=n.parentElement)if(n===area)return true;return false;}},
  className:{get(){return this._className||'';},set(v){
    for(const old of (this._className||'').split(/\s+/))if(old)this.classList.remove(old);
    this._className=String(v);for(const name of this._className.split(/\s+/))if(name)this.classList.add(name);
  }},
  offsetHeight:{get(){if(this._height!==undefined)return this._height;const styled=Number.parseFloat(this.style.height);return Number.isFinite(styled)?Math.max(0,styled):96;}},
});
Object.defineProperties(area,{
  innerHTML:{get(){return this._html;},set(value){this._html=String(value);this.htmlWrites++;for(const node of [...this.children])node.remove();}},
  scrollHeight:{get(){this.heightReads++;return this.children.reduce((height,node)=>height+node.offsetHeight,0);}},
  scrollTop:{get(){return Math.max(0,Math.min(this._scrollTop||0,Math.max(0,this.scrollHeight-this.clientHeight)));},set(value){this._scrollTop=Math.max(0,Math.min(Number(value)||0,Math.max(0,this.scrollHeight-this.clientHeight)));this.scrollWrites++;}},
});
const originalSetAttribute=Element.prototype.setAttribute;
Element.prototype.setAttribute=function(k,v){originalSetAttribute.call(this,k,v);if(k.startsWith('data-'))this.dataset[k.slice(5).replace(/-([a-z])/g,(_,x)=>x.toUpperCase())]=String(v);};
Element.prototype.remove=function(){const p=this.parentElement;if(p){const at=p.children.indexOf(this);if(at>=0)p.children.splice(at,1);}this.parentElement=null;};
Element.prototype.appendChild=function(node){
  if(node.nodeType===11){for(const child of [...node.children])this.appendChild(child);return node;}
  node.remove();node.parentElement=this;this.children.push(node);return node;
};
Element.prototype.insertBefore=function(node,before){
  if(node.nodeType===11){for(const child of [...node.children])this.insertBefore(child,before);return node;}
  node.remove();const at=before?this.children.indexOf(before):-1;
  this.children.splice(at<0?this.children.length:at,0,node);node.parentElement=this;return node;
};
Element.prototype.replaceChildren=function(...nodes){for(const child of [...this.children])child.remove();for(const node of nodes)this.appendChild(node);};
Element.prototype.contains=function(node){for(let n=node;n;n=n.parentElement)if(n===this)return true;return false;};
Element.prototype.getBoundingClientRect=function(){
  if(this===area)return {left:0,right:1000,top:0,bottom:300,width:1000,height:300};
  let top=-area.scrollTop;const p=this.parentElement;
  if(p)for(const sibling of p.children){if(sibling===this)break;top+=sibling.offsetHeight;}
  return {left:0,right:980,top,bottom:top+this.offsetHeight,width:980,height:this.offsetHeight};
};
Element.prototype.querySelectorAll=function(selector){
  const options=selector.split(',').map(s=>s.trim());const matches=[];
  const accept=(node,s)=>{
    const tail=s.trim().split(/\s+/).at(-1);
    const cls=tail.match(/^\.([\w-]+)/);if(cls&&!node.classList.contains(cls[1]))return false;
    for(const attr of tail.matchAll(/\[([\w-]+)(?:=['"]?([^\]'"\s]+)['"]?)?\]/g)){
      const name=attr[1];const value=name.startsWith('data-')?node.dataset[name.slice(5).replace(/-([a-z])/g,(_,x)=>x.toUpperCase())]:node.attrs[name];
      if(value===undefined||attr[2]!==undefined&&String(value)!==attr[2])return false;
    }
    return !!cls||tail.startsWith('[');
  };
  const walk=node=>{for(const child of node.children){if(options.some(s=>accept(child,s)))matches.push(child);walk(child);}};
  walk(this);return matches;
};
Element.prototype.querySelector=function(selector){return this.nodes.get(selector)||this.querySelectorAll(selector)[0]||null;};
Element.prototype.matches=function(selector){
  return selector.split(',').some(option=>{
    const cls=option.trim().match(/^\.([\w-]+)/);if(cls&&!this.classList.contains(cls[1]))return false;
    for(const attr of option.matchAll(/\[([\w-]+)(?:=['"]?([^\]'"\s]+)['"]?)?\]/g)){
      const name=attr[1],value=name.startsWith('data-')?this.dataset[name.slice(5).replace(/-([a-z])/g,(_,x)=>x.toUpperCase())]:this.attrs[name];
      if(value===undefined||attr[2]!==undefined&&String(value)!==attr[2])return false;
    }
    return !!cls||option.trim().startsWith('[');
  });
};
Element.prototype.closest=function(selector){for(let node=this;node;node=node.parentElement)if(node.matches(selector))return node;return null;};
Element.prototype.insertAdjacentHTML=function(position,html){
  assert.equal(position,'beforeend');this.insertions++;this._html+=String(html);
  // The first-paint fixture is text-only. Represent its top-level message
  // siblings so the real virtualizer can mount, measure and release them.
  const matches=[...String(html).matchAll(/<div class="message (message-(?:user|assistant))"([^>]*)>/g)];
  for(let i=0;i<matches.length;i++){
    const match=matches[i];
    const row=new Element();row.nodeType=1;row.className='message '+match[1];
    row._html=String(html).slice(match.index,matches[i+1]?.index);
    for(const attr of match[2].matchAll(/(data-[\w-]+)="([^"]*)"/g))row.setAttribute(attr[1],attr[2]);
    this.appendChild(row);
  }
};
c.document.createElement=tag=>{const el=new Element();el.tagName=String(tag).toUpperCase();el.nodeType=1;el.ownerDocument=c.document;return el;};
c.document.createDocumentFragment=()=>{const el=new Element();el.nodeType=11;return el;};
c.document.querySelectorAll=s=>area.querySelectorAll(s);
c.document.querySelector=s=>area.querySelector(s);
c.document.activeElement=null;
c.Element=Element;c.HTMLElement=Element;
area.nodeType=1;area.tagName='DIV';area.ownerDocument=c.document;
`
