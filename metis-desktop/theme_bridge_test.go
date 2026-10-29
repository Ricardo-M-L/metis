package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFrontendThemeBridgeControlsNativeWindowAppearance(t *testing.T) {
	body, err := os.ReadFile("frontend/src/main.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	for _, want := range []string{
		"SetNativeTheme",
		"'set-theme': payload =>",
		"return SetNativeTheme(theme)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("frontend theme bridge missing %q", want)
		}
	}
}

func TestDesktopThemeBridgePreservesSystemPreference(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", `
const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const source=fs.readFileSync('frontend/src/main.js','utf8').replace(/import\s*\{[\s\S]*?\}\s*from\s*[^;]+;/,'');
const listeners={},native=[],responses=[];
const frame={style:{},contentWindow:{postMessage(message,origin){responses.push({message,origin})}},addEventListener(){}};
const root={dataset:{}},elements={'metis-frame':frame,boot:{style:{}},'boot-status':{}};
const c=vm.createContext({URL,document:{documentElement:root,getElementById(id){return elements[id]}},
 window:{addEventListener(type,fn){listeners[type]=fn}},
 StartWebUI:async()=> 'http://127.0.0.1:1234/?desktop-frame=owned-test-frame',
 SetNativeTheme:async(theme)=>{native.push(theme)},ChooseWorkspaceDirectory(){},GetUpdateProgress(){},GetUpdateStatus(){},InstallUpdateAndRestart(){},StartInstallUpdateAndRestart(){}});
vm.runInContext(source,c);
(async()=>{
 await listeners.load();
 for(const theme of ['auto','light','dark','auto']){
  await listeners.message({source:frame.contentWindow,origin:'http://127.0.0.1:1234',data:{channel:'metis-native',kind:'request',id:theme,action:'set-theme',payload:{theme}}});
  assert.equal(native.at(-1),theme,'the native call must preserve auto');assert.equal(root.dataset.theme,theme,'parent loading surface must share the selected preference');
 }
 const count=native.length;
 await listeners.message({source:{},origin:'http://127.0.0.1:1234',data:{channel:'metis-native',kind:'request',action:'set-theme',payload:{theme:'dark'}}});
 assert.equal(native.length,count,'another frame cannot override native appearance');assert.equal(responses.length,4);
})().catch(error=>{console.error(error);process.exitCode=1});
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("desktop theme behavior: %v\n%s", err, out)
	}
}
