package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestAutomationsBrowserBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/automations.js")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, body string }{
		{"server state and safe rendering", `
server = async () => response({automations:[job({name:'<img onerror=bad()>',prompt:'<script>bad()</script>'})],scheduler:{enabled:false,available:true},workspace:'/workspace',model:'local-model'});
c.showAutomationsPage(); await new Promise(setImmediate);
assert.equal(calls[0].url,'/api/automations');
assert.match(get('#automationList').innerHTML,/&lt;img/);
assert.doesNotMatch(get('#automationList').innerHTML,/<img/);
assert.match(get('#automationScheduler').innerHTML,/自动调度未启用/);
assert.match(get('#automationScheduler').innerHTML,/退出应用后不会继续调度/);
state.model='desktop-current-model'; state.modelSource='workspace-default'; state.jobs[0].workDir='/bound/workspace'; state.selectedId=state.jobs[0].id;
c.renderAutomationDetail();
assert.match(get('#automationDetail').innerHTML,/使用任务工作区的默认模型/);
assert.match(get('#automationDetail').innerHTML,/\/bound\/workspace/);
assert.doesNotMatch(get('#automationDetail').innerHTML,/desktop-current-model/);
state.query='missing'; c.renderAutomationList(); assert.match(get('#automationList').innerHTML,/没有匹配/);
state.query=''; state.filter='paused'; c.renderAutomationList(); assert.match(get('#automationList').innerHTML,/没有匹配/);
state.jobs=[]; c.renderAutomationList(); assert.match(get('#automationList').innerHTML,/创建第一个任务/);
c.document.documentElement.lang='en'; c.showAutomationsPage(); await new Promise(setImmediate); assert.match(root.innerHTML,/Scheduled tasks/);
`},
		{"stale lists and leaving the page", `
const unresponsive=deferred(); server=()=>unresponsive.promise;
assert.equal(c.showAutomationsPage(),undefined,'navigation must not wait for the scheduler network');
c.hideAutomationsPage(); state.active=true; calls=[];
const a=deferred(), b=deferred(); let number=0;
server=()=> ++number===1 ? a.promise : b.promise;
const first=c.loadAutomations(), second=c.loadAutomations();
assert.equal(calls[0].options.signal.aborted,true);
b.resolve(response({automations:[job({id:'new'})],scheduler:{available:true}})); await second;
a.resolve(response({automations:[job({id:'old'})],scheduler:{available:true}})); await first;
assert.equal(state.jobs[0].id,'new');
const late=deferred(); server=()=>late.promise; const loading=c.loadAutomations();
c.hideAutomationsPage(); assert.equal(calls.at(-1).options.signal.aborted,true);
late.resolve(response({automations:[job({id:'after-leave'})]})); await loading;
assert.equal(state.jobs[0].id,'new'); assert.equal(state.active,false); assert.equal(state.timer,null);
`},
		{"pause resume run scheduler and duplicate dispatch", `
ready(); let pending=deferred(); server=()=>pending.promise;
const first=click('toggle','task/1'), duplicate=click('toggle','task/1');
assert.equal(calls.length,1); assert.equal(calls[0].url,'/api/automations/task%2F1');
assert.deepEqual(JSON.parse(calls[0].options.body),{paused:true});
pending.resolve(response({})); state.active=false; await first; await duplicate;
state.jobs[0].paused=true; server=async()=>response({}); await click('toggle','task/1');
assert.deepEqual(JSON.parse(calls.at(-1).options.body),{paused:false,enabled:true});
await click('run','task/1'); assert.equal(calls.at(-1).url,'/api/automations/task%2F1/run');
await click('scheduler'); assert.equal(calls.at(-1).url,'/api/automations/scheduler');
assert.deepEqual(JSON.parse(calls.at(-1).options.body),{enabled:true});
server=async()=>response({error:'busy upstream'},409); await click('run','task/1');
assert.equal(state.error,'busy upstream'); assert.equal(state.pending.size,0);
`},
		{"schedule timezone and payload validation", `
assert.equal(c.automationDateToISO('2030-01-02T09:00','Asia/Shanghai'),'2030-01-02T01:00:00.000Z');
assert.equal(c.automationDateToISO('2030-01-02T09:00','America/Los_Angeles'),'2030-01-02T17:00:00.000Z');
assert.throws(()=>c.automationDateToISO('2030-03-10T02:30','America/Los_Angeles'),/不存在/);
assert.throws(()=>c.automationDateToISO('2030-02-31T09:00','UTC'),/有效/);
assert.throws(()=>c.automationDateToISO('2030-01-02T09:00','Not/AZone'),/IANA/);
let form=makeForm({name:'检查',prompt:'认真检查',kind:'interval',interval:'2',unit:'3600',timezone:'Asia/Shanghai',enabled:true,repeat:'0',sessionMode:'isolated',allowTools:'Read\nBash(git status:*)',disabledTools:'Write'});
let body=c.automationFormPayload(form,null);
assert.equal(body.schedule.intervalSeconds,7200); assert.equal(body.enabled,true); assert.equal(body.repeat,0);
assert.deepEqual(Array.from(body.allowTools),['Read','Bash(git status:*)']); assert.deepEqual(Array.from(body.disabledTools),['Write']);
assert.equal('workspace' in body,false); assert.equal('model' in body,false);
form.elements.namedItem('interval').value='1'; form.elements.namedItem('unit').value='1';
assert.throws(()=>c.automationFormPayload(form,null),/30/);
form.elements.namedItem('kind').value='once'; form.elements.namedItem('at').value='2030-01-02T09:00';
body=c.automationFormPayload(form,null); assert.equal(body.repeat,1); assert.equal(body.schedule.at,'2030-01-02T01:00:00.000Z');
const existing=job({schedule:{kind:'once',timezone:'Asia/Shanghai',at:'2030-01-02T01:00:38.123Z'}});
body=c.automationFormPayload(form,existing); assert.equal(body.schedule.at,'2030-01-02T01:00:38.123Z','editing must preserve unchanged seconds');
existing.schedule.timezone=''; existing.schedule.jitterSeconds=20;
body=c.automationFormPayload(form,existing); assert.equal(body.schedule.at,'2030-01-02T01:00:38.123Z'); assert.equal(body.schedule.jitterSeconds,20,'editing must preserve jitter');
form.elements.namedItem('kind').value='cron'; form.elements.namedItem('cron').value='0 9 * * 1-5';
assert.equal(c.automationFormPayload(form,null).schedule.cron,'0 9 * * 1-5');
const expired=job({name:'old name',enabled:false,repeat:1,schedule:{kind:'once',at:'2020-01-02T01:00:00Z',timezone:'Asia/Shanghai'}});
const renamed={...expired,name:'new name',schedule:{...expired.schedule,jitterSeconds:0}};
const changed=c.automationChangedFields(renamed,expired);
assert.equal(changed.name,'new name'); assert.equal('schedule' in changed,false); assert.equal('enabled' in changed,false);
form.elements.namedItem('name').value=' '; assert.throws(()=>c.automationFormPayload(form,null),/名称/);
`},
		{"create failure preserves draft and blocks duplicate saves", `
ready(); state.active=false; const pending=deferred(); server=()=>pending.promise;
const editor=makeEditor(null); state.editor=editor;
const first=c.saveAutomationEditor(editor), second=c.saveAutomationEditor(editor);
assert.equal(calls.length,1); assert.equal(calls[0].options.method,'POST'); assert.equal(editor.pending,true);
c.closeAutomationEditor(); assert.equal(state.editor,editor,'cannot dismiss an in-flight save');
pending.resolve(response({error:'invalid cron'},400)); await first; await second;
assert.equal(state.editor,editor); assert.equal(editor.form.elements.namedItem('prompt').value,'保留我的指令');
assert.equal(editor.error.textContent,'invalid cron'); assert.equal(editor.pending,false);
server=async()=>response({id:'created'}); await c.saveAutomationEditor(editor);
assert.equal(state.editor,null); assert.equal(state.selectedId,'created'); assert.equal(editor.overlay.removed,true);
`},
		{"edit targets correct task and delete requires a second action", `
ready(); state.active=false; const existing=job(); const editor=makeEditor(existing); state.editor=editor;
server=async()=>response({id:existing.id}); await c.saveAutomationEditor(editor);
assert.equal(calls[0].url,'/api/automations/task%2F1'); assert.equal(calls[0].options.method,'PATCH');
const before=calls.length; c.openAutomationDelete(existing,trigger);
assert.equal(calls.length,before,'opening confirmation must not delete');
assert.match(state.deletion.overlay.innerHTML,/删除这个任务/);
c.closeAutomationDelete(); assert.equal(calls.length,before,'cancelling confirmation must preserve task');
c.openAutomationDelete(existing,trigger); const deletion=state.deletion;
server=async()=>response({error:'still running'},409); await c.confirmAutomationDelete(deletion);
assert.equal(state.deletion,deletion); assert.match(deletion.overlay.querySelector('.automation-form-error').textContent,/still running/);
server=async()=>response(null,204); await c.confirmAutomationDelete(deletion);
assert.equal(calls.at(-1).options.method,'DELETE'); assert.equal(state.deletion,null);
`},
		{"run history isolates task and opens real session route", `
ready(); state.selectedId='task/1';
server=async url=>url.endsWith('/runs') ? response({runs:[{id:'run/1',jobId:'task/1',status:'failed',startedAt:'2030-01-02T01:00:00Z',error:'<script>bad()</script>',sessionId:'session/42'}]}) : response({id:'run/1',status:'failed',output:'<img src=x onerror=bad()>',sessionId:'session/42'});
await c.loadAutomationRuns('task/1'); assert.match(get('#automationRuns').innerHTML,/&lt;script/);
assert.match(get('#automationRuns').innerHTML,/打开会话/);
assert.match(get('#automationLatestResult').innerHTML,/最新执行结果/);
assert.match(get('#automationLatestResult').innerHTML,/&lt;img/); assert.doesNotMatch(get('#automationLatestResult').innerHTML,/<img/);
await c.loadAutomationRun('run/1'); assert.equal(calls.at(-1).url,'/api/automations/task%2F1/runs/run%2F1');
assert.match(get('#automationRunOutput').innerHTML,/&lt;img/); assert.doesNotMatch(get('#automationRunOutput').innerHTML,/<img/);
const opening=click('open-session','session/42');
assert.equal(sessionRefreshes,1,'refresh saved sessions before opening an externally created run');
await opening; assert.equal(navigated.page,'session'); assert.equal(navigated.sessionId,'session/42');
const late=deferred(); server=()=>late.promise; const pending=c.loadAutomationRuns('task/1');
state.selectedId='other'; state.runs=[]; late.resolve(response({runs:[{id:'old-private-output'}]})); await pending;
assert.equal(state.runs.length,0);
`},
		{"run session refresh never overrides a newer selection", `
ready(); state.selectedId='task/1';
const first=deferred(), second=deferred(); let request=0;
c.loadSessions=()=>++request===1?first.promise:second.promise;
const older=click('open-session','session/older'), newer=click('open-session','session/newer');
assert.equal(navigated,null,'must await the session index');
second.resolve(); await newer; assert.equal(navigated.sessionId,'session/newer');
first.resolve(); await older; assert.equal(navigated.sessionId,'session/newer','late refresh must not navigate to an older run');
const pending=deferred(); c.loadSessions=()=>pending.promise; navigated=null;
const leave=click('open-session','session/after-leave'); c.hideAutomationsPage(); pending.resolve(); await leave;
assert.equal(navigated,null,'leaving schedules invalidates the open request');
ready(); const intent=deferred(); c.loadSessions=()=>intent.promise;
const changing=click('open-session','session/after-new-intent'); c.resumeSessionGeneration++; intent.resolve(); await changing;
assert.equal(navigated,null,'a newer navigation intent wins even before its page commits');
const failed=deferred(); c.loadSessions=()=>failed.promise;
const oldJob=click('open-session','session/other-job'); state.selectedId='other-task'; failed.resolve(); await oldJob;
assert.equal(navigated,null,'selecting another scheduled task supersedes an open request');
`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command(node, "-e", automationBrowserHarness+"\n(async()=>{\n"+test.body+"\n})().catch(error=>{console.error(error);process.exitCode=1;}).finally(()=>clearTimeout(watchdog));")
			cmd.Stdin = bytes.NewReader(source)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("browser behavior: %v\n%s", err, output)
			}
		})
	}
}

