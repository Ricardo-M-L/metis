package webui

import "testing"

// Execute the shipped browser functions with controllable HTTP completion.
// Each scenario gets fresh session maps so one lifecycle cannot hide another.
func TestResumeTurnBrowserBehavior(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"snapshot_ownership", `
 const f=context(),{c}=f;
 c.applyResumeSnapshot({resumeAvailable:true,resumeToken:'foreign-token',resumeSessionId:'B'},'A');
 assert.equal(c.canResumeCurrentTurn(),false,'foreign status offered Resume for A');
 c.currentSessionId='B';assert.equal(c.canResumeCurrentTurn(),false,'rejected foreign snapshot populated B');
 c.currentSessionId='A';
 c.applyResumeSnapshot({resumeAvailable:true,resumeToken:'unowned'},'A');
 assert.equal(c.canResumeCurrentTurn(),false,'snapshot without proven owner offered Resume');
 c.applyResumeSnapshot({status:'stopped',resumeSessionId:'A'},'A');
 assert.equal(c.canResumeCurrentTurn(),false,'saved stopped status invented a resumable turn');
 c.applyResumeSnapshot(snapshot('A',''),'A');
 assert.equal(c.canResumeCurrentTurn(),false,'available flag without a token offered Resume');
 c.applyResumeSnapshot(snapshot('A','A-1'),'A');
 assert.equal(c.canResumeCurrentTurn(),true);
 c.currentSessionId='B';assert.equal(c.canResumeCurrentTurn(),false,'A token leaked into B');
 c.applyResumeSnapshot(snapshot('B','B-1'),'B');assert.equal(c.canResumeCurrentTurn(),true);
 c.currentSessionId='A';assert.equal(c.canResumeCurrentTurn(),true,'B snapshot erased A state');
 c.applyResumeSnapshot({resumeAvailable:false,resumeToken:'',resumeSessionId:'A'},'A');
 assert.equal(c.canResumeCurrentTurn(),false,'latest explicit unavailable state retained Resume');
 c.currentSessionId='B';assert.equal(c.canResumeCurrentTurn(),true,'clearing A erased B token');
`},
		{"composer_action", `
 const f=context(),{c}=f;
 c.applyResumeSnapshot(snapshot('A','A-1'),'A');c.updateSendBtn();
 assert.equal(f.el('sendBtn').disabled,false,'empty interrupted composer cannot be resumed');
 assert.match(f.el('sendBtn').getAttribute('aria-label')||f.el('sendBtn').title,/Resume|Continue/i);
 f.el('inputField').value='A new prompt';c.updateSendBtn();
 assert.equal(f.el('sendBtn').disabled,false);
 assert.match(f.el('sendBtn').getAttribute('aria-label')||f.el('sendBtn').title,/Send/i,'draft must retain Send action');
 const send=c.sendMessage();await tick();
 assert.equal(f.requests.length,1);assert.equal(f.requests[0].body.input,'A new prompt');
 assert.equal(f.requests[0].body.resume,undefined,'typed draft unexpectedly resumed the old turn');
 f.requests[0].pending.resolve(ok({sessionId:'A',text:'done'}));await send;
 const g=context();g.c.applyResumeSnapshot(snapshot('A','A-2'),'A');
 g.c.attachments=[{mediaType:'image/png',data:'base64'}];g.c.updateSendBtn();
 assert.match(g.el('sendBtn').getAttribute('aria-label')||g.el('sendBtn').title,/Send/i,'attachment must retain Send action');
 const image=g.c.sendMessage();await tick();
 assert.equal(g.requests.length,1);assert.equal(g.requests[0].body.images.length,1);
 assert.equal(g.requests[0].body.resume,undefined,'attachment unexpectedly resumed the old turn');
 g.requests[0].pending.resolve(ok({sessionId:'A',text:'done'}));await image;
`},
		{"resume_guards", `
 const blockers=[
  ['pending stop',f=>evaluate(f.c,"pendingStopSessions.add('A')")],
  ['foreground owner',f=>evaluate(f.c,"foregroundRequests.set('A',{sessionId:'A'})")],
  ['legacy foreground owner',f=>f.c.pendingForegroundRequest={sessionId:'A'}],
  ['session navigation',f=>f.c.pendingSessionId='B'],
  ['workspace navigation',f=>f.c.workspaceSelectionPending=true],
  ['unavailable workspace',f=>{f.c.workspaceCanRun=false;f.c.workspaces=[{id:'ws-A',available:false}];}],
  ['live turn',f=>{f.c.turnRunning=true;f.c.runningSessionId='A';f.c.lastStatusSnapshot.isolatedTurnSessions=['A'];}],
  ['pending question',f=>f.c.pendingAsk={question:'Confirm?'}],
 ];
 for(const [label,block] of blockers){
  const f=context();f.c.applyResumeSnapshot(snapshot('A','A-1'),'A');block(f);
  assert.equal(f.c.canResumeCurrentTurn(),false,label+' allowed Resume');
  await f.c.resumeCurrentTurn();assert.equal(f.requests.length,0,label+' sent a resume request');
 }
 const f=context();f.c.applyResumeSnapshot(snapshot('A','A-1'),'A');
 evaluate(f.c,"foregroundRequests.set('B',{sessionId:'B'});pendingStopSessions.add('B')");
 assert.equal(f.c.canResumeCurrentTurn(),true,'another session\'s request blocked idle A');
`},
		{"double_click_and_request_binding", `
 const f=context(),{c}=f;c.activeWorkspaceId='ws-A';c.resumeSessionGeneration=7;
 c.pollSnapshot={resumeAvailable:false,resumeToken:'',resumeSessionId:'A'};
 c.applyResumeSnapshot(snapshot('A','A-1'),'A');
 const first=c.resumeCurrentTurn(),second=c.resumeCurrentTurn();await tick();
 assert.equal(f.requests.length,1,'double click issued duplicate resume requests');
 const request=f.requests[0];assert.equal(request.url,'/api/turns');
 assert.deepEqual(Object.keys(request.body).sort(),['resume','resumeToken','sessionId']);
 assert.deepEqual(request.body,{sessionId:'A',resume:true,resumeToken:'A-1'});
 const owner=evaluate(c,"foregroundRequests.get('A')");
 assert.equal(owner.sessionId,'A');assert.equal(owner.workspaceId,'ws-A');assert.equal(owner.viewGeneration,7);
 assert.equal(f.added.filter(m=>m.role==='user').length,0,'resume appended a fake user message');
 request.pending.resolve(ok({sessionId:'A',text:'continued',resumeAvailable:false,resumeToken:'',resumeSessionId:'A'}));
 await Promise.all([first,second]);assert.equal(c.canResumeCurrentTurn(),false);
 assert.equal(f.errors.length,0);
`},
		{"navigation_during_resume", `
 const f=context(),{c}=f;c.activeWorkspaceId='ws-A';c.resumeSessionGeneration=3;
 c.applyResumeSnapshot(snapshot('A','A-1'),'A');const turn=c.resumeCurrentTurn();await tick();
 c.currentSessionId='B';c.activeWorkspaceId='ws-B';c.resumeSessionGeneration++;
 evaluate(c,"foregroundRequests.set('B',{sessionId:'B',workspaceId:'ws-B',viewGeneration:4})");
 c.syncTrackedRunningState();
 f.requests[0].pending.resolve(ok({sessionId:'A',text:'A continued',resumeAvailable:false,resumeToken:'',resumeSessionId:'A'}));await turn;
 assert.equal(c.currentSessionId,'B');assert.equal(c.activeWorkspaceId,'ws-B');
 assert.equal(c.runningSessionId,'B');assert.equal(c.isViewedTurnRunning(),true,'A completion cleared B controls');
 assert.equal(evaluate(c,"foregroundRequests.has('B')"),true);
 assert.equal(f.added.filter(m=>m.session==='B').length,0,'A fallback was appended to B');
 assert.equal(f.timers.filter(fn=>fn===c.drainQueuedTurns).length,0,'A completion drained B queue');
`},
		{"pause_preserves_queue", `
 const f=context(),{c}=f;c.queuedTurns=[{text:'queued one'},{text:'queued two'}];c.queuedSessionId='A';c.saveSessionQueue();
 c.pollSnapshot=snapshot('A','A-2');
 const turn=c.runTurnItem({text:'work',images:[]});await tick();const pause=c.pauseTurn();await tick();
 assert.equal(f.requests.length,2);assert.equal(f.requests[1].url,'/api/stop');
 assert.deepEqual(f.requests[1].body,{sessionId:'A'});
 f.requests[1].pending.resolve(ok({stopped:true,...snapshot('A','A-2')}));await pause;
 assert.deepEqual(Array.from(c.queuedTurns,x=>x.text),['queued one','queued two'],'pause discarded pending messages');
 f.requests[0].pending.resolve(ok({sessionId:'A',stopped:true,...snapshot('A','A-2')}));
 assert.equal(await turn,false,'stopped turn was marked successful');
 assert.equal(f.timers.filter(fn=>fn===c.drainQueuedTurns).length,0,'stopped POST automatically drained queued messages');
 assert.equal(c.canResumeCurrentTurn(),true,'settled paused turn did not expose Resume');
 const count=f.requests.length;await c.drainQueuedTurns();assert.equal(f.requests.length,count,'paused queue drained without Resume');
`},
		{"pause_navigation_keeps_queue_owners", `
 const f=context(),{c}=f;c.queuedTurns=[{text:'A queued'}];c.queuedSessionId='A';c.saveSessionQueue();
 const turn=c.runTurnItem({text:'A work',images:[]});await tick();const pause=c.pauseTurn();await tick();
 c.currentSessionId='B';c.activeWorkspaceId='ws-B';c.resumeSessionGeneration++;
 c.restoreSessionQueue('B');c.queuedTurns=[{text:'B queued'}];c.queuedSessionId='B';c.saveSessionQueue();
 f.requests[1].pending.resolve(ok({stopped:true}));await pause;
 assert.equal(c.queuedTurns[0].text,'B queued','A pause changed B queue');
 f.requests[0].pending.resolve(ok({sessionId:'A',stopped:true}));await turn;
 assert.equal(c.queuedTurns[0].text,'B queued','A stopped POST changed B queue');
 const selection=c.resumeSession('A');await tick();
 f.requests[2].pending.resolve(ok({session:{id:'A',status:'stopped'},messages:[],...snapshot('A','A-paused')}));await selection;
 assert.equal(c.queuedTurns[0].text,'A queued','opening A lost its paused queue');
 assert.equal(f.timers.filter(fn=>fn===c.drainQueuedTurns).length,0,'opening paused history restarted A queue');
 c.currentSessionId='B';c.restoreSessionQueue('B');assert.equal(c.queuedTurns[0].text,'B queued');
`},
		{"accepted_stop_waits_for_settlement", `
 const f=context(),{c}=f;const turn=c.runTurnItem({text:'work',images:[]});await tick();
 c.pollSnapshot={resumeAvailable:false,resumeSessionId:'A',turnRunning:true,runningSessionId:'A'};
 const pause=c.pauseTurn();await tick();
 f.requests[1].pending.resolve(ok({stopping:true},202));await pause;
 assert.equal(evaluate(c,"pendingStopSessions.has('A')"),true,'202 released stop ownership before the turn settled');
 c.applyResumeSnapshot({...snapshot('A','early-token'),turnRunning:true,runningSessionId:'A'},'A');assert.equal(c.canResumeCurrentTurn(),false,'in-flight stop allowed Resume');
 c.pollSnapshot=snapshot('A','settled-token');
 f.requests[0].pending.resolve(ok({sessionId:'A',stopped:true,...snapshot('A','settled-token')}));await turn;
 assert.equal(evaluate(c,"pendingStopSessions.has('A')"),false,'settled turn retained stop ownership');
 assert.equal(c.canResumeCurrentTurn(),true);
`},
		{"successful_resume_restarts_preserved_queue", `
 const f=context(),{c}=f;c.queuedTurns=[{text:'run exactly this follow-up',images:[]}];c.queuedSessionId='A';c.saveSessionQueue();
 const original=c.runTurnItem({text:'original task',images:[]});await tick();const pause=c.pauseTurn();await tick();
 c.pollSnapshot=snapshot('A','settled-token');
 f.requests[0].pending.resolve(ok({sessionId:'A',stopped:true}));assert.equal(await original,false);
 assert.equal(evaluate(c,"pendingStopSessions.has('A')"),true,'turn completion released an outstanding stop request');
 await c.drainQueuedTurns();await c.resumeCurrentTurn();
 assert.equal(f.requests.length,2,'outstanding stop allowed queued work or Resume');
 assert.equal(f.timers.filter(fn=>fn===c.drainQueuedTurns).length,0,'outstanding stop scheduled queue drain');
 f.requests[1].pending.resolve(ok({stopped:true}));await pause;
 assert.equal(c.canResumeCurrentTurn(),true);
 const resume=c.resumeCurrentTurn();await tick();
 assert.deepEqual(f.requests[2].body,{sessionId:'A',resume:true,resumeToken:'settled-token'});
 assert.equal(c.queuedTurns.length,1,'submitting Resume consumed queued input');
 assert.equal(f.timers.filter(fn=>fn===c.drainQueuedTurns).length,0,'resume submission scheduled queue drain before success');
 c.pollSnapshot={resumeAvailable:false,resumeToken:'',resumeSessionId:'A'};
 f.requests[2].pending.resolve(ok({sessionId:'A',text:'original task completed'}));assert.equal(await resume,true);
 const drains=f.timers.filter(fn=>fn===c.drainQueuedTurns);
 assert.equal(drains.length,1,'successful Resume did not schedule exactly one queue drain');
 const queued=drains[0]();await tick();assert.equal(f.requests.length,4);
 assert.equal(f.requests[3].body.input,'run exactly this follow-up','drained queue changed the preserved input');
 assert.equal(f.requests[3].body.resume,undefined,'queued input was sent as another resume');
 assert.equal(f.requests[3].body.resumeToken,undefined,'queued input inherited the checkpoint token');
 assert.equal(f.requests[3].body.sessionId,'A');
 f.requests[3].pending.resolve(ok({sessionId:'A',text:'follow-up completed'}));await queued;
 assert.equal(c.queuedTurns.length,0);assert.equal(c.drainingQueuedTurns,false);
 assert.equal(f.added.filter(m=>m.role==='user').length,2,'resume inserted a user row between original and queued input');
`},
		{"composer_controls_follow_language", `
 const f=context(),{c}=f;
 vm.runInContext(extract('app.js','const DESKTOP_I18N','function presetDisplayName('),c);
 const label=(id,text)=>{const button=f.el(id);assert.equal(button.title,text);assert.equal(button.getAttribute('aria-label'),text,'tooltip and accessible action differ');};
 for(const [lang,resume,send,pause,pausing] of [
  ['en','Resume current task','Send message','Pause current task','Pausing current task…'],
  ['zh-CN','恢复当前任务','发送消息','暂停当前任务','正在暂停当前任务…'],
 ]){
  evaluate(c,"pendingStopSessions.delete('A')");
  c.lastStatusSnapshot={isolatedTurnsEnabled:true,isolatedTurnSessions:[],turnRunning:false};
  c.turnRunning=false;c.runningSessionId=null;f.el('inputField').value='';c.attachments=[];
  c.applyResumeSnapshot(snapshot('A','locale-token'),'A');c.applyLanguage(lang);
  assert.equal(c.document.documentElement.lang,lang);label('sendBtn',resume);
  f.el('inputField').value='new draft';c.applyLanguage(lang);label('sendBtn',send);
  c.lastStatusSnapshot={isolatedTurnsEnabled:true,isolatedTurnSessions:['A'],turnRunning:true,runningSessionId:'A'};
  c.applyLanguage(lang);label('stopBtn',pause);
  assert.equal(f.el('sendBtn').style.display,'none');assert.equal(f.el('stopBtn').style.display,'');
  evaluate(c,"pendingStopSessions.add('A')");c.applyLanguage(lang);label('stopBtn',pausing);
  assert.equal(f.el('stopBtn').disabled,true);
 }
`},
		{"rejected_resume_clears_play_and_preserves_queue", `
 const f=context(),{c}=f;c.queuedTurns=[{text:'keep this queued'}];c.queuedSessionId='A';c.saveSessionQueue();
 c.applyResumeSnapshot(snapshot('A','expired-token'),'A');loadPollStatus(c);
 const resume=c.resumeCurrentTurn();await tick();
 f.requests[0].pending.resolve({ok:false,status:409,json:async()=>({error:'Checkpoint invalid'})});await tick();
 assert.equal(f.requests[1].url,'/api/status?sessionId=A','409 did not refresh checkpoint from its owner');
 f.requests[1].pending.resolve(ok({resumeAvailable:false,resumeToken:'',resumeSessionId:'A'}));assert.equal(await resume,false);
 assert.deepEqual(f.errors,['Checkpoint invalid']);assert.equal(c.canResumeCurrentTurn(),false);
 assert.equal(f.el('sendBtn').disabled,true,'409 left empty composer enabled for Resume');
 assert.equal(f.el('sendBtn').getAttribute('aria-label'),'Send message');
 assert.equal(c.queuedTurns[0].text,'keep this queued');assert.equal(c.isSessionQueuePaused('A'),true);
 assert.equal(f.timers.filter(fn=>fn===c.drainQueuedTurns).length,0,'409 scheduled queued input');
 await c.drainQueuedTurns();assert.equal(f.requests.length,2,'409 unpaused preserved queue');
 assert.equal(f.added.filter(m=>m.role==='user').length,0,'rejected Resume added a synthetic user row');
`},
		{"history_refreshes_metadata", `
 const f=context(),{c}=f;c.sessions=[{id:'A',title:'old title',status:'running',resumeToken:'obsolete'}];
 c.turnRunning=true;c.runningSessionId='B';c.lastStatusSnapshot.isolatedTurnSessions=['B'];
 const selection=c.resumeSession('A');await tick();
 assert.equal(f.requests[0].url,'/api/sessions/A');assert.equal(f.requests[0].options.method,'GET');
 f.requests[0].pending.resolve(ok({session:{id:'A',title:'latest title',status:'stopped'},messages:['saved history'],...snapshot('A','fresh-token')}));
 await selection;assert.equal(c.sessions.find(s=>s.id==='A').title,'latest title','existing sidebar record kept stale metadata');
 assert.equal(c.sessions.find(s=>s.id==='A').status,'stopped');assert.equal(c.canResumeCurrentTurn(),true);
 const resume=c.resumeCurrentTurn();await tick();assert.equal(f.requests[1].body.resumeToken,'fresh-token','history used cached token');
 f.requests[1].pending.resolve(ok({sessionId:'A',text:'done',resumeAvailable:false,resumeToken:'',resumeSessionId:'A'}));await resume;
`},
		{"history_snapshot_requires_owner", `
 const f=context(),{c}=f;c.turnRunning=true;c.runningSessionId='B';c.lastStatusSnapshot.isolatedTurnSessions=['B'];
 const selection=c.resumeSession('A');await tick();
 f.requests[0].pending.resolve(ok({session:{id:'A',status:'stopped'},messages:[],...snapshot('B','foreign-token')}));await selection;
 assert.equal(c.currentSessionId,'A');assert.equal(c.canResumeCurrentTurn(),false,'GET accepted another session\'s checkpoint');
`},
		{"status_snapshot_requires_owner", `
 const f=context(),{c}=f;loadPollStatus(c);
 let status=c.pollStatus();await tick();assert.equal(f.requests[0].url,'/api/status?sessionId=A');
 f.requests[0].pending.resolve(ok(snapshot('B','foreign-token')));await status;
 assert.equal(c.canResumeCurrentTurn(),false,'status accepted unowned checkpoint');
 status=c.pollStatus();await tick();f.requests[1].pending.resolve(ok(snapshot('A','owned-token')));await status;
 assert.equal(c.canResumeCurrentTurn(),true,'owned status did not expose Resume');
 status=c.pollStatus();await tick();c.currentSessionId='B';c.resumeSessionGeneration++;
 f.requests[2].pending.resolve(ok(snapshot('A','stale-token')));await status;
 assert.equal(c.canResumeCurrentTurn(),false,'stale A status entered B');
 c.currentSessionId='A';c.resumeSessionGeneration++;
 status=c.pollStatus();await tick();c.resumeSessionGeneration+=2;
 f.requests[3].pending.resolve(ok(snapshot('A','stale-ABA-token')));await status;
 const resume=c.resumeCurrentTurn();await tick();assert.equal(f.requests[4].body.resumeToken,'owned-token','stale view generation replaced valid token');
 f.requests[4].pending.resolve(ok({sessionId:'A',text:'done'}));await tick();
 assert.equal(f.requests[5].url,'/api/status?sessionId=A','completed resume did not refresh server checkpoint');
 f.requests[5].pending.resolve(ok({resumeAvailable:false,resumeToken:'',resumeSessionId:'A'}));await resume;
`},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			runDesktopStatsNode(t, resumeTurnBrowserFixture+"\nconst deadline=setTimeout(()=>{console.error('browser scenario did not settle');process.exit(1);},3000);\n(async()=>{\n"+scenario.body+"\n})().then(()=>clearTimeout(deadline),error=>{clearTimeout(deadline);console.error(error);process.exitCode=1;});", "app.js", "chat.js", "sessions.js")
		})
	}
}

