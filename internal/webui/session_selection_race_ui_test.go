package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestSessionSelectionBrowserRaces(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"app.js", "sessions.js", "chat.js"} {
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
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function extract(file, start, next) {
 const source=sources[file], from=source.indexOf(start), to=next ? source.indexOf(next,from+start.length) : source.length;
 assert(from>=0&&to>from,start); return source.slice(from,to);
}
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const deferred=()=>{let resolve; const promise=new Promise(r=>resolve=r); return {promise,resolve};};
function context() {
 const errors=[], rendered=[], added=[];
 const c={ URLSearchParams,sessions:[],sessionsLoading:false,sessionsLoadGeneration:0,sessionsNextCursor:'',sessionsTotal:0,sessionFilter:'',window:{}, showArchivedSessions:false,pendingAsk:null,lastStatusSnapshot:null,currentSessionId:'A', pendingSessionId:null, resumeSessionGeneration:0, sessionStatsGeneration:0,
  turnRunning:false,runningSessionId:null,currentView:'chat',messages:[],streamedTextThisTurn:false,
  queuedTurns:[],queuedSessionId:null,drainingQueuedTurns:false,backgroundContinuationGeneration:0,
  backgroundContinuationGenerations:new Map(),pendingForegroundRequest:null,runningTurnNeedsHistorySync:false,runningTurnIncompleteReason:'',
  document:{documentElement:{lang:'en'},getElementById:id=>id==='welcomeScreen'?null:{disabled:false},querySelector:()=>null,querySelectorAll:()=>[]},
  clearTodoPlan(){},resumeAutoScroll(){},beginUserTurn(){},finishUserTurn(){},updateSendBtn(){},syncTurnControls(){},
  setTurnRunning:(value,id)=>{c.turnRunning=value;c.runningSessionId=value?id:null;},
  addMessage:(role,text)=>added.push({session:c.currentSessionId,role,text}),
  renderHistoryMessages:history=>rendered.push({session:c.currentSessionId,history}),
  renderSessions:()=>rendered.push({sidebar:c.currentSessionId}),
  renderQueuedTurns(){},resetSessionFiles(){},resetArtifactsForSession(){},resetTraceForSession(){},loadSessionFiles(){},
  detachRunningTurnView(){rendered.push({detached:c.currentSessionId});},
  loadSessions:async()=>{},loadSessionStatsbar(){},loadArtifactsForSession:async()=>{},restoreCompactionHistory:async()=>{},
  loadEffort:async()=>{},pollStatus:async()=>{},loadTrace(){},setTimeout(){},drainQueuedTurns(){},
  showError:error=>errors.push(error),showToast(){},applyLayout(){},syncApprovalChip(){},approvalMode:'default',
 };
 c.uiText=(en,zh)=>c.document.documentElement.lang==='zh-CN'?zh:en;
 vm.createContext(c);
 vm.runInContext(extract('app.js','const DESKTOP_I18N','function presetDisplayName'),c);
 vm.runInContext(extract('sessions.js','async function resumeSession(id)'),c);
 vm.runInContext(extract('sessions.js','async function loadSessions(append)','async function loadMoreSessions('),c);
 vm.runInContext(extract('chat.js','async function runTurnItem(item)','const MESSAGE_ACTION_ICONS'),c);
 vm.runInContext(extract('chat.js','function sameSession(d)','let thinkingEl'),c);
 vm.runInContext(extract('sessions.js','function sessionState(s)','function sessionItemKeydown('),c);
 return {c,errors,rendered,added};
}
(async()=>{
 // Slow turn artifact refresh must not append A's POST fallback into B.
 {
  const {c,added}=context(), artifact=deferred();
  c.fetch=async()=>({ok:true,json:async()=>({sessionId:'A',text:'A completed'})});
  c.loadArtifactsForSession=()=>artifact.promise;
  const turn=c.runTurnItem({text:'A prompt'}); await tick();
  c.currentSessionId='B'; artifact.resolve(); await turn;
  assert.equal(added.filter(item=>item.session==='B').length,0,'A completion appended into B after artifact await');
 }
 // Sidebar selection commits together with B history, before optional loads finish.
 {
  const {c,rendered}=context(), compaction=deferred();
  c.fetch=async()=>({ok:true,json:async()=>({messages:['B history']})});
  c.restoreCompactionHistory=()=>compaction.promise;
  const selection=c.resumeSession('B'); await tick();
  assert.equal(c.currentSessionId,'B');
  assert(rendered.some(item=>item.sidebar==='B'),'sidebar still shows A while B transcript already rendered');
  compaction.resolve(); await selection;
 }
 // A late activation cannot replace the user's latest successful selection.
 {
  const {c,rendered}=context(), slowA=deferred();
  c.fetch=(_url,options)=>JSON.parse(options.body).id==='A'?slowA.promise:Promise.resolve({ok:true,json:async()=>({messages:['B']})});
  const a=c.resumeSession('A'), b=c.resumeSession('B'); await b;
  slowA.resolve({ok:true,json:async()=>({messages:['old A']})}); await a;
  assert.equal(c.currentSessionId,'B');
  assert(!rendered.some(item=>item.history?.includes('old A')));
 }
 // Direct navigation can open a real saved session outside the current list page.
 {
  const {c}=context();c.sessions=[{id:'A',title:'Alpha'}];
  c.fetch=async()=>({ok:true,json:async()=>({session:{id:'B',title:'Automation result',workDir:'/saved'},messages:[]})});
  await c.resumeSession('B');
  assert.equal(c.sessions.find(item=>item.id==='B')?.title,'Automation result','selected session metadata missing from sidebar cache');
  c.fetch=async()=>({ok:true,json:async()=>({session:{id:'wrong-id',title:'Do not cache'},messages:[]})});
  await c.resumeSession('C');
  assert(!c.sessions.some(item=>item.id==='wrong-id'));
  assert(!c.sessions.some(item=>item.id==='C'),'session metadata must not be invented');
 }
 // Failed selection must leave the visible live transcript attached.
 {
  const {c,rendered,errors}=context();c.turnRunning=true;c.runningSessionId='A';
  c.fetch=async()=>({ok:false,status:404}); await c.resumeSession('missing');
  assert.equal(c.currentSessionId,'A');assert.equal(errors.length,1);
  assert(!rendered.some(item=>item.detached),'failed navigation detached the still-visible running transcript');
 }
 // Blank composer must reject unrelated SSE while first pending turn can claim its id.
 {
  const {c}=context();c.currentSessionId=null;
  assert.equal(c.sameSession({session:'old-background'}),false,'foreign SSE entered a blank session');
  c.turnRunning=true;c.pendingForegroundRequest={sessionId:null};
  assert.equal(c.sameSession({session:'new-turn'}),true);
  assert.equal(c.runningSessionId,'new-turn');
 }
 // A newer filter request must supersede an older in-flight list load.
 {
  const {c}=context(), oldList=deferred(), newList=deferred();let calls=0;
  c.fetch=()=> (++calls===1?oldList.promise:newList.promise);
  c.sessionFilter='alpha';const first=c.loadSessions(false);
  c.sessionFilter='beta';const second=c.loadSessions(false);
  assert.equal(calls,2,'new session filter was dropped behind a stale pending list');
  newList.resolve({ok:true,json:async()=>({sessions:[{id:'B'}]})});await second;
  oldList.resolve({ok:true,json:async()=>({sessions:[{id:'A'}]})});await first;
  assert.equal(c.sessions[0].id,'B');
 }
 // Operator actions have priority over the running owner, and saved interrupted turns are not live.
 {
 const {c}=context(); c.turnRunning=true;c.runningSessionId='A';c.pendingAsk={question:'Confirm?'};
  assert.equal(c.sessionState({id:'A'}).name,'waiting');
  c.pendingAsk=null;c.document.querySelector=()=>({});
  assert.equal(c.sessionState({id:'A'}).name,'approval');
  c.document.querySelector=()=>null;c.turnRunning=false;
  assert.equal(c.sessionState({id:'B',status:'running'}).name,'interrupted');
  assert.equal(c.sessionState({id:'B'}).name,'idle');
  const completedState=c.sessionState({id:'B',status:'completed'});
  assert.equal(completedState.tooltip,'Completed','status icon must use the current English interface language');
  c.uiText=(_en,zh)=>zh;
  assert.equal(c.sessionState({id:'B',status:'completed'}).tooltip,'已完成','status icon must use the current Chinese interface language');
  c.relativeTime=()=>'';c.escAttr=String;c.escHtml=String;c.escOnclick=String;
  vm.runInContext(extract('sessions.js','function renderSessionItem(s)','function renderSessions('),c);
  const completed=c.renderSessionItem({id:'C',title:'Example',status:'completed'});
  assert.match(completed,/title="已完成"/,'status icon native tooltip must use the current Chinese interface language');
  assert.match(completed,/session-status-tooltip[^>]*>已完成/,'status icon hover tooltip must use the current Chinese interface language');
  assert.doesNotMatch(completed,/Completed/,'Chinese tooltip must not contain the English label');
  c.uiText=en=>en;
  const completedEnglish=c.renderSessionItem({id:'C',title:'Example',status:'completed'});
  assert.match(completedEnglish,/title="Completed"/,'status icon native tooltip must use the current English interface language');
  assert.match(completedEnglish,/session-status-tooltip[^>]*>Completed/,'status icon hover tooltip must use the current English interface language');
  assert.doesNotMatch(completedEnglish,/已完成/,'English tooltip must not contain the Chinese label');
  const saved={id:'B',title:'Beta',status:'stopped'};
  c.turnRunning=true;c.runningSessionId='B';
  const running=c.renderSessionItem(saved);
  assert.match(running,/data-state="running"/);
  assert.match(running,/data-detail-meta="Running in background ·/,'tooltip must describe the live background turn rather than saved stopped status');
  c.turnRunning=false;
  assert.match(c.renderSessionItem(saved),/data-detail-meta="Last turn stopped ·/,'historical stopped status still applies after the live turn ends');
 }
 // Changing the Desktop language refreshes already-rendered sidebar icon titles.
 {
  const {c,rendered}=context(); const before=rendered.length;
  c.applyLanguage('zh-CN');
  assert.equal(c.document.documentElement.lang,'zh-CN');
  assert.equal(c.sessionState({id:'B',status:'completed'}).tooltip,'已完成');
  assert(rendered.slice(before).some(item=>item.sidebar==='A'),'language change did not rerender existing session status icons');
  c.applyLanguage('en');
  assert.equal(c.document.documentElement.lang,'en');
  assert.equal(c.sessionState({id:'B',status:'completed'}).tooltip,'Completed');
 }
 // The global stop control names its actual target when another transcript is open.
 {
  const {c}=context(), stop={style:{},classList:{toggle(){}},setAttribute(name,value){this[name]=value;}};
  c.document.getElementById=id=>id==='stopBtn'?stop:{style:{}};
  c.sessions=[{id:'A',title:'Alpha'},{id:'B',title:'Beta'}];
  c.turnRunning=true;c.runningSessionId='B';c.stopRequestPending=false;
  vm.runInContext(extract('chat.js','function syncTurnControls()','function setTurnRunning('),c);
  c.syncTurnControls();
  assert.match(stop.title,/Beta/,'background stop action does not name its real session');
  assert.equal(stop['aria-label'],stop.title);
  c.currentSessionId='B';c.syncTurnControls();
  assert.match(stop.title,/current turn/i);
  c.currentSessionId='A';c.stopRequestPending=true;c.syncTurnControls();
  assert.match(stop.title,/Beta/);assert.equal(stop.disabled,true);
 }
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session selection races: %v\n%s", err, out)
	}
}
