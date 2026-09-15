package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestSessionFilesBrowserLifecycle(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"file identity and cached tabs", `
 await listFiles();
 assert.equal(c.matchSessionFile('/tmp/report.md').id,'1');
 assert.equal(c.matchSessionFile('same.md'),null);
 assert.equal(c.matchSessionFile('https://x/report.md'),null);
 assert.equal(c.matchSessionFile('/work/a/same.md').id,'2');
 assert.equal(c.matchSessionFile('/work/b/same.md').id,'3');
 await c.openSessionFile('2'); await c.openSessionFile('3');
 assert.equal(tabButtons().length,2);
 assert.deepEqual(tabButtons().map(button=>button.title),['/work/a/same.md','/work/b/same.md']);
 await c.openSessionFile('2');
 assert.equal(tabButtons().length,2);
 assert.equal(contentCalls().length,2,'switching to a loaded tab must use its cache');
 assert.equal(sourceText(),'FIRST FILE\n');
 c.closeSessionFile(); await c.openSessionFile('2');
 assert.equal(contentCalls().length,2,'reopening a closed panel must retain its loaded tabs');
 assert.equal(get('sessionFilePanel').hidden,false);
 assert.equal(c.sessionFilePathFromLink('https://evil/a.md'),'');
 assert.equal(c.sessionFilePathFromLink('javascript:alert(1)'),'');
 assert.equal(c.sessionFilePathFromLink('%6aavascript:alert(1)'),'');
 assert.equal(c.sessionFilePathFromLink('/tmp/report.md:12'),'/tmp/report.md');
`},
		{"per tab source wrap and scroll", `
 await listFiles(); await c.openSessionFile('1');
 assert.equal(get('sessionFileContent').classList.contains('is-markdown'),true);
 c.toggleSessionFileMode(); c.toggleSessionFileWrap();
 assert.equal(get('sessionFileContent').classList.contains('is-code'),true);
 assert.equal(get('sessionFileWrap').getAttribute('aria-pressed'),'true');
 get('sessionFileContent').scrollTop=260; get('sessionFileContent').scrollLeft=31;
 await c.openSessionFile('2');
 assert.equal(get('sessionFileWrap').getAttribute('aria-pressed'),'false');
 get('sessionFileContent').scrollTop=75; get('sessionFileContent').scrollLeft=8;
 await c.openSessionFile('1');
 assert.equal(get('sessionFileContent').classList.contains('is-code'),true);
 assert.equal(get('sessionFileWrap').getAttribute('aria-pressed'),'true');
 assert.equal(get('sessionFileContent').querySelector('.source-text').dataset.wrap,'true');
 assert.equal(get('sessionFileContent').scrollTop,260);
 assert.equal(get('sessionFileContent').scrollLeft,31);
 await c.openSessionFile('2');
 assert.equal(get('sessionFileContent').scrollTop,75);
 assert.equal(get('sessionFileContent').scrollLeft,8);
 assert.equal(contentCalls().length,2);
`},
		{"session switch aborts and discards stale content", `
 await listFiles();
 const pending=deferFetch(); const opening=c.openSessionFile('1');
 assert.equal(get('sessionFilePanel').hidden,false);
 c.currentSessionId='B'; c.resetSessionFiles();
 assert.equal(pending[0].options.signal.aborted,true);
 pending[0].resolve(response({file:files[0],content:'OLD SECRET'})); await opening;
 assert.equal(get('sessionFilePanel').hidden,true);
 assert.equal(get('sessionFileContent').textContent,'');
 assert.equal(tabButtons().length,0);
 useServer(); await c.loadSessionFiles('B'); await c.openSessionFile('2');
 c.currentSessionId='A'; await c.loadSessionFiles('A'); await c.openSessionFile('2');
 assert.equal(contentCalls().filter(call=>call.id==='2').length,2,'file cache must not cross sessions');
`},
		{"newer preview wins over aborted response", `
 await listFiles();
 const pending=deferFetch();
 const old=c.openSessionFile('1'), latest=c.openSessionFile('2');
 assert.equal(pending[0].options.signal.aborted,true);
 pending[1].resolve(response({file:files[1],content:'NEW FILE'})); await latest;
 pending[0].resolve(response({file:files[0],content:'OLD FILE'})); await old;
 assert.equal(sourceText(),'NEW FILE');
 assert.equal(get('sessionFileTitle').textContent,'same.md');
 c.closeSessionFile(); assert.equal(get('sessionFilePanel').hidden,true);
`},
		{"new session list wins over stale listing", `
 const pending=deferFetch(); const first=c.loadSessionFiles('A');
 c.currentSessionId='B'; const latest=c.loadSessionFiles('B');
 pending[1].resolve(response({files:[files[2]]})); await latest;
 pending[0].resolve(response({files})); await first;
 assert.equal(get('sessionFileCount').textContent,'1');
 assert.equal(c.matchSessionFile('/work/a/same.md'),null);
 assert.equal(c.matchSessionFile('/work/b/same.md').id,'3');
`},
		{"close inactive active and last tabs", `
 await listFiles();
 await c.openSessionFile('1'); await c.openSessionFile('2'); await c.openSessionFile('3');
 c.closeSessionFileTab('1');
 assert.equal(sourceText(),'SECOND FILE');
 assert.equal(tabButtons().length,2);
 selectedTab().focus(); selectedTab().dispatch('keydown',{key:'Delete'}); await tick();
 assert.equal(sourceText(),'FIRST FILE\n');
 assert.equal(tabButtons().length,1);
 assert.equal(c.document.activeElement,selectedTab(),'closing active tab must focus its replacement');
 c.closeSessionFileTab('2');
 assert.equal(tabButtons().length,0);
 assert.equal(get('sessionFileCopy').disabled,true);
 assert.match(get('sessionFileContent').textContent,/Open a session file/);
 assert.equal(c.document.activeElement,get('sessionFileAdd'));
 assert.equal(contentCalls().length,3);
 c.closeSessionFileTab('2');
 assert.equal(tabButtons().length,0,'closing a tab twice is harmless');
`},
		{"refresh reloads bytes and copy uses source", `
 await listFiles(); await c.openSessionFile('2');
 assert.notEqual(get('sessionFileContent').textContent,sources['2'],'fake renderer includes visible line number chrome');
 await c.copySessionFileContent();
 assert.equal(copied[0],'FIRST FILE\n','copy excludes rendered line numbers and preserves the final newline');
 assert.equal(get('sessionFileActionStatus').textContent,'Copied');
 sources['2']='CHANGED\n<literal>&\n'; await c.refreshSessionFile();
 assert.equal(sourceText(),sources['2']);
 assert.equal(contentCalls().length,2);
 assert.equal(tabButtons().length,1);
 await c.copySessionFileContent(); assert.equal(copied[1],sources['2']);
`},
		{"refresh does not reopen after close", `
 await listFiles(); await c.openSessionFile('2');
 let pending=deferFetch(); const refresh=c.refreshSessionFile();
 c.closeSessionFile(); pending[0].resolve(response({files})); await refresh;
 assert.equal(get('sessionFilePanel').hidden,true);
 assert.equal(pending.length,1,'closed panel must not launch content fetch after refreshing the list');
 useServer(); await c.openSessionFile('2');
 pending=deferFetch(); const second=c.refreshSessionFile();
 pending[0].resolve(response({files})); await tick();
 assert.equal(pending.length,2);
 c.closeSessionFile(); assert.equal(pending[1].options.signal.aborted,true);
 pending[1].resolve(response({file:files[1],content:'LATE BYTES'})); await second;
 assert.equal(get('sessionFilePanel').hidden,true);
 assert.equal(get('sessionFileContent').textContent.includes('LATE BYTES'),false);
`},
		{"keyboard tabs maximize and escape", `
 await listFiles(); await c.openSessionFile('2'); await c.openSessionFile('3');
 selectedTab().focus(); selectedTab().dispatch('keydown',{key:'ArrowRight'}); await tick();
 assert.equal(selectedTab().title,'/work/a/same.md');
 assert.equal(c.document.activeElement,selectedTab());
 assert.deepEqual(tabButtons().map(button=>button.tabIndex),[0,-1]);
 selectedTab().dispatch('keydown',{key:'End'}); await tick();
 assert.equal(selectedTab().title,'/work/b/same.md');
 selectedTab().dispatch('keydown',{key:'Home'}); await tick();
 assert.equal(selectedTab().title,'/work/a/same.md');
 selectedTab().dispatch('keydown',{key:'ArrowLeft'}); await tick();
 assert.equal(selectedTab().title,'/work/b/same.md');
 const content=get('sessionFileContent').children[0];
 get('sessionFileContent').scrollTop=143; c.toggleSessionFileMaximize();
 assert.equal(get('sessionFilePanel').classList.contains('is-maximized'),true);
 assert.equal(get('sessionFileMaximize').getAttribute('aria-pressed'),'true');
 assert.equal(get('sessionFileContent').children[0],content,'maximize must not remount content');
 assert.equal(get('sessionFileContent').scrollTop,143);
 c.setSessionFilePicker(true); c.sessionFilePanelKeydown(event('Escape'));
 assert.equal(get('sessionFilePicker').hidden,true);
 assert.equal(get('sessionFilePanel').classList.contains('is-maximized'),true);
 c.sessionFilePanelKeydown(event('Escape'));
 assert.equal(get('sessionFilePanel').classList.contains('is-maximized'),false);
 assert.equal(get('sessionFilePanel').hidden,false);
 c.sessionFilePanelKeydown(event('Escape'));
 assert.equal(get('sessionFilePanel').hidden,true);
`},
		{"pending file picker cannot reopen closed panel", `
 await listFiles(); await c.openSessionFile('2');
 const pending=deferFetch(); const opening=c.openSessionFiles();
 c.closeSessionFile(); pending[0].resolve(response({files})); await opening;
 assert.equal(get('sessionFilePanel').hidden,true);
 assert.equal(get('sessionFilePicker').hidden,true);
`},
		{"keyboard focus stays in tabs while source loads", `
 await listFiles(); const pending=deferFetch();
 const old=c.openSessionFile('2'), latest=c.openSessionFile('3');
 pending[1].resolve(response({file:files[2],content:sources['3']})); await latest;
 selectedTab().focus(); selectedTab().dispatch('keydown',{key:'ArrowLeft'});
 assert.equal(selectedTab().title,'/work/a/same.md');
 assert.equal(c.document.activeElement,selectedTab(),'keyboard focus must not wait for a source fetch');
 pending[2].resolve(response({file:files[1],content:sources['2']})); await tick();
 pending[0].resolve(response({file:files[1],content:'STALE'})); await old;
 assert.equal(sourceText(),sources['2']);
`},
		{"reopen retries an aborted uncached preview", `
 await listFiles(); const pending=deferFetch();
 const opening=c.openSessionFile('2'); c.closeSessionFile();
 pending[0].reject(Object.assign(new Error('aborted'),{name:'AbortError'})); await opening;
 useServer(); await c.openSessionFiles(); await tick();
 assert.equal(get('sessionFilePanel').hidden,false);
 assert.equal(sourceText(),'FIRST FILE\n');
 assert.equal(get('sessionFileContent').classList.contains('is-loading'),false);
`},
		{"close while reopened source loads stays closed", `
 await listFiles(); let pending=deferFetch();
 const first=c.openSessionFile('2'); c.closeSessionFile();
 pending[0].reject(Object.assign(new Error('aborted'),{name:'AbortError'})); await first;
 pending=deferFetch(); const reopening=c.openSessionFiles();
 pending[0].resolve(response({files})); await tick();
 assert.equal(pending.length,2);
 c.closeSessionFile();
 pending[1].resolve(response({file:files[1],content:'LATE REOPEN'})); await reopening;
 assert.equal(get('sessionFilePanel').hidden,true);
 assert.equal(get('sessionFilePicker').hidden,true);
 assert.equal(get('sessionFileContent').textContent.includes('LATE REOPEN'),false);
`},
		{"unavailable neighboring tab cannot retain closed content", `
 await listFiles(); await c.openSessionFile('3'); await c.openSessionFile('2');
 c.fetch=async()=>response({files:[files[1]]}); await c.loadSessionFiles('A');
 c.closeSessionFileTab('2'); await tick();
 assert.equal(get('sessionFileContent').textContent.includes('FIRST FILE'),false);
 assert.equal(get('sessionFileCopy').disabled,true);
 assert.equal(tabButtons().some(button=>button.title==='/work/a/same.md'),false);
`},
		{"refresh loading state does not erase cached scroll", `
 await listFiles(); await c.openSessionFile('2');
 get('sessionFileContent').scrollTop=500;
 const pending=deferFetch(); const refresh=c.refreshSessionFile();
 pending[0].resolve(response({files})); await tick();
 // Browsers clamp scrollTop when the long source is replaced by one loading line.
 get('sessionFileContent').scrollTop=0;
 c.closeSessionFile();
 pending[1].reject(Object.assign(new Error('aborted'),{name:'AbortError'})); await refresh;
 useServer(); await c.openSessionFile('2');
 assert.equal(get('sessionFileContent').scrollTop,500);
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { runSessionFilesUIScript(t, tt.body) })
	}
}

// The controller runs unchanged; rendering is stubbed to isolate tab and request lifetimes.
func runSessionFilesUIScript(t *testing.T, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	source, err := staticFS.ReadFile("static/session_files.js")
	if err != nil {
		t.Fatal(err)
	}
	const harness = `
const assert = require('node:assert/strict'), vm = require('node:vm');
const source = require('node:fs').readFileSync(0,'utf8');
let activeDocument;
class Element {
 constructor(tag='div') {
  this.tagName=tag.toUpperCase(); this.hidden=true; this.dataset={}; this.children=[]; this._text='';
  this.listeners=new Map(); this.attributes=new Map(); this.className=''; this.value=''; this.style={};
  this.scrollTop=0; this.scrollLeft=0; this.tabIndex=0; this.disabled=false; this.parentElement=null;
  const classes=()=>this.className.split(/\s+/).filter(Boolean);
  this.classList={contains:name=>classes().includes(name),add:(...names)=>{this.className=[...new Set([...classes(),...names])].join(' ');},
   remove:(...names)=>{this.className=classes().filter(name=>!names.includes(name)).join(' ');},
   toggle:(name,force)=>{const include=force===undefined?!classes().includes(name):force; include?this.classList.add(name):this.classList.remove(name); return include;}};
 }
 get isConnected(){return !!this.root||!!this.parentElement?.isConnected;}
 get textContent(){return this._text+this.children.map(child=>child.textContent).join('');}
 set textContent(value){this.replaceChildren();this._text=String(value);}
 set innerHTML(value){this.replaceChildren();this._html=String(value);this._text=String(value);}
 get innerHTML(){return this._html||'';}
 contains(node){return this===node||this.children.some(child=>child.contains(node));}
 replaceChildren(...items) {
  for(const child of this.children){if(child.contains(activeDocument?.activeElement))activeDocument.activeElement=activeDocument.body;child.parentElement=null;}
  this.children=[];this._text='';this._html='';this.append(...items);
 }
 append(...items){for(const item of items){item.parentElement=this;this.children.push(item);}}
 setAttribute(name,value){this.attributes.set(name,String(value));}
 getAttribute(name){return this.attributes.get(name)??null;}
 focus(){if(this.isConnected)activeDocument.activeElement=this;}
 scrollIntoView(){this.scrolledIntoView=true;}
 querySelectorAll(selector){
  const attribute=selector.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/);
  const matches=node=>attribute?(node.getAttribute(attribute[1])!==null&&(attribute[2]===undefined||node.getAttribute(attribute[1])===attribute[2])):
   selector.startsWith('.')?node.classList.contains(selector.slice(1)):node.tagName.toLowerCase()===selector;
  const descendants=[];for(const child of this.children){if(matches(child))descendants.push(child);descendants.push(...child.querySelectorAll(selector));}return descendants;
 }
 querySelector(selector){return this.querySelectorAll(selector)[0]||null;}
 addEventListener(name,fn){if(!this.listeners.has(name))this.listeners.set(name,new Set());this.listeners.get(name).add(fn);}
 removeEventListener(name,fn){this.listeners.get(name)?.delete(fn);}
 dispatch(name,props={}){const e={...event(props.key),...props,currentTarget:this,target:this};for(const fn of this.listeners.get(name)||[])fn(e);return e;}
 getBoundingClientRect(){return {width:parseInt(this.style.width)||600};}
 setPointerCapture(id){this.capturedPointer=id;}
}
const event=key=>({key,preventDefault(){this.defaultPrevented=true;},stopPropagation(){this.propagationStopped=true;}});
const els=new Map(); const get=id=>{if(!els.has(id)){const element=new Element();element.root=true;els.set(id,element);}return els.get(id);};
const body=new Element('body');body.root=true;
const copied=[], calls=[];
const c={currentSessionId:'A', AbortController, console,
 document:{getElementById:get,createElement:tag=>new Element(tag),createTextNode:text=>{const node=new Element('#text');node.textContent=text;return node;},
 querySelectorAll:()=>[],querySelector:()=>null,addEventListener(){},activeElement:body,body},
 navigator:{clipboard:{writeText:async text=>{copied.push(text);}}},window:{innerWidth:1400},
 uiText:(en,zh)=>en,formatContent:s=>s.replaceAll('<','&lt;'),
 sessionFileLanguage:file=>({id:file.kind==='markdown'?'markdown':'plaintext',label:file.kind==='markdown'?'Markdown':'Text'}),
 renderSessionFileSource:(text,file,options)=>{const block=new Element('pre'),gutter=new Element('span'),code=new Element('code');
  gutter.textContent='1 2 3';code.className='source-text';code.textContent=text;code.dataset.wrap=String(options.wrap);block.append(gutter,code);return block;},
};
activeDocument=c.document;
vm.createContext(c); vm.runInContext(source,c);
const files=[{id:'1',name:'report.md',path:'/private/tmp/report.md',kind:'markdown'},
 {id:'2',name:'same.md',path:'/work/a/same.md'},{id:'3',name:'same.md',path:'/work/b/same.md'}];
