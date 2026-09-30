package webui

import (
	"strings"
	"testing"
)

func TestDesktopTokenStatsDisclosureStyles(t *testing.T) {
	content, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(content)
	if !strings.Contains(css, ".token-stats-popover[hidden] { display: none; }") {
		t.Fatal("the token details hidden attribute must override the popover display style")
	}
	if !strings.Contains(css, ".token-stats-trigger:focus-visible") {
		t.Fatal("the token disclosure needs a visible keyboard focus style")
	}
}

func TestDesktopTokenStatsSummaryAndExactDisclosure(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function extract(source,start,end) {
  const from=source.indexOf(start), to=source.indexOf(end,from);
  assert.ok(from>=0 && to>from,'cannot isolate Desktop token functions');
  return source.slice(from,to);
}
function attrsFrom(tag) {
  return Object.fromEntries([...tag.matchAll(/([\w-]+)="([^"]*)"/g)].map(match=>[match[1],match[2]]));
}
let markup='', sessionTrigger=null,sessionPopover=null,tokenTrigger=null,tokenPopover=null,contextCloses=0;
const document={documentElement:{lang:'zh-CN'},body:{},activeElement:null,listeners:{},
  addEventListener(name,callback){(this.listeners[name] ||= []).push(callback)},
  getElementById(id){return ({sessionStatsbar:bar,sessionStatsTrigger:sessionTrigger,
    sessionStatsPopover:sessionPopover,tokenStatsTrigger:tokenTrigger,
    tokenStatsPopover:tokenPopover,composerRuntimeDock:dock,contextMeter:meter,inputField})[id]||null}};
function element(attrs={},hidden=false) {
  return {attrs,hidden,focused:false,setAttribute(name,value){this.attrs[name]=String(value)},
    getAttribute(name){return this.attrs[name]},contains(other){return other===this},
    focus(){document.activeElement=this;this.focused=true}};
}
function found(id,tag) {
  const match=markup.match(new RegExp('<'+tag+'\\b[^>]*id="'+id+'"[^>]*>'));
  return match?element(attrsFrom(match[0]),/\bhidden\b/.test(match[0])):null;
}
const bar={style:{display:'none'}};
const dock={hidden:true},meter={style:{display:'none'}},inputField=element();
Object.defineProperty(bar,'innerHTML',{get(){return markup},set(value){
  if(document.activeElement===sessionTrigger || document.activeElement===tokenTrigger)document.activeElement=document.body;
  markup=value;
  sessionTrigger=found('sessionStatsTrigger','button');sessionPopover=found('sessionStatsPopover','section');
  tokenTrigger=found('tokenStatsTrigger','button');tokenPopover=found('tokenStatsPopover','section');
}});
const pending=[];
const c={document,currentSessionId:'A',uiText:(en,zh)=>document.documentElement.lang==='zh-CN'?zh:en,
  escHtml:value=>String(value),escAttr:value=>String(value),fmtMs:value=>String(value)+'ms',
  closeContextMeterPopover(){contextCloses++},
  fetch(){let resolve;const promise=new Promise(r=>resolve=r);pending.push(resolve);return promise}};
vm.createContext(c);
vm.runInContext('let sessionStatsSnapshot=null,sessionStatsSessionID="",sessionStatsGeneration=0;',c);
vm.runInContext(extract(sources['trace.js'],'function fmtTokens(n) {','function oneLine(s) {'),c);
vm.runInContext(extract(sources['app.js'],'function syncComposerRuntimeDock()','function toggleContextMeter(event)'),c);
vm.runInContext(extract(sources['sessions.js'],'function closeSessionStatsPopover()','function toggleSessionSearch()'),c);
const stats={turns:4,steps:9,inputTokens:3989008295,outputTokens:6029414,
  cacheReadTokens:3904037293,cacheWriteTokens:16657266,tokPerSec:55};
c.storedStats={stats};
vm.runInContext('sessionStatsSnapshot=storedStats;sessionStatsSessionID="A";',c);
c.renderSessionStatsbar({stats},'A');
assert.equal(bar.style.display,'flex');assert.equal(dock.hidden,false);
assert.match(markup,/4轮 · 9步 · 55 tok\/s/);
assert.match(markup,/3995\.0M tok/);
assert.match(markup,/缓存命中 98%/);
assert.doesNotMatch(markup,/\btitle=/,'details cannot be exposed by hover');
assert.equal(tokenTrigger.getAttribute('aria-controls'),'tokenStatsPopover');
assert.equal(tokenTrigger.getAttribute('aria-expanded'),'false');
assert.equal(tokenPopover.getAttribute('role'),'region');
assert.equal(tokenPopover.hidden,true);
const sessionRows=markup.match(/<dl class="session-stats-details">([\s\S]*?)<\/dl>/)[1];
assert.doesNotMatch(sessionRows,/输入|缓存|Input|Cache/);
const tokenRows=markup.match(/<dl class="token-stats-details">([\s\S]*?)<\/dl>/)[1];
for(const [label,value] of [['总量','3,995,037,709'],['输入总量','3,989,008,295'],
  ['未缓存输入','68,313,736'],['缓存读取','3,904,037,293'],
  ['缓存写入','16,657,266'],['输出','6,029,414']]) {
  assert.match(tokenRows,new RegExp(label+'[\\s\\S]*?'+value+' tok'));
}
c.toggleTokenStats({stopPropagation(){}});
assert.equal(tokenPopover.hidden,false);assert.equal(tokenTrigger.getAttribute('aria-expanded'),'true');
assert.equal(contextCloses,1);
c.toggleSessionStats({stopPropagation(){}});
assert.equal(tokenPopover.hidden,true,'session disclosure closes token details');
assert.equal(sessionPopover.hidden,false);
c.toggleTokenStats({stopPropagation(){}});
assert.equal(sessionPopover.hidden,true,'token disclosure closes session details');
tokenTrigger.focus();
document.documentElement.lang='en';c.renderCurrentSessionStatsbar();
assert.equal(document.activeElement,tokenTrigger);
assert.equal(tokenPopover.hidden,false,'language redraw preserves open token details');
assert.match(markup,/Cache hit 98%/);
assert.match(markup,/Uncached input[\s\S]*68,313,736 tok/);
for(const listener of document.listeners.keydown)listener({key:'Escape'});
assert.equal(tokenPopover.hidden,true);assert.equal(tokenTrigger.getAttribute('aria-expanded'),'false');
assert.equal(tokenTrigger.focused,true);
c.toggleTokenStats({stopPropagation(){}});
for(const listener of document.listeners.click)listener({target:{}});
assert.equal(tokenPopover.hidden,true,'outside click closes token details');
c.renderSessionStatsbar({stats:{turns:1,steps:2}},'A');
assert.equal(tokenTrigger,null,'no recorded token total means no token button');
c.renderSessionStatsbar({stats:{inputTokens:100,outputTokens:10,cacheReadTokens:80,cacheWriteTokens:40}},'A');
assert.ok(tokenTrigger);
assert.equal(sessionTrigger,null,'token-only stats should not duplicate the token pill');
assert.doesNotMatch(markup,/Cache hit|Uncached input/,'inconsistent cache fields cannot produce a derived metric');
assert.match(markup,/Cache read[\s\S]*80 tok/,'reported cache fields remain visible');
c.renderSessionStatsbar({stats:{inputTokens:100,outputTokens:10}},'A');
assert.doesNotMatch(markup,/Cache hit|Uncached input/,'missing cache fields cannot produce a derived metric');
tokenTrigger.focus();c.currentSessionId='B';
const loading=c.loadSessionStatsbar('B');
assert.equal(document.activeElement,inputField,'switching sessions releases focus to the composer');
assert.equal(bar.style.display,'none');assert.equal(tokenTrigger,null);
pending[0]({ok:true,json:async()=>({stats:{turns:1,inputTokens:4,outputTokens:1,cacheReadTokens:0,cacheWriteTokens:0}})});
loading.then(()=>{assert.match(markup,/5 tok/);assert.doesNotMatch(markup,/3995\.0M/)}).catch(error=>{console.error(error);process.exitCode=1});
`, "sessions.js", "trace.js", "app.js")
}
