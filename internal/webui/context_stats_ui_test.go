package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func desktopStatsNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	return node
}

func desktopStatsSources(t *testing.T, names ...string) []byte {
	t.Helper()
	sources := make(map[string]string, len(names))
	for _, name := range names {
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
	return payload
}

func runDesktopStatsNode(t *testing.T, script string, sources ...string) {
	t.Helper()
	cmd := exec.Command(desktopStatsNode(t), "-e", script)
	cmd.Stdin = bytes.NewReader(desktopStatsSources(t, sources...))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Desktop context/stats browser behavior: %v\n%s", err, output)
	}
}

func desktopElementByID(node *html.Node, id string) *html.Node {
	if node.Type == html.ElementNode && desktopAttr(node, "id") == id {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := desktopElementByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func desktopAttr(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func desktopHasAttr(node *html.Node, key string) bool {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return true
		}
	}
	return false
}

func desktopHasClass(node *html.Node, class string) bool {
	for _, item := range strings.Fields(desktopAttr(node, "class")) {
		if item == class {
			return true
		}
	}
	return false
}

func desktopAncestorWithClass(node *html.Node, class string) *html.Node {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if desktopHasClass(parent, class) {
			return parent
		}
	}
	return nil
}

func TestDesktopContextAndStatsDockMarkup(t *testing.T) {
	content, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	meter := desktopElementByID(doc, "contextMeter")
	contextDetails := desktopElementByID(doc, "contextMeterPopover")
	stats := desktopElementByID(doc, "sessionStatsbar")
	runtimeDock := desktopElementByID(doc, "composerRuntimeDock")
	input := desktopElementByID(doc, "inputField")
	if meter == nil || contextDetails == nil || stats == nil || runtimeDock == nil || input == nil {
		t.Fatal("composer context, detail, stats, or input node missing")
	}
	if meter.Data != "button" || desktopAttr(meter, "type") != "button" ||
		desktopAttr(meter, "aria-controls") != "contextMeterPopover" || desktopAttr(meter, "aria-expanded") != "false" {
		t.Fatal("context pressure must be an accessible disclosure button")
	}
	if desktopHasAttr(meter, "title") {
		t.Fatal("context pressure details must not appear in a hover tooltip")
	}
	composer := desktopAncestorWithClass(input, "input-container")
	dock := desktopAncestorWithClass(meter, "composer-runtime-dock")
	if composer == nil || desktopAncestorWithClass(meter, "input-container") != composer ||
		dock == nil || dock != runtimeDock || dock.Parent != composer || stats.Parent != dock || contextDetails.Parent != composer {
		t.Fatal("context detail and session stats must share the input container with the composer")
	}
	if !desktopHasAttr(runtimeDock, "hidden") {
		t.Fatal("empty runtime dock must start hidden")
	}
	previous := dock.PrevSibling
	for previous != nil && previous.Type == html.TextNode {
		previous = previous.PrevSibling
	}
	if previous == nil || !desktopHasClass(previous, "input-box") {
		t.Fatal("runtime stats dock must sit directly below the composer input")
	}
}

func TestDesktopContextAndStatsHiddenDockStyles(t *testing.T) {
	content, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range strings.Split(string(content), "}") {
		if strings.Contains(rule, ".composer-runtime-dock[hidden]") &&
			strings.Contains(rule, ".context-meter-popover[hidden]") &&
			strings.Contains(rule, ".session-stats-popover[hidden]") {
			if strings.Contains(rule, "display: none") {
				return
			}
			break
		}
	}
	t.Fatal("hidden dock and detail panels must override their visible author styles")
}

