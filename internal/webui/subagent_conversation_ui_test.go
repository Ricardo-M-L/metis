package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The reader is loaded together with the real app state/ownership controller
// and Markdown formatter. No renderer stub may make a new-library test pass.
func runSubagentConversationUI(t *testing.T, scenario string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"history_virtual.js", "app.js", "chat.js", "subagent_view.js"} {
		data, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(data)
	}
	app := sources["app.js"]
	start := strings.Index(app, "function renderStatusSnapshot(d)")
	if start < 0 {
		t.Fatal("cannot locate sub-agent application controller")
	}
	end := strings.Index(app[start:], "// ============================================================\n// Layout")
	if start < 0 || end <= 0 {
		t.Fatal("cannot locate sub-agent application controller")
	}
	sources["app.js"] = strings.ReplaceAll(app[start:start+end], "setInterval(pollStatus, 3000);\npollStatus();", "")
	payload, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", subagentConversationUIHarness, scenario)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sub-agent reader %s: %v\n%s", scenario, err, output)
	}
}

func TestSubagentConversationReader(t *testing.T) {
	for _, scenario := range []string{"messages_tools_safe", "tool_pairing_boundaries", "owner_generation", "paused_updates", "pagination_anchor", "directory_usage", "catalog_pagination_refresh", "reopen_paused_canonical", "reopen_chrome_latest", "late_fetch_keeps_delta", "pagination_cursor_advances", "live_preview_boundaries", "keyboard_presentation", "parent_owned_capability"} {
		t.Run(scenario, func(t *testing.T) { runSubagentConversationUI(t, scenario) })
	}
}

