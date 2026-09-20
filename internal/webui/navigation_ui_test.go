package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestNavigationPagesShareMainWindow(t *testing.T) {
	content, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	start, end := strings.Index(source, "<main "), strings.Index(source, "</main>")
	if start < 0 || end < start {
		t.Fatal("main page missing")
	}
	for _, id := range []string{"navigationHeader", "settingsOverlay", "automationsPage", "artifactPreviewOverlay", "chatArea", "inputField"} {
		if !strings.Contains(source[start:end], `id="`+id+`"`) {
			t.Fatalf("%s must stay inside the shared main window", id)
		}
	}
	for _, asset := range []string{"navigation.js", "navigation.css", "automations.js", "automations.css", "session_status.css"} {
		if !strings.Contains(source, asset+"?v=__BUILD__") {
			t.Fatalf("missing navigation asset %s", asset)
		}
	}
	if strings.Contains(source, `class="settings-overlay"`) {
		t.Fatal("settings still uses a modal overlay")
	}
	if !strings.Contains(source[end:], `id="artifactDeleteOverlay" role="alertdialog" aria-modal="true"`) {
		t.Fatal("artifact deletion must remain a confirmation dialog")
	}
}

func TestNavigationHeaderStaysAboveEmptyConversation(t *testing.T) {
	css, err := staticFS.ReadFile("static/navigation.css")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	// Empty conversations center their hero/composer with justify-content.
	// Navigation must be removed from that centered flow, using the actual
	// class updateEmptyLayout applies (rather than an unused welcome selector).
	if !strings.Contains(string(chat), "main.classList.toggle('empty', !hasContent)") {
		t.Fatal("empty conversation class changed; review navigation header positioning")
	}
	selector := ".main[data-page='session'].empty > .navigation-header"
	_, rule, ok := strings.Cut(string(css), selector)
	if !ok {
		t.Fatal("navigation lacks an override for the centered empty conversation")
	}
	rule, _, _ = strings.Cut(rule, "}")
	for _, declaration := range []string{"position: absolute", "top: 0", "left: 0", "right: 0", "height: 44px"} {
		if !strings.Contains(rule, declaration) {
			t.Fatalf("empty conversation navigation rule missing %q", declaration)
		}
	}
}

