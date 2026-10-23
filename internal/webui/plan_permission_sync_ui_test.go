package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// These checks execute the actual composer, navigation and polling functions.
// Approval buttons alone never authorize a local chip change: only an owned
// server snapshot may establish that the worker has left read-only plan mode.
func TestPlanPermissionBrowserSynchronization(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"app.js", "chat.js", "sessions.js"} {
		content, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(content)
	}
	payload, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert=require('node:assert/strict'), vm=require('node:vm');
const sources=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
function extract(file,start,end){const s=sources[file],a=s.indexOf(start),b=end?s.indexOf(end,a+start.length):s.length; assert(a>=0&&b>a,start);return s.slice(a,b);}
function node(){return {textContent:'',title:'',attributes:{},style:{},dataset:{},classList:{toggle(){}},setAttribute(k,v){this.attributes[k]=v;},removeAttribute(k){delete this.attributes[k];}};}
const tick=()=>new Promise(r=>setImmediate(r));
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
const ok=data=>({ok:true,status:200,json:async()=>data});
function fixture(){
 const nodes={approvalBtn:node(),approvalLabel:node(),sendBtn:node()},toasts=[],requests=[];
 const c={console,Map,Set,URLSearchParams,currentSessionId:'A',activeWorkspaceId:'w',approvalMode:'fullAccess',approvalMenuOpen:false,
  permissionModeGeneration:0,permissionModeSnapshot:null,permissionModePending:false,statusRequestGeneration:0,resumeSessionGeneration:0,
  sessions:[],workspaces:[],turnRunning:false,currentView:'chat',pendingSessionId:null,lastStatusSnapshot:null,
  messages:[],streamedTextThisTurn:false,queuedTurns:[],queuedSessionId:null,drainingQueuedTurns:false,
  document:{documentElement:{lang:'en'},getElementById:id=>nodes[id]||null,querySelectorAll:()=>[]},window:{},
  uiText:(en,zh)=>en,showToast:m=>toasts.push(m),showError:m=>toasts.push(m),
  mergeServerSessionMetadata(){},renderSessions(){},renderStatusSnapshot(){},applyResumeSnapshot(){},restoreCompactionHistory:async()=>{},loadSessionStatsbar(){},
  renderHistoryMessages(){},loadEffort:async()=>{},renderQueuedTurns(){},syncTurnControls(){},updateSendBtn(){},
  closeMismatchedSubAgentDetails(){},clearTodoPlan(){},resumeAutoScroll(){},beginUserTurn(){},finishUserTurn(){},addMessage(){},setTurnRunning(){},
  loadSessions:async()=>{},backgroundContinuationGenerations:new Map(),backgroundContinuationGeneration:0,
  pendingForegroundRequest:null,runningSessionId:null,runningTurnIncompleteReason:'',runningTurnNeedsHistorySync:false,
  settingByKey(){return null;},normalizedPermissionMode:v=>['default','plan','fullAccess'].includes(v)?v:null,
  ensureComposerSessionSettingsTarget:async()=>({sessionId:c.currentSessionId,isCurrent:()=>c.currentSessionId==='A'}),
  setTimeout(){},
  fetch:async(url,options)=>{requests.push({url,options});return ok({});}
 };
 vm.createContext(c);
 vm.runInContext(extract('chat.js','const PERMISSION_LABELS =','const THEME_LABELS ='),c);
 vm.runInContext(extract('app.js','async function pollStatus(','function closeContextMeterPopover('),c);
 vm.runInContext(extract('sessions.js','async function resumeSession(id)'),c);
 vm.runInContext(extract('chat.js','async function runTurnItem(item)','const MESSAGE_ACTION_ICONS'),c);
 vm.runInContext(extract('chat.js','async function setPermissionMode(value, trigger)','async function openPluginSettings()'),c);
 vm.runInContext(extract('chat.js','async function submitAskAnswer(answer, askId)','let turnStartMs ='),c);
 vm.runInContext(extract('chat.js','function renderEnumSetting(s, labels, descs)','function renderNumberSetting(s)'),c);
 return {c,nodes,toasts,requests};
}
(async()=>{
 // This reproduces the original failure: a saved plan session was displayed
 // using another session's stale fullAccess composer state.
 {
  const {c,nodes}=fixture();c.fetch=async(url)=>url.startsWith('/api/status')?ok({permissionSessionId:'B',permissionMode:'plan',permissionModeAvailable:true}):ok({session:{id:'B',mode:'plan'},permissionSessionId:'B',permissionMode:'plan',permissionModeAvailable:true,messages:[]});
  await c.resumeSession('B');assert.equal(c.approvalMode,'plan','resuming a plan session retained stale fullAccess');
  assert.equal(nodes.approvalLabel.textContent,'Plan Mode');
 }
 // Pending/rejected/cancelled plan approval remains plan. A completed human
 // approval is reflected only when the server reports the worker transition.
 {
  const {c,nodes,requests}=fixture();c.fetch=async()=>ok({permissionSessionId:'A',permissionMode:'plan',permissionModeAvailable:true});
  await c.pollStatus();assert.equal(c.approvalMode,'plan');
  await c.pollStatus();assert.equal(c.approvalMode,'plan');
  c.fetch=async()=>ok({permissionSessionId:'A',permissionMode:'default',permissionModeAvailable:true});
  await c.pollStatus();assert.equal(c.approvalMode,'default');assert.equal(nodes.approvalLabel.textContent,'Default');
  assert.equal(requests.filter(r=>r.options?.method==='POST').length,0,'passive status performed a settings write');
 }
 // Actual approval-card submission does not optimistically exit plan mode.
 // Reject, failed/cancelled response, and successful acknowledgement all wait
 // for a worker-owned mode snapshot before the chip can show execution.
 {
  const {c}=fixture();c.applySessionPermissionMode('A','plan');
  const card={isConnected:true,querySelectorAll:()=>[],querySelector:()=>null,classList:{add(){},remove(){}}};
  c.pendingInteractions=new Map();c.interactionCard=()=>card;c.forgetInteraction=()=>{};
  const ask={askId:'approve-plan',session:'A'};c.pendingInteractions.set('ask:approve-plan',ask);
  c.fetch=async(url,options)=>{
   if(url==='/api/ask'){assert.equal(JSON.parse(options.body).answer,'No, keep planning');return ok({});}
   return ok({permissionSessionId:'A',permissionMode:'plan',permissionModeAvailable:true});
  };
  await c.submitAskAnswer('No, keep planning','approve-plan');await tick();assert.equal(c.approvalMode,'plan');
  c.fetch=async()=>({ok:false,status:409,json:async()=>({error:'approval cancelled'})});
  await c.submitAskAnswer('Yes, full access (no sandbox)','approve-plan');assert.equal(c.approvalMode,'plan');
  const worker=deferred();c.fetch=async(url,options)=>{
   if(url==='/api/ask'){assert.equal(JSON.parse(options.body).answer,'Yes, full access (no sandbox)');return ok({});}
   return worker.promise;
  };
  await c.submitAskAnswer('Yes, full access (no sandbox)','approve-plan');assert.equal(c.approvalMode,'plan','button acknowledgement bypassed worker mode');
  worker.resolve(ok({permissionSessionId:'A',permissionMode:'fullAccess',permissionModeAvailable:true}));await tick();assert.equal(c.approvalMode,'fullAccess');
 }
 // The settings panel uses the same owned mode as the composer, regardless
 // of a global settings cache that still stores fullAccess as the default.
 {
  const {c}=fixture();c.escHtml=c.escAttr=String;c.settingTitle=()=>'';c.settingDescription=()=>'';c.lockBadge=()=>'';
  c.applySessionPermissionMode('A','plan');
  const html=c.renderEnumSetting({key:'permission.mode',value:'fullAccess',options:['plan','fullAccess']},{},{});
  assert.match(html,/aria-pressed="true"[^>]*data-value="plan"/);
  assert.match(html,/aria-pressed="false"[^>]*data-value="fullAccess"/);
  const cards=[{...node(),dataset:{value:'plan'}},{...node(),dataset:{value:'fullAccess'}}];
  c.document.querySelectorAll=()=>cards;c.applySessionPermissionMode('A','plan');
  assert.equal(cards[0].attributes['aria-pressed'],'true');assert.equal(cards[1].attributes['aria-pressed'],'false');
  c.applySessionPermissionMode('A',null);assert.equal(cards[0].attributes['aria-pressed'],'false');
 }
 // A poll started before a human's new plan selection must not restore the
 // old fullAccess response after that selection has committed.
 {
  const {c}=fixture(),old=deferred();c.fetch=()=>old.promise;
  const polling=c.pollStatus();c.permissionModeGeneration++;
  c.applySessionPermissionMode('A','plan');
  old.resolve(ok({permissionSessionId:'A',permissionMode:'fullAccess',permissionModeAvailable:true}));await polling;
  assert.equal(c.approvalMode,'plan','old status undid the selected plan mode');
 }
 // A poll begun during the write cannot overwrite the settings acknowledgement.
 {
  const {c}=fixture(),post=deferred(),poll=deferred();c.fetch=(url,options)=>{
   if(url==='/api/settings'){assert.equal(JSON.parse(options.body).sessionId,'A');return post.promise;}
   assert(url.startsWith('/api/status'));return poll.promise;
  };
  const changing=c.setPermissionMode('plan');await tick();assert.equal(c.approvalMode,'unknown');
  assert.equal(c.applySessionPermissionMode('A','fullAccess'),false,'activation snapshot overwrote a pending user selection');
  assert.equal(c.approvalMode,'unknown');
  const polling=c.pollStatus();post.resolve(ok({permissionSessionId:'A',permissionMode:'plan',permissionModeAvailable:true}));
  assert.equal(await changing,true);poll.resolve(ok({permissionSessionId:'A',permissionMode:'fullAccess',permissionModeAvailable:true}));await polling;
  assert.equal(c.approvalMode,'plan');
 }
 // An acknowledgement from the previous session never claims the new composer.
 {
  const {c}=fixture(),post=deferred();c.fetch=()=>post.promise;
  const changing=c.setPermissionMode('plan');await tick();c.currentSessionId='B';c.resumeSessionGeneration++;
  c.invalidatePermissionMode();c.applySessionPermissionMode('B','default');
  post.resolve(ok({permissionSessionId:'A',permissionMode:'plan',permissionModeAvailable:true}));
  assert.equal(await changing,false);assert.equal(c.approvalMode,'default');
 }
 // Navigation and cross-session ownership checks also defeat delayed polls.
 {
  const {c}=fixture(),old=deferred();c.fetch=()=>old.promise;const polling=c.pollStatus();
  c.currentSessionId='B';c.resumeSessionGeneration++;c.applySessionPermissionMode('B','plan');
  old.resolve(ok({permissionSessionId:'A',permissionMode:'fullAccess',permissionModeAvailable:true}));await polling;
  assert.equal(c.approvalMode,'plan');
  c.fetch=async()=>ok({permissionSessionId:'A',permissionMode:'fullAccess',permissionModeAvailable:true});await c.pollStatus();
  assert.equal(c.approvalMode,'unknown','wrong-owner permission snapshot was trusted');
 }
 // Unknown worker permission is explicit and blocks submission, never silently
 // becoming default or fullAccess. Server atomically checks the submitted mode.
 {
  const {c,nodes}=fixture();c.fetch=async()=>ok({permissionSessionId:'A',permissionModeAvailable:false});await c.pollStatus();
  assert.equal(c.approvalMode,'unknown');assert.match(nodes.approvalLabel.textContent,/unavailable/i);
  let turns=0;c.fetch=async(url)=>{if(url==='/api/turns')turns++;return ok({permissionSessionId:'A',permissionModeAvailable:false});};
  assert.equal(await c.runTurnItem({text:'make changes'}),false);assert.equal(turns,0);
  c.applySessionPermissionMode('A','plan');c.fetch=async(url,options)=>{assert.equal(url,'/api/turns');assert.equal(JSON.parse(options.body).expectedPermissionMode,'plan');return ok({sessionId:'A',text:'planned'});};
  c.pollStatus=async()=>{};assert.equal(await c.runTurnItem({text:'make changes'}),true);
 }
})().catch(e=>{console.error(e);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plan permission browser synchronization failed: %v\n%s", err, output)
	}
}
