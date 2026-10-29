package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThemeKeepsSystemPreferenceAcrossNativeBridgeAndOSChanges(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", themeFollowBrowserScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("theme browser behavior: %v\n%s", err, out)
	}
}

func TestInitialPageCarriesThemeBeforeStylesLoad(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	s, _ := testServer(t)
	for _, theme := range []string{"auto", "light", "dark", "nord"} {
		if err := os.WriteFile(filepath.Join(os.Getenv("METIS_HOME"), "config.toml"), []byte(fmt.Sprintf("[ui]\ntheme = %q\n", theme)), 0o600); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		body := rr.Body.String()
		if rr.Code != http.StatusOK || !strings.Contains(body, `data-theme-preference="`+theme+`"`) {
			t.Fatalf("initial %s theme was not bootstrapped: status=%d", theme, rr.Code)
		}
		boot, styles := strings.Index(body, `src="theme_boot.js?v=`), strings.Index(body, `href="style.css?v=`)
		if boot < 0 || boot > styles {
			t.Fatalf("theme bootstrap does not precede styles: boot=%d styles=%d", boot, styles)
		}
	}
}

func TestThemeSaveConfirmationUsesUserLanguageAndKeepsFailureState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", `
const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const source=fs.readFileSync('static/chat.js','utf8');
const start=source.indexOf('async function saveSettingValue('),end=source.indexOf('async function chooseEnum(',start);
assert.ok(start>=0&&end>start);
function fixture(zh,ok=true){
 const setting={value:'dark'},toasts=[],applied=[];
 const c=vm.createContext({
  fetch:async()=>({ok,status:ok?200:403,json:async()=>ok?{liveApplied:['ui.theme']}:{error:'denied'}}),
  settingByKey:()=>setting,applyTheme:value=>applied.push(value),
  uiText:(en,cn)=>zh?cn:en,showToast:value=>toasts.push(value),renderSettingsTab(){},
 });
 vm.runInContext(source.slice(start,end),c);
 return {c,setting,toasts,applied};
}
(async()=>{
 for(const [zh,want] of [[true,'主题已保存'],[false,'Theme saved']]){
  const f=fixture(zh);await f.c.saveSettingValue('ui.theme','auto');
  assert.equal(f.setting.value,'auto');assert.deepEqual(f.applied,['auto']);assert.deepEqual(f.toasts,[want]);
 }
 const other=fixture(true);await other.c.saveSettingValue('session.max_iterations',12);
 assert.equal(other.toasts[0],'Saved: session.max_iterations (applies after restart)');
 const failed=fixture(true,false);await failed.c.saveSettingValue('ui.theme','auto');
 assert.equal(failed.setting.value,'dark');assert.deepEqual(failed.applied,[]);assert.deepEqual(failed.toasts,['Save failed: denied']);
})().catch(error=>{console.error(error);process.exitCode=1});
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("theme save confirmation: %v\n%s", err, out)
	}
}

const themeFollowBrowserScript = `
const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const source=fs.readFileSync('static/chat.js','utf8');
const start=source.indexOf('function settingByKey('),end=source.indexOf('function renderSettingsTab(',start);
assert.ok(start>=0&&end>start);
function fixture(preference='auto',light=false,requestError=false){
 const root={dataset:{themePreference:preference},style:{}},calls=[],listeners=[];
 const media={matches:light,addEventListener(type,fn){assert.equal(type,'change');listeners.push(fn)}};
 const c=vm.createContext({document:{documentElement:root},window:{matchMedia(query){assert.equal(query,'(prefers-color-scheme: light)');return media}},
  requestNative(action,payload){calls.push({action,payload:JSON.parse(JSON.stringify(payload))});return Promise.resolve()},
  fetch:async()=>{if(requestError)throw Error('offline');return {ok:true,json:async()=>({settings:[{key:'ui.theme',value:preference}]})}}});
 vm.runInContext('let settingsCache=null;let themeMedia=null;'+source.slice(start,end),c);
 return {c,root,calls,listeners,media,select(pref){vm.runInContext('settingsCache={settings:[{key:"ui.theme",value:'+JSON.stringify(pref)+'}]};applyTheme('+JSON.stringify(pref)+')',c)}};
}
(async()=>{
 const f=fixture();f.select('auto');assert.equal(f.root.dataset.theme,'dark');assert.equal(f.calls.at(-1).payload.theme,'auto','auto must remain auto at the native boundary');
 f.media.matches=true;f.listeners[0]();assert.equal(f.root.dataset.theme,'light');assert.equal(f.calls.at(-1).payload.theme,'auto');assert.equal(f.listeners.length,1);
 f.select('dark');const before=f.calls.length;f.media.matches=false;f.listeners[0]();assert.equal(f.root.dataset.theme,'dark');assert.equal(f.calls.length,before,'fixed theme must not react to OS changes');
 f.select('nord');assert.equal(f.root.dataset.theme,'nord');assert.equal(f.calls.at(-1).payload.theme,'dark','custom dark palettes use native dark chrome');
 f.select('auto');assert.equal(f.listeners.length,1,'returning to auto must reuse one OS listener');assert.equal(f.calls.at(-1).payload.theme,'auto');
 const offline=fixture('auto',true,true);await offline.c.initTheme();assert.equal(offline.root.dataset.theme,'light','offline settings still follow OS');assert.equal(offline.calls.at(-1).payload.theme,'auto');
 const boot=fs.readFileSync('static/theme_boot.js','utf8');
 for(const [pref,light,want] of [['auto',true,'light'],['auto',false,'dark'],['light',false,'light'],['nord',true,'nord']]){
  const root={dataset:{themePreference:pref},style:{}};vm.runInNewContext(boot,{document:{documentElement:root},window:{matchMedia(){return {matches:light}}}});assert.equal(root.dataset.theme,want,'first paint must use the requested palette');
 }
})().catch(error=>{console.error(error);process.exitCode=1});
`
