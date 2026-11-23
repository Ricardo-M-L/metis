package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestQueuedTurnDirectSendBrowserInteractions(t *testing.T) {
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
const to = source.indexOf('async function syncViewedSessionHistory(', from);
assert(from >= 0 && to > from);
function fixture() {
  const wrap={style:{},innerHTML:'',setAttribute(){}};
  const input={value:'',focus(){},setSelectionRange(){}};
  const state={requests:[],messages:[],toasts:[],turns:[],response:null};
  const c={queuedTurns:[{text:'直接调整当前任务',images:[]}],queuedSessionId:'S',currentSessionId:'S',
    runningSessionId:'S',turnRunning:true,queuedTurnMenuIndex:-1,queuedTurnPendingItem:null,
    drainingQueuedTurns:false,pendingAsk:null,pendingSessionId:null,attachments:[],
    document:{getElementById:id=>id==='queuedTurns'?wrap:id==='inputField'?input:null,
      querySelector(){return null;},addEventListener(){}},
    uiText:(en,zh)=>zh,escHtml:String,escAttr:String,event:{stopPropagation(){}},
    showToast:text=>state.toasts.push(text),addMessage:(...args)=>state.messages.push(args),
    renderAttachments(){},onComposerInput(){},loadSessions:async()=>{},
    syncTurnControls(){},renderSessions(){},renderTodoPlan(){},stopRequestPending:false,
    fetch:async(url,options)=>{
      state.requests.push({url,body:JSON.parse(options.body)});
      if(state.response)return state.response;
      return {ok:true,json:async()=>({})};
    }};
  vm.createContext(c);vm.runInContext(source.slice(from,to),c);
  c.isViewedTurnRunning=()=>c.turnRunning;c.isSessionQueuePaused=()=>false;
  c.saveSessionQueue=()=>({});c.runTurnItem=async item=>{state.turns.push(item);return false;};
  const runningFrom=source.indexOf('function setTurnRunning(');
  const runningTo=source.indexOf('// Detach transient DOM state',runningFrom);
  assert(runningFrom>=0 && runningTo>runningFrom);vm.runInContext(source.slice(runningFrom,runningTo),c);
  function markup(){
    c.renderQueuedTurns();
    if(wrap.innerHTML.includes('aria-expanded="false" aria-controls="queuedTurnsList"'))c.toggleQueuedTurnsDisclosure(c.event);
    return wrap.innerHTML;
  }
  function button(index=0){
    const buttons=markup().match(/<button\b[^>]*class="[^"]*\bqueued-send\b[^"]*"[^>]*>[^]*?<\/button>/g)||[];
    assert(buttons[index],'the queue card must directly expose an immediate-send arrow');
    return buttons[index];
  }
  const handler=(index=0)=>button(index).match(/onclick="([^"]+)"/)[1];
  return {c,state,wrap,input,markup,button,handler,click:code=>vm.runInContext(code||handler(),c)};
}
(async()=>{
  const direct=fixture();const markup=direct.markup();
  const send=direct.button();
  assert.match(send,/aria-label="立即发送给当前任务"/);
  assert.match(send,/title="立即发送给当前任务"/);
  assert.match(send,/icons\/lucide\/arrow-up\.svg/);
  assert.doesNotMatch(send,/ disabled/);
  assert(markup.indexOf('queued-send')<markup.indexOf('class="queued-steer"'),'send must precede the edit button');
  await direct.click();
  assert.equal(direct.state.requests.length,1);
  assert.equal(direct.state.requests[0].url,'/api/steer');
  assert.deepEqual(direct.state.requests[0].body,{sessionId:'S',input:'直接调整当前任务'});
  assert.equal(direct.c.queuedTurns.length,0);assert.equal(direct.c.queuedSessionId,null);
  assert.equal(direct.state.messages.length,1);

  // Run the actual rendered handler twice while the first request is pending.
  const pending=fixture();pending.c.queuedTurns.push({text:'保留后一条',images:[]});
  let release;pending.state.response=new Promise(resolve=>{release=resolve;});
  const firstHandler=pending.handler();const secondHandler=pending.handler(1);
  const sending=pending.click(firstHandler);
  assert.equal(pending.c.queuedTurns.length,2,'a pending request cannot remove a queue item');
  assert.match(pending.button(),/ disabled/);assert.match(pending.button(1),/ disabled/);
  for(const klass of ['queued-steer','queued-remove','queued-more']) {
    const buttons=pending.markup().match(new RegExp('<button\\b[^>]*class="[^"]*\\b'+klass+'\\b[^"]*"[^>]*>','g'))||[];
    assert.equal(buttons.length,2);for(const button of buttons)assert.match(button,/ disabled/);
  }
  await pending.click(firstHandler);await pending.click(secondHandler);
  pending.c.editQueuedTurn(pending.c.event,1);pending.c.removeQueuedTurn(1);
  assert.equal(pending.state.requests.length,1);assert.equal(pending.c.queuedTurns.length,2);
  assert.equal(pending.input.value,'');
  release({ok:true,json:async()=>({})});await sending;
  assert.equal(pending.c.queuedTurns.length,1);assert.equal(pending.c.queuedTurns[0].text,'保留后一条');
  assert.equal(pending.c.queuedTurnPendingItem,null);assert.doesNotMatch(pending.button(),/ disabled/);

  for(const status of [409,500]) {
    const failure=fixture();const item=failure.c.queuedTurns[0];
    failure.state.response={ok:false,status,json:async()=>({error:'fixture '+status})};
    await failure.click();assert.equal(failure.c.queuedTurns[0],item);
    assert.equal(failure.c.queuedSessionId,'S');assert.equal(failure.state.messages.length,0);
    assert.equal(failure.c.queuedTurnPendingItem,null);assert.match(failure.state.toasts[0],/消息仍在队列中/);
    assert.doesNotMatch(failure.button(),/ disabled/);
  }
  const rejected=fixture();const rejectedItem=rejected.c.queuedTurns[0];
  rejected.state.response=Promise.reject(new Error('fixture network failure'));
  await rejected.click();assert.equal(rejected.c.queuedTurns[0],rejectedItem);assert.equal(rejected.state.messages.length,0);

  // A completed old turn cannot drain the item while its immediate-send acknowledgement is pending.
  const finishing=fixture();const finishingItem=finishing.c.queuedTurns[0];
  let rejectSend;finishing.state.response=new Promise(resolve=>{rejectSend=resolve;});
  const attempted=finishing.click();finishing.c.setTurnRunning(false);
  await finishing.c.drainQueuedTurns();
  assert.equal(finishing.state.turns.length,0,'a pending immediate send cannot also start a new queued turn');
  assert.equal(finishing.c.queuedTurns[0],finishingItem);
  rejectSend({ok:false,status:409,json:async()=>({error:'old turn completed'})});await attempted;
  assert.equal(finishing.c.queuedTurns[0],finishingItem,'409 must preserve the item even when the old turn finished before its acknowledgement');
  assert.equal(finishing.state.turns.length,0);assert.equal(finishing.state.messages.length,0);

  const image=fixture();image.c.queuedTurns[0].images=[{data:'fixture-image'}];
  assert.match(image.button(),/ disabled/);await image.click();assert.equal(image.state.requests.length,0);
  const idle=fixture();idle.c.turnRunning=false;idle.c.runningSessionId=null;
  assert.match(idle.button(),/ disabled/);await idle.click();assert.equal(idle.state.requests.length,0);
  const otherRunning=fixture();otherRunning.c.runningSessionId='OTHER';
  assert.match(otherRunning.button(),/ disabled/);await otherRunning.click();assert.equal(otherRunning.state.requests.length,0);
  const foreignView=fixture();foreignView.c.currentSessionId='OTHER';
  assert.equal(foreignView.markup(),'','a queue cannot render in another session');
  const staleView=fixture();const oldHandler=staleView.handler();staleView.c.currentSessionId='OTHER';
  await staleView.click(oldHandler);assert.equal(staleView.state.requests.length,0);assert.equal(staleView.c.queuedTurns.length,1);
  const draining=fixture();draining.c.drainingQueuedTurns=true;
  assert.match(draining.button(),/ disabled/);await draining.click();assert.equal(draining.state.requests.length,0);

  // Runtime snapshots must update the already rendered inline button without an extra user action.
  const restored=fixture();restored.button();restored.c.setTurnRunning(false);
  assert.match(restored.wrap.innerHTML,/<button\b[^>]*class="[^"]*\bqueued-send\b[^"]*"[^>]* disabled/);
  restored.c.setTurnRunning(true,'S');
  assert.doesNotMatch(restored.wrap.innerHTML,/<button\b[^>]*class="[^"]*\bqueued-send\b[^"]*"[^>]* disabled/);
  restored.c.setTurnRunning(true,'OTHER');
  assert.match(restored.wrap.innerHTML,/<button\b[^>]*class="[^"]*\bqueued-send\b[^"]*"[^>]* disabled/);
  restored.c.setTurnRunning(true,'S');await restored.click();assert.equal(restored.state.requests.length,1);

  const menu=fixture();menu.c.toggleQueuedTurnMenu(menu.c.event,0);
  assert.match(menu.wrap.innerHTML,/<span>立即发送给当前任务<\/span>/,'the menu keeps its explicit immediate-send action');
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued direct-send browser harness: %v\n%s", err, out)
	}
}
