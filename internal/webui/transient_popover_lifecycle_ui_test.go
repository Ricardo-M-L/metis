package webui

import "testing"

const desktopTransientPopoverFixture = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function extract(file, start, end) {
  const source=sources[file], from=source.indexOf(start), to=source.indexOf(end,from+start.length);
  assert.ok(from>=0 && to>from,start); return source.slice(from,to);
}
function fixture() {
  const nodes={}, listeners={}, windowListeners={};
  function element(id='', parent=null) {
    const classes=new Set(), attrs={};
    const node={id,parentElement:parent,isConnected:true,hidden:false,style:{},dataset:{},attrs,
      classList:{add(...values){values.forEach(value=>classes.add(value))},remove(...values){values.forEach(value=>classes.delete(value))},
        contains(value){return classes.has(value)},toggle(value,force){const add=force===undefined?!classes.has(value):force;add?classes.add(value):classes.delete(value)}},
      setAttribute(name,value){attrs[name]=String(value)},getAttribute(name){return attrs[name]??null},removeAttribute(name){delete attrs[name]},
      contains(other){for(let current=other;current;current=current.parentElement)if(current===this)return true;return false},
      closest(selector){if(selector==='.input-container')return null;for(let current=this;current;current=current.parentElement){if(selector.includes('.session-item')&&current.dataset.detailTitle)return current}return null},
      getBoundingClientRect(){return {left:240,right:500,top:680,width:300,height:120}},
      focus(){document.activeElement=this},appendChild(child){child.parentElement=this;if(child.id)nodes[child.id]=child},
    };
    if(id)nodes[id]=node;
    return node;
  }
  const document={documentElement:{clientWidth:1000,clientHeight:800,lang:'zh-CN'},hidden:false,
    addEventListener(name,fn){(listeners[name] ||= []).push(fn)},getElementById:id=>nodes[id]||null,
    createElement(){return element()},activeElement:null};
  document.body=element('body');document.activeElement=document.body;
  const window={innerWidth:1000,innerHeight:800,
    addEventListener(name,fn){(windowListeners[name] ||= []).push(fn)},requestAnimationFrame(fn){fn()}};
  const item=element('row'),child=element('rowName',item),outside=element('outside');
  item.dataset={detailTitle:'会话 A',detailPath:'/workspace',detailModel:'model',detailMeta:'已完成'};
  const list=element('sessionList');
  Object.defineProperty(list,'innerHTML',{set(){item.isConnected=false}});
  for(const [triggerID,panelID] of [['sessionStatsTrigger','sessionStatsPopover'],['tokenStatsTrigger','tokenStatsPopover'],['contextMeter','contextMeterPopover']]) {
    element(triggerID).setAttribute('aria-expanded','false');element(panelID).hidden=true;
  }
  const c={window,document,sessions:[],workspaces:[],desktopPreferences:{sidebarView:'list'},showArchivedSessions:false,
    escHtml:String,uiText:(en,zh)=>zh,resumeSessionGeneration:0,sessionStatsGeneration:0,pendingSessionId:null};
  vm.createContext(c);
  vm.runInContext(sources['stat_popovers.js'],c);
  vm.runInContext(extract('app.js','function closeContextMeterPopover()','function renderContextMeter('),c);
  vm.runInContext(extract('sessions.js','function closeSessionStatsPopover()','function renderCurrentSessionStatsbar()'),c);
  vm.runInContext(extract('sessions.js','function renderSessions()','async function moveSession('),c);
  vm.runInContext(extract('sessions.js','let sessionDetailCard = null;','function toggleSessionsExpand()'),c);
  vm.runInContext(extract('sessions.js','function invalidateSessionAsyncLoads()','async function loadWorkspaces()'),c);
  const event=(name,target=outside,relatedTarget=null,extra={})=>{
    if(name==='focusin')document.activeElement=target;
    if(name==='focusout')document.activeElement=relatedTarget||document.body;
    for(const listener of listeners[name]||[])listener({target,relatedTarget,stopPropagation(){},preventDefault(){},...extra});
  };
  const card=()=>vm.runInContext('sessionDetailCard',c);
  const shown=()=>!!card()?.classList.contains('show');
  return {c,nodes,document,item,child,outside,event,card,shown,
    windowEvent:name=>(windowListeners[name]||[]).forEach(listener=>listener())};
}
`

func TestDesktopSessionTooltipClosesOnReplacementAndDismissal(t *testing.T) {
	runDesktopStatsNode(t, desktopTransientPopoverFixture+`
{
  const f=fixture();f.event('mouseover',f.item);assert.equal(f.shown(),true);
  f.c.renderSessions();assert.equal(f.shown(),false,'replacing sidebar rows must close the body tooltip');
}
{
  const f=fixture();f.event('focusin',f.item);assert.equal(f.shown(),true);
  assert.equal(f.card().getAttribute('role'),'tooltip');
  assert.equal(f.item.getAttribute('aria-describedby'),f.card().id,'keyboard focus announces the same details');
  f.event('keydown',f.item,null,{key:'Escape'});assert.equal(f.shown(),false,'Escape dismisses sidebar details');
  assert.equal(f.item.getAttribute('aria-describedby'),null);
  assert.equal(f.document.activeElement,f.item,'dismissing a tooltip keeps keyboard focus');
}
{
  const f=fixture();f.event('mouseover',f.item);f.event('click',f.outside);
  assert.equal(f.shown(),false,'outside clicks dismiss sidebar details');
}
{
  const f=fixture();f.event('mouseover',f.item);f.event('focusin',f.item);
  f.event('keydown',f.item,null,{key:'Escape'});
  f.event('mouseover',f.child,f.item);f.event('focusin',f.child,f.item);
  assert.equal(f.shown(),false,'moving inside a dismissed row cannot reopen its tooltip');
}
{
  const f=fixture();f.c.openMenuBtn=f.child;f.event('focusin',f.child);
  assert.equal(f.shown(),false,'session menu keyboard focus cannot reopen sidebar details');
}
`, "stat_popovers.js", "app.js", "sessions.js")
}

func TestDesktopSessionTooltipRetainsPointerAndKeyboardOwnership(t *testing.T) {
	runDesktopStatsNode(t, desktopTransientPopoverFixture+`