func TestNavigationHistoryAndRetainedConversation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"navigation.js", "chat.js", "artifacts.js"} {
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
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve; const promise = new Promise(r => resolve = r); return {promise,resolve}; };
class Element {
 constructor() { this.dataset={}; this.hidden=false; this.disabled=false; this.value=''; this.textContent=''; this.attrs={}; this.scrollTop=0; this.classes=new Set(); this.classList={toggle:(key,on)=>on?this.classes.add(key):this.classes.delete(key),contains:key=>this.classes.has(key),add:(...keys)=>keys.forEach(key=>this.classes.add(key)),remove:(...keys)=>keys.forEach(key=>this.classes.delete(key))}; }
 setAttribute(key,value) { this.attrs[key]=value; }
 removeAttribute(key) { delete this.attrs[key]; }
 querySelector() { return this.label ||= new Element(); }
 getClientRects() { return this.hidden?[]:[{}]; }
}
function setup() {
 const elements=new Map(), get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
 const listeners=new Map(), app=new Element(), main=new Element(), sidebar=new Element();
 main.classes.add('trace-mode');
 const tabs=['general','appearance','providers','presets','plugins','computer-use','routing','config'].map(tab=>{const item=new Element();item.dataset.settingsTab=tab;return item;});
 const modal=new Element();modal.hidden=true;
 const c={console,Promise,Map,Set,setTimeout,clearTimeout,currentSessionId:'A',currentView:'trace',settingsTab:'general',settingsCache:{settings:[]},
  pendingSessionId:null,selectionGeneration:0,artifactState:{active:null,previewSequence:0},sessions:[{id:'A',title:'Session A'}],
  document:{documentElement:{lang:'zh-CN'},getElementById:get,
   querySelector:s=>s==='.app'?app:s==='.main'?main:s==='.main.artifacts-mode'&&main.classes.has('artifacts-mode')?main:s==='.sb-settings'?sidebar:null,
   querySelectorAll:s=>s==='.settings-item'?tabs:s==='[aria-modal="true"]'?[modal]:[],
   addEventListener:(type,fn)=>{if(!listeners.has(type))listeners.set(type,[]);listeners.get(type).push(fn);}},
  renderSettingsTab(){get('settingsContent').textContent=c.settingsTab;},applyTheme(){},currentThemeSetting(){return 'dark';},showToast(){},
  invalidateSessionAsyncLoads(){c.selectionGeneration++;c.pendingSessionId=null;},
  closeArtifactPreview(){c.artifactState.previewSequence++;c.artifactState.active=null;get('artifactPreviewOverlay').hidden=true;},
  async resumeSession(id){if(id==='missing')return false;const generation=++c.selectionGeneration;c.currentSessionId=id;c.metisNavigation.recordSession(id);await tick();return generation===c.selectionGeneration;},
  switchView(view){c.currentView=view;main.classList.toggle('trace-mode',view==='trace');main.classList.toggle('artifacts-mode',view==='artifacts');get('tabChat').classList.toggle('active',view==='chat');c.metisNavigation.recordView(view);},
  openArtifactsPanel(){c.currentView='artifacts';main.classList.add('artifacts-mode');main.classList.remove('trace-mode');get('tabChat').classList.remove('active');c.metisNavigation.recordView('artifacts');},
  renderArtifactGallery(){},updateArtifactTabCount(){},
  async previewArtifactByID(id,version){c.artifactState.active={id};c.metisNavigation.recordArtifact(id,version);},
  newChat(){c.currentSessionId=null;c.metisNavigation.recordSession(null);},
  showAutomationsPage(){c.scheduleEnters++;},hideAutomationsPage(){c.scheduleLeaves++;},scheduleEnters:0,scheduleLeaves:0,
 };
 c.window=c;
 vm.createContext(c);
 const chat=sources['chat.js'],start=chat.indexOf('async function openSettings('),end=chat.indexOf('function filterSettings',start);
 assert(start>=0&&end>start);vm.runInContext(chat.slice(start,end),c);
 const artifacts=sources['artifacts.js'];
 for(const [start,next] of [['function resetArtifactsForSession()', 'function openArtifactsPanel()'],['function leaveArtifactsPanel()', 'async function fetchArtifactDetail']]) {
  const from=artifacts.indexOf(start),to=artifacts.indexOf(next,from);assert(from>=0&&to>from);vm.runInContext(artifacts.slice(from,to),c);
 }
 vm.runInContext(sources['navigation.js'],c);
 listeners.get('DOMContentLoaded').forEach(fn=>fn());
 const dispatch=(type,props)=>{const e={defaultPrevented:false,preventDefault(){this.defaultPrevented=true;},...props};(listeners.get(type)||[]).forEach(fn=>fn(e));return e;};
 return {c,get,app,main,tabs,modal,dispatch};
}
(async()=>{
 // Real settings functions route subpages and return to the original view,
 // retaining the exact composer/transcript nodes while a stream continues.
 {
  const {c,get,app,tabs}=setup(),nav=c.metisNavigation,composer=get('inputField'),transcript=get('chatArea');
  composer.value='unfinished draft';transcript.textContent='stream';
  assert.equal(get('navigationBack').disabled,true);assert.equal(get('navigationForward').disabled,true);
  await c.openSettings();assert.equal(app.dataset.page,'settings');
  await c.showSettingsTab('plugins');assert.equal(c.settingsTab,'plugins');assert(tabs.find(t=>t.dataset.settingsTab==='plugins').classes.has('active'));
  await c.showSettingsTab('routing');transcript.textContent+=' still running';
  await nav.back();assert.equal(c.settingsTab,'plugins');assert.equal(get('navigationForward').disabled,false);
  await nav.forward();assert.equal(c.settingsTab,'routing');
  c.closeSettings();await tick();assert.equal(app.dataset.page,'session');assert.equal(nav.current().view,'trace');
  assert.equal(get('inputField'),composer);assert.equal(composer.value,'unfinished draft');
  assert.equal(get('chatArea'),transcript);assert.equal(transcript.textContent,'stream still running');
  await nav.forward();assert.equal(c.settingsTab,'general','Back to app preserves the full forward stack');
  await c.showSettingsTab('appearance');assert.equal(await nav.forward(),false,'new branch must discard former forward entries');
  const count=nav.snapshot().entries.length;await c.showSettingsTab('appearance');assert.equal(nav.snapshot().entries.length,count,'same page duplicated history');
 }
 // Native reproduction: gallery -> preview -> version Back/Forward -> a
 // different sidebar session. The real artifact reset clears the visible
 // gallery but used to leave currentView='artifacts' in the navigation title.
 {
  const {c,get,main}=setup(),nav=c.metisNavigation;
  c.switchView('chat');c.openArtifactsPanel();
  nav.recordArtifact('alpha-artifact',2);nav.recordArtifact('alpha-artifact',1);
  await nav.back();await nav.forward();
  c.currentSessionId='B';c.resetArtifactsForSession();nav.recordSession('B');
  assert.equal(nav.current().sessionId,'B');assert.equal(nav.current().view,'chat');
  assert.equal(c.currentView,'chat');assert.equal(main.classes.has('artifacts-mode'),false);
  assert(get('tabChat').classes.has('active'));assert(!get('navigationTitle').textContent.includes('产物'));
  await nav.back();assert.equal(nav.current().page,'artifact');assert.equal(nav.current().sessionId,'A');
 }
 // Failed selection does not consume history; a validated selection does not
 // invalidate its own still-running artifact/effort/status restoration.
 {
  const {c,app}=setup(),nav=c.metisNavigation;
  await c.openSettings('plugins');const before=nav.snapshot().index;
  assert.equal(await nav.navigate({page:'session',sessionId:'missing'}),false);
  assert.equal(nav.snapshot().index,before);assert.equal(app.dataset.page,'settings');
  assert.equal(await nav.navigate({page:'session',sessionId:'B',view:'chat'}),true);
  assert.equal(nav.current().sessionId,'B');
  const generation=c.selectionGeneration;c.currentSessionId='C';nav.recordSession('C');
  assert.equal(c.selectionGeneration,generation,'recordSession cancelled its own completed selection');
 }
 // A late target cannot override a newer page or append a hidden history entry.
 {
  const {c,app}=setup(),nav=c.metisNavigation,slow=deferred();
  c.resumeSession=async id=>{const generation=++c.selectionGeneration;c.pendingSessionId=id;await slow.promise;if(generation!==c.selectionGeneration)return false;c.currentSessionId=id;nav.recordSession(id);return true;};
  const pending=nav.navigate({page:'session',sessionId:'slow',view:'chat'});
  assert.equal(c.document.getElementById('navigationBack').disabled,true);
  await c.openSettings('plugins');slow.resolve();assert.equal(await pending,false);
  assert.equal(app.dataset.page,'settings');assert.equal(nav.current().tab,'plugins');assert.equal(c.currentSessionId,'A');
  assert(!nav.snapshot().entries.some(route=>route.sessionId==='slow'));
 }
 // If an artifact disappears after activating its other session, display the
 // session that actually committed instead of retaining the old A title.
 {
  const {c,app}=setup(),nav=c.metisNavigation;
  c.previewArtifactByID=async()=>{};
  assert.equal(await nav.navigate({page:'artifact',sessionId:'B',artifactId:'deleted',version:1}),false);
  assert.equal(c.currentSessionId,'B');assert.equal(nav.current().sessionId,'B');assert.equal(app.dataset.page,'session');
 }
 // Artifact requests waiting for their first detail response are invalidated
 // even when there is no active artifact yet. Returning to an artifact works.
 {
  const {c,app}=setup(),nav=c.metisNavigation;
  const sequence=++c.artifactState.previewSequence;
  await c.openSettings();assert(c.artifactState.previewSequence>sequence);
  await nav.back();nav.recordArtifact('artifact-1',1);nav.recordArtifact('artifact-1',2);assert.equal(app.dataset.page,'artifact');
  assert.equal(nav.closeArtifact(),true);await tick();assert.equal(app.dataset.page,'session');
  await nav.forward();assert.equal(app.dataset.page,'artifact');assert.equal(nav.current().artifactId,'artifact-1');assert.equal(nav.current().version,1);
 }
 // Restoring an artifact shows its page while its URL is loading, rather
 // than unhiding its shell alongside the old conversation.
 {
  const {c,app}=setup(),nav=c.metisNavigation,preview=deferred();
  c.previewArtifactByID=async(id,version)=>{c.artifactState.active={id};nav.recordArtifact(id,version);await preview.promise;};
  const restored=nav.navigate({page:'artifact',sessionId:'A',artifactId:'slow-preview',version:1});
  assert.equal(app.dataset.page,'artifact');preview.resolve();assert.equal(await restored,true);
 }
 // Initial turn session assignment cannot take focus from settings or turn
 // the retained blank route into a destructive newChat on Back.
 {
  const {c,app}=setup(),nav=c.metisNavigation;
  c.currentSessionId=null;nav.recordSession(null);
  await c.openSettings('plugins');c.currentSessionId='created';nav.recordSession('created',{replace:true});
  assert.equal(app.dataset.page,'settings');await nav.back();assert.equal(c.currentSessionId,'created');assert.equal(nav.current().sessionId,'created');
 }
 // Shortcut navigation does not use Escape, bare cursor keys or shortcuts
 // while a confirmation is open. Scheduled-page lifecycle runs once.
 {
  const {c,app,dispatch,modal}=setup(),nav=c.metisNavigation;
  await nav.navigate({page:'schedules'});assert.equal(c.scheduleEnters,1);
  const cursor=dispatch('keydown',{key:'ArrowLeft'});assert.equal(cursor.defaultPrevented,false);
  const escape=dispatch('keydown',{key:'Escape'});assert.equal(escape.defaultPrevented,false);assert.equal(app.dataset.page,'schedules');
  modal.hidden=false;assert.equal(dispatch('keydown',{key:'ArrowLeft',altKey:true}).defaultPrevented,false);assert.equal(app.dataset.page,'schedules');modal.hidden=true;
  assert.equal(dispatch('keydown',{key:'[',metaKey:true}).defaultPrevented,true);await tick();assert.equal(app.dataset.page,'session');assert.equal(c.scheduleLeaves,1);
  dispatch('mouseup',{button:4});await tick();assert.equal(app.dataset.page,'schedules');assert.equal(c.scheduleEnters,2);
  dispatch('keydown',{key:'ArrowLeft',altKey:true});await tick();assert.equal(app.dataset.page,'session');
 }
 // Registration and failures use the same transaction rules for future pages.
 {
  const {c}=setup(),events=[],nav=c.createMetisNavigation({initialRoute:{page:'one'},changed:(route,state)=>events.push([route.page,state.canBack,state.canForward])});
  nav.register('one',{enter:()=>true});nav.register('two',{enter:()=>{throw Error('offline');}});
  assert.equal(await nav.navigate({page:'two'}),false);assert.equal(nav.current().page,'one');assert.equal(nav.snapshot().entries.length,1);
 }
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("navigation history: %v\n%s", err, out)
	}
}
