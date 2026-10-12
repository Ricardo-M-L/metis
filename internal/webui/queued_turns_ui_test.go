package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestQueuedTurnCardAssetsMatchInteractiveContract(t *testing.T) {
	chatBytes, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	chat := string(chatBytes)
	for _, want := range []string{
		`class="queued-dock`,
		`class="queued-list`,
		`class="queued-card`,
		`function toggleQueuedTurnsDisclosure(`,
		`uiText('Adjust direction', '调整方向')`,
		`uiText('Send to the current turn now', '立即发送给当前任务')`,
		`uiText('Edit message', '编辑消息')`,
		`uiText('Open in side chat', '在侧边聊天中打开')`,
		`uiText('Close queue', '关闭排队')`,
		`fetch('/api/steer'`,
		`fetch('/api/fork'`,
		`function replaceComposerWithQueuedTurn(`,
	} {
		if !strings.Contains(chat, want) {
			t.Errorf("chat.js missing queued-card contract %q", want)
		}
	}
	if strings.Contains(chat, `class="queued-count"`) || strings.Contains(chat, `class="queued-item"`) {
		t.Error("chat.js still renders the old Queued-count pill UI")
	}

	styleBytes, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	style := string(styleBytes)
	for _, want := range []string{
		".queued-dock {",
		".queued-list {",
		".queued-card {",
		".queued-menu {",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing queued-card style %q", want)
		}
	}

	for _, name := range []string{"list-start", "list-end", "corner-down-left", "trash-2", "ellipsis", "pen-line", "message-circle-plus"} {
		asset, err := staticFS.ReadFile("static/icons/lucide/" + name + ".svg")
		if err != nil {
			t.Errorf("read %s icon: %v", name, err)
			continue
		}
		if !bytes.Contains(asset, []byte(`<svg xmlns="http://www.w3.org/2000/svg"`)) {
			t.Errorf("%s is not a standalone SVG icon asset", name)
		}
	}
}

func TestQueuedTurnCardBrowserInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const from = source.indexOf('function queuedTurnIcon(');
const to = source.indexOf('async function drainQueuedTurns(', from);
assert(from >= 0 && to > from);
const wrap = {style:{}, innerHTML:'', attrs:{}, setAttribute(k,v){this.attrs[k]=v;}};
const input = {value:'', focused:false, focus(){this.focused=true;}, setSelectionRange(){}};
let request = null, resumed = null, renderedAttachments = 0, messages = 0;
const c = {
  queuedTurns: [{text:'就是要再具体点', images:[]}], queuedSessionId:'S', queuedTurnMenuIndex:-1,
  queuedTurnPendingItem:null, turnRunning:true, runningSessionId:'S', currentSessionId:'S', attachments:[],
  document:{getElementById:id => id === 'queuedTurns' ? wrap : id === 'inputField' ? input : null,
    querySelector(){return null;}, addEventListener(){}},
  escHtml:String, escAttr:String, uiText:(en,zh)=>zh, renderAttachments(){renderedAttachments++;},
  onComposerInput(){}, showToast(){}, addMessage(){messages++;}, loadSessions:async()=>{},
  resumeSession:async id=>{resumed=id; c.currentSessionId=id;},
  fetch:async (url, options)=>{request={url,options}; return {ok:true,json:async()=>({sessionId:'branch'})};},
};
vm.createContext(c);
vm.runInContext(source.slice(from, to), c);