const sources={'1':'# Report\n','2':'FIRST FILE\n','3':'SECOND FILE'};
const response=data=>({ok:true,json:async()=>data});
function useServer(){c.fetch=async(url,options)=>{const query=new URL(url,'http://local.test'),id=query.searchParams.get('id');calls.push({url,id,options});
 return response(id?{file:files.find(file=>file.id===id),content:sources[id]}:{files});};}
async function listFiles(){useServer();await c.loadSessionFiles(c.currentSessionId);}
function deferFetch(){const pending=[];c.fetch=(url,options={})=>new Promise((resolve,reject)=>pending.push({url,options,resolve,reject}));return pending;}
const contentCalls=()=>calls.filter(call=>call.id);
const tabButtons=()=>get('sessionFileTabs').querySelectorAll('[role="tab"]');
const selectedTab=()=>get('sessionFileTabs').querySelector('[aria-selected="true"]');
const sourceText=()=>get('sessionFileContent').querySelector('.source-text')?.textContent;
const tick=()=>new Promise(resolve=>setImmediate(resolve));
`
	script := harness + "\nconst watchdog=setTimeout(()=>{console.error('UI scenario did not settle');process.exit(1);},5000);\n" +
		"(async()=>{\n" + body + "\n})().catch(e=>{console.error(e);process.exitCode=1;}).finally(()=>clearTimeout(watchdog));\n"
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session file UI: %v\n%s", err, out)
	}
}
