// Run with node scripts/e2e/artifact_annotation_ui_check.cjs.
// Check the actual host module: Artifact documents remain in an opaque iframe.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function element() {
  const attributes = new Map(), listeners = new Map();
  return {hidden:true,disabled:false,textContent:'',value:'',dataset:{},children:[],style:{},
    classList:{add(){},remove(){},toggle(){}},
    setAttribute(k,v){attributes.set(k,String(v));},getAttribute(k){return attributes.get(k)??null;},
    removeAttribute(k){attributes.delete(k);},focus(){},
    addEventListener(k,fn){if(!listeners.has(k))listeners.set(k,[]);listeners.get(k).push(fn);},
    dispatch(k,event){for(const fn of [...listeners.get(k)||[]])fn(event);},
    appendChild(child){this.children.push(child);},replaceChildren(){this.children=[];}};
}
function harness(options={}) {
  const ids=['artifactAnnotationToggle','artifactAnnotationPanel','artifactAnnotationHint','artifactAnnotationSelection','artifactAnnotationVersion','artifactAnnotationInstruction','artifactAnnotationApply','artifactAnnotationCancel','artifactCompareControls','artifactCompareBefore','artifactCompareAfter','artifactVersionSelect','artifactPreviewMeta','artifactPreviewFrame','artifactPreviewState','artifactPreviewOverlay'];
  const nodes=Object.fromEntries(ids.map(id=>[id,element()]));
  const hostListeners=new Map(),requests=[],submissions=[],toasts=[],loads=[];
  nodes.artifactPreviewFrame.contentWindow={};
  const ctx={console,URL,AbortController,currentSessionId:'session-a',
    artifactState:{active:{id:'artifact-a',sessionId:'session-a',mediaType:'text/html',title:'Demo',currentVersion:2,versions:[{number:2,digest:'a'.repeat(64)}]},activeVersion:2,previewSequence:0,sessionGeneration:0,previewURL:'http://127.0.0.1:9988/preview'},
    document:{documentElement:{lang:'zh-CN'},getElementById:id=>nodes[id]||null,querySelector:()=>null,createElement:()=>element(),
      addEventListener(k,fn){if(!hostListeners.has(k))hostListeners.set(k,[]);hostListeners.get(k).push(fn);}},
    window:{location:{href:'http://localhost/',origin:'http://localhost'},
      addEventListener(k,fn){if(!hostListeners.has(k))hostListeners.set(k,[]);hostListeners.get(k).push(fn);}},
    requestAnimationFrame:fn=>fn(),uiText:(en,zh)=>zh,showToast:message=>toasts.push(message),
    artifactAPIPath:(id,action,version,sid)=>'/api/artifacts/'+id+'/'+action+'?version='+version+'&sessionId='+sid,
    safeArtifactURL:value=>new URL(value,'http://localhost/').href,
    fetch:(url,init)=>new Promise((resolve,reject)=>requests.push({url,init,resolve,reject})),
    loadArtifactPreviewURL:async(item,version)=>{loads.push({item,version});nodes.artifactPreviewFrame.setAttribute('sandbox','');},
    selectArtifactVersion:async(version)=>{ctx.artifactState.activeVersion=version;loads.push({version});},
    submitArtifactAnnotationPrompt:async(prompt,reference,sessionId)=>{submissions.push({prompt,reference,sessionId});return true;},
    closeArtifactPreview:()=>{nodes.artifactPreviewOverlay.hidden=true;}};
  vm.createContext(ctx);
  if (options.realArtifacts) vm.runInContext(fs.readFileSync(path.resolve(__dirname,'../../internal/webui/static/artifacts.js'),'utf8'),ctx);
  vm.runInContext(fs.readFileSync(path.resolve(__dirname,'../../internal/webui/static/artifact_annotation.js'),'utf8'),ctx);
  const state=()=>vm.runInContext('artifactAnnotationState',ctx);
  const reply=(request,data,status=200)=>request.resolve({ok:status>=200&&status<300,status,headers:{get:()=> 'application/json'},json:async()=>data});
  const emit=data=>{for(const fn of hostListeners.get('message')||[])fn({origin:'null',source:nodes.artifactPreviewFrame.contentWindow,data});};
  return {ctx,nodes,requests,submissions,toasts,loads,state,reply,emit,hostListeners};
}
const preview=()=>({url:'http://127.0.0.1:9989/annotation-capability',channel:'picker-channel-0123456789abcdef',version:2,digest:'a'.repeat(64),targets:[{id:'target-1',tag:'button',selector:'#submit',text:'生成报告'}]});
async function settle(){await new Promise(resolve=>setImmediate(resolve));}
async function open(h){const pending=h.ctx.beginArtifactAnnotation();h.reply(h.requests.at(-1),preview());await pending;h.nodes.artifactPreviewFrame.onload();}