const automationBrowserHarness = `
const assert=require('node:assert/strict');
const vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const document={documentElement:{lang:'zh-CN'},visibilityState:'visible',activeElement:null};
class Element {
 constructor(){this.innerHTML='';this.textContent='';this.dataset={};this.hidden=false;this.disabled=false;this.isConnected=true;this.nodes=new Map();this.events={};}
 querySelector(selector){if(!this.nodes.has(selector))this.nodes.set(selector,new Element());return this.nodes.get(selector);}
 querySelectorAll(selector){if(selector==='button')return [this.querySelector('[data-keep]'),this.querySelector('[data-delete]')];return [];}
 addEventListener(type,fn){this.events[type]=fn;}
 setAttribute(name,value){this[name]=value;}
 contains(){return true;}
 focus(){document.activeElement=this;}
 remove(){this.removed=true;this.isConnected=false;}
 closest(){return null;}
}
const root=new Element();root.dataset.automationsReady='true';root.dataset.automationsLanguage='zh-CN';
const get=selector=>root.querySelector(selector);
document.getElementById=id=>id==='automationsPage'?root:null;
document.createElement=()=>new Element();document.body={appendChild(){}};
let sessionRefreshes=0;
let calls=[],server=async()=>response({automations:[],scheduler:{available:true}}),navigated=null;
const c={document,console,Intl,Date,AbortController,Set,resumeSessionGeneration:0,loadSessions:async()=>{sessionRefreshes++;},uiText:(en,zh)=>document.documentElement.lang==='en'?en:zh,
 fetch:async(url,options)=>{calls.push({url,options});return server(url,options);},
 setInterval:()=>1,clearInterval(){},setTimeout:()=>1,clearTimeout(){},window:{metisNavigation:{navigate:async route=>{navigated=route;return true;}}}};
vm.createContext(c);vm.runInContext(source,c);
const state=vm.runInContext('automationState',c);
const response=(data,status=200)=>({ok:status>=200&&status<300,status,json:async()=>data});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const job=(patch={})=>({id:'task/1',name:'每日检查',prompt:'查看变更并总结',enabled:true,paused:false,schedule:{kind:'interval',intervalSeconds:3600,timezone:'Asia/Shanghai'},runCount:0,...patch});
function ready(){state.active=true;state.loaded=true;state.jobs=[job()];state.scheduler={enabled:false,available:true};state.error='';}
function click(action,id){const button={dataset:{automationAction:action,id},disabled:false};return c.automationPageClick({target:{closest:()=>button}});}
const trigger=new Element();
function makeForm(values={}){const map=new Map();for(const [name,value]of Object.entries(values)){const el=new Element();el.value=typeof value==='boolean'?'':String(value);el.checked=value===true;map.set(name,el);}const form=new Element();form.elements={namedItem(name){if(!map.has(name)){const el=new Element();el.value='';map.set(name,el);}return map.get(name);}};return form;}
function makeEditor(job){const overlay=new Element();const form=makeForm({name:'任务',prompt:'保留我的指令',kind:'interval',interval:'1',unit:'3600',timezone:'Asia/Shanghai',enabled:true,repeat:'0',sessionMode:'isolated'});const error=form.querySelector('.automation-form-error');overlay.nodes.set('form',form);return{overlay,form,error,job,trigger,pending:false};}
const watchdog=setTimeout(()=>{console.error('browser test timed out');process.exit(1);},5000);
`
