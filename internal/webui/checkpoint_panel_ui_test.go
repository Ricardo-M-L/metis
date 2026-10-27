package webui

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCheckpointSessionSwitchMarkup(t *testing.T) {
	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	content := string(html)
	for _, want := range []string{`id="checkpointBtn"`, `role="switch"`, `onclick="toggleCheckpointControl(event)"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing compact session switch %q", want)
		}
	}
	for _, removed := range []string{`id="checkpointPanel"`, `id="checkpointRoots"`, `id="checkpointBrowse"`, `id="checkpointEnable"`} {
		if strings.Contains(content, removed) {
			t.Fatalf("project scope UI must be removed: %s", removed)
		}
	}
}

func TestCheckpointSessionSwitchBrowserInteractions(t *testing.T) {
	source, err := staticFS.ReadFile("static/checkpoint.js")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, desktopStatsNode(t), "-e", checkpointSessionSwitchFixture)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkpoint session switch: %v\n%s", err, output)
	}
}

const checkpointSessionSwitchFixture = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const clone=value=>JSON.parse(JSON.stringify(value));
const tick=()=>new Promise(resolve=>setImmediate(resolve));
function element(){return {textContent:'',value:'',disabled:false,dataset:{},attrs:{},classList:{toggle(){},add(){},remove(){}},setAttribute(k,v){this.attrs[k]=String(v)},removeAttribute(k){delete this.attrs[k]},focus(){}}}
function fixture(sid='A'){
 const nodes={checkpointBtn:element(),inputField:element()};nodes.inputField.value='keep my draft';
 const calls=[],toasts=[],listeners={},creation=[];
 const c={currentSessionId:sid,activeWorkspaceId:'W',resumeSessionGeneration:1,attachments:[{name:'keep.png'}],console,URLSearchParams,Promise,setTimeout,clearTimeout,uiText:(en,zh)=>zh,showToast:s=>toasts.push(s),document:{documentElement:{lang:'zh-CN'},getElementById:id=>nodes[id]||null,addEventListener:(name,fn)=>{(listeners[name]??=[]).push(fn)}},fetch:(url,options={})=>new Promise((resolve,reject)=>calls.push({url,options,body:options.body?JSON.parse(options.body):null,resolve,reject})),prepareDraftSession:options=>new Promise(resolve=>creation.push({options,resolve}))};
 vm.createContext(c);vm.runInContext(source,c);
 const reply=(i,data,status=200)=>calls[i].resolve({ok:status>=200&&status<300,status,json:async()=>clone(data)});
 return {c,nodes,calls,toasts,listeners,reply,creation};
}
const state=(sid='A',enabled=true)=>({sessionId:sid,workspace:'/projects/shared',enabled,mode:enabled?'on':'off',reason:'',revision:'r1'});
const watchdog=setTimeout(()=>{console.error('checkpoint switch did not settle');process.exit(1)},18000);
(async()=>{
 {
  const f=fixture();f.c.renderCheckpointControl(state());
  assert.equal(f.nodes.checkpointBtn.textContent,'检查点：开');assert.equal(f.nodes.checkpointBtn.attrs['aria-checked'],'true','the control exposes switch state');
  assert.equal(f.nodes.checkpointBtn.disabled,false);
  const switching=f.c.toggleCheckpointControl();
  assert.equal(f.calls.length,1);assert.deepEqual(f.calls[0].body,{sessionId:'A',command:'off'},'an explicit session setting never sends project roots or workspace-global mutation');
  assert.equal(f.nodes.checkpointBtn.disabled,true);await f.c.toggleCheckpointControl();assert.equal(f.calls.length,1,'double click cannot issue a second mutation');
  f.reply(0,{checkpoint:state('A',false),message:'CLI details '.repeat(100)});await switching;
  assert.equal(f.nodes.checkpointBtn.textContent,'检查点：关');assert.equal(f.nodes.checkpointBtn.attrs['aria-checked'],'false');assert.equal(f.nodes.checkpointBtn.disabled,false);
  assert.deepEqual(f.toasts,['检查点已关闭'],'only a short operation result appears');
 }
 {
  const f=fixture();f.c.renderCheckpointControl(state());const a=f.c.toggleCheckpointControl();
  f.c.currentSessionId='B';f.c.resumeSessionGeneration++;f.c.renderCheckpointControl(state('B',false));
  const b=f.c.toggleCheckpointControl();assert.equal(f.calls[1].body.sessionId,'B');assert.equal(f.calls[1].body.command,'on');
  f.reply(0,{checkpoint:state('A',false)});await a;assert.equal(f.toasts.length,0,'offscreen operation has no toast in B');
  f.reply(1,{checkpoint:state('B',true)});await b;assert.equal(f.nodes.checkpointBtn.textContent,'检查点：开');assert.deepEqual(f.toasts,['检查点已开启']);
  f.c.renderCheckpointControl(state('A',false));assert.equal(f.nodes.checkpointBtn.textContent,'检查点：开','a mismatched session snapshot cannot overwrite B');
 }
 {
  const f=fixture('');f.c.renderCheckpointControl(state('',true));const switching=f.c.toggleCheckpointControl();
  assert.equal(f.creation.length,1);assert.equal(f.creation[0].options.force,true);assert.equal(f.calls.length,0,'allocate a saved id before any mutation');
  await f.c.toggleCheckpointControl();assert.equal(f.creation.length,1,'double click during allocation is ignored');
  f.c.currentSessionId='created';f.creation[0].resolve(true);await tick();
  assert.deepEqual(f.calls[0].body,{sessionId:'created',command:'off'},'preserve the originally requested desired state after id allocation');
  assert.equal(f.nodes.inputField.value,'keep my draft');assert.equal(f.c.attachments[0].name,'keep.png');
  f.reply(0,{checkpoint:state('created',false)});await switching;
  assert.equal(f.nodes.checkpointBtn.textContent,'检查点：关');assert.equal(f.calls.some(call=>call.url.includes('activate')),false,'session-local control does not activate another runtime');
 }
 {
  const f=fixture('');f.c.renderCheckpointControl(state('',true));const switching=f.c.toggleCheckpointControl();
  f.c.currentSessionId='B';f.c.activeWorkspaceId='W2';f.c.resumeSessionGeneration++;f.c.renderCheckpointControl(state('B',false));
  f.creation[0].resolve(false);await switching;
  assert.equal(f.calls.length,0,'navigation during id creation cannot redirect a write');assert.equal(f.toasts.length,0);assert.equal(f.nodes.checkpointBtn.disabled,false);
 }
 {
  const f=fixture();f.c.renderCheckpointControl(state('B',false));assert.equal(f.nodes.checkpointBtn.disabled,true,'unknown ownership is not an actionable switch');
  const query=f.c.runCheckpointControl('status');assert.ok(f.calls[0].url.startsWith('/api/checkpoint?'));assert.equal(f.calls[0].body,null);
  f.reply(0,state());await query;assert.equal(f.nodes.checkpointBtn.textContent,'检查点：开');assert.deepEqual(f.toasts,['检查点：开']);
  const count=f.calls.length;await f.c.runCheckpointControl('on /some/project');assert.equal(f.calls.length,count,'scope arguments are rejected locally');assert.ok(f.toasts.at(-1).length<80);
 }
 {
  const f=fixture();f.c.renderCheckpointControl(state('A',false));const switching=f.c.toggleCheckpointControl();
  f.reply(0,{error:'请在项目工作区中开启'},400);await tick();
  assert.equal(f.toasts[0],'请在项目工作区中开启');
  if(f.calls.length>1)f.reply(1,state('A',false));await switching;
  assert.equal(f.nodes.checkpointBtn.textContent,'检查点：关');assert.equal(f.nodes.checkpointBtn.disabled,false);
 }
 {
  const f=fixture();const old=f.c.runCheckpointControl('status');f.c.currentSessionId='B';f.c.resumeSessionGeneration++;f.c.renderCheckpointControl(state('B',false));
  f.reply(0,state('A',true));await old;assert.equal(f.nodes.checkpointBtn.textContent,'检查点：关');assert.equal(f.toasts.length,0);
 }
})().then(()=>clearTimeout(watchdog),error=>{clearTimeout(watchdog);console.error(error);process.exitCode=1});
`

func TestCheckpointPollCannotOverwriteCompletedAction(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/app.js", `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const start=source.indexOf('async function pollStatus('),end=source.indexOf('function closeContextMeterPopover(',start);
assert.ok(start>=0&&end>start);
const requests=[],renders=[];
const c={statusRequestGeneration:0,checkpointControlGeneration:0,currentSessionId:'A',resumeSessionGeneration:1,renderCheckpointControl:state=>renders.push(state),renderStatusSnapshot(){},fetch:()=>new Promise(resolve=>requests.push(resolve))};
vm.createContext(c);vm.runInContext(source.slice(start,end),c);
(async()=>{
 const old=c.pollStatus();c.checkpointControlGeneration=2;
 requests[0]({ok:true,json:async()=>({checkpoint:{enabled:false}})});await old;
 assert.equal(renders.length,0,'a poll started before enable/disable must not restore the previous state');
 const current=c.pollStatus();requests[1]({ok:true,json:async()=>({checkpoint:{enabled:true}})});await current;
 assert.equal(renders.length,1);assert.equal(renders[0].enabled,true,'subsequent polling still synchronizes CLI changes');
})().catch(error=>{console.error(error);process.exitCode=1});
`)
}