(async()=>{
  // A valid picker selects only backend metadata, never supplied message text.
  {
    const h=harness();await open(h);
    assert.equal(h.nodes.artifactPreviewFrame.getAttribute('sandbox'),'allow-scripts');
    assert.equal(h.nodes.artifactAnnotationPanel.hidden,false);
    h.emit({type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1',rect:{x:0,y:0,width:20,height:10}});
    assert.equal(h.state().selected.id,'target-1');
    assert.match(h.nodes.artifactAnnotationSelection.textContent,/生成报告/);
    h.nodes.artifactAnnotationInstruction.value='让按钮小一点';h.nodes.artifactAnnotationInstruction.dispatch('input',{});
    assert.equal(h.nodes.artifactAnnotationApply.disabled,false);
    const pending=h.ctx.submitArtifactAnnotation();await settle();
    const body=JSON.parse(h.requests.at(-1).init.body);
    assert.deepEqual(body,{version:2,digest:'a'.repeat(64),targetId:'target-1',instruction:'让按钮小一点'});
    h.reply(h.requests.at(-1),{prompt:'Canonical revision instruction',reference:{artifactId:'artifact-a',version:2,digest:'a'.repeat(64),targetId:'target-1'}});await pending;
    assert.equal(h.submissions.length,1);assert.equal(h.submissions[0].sessionId,'session-a');
    assert.equal(h.nodes.artifactPreviewOverlay.hidden,true);
  }
  // Wrong origin/source/channel/target or extra message fields cannot select.
  {
    const h=harness();await open(h);const base={type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1'};
    const handlers=h.hostListeners.get('message');
    handlers.forEach(fn=>fn({origin:'http://127.0.0.1:9989',source:h.nodes.artifactPreviewFrame.contentWindow,data:base}));
    handlers.forEach(fn=>fn({origin:'null',source:{},data:base}));
    h.emit({...base,channel:'old-channel'});h.emit({...base,targetId:'not-in-manifest'});h.emit({...base,text:'malicious'});
    assert.equal(h.state().selected,null);
    h.emit({...base,rect:{x:0,y:0,width:Infinity,height:5}});assert.equal(h.state().selected,null);
  }
  // Cancel prevents a pending preview from restoring annotation mode.
  {
    const h=harness(),pending=h.ctx.beginArtifactAnnotation();h.ctx.resetArtifactAnnotation();h.reply(h.requests[0],preview());await pending;
    assert.equal(h.state().mode,'off');assert.equal(h.nodes.artifactAnnotationPanel.hidden,true);
    assert.notEqual(h.nodes.artifactPreviewFrame.getAttribute('sandbox'),'allow-scripts');
  }
  // A switched session cannot receive stale selection or a queued instruction.
  {
    const h=harness();await open(h);h.emit({type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1'});
    h.nodes.artifactAnnotationInstruction.value='修改';const pending=h.ctx.submitArtifactAnnotation();await settle();
    h.ctx.currentSessionId='session-b';h.ctx.resetArtifactAnnotation();
    h.reply(h.requests.at(-1),{prompt:'stale',reference:{artifactId:'artifact-a',version:2,digest:'a'.repeat(64),targetId:'target-1'}});await pending;
    assert.equal(h.submissions.length,0);assert.equal(h.state().mode,'off');
  }
  // Canonical validation errors keep the user's instruction available.
  {
    const h=harness();await open(h);h.emit({type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1'});
    h.nodes.artifactAnnotationInstruction.value='修改';const pending=h.ctx.submitArtifactAnnotation();await settle();h.reply(h.requests.at(-1),{error:'Version has changed'},409);await pending;
    assert.equal(h.state().mode,'selecting');assert.equal(h.nodes.artifactAnnotationInstruction.value,'修改');assert.equal(h.submissions.length,0);
  }
  // Escape from the opaque picker exits the mode and restores static preview.
  {
    const h=harness();await open(h);h.emit({type:'metis-artifact-annotation-exit',channel:preview().channel});await settle();
    assert.equal(h.state().mode,'off');assert.equal(h.loads.length,1);assert.equal(h.nodes.artifactPreviewFrame.getAttribute('sandbox'),'');
  }
  // Busy normal chat keeps the request, and cannot create a false comparison.
  {
    const h=harness();h.ctx.submitArtifactAnnotationPrompt=async()=>false;await open(h);
    h.emit({type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1'});
    h.nodes.artifactAnnotationInstruction.value='修改';const pending=h.ctx.submitArtifactAnnotation();await settle();
    h.reply(h.requests.at(-1),{prompt:'canonical',reference:{artifactId:'artifact-a',version:2,digest:'a'.repeat(64),targetId:'target-1'}});await pending;
    assert.equal(h.state().mode,'selecting');assert.equal(h.nodes.artifactAnnotationInstruction.value,'修改');
    assert.equal(h.state().comparisons.size,0);assert.match(h.nodes.artifactAnnotationHint.textContent,/当前任务/);
  }
  // A rejected chat handoff must preserve an earlier successful comparison.
  {
    const h=harness(),key=h.ctx.artifactAnnotationComparisonKey('session-a','artifact-a');
    h.state().comparisons.set(key,{beforeVersion:1});h.ctx.submitArtifactAnnotationPrompt=async()=>{throw new Error('chat send failed');};await open(h);
    h.emit({type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1'});
    h.nodes.artifactAnnotationInstruction.value='修改';const pending=h.ctx.submitArtifactAnnotation();await settle();
    h.reply(h.requests.at(-1),{prompt:'canonical',reference:{artifactId:'artifact-a',version:2,digest:'a'.repeat(64),targetId:'target-1'}});await pending;
    assert.equal(h.state().comparisons.get(key).beforeVersion,1);assert.equal(h.state().mode,'selecting');
  }
  // Escape is also usable while the canonical validation request is pending.
  {
    const h=harness();await open(h);h.emit({type:'metis-artifact-selection',channel:preview().channel,targetId:'target-1'});
    h.nodes.artifactAnnotationInstruction.value='修改';const pending=h.ctx.submitArtifactAnnotation();await settle();
    h.emit({type:'metis-artifact-annotation-exit',channel:preview().channel});await settle();
    assert.equal(h.state().mode,'off');assert.equal(h.requests.at(-1).init.signal.aborted,true);
    h.reply(h.requests.at(-1),{prompt:'canceled',reference:{artifactId:'artifact-a',version:2,digest:'a'.repeat(64),targetId:'target-1'}});await pending;
    assert.equal(h.submissions.length,0);
  }
  // Before/after is shown only after a new version exists, scoped to its owner.
  {
    const h=harness(),key=h.ctx.artifactAnnotationComparisonKey('session-a','artifact-a');
    h.state().comparisons.set(key,{beforeVersion:2});h.ctx.refreshArtifactRevisionComparison();
    assert.equal(h.nodes.artifactCompareControls.hidden,true);
    h.ctx.artifactState.active.currentVersion=3;h.ctx.artifactState.active.versions.push({number:3});
    h.ctx.refreshArtifactRevisionComparison();assert.equal(h.nodes.artifactCompareControls.hidden,false);
    await h.ctx.compareArtifactRevision('after');assert.equal(h.ctx.artifactState.activeVersion,3);
    await h.ctx.compareArtifactRevision('before');assert.equal(h.ctx.artifactState.activeVersion,2);
    h.ctx.currentSessionId='session-b';h.ctx.artifactState.active.sessionId='session-b';
    h.ctx.refreshArtifactRevisionComparison();assert.equal(h.nodes.artifactCompareControls.hidden,false);
    assert.match(h.nodes.artifactCompareBefore.textContent,/v2$/,'another owner can compare only its own saved versions');
  }
  // Restarting without a memory baseline still compares the latest saved pair.
  {
    const h=harness();h.ctx.artifactState.active.currentVersion=6;
    h.ctx.artifactState.active.versions=[{number:1},{number:4},{number:6}];
    h.ctx.artifactState.activeVersion=6;h.ctx.refreshArtifactRevisionComparison();
    assert.equal(h.nodes.artifactCompareControls.hidden,false);
    assert.match(h.nodes.artifactCompareBefore.textContent,/v4$/);
    await h.ctx.compareArtifactRevision('before');assert.equal(h.ctx.artifactState.activeVersion,4);
    await h.ctx.compareArtifactRevision('after');assert.equal(h.ctx.artifactState.activeVersion,6);
    // Prefer the actual submitted baseline, while it still exists in this manifest.
    h.state().comparisons.set(h.ctx.artifactAnnotationComparisonKey('session-a','artifact-a'),{beforeVersion:1});
    h.ctx.refreshArtifactRevisionComparison();assert.match(h.nodes.artifactCompareBefore.textContent,/v1$/);
    // A different Artifact cannot consume artifact-a's recorded baseline.
    h.ctx.artifactState.active={id:'artifact-b',sessionId:'session-a',currentVersion:9,versions:[{number:2},{number:9}]};
    h.ctx.artifactState.activeVersion=9;h.ctx.refreshArtifactRevisionComparison();
    assert.match(h.nodes.artifactCompareBefore.textContent,/v2$/);
    await h.ctx.compareArtifactRevision('before');assert.equal(h.ctx.artifactState.activeVersion,2);
    // A missing saved baseline falls back instead of pointing at an absent version.
    h.state().comparisons.set(h.ctx.artifactAnnotationComparisonKey('session-a','artifact-b'),{beforeVersion:1});
    h.ctx.refreshArtifactRevisionComparison();assert.match(h.nodes.artifactCompareBefore.textContent,/v2$/);
  }
  // The authenticated shell origin must never render the selectable document.
  {
    const h=harness(),pending=h.ctx.beginArtifactAnnotation();h.reply(h.requests[0],{...preview(),url:'http://localhost/api/artifacts/a/unsafe'});await pending;
    assert.equal(h.state().mode,'off');assert.notEqual(h.nodes.artifactPreviewFrame.getAttribute('sandbox'),'allow-scripts');
  }
  // Programmatic comparison must keep the dropdown aligned with the iframe.
  {
    const h=harness({realArtifacts:true});
    const realState=vm.runInContext('artifactState',h.ctx);
    realState.active=h.ctx.normalizeArtifact({id:'artifact-a',sessionId:'session-a',title:'Demo',currentVersion:2,versions:[{number:1},{number:2}]});
    realState.activeVersion=2;
    const select=h.nodes.artifactVersionSelect;select.value='2';select.children=[{value:'2',selected:true},{value:'1',selected:false}];
    h.state().comparisons.set(h.ctx.artifactAnnotationComparisonKey('session-a','artifact-a'),{beforeVersion:1});
    const before=h.ctx.compareArtifactRevision('before');h.reply(h.requests.at(-1),{previewUrl:'http://127.0.0.1:9988/preview-v1'});await before;
    assert.equal(realState.activeVersion,1);assert.equal(select.value,'1','before comparison must update the version dropdown');
    assert.equal(select.children[1].selected,true);assert.equal(select.children[0].selected,false);
    const after=h.ctx.compareArtifactRevision('after');h.reply(h.requests.at(-1),{previewUrl:'http://127.0.0.1:9988/preview-v2'});await after;
    assert.equal(realState.activeVersion,2);assert.equal(select.value,'2','after comparison must update the version dropdown');
    assert.equal(select.children[0].selected,true);assert.equal(select.children[1].selected,false);
  }
  console.log('Artifact annotation UI checks passed');
})().catch(error=>{console.error(error);process.exitCode=1;});
