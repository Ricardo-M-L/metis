package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// Execute the actual Desktop interaction handlers with deferred network
// responses; source assertions cannot exercise cross-session reply races.
func TestConcurrentDesktopInteractionsUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"chat.js", "sessions.js", "app.js"} {
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
 const from=sources[file].indexOf(start), to=sources[file].indexOf(next,from+start.length);
 assert(from>=0&&to>from,start);return sources[file].slice(from,to);
}
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const ok=data=>({ok:true,json:async()=>data});
function context() {
 const cards=[], input={placeholder:''},errors=[], sent=[], controls=[];
 const node=()=>({textContent:'',innerHTML:'',className:'',style:{display:'none'},attrs:{},
  setAttribute(key,value){this.attrs[key]=String(value);},querySelectorAll(){return[];},remove(){},
  classList:{add(){},remove(){},toggle(){}}});
 const chip=node(), pop=node();
 const area={insertAdjacentHTML(_position,html){
  const attrs=Object.fromEntries(Array.from(html.matchAll(/(data-[\w-]+)="([^"]*)"/g)).map(match=>[match[1],match[2]]));
  const card={attrs,isConnected:true,html,children:{},getAttribute:key=>attrs[key],classList:{add(){},remove(){}},querySelector(selector){return this.children[selector]||(this.children[selector]=node());},querySelectorAll(){return[];},remove(){this.isConnected=false;}};
  cards.push(card);
 }};
 const c={console,Map,Set,Array,URLSearchParams,window:{},currentSessionId:'A',runningSessionId:'A',turnRunning:true,
  pendingSessionId:null,showArchivedSessions:false,lastStatusSnapshot:null,statusRequestGeneration:0,stopRequestPending:false,
  DESKTOP_I18N:{en:{subAgents:'agents',backgroundTasks:'tasks'}},subAgentDetailState:{agentId:'',ownerSessionId:''},
  document:{documentElement:{lang:'en'},getElementById:id=>id==='chatArea'?area:id==='inputField'?input:id==='statusChip'?chip:id==='statusPopover'?pop:null,addEventListener(){},
   querySelectorAll:selector=>cards.filter(card=>card.isConnected&&(selector==='[data-ask]'?card.attrs['data-ask']:selector==='[data-perm]'?card.attrs['data-perm']:false)),querySelector:()=>null},
  sameSession:d=>String(d.session||'')===String(c.currentSessionId||''),
  uiText:en=>en,escHtml:String,escAttr:String,escOnclick:String,renderSessions(){},autoScroll(){},showToast:error=>errors.push(error),
  closeStatusPopover(){},setTurnRunning:(running,id)=>{c.turnRunning=running;c.runningSessionId=id;controls.push({running,id});},
  fetch:async(url,options)=>{sent.push({url,body:JSON.parse(options.body)});return ok({});},
 };
 vm.createContext(c);
 vm.runInContext('let activityGroupEl = null;' + extract('chat.js','function finishActivityGroup()','const THINK_ORBIT_ICON'),c);
 vm.runInContext(extract('chat.js','let pendingAsk = null;','let turnStartMs = 0;'),c);
 vm.runInContext(extract('chat.js','function handlePermissionRequest(d)','function isChatNearBottom('),c);
 vm.runInContext(extract('sessions.js','function sessionState(s)','function sessionStatusIcon('),c);
 vm.runInContext(extract('app.js','function subAgentText(en, zh)','function renderStatusPopover()'),c);
 vm.runInContext(extract('app.js','async function pollStatus(', 'function renderStatusSnapshot(d)'),c);
 vm.runInContext(extract('app.js','function renderStatusSnapshot(d)','setInterval(pollStatus'),c);
 vm.runInContext(extract('app.js','function renderStatusPopover()',"document.addEventListener('click'"),c);
 return {c,cards,input,errors,sent,controls,chip,pop,visible:()=>cards.filter(card=>card.isConnected),pending:()=>vm.runInContext('pendingAsk',c)};
}
(async()=>{
 // Background requests survive selection, duplicate SSE/snapshot frames do not duplicate cards.
 {
  const f=context(),{c}=f;
  c.handleAskUser({askId:'a',session:'A',question:'A?'});
  c.handleAskUser({askId:'b',session:'B',question:'B?'});
  assert.equal(f.visible().length,1);
  assert.equal(c.sessionState({id:'B'}).name,'waiting');
  c.currentSessionId='B';f.cards.forEach(card=>card.remove());
  c.fetch=async()=>ok({permissions:[],asks:[{askId:'b',session:'B',question:'B?'}]});
  await c.restorePendingInteractions('B');
  c.handleAskUser({askId:'b',session:'B',question:'B?'});
  assert.equal(f.visible().length,1);assert.equal(f.pending().id,'b');
 }
 // Each question button resolves its own ID; the composer chooses the oldest remaining question.
 {
  const f=context(),{c}=f;
  c.handleAskUser({askId:'first',session:'A',question:'First?'});
  c.handleAskUser({askId:'second',session:'A',question:'Second?'});
  assert.equal(f.pending().id,'first');
  await c.submitAskAnswer('yes','first');
  assert.deepEqual(f.sent[0].body,{id:'first',sessionId:'A',answer:'yes'});
  assert.equal(f.pending().id,'second');
 }
 // Finishing A's reply cannot clear B's question after navigation.
 {
  const f=context(),{c}=f, reply=deferred();
  c.handleAskUser({askId:'a',session:'A',question:'A?'});
  c.handleAskUser({askId:'b',session:'B',question:'B?'});
  c.fetch=()=>reply.promise;
  const request=c.submitAskAnswer('A answer');
  c.currentSessionId='B';c.syncPendingAsk();reply.resolve(ok({}));await request;
  assert.equal(f.pending().id,'b');assert.equal(f.pending().session,'B');
 }
 // An old session's snapshot cannot render into the newly selected session.
 {
  const f=context(),{c}=f, response=deferred();c.fetch=()=>response.promise;
  const request=c.restorePendingInteractions('A');c.currentSessionId='B';
  response.resolve(ok({permissions:[{permId:'old',session:'A'}],asks:[]}));await request;
  assert.equal(f.visible().length,0);
 }
 // A stale snapshot must neither revive a resolved request nor remove a newer live request.
 {
  const f=context(),{c}=f, response=deferred();c.fetch=()=>response.promise;
  const request=c.restorePendingInteractions('A');
  c.handlePermissionRequest({permId:'old',session:'A',tool:'Bash'});
  c.handleInteractionResolved({permId:'old',session:'A'});
  c.handleAskUser({askId:'new',session:'A',question:'Fresh?'});
  response.resolve(ok({permissions:[{permId:'old',session:'A',tool:'Bash'}],asks:[]}));await request;
  assert.equal(f.visible().length,1);assert.equal(f.pending().id,'new');
 }
 // Permission replies use the card's session even when the selection changes.
 {
  const f=context(),{c}=f;
  c.handlePermissionRequest({permId:'p',session:'A',tool:'Bash'});
  const card=f.visible()[0];c.currentSessionId='B';
  await c.resolvePermission('p',true,{closest:()=>card});
  assert.deepEqual(f.sent[0].body,{id:'p',sessionId:'A',approve:true});
  c.handleInteractionResolved({permId:'p',session:'A'});assert.equal(f.visible().length,0);
 }
 // Every live worker is marked running; a random backend iteration order cannot redirect Stop.
 {
  const f=context(),{c}=f;
  c.lastStatusSnapshot={turnRunning:true,runningSessionId:'B',isolatedTurnSessions:['B','A']};
  c.renderStatusSnapshot(c.lastStatusSnapshot);
  assert.equal(c.runningSessionId,'A');assert.equal(c.sessionState({id:'B',status:'running'}).name,'running');
  c.currentSessionId='B';c.renderStatusSnapshot({...c.lastStatusSnapshot,runningSessionId:'A'});
  assert.equal(c.runningSessionId,'B');
 }
 // The selected transcript owns the roster chip, even when the backend's
 // active Loop still belongs to a different session.
 {
  const f=context(),{c,chip,pop}=f;
  const snapshot={activeSessionId:'A',subAgents:9,backgroundTasks:8,
   agents:[{agentId:'agent-A',sessionId:'A',name:'Wrong session'}],jobs:[{id:'job-A',description:'Wrong job'}],
   viewRoster:{sessionId:'B',subAgents:1,backgroundTasks:1,
    agents:[{agentId:'agent-B',sessionId:'B',name:'Selected agent',status:'running'}],
    jobs:[{id:'job-B',description:'Selected job',status:'running'}]}};
  c.currentSessionId='B';c.lastStatusSnapshot=snapshot;c.renderStatusSnapshot(snapshot);
  assert.equal(chip.style.display,'');
  assert.match(chip.innerHTML,/1 agents/);assert.match(chip.innerHTML,/1 tasks/);
  c.renderStatusPopover();
  assert.match(pop.innerHTML,/Selected agent/);assert.match(pop.innerHTML,/Selected job/);
  assert.doesNotMatch(pop.innerHTML,/Wrong session|Wrong job/);
  c.currentSessionId='C';c.renderStatusSnapshot(snapshot);
  assert.equal(chip.style.display,'none','a mismatched viewRoster cannot appear in C');
  c.currentSessionId=null;c.renderStatusSnapshot(snapshot);
  assert.equal(chip.style.display,'none','blank composer has no selected roster');
 }
 // An in-flight B poll cannot repaint C after a selection change; the next
 // request must ask the server for C's view rather than its active Loop.
 {
  const f=context(),{c,chip}=f, response=deferred(),urls=[];
  c.currentSessionId='B';
  c.fetch=url=>{urls.push(url);return response.promise;};
  const stale=c.pollStatus();
  assert.equal(urls[0],'/api/status?sessionId=B');
  c.currentSessionId='C';
  response.resolve(ok({activeSessionId:'A',viewRoster:{sessionId:'B',subAgents:1,backgroundTasks:0,agents:[],jobs:[]}}));
  await stale;
  assert.equal(c.lastStatusSnapshot,null,'old selected-session response is discarded entirely');
  assert.equal(chip.style.display,'none');
  c.fetch=url=>{urls.push(url);return ok({activeSessionId:'A',viewRoster:{sessionId:'C',subAgents:0,backgroundTasks:0,agents:[],jobs:[]}});};
  await c.pollStatus();
  assert.equal(urls.at(-1),'/api/status?sessionId=C');
  assert.equal(c.lastStatusSnapshot.viewRoster.sessionId,'C');
 }
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("concurrent interaction UI checks failed: %v\n%s", err, output)
	}
}
