package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestAutomationSessionShowsSavedProgressWithoutDuplicateLiveRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/automations.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const response=data=>({ok:true,status:200,json:async()=>data});
const area={status:null,scrollTop:100,scrollHeight:1000,clientHeight:500,
 querySelector(sel){return sel==='.automation-session-status'?this.status:null;},querySelectorAll(){return [];},
 insertAdjacentHTML(){const parent=this;this.status={innerHTML:'',classList:{toggle(){}},remove(){parent.status=null;}};}};
const timers=new Map();let nextTimer=0,calls=[],renders=[],runReplies=[],historyReplies=[],listReplies=[];
const c={console,Map,Set,Date,Intl,AbortController,messages:[],streamedTextThisTurn:false,
 currentSessionId:'session-one',turnRunning:false,runningSessionId:null,sessions:[],
 document:{documentElement:{lang:'zh-CN'},visibilityState:'visible',getElementById:id=>id==='chatArea'?area:null},
 uiText:(en,zh)=>zh,autoScroll(){},restoreCompactionHistory:async()=>{},
 renderHistoryMessages:history=>{renders.push({session:c.currentSessionId,history:JSON.parse(JSON.stringify(history))});area.status=null;area.scrollTop=area.scrollHeight;},
 fetch:async url=>{calls.push(url);const q=url==='/api/automations'?listReplies:url.startsWith('/api/sessions/')?historyReplies:runReplies;
   assert(q.length,'unexpected request '+url);const value=q.shift();return await value;},
 setTimeout:fn=>{const id=++nextTimer;timers.set(id,fn);return id;},clearTimeout:id=>timers.delete(id),window:{}};
vm.createContext(c);vm.runInContext(source,c);
const runs=vm.runInContext('automationSessionRuns',c);
async function flush(){await tick();await tick();}
async function next(){const [id,fn]=timers.entries().next().value||[];assert(fn,'expected another poll');timers.delete(id);fn();await flush();}
function history(...blocks){return {messages:[{role:'user',content:[{type:'text',text:'请执行任务'}]},...blocks]};}
const denied={role:'assistant',content:[{type:'tool_use',name:'AskUser',tool_use_id:'ask-1',input:{}},
 {type:'tool_result',tool_use_id:'ask-1',content:'unattended admission policy: unauthorized',is_error:true}]};