const f=fixture();f.event('mouseover',f.item);f.event('focusin',f.item);
f.event('mouseout',f.item,f.child);assert.equal(f.shown(),true,'moving between row descendants is not a pointer exit');
f.event('mouseout',f.child,f.outside);assert.equal(f.shown(),true,'keyboard focus retains details after the pointer exits');
f.event('focusout',f.item,f.outside);assert.equal(f.shown(),false,'details close after both pointer and focus leave');
f.event('mouseover',f.item);f.event('focusin',f.item);f.event('focusout',f.item,f.outside);
assert.equal(f.shown(),true,'pointer ownership retains details after focus exits');
f.event('mouseout',f.item,f.outside);assert.equal(f.shown(),false);
`, "stat_popovers.js", "app.js", "sessions.js")
}

func TestDesktopTransientPopoversAreExclusiveAndCloseWhenInactive(t *testing.T) {
	runDesktopStatsNode(t, desktopTransientPopoverFixture+`
for(const [toggle,panelID] of [['toggleSessionStats','sessionStatsPopover'],['toggleTokenStats','tokenStatsPopover'],['toggleContextMeter','contextMeterPopover']]) {
  const f=fixture();f.event('mouseover',f.item);f.c[toggle]({stopPropagation(){}});
  assert.equal(f.shown(),false,toggle+' closes sidebar tooltip');assert.equal(f.nodes[panelID].hidden,false);
  f.event('focusin',f.item);assert.equal(f.shown(),true);assert.equal(f.nodes[panelID].hidden,true,'sidebar details close existing statistics');
}
for(const close of ['blur','visibility','navigation']) {
  const f=fixture();f.c.toggleTokenStats({stopPropagation(){}});
  if(close==='blur')f.windowEvent('blur');
  if(close==='visibility'){f.document.hidden=true;f.event('visibilitychange')}
  if(close==='navigation')f.c.invalidateSessionAsyncLoads();
  assert.equal(f.nodes.tokenStatsPopover.hidden,true,close+' closes statistics');
  assert.equal(f.nodes.tokenStatsTrigger.getAttribute('aria-expanded'),'false');
  const tooltip=fixture();tooltip.event('mouseover',tooltip.item);assert.equal(tooltip.shown(),true);
  if(close==='blur')tooltip.windowEvent('blur');
  if(close==='visibility'){tooltip.document.hidden=true;tooltip.event('visibilitychange')}
  if(close==='navigation')tooltip.c.invalidateSessionAsyncLoads();
  assert.equal(tooltip.shown(),false,close+' closes sidebar tooltip');
}
{
  const f=fixture();f.c.toggleSessionStats({stopPropagation(){}});
  const inside={parentElement:f.nodes.sessionStatsPopover};f.event('focusin',inside);
  assert.equal(f.nodes.sessionStatsPopover.hidden,false,'focus inside details keeps statistics open');
  f.event('focusin',f.outside);assert.equal(f.nodes.sessionStatsPopover.hidden,true,'focus leaving statistics closes details');
}
{
  const f=fixture();f.c.currentSessionId='A';f.c.fetch=()=>new Promise(()=>{});
  vm.runInContext(sources['sessions.js'].slice(sources['sessions.js'].indexOf('async function resumeSession(id)')),f.c);
  f.c.toggleTokenStats({stopPropagation(){}});f.c.resumeSession('B');
  assert.equal(f.nodes.tokenStatsPopover.hidden,true,'session selection closes old statistics before the request resolves');
}
`, "stat_popovers.js", "app.js", "sessions.js")
}
