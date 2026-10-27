package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestDraftCreationFailureKeepsComposer(t *testing.T) {
	runDraftCreationBrowserCheck(t, draftCreationFailureCase)
}

func TestDraftCreationNavigationCannotClaimNewView(t *testing.T) {
	runDraftCreationBrowserCheck(t, draftCreationNavigationCase)
}

func TestDraftCreationKeepsOwnedPermissionSnapshot(t *testing.T) {
	runDraftCreationBrowserCheck(t, draftCreationPermissionCase)
}

func TestDraftCreationLocalCommandsDoNotSubmitTurns(t *testing.T) {
	runDraftCreationBrowserCheck(t, draftCreationLocalCommandCase)
}

func TestDraftCreationQueuedFailureKeepsOriginalOwner(t *testing.T) {
	runDraftCreationBrowserCheck(t, draftCreationQueueCase)
}

func TestDraftCreationDoesNotConsumeLaterComposerEdits(t *testing.T) {
	runDraftCreationBrowserCheck(t, draftCreationLaterEditsCase)
}

func runDraftCreationBrowserCheck(t *testing.T, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	content, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(string(content))
	if err != nil {
		t.Fatal(err)
	}
	script := draftCreationBrowserHarness + "\n(async()=>{\n" + body + "\n})().catch(error=>{console.error(error);process.exitCode=1;});"
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("draft creation browser checks failed: %v\n%s", err, output)
	}
}

