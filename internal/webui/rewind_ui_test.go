package webui

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDesktopRewindComposerEntry(t *testing.T) {
	b, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start, end := strings.Index(s, "const COMPOSER_COMMANDS = ["), strings.Index(s, "const COMPOSER_ADD_ACTIONS = [")
	if start < 0 || end <= start {
		t.Fatal("command catalog unavailable")
	}
	catalog := s[start:end]
	if !strings.Contains(catalog, "name: '/rewind'") || strings.Contains(catalog, "name: '/checkpoint'") {
		t.Fatal("everyday catalog should expose rewind, not the checkpoint compatibility control")
	}
	for _, want := range []string{"name !== '/checkpoint'", "case '/checkpoint': await runCheckpointControl(input)", "case '/rewind': await openRewindDialog(input)", "case 'rewind': await openRewindDialog('', document.getElementById('attachmentBtn') || trigger)", "/^\\/(?:rewind|checkpoint)(?:\\s|$)/i.test(text)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing local rewind/compatibility routing %q", want)
		}
	}
}

func TestDesktopRewindInteractionOwnershipAndDrafts(t *testing.T) {
	b, err := staticFS.ReadFile("static/rewind.js")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, desktopStatsNode(t), "-e", rewindUIFixture)
	cmd.Stdin = bytes.NewReader(b)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rewind browser interaction fixture: %v\n%s", err, out)
	}
}