const subagentConversationUIHarness = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const sources=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
const scenario=process.argv[1];
const encode=s=>String(s??'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const decode=s=>String(s).replace(/&quot;/g,'"').replace(/&#39;|&#x27;/g,"'").replace(/&lt;/g,'<').replace(/&gt;/g,'>').replace(/&amp;/g,'&');
let c;
class TextNode {
  constructor(text){this.nodeType=3;this._text=String(text);this.parentElement=null;}
  get textContent(){return this._text;}set textContent(v){this._text=String(v);}
  get outerHTML(){return encode(this._text);}remove(){this.parentElement?.removeChild(this);}
  get isConnected(){return !!this.parentElement?.isConnected;}
}
class Element {
  constructor(tag='div') {
    this.nodeType=1;this.tagName=tag.toUpperCase();this.parentElement=null;this.childNodes=[];this.attrs={};this.style={};this.dataset={};this.listeners={};
    this.hidden=false;this.disabled=false;this._scrollTop=0;this.clientHeight=400;this.clientWidth=800;this.htmlWrites=0;
    this.classList={contains:x=>this.className.split(/\s+/).includes(x),add:(...xs)=>{this.className=[...new Set([...this.className.split(/\s+/).filter(Boolean),...xs])].join(' ');},remove:(...xs)=>{this.className=this.className.split(/\s+/).filter(x=>!xs.includes(x)).join(' ');},toggle:(x,on)=>{on=on===undefined?!this.classList.contains(x):on;this.classList[on?'add':'remove'](x);return on;}};
  }
  get id(){return this.attrs.id||'';}set id(v){this.attrs.id=String(v);}
  get className(){return this.attrs.class||'';}set className(v){this.attrs.class=String(v);}
  get children(){return this.childNodes.filter(x=>x.nodeType===1);}
  get firstChild(){return this.childNodes[0]||null;}get lastChild(){return this.childNodes.at(-1)||null;}
  get firstElementChild(){return this.children[0]||null;}get lastElementChild(){return this.children.at(-1)||null;}
  get nextSibling(){const siblings=this.parentElement?.childNodes||[];return siblings[siblings.indexOf(this)+1]||null;}
  get textContent(){return this.childNodes.map(x=>x.textContent).join('');}set textContent(v){this.replaceChildren(new TextNode(v??''));}
  get innerHTML(){return this.childNodes.map(x=>x.outerHTML).join('');}
  set innerHTML(value){this.htmlWrites++;this.replaceChildren();parseHTML(String(value),this);}
  get outerHTML(){return '<'+this.tagName.toLowerCase()+Object.entries(this.attrs).map(([k,v])=>' '+k+'="'+encode(v)+'"').join('')+'>'+this.innerHTML+'</'+this.tagName.toLowerCase()+'>';}
  appendChild(node){if(node.nodeType===11){for(const child of [...node.childNodes])this.appendChild(child);return node;}node.parentElement?.removeChild(node);this.childNodes.push(node);node.parentElement=this;return node;}
  append(...nodes){for(const node of nodes)this.appendChild(typeof node==='string'?new TextNode(node):node);}
  prepend(...nodes){for(const node of nodes.reverse())this.insertBefore(typeof node==='string'?new TextNode(node):node,this.firstChild);}
  insertBefore(node,before){if(!before)return this.appendChild(node);node.parentElement?.removeChild(node);const i=this.childNodes.indexOf(before);assert(i>=0);this.childNodes.splice(i,0,node);node.parentElement=this;return node;}
  removeChild(node){const i=this.childNodes.indexOf(node);if(i>=0)this.childNodes.splice(i,1);node.parentElement=null;return node;}
  replaceChildren(...nodes){for(const old of [...this.childNodes])this.removeChild(old);this.append(...nodes);}
  replaceWith(node){const parent=this.parentElement;if(parent){parent.insertBefore(node,this);this.remove();}}
  remove(){this.parentElement?.removeChild(this);}
  setAttribute(k,v){this.attrs[k]=String(v);if(k.startsWith('data-'))this.dataset[k.slice(5).replace(/-([a-z])/g,(_,x)=>x.toUpperCase())]=String(v);if(k==='hidden')this.hidden=true;if(k==='disabled')this.disabled=true;}
  getAttribute(k){if(k.startsWith('data-'))return this.dataset[k.slice(5).replace(/-([a-z])/g,(_,x)=>x.toUpperCase())]??null;return this.attrs[k]??null;}
  removeAttribute(k){delete this.attrs[k];if(k==='hidden')this.hidden=false;if(k==='disabled')this.disabled=false;}
  hasAttribute(k){return this.getAttribute(k)!==null;}
  addEventListener(k,fn){(this.listeners[k]??=[]).push(fn);}removeEventListener(k,fn){this.listeners[k]=(this.listeners[k]||[]).filter(x=>x!==fn);}
  dispatch(type,values={}){const event={type,target:this,currentTarget:this,defaultPrevented:false,preventDefault(){this.defaultPrevented=true;},stopPropagation(){this.stopped=true;},...values};for(const fn of this.listeners[type]||[])fn(event);return event;}
  click(){if(!this.disabled)this.dispatch('click');}focus(){c.document.activeElement=this;}
  contains(node){return node===this||this.childNodes.some(x=>x===node||x.contains?.(node));}
  matches(selector){return matches(this,selector);}
  closest(selector){for(let n=this;n;n=n.parentElement)if(matches(n,selector))return n;return null;}
  querySelectorAll(selector){const all=[];for(const child of this.children){if(matches(child,selector))all.push(child);all.push(...child.querySelectorAll(selector));}return all;}
  querySelector(selector){return this.querySelectorAll(selector)[0]||null;}
  insertAdjacentHTML(where,html){const fragment=new Element();parseHTML(html,fragment);if(where==='beforeend')this.append(...fragment.childNodes);else if(where==='afterbegin')this.prepend(...fragment.childNodes);else throw Error('unsupported position '+where);}
  get scrollTop(){return this._scrollTop||0;}set scrollTop(value){this._scrollTop=Math.max(0,Math.min(Number(value)||0,this.scrollHeight-this.clientHeight));}
  get scrollHeight(){return this._scrollHeight??Math.max(this.clientHeight,this.children.reduce((n,x)=>n+x.offsetHeight,0));}
  get offsetHeight(){if(this.hidden)return 0;if(this.tagName==='DETAILS'&&!this.open)return this.firstElementChild?.offsetHeight||28;if(this.style.height)return parseFloat(this.style.height)||0;return this._height??Math.max(28,Math.ceil(this.textContent.length/80)*20+this.children.reduce((n,x)=>n+x.offsetHeight,0));}
  getBoundingClientRect(){if(this.id==='subAgentDetailBody')return {top:0,bottom:this.clientHeight,left:0,right:800,width:800,height:this.clientHeight};let top=0;for(let n=this;n?.parentElement;n=n.parentElement){for(const s of n.parentElement.children){if(s===n)break;top+=s.offsetHeight;}top-=n.parentElement.scrollTop||0;if(n.parentElement.id==='subAgentDetailBody')break;}return {top,bottom:top+this.offsetHeight,left:0,right:800,width:800,height:this.offsetHeight};}
  get isConnected(){return this===root||!!this.parentElement?.isConnected;}
  scrollTo(options){this.scrollTop=typeof options==='number'?options:Number(options.top)||0;}
  scrollIntoView(){if(this.parentElement)this.parentElement.scrollTop=this.parentElement.scrollHeight;}
}
function matches(el,selector){
  return selector.split(',').some(raw=>{
    let s=raw.trim().replace(/^:scope\s*>?\s*/,'').replace(/\s*>\s*/g,' ');if(s.includes(':focus')){if(c.document.activeElement!==el)return false;s=s.replace(':focus','');}
    if(s.includes(' ')){const parts=s.split(/\s+/);if(!matches(el,parts.pop()))return false;let p=el.parentElement;for(const part of parts.reverse()){while(p&&!matches(p,part))p=p.parentElement;if(!p)return false;p=p.parentElement;}return true;}
    const base=s.replace(/\[[^\]]+\]/g,'');
    const tag=/^[a-z][\w-]*/i.exec(base)?.[0];if(tag&&el.tagName!==tag.toUpperCase())return false;
    for(const cls of base.matchAll(/\.([\w-]+)/g))if(!el.classList.contains(cls[1]))return false;
    const id=/#([\w-]+)/.exec(base)?.[1];if(id&&el.id!==id)return false;
    for(const [,key,value]of s.matchAll(/\[([\w-]+)(?:=["']?([^\]"']*)["']?)?\]/g)){const actual=el.getAttribute(key);if(actual===null||(value!==undefined&&actual!==value))return false;}
    return true;
  });
}
function parseHTML(html,parent){
  const stack=[parent];for(const token of String(html).match(/<!--[\s\S]*?-->|<[^>]+>|[^<]+/g)||[]){
    if(token.startsWith('<!--'))continue;
    if(/^<\//.test(token)){if(stack.length>1)stack.pop();continue;}
    if(token.startsWith('<')){const tag=/^<([\w-]+)/.exec(token)?.[1];if(!tag)continue;const el=new Element(tag);for(const [,key,a,b]of token.matchAll(/([\w-]+)(?:="([^"]*)"|='([^']*)')?/g)){if(key===tag)continue;el.setAttribute(key,decode(a??b??''));}stack.at(-1).appendChild(el);if(!/^(br|hr|img|input|meta|link)$/i.test(tag)&&!token.endsWith('/>'))stack.push(el);}
    else stack.at(-1).appendChild(new TextNode(decode(token)));
  }
}
const root=new Element('body'),frames=new Map(),timers=new Map(),intervals=new Map(),requests=[],streams=[];
let nextTimer=0,now=Date.parse('2026-10-28T00:00:00Z');
const ids=['statusPopover','statusChip','subAgentDetailOverlay','subAgentDetailTitle','subAgentDetailDescription','subAgentDetailBody','subAgentDetailMetrics','subAgentDetailInfo','subAgentDetailFooterActions','subAgentDetailClose','subAgentInfoToggle','subAgentPresentationToggle','subAgentBack','subAgentReadOnlyLabel','chatArea'];
for(const id of ids){const el=new Element();el.id=id;root.appendChild(el);}
const el=id=>root.querySelector('#'+id),body=el('subAgentDetailBody'),overlay=el('subAgentDetailOverlay');
overlay.hidden=true;overlay.dataset.presentation='page';const dialog=new Element('section');dialog.className='subagent-detail-dialog';overlay.appendChild(dialog);
const agent={ownership:'parent',parentSessionId:'A',canAcceptDirectInput:false,directInputBlockedReason:'parent_owned',agentId:'child',sessionId:'A',name:'reviewer',task:'Review transport',status:'running',startedAt:new Date(now-90000).toISOString(),elapsedMs:90000,output:'',conversationRevision:1};
let conversation={agent:{...agent},messages:[],startIndex:0,totalMessages:0,hasMore:false,nextBefore:0,revision:1,completeness:{state:'recorded',contextResets:0,liveTailAvailable:false},segments:[],usage:{available:false,totalTokens:null}};
let respond=async(url,options)=>({ok:true,json:async()=>url.includes('/conversation')?conversation:url.startsWith('/api/subagents?')?{sessionId:'A',agents:[{...agent}],hasMore:false}:options?.method==='POST'?{agent:{...agent}}:{agent:{...agent}}});
c={console,Promise,Map,Set,Array,AbortController,URL,URLSearchParams,encodeURIComponent,performance:{now:()=>now},
  currentSessionId:'A',lastStatusSnapshot:null,currentView:'chat',messages:[],DESKTOP_I18N:{'zh-CN':{subAgents:'子代理',backgroundTasks:'后台任务'},en:{subAgents:'sub-agents',backgroundTasks:'background tasks'}},
  subAgentDetailState:{agentId:'',ownerSessionId:'',trigger:null,data:null,loading:false,error:'',requestGeneration:0,stopping:false,stopGeneration:0},subAgentDetailStream:null,subAgentDetailStreamGeneration:0,
  Date:class extends Date {static now(){return now;}},
  document:{documentElement:{lang:'en'},body:root,getElementById:el,createElement:tag=>new Element(tag),createTextNode:text=>new TextNode(text),createDocumentFragment:()=>{const f=new Element();f.nodeType=11;return f;},querySelector:s=>root.querySelector(s),querySelectorAll:s=>root.querySelectorAll(s),addEventListener(){},removeEventListener(){},activeElement:null},
  requestAnimationFrame:fn=>{const id=++nextTimer;frames.set(id,fn);return id;},cancelAnimationFrame:id=>frames.delete(id),
  setTimeout:(fn,ms)=>{const id=++nextTimer;timers.set(id,{fn,ms});return id;},clearTimeout:id=>timers.delete(id),setInterval:(fn,ms)=>{const id=++nextTimer;intervals.set(id,fn);return id;},clearInterval:id=>intervals.delete(id),queueMicrotask,
  getComputedStyle:()=>({marginTop:'0',marginBottom:'0',fontSize:'15px',fontFamily:'system',lineHeight:'24px'}),escHtml:encode,escAttr:encode,uiText:(en,zh)=>c.document.documentElement.lang==='en'?en:zh,fmtMs:ms=>String(ms),renderContextMeter(){},pollStatus(){},
  fetch:(url,options)=>{requests.push({url,options});return respond(String(url),options);},
  EventSource:class {constructor(url){this.url=url;this.listeners={};this.closed=false;streams.push(this);}addEventListener(k,fn){this.listeners[k]=fn;}close(){this.closed=true;}emit(k,payload){this.listeners[k]?.({data:JSON.stringify(payload)});}},
};c.window=c;vm.createContext(c);
// Formatters are production code, including link protocol/HTML escaping.
vm.runInContext(sources['history_virtual.js'],c);
vm.runInContext(sources['chat.js'],c);
vm.runInContext(sources['app.js'],c);
vm.runInContext(sources['subagent_view.js'],c);
function flushFrames(){for(let i=0;frames.size;i++){assert(i<30,'animation frames must settle');const batch=[...frames.values()];frames.clear();for(const fn of batch)fn();}}
async function tick(){await new Promise(resolve=>setImmediate(resolve));flushFrames();await new Promise(resolve=>setImmediate(resolve));}
async function settle(){for(let i=0;i<4;i++){for(const [id,t]of [...timers]){timers.delete(id);t.fn();}await tick();}}
function stream(){return streams.at(-1);}
const readerState=()=>vm.runInContext('subAgentConversationState',c);
function revealMessage(index){const state=readerState();assert(state?.viewport,'the real history window must be active');const unit=state.units.find(unit=>unit.model.index===index);assert(unit,'source message is retained in the data model: '+index);state.viewport.reveal(unit.key);flushFrames();return body.querySelector('[data-source-index="'+index+'"]');}
function open(id='child',owner='A'){return c.openSubAgentDetails(id,el('statusChip'),owner);}
function snapshot(data={}){stream().emit('snapshot',{agent:{...agent,...data}});}
function text(){return root.textContent;}
function recorded(messages,extra={}){
  conversation={agent:{...agent},messages,startIndex:0,totalMessages:messages.length,hasMore:false,nextBefore:null,revision:'r1',completeness:{state:'recorded',contextResets:0,liveTailAvailable:false},segments:[],usage:{available:false,inputTokens:null,outputTokens:null,totalTokens:null},...extra};return conversation;
}
async function show(messages,extra={}){
  recorded(messages,extra);assert.equal(open(),true);snapshot();await settle();return body;
}
const message=(role,text)=>({role,content:[{type:'text',text}]});
const stopButton=()=>{const button=el('subAgentDetailFooterActions').querySelector('.subagent-detail-stop');return button&&!button.hidden?button:null;};
async function run(){
  if(scenario==='parent_owned_capability'){
    await show([message('user','Inspect the project'),message('assistant','Done')]);
    assert.equal(el('subAgentReadOnlyLabel').textContent,'Managed by parent agent');
    assert.match(el('subAgentReadOnlyLabel').title,/parent conversation/);
    assert.equal(root.querySelector('textarea'),null,'child reader must not create an independent composer');
    assert.equal(stopButton()?.textContent,'Stop agent','parent-managed live child remains stoppable');
    snapshot({status:'completed'});await settle();
    assert.equal(el('subAgentReadOnlyLabel').textContent,'Managed by parent agent');
    assert.equal(stopButton(),null,'completed child has no stop action');
    c.document.documentElement.lang='zh-CN';c.renderSubAgentConversationView();flushFrames();
    assert.equal(el('subAgentReadOnlyLabel').textContent,'由主代理管理');
    assert.match(el('subAgentReadOnlyLabel').title,/主对话/);
    c.closeSubAgentDetails(false);
  }else if(scenario==='panel_lifecycle'){
    const content='# Transport review\n\nReading **transport.go**';
    await show([message('user','Review transport'),message('assistant',content)]);
    assert.equal(overlay.hidden,false);assert.equal(el('subAgentDetailTitle').textContent,'reviewer');
    assert.match(el('subAgentDetailDescription').textContent,/Running/);
    assert.equal(body.querySelector('h1').textContent,'Transport review','normal Markdown is rendered in the child lane');
    assert.match(body.textContent,/transport.go/);
    assert(requests.some(r=>r.url==='/api/subagents/child/conversation?sessionId=A&limit=100'),'the on-demand transcript is parent scoped');
    assert.equal(c.messages.length,0,'child prose does not append to the parent transcript');
    const old=stream(),postCount=requests.filter(r=>r.options?.method==='POST').length;
    c.closeSubAgentDetails();assert.equal(overlay.hidden,true);assert.equal(old.closed,true);
    assert.equal(requests.filter(r=>r.options?.method==='POST').length,postCount,'close only closes transport; it does not stop work');
    assert.equal(c.document.activeElement,el('statusChip'),'close restores the entry control focus');
    c.currentSessionId='B';const count=requests.length;
    assert.equal(open('child','A'),false);assert.equal(requests.length,count);assert.equal(overlay.hidden,true);
    c.currentSessionId='A';open();snapshot();await settle();const active=stream();
    c.currentSessionId='B';c.renderStatusSnapshot({viewRoster:{sessionId:'A',agents:[agent],subAgents:1,jobs:[]}});
    assert.equal(overlay.hidden,true);assert.equal(active.closed,true,'switching the parent session closes the child lane');
    active.emit('delta',{agentId:'child',sessionId:'A',delta:'LATE_FOREIGN_TEXT'});assert.doesNotMatch(body.textContent,/LATE_FOREIGN_TEXT/);
  }else if(scenario==='messages_tools_safe'){
    const long='LONG_BEGIN\n'+('complete output '+ '<img src=x onerror=evil()>\n').repeat(6000)+'LONG_END';
    const data=[message('user','FIRST_TASK'),
      {role:'assistant',content:[{type:'thinking',text:'Reason safely <script>evil()</script>'},{type:'text',text:'# Review\n\n**Complete** [bad](javascript:evil)\n\n'+String.fromCharCode(96).repeat(3)+'js\nconsole.log("<safe>")\n'+String.fromCharCode(96).repeat(3)+'\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n$x+1$\n\n<script>evil()</script>'},{type:'tool_use',tool_use_id:'reused',name:'Read',input:{path:'first.txt'}}]},
      {role:'user',content:[{type:'tool_result',tool_use_id:'reused',content:'FIRST_TOOL_RESULT'}]},message('assistant','FIRST_COMPLETE'),
      message('user','SECOND_TASK'),{role:'assistant',content:[{type:'tool_use',tool_use_id:'reused',name:'Bash',input:{command:'long output'}}]},
      {role:'user',content:[{type:'tool_result',tool_use_id:'reused',content:long,is_error:true}]},message('assistant','FINAL_REPLY')];
    await show(data);
    const models=c.buildSubAgentViewUnits(data.map((m,i)=>[i,m]));
    const projected=models.flatMap(unit=>unit.blocks).filter(block=>block.kind==='tool');
    assert.equal(projected.length,2);assert.equal(projected[0].tool.output,'FIRST_TOOL_RESULT');assert.equal(projected[1].tool.output,long);assert.equal(projected[1].tool.status,'failed');
    const first=revealMessage(1);assert(first);assert.equal(first.querySelectorAll('h1').length,1);assert(first.querySelector('strong'));assert(first.querySelector('.md-codeblock'));assert(first.querySelector('table'));assert(first.querySelector('.md-math'));assert(first.querySelector('.subagent-thinking'),'recorded reasoning has its own disclosure');
    assert.equal(first.querySelectorAll('script').length,0);assert.equal(first.querySelectorAll('img').length,0);assert.equal(first.querySelector('a').getAttribute('href'),'#','unsafe link protocols remain inert');
    const firstTool=first.querySelector('.subagent-tool');firstTool.open=true;firstTool.dispatch('toggle');flushFrames();assert.match(firstTool.textContent,/first.txt/);assert.match(firstTool.textContent,/FIRST_TOOL_RESULT/);assert.doesNotMatch(firstTool.textContent,/LONG_BEGIN/);
    const second=revealMessage(5),secondTool=second.querySelector('.subagent-tool');secondTool.open=true;secondTool.dispatch('toggle');flushFrames();assert.match(secondTool.textContent,/LONG_BEGIN/);assert.match(secondTool.textContent,/LONG_END/);assert.doesNotMatch(secondTool.textContent,/FIRST_TOOL_RESULT/);assert(secondTool.textContent.length>=long.length,'full tool output remains reachable without preview truncation');assert.equal(secondTool.querySelectorAll('img').length,0);
    assert.match(revealMessage(0).textContent,/FIRST_TASK/);assert.match(revealMessage(4).textContent,/SECOND_TASK/);assert.match(revealMessage(7).textContent,/FINAL_REPLY/);
  }else if(scenario==='tool_pairing_boundaries'){
    const call=(trace)=>({type:'tool_use',tool_use_id:'dup',trace_call_id:trace,name:'Read',input:{path:trace||'ambiguous'}});
    const result=(trace,content)=>({type:'tool_result',tool_use_id:'dup',trace_call_id:trace,content});
    const traced=c.buildSubAgentViewUnits([[0,{role:'assistant',content:[call('trace-a'),call('trace-b')]}],[1,{role:'user',content:[result('trace-b','SECOND'),result('trace-a','FIRST')]}]]);
    const tools=traced.flatMap(unit=>unit.blocks).filter(item=>item.kind==='tool');assert.equal(tools.length,2);assert.equal(tools[0].tool.output,'FIRST');assert.equal(tools[1].tool.output,'SECOND');
    const ambiguous=c.buildSubAgentViewUnits([[0,{role:'assistant',content:[call(''),call('')]}],[1,{role:'user',content:[result('','UNATTRIBUTED')]}]]).flatMap(unit=>unit.blocks).filter(item=>item.kind==='tool');
    assert.equal(ambiguous.length,3);assert.equal(ambiguous[0].tool.status,'running');assert.equal(ambiguous[1].tool.status,'running');assert.equal(ambiguous[2].tool.output,'UNATTRIBUTED','an ambiguous record cannot be attached to an arbitrary duplicate call');
    const reset=c.buildSubAgentViewUnits([[0,{role:'assistant',content:[call('old')]}],[1,{role:'user',content:[result('old','AFTER_RESET')]}]],[],[{startIndex:1,reason:'context_reset'}]);
    assert.equal(reset[0].blocks[0].tool.status,'running');assert.equal(reset[1].blocks[0].tool.output,'AFTER_RESET');assert.equal(reset[1].reset,true,'a context reset explicitly breaks tool ownership continuity');
    const gap=c.buildSubAgentViewUnits([[0,{role:'assistant',content:[call('old')]}],[5,{role:'user',content:[result('old','AFTER_GAP')]}]]);assert.equal(gap[0].blocks[0].tool.status,'running');assert.equal(gap[1].blocks[0].tool.output,'AFTER_GAP');
  }else if(scenario==='owner_generation'){
    recorded([message('assistant','INITIAL')]);let releaseOld;
    respond=(url,options)=>url.includes('/conversation')?new Promise(resolve=>{releaseOld=resolve;}):Promise.resolve({ok:true,json:async()=>({agent:{...agent}})});
    open();snapshot();await tick();for(const [id,t]of [...timers]){timers.delete(id);t.fn();}await tick();
    assert.equal(typeof releaseOld,'function','conversation request actually starts');
    const old=stream(),oldFetch=releaseOld;c.closeSubAgentDetails(false);open('other','A');
    stream().emit('snapshot',{agent:{...agent,agentId:'other',name:'CURRENT'}});
    old.emit('snapshot',{agent:{...agent,name:'LATE_OLD_SNAPSHOT'}});old.emit('delta',{agentId:'child',sessionId:'A',delta:'LATE_OLD_DELTA'});
    oldFetch({ok:true,json:async()=>recorded([message('assistant','LATE_OLD_FETCH')])});await tick();
    assert.equal(c.subAgentDetailState.agentId,'other');assert.equal(c.subAgentDetailState.data.name,'CURRENT');
    assert.doesNotMatch(body.textContent,/LATE_OLD/,'late snapshot/delta/fetch never enters the newly selected child');
    c.closeSubAgentDetails(false);
    let releaseSame;respond=(url,options)=>url.includes('/conversation')?new Promise(resolve=>{releaseSame=resolve;}):Promise.resolve({ok:true,json:async()=>({agent:{...agent}})});
    open();snapshot();await settle();const firstRequest=releaseSame,firstStream=stream();
    c.closeSubAgentDetails(false);open();snapshot({name:'REOPENED'});await tick();
    firstStream.emit('snapshot',{agent:{...agent,name:'OLD_REOPEN_SNAPSHOT'}});firstRequest({ok:true,json:async()=>recorded([message('assistant','OLD_REOPEN_FETCH')])});await tick();
    assert.equal(c.subAgentDetailState.data.name,'REOPENED');assert.doesNotMatch(body.textContent,/OLD_REOPEN/,'generation also isolates close/reopen of the same child and owner');
    c.closeSubAgentDetails(false);
  }else if(scenario==='queued_cancel'){
    const queued={...agent,status:'queued',output:''};recorded([],{agent:queued});open();snapshot(queued);await settle();
    assert.match(body.textContent,/has not started yet/);assert.equal(stopButton().textContent,'Cancel queued agent');
    const elapsed=el('subAgentDetailMetrics');assert.match(elapsed.textContent,/1m 30s/);
    const button=stopButton();button.focus();const writes=body.htmlWrites,firstNode=body.firstChild;now+=1000;for(const fn of intervals.values())fn();
    assert.equal(body.htmlWrites,writes,'elapsed/status telemetry cannot redraw the conversation');assert.equal(c.document.activeElement,button);assert.equal(body.firstChild,firstNode,'clock telemetry must preserve mounted reader nodes');assert.match(el('subAgentDetailMetrics').textContent,/1m 31s/);
    let release;respond=(url,options)=>options?.method==='POST'?new Promise(resolve=>{release=resolve;}):Promise.resolve({ok:true,json:async()=>url.includes('/conversation')?conversation:{agent:queued}});
    stopButton().click();flushFrames();const posts=()=>requests.filter(r=>r.options?.method==='POST');assert.equal(posts().length,1);assert(stopButton().disabled);stopButton().click();assert.equal(posts().length,1);
    release({ok:true,json:async()=>({agent:queued})});await tick();assert.equal(c.subAgentDetailState.data.status,'queued','HTTP200 cancellation ACK is not proof of terminal state');assert(stopButton());
    stopButton().click();const oldStop=release;const current=stream();current.emit('snapshot',{agent:{...agent,status:'running'}});oldStop({ok:true,json:async()=>({agent:queued})});await tick();
    assert.equal(c.subAgentDetailState.data.status,'running','a newer stream owns state over an older stop response');assert.match(stopButton().textContent,/^Stop (?:sub-)?agent$/);
    respond=(url,options)=>Promise.resolve({ok:false,json:async()=>({error:'Unavailable'})});stopButton().click();await tick();assert.equal(c.subAgentDetailState.data.status,'running');assert.equal(stopButton().disabled,false);assert.match(text(),/Unable to stop/);
    respond=(url,options)=>options?.method==='POST'?new Promise(resolve=>{release=resolve;}):Promise.resolve({ok:true,json:async()=>url.includes('/conversation')?{...conversation,agent:{...agent}}:{agent:{...agent}}});stopButton().click();const oldAck=release;c.closeSubAgentDetails(false);open();stream().emit('snapshot',{agent:{...agent,name:'NEW_OPEN'}});oldAck({ok:true,json:async()=>({agent:{...agent,status:'killed'}})});await tick();assert.equal(c.subAgentDetailState.data.status,'running','same child reopen invalidates an old stop ACK');
    conversation.agent={...agent,status:'killed',stopHint:'cancelled',exitError:'context canceled'};stream().emit('terminal',{agent:conversation.agent});await settle();assert.equal(stopButton(),null);assert.equal(c.subAgentDetailState.elapsedTimer,null,'terminal state stops the elapsed timer');assert.match(text(),/Cancelled/);assert.doesNotMatch(text(),/Error: context canceled/);
    c.closeSubAgentDetails();assert.equal(intervals.size,0,'closing also clears the reader refresh interval');
  }else if(scenario==='paused_updates'){
    const content=Array.from({length:80},(_,i)=>'READ_'+i+' A paragraph of content.').join('\n\n');await show([message('assistant',content)]);
    body.scrollTop=160;body.dispatch('wheel',{deltaY:-80});body.dispatch('scroll');const top=body.scrollTop;
    stream().emit('delta',{agentId:'child',sessionId:'A',delta:' new tail'});await settle();assert.equal(body.scrollTop,top,'delta cannot grab a paused reader');
    recorded([message('assistant',content+'\n\nFINAL')],{revision:'r2'});stream().emit('terminal',{agent:{...agent,status:'completed',output:content+'\n\nFINAL',conversationRevision:'r2'}});await settle();assert.equal(body.scrollTop,top,'terminal and recorded conversation replacement preserve reading');assert.match(body.textContent,/READ_20/);
  }else if(scenario==='pagination_anchor'){
    const newerText='NEWER_MARKER\n\n'+Array.from({length:25},(_,i)=>'NEWER_PARAGRAPH_'+i+' '+('read '.repeat(30))).join('\n\n');recorded([message('assistant',newerText)],{startIndex:1,totalMessages:2,hasMore:true,nextBefore:1});open();snapshot();await settle();
    const earlierUnit=readerState().units.find(unit=>unit.model.index==='earlier');assert(earlierUnit);readerState().viewport.reveal(earlierUnit.key);flushFrames();const older=body.querySelector('.subagent-load-earlier');assert(older,'a real control exposes unmounted earlier history');body.scrollTop=24;body.dispatch('wheel',{deltaY:-60});body.dispatch('scroll');const anchor=body.querySelectorAll('.message-content p').find(node=>node.getBoundingClientRect().bottom>0),anchorText=anchor.textContent,y=anchor.getBoundingClientRect().top;assert(anchor,'the page has a visible reading paragraph');
    respond=async(url)=>({ok:true,json:async()=>url.includes('before=1')?{...conversation,messages:[message('user','EARLIER_MARKER')],startIndex:0,totalMessages:2,hasMore:false,nextBefore:null}:conversation});
    older.click();await settle();assert(requests.some(r=>r.url.includes('before=1')));assert.match(body.textContent,/EARLIER_MARKER/);assert.match(body.textContent,/NEWER_MARKER/);assert.equal(readerState().messages.size,2,'both persisted messages remain in the data model');
    const newer=body.querySelectorAll('.message-content p').find(n=>n.textContent===anchorText);assert(newer);assert.equal(newer.getBoundingClientRect().top,y,'prepend compensates the visible message offset');
  }else if(scenario==='directory_usage'){
    c.lastStatusSnapshot={viewRoster:{sessionId:'A',agents:[],subAgents:0,backgroundTasks:0,jobs:[]}};
    respond=async(url)=>({ok:true,json:async()=>({sessionId:'A',agents:[{...agent,agentId:'unknown',name:'Unknown usage',status:'completed',usage:{available:false,totalTokens:null}},{...agent,agentId:'zero',name:'Zero usage',status:'completed',usage:{available:true,inputTokens:0,outputTokens:0,totalTokens:0}}],hasMore:false,nextCursor:''})});
    el('statusPopover').style.display='none';c.renderStatusSnapshot(c.lastStatusSnapshot);await settle();c.toggleStatusPopover();const pop=el('statusPopover');assert(requests.some(r=>r.url.startsWith('/api/subagents?sessionId=A')),'a cold directory is fetched without live roster samples');
    const rows=pop.querySelectorAll('[data-subagent-id]');assert.equal(rows.length,2);const unknown=rows.find(n=>n.dataset.subagentId==='unknown'),zero=rows.find(n=>n.dataset.subagentId==='zero');assert(unknown);assert(zero);assert.match(zero.textContent,/0/);assert.doesNotMatch(unknown.textContent,/0\s*(?:tok|tokens)/i,'unknown tokens are not fabricated as zero');assert.notEqual(unknown.textContent,zero.textContent);
    const pendingCatalog=new Map();respond=(url)=>new Promise(resolve=>pendingCatalog.set(new URL('http://local'+url).searchParams.get('sessionId'),resolve));c.currentSessionId='B';c.syncSubAgentCatalog();assert(pendingCatalog.has('B'));c.currentSessionId='C';c.syncSubAgentCatalog();assert(pendingCatalog.has('C'));pendingCatalog.get('C')({ok:true,json:async()=>({sessionId:'C',agents:[{...agent,sessionId:'C',agentId:'c-child',name:'CURRENT_C'}],hasMore:false})});await tick();pendingCatalog.get('B')({ok:true,json:async()=>({sessionId:'B',agents:[{...agent,sessionId:'B',agentId:'b-child',name:'LATE_B'}],hasMore:false})});await tick();c.renderStatusPopover();assert.match(pop.textContent,/CURRENT_C/);assert.doesNotMatch(pop.textContent,/LATE_B|Zero usage|Unknown usage/,'late directory pages stay scoped to the current parent');
  }else if(scenario==='catalog_pagination_refresh'){
    c.lastStatusSnapshot={viewRoster:{sessionId:'A',agents:[],subAgents:0,backgroundTasks:0,jobs:[]}};
    let headLoads=0;
    const entry=(id,name)=>({...agent,agentId:id,name,status:'completed',usage:{available:true,totalTokens:10}});
    respond=async(url)=>{const cursor=new URL('http://local'+url).searchParams.get('cursor');let data;
      if(cursor==='page-1')data={sessionId:'A',agents:[entry('older','OLDER_PAGE')],hasMore:true,nextCursor:'page-2'};
      else if(cursor==='page-2')data={sessionId:'A',agents:[entry('oldest','OLDEST_PAGE')],hasMore:false,nextCursor:null};
      else {headLoads++;data={sessionId:'A',agents:headLoads===1?[entry('first','FIRST_PAGE')]:[entry('first','FIRST_REFRESHED'),entry('new','NEW_ON_REFRESH')],hasMore:true,nextCursor:'page-1'};}
      return {ok:true,json:async()=>data};
    };
    el('statusPopover').style.display='none';c.renderStatusSnapshot(c.lastStatusSnapshot);await settle();c.toggleStatusPopover();const pop=el('statusPopover');
    const more=()=>pop.querySelector('[data-agent-more]');assert(more(),'the first page exposes its continuation');more().click();await settle();
    assert.match(pop.textContent,/OLDER_PAGE/);assert.equal(vm.runInContext('subAgentCatalogState.nextCursor',c),'page-2');
    now+=4000;c.syncSubAgentCatalog();await settle();
    assert.equal(headLoads,2,'the periodic refresh really fetches the first page again');assert.match(pop.textContent,/FIRST_REFRESHED/);assert.match(pop.textContent,/NEW_ON_REFRESH/);assert.match(pop.textContent,/OLDER_PAGE/,'refresh cannot discard already-loaded later pages');
    assert.equal(vm.runInContext('subAgentCatalogState.nextCursor',c),'page-2','refresh cannot rewind the continuation to the first page cursor');assert.equal(pop.querySelectorAll('[data-subagent-id]').length,3,'refresh merges exact child identities without duplicating entries');
    more().click();await settle();assert(requests.some(r=>r.url.includes('cursor=page-2')),'the next user action continues after the loaded page');assert.match(pop.textContent,/OLDEST_PAGE/);assert.equal(more(),null,'exhausted pagination removes the load control');
    now+=4000;c.syncSubAgentCatalog();await settle();assert.match(pop.textContent,/OLDER_PAGE/);assert.match(pop.textContent,/OLDEST_PAGE/);assert.equal(more(),null,'refresh cannot revive an exhausted cursor');assert.equal(vm.runInContext('subAgentCatalogState.nextCursor',c),null);
  }else if(scenario==='reopen_paused_canonical'){
    const task=Array.from({length:8},(_,i)=>'TASK_READ_'+i+' '+('A task reading paragraph. '.repeat(12))).join('\n\n');
    const answer=Array.from({length:70},(_,i)=>'ASSISTANT_'+i+' '+('A long answer paragraph. '.repeat(12))).join('\n\n');
    await show([message('user',task),message('assistant',answer)]);revealMessage(0);body.scrollTop=24;body.dispatch('wheel',{deltaY:-80});body.dispatch('scroll');flushFrames();
    const marker=body.querySelectorAll('.message-content p').find(node=>node.textContent.startsWith('TASK_READ_')&&node.getBoundingClientRect().bottom>0);assert(marker,'the paused reader really views a task paragraph');
    const markerText=marker.textContent,markerY=marker.getBoundingClientRect().top;assert.equal(readerState().follow,false);c.closeSubAgentDetails(false);
    let canonical,requestedBefore;
    respond=async(url)=>{
      if(!url.includes('/conversation'))return {ok:true,json:async()=>({agent:{...agent}})};
      const rawBefore=new URL('http://local'+url).searchParams.get('before');requestedBefore=rawBefore===null?null:Number(rawBefore);
      if(requestedBefore!==null&&requestedBefore>conversation.totalMessages)return {ok:false,json:async()=>({error:'conversation cursor exceeds recorded history'})};
      return new Promise(resolve=>{canonical=resolve;});
    };
    open();snapshot({output:answer,conversationRevision:'r1'});await tick();
    assert(requestedBefore===null||requestedBefore<=conversation.totalMessages,'reopen must not derive a before cursor beyond the recorded message total');
    assert.equal(typeof canonical,'function','a valid canonical request is held while the initial preview arrives');
    canonical({ok:true,json:async()=>conversation});await settle();
    const restored=body.querySelectorAll('.message-content p').find(node=>node.textContent===markerText);assert(restored,'canonical history restores the same task paragraph, not an assistant fallback');assert.equal(restored.getBoundingClientRect().top,markerY,'an early SSE preview cannot consume the remembered read anchor');assert.equal(readerState().follow,false);
    stream().emit('terminal',{agent:{...agent,status:'killed',exitError:'context canceled',output:answer,conversationRevision:'r1'}});await tick();assert.equal(restored.getBoundingClientRect().top,markerY);assert.equal(readerState().follow,false,'terminal metadata must preserve the reopened paused task anchor');
    c.closeSubAgentDetails(false);const remembered=vm.runInContext('subAgentReadingMemory.get("A:child")',c);delete remembered.total;canonical=null;requestedBefore=undefined;
    open();snapshot({output:answer});await tick();assert.equal(requestedBefore,null,'older anchor memory without a total must not invent a cursor');assert.equal(typeof canonical,'function');canonical({ok:true,json:async()=>conversation});await settle();
    const legacyRestored=body.querySelectorAll('.message-content p').find(node=>node.textContent===markerText);assert(legacyRestored);assert.equal(legacyRestored.getBoundingClientRect().top,markerY);c.closeSubAgentDetails(false);
  }else if(scenario==='reopen_chrome_latest'){
    const answer=Array.from({length:60},(_,i)=>'LATEST_READ_'+i+' '+('Long answer. '.repeat(15))).join('\n\n');await show([message('assistant',answer)]);
    c.toggleSubAgentInfo();c.toggleSubAgentPresentation();flushFrames();assert.equal(el('subAgentDetailInfo').hidden,false);assert.equal(el('subAgentInfoToggle').getAttribute('aria-expanded'),'true');assert.equal(overlay.dataset.presentation,'side');assert.equal(el('subAgentPresentationToggle').getAttribute('aria-pressed'),'true');
    body.scrollTop=24;body.dispatch('wheel',{deltaY:-80});body.dispatch('scroll');flushFrames();const oldState=readerState(),oldLatest=el('subAgentDetailFooterActions').querySelector('.subagent-return-latest');assert.equal(oldState.follow,false);assert(oldLatest);c.closeSubAgentDetails(false);
    open();snapshot();await settle();const current=readerState();assert.notEqual(current,oldState);assert.equal(el('subAgentDetailInfo').hidden,true,'a new open resets the Info DOM');assert.equal(el('subAgentInfoToggle').getAttribute('aria-expanded'),'false');assert.equal(overlay.dataset.presentation,'page');assert.equal(el('subAgentPresentationToggle').getAttribute('aria-pressed'),'false','presentation control follows the actual page mode');
    body.scrollTop=24;body.dispatch('wheel',{deltaY:-80});body.dispatch('scroll');flushFrames();assert.equal(current.follow,false);const latest=el('subAgentDetailFooterActions').querySelector('.subagent-return-latest');assert(latest);assert.notEqual(latest,oldLatest,'a footer control cannot retain an old reader closure');latest.click();flushFrames();
    assert.equal(current.follow,true,'Latest resumes the current reader');assert.equal(oldState.follow,false,'Latest must not mutate the closed reader');assert.equal(body.scrollHeight-body.scrollTop-body.clientHeight,0);assert.equal(latest.hidden,true);c.closeSubAgentDetails(false);
  }else if(scenario==='late_fetch_keeps_delta'){
    const olderUsage={available:true,inputTokens:90,outputTokens:10,totalTokens:100,cacheReadInputTokens:0,cacheCreationInputTokens:0};const newerUsage={...olderUsage,inputTokens:180,outputTokens:20,totalTokens:200};
    recorded([message('user','Read task'),message('assistant','A')],{agent:{...agent,output:'A',usage:olderUsage},usage:olderUsage});let release;
    respond=(url)=>url.includes('/conversation')?new Promise(resolve=>{release=resolve;}):Promise.resolve({ok:true,json:async()=>({agent:{...agent}})});
    open();snapshot({output:'A',usage:olderUsage});stream().emit('delta',{agentId:'child',sessionId:'A',delta:'B',usage:newerUsage});await tick();assert.equal(c.subAgentDetailState.data.output,'AB');const canonical=release;assert.equal(typeof canonical,'function');
    canonical({ok:true,json:async()=>conversation});await tick();assert.equal(c.subAgentDetailState.data.output,'AB','an older GET cannot erase an in-flight SSE delta');assert.equal(c.subAgentDetailState.data.usage.totalTokens,200);assert.equal(readerState().usage.totalTokens,200,'the Chrome usage source must not regress to the old response');
    stream().emit('delta',{agentId:'child',sessionId:'A',delta:'C'});await tick();assert.equal(c.subAgentDetailState.data.output,'ABC','the next delta appends to the latest stream, not the stale GET');assert.equal(el('subAgentDetailMetrics').querySelector('.subagent-detail-token-value').textContent,'200 tok');
    assert.equal(body.querySelectorAll('.message-body').map(node=>node.textContent).join(''),'Read taskABC','recorded A plus only the remaining BC tail render without loss or duplication');c.closeSubAgentDetails(false);
  }else if(scenario==='pagination_cursor_advances'){
    const history=Array.from({length:300},(_,i)=>message(i%2?'assistant':'user','PAGED_MESSAGE_'+i));let releasePage;const cursors=[];
    respond=(url)=>{if(!url.includes('/conversation'))return Promise.resolve({ok:true,json:async()=>({agent:{...agent}})});const value=new URL('http://local'+url).searchParams.get('before');if(value===null)return Promise.resolve({ok:true,json:async()=>({...conversation,messages:history.slice(200),startIndex:200,totalMessages:300,hasMore:true,nextBefore:200})});const before=Number(value);cursors.push(before);return new Promise(resolve=>{releasePage=()=>resolve({ok:true,json:async()=>({...conversation,messages:history.slice(Math.max(0,before-100),before),startIndex:Math.max(0,before-100),totalMessages:300,hasMore:before>100,nextBefore:before>100?before-100:null})});});};
    await show(history.slice(200),{startIndex:200,totalMessages:300,hasMore:true,nextBefore:200});
    const earlier=()=>{const unit=readerState().units.find(unit=>unit.model.index==='earlier');assert(unit);readerState().viewport.reveal(unit.key);flushFrames();return body.querySelector('.subagent-load-earlier');};
    earlier().click();flushFrames();assert.equal(body.querySelector('.subagent-load-earlier').disabled,true,'the loading state updates the actual mounted pagination control');const firstRelease=releasePage;assert.equal(typeof firstRelease,'function');firstRelease();await settle();assert.equal(readerState().messages.size,200);
    earlier().click();flushFrames();assert.deepEqual(cursors,[200,100],'the replacement control uses the current before cursor rather than an old closure');releasePage();await settle();assert.equal(readerState().messages.size,300);assert.equal(readerState().units.some(unit=>unit.model.index==='earlier'),false);assert.equal(body.querySelector('.subagent-load-earlier'),null);
    for(let i=0;i<300;i++)assert.equal(readerState().messages.get(i).content[0].text,'PAGED_MESSAGE_'+i,'all pages remain complete and ordered');assert.match(revealMessage(0).textContent,/PAGED_MESSAGE_0/);assert.match(revealMessage(299).textContent,/PAGED_MESSAGE_299/);c.closeSubAgentDetails(false);
  }else if(scenario==='live_preview_boundaries'){
    const state=(entries,segments=[])=>({messages:new Map(entries),liveTools:[],segments,projectionEpoch:1,loaded:true,error:''});
    const fixtures=[
      {name:'paged suffix',state:state([[10,message('assistant','COMMITTED_LATEST')]]),agent:{status:'running',output:'OLDER|COMMITTED_LATEST|LIVE_END'}},
      {name:'context reset',state:state([[0,message('assistant','OLD_CONTEXT')],[1,message('assistant','NEW_CONTEXT')]], [{reason:'context_reset',startIndex:1}]),agent:{status:'running',output:'NEW_CONTEXT|LIVE_END'}},
      {name:'bounded output',state:state([[0,message('assistant','PREFIX')]]),agent:{status:'running',output:'PREFIX|LIVE_END',outputTruncated:true}},
    ];
    for(const item of fixtures){
      const models=c.subAgentConversationModels(item.state,item.agent),tail=models.find(model=>model.index==='tail');assert(tail,item.name);assert.equal(tail.live,true);assert.match(tail.notice,/Live output preview/);assert.equal(tail.blocks[0].block.text,item.agent.output,'an unsafe prefix match is never guessed');
      const next=c.subAgentConversationModels(item.state,{...item.agent,output:item.agent.output+'|NEW_DELTA'}).find(model=>model.index==='tail');assert.match(next.blocks[0].block.text,/NEW_DELTA/,'new output remains visible despite paged/reset/truncated history');
      if(item.name==='paged suffix')assert(models.some(model=>model.index==='earlier'&&model.before===10));
      if(item.name==='context reset')assert.equal(models.find(model=>model.index===1).reset,true);
      for(const status of ['completed','failed','killed'])assert.equal(c.subAgentConversationModels(item.state,{...item.agent,status}).some(model=>model.index==='tail'),false,'recorded terminal history does not repeat a mismatched preview');
    }
    const models=c.subAgentConversationModels(state([[0,message('assistant','COMMITTED')]]),{status:'running',output:'COMMITTED|ONLY_TAIL'});const tail=models.find(model=>model.index==='tail');assert.equal(tail.blocks[0].block.text,'|ONLY_TAIL');assert.equal(tail.notice,'','a proven prefix appends exactly once without a preview warning');
  }else if(scenario==='keyboard_presentation'){
    await show([message('assistant','Keyboard-readable answer')]);c.initSubAgentDetails();flushFrames();assert.equal(c.document.activeElement,dialog,'open focuses the reader region');
    c.toggleSubAgentInfo();assert.equal(el('subAgentDetailInfo').hidden,false);assert.equal(el('subAgentInfoToggle').getAttribute('aria-expanded'),'true');
    c.toggleSubAgentPresentation();assert.equal(overlay.dataset.presentation,'side');c.toggleSubAgentPresentation();assert.equal(overlay.dataset.presentation,'page');
    const posts=requests.filter(r=>r.options?.method==='POST').length;overlay.dispatch('keydown',{key:'Escape'});assert.equal(overlay.hidden,true);assert.equal(c.document.activeElement,el('statusChip'));assert.equal(requests.filter(r=>r.options?.method==='POST').length,posts);
    open();c.closeSubAgentDetails();flushFrames();assert.equal(c.document.activeElement===el('statusChip'),true,'a queued open-focus callback cannot refocus the reader after it closes');
  }else throw new Error('unknown reader scenario '+scenario);
}
run().catch(error=>{console.error(error);process.exitCode=1;});
`