const resumeTurnBrowserFixture = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const sources=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
function extract(file,start,next){const source=sources[file],a=source.indexOf(start),b=next?source.indexOf(next,a+start.length):source.length;assert(a>=0&&b>a,start);return source.slice(a,b);}
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const ok=(data,status=200)=>({ok:true,status,json:async()=>data});
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const snapshot=(id,token)=>({resumeAvailable:true,resumeToken:token,resumeSessionId:id});
const evaluate=(c,script)=>vm.runInContext(script,c);
function loadPollStatus(c){vm.runInContext(extract('app.js','async function pollStatus(','function closeContextMeterPopover('),c);}
function context(){
 const requests=[],errors=[],added=[],rendered=[],timers=[],statusPolls=[],elements=new Map();
 const el=id=>{if(!elements.has(id))elements.set(id,{value:'',style:{},dataset:{},disabled:false,title:'',textContent:'',innerHTML:'',attrs:{},remove(){},classList:{add(){},remove(){},toggle(){}},setAttribute(name,value){this.attrs[name]=String(value)},getAttribute(name){return this.attrs[name]}});return elements.get(id);};
 const c={Map,Set,Array,AbortController,URLSearchParams,console,window:{},document:{documentElement:{lang:'en'},getElementById:el,querySelector:()=>null,querySelectorAll:()=>[]},
  lastStatusSnapshot:{isolatedTurnsEnabled:true,isolatedTurnSessions:[],turnRunning:false},statusRequestGeneration:0,
  currentSessionId:'A',activeWorkspaceId:'ws-A',resumeSessionGeneration:0,pendingSessionId:null,currentView:'chat',
  runningSessionId:null,turnRunning:false,stopRequestPending:false,pendingForegroundRequest:null,pendingAsk:null,
  queuedTurns:[],queuedSessionId:null,queuedTurnMenuIndex:-1,queuedTurnPendingItem:null,drainingQueuedTurns:false,
  sessions:[{id:'A'},{id:'B'}],workspaces:[],attachments:[],desktopPreferences:{busyEnter:'queue'},
  backgroundContinuationGenerations:new Map(),backgroundContinuationGeneration:0,runningTurnIncompleteReason:'',runningTurnNeedsHistorySync:false,
  streamedTextThisTurn:false,messages:[],workspaceSelectionPending:false,workspaceCanRun:true,
  isWorkspaceSelectionPending:()=>c.workspaceSelectionPending,selectedWorkspaceCanRun:()=>c.workspaceCanRun,applyLayout(){},
  clearTodoPlan(){},resumeAutoScroll(){},beginUserTurn(){},finishUserTurn(){},renderTodoPlan(){},renderSessions(){},
  renderAttachments(){},autoResize(){},closeCommandMenu(){},executeComposerCommand:async()=>false,
  renderQueuedTurns(){if(typeof c.saveSessionQueue==='function')c.saveSessionQueue();},
  addMessage:(role,text)=>added.push({session:c.currentSessionId,role,text}),
  setTurnRunning:(running,id)=>{c.turnRunning=running;c.runningSessionId=running?id:null;},
  showError:error=>errors.push(error),showToast(){},loadSessions:async()=>{},loadSessionStatsbar(){},
  loadArtifactsForSession:async()=>{},loadSessionFiles(){},restoreCompactionHistory:async()=>{},loadEffort:async()=>{},
  pollStatus:async(shouldApply=()=>true)=>{statusPolls.push(c.currentSessionId);if(c.pollSnapshot&&shouldApply())c.applyResumeSnapshot(c.pollSnapshot,c.currentSessionId);},
  renderHistoryMessages:history=>rendered.push({session:c.currentSessionId,history}),
  resetTraceForSession(){},resetSessionFiles(){},resetArtifactsForSession(){},loadTrace(){},detachRunningTurnView(){},
  closeMismatchedSubAgentDetails(){},setTimeout:fn=>{timers.push(fn);},
 };
 vm.createContext(c);
 vm.runInContext(extract('chat.js','function uiText(en, zh)','const SETTINGS_ZH'),c);
 vm.runInContext(extract('chat.js','const foregroundRequests = new Map();','function syncTurnControls()'),c);
 vm.runInContext(extract('chat.js','function syncTurnControls()','function setTurnRunning('),c);
 vm.runInContext(extract('chat.js','async function pauseTurn()','function startStreamingMessage('),c);
 vm.runInContext(extract('chat.js','async function sendMessage(busyBehavior)','function queuedTurnIcon('),c);
 vm.runInContext(extract('chat.js','async function drainQueuedTurns()','const MESSAGE_ACTION_ICONS'),c);
 vm.runInContext(extract('chat.js','function updateSendBtn()','// --- Model ---'),c);
 vm.runInContext(extract('sessions.js','function mergeServerSessionMetadata(','function isWorkspaceSelectionPending('),c);
 vm.runInContext(extract('sessions.js','async function resumeSession(id)'),c);
 c.renderStatusSnapshot=()=>c.syncTrackedRunningState();
 c.fetch=(url,options={})=>{const pending=deferred();requests.push({url,options,body:JSON.parse(options.body||'{}'),pending});return pending.promise;};
 return{c,requests,errors,added,rendered,timers,statusPolls,el};
}
`