(async()=>{
 runs.set('session-one',{jobId:'job-one',runId:'run-one'});
 const first=history(denied), final=history(denied,{role:'assistant',content:[{type:'text',text:'最终回答'}]});
 runReplies=[response({status:'running',liveText:'正在搜索 <script>bad()</script>',activity:[{kind:'tool_failed',tool:'AskUser'}]}),
   response({status:'running',liveText:'正在搜索 <script>bad()</script>'}),
   response({status:'running',liveText:'unattended admission policy: unauthorized'}),
   response({status:'succeeded',output:'最终回答'})];
 historyReplies=[response(first),response(first),response(first),response(final)];
 c.watchAutomationSession('session-one');await flush();
 assert.equal(renders.length,1);assert.match(JSON.stringify(renders[0].history),/unattended admission policy: unauthorized/);
 assert.equal(area.scrollTop,100,'a history refresh must preserve the reading position');
 assert.match(area.status.innerHTML,/正在搜索/);assert.match(area.status.innerHTML,/&lt;script&gt;/);
 assert.doesNotMatch(area.status.innerHTML,/AskUser/,'run activity must not masquerade as a saved tool row');
 const existingStatus=area.status;
 await next();assert.equal(renders.length,1,'an unchanged checkpoint must keep open UI intact');
 assert.equal(area.status,existingStatus);
 await next();assert.equal(renders.length,1);assert.doesNotMatch(area.status.innerHTML,/unattended admission policy/,
   'a saved tool result must not also appear as a live tail');
 await next();assert.equal(renders.length,2);assert.match(JSON.stringify(renders.at(-1).history),/最终回答/);
 assert.equal(area.status,null);assert.equal(timers.size,0);assert.equal(runs.has('session-one'),false);
 assert(calls.includes('/api/sessions/session-one'));

 c.currentSessionId='session-failed';runs.set('session-failed',{jobId:'job-failed',runId:'run-failed'});
 runReplies=[response({status:'failed',error:'provider <unavailable>',output:''})];historyReplies=[response(history(denied))];
 c.watchAutomationSession('session-failed');await flush();
 assert.match(area.status.innerHTML,/失败/);assert.match(area.status.innerHTML,/provider &lt;unavailable&gt;/);
 assert.equal(timers.size,0);assert.equal(runs.has('session-failed'),false);

 c.currentSessionId='session-empty';runs.set('session-empty',{jobId:'job-empty',runId:'run-empty'});
 runReplies=[response({status:'succeeded',output:''})];historyReplies=[response(history())];
 c.watchAutomationSession('session-empty');await flush();
 assert.match(area.status.innerHTML,/没有文字回答/);

 // Reloaded app: a sidebar Cron session discovers its running job from the list API.
 c.currentSessionId='session-side';c.sessions=[{id:'session-side',title:'Cron · 日报'}];
 listReplies=[response({automations:[{id:'job-side',lastRun:{id:'run-side',status:'running',sessionId:'session-side'}}]})];
 runReplies=[response({status:'running',liveText:'等待下一步'})];historyReplies=[response(history())];
 c.watchAutomationSession('session-side');await flush();
 assert.equal(runs.get('session-side').runId,'run-side');assert(calls.includes('/api/automations'));
 assert.equal(renders.at(-1).session,'session-side');
 c.stopAutomationSessionWatch();timers.clear();

 // A newer skipped attempt must not hide the execution lock's active session.
 c.currentSessionId='session-skipped';c.sessions=[{id:'session-skipped',title:'Cron · 日报'}];
 listReplies=[response({automations:[
   {id:'job-idle',running:false,lastRun:{id:'run-idle',status:'succeeded'}},
   {id:'job-other',running:true,lastRun:{id:'skip-other',status:'skipped'}},
   {id:'job-skipped',running:true,lastRun:{id:'skip-newest',status:'skipped'}}]})];
 runReplies=[response({runs:[{id:'run-other',status:'running',sessionId:'session-other'}]}),
   response({runs:[{id:'skip-newest',status:'skipped'},
     {id:'run-older',status:'succeeded',sessionId:'session-skipped'},
     {id:'run-skipped',status:'running',sessionId:'session-skipped'}]}),
   response({status:'running'}),response({status:'succeeded',output:'跳过期间完成的回答'})];
 historyReplies=[response(history({role:'assistant',content:[{type:'text',text:'跳过期间的进度'}]})),
   response(history({role:'assistant',content:[{type:'text',text:'跳过期间完成的回答'}]}))];
 c.watchAutomationSession('session-skipped');await flush();
 assert(calls.includes('/api/automations/job-skipped/runs'),'a busy job with a newer skipped attempt must inspect its run history');
 assert(!calls.includes('/api/automations/job-idle/runs'),'idle jobs without a skipped latest attempt do not need discovery history requests');
 assert.equal(runs.get('session-skipped').runId,'run-skipped','matching running records take precedence over older terminal records');
 assert(calls.includes('/api/automations/job-skipped/runs/run-skipped'));
 assert.match(JSON.stringify(renders.at(-1).history),/跳过期间的进度/);
 assert.equal(timers.size,1);await next();
 assert.match(JSON.stringify(renders.at(-1).history),/跳过期间完成的回答/);
 assert.equal(timers.size,0);assert.equal(runs.has('session-skipped'),false);

 // The run can finish before discovery while a newer skipped attempt remains lastRun.
 c.currentSessionId='session-discovery-finished';c.sessions=[{id:'session-discovery-finished',title:'Cron · 日报'}];
 listReplies=[response({automations:[{id:'job-discovery-finished',running:false,lastRun:{id:'skip-finished',status:'skipped'}}]})];
 runReplies=[response({runs:[{id:'skip-finished',status:'skipped'},
   {id:'run-discovery-finished',status:'succeeded',sessionId:'session-discovery-finished'}]}),
   response({status:'succeeded',output:'发现期间完成的回答'})];
 historyReplies=[response(history({role:'assistant',content:[{type:'text',text:'发现期间完成的回答'}]}))];
 c.watchAutomationSession('session-discovery-finished');await flush();
 assert.match(JSON.stringify(renders.at(-1).history),/发现期间完成的回答/);
 assert.equal(timers.size,0);assert.equal(runs.has('session-discovery-finished'),false);

 // Sidebar activation can finish after the last saved running checkpoint.
 for (const status of ['succeeded','failed']) {
   const sessionId='session-terminal-'+status,jobId='job-terminal-'+status,runId='run-terminal-'+status;
   c.currentSessionId=sessionId;c.sessions=[{id:sessionId,title:'Cron · 日报'}];
   listReplies=[response({automations:[{id:jobId,running:false,lastRun:{id:runId,status,sessionId}}]})];
   runReplies=[response({status,output:status==='succeeded'?'侧栏最终回答':'',error:status==='failed'?'侧栏失败原因':''})];
   historyReplies=[response(history({role:'assistant',content:[{type:'text',text:'侧栏最终检查点 '+status}]}))];
   const beforeTerminal=renders.length;
   c.watchAutomationSession(sessionId);await flush();
   assert.equal(renders.length,beforeTerminal+1,'terminal discovery must reconcile the final saved checkpoint');
   assert.match(JSON.stringify(renders.at(-1).history),/侧栏最终检查点/);
   if (status==='failed') assert.match(area.status.innerHTML,/侧栏失败原因/);
   assert.equal(timers.size,0);assert.equal(runs.has(sessionId),false);
   assert.equal(vm.runInContext('automationSessionWatch',c),null);
 }

 // Opening an explicit failed run must restore its error even without saved assistant text.
 vm.runInContext("automationState.active=true;automationState.selectedId='job-open-failed';automationState.runs=[{id:'run-open-failed',status:'failed',sessionId:'session-open-failed'}]",c);
 c.loadSessions=async()=>{};
 c.window.metisNavigation={current:()=>({page:'schedules'}),navigate:async route=>{c.currentSessionId=route.sessionId;return true;}};
 c.sessions=[{id:'session-open-failed',title:'Cron · 日报'}];
 listReplies=[response({automations:[]})];
 runReplies=[response({status:'failed',error:'重新打开的 provider 失败原因',output:''})];historyReplies=[response(history())];
 assert.equal(await c.openAutomationRunSession('session-open-failed','job-open-failed','run-open-failed'),true);await flush();
 assert(calls.includes('/api/automations/job-open-failed/runs/run-open-failed'),'an explicitly opened terminal run must reconcile its saved result');
 assert.match(area.status.innerHTML,/重新打开的 provider 失败原因/);
 assert.equal(renders.at(-1).session,'session-open-failed');
 assert.equal(timers.size,0);assert.equal(runs.has('session-open-failed'),false);
 vm.runInContext("automationState.active=false;automationState.selectedId='';automationState.runs=[]",c);

 // A run-list response arriving after navigation must not seed or poll the old session.
 let releaseDiscovery;const delayedDiscovery=new Promise(resolve=>releaseDiscovery=resolve);
 c.currentSessionId='session-discovery-old';c.sessions=[{id:'session-discovery-old',title:'Cron · 日报'}];
 listReplies=[response({automations:[{id:'job-discovery-old',running:true,lastRun:{id:'skip-discovery-old',status:'skipped'}}]})];
 runReplies=[delayedDiscovery];
 c.watchAutomationSession('session-discovery-old');await flush();
 const beforeDiscovery=renders.length;c.currentSessionId='session-discovery-new';c.stopAutomationSessionWatch();
 runs.set('session-discovery-new',{jobId:'job-discovery-new',runId:'run-discovery-new'});
 runReplies=[response({status:'running'})];historyReplies=[response(history({role:'assistant',content:[{type:'text',text:'发现后的新会话'}]}))];
 c.watchAutomationSession('session-discovery-new');await flush();
 releaseDiscovery(response({runs:[{id:'run-discovery-old',status:'running',sessionId:'session-discovery-old'}]}));await flush();
 assert.equal(runs.has('session-discovery-old'),false);
 assert(!calls.includes('/api/automations/job-discovery-old/runs/run-discovery-old'));
 assert.equal(renders.length,beforeDiscovery+1);assert.equal(renders.at(-1).session,'session-discovery-new');
 assert.equal(vm.runInContext('automationSessionWatch.sessionId',c),'session-discovery-new');
 c.stopAutomationSessionWatch();timers.clear();

 // A late response for A must not redraw B after navigation changes selection.
 let release;const delayed=new Promise(resolve=>release=resolve);
 c.currentSessionId='session-old';runs.set('session-old',{jobId:'job-old',runId:'run-old'});
 runReplies=[response({status:'running'})];historyReplies=[delayed];
 c.watchAutomationSession('session-old');await flush();
 const before=renders.length;c.currentSessionId='session-new';c.stopAutomationSessionWatch();
 runs.set('session-new',{jobId:'job-new',runId:'run-new'});
 runReplies=[response({status:'running'})];historyReplies=[response(history({role:'assistant',content:[{type:'text',text:'新会话'}]}))];
 c.watchAutomationSession('session-new');await flush();release(response(history({role:'assistant',content:[{type:'text',text:'旧会话'}]})));await flush();
 assert.equal(renders.length,before+1);assert.equal(renders.at(-1).session,'session-new');
 assert.doesNotMatch(JSON.stringify(renders.at(-1).history),/旧会话/);

 // A foreground turn taking over the same session retires the cron watcher.
 let releaseForeground;const pendingHistory=new Promise(resolve=>releaseForeground=resolve);
 c.stopAutomationSessionWatch();c.currentSessionId='session-foreground';
 runs.set('session-foreground',{jobId:'job-foreground',runId:'run-foreground'});
 runReplies=[response({status:'running'})];historyReplies=[pendingHistory];
 c.watchAutomationSession('session-foreground');await flush();
 c.turnRunning=true;c.runningSessionId='session-foreground';
 releaseForeground(response(history({role:'assistant',content:[{type:'text',text:'旧定时任务'}]})));await flush();
 assert.equal(vm.runInContext('automationSessionWatch',c),null);

 // Compaction can remove a traced tool; its old index must not open another tool.
 const restoredTools=[],selectedTools=[];
 const replacementRow={dataset:{rowKey:'replacement-row',traceCallId:'replacement-trace'},classList:{contains:()=>false}};
 const restoredArea={querySelectorAll:sel=>sel==='.call-row'?[replacementRow]:[],scrollTop:0};
 c.toggleToolInline=id=>restoredTools.push(id);c.openToolDetail=id=>selectedTools.push(id);
 const missingTraceView={openTurns:[],openGroups:[],openTools:[{trace:'removed-trace',index:0}],openThoughts:[],
   selectedIndex:0,selectedTrace:'removed-trace',selectedTab:'summary',nearBottom:false,scrollTop:0};
 c.automationRestoreHistoryView(restoredArea,missingTraceView);
 assert.equal(restoredTools.length,0,'a missing trace must not restore a different tool at the old index');
 assert.equal(selectedTools.length,0,'a missing selected trace must not open a different tool detail');
 c.automationRestoreHistoryView(restoredArea,{...missingTraceView,openTools:[{trace:'',index:0}],selectedTrace:''});
 assert.deepEqual(restoredTools,['replacement-row']);assert.deepEqual(selectedTools,['replacement-row']);
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser behavior: %v\n%s", err, output)
	}
}
