package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestStreamingRenderBrowserBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"batch", "stream-frame-follow", "stream-frame-pause", "thinking-frame-follow", "terminal", "terminal-noop-dom", "terminal-changed-paused", "terminal-changed-follow", "terminal-anchor-guards", "thinking-boundary", "text-thinking-text", "filter", "session", "scroll", "scroll-anchoring", "scroll-keyboard", "scroll-pointer", "scroll-touch", "history", "markdown-frontier"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(node, "-e", streamingRenderBrowserHarness, scenario)
			cmd.Stdin = bytes.NewReader(source)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("stream rendering %s: %v\n%s", scenario, err, output)
			}
		})
	}
}

// The shim deliberately has no childNodes: Markdown's native-node cache is
// exercised by browser acceptance, while this fixture observes batching,
// terminal ordering, and scroll reads without requiring a browser install.
const streamingRenderBrowserHarness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
class Element {
  constructor() {
    this.dataset = {}; this.style = {}; this.attrs = {}; this.hidden = false;
    this.parentElement = null; this.children = []; this.nodes = new Map();
    this._html = ''; this.htmlWrites = 0; this.insertions = 0; this._text = ''; this.textWrites = 0;
    this.offsetWidth = 1000; this.clientWidth = 980; this._scrollTop = 0; this._scrollHeight = 1000; this.heightReads = 0; this.scrollWrites = 0; this.clientHeight = 300;
    const classes = new Set();
    this.classList = {contains: key => classes.has(key), add: key => classes.add(key),
      remove: key => classes.delete(key), toggle: (key, on) => {if(on) classes.add(key);else classes.delete(key);return !!on;}};
  }
  set innerHTML(value) { this._html = String(value); this.htmlWrites++; }
  get innerHTML() { return this._html; }
  set textContent(value) { this._text = String(value); this.textWrites++; }
  get textContent() { return this._text; }
  get scrollHeight() { this.heightReads++; return this._scrollHeight; }
  set scrollTop(value) { this._scrollTop = Number(value); this.scrollWrites++; }
  get scrollTop() { return this._scrollTop; }
  insertAdjacentHTML(position, html) { assert.equal(position,'beforeend'); this.insertions++; this._html += html; }
  appendChild(node) { this.children.push(node); node.parentElement = this; return node; }
  setAttribute(key, value) { this.attrs[key] = String(value); }
  getAttribute(key) { return this.attrs[key] || null; }
  getBoundingClientRect() { return {left:0,right:1000,top:0,bottom:300,width:1000,height:300}; }
  removeAttribute(key) { delete this.attrs[key]; }
  querySelector(selector) { return this.nodes.get(selector) || null; }
  querySelectorAll() { return []; }
  closest() { return null; }
  remove() { this.removed = true; this.parentElement = null; }
  addEventListener() {}
}
const area = new Element(), jump = new Element();
let frameID = 0;
const frames = new Map(), timers = new Map(), rendered = [], thinkRows = [];
const c = {console, Map, Set, Array, Date, AbortController, window: {},
  document: {documentElement:{lang:'en'}, addEventListener(){},
    getElementById:id => id === 'chatArea' ? area : id === 'jumpLatestBtn' ? jump : null,
    querySelectorAll:()=>[], querySelector:()=>null, createElement:()=>new Element()},
  requestAnimationFrame:fn => {const id=++frameID;frames.set(id,fn);return id;},
  cancelAnimationFrame:id => frames.delete(id),
  setTimeout:fn => {const id=++frameID;timers.set(id,fn);return id;}, clearTimeout:id=>timers.delete(id),
  queueMicrotask:fn=>fn(),
  uiText:en=>en,
  escHtml:value=>String(value).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'),
  escAttr:value=>String(value).replace(/&/g,'&amp;').replace(/"/g,'&quot;'),
  fmtMs:ms=>String(ms), currentSessionId:'A', currentView:'chat', messages:[],
};
vm.createContext(c);
vm.runInContext(source,c);
const evaluate = expression => vm.runInContext(expression,c);
function flushFrame() {
  const pending=[...frames]; frames.clear();
  for(const [,fn] of pending) fn();
}
function flushAll() {
  for(let count=0;frames.size;count++){assert(count<20,'frame scheduling must settle');flushFrame();}
}
for(const key of ['finishActivityGroup','endTurnStatus','closeToolDetail','updateSendBtn','loadSessions',
  'loadSessionStatsbar','loadTrace','clearTodoPlan','setTurnRunning','restoreTodoPlanFromHistory',
  'refreshHistoricalSubagentStatuses','restoreHistoryMessageMetadata','scheduleActivityGroupFollow']) c[key]=()=>{};
let layoutUpdates=0;
c.updateEmptyLayout=()=>layoutUpdates++;
c.messageActionsMarkup=()=>'';
c.attachMessageActions=()=>{};
c.viewedHistoryTurnIsRunning=()=>false;
c.startStreamingMessage=()=>{
  const row=new Element(), box=new Element(); row.nodes.set('.message-content',box);
  row.parentElement=area; rendered.push({row,box});
  c.__row=row;
  evaluate('streaming=true; streamingEl=__row; streamMsgIdx=messages.length');
  c.autoScroll();
};
c.appendThinkingRow=(text,options={})=>{
  const row=new Element(), body=new Element(), summary=new Element(), time=new Element();
  row.nodes.set('.think-body',body);row.nodes.set('.think-summary',summary);row.nodes.set('.think-time',time);
  row.parentElement=area; body.textContent=text; row.attrs['data-running']=String(!!options.running);
  thinkRows.push({row,body,summary,time});return row;
};
function nativePaintScroll() {
  Object.defineProperty(area,'scrollTop',{
    get(){return this._scrollTop;},
    set(value){this._scrollTop=Math.max(0,Math.min(Number(value)||0,Math.max(0,this._scrollHeight-this.clientHeight)));this.scrollWrites++;},
  });
  area._scrollTop=area._scrollHeight-area.clientHeight;
}
function streamPaintFixture() {
  nativePaintScroll();c.handleTextDelta({delta:'first'});flushFrame();
  const box=rendered[0].box;
  Object.defineProperty(box,'innerHTML',{
    get(){return this._html;},
    set(value){this._html=String(value);this.htmlWrites++;area._scrollHeight+=24;},
  });
  area.scrollWrites=0;area.heightReads=0;return box;
}
function assertPaintAtBottom(message) {
  assert.equal(area.scrollHeight-area.scrollTop-area.clientHeight,0,message);
}
function terminalReadFixture(text,paused=true) {
  nativePaintScroll();c.handleTextDelta({delta:text});flushFrame();
  const {row,box}=rendered[0];area.appendChild(row);
  row.contains=node=>node===box||node===row;
  row.children=[box];box.parentElement=row;
  row.getBoundingClientRect=box.getBoundingClientRect=()=>({left:0,right:1000,top:-area.scrollTop,bottom:1000-area.scrollTop,width:1000,height:1000});
  if(paused){c.pauseAutoScrollOnWheel({deltaY:-1});area.scrollTop=400;}
  area.scrollWrites=0;return box;
}
function terminalClampOnHTMLWrite(box) {
  let writes=0;
  Object.defineProperty(box,'innerHTML',{
    get(){return this._html;},
    set(value){writes++;this._html=String(value);this.htmlWrites++;area._scrollTop=0;},
  });
  return ()=>writes;
}
const scenario=process.argv[1];
if(scenario==='batch') {
  c.handleTextDelta({delta:'first'});
  assert.equal(rendered.length,1,'the first visible token appears immediately');
  assert.equal(rendered[0].box.innerHTML,c.formatContent('first'));
  const writes=rendered[0].box.htmlWrites;
  for(let n=0;n<100;n++)c.handleTextDelta({delta:' x'});
  assert.equal(rendered[0].box.htmlWrites,writes,'same-frame chunks must not repaint the whole answer');
  flushFrame();
  assert.equal(rendered[0].box.htmlWrites,writes+1,'one frame paints one accumulated update');
  assert.equal(rendered[0].box.innerHTML,c.formatContent('first'+' x'.repeat(100)));
  c.handleTextDelta({delta:' last'});c.flushStreamingRender();
  assert.equal(rendered[0].box.innerHTML,c.formatContent('first'+' x'.repeat(100)+' last'));
  flushAll();
} else if(scenario==='stream-frame-follow') {
  streamPaintFixture();
  for(let frame=0;frame<16;frame++){
    const writes=area.scrollWrites;
    for(let chunk=0;chunk<4;chunk++)c.handleTextDelta({delta:' next'});
    flushFrame(); // Observe this paint, not a later settling animation frame.
    assertPaintAtBottom('streaming DOM growth must follow in its own paint frame '+frame);
    assert.equal(area.scrollWrites-writes,1,'one text commit has one chat scroll write');
    assert.equal(evaluate('chatScrollFrame'),0,'a completed paint must not leave a second chat scroll queued');
  }
  const writes=area.scrollWrites;flushFrame();
  assert.equal(area.scrollWrites,writes,'a canceled follow callback cannot write again on the next paint');
} else if(scenario==='stream-frame-pause') {
  streamPaintFixture();
  c.autoScroll(); // An ordinary follow request is queued alongside the delta.
  assert.equal(area.scrollWrites,0,'ordinary autoScroll remains frame batched');
  c.handleTextDelta({delta:' queued content'});
  c.pauseAutoScrollOnWheel({deltaY:-1});
  const top=area.scrollTop;flushFrame();
  assert.equal(evaluate('followOutput'),false);
  assert.equal(area.scrollTop,top,'an upward reading gesture wins over the text commit');
  assert.equal(area.scrollWrites,0,'neither the queued follow nor the text commit can write after a user pause');
  flushFrame();assert.equal(area.scrollWrites,0,'a late callback also respects the pause');
} else if(scenario==='thinking-frame-follow') {
  nativePaintScroll();c.handleThinkingDelta({delta:'first thought'});flushFrame();
  const body=thinkRows[0].body;
  Object.defineProperty(body,'textContent',{
    get(){return this._text;},
    set(value){this._text=String(value);this.textWrites++;area._scrollHeight+=24;},
  });
  area.scrollWrites=0;
  for(let frame=0;frame<8;frame++){
    const writes=area.scrollWrites;c.handleThinkingDelta({delta:' more'});flushFrame();
    assertPaintAtBottom('thinking DOM growth must follow in its own paint frame '+frame);
    assert.equal(area.scrollWrites-writes,1);
  }
} else if(scenario==='terminal') {
  const text='# Heading\n\n- one\n- two\n\n| A | B |\n| -- | -- |\n| 1 | 2 |\n\n'+
    '\x60\x60\x60js\nconst n = 1;\n\x60\x60\x60\n\n$x^2$ and <script>unsafe</script> <';
  for(let n=0;n<text.length;n+=7)c.handleTextDelta({delta:text.slice(n,n+7)});
  c.endStreamingMessage(false);
  assert.equal(rendered.length,1,'ending a queued stream creates no duplicate answer');
  assert.equal(rendered[0].box.innerHTML,c.formatContent(text),'settled HTML must use the complete original formatter');
  assert.equal(c.messages.length,1);assert.equal(c.messages[0].content,text);
  const settled=rendered[0].box.htmlWrites;
  flushAll();
  assert.equal(rendered[0].box.htmlWrites,settled,'a stale animation frame cannot rewrite a completed answer');
  assert.equal(rendered.length,1);assert.equal(c.messages.length,1);
} else if(scenario==='terminal-noop-dom') {
  const text='Stable **reply**';
  const box=terminalReadFixture(text),before=box.innerHTML,terminalWrites=terminalClampOnHTMLWrite(box);
  c.endStreamingMessage(false);
  assert.equal(terminalWrites(),0,'identical terminal markup must retain the existing DOM instead of triggering a native scroll clamp');
  assert.equal(box.innerHTML,before);assert.equal(area.scrollTop,400);assert.equal(area.scrollWrites,0);
  assert.equal(c.messages.length,1);assert.equal(c.messages[0].content,text);
  c.endStreamingMessage(false);assert.equal(terminalWrites(),0,'a second empty terminal event does not replace settled nodes');
} else if(scenario==='terminal-changed-paused') {
  const text='Visible answer <';
  const box=terminalReadFixture(text),before=box.innerHTML,terminalWrites=terminalClampOnHTMLWrite(box);
  assert.notEqual(before,c.formatContent(text),'the fixture must require a final real markup change');
  c.endStreamingMessage(false); // No animation frame is delivered before these assertions.
  assert.equal(terminalWrites(),1,'a final buffered literal must still be committed');
  assert.equal(box.innerHTML,c.formatContent(text));assert.equal(c.messages[0].content,text);
  assert.equal(area.scrollTop,400,'a terminal DOM setter clamp must restore the cold-session reading anchor in the same task');
  assert.equal(evaluate('followOutput'),false);assert.equal(evaluate('chatScrollFrame'),0);
  const writes=area.scrollWrites;flushFrame();assert.equal(area.scrollWrites,writes,'restoring reading leaves no deferred tail write');
} else if(scenario==='terminal-changed-follow') {
  const text='Followed answer <';
  const box=terminalReadFixture(text,false),terminalWrites=terminalClampOnHTMLWrite(box);
  c.endStreamingMessage(false);
  assert.equal(terminalWrites(),1);assert.equal(box.innerHTML,c.formatContent(text));
  assert.equal(evaluate('followOutput'),true);
  assertPaintAtBottom('a terminal clamp while following must settle before the next frame');
  const writes=area.scrollWrites;flushFrame();assert.equal(area.scrollWrites,writes,'terminal following must not repeat in the next frame');
} else if(scenario==='terminal-anchor-guards') {
  area._scrollTop=400;area.scrollWrites=0;c.endStreamingMessage(false);
  assert.equal(area.scrollTop,400);assert.equal(area.scrollWrites,0,'an empty terminal event has no pending DOM or follow write');
  const box=terminalReadFixture('Guarded answer <');
  const saved=c.captureStreamingReadAnchor(box);assert(saved,'the paused cold-session fixture captures a real anchor');
  area._scrollTop=0;c.currentSessionId='B';c.restoreStreamingReadAnchor(saved);
  assert.equal(area.scrollWrites,0,'a stale session cannot restore an old reading position');
  c.currentSessionId='A';
  const getById=c.document.getElementById,other=new Element();
  c.document.getElementById=id=>id==='chatArea'?other:getById(id);c.restoreStreamingReadAnchor(saved);
  assert.equal(area.scrollWrites,0,'a replaced chat area rejects an old reading position');c.document.getElementById=getById;
  area._metisScrollGesture={active:true};assert.equal(c.captureStreamingReadAnchor(box),null);c.restoreStreamingReadAnchor(saved);
  assert.equal(area.scrollWrites,0,'an active user gesture owns the scroll position');delete area._metisScrollGesture;
  evaluate('followOutput=true');assert.equal(c.captureStreamingReadAnchor(box),null);c.restoreStreamingReadAnchor(saved);
  assert.equal(area.scrollWrites,0,'an old reading anchor cannot override resumed following');evaluate('followOutput=false');
  c.restoreStreamingReadAnchor(saved);assert.equal(area.scrollTop,400);
} else if(scenario==='thinking-boundary') {
  c.handleThinkingDelta({delta:'First line'});
  assert.equal(thinkRows.length,1);assert.equal(thinkRows[0].body.textContent,'First line');
  c.handleThinkingDelta({delta:'\nLast line'});
  const before=thinkRows[0].body.textWrites;
  c.handleThinkingDelta({delta:' complete'});
  assert.equal(thinkRows[0].body.textWrites,before,'thinking updates should batch too');
  c.handleTextDelta({delta:'Answer'});
  assert.equal(thinkRows[0].body.textContent,'First line\nLast line complete','text boundary must not lose queued reasoning');
  assert.equal(thinkRows[0].summary.textContent,'First line','settled reasoning previews its first line');
  assert.equal(thinkRows[0].row.attrs['data-running'],undefined);
  flushAll();
  assert.equal(rendered[0].box.innerHTML,c.formatContent('Answer'));
} else if(scenario==='text-thinking-text') {
  c.handleTextDelta({delta:'Before'});c.handleTextDelta({delta:' tool'});
  c.handleThinkingDelta({delta:'Reason'});c.handleThinkingDelta({delta:' more'});
  assert.equal(c.messages.length,1,'entering reasoning commits the preceding text segment');
  assert.equal(c.messages[0].content,'Before tool');
  assert.equal(rendered.length,1);
  c.handleTextDelta({delta:'After'});c.endStreamingMessage(false);
  assert.equal(rendered.length,2,'a later answer owns a fresh text segment');
  assert.equal(c.messages.length,2);assert.equal(c.messages[1].content,'After');
  assert.equal(thinkRows[0].body.textContent,'Reason more');
  flushAll();
} else if(scenario==='filter') {
  c.handleTextDelta({delta:'<sys'});
  assert.equal(rendered.length,0,'a partial internal tag is never painted');
  c.handleTextDelta({delta:'tem-reminder>private'});
  assert.equal(rendered.length,0,'an unfinished internal envelope remains hidden');
  c.handleTextDelta({delta:'</system-reminder>Visible'});
  assert.equal(rendered.length,1);assert.equal(rendered[0].box.innerHTML,c.formatContent('Visible'));
  c.handleTextDelta({delta:'<memory-context>hidden</memory-context> final <'});
  c.endStreamingMessage(false);flushAll();
  assert.equal(rendered[0].box.innerHTML,c.formatContent('Visible final <'));
  assert.equal(c.messages[0].content,'Visible final <');
  assert(!rendered[0].box.innerHTML.includes('private'));assert(!rendered[0].box.innerHTML.includes('hidden'));
} else if(scenario==='session') {
  c.handleTextDelta({delta:'A first'});c.handleTextDelta({delta:' A queued'});
  c.currentSessionId='B';
  const before=rendered[0].box.htmlWrites;
  flushAll();
  assert.equal(rendered[0].box.htmlWrites,before,'old-session work is rejected when its frame arrives');
  c.cancelStreamingRender();
  evaluate('streaming=false;streamingEl=null;streamingText=""');
  c.handleTextDelta({delta:'B first'});c.handleTextDelta({delta:' B queued'});
  c.flushStreamingRender();
  assert.equal(rendered.length,2);
  assert.equal(rendered[1].box.innerHTML,c.formatContent('B first B queued'));
  assert(!rendered[1].box.innerHTML.includes('A first'));
  flushAll();
} else if(scenario==='scroll') {
  for(let n=0;n<100;n++)c.autoScroll();
  assert.equal(area.heightReads,0,'stream arrival must not synchronously measure layout');
  assert.equal(area.scrollWrites,0);
  flushFrame();
  assert(area.heightReads<=2,'one animation frame measures scroll geometry once');
  assert.equal(area.scrollWrites,1,'one animation frame follows once');
  flushAll();
  area._scrollTop=100;area.heightReads=0;area.scrollWrites=0;
  c.autoScroll();c.pauseAutoScrollOnWheel({deltaY:-1});flushAll();
  assert.equal(area.scrollTop,100,'an upward gesture wins over previously scheduled follow');
  assert.equal(area.scrollWrites,0);
  c.resumeAutoScroll();flushAll();
  assert.equal(area.scrollTop,1000,'explicit jump to latest resumes follow');
  assert.equal(area.scrollWrites,1);
} else if(scenario==='scroll-anchoring') {
  c.autoScroll();flushAll();
  // Native scroll anchoring and layout clamping can change the offset
  // without a wheel, key, touch, or scrollbar interaction from the user.
  area._scrollHeight=1600;area._scrollTop=500;area.scrollWrites=0;
  c.onChatScroll();
  assert.equal(evaluate('followOutput'),true,'layout scroll without user intent must preserve output follow');
  flushAll();
  assert.equal(area.scrollTop,1600,'layout scroll schedules a fresh bottom measurement');
  assert.equal(area.scrollWrites,1,'layout recovery follows once in the next frame');
} else if(scenario==='scroll-keyboard') {
  c.autoScroll();
  c.pauseAutoScrollOnKey({key:'PageUp',target:area});flushAll();
  assert.equal(evaluate('followOutput'),false,'keyboard upward navigation pauses before a scheduled follow');
  assert.equal(area.scrollWrites,0);
  area._scrollTop=700;c.onChatScroll();
  assert.equal(evaluate('followOutput'),false,'near-bottom layout does not override an upward reading intent');
  c.pauseAutoScrollOnKey({key:'PageDown',target:area});c.onChatScroll();
  assert.equal(evaluate('followOutput'),true,'downward navigation reaching bottom may resume follow');
  c.pauseAutoScrollOnKey({key:'Home',target:{isContentEditable:true}});
  assert.equal(evaluate('followOutput'),true,'editing shortcuts do not pause the transcript');
  c.pauseAutoScrollOnKey({key:' ',target:{closest:selector=>selector.includes('button')?{}:null}});
  assert.equal(evaluate('followOutput'),true,'Space activating a button is not a scrolling gesture');
  c.pauseAutoScrollOnKey({key:'ArrowUp',target:area});
  assert.equal(evaluate('followOutput'),false);
  c.pauseAutoScrollOnKey({key:'End',target:area});flushAll();
  assert.equal(evaluate('followOutput'),true);assert.equal(area.scrollTop,1000,'End explicitly returns to latest output');
} else if(scenario==='scroll-pointer') {
  c.autoScroll();
  c.beginChatPointerScroll({button:0,pointerType:'mouse',pointerId:1,clientX:995,clientY:100,target:area});
  assert.equal(evaluate('followOutput'),false,'a scrollbar grab pauses an already queued automatic scroll');
  area._scrollTop=100;area.scrollWrites=0;
  c.moveChatPointerScroll({pointerId:1,clientY:20});c.onChatScroll();c.autoScroll(true);flushAll();
  assert.equal(area.scrollTop,100,'even forced follow cannot fight an active scrollbar drag');
  assert.equal(area.scrollWrites,0);
  c.endChatPointerScroll({pointerId:1});
  assert.equal(evaluate('followOutput'),false,'releasing while reading earlier output preserves the pause');
  c.resumeAutoScroll();flushAll();
  assert.equal(evaluate('followOutput'),true);
  c.beginChatPointerScroll({button:0,pointerType:'mouse',pointerId:2,clientX:500,clientY:100,target:area});
  assert.equal(evaluate('followOutput'),true,'an ordinary text click is not enough to pause follow');
  c.moveChatPointerScroll({pointerId:99,clientY:20});
  assert.equal(evaluate('followOutput'),true,'another pointer does not own this gesture');
  c.moveChatPointerScroll({pointerId:2,clientY:20});
  assert.equal(evaluate('followOutput'),false,'a text-selection drag is a deliberate reading gesture');
  c.cancelChatScroll();c.autoScroll(true);flushAll();
  c.moveChatPointerScroll({pointerId:2,clientY:0});
  assert.equal(evaluate('followOutput'),true,'a cancelled old-view drag cannot pause a later view');
  c.getComputedStyle=()=>({direction:'rtl'});
  c.beginChatPointerScroll({button:0,pointerType:'mouse',pointerId:3,clientX:5,clientY:100,target:area});
  assert.equal(evaluate('followOutput'),false,'RTL scrollbars use the left gutter');
  c.cancelChatScroll();
  c.getComputedStyle=()=>({direction:'ltr'});
  area._scrollHeight=1300;c.resumeAutoScroll();flushAll();area._scrollTop=500;
  c.beginChatPointerScroll({button:0,pointerType:'mouse',pointerId:4,clientX:995,clientY:100,target:area});
  // A native scrollbar can consume all pointermove events. Its scroll
  // events still establish direction, and its final scroll may follow up.
  area._scrollTop=800;c.onChatScroll();
  assert.equal(area._metisScrollIntent,'down','native thumb scrolling determines direction without pointermove');
  c.endChatPointerScroll({pointerId:4});
  assert.equal(evaluate('followOutput'),false,'release before the native final scroll remains paused');
  area._scrollTop=1000;c.onChatScroll();
  assert.equal(evaluate('followOutput'),true,'the final native scroll after pointerup can resume at bottom');
  area._scrollTop=1000;
  c.beginChatPointerScroll({button:0,pointerType:'mouse',pointerId:5,clientX:995,clientY:100,target:area});
  area._scrollTop=980;area.scrollWrites=0;c.onChatScroll();
  assert.equal(area._metisScrollIntent,'up');
  c.endChatPointerScroll({pointerId:5});flushAll();
  assert.equal(evaluate('followOutput'),false,'a small upward drag is not undone by the near-bottom threshold');
  assert.equal(area.scrollTop,980);assert.equal(area.scrollWrites,0);
  c.onChatScroll();assert.equal(evaluate('followOutput'),false,'a late layout event cannot reverse the upward drag');
} else if(scenario==='scroll-touch') {
  c.autoScroll();
  c.beginChatTouchScroll({touches:[{clientY:100}]});
  c.moveChatTouchScroll({touches:[{clientY:103}]});
  assert.equal(evaluate('followOutput'),true,'touch jitter and taps do not pause follow');
  c.moveChatTouchScroll({touches:[{clientY:150}]});
  assert.equal(evaluate('followOutput'),false,'an upward-content touch gesture pauses follow');
  area._scrollTop=100;area.scrollWrites=0;c.onChatScroll();c.autoScroll(true);flushAll();
  assert.equal(area.scrollTop,100,'active touch scrolling wins over automatic follow');
  assert.equal(area.scrollWrites,0);
  c.endChatTouchScroll();
  assert.equal(evaluate('followOutput'),false,'touch release away from bottom preserves reading position');
  c.beginChatTouchScroll({touches:[{clientY:150}]});
  c.moveChatTouchScroll({touches:[{clientY:100}]});area._scrollTop=700;c.onChatScroll();
  assert.equal(evaluate('followOutput'),false,'follow remains paused during the live gesture');
  c.endChatTouchScroll();flushAll();
  assert.equal(evaluate('followOutput'),true,'touch scrolling back to bottom resumes after release');
  assert.equal(area.scrollTop,1000);
} else if(scenario==='history') {
  const history=Array.from({length:1000},(_,n)=>({role:n%2?'assistant':'user',content:'message '+n}));
  c.renderHistoryMessages(history);
  assert.equal(area.heightReads,0,'history reconstruction performs no per-message layout read');
  assert(area.insertions<history.length,'text history is inserted in batches');
  assert(layoutUpdates<=2,'empty-state layout is updated after reconstruction');
  flushAll();
  assert(area.heightReads<=2,'complete history needs only the final scroll measurement');
  assert.equal(area.scrollWrites,1);
  assert.match(area.innerHTML,/message 0/);assert.match(area.innerHTML,/message 999/);
} else if(scenario==='markdown-frontier') {
  const cut=c.streamMarkdownFreezeOffset;
  assert.equal(typeof cut,'function');
  for(const text of ['one','one\n\ntwo','one\n\ntwo\n\nthree'])assert.equal(cut(text),0,'retain two completed blocks and the unfinished tail');
  const prefix='one\n\ntwo\n\nthree\n\nfour';
  const offset=cut(prefix);assert(offset>0&&offset<prefix.length);
  assert.equal(c.formatContent(prefix.slice(0,offset))+c.formatContent(prefix.slice(offset)),c.formatContent(prefix),'freezing preserves block structure');
  for(const token of ['\x60\x60\x60js\n','$$\n',String.fromCharCode(92)+'[\n']) {
    const text=token+'one\n\ntwo\n\nthree\n\nfour';
    assert.equal(cut(text),0,'never freeze across an unclosed multi-line construct: '+token);
  }
  for(const text of [
    '- a\n- b\n\nparagraph\n\nthird\n\nfourth',
    '| A | B |\n| -- | -- |\n| 1 | 2 |\n\nparagraph\n\nthird\n\nfourth',
    '\x60\x60\x60js\nconst x = 1;\n\x60\x60\x60\n\nparagraph\n\nthird\n\nfourth',
    '$$x^2$$\n\nparagraph\n\nthird\n\nfourth',
    String.fromCharCode(92)+'[x^2'+String.fromCharCode(92)+']\n\nparagraph\n\nthird\n\nfourth',
  ]) {
    const at=cut(text);
    if(c.streamMarkdownNeedsFullRender(text))assert.equal(at,0,'display math keeps whole-document placeholder numbering');
    else assert(at>0,'completed prose, lists, tables and code may freeze');
    assert.equal(c.formatContent(text.slice(0,at))+c.formatContent(text.slice(at)),c.formatContent(text),'the safe frontier keeps lists/tables/fences/math intact');
  }
  // Every intermediate source frontier must agree with a complete parse;
  // checking only the final render would hide a mistaken early freeze.
  const tick=String.fromCharCode(96), backslash=String.fromCharCode(92);
  const fixtures=[
    '# Intro\n\nA **bold** paragraph with '+tick+'inline'+tick+' and a [link](https://example.test).\n\n'+
      '- one\n- two\n\n| A | B |\n| -- | -- |\n| 1 | 2 |\n\n'+
      tick.repeat(3)+'js\nconst x = \"<unsafe>\";\n\nconsole.log(x);\n'+tick.repeat(3)+'\n\nFinal.\n\nMore.\n\nEnd.',
    '> quote one\n> quote two\n\n1. first\n2. second\n\n$$\nx^2\n\n+y^2\n$$\n\n'+
      backslash+'[a+b'+backslash+']\n\nTail.\n\nMore.\n\nEnd.',
    'Prefix\n\n'+tick+'a\n\nspanning code\n\nb'+tick+'\n\nThird\n\nFourth\n\nFifth',
    Array.from({length:12},(_,n)=>'Paragraph '+n+' '+tick+'inline'+tick+' **value**.').join('\n\n'),
    tick+'$$'+tick+'\n\none\n\ntwo\n\nthree\n\n$$\n\nfour',
    tick+backslash+'['+tick+'\n\none\n\ntwo\n\nthree\n\n'+backslash+']\n\nfour',
    '$$$$\n\none\n\ntwo\n\nthree\n\n$$\n\nfour',
    backslash+'['+backslash+']\n\none\n\ntwo\n\nthree\n\n'+backslash+']\n\nfour',
    backslash+'[\n$$'+backslash+']\n$$\n\none\n\ntwo\n\nthree\n\nfour\n'+backslash+']',
    'Early paragraph\n\nSecond\n\nThird\n\nFourth\n\n$$late display$$\n\nEnd.',
    tick.repeat(3)+'js\nfirst code block\n'+tick.repeat(3)+'\n\none\n\ntwo\n\nthree\n\n$$\n'+
      tick.repeat(3)+'js\nsecond code block\n'+tick.repeat(3)+'\n$$\n\nfour',
    tick.repeat(3)+'js\nA\n'+tick.repeat(3)+'\n\none\n\ntwo\n\nCODEBLOCK0\n\nend',
    tick.repeat(3)+'js\nA\n'+tick.repeat(3)+'\n\none\n\ntwo\n\nthree\n\nCODEBLOCK0\n\n'+
      tick.repeat(3)+'js\nB\n'+tick.repeat(3)+'\n\nCODEBLOCK0\n\nend',
  ];
  let invalidatedPrefixes=0;
  for(const fixture of fixtures)for(const size of [1,7,32,129]) {
    let all='',frozenAt=0,frozenHTML='';
    for(let n=0;n<fixture.length;n+=size) {
      all+=fixture.slice(n,n+size);
      if(c.streamMarkdownNeedsFullRender(all)) {
        if(frozenAt>0)invalidatedPrefixes++;
        frozenAt=0;frozenHTML='';
        assert.equal(c.streamMarkdownFreezeOffset(all),0);
      } else {
        const safe=c.streamMarkdownFreezeOffset(all.slice(frozenAt));
        if(safe>0){frozenHTML+=c.formatContent(all.slice(frozenAt,frozenAt+safe));frozenAt+=safe;}
      }
      assert.equal(frozenHTML+c.formatContent(all.slice(frozenAt)),c.formatContent(all),
        'every streamed source frontier agrees with a complete parse (chunk '+size+', offset '+n+')');
    }
  }
  assert(invalidatedPrefixes>0,'late display math invalidates a prefix already frozen by earlier frames');
  // Exercise the production invalidation branch, including its DOM cache,
  // rather than validating only the source-frontier simulation above.
  for(const text of fixtures.slice(-4)) {
    const box=new Element();c.__fullRenderBox=box;
    evaluate('streamingMarkdownStates.set(__fullRenderBox,{text:"earlier source",frozenLength:10,frozenChildren:1})');
    c.renderStreamingMarkdown(box,text);
    assert.equal(evaluate('streamingMarkdownStates.has(__fullRenderBox)'),false,'whole-document fallback releases the old frozen DOM cache');
    assert.equal(box.innerHTML,c.formatContent(text),'late math and literal code placeholders keep their global indices');
  }
} else throw Error('unknown scenario '+scenario);
`