func TestDesktopContextMeterUsesOnlyOwnedSessionPressure(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'))['app.js'];
function extract(start, end) {
  const from = source.indexOf(start), to = source.indexOf(end, from);
  assert.ok(from >= 0 && to > from, 'cannot isolate Desktop context functions');
  return source.slice(from, to);
}

function element() {
  const attrs = {}, classes = new Set(), style = {display:'none', setProperty(name, value) { this[name] = value; }};
  const node = {attrs, dataset:{}, style, hidden:true, innerHTML:'', textContent:'', focused:false,
    classList:{add(name){classes.add(name)},remove(name){classes.delete(name)},toggle(name, on){if(on)classes.add(name);else classes.delete(name)},contains(name){return classes.has(name)}},
    setAttribute(name, value){attrs[name]=String(value);if(name==='data-session-id')this.dataset.sessionId=String(value)},
    removeAttribute(name){delete attrs[name];if(name==='data-session-id')delete this.dataset.sessionId},
    getAttribute(name){return attrs[name]}, contains(other){return other===this}, focus(){this.focused=true}};
  Object.defineProperty(node,'title',{get(){return attrs.title||''},set(value){attrs.title=String(value)}});
  return node;
}
const ids = ['statusChip','statusPopover','contextMeter','contextMeterPopover','composerRuntimeDock','sessionStatsbar'];
const nodes = Object.fromEntries(ids.map(id=>[id,element()]));
const listeners = {};
let tokenCloses=0;
const document = {documentElement:{lang:'zh-CN'}, getElementById:id=>nodes[id]||null,
  addEventListener(name, callback){(listeners[name] ||= []).push(callback)},querySelectorAll:()=>[]};
const c = {document,window:{},navigator:{language:'zh-CN'},currentSessionId:null,
  runningSessionId:'',turnRunning:false,lastStatusSnapshot:null,
  subAgentDetailState:{agentId:''},subAgentDetailStream:null,
  statusRosterForSelectedSession:()=>null,statusSubAgentsForSelectedSession:()=>[],
  closeStatusPopover(){nodes.statusPopover.style.display='none'},applyLayout(){},
  closeTokenStatsPopover(){tokenCloses++},
  escHtml:value=>String(value),fmtTokens:value=>String(value)};
vm.createContext(c);
vm.runInContext(extract('const DESKTOP_I18N =', 'function presetDisplayName('),c);
vm.runInContext(extract('function closeContextMeterPopover()', 'function renderStatusSnapshot(d)'),c);
vm.runInContext(extract('function renderStatusSnapshot(d)', 'setInterval(pollStatus, 3000);'),c);
const snapshot = {activeSessionId:'A', contextSessionId:'A', contextUsed:64000, contextWindow:128000, compactAtTokens:102400};
const render = data=>c.renderStatusSnapshot(data);
const meter=nodes.contextMeter, pop=nodes.contextMeterPopover;
const assertHidden=()=>{assert.equal(meter.style.display,'none');assert.equal(pop.hidden,true);assert.equal(meter.getAttribute('aria-expanded'),'false');assert.equal(nodes.composerRuntimeDock.hidden,true)};
render(snapshot);
assertHidden(); // A fresh composer cannot inherit the active Loop's pressure.
c.turnRunning=true;c.runningSessionId='A';render(snapshot);
assertHidden(); // Parallel turns may leave A running while a blank composer is viewed.
c.turnRunning=false;c.runningSessionId='';
c.currentSessionId='B';render(snapshot);
assertHidden(); // A saved or parallel session is not the active Loop.
c.currentSessionId='A';
render({...snapshot,contextSessionId:undefined});assertHidden();
render({...snapshot,contextSessionId:'B'});assertHidden();
render({...snapshot,contextWindow:0});assertHidden();
render({...snapshot,contextUsed:undefined});assertHidden();
render(snapshot);
assert.equal(meter.style.display,'');
assert.equal(nodes.composerRuntimeDock.hidden,false);
assert.match(meter.innerHTML,/50%/);
assert.equal(meter.style['--context-progress'],'50%');
assert.equal(meter.getAttribute('title'),undefined,'hover must not reveal context details');
assert.equal(meter.title,'');
assert.match(meter.getAttribute('aria-label'),/^上下文 50% · 点击查看详情$/);
assert.doesNotMatch(meter.getAttribute('aria-label'),/64,000|128,000|tokens/);
c.toggleContextMeter({stopPropagation(){}});
assert.equal(pop.hidden,false);
assert.equal(meter.getAttribute('aria-expanded'),'true');
assert.equal(tokenCloses,1,'opening context details closes token details');
assert.match(pop.innerHTML,/已使用.*64,000 tokens/);
assert.match(pop.innerHTML,/自动压缩阈值.*102,400 tokens/);
c.toggleContextMeter({stopPropagation(){}});
assert.equal(pop.hidden,true);
c.toggleContextMeter({stopPropagation(){}});
listeners.click[0]({target:{}});
assert.equal(pop.hidden,true);
assert.equal(meter.getAttribute('aria-expanded'),'false');
c.currentSessionId='B';render(snapshot);assertHidden();
c.currentSessionId='A';render(snapshot);
c.toggleContextMeter({stopPropagation(){}});
listeners.keydown[0]({key:'Escape'});
assert.equal(pop.hidden,true);assert.equal(meter.focused,true);
c.lastStatusSnapshot=snapshot;
c.applyLanguage('en');
assert.equal(document.documentElement.lang,'en');
assert.match(pop.innerHTML,/Context details[\s\S]*Used[\s\S]*64,000 tokens/);
assert.equal(meter.getAttribute('aria-label'),'Context 50% · Click to view details');
assert.equal(meter.getAttribute('title'),undefined);
assert.doesNotMatch(meter.getAttribute('aria-label'),/64,000|128,000|tokens/);
render({...snapshot,contextUsed:256000});
assert.match(meter.innerHTML,/100%/);
assert.match(pop.innerHTML,/256,000 tokens/);
render({...snapshot,contextUsed:1});
assert.match(meter.innerHTML,/&lt;1%|<1%/);
`, "app.js")
}

func TestDesktopSessionStatsFollowSelectedSessionAndLanguage(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
function extract(source, start, end) {
  const from=source.indexOf(start), to=source.indexOf(end,from);
  assert.ok(from>=0 && to>from,'cannot isolate Desktop stats functions');
  return source.slice(from,to);
}
function attrsFrom(tag) {
  return Object.fromEntries([...tag.matchAll(/([\w-]+)="([^"]*)"/g)].map(match=>[match[1],match[2]]));
}
function dynamicNode(attrs, hidden) {
  return {attrs,hidden,focused:false,setAttribute(name,value){this.attrs[name]=String(value)},
    getAttribute(name){return this.attrs[name]},focus(){this.focused=true;document.activeElement=this},contains(other){return other===this}};
}
let markup='',trigger=null,popover=null,contextCloses=0;
const bar={style:{display:'none'}};
const dock={hidden:true},contextMeter={style:{display:'none'}};
const inputField=dynamicNode({},false);
Object.defineProperty(bar,'innerHTML',{get(){return markup},set(value){
  if(document.activeElement===trigger)document.activeElement=document.body;
  markup=value;trigger=null;popover=null;
  const button=value.match(/<button\b[^>]*id="sessionStatsTrigger"[^>]*>/);
  const section=value.match(/<section\b[^>]*id="sessionStatsPopover"[^>]*>/);
  if(button)trigger=dynamicNode(attrsFrom(button[0]),false);
  if(section)popover=dynamicNode(attrsFrom(section[0]),/\bhidden\b/.test(section[0]));
}});
const requests=[];
const document={documentElement:{lang:'zh-CN'},body:{},activeElement:null,
  getElementById(id){return id==='sessionStatsbar'?bar:id==='sessionStatsTrigger'?trigger:id==='sessionStatsPopover'?popover:id==='composerRuntimeDock'?dock:id==='contextMeter'?contextMeter:id==='inputField'?inputField:null},
  addEventListener(name,callback){(this.listeners[name] ||= []).push(callback)},listeners:{},querySelectorAll:()=>[]};
const c={document,window:{},navigator:{language:'zh-CN'},currentSessionId:'A',lastStatusSnapshot:null,
  fetch(url){let resolve;const promise=new Promise(r=>resolve=r);requests.push({url,resolve});return promise},
  uiText:(en,zh)=>document.documentElement.lang==='zh-CN'?zh:en,
  escHtml:value=>String(value),escAttr:value=>String(value),
  fmtTokens:value=>String(value),fmtMs:value=>(Number(value)/1000).toFixed(1)+'s',
  closeContextMeterPopover(){contextCloses++},
  applyLayout(){}};
vm.createContext(c);
vm.runInContext('let sessionStatsSnapshot=null,sessionStatsSessionID="",sessionStatsGeneration=0;',c);
vm.runInContext(extract(sources['app.js'],'function syncComposerRuntimeDock()','function toggleContextMeter(event)'),c);
vm.runInContext(extract(sources['sessions.js'],'function closeSessionStatsPopover()','function toggleSessionSearch()'),c);
vm.runInContext(extract(sources['app.js'],'const DESKTOP_I18N =','function presetDisplayName('),c);
const reply=(index,stats)=>requests[index].resolve({ok:true,json:async()=>({stats})});
const tick=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
  const old=c.loadSessionStatsbar('A');
  assert.equal(requests[0].url,'/api/trace?sessionId=A');
  c.currentSessionId='B';
  const selected=c.loadSessionStatsbar('B');
  assert.equal(requests[1].url,'/api/trace?sessionId=B');
  reply(0,{turns:99,steps:99,inputTokens:9999});await old;
  assert.equal(bar.style.display,'none','late trace for A must not appear in B');
  assert.equal(dock.hidden,true);
  reply(1,{turns:5,steps:10,toolCalls:2,durationMs:2500,llmMs:1800,toolMs:400,
    inputTokens:1200,outputTokens:300,cacheReadTokens:480,cacheWriteTokens:0,ttftAverageMs:200,tokPerSec:25,cacheHitRate:40});
  await selected;
  assert.equal(bar.style.display,'flex');
  assert.equal(dock.hidden,false,'selected session stats reveal the dock');
  assert.match(markup,/5轮 · 10步/);
  assert.match(markup,/总用时[\s\S]*2\.5秒/);
  assert.doesNotMatch(markup.match(/<dl class="session-stats-details">([\s\S]*?)<\/dl>/)[1],/输入 tokens|缓存命中/,'session details leave token usage to its own disclosure');
  assert.equal(trigger.getAttribute('aria-controls'),'sessionStatsPopover');
  assert.equal(trigger.getAttribute('aria-expanded'),'false');
  assert.equal(popover.getAttribute('role'),'region');
  c.toggleSessionStats({stopPropagation(){}});
  assert.equal(popover.hidden,false);assert.equal(trigger.getAttribute('aria-expanded'),'true');
  assert.equal(contextCloses,1,'opening stats closes context details');
  trigger.focus();
  const focusedBeforeLanguageChange=trigger;
  c.applyLanguage('en');
  assert.notEqual(trigger,focusedBeforeLanguageChange,'language change rebuilds translated button');
  assert.equal(document.activeElement,trigger,'translated statistics button keeps keyboard focus');
  assert.equal(document.documentElement.lang,'en');
  assert.match(markup,/5 turns · 10 steps/);
  assert.match(markup,/Total time[\s\S]*2\.5s/);
  assert.doesNotMatch(markup,/总用时|轮数|记录步数/);
  assert.equal(trigger.getAttribute('aria-expanded'),'true','language redraw preserves open detail');
  assert.equal(popover.hidden,false);
  document.listeners.keydown[0]({key:'Escape'});
  assert.equal(popover.hidden,true);assert.equal(trigger.getAttribute('aria-expanded'),'false');
  assert.equal(trigger.focused,true);
  c.toggleSessionStats({stopPropagation(){}});
  document.listeners.click[0]({target:{}});
  assert.equal(popover.hidden,true,'outside click closes details');
  trigger.focus();
  const focusedBeforeRefresh=trigger;
  const refresh=c.loadSessionStatsbar('B');
  assert.equal(bar.style.display,'flex','same-session refresh keeps old statistics while awaiting trace');
  assert.equal(document.activeElement,focusedBeforeRefresh);
  reply(2,{turns:6,steps:11,inputTokens:1300});await refresh;
  assert.match(markup,/6 turns · 11 steps/);
  assert.notEqual(trigger,focusedBeforeRefresh);
  assert.equal(document.activeElement,trigger,'same-session redraw keeps keyboard focus');
  const singular=c.loadSessionStatsbar('B');
  reply(3,{turns:1,steps:2});await singular;
  assert.match(markup,/1 turn · 2 steps/,'English labels follow the recorded counts');
  assert.doesNotMatch(markup,/1 turns|2 step(?!s)/);
  c.renderSessionStatsbar({stats:{turns:2,steps:1}},'B');
  assert.match(markup,/2 turns · 1 step/);
  c.renderSessionStatsbar({stats:{toolCalls:1}},'B');
  assert.match(markup,/1 tool call/);
  assert.doesNotMatch(markup,/1 tool calls/);
  c.currentSessionId='C';
  const empty=c.loadSessionStatsbar('C');
  assert.equal(document.activeElement,inputField,'session switch sends focus to the composer');
  assert.equal(bar.style.display,'none','cached B stats must not appear in C');
  assert.equal(dock.hidden,true,'dock collapses with no owned stats or context');
  reply(4,{});await empty;
  assert.equal(bar.style.display,'none','no recorded stats leaves dock empty');
  c.currentSessionId=null;await c.loadSessionStatsbar();
  assert.equal(bar.style.display,'none','blank composer has no session stats');
  assert.equal(dock.hidden,true);
  await tick();
})().catch(error=>{console.error(error);process.exitCode=1});
`, "app.js", "sessions.js")
}
