package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// Exercise the actual production functions, including their default arguments.
// A historical mutation must not borrow the selected live plan during replay.
func TestTodoMutationHistoryDoesNotBorrowLivePlan(t *testing.T) {
	runTodoProjectionBrowserTest(t, `
 const historical = JSON.stringify({content:'ALPHA historical', status:'completed'});
 c.applyTodoSnapshot('TodoWrite',{todos:[{content:'BETA live',status:'pending'}]});
 const pending = c.toolSummaryText('TodoWrite',historical);
 c.applyTodoSnapshot('TodoWrite',{todos:[{content:'BETA live',status:'completed'}]});
 const complete = c.toolSummaryText('TodoWrite',historical);
 assert.equal(pending,complete,'changing the live plan rewrote a historical tool summary');
 assert.doesNotMatch(pending,/BETA/);
 assert.match(pending,/1\/1|ALPHA/,'summary should describe only this historical event');
 assert.equal(c.normalizeTodoItems(historical).length,1,'default normalization borrowed live tasks');
 // A live single-item mutation still merges into its explicitly supplied base.
 c.applyTodoSnapshot('TodoWrite',historical);
 assert.match(elements.todoPlanList.innerHTML,/BETA live/);
 assert.match(elements.todoPlanList.innerHTML,/ALPHA historical/);
 // History normalization and re-rendering must not mutate the current list.
 const before=elements.todoPlanList.innerHTML;
 c.toolSummaryText('TodoWrite',historical);
 c.renderTodoPlan();
 assert.equal(elements.todoPlanList.innerHTML,before);
 `)
}

func TestTodoStepsAndExecutionTasksHaveSeparatePresentation(t *testing.T) {
	runTodoProjectionBrowserTest(t, `
 const steps=[{content:'Inspect schema',status:'completed'},
  {content:'Compare identity architecture',status:'completed'},
  {content:'Review proposal',status:'in_progress'}];
 const execution=[{content:'Check IAM storage boundary',status:'completed',owner:'verify'}];
 const status={activeSessionId:'A',planTurn:4,planItems:[...steps,...execution],
  planSteps:steps,executionItems:execution,turnRunning:true,runningSessionId:'A'};
 assert.equal(c.applyStatusPlanSnapshot(status),true);
 assert.match(elements.todoPlanStepLabel.textContent,/3 \/ 3/,'execution task became a fourth primary step');
 assert.doesNotMatch(elements.todoPlanStepLabel.textContent,/4 \/ 4|3 \/ 4/);
 assert.match(elements.todoPlanList.innerHTML,/todo-plan-execution-group/);
 assert.match(elements.todoPlanList.innerHTML,/执行任务/);
 assert.equal((elements.todoPlanList.innerHTML.match(/Check IAM storage boundary/g)||[]).length,1);
 assert.match(elements.todoPlanList.innerHTML,/verify/,'execution ownership was hidden');
 const writes=elements.todoPlanList.innerHTMLWrites;
 elements.todoPlanList.scrollTop=37;
 c.applyStatusPlanSnapshot(status);
 c.renderTodoPlan();
 assert.equal(elements.todoPlanList.innerHTMLWrites,writes,'unchanged polling remounted the task disclosure');
 assert.equal(elements.todoPlanList.scrollTop,37);
 vm.runInContext('todoExecutionOpen = true',c);
 c.renderTodoPlan();
 assert.match(elements.todoPlanList.innerHTML,/<details[^>]+open/,'refresh collapsed an expanded execution group');
 // Primary completion does not imply an unfinished execution task completed.
 c.applyStatusPlanSnapshot({...status,planSteps:steps.map(x=>({...x,status:'completed'})),
  executionItems:[{...execution[0],status:'in_progress'}]});
 assert.equal(elements.todoPlanDock.classList.contains('is-complete'),false);
 assert.match(elements.todoPlanCounts.textContent,/执行任务/);
 // Authoritative empty arrays clear old groups, including on session changes.
 c.clearTodoPlan();c.currentSessionId='B';
 c.applyStatusPlanSnapshot({activeSessionId:'B',planItems:[],planSteps:[],executionItems:[]});
 assert.equal(elements.todoPlanDock.hidden,true);
 assert.doesNotMatch(elements.todoPlanList.innerHTML,/Check IAM storage boundary/);
 // Old servers have no source metadata; preserve their previous behavior.
 c.applyStatusPlanSnapshot({activeSessionId:'B',planItems:[{content:'Legacy plan',status:'pending'}]});
 assert.match(elements.todoPlanList.innerHTML,/Legacy plan/);
 `)
}

func runTodoProjectionBrowserTest(t *testing.T, assertions string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"source": string(source), "assertions": assertions})
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const {source,assertions}=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
function extract(start,next){const a=source.indexOf(start),b=source.indexOf(next,a+start.length);assert(a>=0&&b>a,start);return source.slice(a,b)}
function element(){const classes=new Set();let html='',writes=0;return {hidden:false,textContent:'',
 get innerHTML(){return html},set innerHTML(value){html=value;writes++},get innerHTMLWrites(){return writes},
 classList:{toggle:(n,on)=>on?classes.add(n):classes.delete(n),contains:n=>classes.has(n)},setAttribute(){}}}
const elements=Object.fromEntries(['todoPlanDock','todoPlanPopover','todoPlanTrigger','todoPlanStepLabel','todoPlanCounts','todoPlanList'].map(id=>[id,element()]));
const c={currentSessionId:'A',turnRunning:true,runningSessionId:'A',renderingActivityHistory:false,
 document:{getElementById:id=>elements[id]||null},escHtml:String,escAttr:String,
 semanticToolSummary:()=>null,firstLine:s=>String(s).split('\n')[0],toolVariantConfig:()=>undefined};
vm.createContext(c);
vm.runInContext(extract('let todoPlanItems =','let pendingAsk ='),c);
vm.runInContext(extract('function toolSummaryText(name, input)','function isWebSearchTool(name)'),c);
eval(assertions);
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("todo event/projection browser harness: %v\n%s", err, out)
	}
}
