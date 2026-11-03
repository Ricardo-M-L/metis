package webui

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeWindowChromeHandshakeAndGestures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", nativeWindowChromeHarness)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native window chrome browser harness: %v\n%s", err, out)
	}
}

func TestNativeWindowChromeKeepsBrowserShellAndAccessibleControls(t *testing.T) {
	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	css, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	page, style := string(html), string(css)
	if !strings.Contains(page, `class="sb-brand-row"`) || !strings.Contains(style, `.sb-native-chrome { display: none; }`) {
		t.Fatal("browser shell must retain its brand row and hide native-only controls")
	}
	_, nativeButton, ok := strings.Cut(page, `id="nativeSidebarCollapseBtn"`)
	if !ok {
		t.Fatal("native sidebar control missing")
	}
	nativeButton, _, _ = strings.Cut(nativeButton, ">")
	for _, attr := range []string{`aria-label=`, `aria-expanded=`, `aria-controls="sessionList"`, `onclick="toggleSidebar()"`} {
		if !strings.Contains(nativeButton, attr) {
			t.Fatalf("native sidebar control missing %s", attr)
		}
	}
	for _, selector := range []string{
		`.native-mac-chrome .sb-brand-row { display: none; }`,
		`.native-mac-chrome .sb-native-chrome .sb-collapse-btn:focus-visible`,
		`.native-mac-chrome .app.sb-rail`,
		`.native-mac-chrome .app[data-page='settings']`,
		`.native-mac-chrome .main[data-page='session'].empty > .navigation-header`,
		`.native-mac-chrome .session-file-panel.is-maximized .session-file-header`,
	} {
		if !strings.Contains(style, selector) {
			t.Fatalf("missing native chrome layout guard %s", selector)
		}
	}
}

