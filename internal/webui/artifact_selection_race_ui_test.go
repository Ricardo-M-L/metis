package webui

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

func runSelectionRaceBrowserHarness(t *testing.T, sourceName, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile(sourceName)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("selection race browser harness: %v\n%s", err, out)
	}
}

func TestArtifactSelectionBrowserRaces(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/artifacts.js", `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
function element() {
  return {hidden:true, textContent:'', children:[], style:{}, classList:{add(){},remove(){},toggle(){}},
    replaceChildren(){this.children=[];}, appendChild(child){this.children.push(child);},
    removeAttribute(name){delete this[name];}, setAttribute(){}, focus(){}, querySelector(){return null;}};
}
function harness() {
  const nodes = Object.fromEntries(['artifactPreviewOverlay','artifactPreviewFrame','artifactPreviewState','artifactPreviewMeta','artifactPreviewTitle','artifactVersionSelect','artifactDeleteOverlay','artifactDeleteConfirm'].map(id=>[id,element()]));
  const requests = [], toasts = [], opened = [];
  const c = {currentSessionId:'A', URL, AbortController, console, CSS:{escape:value=>value},
    window:{location:{href:'http://localhost/',origin:'http://localhost'},open:url=>{opened.push(url);return {}; }},
    document:{activeElement:null,body:{classList:{add(){},remove(){}}},getElementById:id=>nodes[id]||null,querySelector:()=>null,querySelectorAll:()=>[],createElement:()=>element(),addEventListener(){}},
    requestAnimationFrame:fn=>fn(),showToast:message=>toasts.push(message),
    fetch:(url,init)=>new Promise((resolve,reject)=>requests.push({url,init,resolve,reject}))};
  vm.createContext(c); vm.runInContext(source,c);
  const state = () => vm.runInContext('artifactState',c);
  const reply = (request,data) => request.resolve({ok:true,headers:{get:()=> 'application/json'},json:async()=>data});
  return {c,nodes,requests,toasts,opened,state,reply};
}
const artifact = (id,sid='A')=>({id,sessionId:sid,title:id,currentVersion:2,versions:[{number:1},{number:2}]});
async function settle(){await new Promise(resolve=>setImmediate(resolve));}
(async()=>{
  // A detail resolves after switching to B and loading B's gallery.
  {
    const h=harness(), {c,requests,state,reply,nodes}=h;
    const opening=c.previewArtifactByID('a');
    c.currentSessionId='B'; c.resetArtifactsForSession();
    const loading=c.loadArtifactsForSession('B'); reply(requests[1],{artifacts:[artifact('b','B')]}); await loading;
    reply(requests[0],artifact('a'));
    await settle(); for(const request of requests.slice(2)) reply(request,{previewUrl:'/api/artifacts/a/preview'});
    await opening;
    assert.equal(state().active,null,'A detail must not reopen after session switch');
    assert.equal(JSON.stringify(state().artifacts.map(item=>item.id)),JSON.stringify(['b']),'A detail must not contaminate B gallery');
    assert.equal(nodes.artifactPreviewOverlay.hidden,true);
  }
  // Explicit Close invalidates a pending open even when the session is unchanged.
  {
    const {c,requests,state,reply,nodes}=harness();
    const opening=c.previewArtifactByID('a'); c.closeArtifactPreview(); reply(requests[0],artifact('a'));
    await settle(); for(const request of requests.slice(1)) reply(request,{previewUrl:'/api/artifacts/a/preview'});
    await opening; assert.equal(state().active,null,'Close must invalidate pending detail');
    assert.equal(nodes.artifactPreviewOverlay.hidden,true);
  }
  // A failed old version must not overwrite the already loaded newer version.
  {
    const {c,requests,state,reply,nodes}=harness();
    const item=c.normalizeArtifact(artifact('a')); state().active=item; state().activeVersion=1;
    const old=c.loadArtifactPreviewURL(item,1); const latest=c.selectArtifactVersion(2);
    reply(requests[1],{previewUrl:'/api/artifacts/a/preview?version=2'}); await latest;
    nodes.artifactPreviewFrame.onload(); requests[0].reject(new Error('old version failed')); await old;
    assert.equal(state().previewURL,'http://localhost/api/artifacts/a/preview?version=2','stale error must preserve current URL');
    assert.equal(nodes.artifactPreviewState.hidden,true,'stale error must not cover latest preview');
  }
  // Leaving for a non-session page cancels pending detail without clearing the gallery.
  {
    const {c,requests,state,reply,nodes}=harness(),navigation=[];
    c.window.metisNavigation={recordArtifact:(id,version)=>navigation.push([id,version]),closeArtifact:()=>{throw new Error('page exit must bypass history close');}};
    state().artifacts=[c.normalizeArtifact(artifact('kept'))];
    const opening=c.previewArtifactByID('a');c.closeArtifactPreview({navigation:false});reply(requests[0],artifact('a'));
    await settle();for(const request of requests.slice(1))reply(request,{previewUrl:'/api/artifacts/a/preview'});await opening;
    assert.equal(state().active,null);assert.equal(nodes.artifactPreviewOverlay.hidden,true);
    assert.equal(state().artifacts.length,1);assert.equal(state().artifacts[0].id,'kept');assert.equal(navigation.length,0);
  }
  // Preparing and failed previews are navigable artifact pages, even before URL resolution.
  {
    const {c,requests,state,reply,nodes}=harness(),navigation=[];
    c.window.metisNavigation={recordArtifact:(id,version)=>navigation.push([id,version])};
    const opening=c.previewArtifactByID('a');reply(requests[0],artifact('a'));await settle();
    assert.equal(nodes.artifactPreviewOverlay.hidden,false);
    assert.equal(JSON.stringify(navigation),JSON.stringify([['a',2]]),'visible preparing shell must already have artifact navigation');
    requests[1].reject(new Error('preview service unavailable'));await opening;
    assert.equal(state().active.id,'a');assert.match(nodes.artifactPreviewState.textContent,/unavailable/);
    assert.equal(navigation.length,1,'failed URL must retain artifact navigation');
    const changing=c.selectArtifactVersion(1);
    assert.equal(JSON.stringify(navigation),JSON.stringify([['a',2],['a',1]]),'selected version must navigate before URL resolves');
    requests[2].reject(new Error('historical preview unavailable'));await changing;
    assert.match(nodes.artifactPreviewMeta.textContent,/Version 1/,'failed selected version keeps accurate shell metadata');
    assert.equal(state().activeVersion,1);assert.equal(navigation.length,2);
  }
  // A list failure after currentSessionId changed cannot erase the new view.
  {
    const {c,requests,state,toasts}=harness(); const loading=c.loadArtifactsForSession('A');
    c.currentSessionId='B'; state().sessionId='B'; state().artifacts=[c.normalizeArtifact(artifact('b','B'))];
    requests[0].reject(new Error('old session failed')); await loading;
    assert.equal(state().artifacts[0]?.id,'b','stale list failure must not clear B');
    assert.equal(toasts.length,0,'stale list failure must not announce an error in B');
  }
  // A late caller already targeting A must leave B state untouched.
  {
    const {c,requests,state,reply}=harness(); c.currentSessionId='B'; state().sessionId='B'; state().artifacts=[c.normalizeArtifact(artifact('b','B'))];
    const loading=c.loadArtifactsForSession('A'); if(requests[0]) reply(requests[0],{artifacts:[]}); await loading;
    assert.equal(state().sessionId,'B','stale caller must not change gallery ownership');
    assert.equal(state().artifacts[0]?.id,'b');
  }
  // Normal opens/version changes still display, and implicit resets do not add navigation entries.
  {
    const {c,requests,state,reply,nodes}=harness(), navigation=[];
    c.window.metisNavigation={recordArtifact:(id,version)=>navigation.push([id,version]),closeArtifact:()=>{throw new Error('implicit reset must not navigate');}};
    const opening=c.previewArtifactByID('a');reply(requests[0],artifact('a'));await settle();
    reply(requests[1],{previewUrl:'/api/artifacts/a/preview?version=2'});await opening;
    assert.equal(state().active.id,'a');assert.equal(nodes.artifactPreviewOverlay.hidden,false);
    const oldOnLoad=nodes.artifactPreviewFrame.onload;
    const version=c.selectArtifactVersion(1);oldOnLoad();assert.equal(nodes.artifactPreviewState.hidden,false,'old iframe load must not hide new loading state');
    reply(requests[2],{previewUrl:'/api/artifacts/a/preview?version=1'});await version;
    assert.equal(JSON.stringify(navigation),JSON.stringify([['a',2],['a',1]]));
    c.resetArtifactsForSession();assert.equal(state().active,null);assert.equal(nodes.artifactPreviewOverlay.hidden,true);
  }
  // A delayed external-open resolution cannot open A after the user moved to B.
  {
    const {c,requests,state,reply,opened}=harness();state().active=c.normalizeArtifact(artifact('a'));state().activeVersion=2;
    const opening=c.openActiveArtifactExternally();c.currentSessionId='B';c.resetArtifactsForSession();
    reply(requests[0],{previewUrl:'/api/artifacts/a/preview?version=2'});await opening;
    assert.equal(opened.length,0,'old external-open must not launch after session switch');
  }
  // An approved delete of A completes, but cannot close the newly viewed B preview.
  {
    const {c,requests,state,reply,nodes,toasts}=harness();state().active=c.normalizeArtifact(artifact('a'));state().activeVersion=2;
    const deleting=c.confirmArtifactDeletion();c.currentSessionId='B';c.resetArtifactsForSession();
    const b=c.normalizeArtifact(artifact('b','B'));state().active=b;state().activeVersion=2;state().artifacts=[b];c.openArtifactPreviewShell(b);
    reply(requests[0],{});await deleting;
    assert.equal(state().active?.id,'b','old delete completion must not close B preview');
    assert.equal(nodes.artifactPreviewOverlay.hidden,false);assert.equal(state().artifacts[0].id,'b');
    assert.equal(toasts.length,0,'old deletion result must not show a toast in B');
  }
})().catch(error=>{console.error(error);process.exitCode=1;});
`)
}

func TestTraceSelectionBrowserRaces(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", `
const assert=require('node:assert/strict'), vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
function harness(){
  const nodes=new Map(), requests=[], rendered=[];
  const get=id=>{if(!nodes.has(id))nodes.set(id,{innerHTML:'',style:{},classList:{toggle(){},remove(){}}});return nodes.get(id);};
  const c={currentSessionId:'A',URLSearchParams,console,document:{getElementById:get,querySelectorAll:()=>[]},
    escHtml:value=>String(value),showToast(){},fetch:url=>new Promise((resolve,reject)=>requests.push({url,resolve,reject})),
    capture:rows=>rendered.push(rows.map(row=>row.text))};
  vm.createContext(c);vm.runInContext(source,c);
  vm.runInContext('renderTrace = () => capture(traceRows);',c);
  const seed=()=>vm.runInContext("traceEvents=[{id:'old',kind:'text',text:'A history'}];traceRows=[{text:'A history'}];traceNextCursor='A-cursor';traceTotalEvents=99;traceSelectedIdx=0;traceSelectedReq=1;traceFoldedTurns.add(1);traceFoldedAssistants.add(0);",c);
  return {c,nodes,requests,rendered,seed,state:()=>vm.runInContext('({events:traceEvents,rows:traceRows,cursor:traceNextCursor,total:traceTotalEvents,selected:traceSelectedIdx,request:traceSelectedReq,folds:traceFoldedTurns.size+traceFoldedAssistants.size})',c)};
}
(async()=>{
  {
    const {c,requests,seed,state}=harness();seed();
    const old=c.loadTrace(false,'A'); c.currentSessionId='B'; c.resetTraceForSession();
    assert.equal(state().rows.length,0);assert.equal(state().cursor,'');assert.equal(state().total,0);assert.equal(state().selected,-1);assert.equal(state().request,-1);assert.equal(state().folds,0);
    await c.loadTrace(true,'B');assert.equal(requests.length,1,'B cannot paginate with A cursor');
    requests[0].resolve({ok:true,json:async()=>({enabled:true,events:[{kind:'text',text:'late A'}],nextCursor:'late-A'})});await old;
    assert.equal(state().rows.length,0,'delayed A response must not restore old rows');
  }
  for(const outcome of ['disabled','error']){
    const {c,requests,seed,state,rendered}=harness();seed();c.currentSessionId='B';
    // loadTrace also needs to scope its own state if called directly by a tab.
    const loading=c.loadTrace(false,'B');
    assert.equal(state().rows.length,0,'old rows must disappear while B is loading');
    if(outcome==='disabled')requests[0].resolve({ok:true,json:async()=>({enabled:false})});else requests[0].reject(new Error('B unavailable'));
    await loading;c.onTraceSearch('A');
    assert.equal(state().events.length,0);assert.equal(state().rows.length,0);
    assert.equal(state().cursor,'');assert.equal(rendered.at(-1).length,0,'search cannot redraw A after B load failed/disabled');
  }
})().catch(error=>{console.error(error);process.exitCode=1;});
`)
}
