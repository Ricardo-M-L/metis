package webui

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDurableOperationsPageActionsAndStaleResponses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/durable_ops.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
let html='',calls=[],queue=[],reason='',aborts=0;
const root={hidden:true,dataset:{},set innerHTML(value){html=value},get innerHTML(){return html},setAttribute(){},addEventListener(){},querySelector(){return {scrollIntoView(){}}}};
const c={console,TextEncoder,AbortController,clearInterval,setInterval:()=>1,Promise,document:{visibilityState:'visible',getElementById:id=>id==='durableOpsPage'?root:id==='durableOpsReason'?{value:reason}:null},fetch:async(path,options)=>{calls.push({path,options});const result=queue.shift();return typeof result==='function'?result():{ok:true,json:async()=>result||{}}}};
c.window=c;vm.createContext(c);vm.runInContext(source,c);
const state=()=>vm.runInContext('durableOpsState',c);
const click=(action,data={})=>c.durableOpsClick({target:{closest:()=>({disabled:false,dataset:{durableAction:action,...data}})}});
(async()=>{
 const s=state();s.active=true;s.data={workspace:'/trusted',jobs:[{id:'job-1',name:'<img src=x onerror=bad()>',state:'active',workspace:'/trusted',budget:{max_usd:10,max_input_tokens:333,max_output_tokens:444},usage:{estimated_usd:2,input_tokens:111,output_tokens:222,unresolved_requests:1},run_ids:['run-1']}],handoffs:[{id:'handoff-1',state:'pending',admission:{state:'waiting',waiting_reason:'budget_exhausted',last_checked_at:'2026-10-15T10:00:00Z',recovery_hint:'Increase budget'},business:{state:'failed',acceptance:{passed:false,summary:'wrong artifact'}}}],runs:[{id:'run-1',status:'blocked',storage:{archived_records:40},operations:[{id:'op-1',status:'uncertain',tool_name:'Bash'}],usage_requests:[{id:'request-1',status:'unresolved'}]}],learning:[{kind:'workflow',name:'code-fix',active_version:'v2',baseline_version:'v0',rollback_version:'v1',activation_epoch:7,quality:{feedback_count:5,failure_count:2}}]};
 c.renderDurableOps();assert(html.includes('&lt;img'));assert(!html.includes('<img'));assert(html.includes('$2.0000 / $10.0000'));assert(html.includes('长期岗位'));assert(html.includes('定时任务'));assert(html.includes('>已启用</span>'));assert(html.includes('111 / 333'));assert(html.includes('222 / 444'));
 const pendingReceipt=c.durableOpsHandoff({id:'pending',business:{state:'running',completed_at:'0001-01-01T00:00:00Z',updated_at:'2026-10-16T06:00:00Z'}});assert(pendingReceipt.includes('<dt>回执时间</dt><dd>'+c.durableOpsTime('2026-10-16T06:00:00Z')+'</dd>'),'zero completion time should fall back');
 const noParent=c.durableOpsLearning({name:'initial',active_version:'v1',baseline_version:'v0',rollback_version:''},0);assert(noParent.includes('没有可恢复的上一个版本'));assert(/data-durable-action="rollback_learning"[^>]*disabled/.test(noParent));
 const limited=c.durableOpsMetadata(Array.from({length:101},(_,id)=>({id})));assert(limited.includes('仅显示前 100 条，共 101 条'));
 // Every change first reviews a specific record. No network write before confirmation.
 await click('pause_job',{index:'0'});assert.equal(calls.length,0);assert(html.includes('确认暂停岗位'));
 queue.push({ok:true},s.data);await click('confirm');assert.equal(calls[0].path,'/api/operations/action');assert.deepEqual(JSON.parse(calls[0].options.body),{action:'pause_job',id:'job-1',expected_workspace:'/trusted'});assert.equal(calls[1].path,'/api/operations');assert.equal(s.review,null);
 await click('tab',{tab:'handoffs'});assert(html.includes('budget_exhausted'));assert(html.includes('Increase budget'));assert(html.includes('wrong artifact'));assert(html.includes('未通过'));
 await click('tab',{tab:'runs'});assert(html.includes('op-1'));assert(html.includes('request-1'));assert(html.includes('存储容量与归档'));
 await click('tab',{tab:'learning'});assert(html.includes('<dt>回滚目标</dt><dd>v1</dd>'));await click('rollback_learning',{index:'0'});assert(html.includes('当前版本 v2，启用批次 7'));assert(html.includes('将恢复到上一个版本 v1'));assert(!html.includes('将恢复到基准版本'));assert(html.includes('maxlength="512"'));
 const before=calls.length;await click('confirm');assert.equal(calls.length,before,'reason required');reason='中'.repeat(171);await click('confirm');assert.equal(calls.length,before,'UTF8 over 512 bytes must not submit');assert(html.includes('512 字节'));reason='x'.repeat(513);await click('confirm');assert.equal(calls.length,before,'ASCII over 512 bytes must not submit');reason='中'.repeat(170)+'ab';queue.push({ok:true},s.data);await click('confirm');assert.deepEqual(JSON.parse(calls[before].options.body),{action:'rollback_learning',expected_workspace:'/trusted',kind:'workflow',name:'code-fix',expected_version:'v2',expected_epoch:7,note:'中'.repeat(170)+'ab'});
 // A later refresh supersedes a slow one. Leaving aborts only this page's read.
 let resolve;queue.push(()=>new Promise(r=>resolve=r));const slow=c.loadDurableOps();queue.push({...s.data,workspace:'/new'});await c.loadDurableOps();resolve({ok:true,json:async()=>({...s.data,workspace:'/old'})});await slow;assert.equal(s.data.workspace,'/new');
 queue.push(()=>new Promise(r=>resolve=r));const leaving=c.loadDurableOps();c.hideDurableOpsPage();assert.equal(s.controller.signal.aborted,true);resolve({ok:true,json:async()=>({...s.data,workspace:'/wrong'})});await leaving;assert.equal(s.data.workspace,'/new');assert.equal(s.active,false);
 // Failed refresh keeps the last snapshot but disables actions against stale state.
 s.active=true;queue.push(()=>Promise.reject(new Error('offline')));await c.loadDurableOps();assert.equal(s.data.workspace,'/new');assert(html.includes('保留上次读取的状态'));s.review=null;await click('pause_job',{index:'0'});assert.equal(s.review,null);
 s.active=false;s.review={action:'pause_job'};c.durableOpsWorkspaceChanged('/another');assert.equal(s.data,null);assert.equal(s.review,null);assert.equal(s.error,'');
 console.log('durable operations UI assertions passed');
})().catch(error=>{console.error(error);process.exitCode=1});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = strings.NewReader(string(source))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UI test: %v\n%s", err, output)
	}
}
func TestDurableOperationsPageIsMountedAndNavigable(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	nav, err := staticFS.ReadFile("static/navigation.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`id="operationsNav"`, `id="durableOpsPage"`, `durable_ops.js?v=__BUILD__`, `durable_ops.css?v=__BUILD__`} {
		if !strings.Contains(string(index), needle) {
			t.Fatalf("missing %s", needle)
		}
	}
	for _, needle := range []string{"['durableOpsPage', 'operations']", "navigation.register('operations'", "hideDurableOpsPage()"} {
		if !strings.Contains(string(nav), needle) {
			t.Fatalf("missing navigation %s", needle)
		}
	}
}