c.renderQueuedTurns();
assert.match(wrap.innerHTML, /class="queued-card/);
assert.match(wrap.innerHTML, /调整方向/);
assert.doesNotMatch(wrap.innerHTML, /Queued 1/);
c.toggleQueuedTurnMenu({stopPropagation(){}}, 0);
assert.match(wrap.innerHTML, /编辑消息/);
assert.match(wrap.innerHTML, /在侧边聊天中打开/);
assert.match(wrap.innerHTML, /关闭排队/);

// Editing must not turn an unsent composer draft into an automatically sent queue item.
input.value = '保留这个草稿';
c.editQueuedTurn({stopPropagation(){}}, 0);
assert.equal(input.value, '保留这个草稿');
assert.equal(c.queuedTurns.length, 1);
assert.equal(c.queuedTurns[0].text, '就是要再具体点');
input.value = '';
c.editQueuedTurn({stopPropagation(){}}, 0);
assert.equal(input.value, '就是要再具体点');
assert.equal(c.queuedTurns.length, 0);
assert.equal(input.focused, true);

(async () => {
  const steerItem = {text:'现在调整方向', images:[]};
  c.queuedTurns = [steerItem]; c.queuedSessionId = 'S'; c.currentSessionId = 'S'; c.runningSessionId = 'S';
  await c.steerQueuedTurn({stopPropagation(){}}, 0);
  assert.equal(request.url, '/api/steer');
  assert.deepEqual(JSON.parse(request.options.body), {sessionId:'S', input:'现在调整方向'});
  assert.equal(c.queuedTurns.length, 0);
  assert.equal(messages, 1);

  const sideItem = {text:'单独讨论这个', images:[]};
  c.queuedTurns = [sideItem]; c.queuedSessionId = 'S'; c.currentSessionId = 'S';
  await c.openQueuedTurnInSideChat({stopPropagation(){}}, 0);
  assert.equal(request.url, '/api/fork');
  assert.deepEqual(JSON.parse(request.options.body), {sessionId:'S', messageIndex:-1});
  assert.equal(resumed, 'branch');
  assert.equal(input.value, '单独讨论这个');
  assert.ok(renderedAttachments > 0);
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued turn browser harness: %v\n%s", err, out)
	}
}

func TestQueuedTurnDirectionReturnsToComposerWithoutSending(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const from = source.indexOf('function queuedTurnIcon(');
const to = source.indexOf('async function drainQueuedTurns(', from);
assert(from >= 0 && to > from);
function fixture() {
  const wrap = {style:{}, innerHTML:'', setAttribute(){}};
  const input = {value:'', focusCount:0, caret:null, focus(){this.focusCount++;},
    setSelectionRange(start,end){this.caret=[start,end];}};
  const state = {requests:[], messages:[], turns:[], toasts:[], changed:0, rendered:0, hasInput:true, workspacePending:false};
  const c = {
    queuedTurns:[{text:'帮我搜索清华',images:[]}], queuedSessionId:'S', currentSessionId:'S', runningSessionId:'S',
    queuedTurnMenuIndex:-1, queuedTurnPendingItem:null, drainingQueuedTurns:false, pendingAsk:null, pendingSessionId:null,
    turnRunning:true, attachments:[], event:{stopPropagation(){}},
    document:{getElementById:id => id === 'queuedTurns' ? wrap : id === 'inputField' && state.hasInput ? input : null,
      querySelector(){return null;}, addEventListener(){}},
    uiText:(en,zh)=>zh, escHtml:String, escAttr:String,
    isWorkspaceSelectionPending:()=>state.workspacePending,
    renderAttachments(){state.rendered++;}, onComposerInput(value){assert.equal(value,input);state.changed++;},
    showToast:text=>state.toasts.push(text), addMessage:(...args)=>state.messages.push(args),
    runTurnItem:async item=>{state.turns.push(item);}, loadSessions:async()=>{},
    fetch:async(url,options)=>{state.requests.push({url,options});return {ok:true,json:async()=>({})};},
  };
  vm.createContext(c);vm.runInContext(source.slice(from,to),c);
  const button = (index=0) => {
    c.renderQueuedTurns();
    const disclosure=(wrap.innerHTML.match(/<button\b[^>]*onclick="toggleQueuedTurnsDisclosure\(event\)"[^>]*>/)||[])[0];
    if(disclosure && /aria-expanded="false"/.test(disclosure)) c.toggleQueuedTurnsDisclosure(c.event);
    const matches=wrap.innerHTML.match(/<button\b[^>]*class="queued-steer"[^>]*>/g)||[];
    assert(matches[index],'main direction button exists');return matches[index];
  };
  const handler = (index=0) => {const match=button(index).match(/onclick="([^"]+)"/);assert(match);return match[1];};
  return {c,state,wrap,input,button,handler,click:code=>vm.runInContext(code || handler(),c)};
}
function notSent(f) {
  assert.equal(f.state.requests.length,0,'returning to composer cannot send HTTP requests');
  assert.equal(f.state.messages.length,0,'returning to composer cannot append a user message');
  assert.equal(f.state.turns.length,0,'returning to composer cannot start a turn');
}
(async()=>{
  // Execute the handler rendered on the actual main button, not only an edit helper.
  const f=fixture();const item=f.c.queuedTurns[0];await f.click();
  assert.equal(f.input.value,item.text,'direction restores the queued message into the composer');
  assert.equal(f.c.queuedTurns.length,0);assert.equal(f.c.queuedSessionId,null);
  assert.equal(f.input.focusCount,1);assert.deepEqual(f.input.caret,[item.text.length,item.text.length]);
  assert.equal(f.state.changed,1);assert.equal(f.state.rendered,1);notSent(f);
  assert.match(f.state.toasts[0],/已移回输入框/);

  const images=fixture();const selected={text:'包含所有图片',images:[{data:'first',name:'一.png'},{data:'second',name:'二.png'}]};
  const before={text:'前一条',images:[]},after={text:'后一条',images:[]};
  images.c.queuedTurns=[before,selected,after];await images.click(images.handler(1));
  assert.equal(images.input.value,selected.text);assert.deepEqual(JSON.parse(JSON.stringify(images.c.attachments)),selected.images);
  assert.notEqual(images.c.attachments,selected.images);assert.equal(images.c.queuedTurns[0],before);assert.equal(images.c.queuedTurns[1],after);
  assert.equal(images.c.queuedSessionId,'S');notSent(images);

  const imageOnly=fixture();imageOnly.c.queuedTurns=[{text:'',images:[{data:'image-only'}]}];
  assert.doesNotMatch(imageOnly.button(),/ disabled/);await imageOnly.click();
  assert.equal(imageOnly.c.attachments[0].data,'image-only');assert.equal(imageOnly.c.queuedTurns.length,0);notSent(imageOnly);

  const idle=fixture();idle.c.turnRunning=false;idle.c.runningSessionId=null;
  assert.doesNotMatch(idle.button(),/ disabled/);await idle.click();assert.equal(idle.input.value,'帮我搜索清华');notSent(idle);

  for (const draft of [{text:'  尚未发送的草稿  ',images:[]},{text:'',images:[{data:'unsent-image'}]}]) {
    const protectedDraft=fixture();const queued=protectedDraft.c.queuedTurns[0];protectedDraft.input.value=draft.text;
    protectedDraft.c.attachments=draft.images.slice();await protectedDraft.click();
    assert.equal(protectedDraft.input.value,draft.text);assert.equal(protectedDraft.c.queuedTurns.length,1);
    assert.equal(protectedDraft.c.queuedTurns[0],queued,'unsent draft must never replace or enter the automatic queue');
    assert.deepEqual(protectedDraft.c.attachments,draft.images);assert.match(protectedDraft.state.toasts[0],/草稿/);notSent(protectedDraft);
  }

  for (const configure of [
    f=>{f.c.queuedTurnPendingItem=f.c.queuedTurns[0];},
    f=>{f.c.queuedTurnPendingItem={text:'another pending item'};},
    f=>{f.c.drainingQueuedTurns=true;},f=>{f.c.pendingAsk={id:'ask'};},
    f=>{f.c.pendingSessionId='T';},f=>{f.state.workspacePending=true;},f=>{f.state.hasInput=false;}
  ]) {
    const blocked=fixture();const code=blocked.handler();const queued=blocked.c.queuedTurns[0];configure(blocked);
    assert.match(blocked.button(),/ disabled/,'main button reflects the same busy guard as its handler');
    await blocked.click(code);assert.equal(blocked.c.queuedTurns[0],queued);assert.equal(blocked.input.value,'');
    assert.equal(blocked.input.focusCount,0);notSent(blocked);
  }

  const foreign=fixture();const oldHandler=foreign.handler();foreign.c.currentSessionId='OTHER';
  await foreign.click(oldHandler);assert.equal(foreign.c.queuedTurns.length,1);assert.equal(foreign.input.value,'');notSent(foreign);
  const emptyView=fixture();const emptyHandler=emptyView.handler();emptyView.c.currentSessionId=null;
  await emptyView.click(emptyHandler);assert.equal(emptyView.c.queuedTurns.length,1);notSent(emptyView);
  const stale=fixture();const staleHandler=stale.handler();stale.c.queuedTurns=[];
  await stale.click(staleHandler);assert.equal(stale.input.value,'');notSent(stale);
  for (const index of [-1,1,0.5,NaN,'0']) {
    const invalid=fixture();invalid.c.editQueuedTurn(invalid.c.event,index);
    assert.equal(invalid.c.queuedTurns.length,1);assert.equal(invalid.input.value,'');notSent(invalid);
  }
  // Compatibility with extracted contexts that do not include optional navigation globals.
  const minimal=fixture();delete minimal.c.drainingQueuedTurns;delete minimal.c.pendingAsk;delete minimal.c.pendingSessionId;
  delete minimal.c.isWorkspaceSelectionPending;await minimal.click();assert.equal(minimal.input.value,'帮我搜索清华');notSent(minimal);

  // Immediate steering remains available, but only behind an explicit action in the menu.
  const immediate=fixture();immediate.button();immediate.c.toggleQueuedTurnMenu(immediate.c.event,0);
  const menu=(immediate.wrap.innerHTML.match(/<button\b[^>]*role="menuitem"[^>]*>[^]*?<\/button>/g)||[])
    .find(button=>button.includes('<span>立即发送给当前任务</span>'));
  assert(menu,'the menu must expose a clearly named immediate send action');
  const explicit=menu.match(/onclick="([^"]+)"/)[1];await immediate.click(explicit);
  assert.equal(immediate.state.requests.length,1);assert.equal(immediate.state.requests[0].url,'/api/steer');
  assert.equal(immediate.state.messages.length,1);assert.equal(immediate.c.queuedTurns.length,0);
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued direction browser harness: %v\n%s", err, out)
	}
}

func TestQueuedTurnDockDisclosureBrowserInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const from = source.indexOf('function queuedTurnIcon(');
const to = source.indexOf('async function drainQueuedTurns(', from);
const queueFrom = source.indexOf('function saveSessionQueue(');
const queueTo = source.indexOf('async function prepareDraftSession(', queueFrom);
assert(from >= 0 && to > from && queueFrom >= 0 && queueTo > queueFrom);
const item = text => ({text, images:[]});
function fixture(items=[item('第一条'),item('第二条'),item('第三条')]) {
  const wrap={style:{},innerHTML:'',attrs:{},setAttribute(k,v){this.attrs[k]=v;}};
  const input={value:'',focus(){},setSelectionRange(){}};
  const state={requests:[],messages:[],turns:[],stops:0,listeners:{}};
  const c={queuedTurns:items,queuedSessionId:'S',currentSessionId:'S',runningSessionId:'S',turnRunning:true,
    queuedTurnMenuIndex:-1,queuedTurnPendingItem:null,drainingQueuedTurns:false,attachments:[],sessionQueues:new Map(),
    event:{stopPropagation(){state.stops++;}},
    document:{getElementById:id=>id==='queuedTurns'?wrap:id==='inputField'?input:null,
      querySelector(){return null;},addEventListener(type,handler){state.listeners[type]=handler;}},
    uiText:(en,zh)=>zh,escHtml:String,escAttr:String,renderAttachments(){},onComposerInput(){},showToast(){},
    addMessage:(...args)=>state.messages.push(args),runTurnItem:async queued=>state.turns.push(queued),
    loadSessions:async()=>{},
    fetch:async(url,options)=>{state.requests.push({url,options});return {ok:true,json:async()=>({sessionId:'branch'})};},
  };
  vm.createContext(c);
  vm.runInContext(source.slice(queueFrom,queueTo),c);
  vm.runInContext(source.slice(from,to),c);
  function select(sessionId) {
    c.saveSessionQueue();c.currentSessionId=sessionId;c.runningSessionId=sessionId;c.restoreSessionQueue(sessionId);c.renderQueuedTurns();
  }
  c.resumeSession=async sessionId=>select(sessionId);
  const button=pattern=>{
    const buttons=wrap.innerHTML.match(/<button\b[^>]*>/g)||[];
    const found=buttons.find(markup=>pattern.test(markup));assert(found,'rendered button exists: '+pattern);return found;
  };
  const click=markup=>{
    const handler=markup.match(/onclick="([^"]+)"/);assert(handler,'rendered button has a handler');
    return vm.runInContext(handler[1],c);
  };
  const disclosure=()=>button(/onclick="toggleQueuedTurnsDisclosure\(event\)"/);
  const toggle=()=>click(disclosure());
  const rows=()=>Array.from(wrap.innerHTML.matchAll(/class="queued-card(?:\s[^"]*)?"[^>]*data-queued-index="(\d+)"/g),match=>Number(match[1]));
  function expanded(value) {
    assert.match(disclosure(),new RegExp('aria-expanded="'+value+'"'));
    if(value) {assert.match(wrap.innerHTML,/class="queued-list/);assert.equal(rows().length,c.queuedTurns.length);}
    else assert.equal(rows().length,0,'collapsed queue must not leave message rows in the DOM');
  }
  const menuAction=label=>{
    const actions=wrap.innerHTML.match(/<button\b[^>]*role="menuitem"[^>]*>[^]*?<\/button>/g)||[];
    const found=actions.find(markup=>markup.includes('<span>'+label+'</span>'));assert(found,'rendered menu action exists: '+label);return found;
  };
  c.renderQueuedTurns();
  return {c,state,wrap,input,items,select,button,click,disclosure,toggle,rows,expanded,menuAction};
}
function notSent(f) {
  assert.equal(f.state.requests.length,0,'disclosure cannot send an HTTP request');
  assert.equal(f.state.messages.length,0,'disclosure cannot append a user message');
  assert.equal(f.state.turns.length,0,'disclosure cannot start a turn');
}
(async()=>{
  const single=fixture([item('唯一消息')]);
  assert.match(single.wrap.innerHTML,/class="queued-dock/);
  assert.deepEqual(single.rows(),[0],'a single queued message is visible immediately');
  assert.doesNotMatch(single.wrap.innerHTML,/onclick="toggleQueuedTurnsDisclosure/,'single-message queue has no count header');

  const f=fixture();const original=f.c.queuedTurns;const draftImages=[{data:'unsent'}];
  f.input.value='尚未发送的草稿';f.c.attachments=draftImages;
  assert.match(f.wrap.innerHTML,/class="queued-dock/);assert.match(f.wrap.innerHTML,/3 条排队消息/);f.expanded(false);
  await f.toggle();f.expanded(true);assert.deepEqual(f.rows(),[0,1,2]);
  const previews=['第一条','第二条','第三条'].map(text=>f.wrap.innerHTML.indexOf(text));
  assert(previews[0]>=0 && previews[0]<previews[1] && previews[1]<previews[2],'expanded rows keep queue order');
  await f.click(f.button(/class="[^"]*\bqueued-more\b/));assert.match(f.wrap.innerHTML,/class="queued-menu/);
  await f.toggle();f.expanded(false);assert.equal(f.c.queuedTurnMenuIndex,-1,'collapsing also dismisses an open item menu');
  assert.equal(f.c.queuedTurns,original,'folding preserves the queue array');assert.deepEqual(f.c.queuedTurns,f.items);
  assert.equal(f.input.value,'尚未发送的草稿');assert.equal(f.c.attachments,draftImages);
  assert.equal(f.c.queuedTurnPendingItem,null);assert.equal(f.c.drainingQueuedTurns,false);notSent(f);

  // Use the actual queue save/restore functions when moving between sessions.
  const sessions=fixture();await sessions.toggle();sessions.expanded(true);const sItems=sessions.c.queuedTurns;
  sessions.select('T');sessions.c.queuedTurns.push(item('T一'),item('T二'),item('T三'));sessions.c.queuedSessionId='T';
  sessions.c.renderQueuedTurns();sessions.expanded(false);const tItems=sessions.c.queuedTurns;
  await sessions.toggle();sessions.expanded(true);await sessions.toggle();sessions.expanded(false);
  sessions.select('S');sessions.expanded(true);assert.equal(sessions.c.queuedTurns,sItems);
  sessions.select('T');sessions.expanded(false);assert.equal(sessions.c.queuedTurns,tItems);
  sessions.select('S');await sessions.toggle();sessions.expanded(false);
  sessions.select('T');await sessions.toggle();sessions.expanded(true);
  sessions.select('S');sessions.expanded(false);notSent(sessions);

  // Queue ownership falls back to the viewed session before an explicit owner is set.
  const owner=fixture();owner.c.queuedSessionId=null;owner.c.renderQueuedTurns();await owner.toggle();owner.expanded(true);
  owner.c.queuedSessionId='S';owner.c.renderQueuedTurns();owner.expanded(true);notSent(owner);

  // Changes in queue length preserve explicit choice until the queue becomes empty.
  const updates=fixture();updates.c.queuedTurns.push(item('第四条'));updates.c.renderQueuedTurns();updates.expanded(false);
  assert.match(updates.wrap.innerHTML,/4 条排队消息/);updates.c.removeQueuedTurn(1);updates.expanded(false);
  await updates.toggle();updates.expanded(true);updates.c.queuedTurns.push(item('新消息'));updates.c.renderQueuedTurns();updates.expanded(true);
  updates.c.removeQueuedTurn(1);updates.expanded(true);assert.match(updates.wrap.innerHTML,/3 条排队消息/);
  updates.c.removeQueuedTurn(1);updates.expanded(true);updates.c.removeQueuedTurn(1);
  assert.deepEqual(updates.rows(),[0]);assert.doesNotMatch(updates.wrap.innerHTML,/onclick="toggleQueuedTurnsDisclosure/);
  updates.c.queuedTurns.push(item('再次两条'));updates.c.renderQueuedTurns();updates.expanded(true);
  updates.c.removeQueuedTurn(1);updates.c.removeQueuedTurn(0);assert.equal(updates.wrap.innerHTML,'');
  updates.c.queuedTurns.push(item('重建一'),item('重建二'));updates.c.queuedSessionId='S';updates.c.renderQueuedTurns();updates.expanded(false);notSent(updates);

  const fresh=fixture([item('先一条')]);fresh.c.queuedTurns.push(item('新增第二条'));fresh.c.renderQueuedTurns();fresh.expanded(false);
  await fresh.toggle();await fresh.toggle();fresh.expanded(false);fresh.c.removeQueuedTurn(1);
  fresh.c.queuedTurns.push(item('收起状态再加一条'));fresh.c.renderQueuedTurns();fresh.expanded(false);
  await fresh.toggle();fresh.c.clearQueuedTurns(fresh.c.event);assert.equal(fresh.c.queuedTurns.length,0);assert.equal(fresh.wrap.innerHTML,'');
  fresh.c.queuedTurns.push(item('清空后第一条'),item('清空后第二条'));fresh.c.queuedSessionId='S';fresh.c.renderQueuedTurns();fresh.expanded(false);notSent(fresh);

  // Clearing T resets T alone; S keeps its choice and the same message objects.
  const reset=fixture();await reset.toggle();const resetS=reset.c.queuedTurns;reset.select('T');
  reset.c.queuedTurns.push(item('T一'),item('T二'));reset.c.queuedSessionId='T';reset.c.renderQueuedTurns();await reset.toggle();
  reset.c.clearQueuedTurns(reset.c.event);reset.c.queuedTurns.push(item('T新一'),item('T新二'));reset.c.queuedSessionId='T';reset.c.renderQueuedTurns();reset.expanded(false);
  reset.select('S');reset.expanded(true);assert.equal(reset.c.queuedTurns,resetS);notSent(reset);

  // Menus and their actions stop propagation and keep the surrounding queue open.
  const edit=fixture();await edit.toggle();const stops=edit.state.stops;
  await edit.click(edit.button(/class="[^"]*\bqueued-more\b/));edit.expanded(true);assert.equal(edit.state.stops,stops+1);
  edit.state.listeners.click({target:{closest(){return null;}}});edit.expanded(true);assert.equal(edit.c.queuedTurnMenuIndex,-1);
  await edit.click(edit.button(/class="[^"]*\bqueued-more\b/));
  await edit.click(edit.menuAction('编辑消息'));edit.expanded(true);assert.equal(edit.input.value,'第一条');assert.equal(edit.state.stops,stops+3);notSent(edit);

  const steer=fixture();await steer.toggle();await steer.click(steer.button(/class="[^"]*\bqueued-more\b/));
  const steerStops=steer.state.stops;await steer.click(steer.menuAction('立即发送给当前任务'));steer.expanded(true);
  assert.equal(steer.state.stops,steerStops+1);assert.equal(steer.state.requests.length,1);assert.equal(steer.state.requests[0].url,'/api/steer');

  const side=fixture();await side.toggle();await side.click(side.button(/class="[^"]*\bqueued-more\b/));
  const sideStops=side.state.stops;await side.click(side.menuAction('在侧边聊天中打开'));
  assert.equal(side.state.stops,sideStops+1);assert.equal(side.state.requests[0].url,'/api/fork');
  side.select('S');side.expanded(true);assert.equal(side.c.queuedTurns.length,2);
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued dock disclosure browser harness: %v\n%s", err, out)
	}
}

func TestQueuedTurnMenuRemainsOpenAfterListScroll(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const from = source.indexOf('function queuedTurnIcon(');
const to = source.indexOf('async function drainQueuedTurns(', from);
assert(from >= 0 && to > from);
const items = Array.from({length:7},(_,index)=>({text:'排队消息'+index,images:[]}));
const draftImages = [{data:'unsent-image',name:'草稿图片.png'}];
const input = {value:'尚未发送的草稿',focus(){},setSelectionRange(){}};
const state = {listeners:{},renders:0,anchorReads:0,requests:[],messages:[],turns:[],toasts:[],composerChanges:0,attachmentRenders:0};
let markup='',list=null,anchor={top:500,bottom:528,right:980};
const wrap = {style:{},setAttribute(){}};
Object.defineProperty(wrap,'innerHTML',{
  get(){return markup;},
  set(value){markup=value;state.renders++;list=value.includes('id="queuedTurnsList"')?{id:'queuedTurnsList',scrollTop:0}:null;},
});
const menu = {style:{},classList:{remove(){},toggle(){}},getBoundingClientRect(){return {width:220,height:160};}};
const more = {getBoundingClientRect(){state.anchorReads++;return anchor;}};
const lastCard = {getBoundingClientRect(){return anchor;},querySelector:selector=>selector==='.queued-more'?more:null};
const c = {queuedTurns:items,queuedSessionId:'S',currentSessionId:'S',runningSessionId:'S',turnRunning:true,
  queuedTurnMenuIndex:-1,queuedTurnPendingItem:null,drainingQueuedTurns:false,attachments:draftImages,
  event:{stopPropagation(){}},window:{innerWidth:1024,innerHeight:720},
  document:{
    getElementById:id=>id==='queuedTurns'?wrap:id==='queuedTurnsList'?list:id==='queuedTurnsToggle'?{focus(){}}:id==='inputField'?input:null,
    querySelector(selector){
      if(selector==='#queuedTurns .queued-menu' && markup.includes('class="queued-menu"'))return menu;
      if(selector==='.queued-card[data-queued-index="6"]' && markup.includes('data-queued-index="6"'))return lastCard;
      return null;
    },
    addEventListener(type,handler){state.listeners[type]=handler;},
  },
  uiText:(en,zh)=>zh,escHtml:String,escAttr:String,showToast:text=>state.toasts.push(text),
  onComposerInput(){state.composerChanges++;},renderAttachments(){state.attachmentRenders++;},
  addMessage:(...args)=>state.messages.push(args),runTurnItem:async queued=>state.turns.push(queued),
  fetch:async(url,options)=>{state.requests.push({url,options});return {ok:true,json:async()=>({})};},
};
vm.createContext(c);vm.runInContext(source.slice(from,to),c);
function clickRenderedButton(pattern) {
  const button=(markup.match(/<button\b[^>]*>/g)||[]).find(button=>pattern.test(button));
  assert(button,'the rendered button exists');const handler=button.match(/onclick="([^"]+)"/);assert(handler);
  vm.runInContext(handler[1],c);
}
c.renderQueuedTurns();
clickRenderedButton(/onclick="toggleQueuedTurnsDisclosure\(event\)"/);
list.scrollTop=240;
clickRenderedButton(/onclick="toggleQueuedTurnMenu\(event,6\)"/);
assert.equal(c.queuedTurnMenuIndex,6);assert.match(markup,/class="queued-menu"/);
assert.equal(list.scrollTop,240,'opening the last row menu restores the scrolled list position');
const openMarkup=markup,renders=state.renders,anchorReads=state.anchorReads,oldTop=menu.style.top;
assert.equal(typeof state.listeners.scroll,'function','execute the actual registered scroll listener');
anchor={top:450,bottom:478,right:980};
state.listeners.scroll({target:list});
assert.equal(c.queuedTurnMenuIndex,6,'restoring scrollTop must not dismiss the last row menu');
assert.match(markup,/class="queued-menu"/);assert.equal(markup,openMarkup);
assert.equal(state.renders,renders,'list scroll repositions the menu without redrawing the queue');
assert(state.anchorReads>anchorReads,'scroll remeasures the menu anchor');assert.notEqual(menu.style.top,oldTop);
assert.equal(c.queuedTurns,items);assert.deepEqual(c.queuedTurns.map(item=>item.text),Array.from({length:7},(_,index)=>'排队消息'+index));
assert.equal(input.value,'尚未发送的草稿');assert.equal(c.attachments,draftImages);
assert.equal(c.queuedSessionId,'S');assert.equal(c.queuedTurnPendingItem,null);assert.equal(c.drainingQueuedTurns,false);
assert.equal(state.requests.length,0);assert.equal(state.messages.length,0);assert.equal(state.turns.length,0);
assert.equal(state.toasts.length,0);assert.equal(state.composerChanges,0);assert.equal(state.attachmentRenders,0);
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued menu scroll browser harness: %v\n%s", err, out)
	}
}
