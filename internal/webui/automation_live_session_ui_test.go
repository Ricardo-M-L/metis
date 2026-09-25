package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestAutomationSessionShowsLiveProgressThenSavedAnswer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/automations.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const area={card:null,querySelector(){return this.card;},insertAdjacentHTML(){this.card={innerHTML:'',classList:{toggle(){}}};}};
const timers=[];let syncs=0,answer='',calls=[];
let records=[
 {status:'running',liveText:'正在搜索 <script>bad()</script>',activity:[{kind:'tool_start',tool:'Search'}]},
 {status:'running',liveText:'已找到第一条来源',activity:[{kind:'tool_done',tool:'Search'}]},
 {status:'succeeded',output:'最终回答'}
];
const c={console,Map,Set,Date,Intl,AbortController,currentSessionId:'session-one',
 document:{documentElement:{lang:'zh-CN'},visibilityState:'visible',getElementById:id=>id==='chatArea'?area:null},
 uiText:(en,zh)=>zh,autoScroll(){},
 fetch:async url=>{calls.push(url);return{ok:true,json:async()=>records.shift()};},
 syncViewedSessionHistory:async(id,guard)=>{assert.equal(id,'session-one');assert(guard());syncs++;answer='最终回答';area.card=null;return true;},
 setTimeout:fn=>{timers.push(fn);return timers.length;},clearTimeout(){},window:{}};
vm.createContext(c);vm.runInContext(source,c);
const runs=vm.runInContext('automationSessionRuns',c);
async function next(){const fn=timers.shift();assert(fn,'expected another poll');fn();await tick();}
(async()=>{
 runs.set('session-one',{jobId:'job-one',runId:'run-one'});
 c.watchAutomationSession('session-one');await tick();
 assert.match(area.card.innerHTML,/正在搜索/);
 assert.match(area.card.innerHTML,/&lt;script&gt;/);
 assert.doesNotMatch(area.card.innerHTML,/<script>/);
 assert.match(area.card.innerHTML,/Search/);
 await next();assert.match(area.card.innerHTML,/已找到第一条来源/);
 await next();assert.equal(syncs,1);assert.equal(answer,'最终回答');assert.equal(area.card,null);
 assert.equal(timers.length,0);assert(calls.every(url=>url==='/api/automations/job-one/runs/run-one'));

 // A terminal failure still explains why no answer appeared in the chat.
 c.currentSessionId='session-two';runs.set('session-two',{jobId:'job-two',runId:'run-two'});
 records=[{status:'failed',error:'provider unavailable',output:''}];
 c.syncViewedSessionHistory=async()=>{area.card=null;return true;};
 c.watchAutomationSession('session-two');await tick();
 assert.match(area.card.innerHTML,/provider unavailable/);
 assert.match(area.card.innerHTML,/失败/);
 assert.equal(timers.length,0);

 // A successful run with no assistant text must not look like a finished answer.
 c.currentSessionId='session-three';runs.set('session-three',{jobId:'job-three',runId:'run-three'});
 records=[{status:'succeeded',output:''}];
 c.watchAutomationSession('session-three');await tick();
 assert.match(area.card.innerHTML,/没有文字回答/);
 assert.equal(timers.length,0);
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser behavior: %v\n%s", err, output)
	}
}
