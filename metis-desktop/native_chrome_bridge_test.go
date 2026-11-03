package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeChromeBridgeScopesGesturesAndTracksFullscreen(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", `
const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const source=fs.readFileSync('frontend/src/main.js','utf8').replace(/import\s*\{[\s\S]*?\}\s*from\s*[^;]+;/g,'');
async function fixture(platform='darwin', failStart=false) {
 const listeners={},frameListeners={},bootListeners={},posted=[],invokes=[],zoom=[],intervals=new Map();
 let full=false,nextTimer=0;
 const frame={style:{},contentWindow:{postMessage(message,origin){posted.push({message,origin})}},addEventListener(name,fn){frameListeners[name]=fn}};
 const boot={style:{},addEventListener(name,fn){bootListeners[name]=fn}},status={};
 const elements={'metis-frame':frame,boot,'boot-status':status},root={dataset:{}};
 const win={runtime:{Environment:async()=>({platform}),WindowIsFullscreen:async()=>full,WindowToggleMaximise(){zoom.push(true)}},
  WailsInvoke(value){invokes.push(value)},addEventListener(name,fn){(listeners[name]??=[]).push(fn)},
  setInterval(fn){const id=++nextTimer;intervals.set(id,fn);return id},clearInterval(id){intervals.delete(id)}};
 const c=vm.createContext({URL,document:{documentElement:root,getElementById(id){return elements[id]}},window:win,
  StartWebUI:async()=>{if(failStart)throw new Error('fixture startup failure');return 'http://127.0.0.1:1234/?desktop-frame=owned'},
  SetNativeTheme:async()=>{},ChooseWorkspaceDirectory(){},GetUpdateProgress(){},GetUpdateStatus(){},InstallUpdateAndRestart(){},StartInstallUpdateAndRestart(){}});
 vm.runInContext(source,c);await listeners.load[0]();
 async function request(action,payload={},overrides={}) {
  const event={source:frame.contentWindow,origin:'http://127.0.0.1:1234',data:{channel:'metis-native',kind:'request',id:'fixture',action,payload},...overrides};
  if(listeners.message)await listeners.message[0](event);
  return posted.at(-1)?.message;
 }
 const flush=async()=>{for(let i=0;i<8;i++)await Promise.resolve()};
 return {root,listeners,frameListeners,bootListeners,posted,invokes,zoom,intervals,status,request,flush,setFull(value){full=value}};
}
(async()=>{
 const mac=await fixture();
 assert.equal(mac.root.dataset.nativeChrome,'macos');
 let response=await mac.request('window-chrome');
 assert.equal(response.kind,'response');assert.equal(response.value.platform,'darwin');assert.equal(response.value.overlay,true);
 assert.equal(response.value.titlebarHeight,48);assert.equal(response.value.trafficLightInset,96);assert.equal(response.value.fullscreen,false);
 assert.equal(mac.posted.at(-1).origin,'http://127.0.0.1:1234');
 const before=mac.posted.length;
 await mac.request('window-drag',{button:0,buttons:1,detail:1},{source:{}});
 await mac.request('window-drag',{button:0,buttons:1,detail:1},{origin:'http://127.0.0.1:9999'});
 await mac.request('window-drag',{button:0,buttons:1,detail:1},{data:{channel:'other',kind:'request',action:'window-drag'}});
 await mac.request('invoke',{value:'Quit'});await mac.request('constructor');await mac.request('toString');
 assert.equal(mac.invokes.length,0,'wrong source/origin/channel or unknown action cannot invoke native code');
 assert.equal(mac.posted.length,before,'untrusted requests receive no response');
 for(const payload of [{},{button:1,buttons:2,detail:1},{button:0,buttons:0,detail:1},{button:0,buttons:1,detail:2}]){
  response=await mac.request('window-drag',payload);assert.ok(response.error);
 }
 assert.equal(mac.invokes.length,0,'invalid mouse state cannot drag');
 response=await mac.request('window-drag',{button:0,buttons:1,detail:1,invoke:'Quit'});
 assert.equal(response.value,true);assert.deepEqual(mac.invokes,['drag'],'only the fixed drag protocol is exposed');
 response=await mac.request('window-toggle-maximise',{button:0,detail:1});assert.ok(response.error);
 response=await mac.request('window-toggle-maximise',{button:0,detail:2});assert.equal(response.value,true);assert.equal(mac.zoom.length,1);
 mac.frameListeners.load();await mac.flush();
 assert.equal(mac.posted.at(-1).message.kind,'window-chrome');
 assert.equal(mac.posted.at(-1).message.value.trafficLightInset,96);
 mac.setFull(true);for(const tick of mac.intervals.values())tick();await mac.flush();
 assert.equal(mac.posted.at(-1).message.kind,'window-chrome');assert.equal(mac.posted.at(-1).message.value.fullscreen,true);
 assert.equal(mac.posted.at(-1).message.value.trafficLightInset,0,'fullscreen removes the traffic-light inset');
 const settled=mac.posted.length;for(const tick of mac.intervals.values())tick();await mac.flush();
 assert.equal(mac.posted.length,settled,'unchanged native state is not broadcast repeatedly');
 response=await mac.request('window-drag',{button:0,buttons:1,detail:1});assert.ok(response.error);
 response=await mac.request('window-toggle-maximise',{button:0,detail:2});assert.ok(response.error);
 assert.deepEqual(mac.invokes,['drag']);assert.equal(mac.zoom.length,1,'fullscreen cannot accidentally zoom');
 mac.setFull(false);for(const focus of mac.listeners.focus)focus();await mac.flush();
 assert.equal(mac.posted.at(-1).message.value.trafficLightInset,96,'leaving fullscreen restores the inset');
 for(const cleanup of mac.listeners.beforeunload)cleanup();assert.equal(mac.intervals.size,0);
 for(const platform of ['windows','linux']){
  const other=await fixture(platform);response=await other.request('window-chrome');
  assert.equal(response.value.overlay,false);assert.equal(response.value.trafficLightInset,0);assert.equal(response.value.titlebarHeight,0);
  await other.request('window-drag',{button:0,buttons:1,detail:1});await other.request('window-toggle-maximise',{button:0,detail:2});
  assert.equal(other.invokes.length,0);assert.equal(other.zoom.length,0);assert.equal(other.intervals.size,0);
  assert.equal(other.root.dataset.nativeChrome,undefined,'other platforms retain their native framed startup layout');
 }
 const failed=await fixture('darwin',true);assert.match(failed.status.textContent,/fixture startup failure/);
 failed.bootListeners.dblclick({button:0,detail:2,target:{closest(){return null}}});await failed.flush();
 assert.equal(failed.zoom.length,1,'startup/error chrome retains double-click zoom');
 failed.bootListeners.dblclick({button:0,detail:2,target:{closest(){return {tagName:'BUTTON'}}}});await failed.flush();
 assert.equal(failed.zoom.length,1,'startup controls do not trigger zoom');
})().catch(error=>{console.error(error);process.exitCode=1});
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native chrome bridge: %v\n%s", err, out)
	}
}

func TestDesktopStartupChromeRemainsDraggableWithoutCoveringFrame(t *testing.T) {
	body, err := os.ReadFile("frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `html[data-native-chrome="macos"] #boot { --wails-draggable: drag;`) ||
		!strings.Contains(text, `html[data-native-chrome="macos"] #boot input { --wails-draggable: no-drag; }`) {
		t.Fatal("startup/error page must retain a drag surface while excluding controls")
	}
	if strings.Contains(text, "position: fixed") || strings.Contains(text, "z-index:") {
		t.Fatal("native chrome must not install an overlay that intercepts iframe controls")
	}
}