const nativeWindowChromeHarness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const tick = () => new Promise(resolve => setImmediate(resolve));
class Element {
  constructor(matches = [], parent = null) {
    this.matches = matches; this.parent = parent; this.attrs = {}; this.lang = 'en'; this.clientWidth = 1300;
    const classes = new Set();
    this.classList = { contains: key => classes.has(key), add: key => classes.add(key), remove: key => classes.delete(key), toggle: (key, on) => on ? classes.add(key) : classes.delete(key) };
    this.props = {};
    this.style = {setProperty: (key,value) => this.props[key] = value, removeProperty: key => delete this.props[key]};
  }
  setAttribute(key, value) { this.attrs[key] = value; }
  closest(selector) {
    const candidates = selector.split(',').map(value => value.trim());
    for (let node = this; node; node = node.parent) if (candidates.some(value => node.matches.includes(value))) return node;
    return null;
  }
}
function setup(browser = false) {
  const docListeners = new Map(), windowListeners = new Map(), requests = [], timers = new Map();
  const root = new Element(), app = new Element();
  const buttons = {sidebarCollapseBtn: new Element(['button']), nativeSidebarCollapseBtn: new Element(['button'])};
  const listen = (map,type,fn) => { const list = map.get(type) || []; list.push(fn); map.set(type,list); };
  const win = {addEventListener: (type,fn) => listen(windowListeners,type,fn)};
  const parent = {postMessage: (message,origin) => requests.push({message,origin})};
  win.parent = browser ? win : parent;
  const c = {window:win, console, navigator:{userAgent:'Macintosh'},
    document:{documentElement:root, getElementById:id=>buttons[id] || null, querySelector:selector=>selector === '.app' ? app : null,
      addEventListener:(type,fn)=>listen(docListeners,type,fn)},
    setInterval(){return 0;}, clearInterval(){},
    setTimeout(fn){const id=timers.size+1;timers.set(id,fn);return id;},clearTimeout:id=>timers.delete(id),
    localStorage:{getItem(){return null;},setItem(){}},requestAnimationFrame:fn=>fn()};
  vm.createContext(c); vm.runInContext(source,c);
  const emit = (map,type,event) => (map.get(type)||[]).forEach(fn=>fn(event));
  const message = (data, origin='wails://wails', sender=parent) => emit(windowListeners,'message',{data,origin,source:sender});
  const state = (value,origin,sender) => message({channel:'metis-native',kind:'window-chrome',value},origin,sender);
  const reply = (request,value,origin,sender) => message({channel:'metis-native',kind:'response',id:request.message.id,value},origin,sender);
  const event = (type,target,changes={}) => {const e={type,target,button:0,buttons:1,detail:type==='dblclick'?2:1,prevented:false,preventDefault(){this.prevented=true;},...changes};emit(docListeners,type,e);return e;};
  c.initNativeWindowChrome();
  return {c,root,app,buttons,requests,timers,parent,message,state,reply,event,win,windowListeners,emit};
}
const mac = {platform:'darwin',overlay:true,fullscreen:false,titlebarHeight:48,trafficLightInset:96};
const enabled = h => h.root.classList.contains('native-mac-chrome');
(async()=>{
  // A browser on a Mac is not a native host; neither UA nor an unsolicited
  // postMessage may change its layout.
  const browser = setup(true);
  assert.equal(browser.requests.length,0); assert.equal(enabled(browser),false);
  browser.state(mac); assert.equal(enabled(browser),false);
  const h = setup();
  assert.equal(h.requests.length,1); assert.equal(h.requests[0].message.action,'window-chrome');
  h.state(mac); assert.equal(enabled(h),false,'push cannot precede handshake');
  h.reply(h.requests[0],mac,'wails://wails',{}); await tick();
  assert.equal(enabled(h),false,'a sibling frame cannot satisfy the handshake');
  h.reply(h.requests[0],mac); await tick();
  assert.equal(enabled(h),true); assert.equal(h.root.props['--native-titlebar-height'],'48px');
  assert.equal(h.root.props['--native-traffic-light-inset'],'96px');
  h.state({...mac,fullscreen:true},'https://unrelated.example');
  assert.equal(h.root.props['--native-traffic-light-inset'],'96px','pinned parent origin must not drift');
  h.state({...mac,fullscreen:true},'wails://wails',{});
  assert.equal(h.root.props['--native-traffic-light-inset'],'96px','nested content cannot change chrome');

  const header = new Element(['[data-native-window-drag]']);
  const title = new Element([],header);
  const initial = h.requests.length;
  h.event('mousedown',title); assert.equal(h.requests.length,initial,'a click is not a native drag');
  h.event('mouseup',title); h.event('mousemove',title);
  assert.equal(h.requests.length,initial,'release must cancel the pending drag');
  h.event('mousedown',title); h.event('mousemove',title);
  assert.equal(h.requests.length,initial+1);
  assert.equal(h.requests.at(-1).message.action,'window-drag');
  assert.deepEqual(JSON.parse(JSON.stringify(h.requests.at(-1).message.payload)),{button:0,buttons:1,detail:1});
  assert.equal(h.requests.at(-1).origin,'wails://wails');
  h.event('mousemove',title); assert.equal(h.requests.length,initial+1,'one drag request per gesture');
  h.event('dblclick',title);
  assert.equal(h.requests.at(-1).message.action,'window-toggle-maximise');
  assert.deepEqual(JSON.parse(JSON.stringify(h.requests.at(-1).message.payload)),{button:0,detail:2});

  // Interactive descendants keep default click/focus behavior. This includes
  // SVG paths inside buttons, existing onclick tabs and resize handles.
  const beforeControls = h.requests.length;
  for (const selector of ['button','a','input','select','textarea','summary','[contenteditable]','[role="tab"]','[role="button"]','[role="menuitem"]','[tabindex]','[onclick]','.col-handle','.sf-resize']) {
    const child = new Element([],new Element([selector],header));
    assert.equal(h.event('mousedown',child).prevented,false,selector+' mousedown');
    h.event('mousemove',child);
    assert.equal(h.event('dblclick',child).prevented,false,selector+' dblclick');
  }
  h.event('mousedown',title,{button:2,buttons:2}); h.event('mousemove',title,{buttons:2});
  h.event('mousedown',new Element()); h.event('mousemove',title);
  h.event('mousedown',title); h.event('mousemove',title,{buttons:0});
  h.event('mousedown',title); h.emit(h.windowListeners,'blur',{}); h.event('mousemove',title);
  assert.equal(h.requests.length,beforeControls,'only a held left button starting on header background can drag');
  h.state({...mac,fullscreen:true});
  assert.equal(h.root.props['--native-traffic-light-inset'],'0px');
  h.event('mousedown',title); h.event('mousemove',title); h.event('dblclick',title);
  assert.equal(h.requests.length,beforeControls,'fullscreen uses native fullscreen behavior');
  h.state(mac); assert.equal(h.root.props['--native-traffic-light-inset'],'96px');

  // Both controls reflect the same persisted/auto-collapse state and locale.
  h.c.applyLayout();
  assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-expanded'],'true');
  assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-label'],'Collapse sidebar');
  h.c.toggleSidebar(); assert.equal(h.app.classList.contains('sb-rail'),true);
  assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-expanded'],'false');
  assert.equal(h.buttons.sidebarCollapseBtn.attrs['aria-expanded'],'false');
  h.root.lang='zh-CN'; h.c.toggleSidebar();
  assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-label'],'收起侧栏');
  assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-expanded'],'true');
  h.app.clientWidth=800; h.c.applyLayout();
  assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-expanded'],'false');
  h.c.toggleSidebar(); assert.equal(h.buttons.nativeSidebarCollapseBtn.attrs['aria-expanded'],'true');

  for (const value of [{platform:'windows',overlay:false},{platform:'linux',overlay:false},{...mac,overlay:false}]) {
    h.state(value); assert.equal(enabled(h),false);
    assert.equal(h.root.props['--native-traffic-light-inset'],undefined);
  }
  const opaque = setup(); opaque.reply(opaque.requests[0],mac,'null'); await tick();
  opaque.event('dblclick',header);
  assert.equal(opaque.requests.at(-1).origin,'*','opaque origin requires wildcard target but still pins incoming source/origin');
  opaque.state({...mac,fullscreen:true},'https://unrelated.example');
  assert.equal(opaque.root.props['--native-traffic-light-inset'],'96px');
  const timedOut = setup();
  [...timedOut.timers.values()][0](); await tick();
  timedOut.reply(timedOut.requests[0],mac); await tick();
  assert.equal(enabled(timedOut),false,'late unsupported-host replies cannot activate layout');
})().catch(error=>{console.error(error);process.exitCode=1;});
`