// Real composer, draft-allocation and permission functions run together. Only
// HTTP transport and unrelated rendering hooks are replaced. Session creation
// is deliberately separate from turn execution, including its response timing.
const draftCreationBrowserHarness = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
function extract(start,end){const a=source.indexOf(start),b=source.indexOf(end,a+start.length);assert(a>=0&&b>a,start);return source.slice(a,b);}
const deferred=()=>{let resolve;const promise=new Promise(done=>resolve=done);return{promise,resolve};};
const tick=()=>new Promise(done=>setImmediate(done));
const ok=data=>({ok:true,status:200,json:async()=>data});
const failed=data=>({ok:false,status:500,json:async()=>data});
function fixture(options={}){
 const request=deferred(),creation=deferred(),messages=[],errors=[],toasts=[],finishes=[],navigation=[],requests=[],commands=[];
 const element=()=>({disabled:false,value:'',dataset:{},classList:{toggle(){}},setAttribute(){},querySelector(){return null;}});
 const nodes={inputField:element(),sendBtn:element(),approvalBtn:element(),approvalLabel:element()};nodes.inputField.value='draft A';
 const c={console,Map,Set,currentSessionId:null,resumeSessionGeneration:0,activeWorkspaceId:'workspace-A',draftSessionCreation:null,
  backgroundContinuationGenerations:new Map(),backgroundContinuationGeneration:0,pendingForegroundRequest:null,runningSessionId:null,
  runningTurnIncompleteReason:'',runningTurnNeedsHistorySync:false,streamedTextThisTurn:false,turnRunning:false,currentView:'chat',
  queuedTurns:[],drainingQueuedTurns:false,queuedSessionId:null,sessions:[],chatHistoryViewport:null,attachments:[],pendingAsk:null,
  approvalMode:'fullAccess',approvalMenuOpen:false,permissionModeGeneration:0,permissionModePending:false,
  permissionModeSnapshot:{sessionId:'',mode:'fullAccess'},desktopPreferences:{busyEnter:'queue'},
  window:{metisNavigation:{recordSession(id,options){navigation.push({id,options});}}},
  document:{documentElement:{lang:'en'},getElementById:id=>id==='welcomeScreen'?null:nodes[id]||null,querySelectorAll:()=>[]},
  uiText:(en,zh)=>en,parallelTurnsEnabled:()=>false,isWorkspaceSelectionPending:()=>false,selectedWorkspaceCanRun:()=>true,
  clearTodoPlan(){},resumeAutoScroll(){},beginUserTurn(){},updateSendBtn(){},loadSessionStatsbar(){},loadSessionFiles(){},
  loadArtifactsForSession:async()=>{},loadSessions:async()=>{},renderSessions(){},renderAttachments(){},autoResize(){},closeCommandMenu(){},
  renderQueuedTurns(){c.queueRenders=(c.queueRenders||0)+1;},isViewedTurnRunning:()=>false,
  addMessage(role,text){messages.push({view:c.currentSessionId,role,text});},
  finishUserTurn(outcome){finishes.push({view:c.currentSessionId,outcome});},
  setTurnRunning(running,id){c.turnRunning=running;c.runningSessionId=running?id:null;},
  showError(message){errors.push({view:c.currentSessionId,message});},showToast(message){toasts.push({view:c.currentSessionId,message});},
  executeComposerCommand:async(text,owner)=>{if(/^\/(?:rename|title|help)(?:\s|$)/i.test(text)){commands.push({text,owner});return true;}return false;},
  fetch(url,init){
   const body=init?.body?JSON.parse(init.body):undefined;requests.push({url,body});
   if(url==='/api/sessions')return options.deferredCreate?creation.promise:Promise.resolve(ok(draft(options.mode||'default')));
   assert.equal(url,'/api/turns','unexpected request in a draft test');assert.equal(body.sessionId,'A','turn must use its allocated owner');return request.promise;
  },
 };
 function draft(mode='default',extra={}){return{id:'A',permissionSessionId:'A',permissionMode:mode,permissionModeAvailable:true,...extra};}
 vm.createContext(c);
 vm.runInContext(extract('function closeMismatchedSubAgentDetails()','function newChat()'),c);
 vm.runInContext(extract('async function prepareDraftSession()','// Composer controls mutate'),c);
 vm.runInContext(extract('const PERMISSION_LABELS =','const THEME_LABELS ='),c);
 vm.runInContext(extract('async function runTurnItem(item)','const MESSAGE_ACTION_ICONS'),c);
 vm.runInContext(extract('async function sendMessage(busyBehavior)','async function submitBusyInput(item, behavior)'),c);
 async function started(){for(let i=0;i<10&&!requests.some(r=>r.url==='/api/turns');i++)await tick();assert(requests.some(r=>r.url==='/api/turns'),'owned turn did not start');}
 return{c,nodes,request,creation,draft,messages,errors,toasts,finishes,navigation,requests,commands,started};
}
`

const draftCreationFailureCase = `
for(const response of [failed({error:'creation unavailable'}),ok({})]){
 const f=fixture({deferredCreate:true});f.c.attachments=[{name:'kept.png'}];
 const send=f.c.sendMessage();await tick();
 assert.deepEqual(f.requests.map(r=>r.url),['/api/sessions']);
 assert.equal(f.nodes.inputField.value,'draft A','input is consumed before allocation succeeds');
 f.creation.resolve(response);await send;
 assert.equal(f.nodes.inputField.value,'draft A','failed allocation lost composer text');
 assert.equal(f.c.attachments.length,1,'failed allocation lost attachments');
 assert.equal(f.c.currentSessionId,null);assert.equal(f.c.pendingForegroundRequest,null);
 assert.equal(f.messages.length,0);assert.equal(f.requests.filter(r=>r.url==='/api/turns').length,0);
}
`

const draftCreationNavigationCase = `
for(const changed of ['session','workspace']){
 const f=fixture({deferredCreate:true});const send=f.c.sendMessage();await tick();
 if(changed==='session'){f.c.currentSessionId='B';f.c.resumeSessionGeneration++;}else f.c.activeWorkspaceId='workspace-B';
 f.nodes.inputField.value='new view draft';f.c.attachments=[{name:'B.png'}];
 f.creation.resolve(ok(f.draft('plan')));await send;
 assert.equal(f.nodes.inputField.value,'new view draft','old draft completion cleared the newly visible composer');
 assert.equal(f.c.attachments[0].name,'B.png');assert.equal(f.messages.length,0);
 assert.equal(f.navigation.length,0,'old draft allocation claimed new navigation');
 assert.equal(f.requests.filter(r=>r.url==='/api/turns').length,0);
 assert.equal(f.c.pendingForegroundRequest,null);
 if(changed==='session')assert.equal(f.c.currentSessionId,'B');else assert.equal(f.c.currentSessionId,null);
}
`

const draftCreationPermissionCase = `
// A newly allocated plan session replaces a stale unrestricted blank chip.
{
 const f=fixture({mode:'plan'});const send=f.c.sendMessage();await f.started();
 const turn=f.requests.find(r=>r.url==='/api/turns');assert.equal(turn.body.expectedPermissionMode,'plan');
 assert.equal(f.c.approvalMode,'plan');assert.equal(f.c.permissionModeSnapshot.sessionId,'A');
 assert.equal(f.messages[0].view,'A','user bubble must belong to the allocated session');
 assert.equal(f.nodes.inputField.value,'');assert.equal(f.navigation.length,1);
 f.request.resolve(ok({sessionId:'A',text:'planned'}));await send;
 assert.equal(f.requests.filter(r=>r.url==='/api/settings'||r.url==='/api/ask').length,0,'preallocation approved or wrote permission state');
}
// Missing or cross-session authority must not clear input or start a worker.
for(const snapshot of [{permissionModeAvailable:false},{permissionSessionId:'B',permissionMode:'fullAccess'}]){
 const f=fixture({deferredCreate:true});f.c.attachments=[{name:'kept.png'}];const send=f.c.sendMessage();await tick();
 f.creation.resolve(ok(f.draft('plan',snapshot)));await send;
 assert.equal(f.c.currentSessionId,'A','header allocation itself is allowed');
 assert.equal(f.c.approvalMode,'unknown');assert.equal(f.c.permissionModeForTurn(),null);
 assert.equal(f.nodes.inputField.value,'draft A');assert.equal(f.c.attachments.length,1);
 assert.equal(f.requests.filter(r=>r.url==='/api/turns').length,0,'unknown permission started execution');
 assert.equal(f.messages.length,0);
}
`

const draftCreationLocalCommandCase = `
for(const text of ['/rename local label','/title local label','/help']){
 const f=fixture();f.nodes.inputField.value=text;await f.c.sendMessage();
 assert.equal(f.commands.length,1);assert.equal(f.requests.length,0,'local command reached the new turn allocation path');
 assert.equal(f.messages.length,0);assert.equal(f.c.currentSessionId,null);
}
{
 const f=fixture();assert.equal(await f.c.runTurnItem({text:'/rename queued label'}),true);
 assert.equal(f.commands.length,1);assert.equal(f.requests.length,0);
}
`

const draftCreationQueueCase = `
const f=fixture({deferredCreate:true}),item={text:'queued A'};f.c.drainingQueuedTurns=true;
const ownedQueue=f.c.queuedTurns,turn=f.c.runTurnItem(item);await tick();
f.c.currentSessionId='B';f.c.resumeSessionGeneration++;f.c.queuedTurns=[{text:'queued B'}];
f.creation.resolve(failed({error:'creation unavailable'}));assert.equal(await turn,false);
assert.equal(ownedQueue[0],item,'failed draft allocation dropped its original queued item');
assert.equal(f.c.queuedTurns.length,1);assert.equal(f.c.queuedTurns[0].text,'queued B','failed old turn was inserted into the new view queue');
assert.equal(f.c.queueRenders||0,0,'an old queue completion refreshed another visible queue');
assert.equal(f.requests.filter(r=>r.url==='/api/turns').length,0);
`

const draftCreationLaterEditsCase = `
for(const changed of ['text','images','both']){
 const f=fixture({deferredCreate:true}),oldImage={data:'clicked-image',mediaType:'image/png'},newImage={data:'new-draft-image',mediaType:'image/png'};
 f.c.attachments=[oldImage];const send=f.c.sendMessage();await tick();
 if(changed!=='images')f.nodes.inputField.value='next draft';
 if(changed!=='text')f.c.attachments.push(newImage);
 f.creation.resolve(ok(f.draft('plan')));await f.started();
 const turn=f.requests.find(r=>r.url==='/api/turns');
 assert.equal(turn.body.input,'draft A','allocation sent text edited after the click');
 assert.equal(turn.body.images.length,1,'allocation sent a newer attachment as part of the old click');
 assert.equal(turn.body.images[0].data,'clicked-image');
 assert.equal(f.nodes.inputField.value,changed==='images'?'':'next draft','allocation consumed newer composer text');
 assert.equal(f.c.attachments.length,changed==='text'?0:2,'allocation consumed newer attachments');
 if(changed!=='text')assert.equal(f.c.attachments[1],newImage);
 f.request.resolve(ok({sessionId:'A',text:'old snapshot answered'}));await send;
 assert.equal(f.nodes.inputField.value,changed==='images'?'':'next draft','turn completion consumed the newer draft');
}
`