const rewindUIFixture = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const clone=x=>JSON.parse(JSON.stringify(x));
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const escape=x=>String(x).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
function fixture(draft='') {
 const calls=[],toasts=[],renders=[],saved=[],overlays=[];let focused=null;
 const element=(name='')=>({name,isConnected:true,scrollTop:0,value:'',disabled:false,innerHTML:'',listeners:{},focus(){focused=this},addEventListener(n,fn){this.listeners[n]=fn},remove(){this.isConnected=false},scrollTo(o){this.scrollTop=o.top},querySelector(selector){if(selector==='form')return this.form??=element('form');if(selector==='.rewind-points')return this.list??=element('list');return (this.nodes??={})[selector]??=element(selector)},querySelectorAll(){return [this.querySelector('.rewind-close'),this.querySelector('cancel'),this.querySelector('confirm')]}});
 const input=element('input');input.value=draft;const trigger=element('trigger');
 const c={console,Promise,AbortController,attachments:[],currentSessionId:'A',activeWorkspaceId:'W',resumeSessionGeneration:1,subAgentConversationState:null,
  messages:[{role:'user',content:'old'}],backgroundContinuationGeneration:0,backgroundContinuationGenerations:new Map(),resumeStatesBySession:new Map([['A',{available:true,token:'old'}]]),pausedQueueSessions:new Set(),statusRequestGeneration:4,isViewedTurnRunning:()=>false,
  showToast:s=>toasts.push(s),escHtml:escape,autoResize(){},clearTodoPlan(){},loadSessions:async()=>{},loadSessionStatsbar(){},saveComposerDraftForView(){saved.push(input.value)},renderHistoryMessages:h=>renders.push(clone(h)),
  document:{body:{appendChild:n=>overlays.push(n)},get activeElement(){return focused||trigger},createElement:()=>element('overlay'),getElementById:id=>id==='inputField'?input:null},
  fetch:(url,options={})=>new Promise((resolve,reject)=>calls.push({url,options,body:options.body?JSON.parse(options.body):null,resolve,reject}))};
 vm.createContext(c);vm.runInContext(source,c);
 const state=()=>vm.runInContext('rewindDialog',c);
 const reply=(index,body,status=200)=>calls[index].resolve({ok:status>=200&&status<300,status,json:async()=>clone(body)});
 const data=(available=true)=>({sessionId:'A',revision:'r1',checkpoint:{enabled:true,sparse:true},points:[{turn:2,prompt:'<latest>',codeAvailable:available,codeUnavailableReason:'Bash无法确定写入范围',latestEdit:true},{turn:1,prompt:'earlier',codeAvailable:false,codeUnavailableReason:'没有代码快照'}]});
 const open=async(available=true)=>{const p=c.openRewindDialog();reply(calls.length-1,data(available));await p;return state()};
 return {c,calls,toasts,renders,saved,input,trigger,overlays,state,reply,data,open,get focused(){return focused}};
}
(async()=>{
 {
  const f=fixture();await f.open(false);
  assert.match(f.state().overlay.innerHTML,/value="both"[^>]* disabled/);assert.match(f.state().overlay.innerHTML,/value="code"[^>]* disabled/);
  assert.match(f.state().overlay.innerHTML,/Bash无法确定写入范围/);assert.equal(f.state().mode,'conversation');
  f.c.selectRewindMode('code');assert.equal(f.state().mode,'conversation','unavailable code cannot be selected even by a handler call');
  const p=f.c.confirmRewind();await f.c.confirmRewind();assert.equal(f.calls.length,2,'double submit issues one mutation');
  assert.deepEqual(f.calls[1].body,{sessionId:'A',turn:2,mode:'conversation',revision:'r1'});
  f.reply(1,{sessionId:'A',changed:true,messages:[],prefill:'old prompt'});await p;
  assert.equal(f.input.value,'old prompt');assert.equal(f.renders.length,1);assert.equal(f.state(),null);
 }
 {
  const f=fixture('my existing draft');await f.open();f.c.selectRewindMode('code');
  const p=f.c.confirmRewind();f.reply(1,{sessionId:'A',changed:true,messages:[{role:'user',content:'unchanged'}],result:{codeRestored:true}});await p;
  assert.equal(f.input.value,'my existing draft');assert.equal(f.renders.length,0,'code-only preserves transcript DOM and scroll');assert.equal(f.saved.length,0);assert.equal(f.state(),null);assert.equal(f.c.resumeStatesBySession.has('A'),false,'code-only also invalidates the old resume token');assert.equal(f.c.statusRequestGeneration,5,'older status responses cannot revive the old token');assert.equal(f.c.pausedQueueSessions.has('A'),true);
 }
 {
  const f=fixture('my existing draft');await f.open();const p=f.c.confirmRewind();
  f.reply(1,{sessionId:'A',changed:true,messages:[],prefill:'restored prompt'});await p;
  assert.equal(f.input.value,'my existing draft','existing draft is not silently replaced');assert.equal(f.state().completed.prefill,'restored prompt');
  f.c.useRewindPrompt();assert.equal(f.input.value,'restored prompt','explicit user choice applies recovered prompt');assert.equal(f.state(),null);
 }
 {
  const f=fixture();const p=f.c.openRewindDialog();f.c.currentSessionId='B';f.c.resumeSessionGeneration++;
  f.reply(0,f.data());await p;assert.equal(f.state(),null);assert.equal(f.overlays[0].isConnected,false);assert.equal(f.toasts.length,0);
 }
 {
  const f=fixture('keep');await f.open();const p=f.c.confirmRewind();f.c.currentSessionId='B';f.c.resumeSessionGeneration++;
  f.reply(1,{sessionId:'A',changed:true,messages:[],prefill:'wrong session'});await p;
  assert.equal(f.input.value,'keep');assert.equal(f.renders.length,0);assert.equal(f.state(),null);
 }
 {
  const f=fixture();await f.open();const p=f.c.confirmRewind();f.reply(1,{error:'workspace is busy; stop running turn'},409);await p;
  assert.equal(f.state().pending,false);assert.match(f.state().error,/停止任务/);assert.equal(f.renders.length,0);
  const retry=f.c.confirmRewind();f.reply(2,{error:'stale revision'},409);await retry;assert.match(f.state().error,/重新选择/);assert.equal(f.renders.length,0);
 }
 for (const mode of ['code','conversation','both']) {
  const f=fixture('keep my draft');f.c.attachments=[{name:'keep.png'}];await f.open();f.c.selectRewindMode(mode);
  const p=f.c.confirmRewind();f.reply(1,{sessionId:'A',error:'late journal failure',partial:true,reloadRequired:true},500);await tick();
  assert.equal(f.calls[2].url,'/api/sessions/A','partial result reloads the authoritative saved transcript');
  assert.equal(f.c.resumeStatesBySession.has('A'),false,'partial result cannot leave a resumable old token');assert.equal(f.c.pausedQueueSessions.has('A'),true);
  assert.equal(f.input.value,'keep my draft');assert.equal(f.c.attachments[0].name,'keep.png');
  const actual=[{role:'user',content:'actual durable state'}];f.reply(2,{session:{id:'A'},messages:actual});await p;
  assert.deepEqual(f.renders,[actual]);assert.equal(f.input.value,'keep my draft');assert.equal(f.c.attachments[0].name,'keep.png');
  assert.equal(f.toasts.length,0,'partial recovery never claims success');assert.match(f.state().error,/恢复可能已执行/);assert.equal(f.state().requiresReopen,true);
  assert.match(f.state().overlay.innerHTML,/type="submit" class="rewind-confirm" disabled/);
  f.c.selectRewindTurn(1);assert.match(f.state().error,/恢复可能已执行/,'changing a point cannot hide the partial failure');await f.c.confirmRewind();assert.equal(f.calls.length,3,'old panel cannot reapply after a partial restore');
 }
 for (const mismatch of [false,true]) {
  const f=fixture('keep');await f.open();const p=f.c.confirmRewind();f.reply(1,{sessionId:'A',error:'I/O',partial:true,reloadRequired:true},500);await tick();
  f.reply(2,mismatch?{session:{id:'B'},messages:[]}:{error:'unavailable'},mismatch?200:503);await p;
  assert.equal(f.renders.length,0);assert.equal(f.input.value,'keep');assert.match(f.state().error,/刷新会话/);assert.equal(f.toasts.length,0);assert.equal(f.c.resumeStatesBySession.has('A'),false);
 }
 {
  const f=fixture('A draft');await f.open();const p=f.c.confirmRewind();f.reply(1,{sessionId:'A',error:'I/O',partial:true,reloadRequired:true},500);await tick();
  f.c.currentSessionId='B';f.c.resumeSessionGeneration++;f.input.value='B draft';
  f.reply(2,{session:{id:'A'},messages:[{role:'user',content:'must stay offscreen'}]});await p;
  assert.equal(f.renders.length,0);assert.equal(f.input.value,'B draft');assert.equal(f.state(),null);assert.equal(f.toasts.length,0);
 }
 {
  const f=fixture();f.c.subAgentConversationState={agentId:'child'};await f.c.openRewindDialog();assert.equal(f.calls.length,0);assert.match(f.toasts[0],/主代理管理/);
  f.c.subAgentConversationState=null;f.c.isViewedTurnRunning=()=>true;await f.c.openRewindDialog();assert.equal(f.calls.length,0);assert.match(f.toasts[1],/停止当前任务/);
 }
 {
  const f=fixture();await f.open();const a=f.state().overlay.querySelectorAll();a.at(-1).focus();let prevented=false;
  f.c.rewindDialogKeydown({key:'Tab',shiftKey:false,preventDefault(){prevented=true}});assert.equal(prevented,true);assert.equal(f.focused,a[0]);
  f.c.rewindDialogKeydown({key:'Escape',preventDefault(){},stopPropagation(){}});assert.equal(f.state(),null);assert.equal(f.focused,f.trigger);
 }
 {
  const f=fixture();await f.open();f.c.subAgentConversationState={agentId:'child'};f.c.syncRewindView();assert.equal(f.state(),null);
 }
 {
  const f=fixture();const p=f.c.openRewindDialog();const bad=f.data();bad.sessionId='B';f.reply(0,bad);await p;
  assert.equal(f.state().points.length,0);assert.match(f.state().error,/不一致/);assert.doesNotMatch(f.state().overlay.innerHTML,/此会话还没有可回退的消息/,'unknown GET failure is not an empty timeline');await f.c.confirmRewind();assert.equal(f.calls.length,1);
 }
})().catch(error=>{console.error(error);process.exitCode=1});
`

func TestDesktopRewindCommandsStayLocalWhileBusy(t *testing.T) {
	b, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, desktopStatsNode(t), "-e", rewindCommandFixture)
	cmd.Stdin = bytes.NewReader(b)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rewind local command fixture: %v\n%s", err, out)
	}
}

const rewindCommandFixture = `
const assert=require('node:assert/strict'),vm=require('node:vm'),source=require('node:fs').readFileSync(0,'utf8');
function extract(a,b){const start=source.indexOf(a),end=source.indexOf(b,start+a.length);assert(start>=0&&end>start);return source.slice(start,end)}
const input={value:''},calls=[],toasts=[];
const c={console,currentSessionId:'S',activeWorkspaceId:'W',runningSessionId:'S',turnRunning:true,pendingAsk:{id:'ask'},attachments:[],
 COMPOSER_COMMANDS:[{name:'/rewind'}],isViewedTurnRunning:()=>true,parallelTurnsEnabled:()=>false,
 document:{getElementById:()=>input},autoResize(){},closeCommandMenu(){},showToast:s=>toasts.push(s),uiText:(en,zh)=>zh,
 executeComposerCommand:null,openRewindDialog:async arg=>calls.push({rewind:arg}),runCheckpointControl:async arg=>calls.push({checkpoint:arg}),
 submitBusyInput:async()=>{throw new Error('local restore was queued to the model')},submitAskAnswer:async()=>{throw new Error('local restore became a model-question answer')},
 drainingQueuedTurns:false,queuedSessionId:null,queuedTurns:[],renderQueuedTurns(){},
};vm.createContext(c);
vm.runInContext(extract('async function sendMessage(', 'async function submitBusyInput('),c);
vm.runInContext(extract('async function runTurnItem(', 'const MESSAGE_ACTION_ICONS'),c);
vm.runInContext(extract('async function executeComposerCommand(', 'async function runSessionCommand('),c);
(async()=>{
 input.value='/rewind';await c.sendMessage();assert.deepEqual(calls,[{rewind:''}]);assert.equal(input.value,'');
 input.value='/checkpoint off';await c.sendMessage();assert.deepEqual(calls.at(-1),{checkpoint:'off'},'hidden compatibility command remains local');
 input.value='/rewind';c.attachments=[{name:'keep.png'}];await c.sendMessage();assert.equal(input.value,'/rewind');assert.equal(c.attachments.length,1);assert.equal(calls.length,2,'invalid attachment command preserves draft and never reaches runtime');
 c.attachments=[];c.pendingAsk=null;await c.runTurnItem({text:'/rewind last',images:[]});assert.deepEqual(calls.at(-1),{rewind:'last'},'legacy queue also remains local');
 await c.runTurnItem({text:'/checkpoint status',images:[]});assert.deepEqual(calls.at(-1),{checkpoint:'status'});
})().catch(err=>{console.error(err);process.exitCode=1});
`
