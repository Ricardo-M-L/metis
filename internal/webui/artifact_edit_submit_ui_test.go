package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestArtifactEditUsesNormalTurnWithoutConsumingDraft(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/artifact_edit_submit.js")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", `
const assert = require('node:assert/strict'), vm = require('node:vm');
const calls = [], toasts = [];
const draft = {text:'Unsent user draft',images:[{data:'draft-image'}]};
const c = {currentSessionId:'s1',pendingAsk:null,turnRunning:false,runningSessionId:null,
  uiText:(_,zh)=>zh,showToast:message=>toasts.push(message),
  escHtml:text=>String(text).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'),
  isViewedTurnRunning:()=>c.turnRunning,parallelTurnsEnabled:()=>false,
  closeArtifactPreview:options=>calls.push(['close',options]),switchView:view=>calls.push(['view',view]),
  runTurnItem:item=>{calls.push(['turn',item]);c.turnRunning=true;return Promise.resolve(true);},
  document:{getElementById:()=>draft},attachments:draft.images};
vm.createContext(c);vm.runInContext(require('node:fs').readFileSync(0,'utf8'),c);
const ref={artifactId:'artifact-a',version:1,digest:'a'.repeat(64),targetId:'target-1'};
(async()=>{
  assert.equal(await c.submitArtifactAnnotationPrompt('canonical edit',ref,'s2'),false);
  assert.equal(calls.length,0,'wrong session must not dispatch');
  c.pendingAsk={id:'ask'};
  assert.equal(await c.submitArtifactAnnotationPrompt('canonical edit',ref,'s1'),false);
  assert.equal(calls.length,0,'artifact instructions must not become AskUser answers');
  c.pendingAsk=null;c.turnRunning=true;
  assert.equal(await c.submitArtifactAnnotationPrompt('canonical edit',ref,'s1'),false);
  assert.equal(calls.length,0,'busy turn must not steer or overwrite request');
  c.turnRunning=false;
  assert.equal(await c.submitArtifactAnnotationPrompt('canonical edit',{...ref,version:0},'s1'),false);
  assert.equal(await c.submitArtifactAnnotationPrompt('canonical edit',ref,'s1'),true);
  assert.equal(calls[0][0],'close');assert.equal(calls[0][1].navigation,false);
  assert.deepEqual(calls[1],['view','chat']);
  assert.equal(calls[2][0],'turn');assert.equal(calls[2][1].text,'canonical edit');
  assert.equal(calls[2][1].images.length,0);
  assert.equal(draft.text,'Unsent user draft');assert.equal(draft.images[0].data,'draft-image');
  assert.equal(await c.submitArtifactAnnotationPrompt('second edit',ref,'s1'),false);
  assert.equal(calls.filter(item=>item[0]==='turn').length,1,'double submission must not duplicate turns');
  const raw='请根据用户在 HTML Artifact 上的点选，修改现有产物。\n\n'+'\x60\x60\x60json\n'+JSON.stringify({
    metis_artifact_annotation:{...ref,title:'<script>not executable</script>',
      selection:{text:'<img src=x>'},instruction:'Make it green\nKeep the label'}
  })+'\n'+'\x60\x60\x60'+'\ninternal execution details';
  const markup=c.artifactAnnotationMessageMarkup(raw);
  assert.match(markup,/&lt;script&gt;/);assert.match(markup,/&lt;img src=x&gt;/);
  assert.match(markup,/Make it green\nKeep the label/);
  assert(!markup.includes('internal execution details'),'technical context stays out of the displayed user bubble');
  assert(raw.includes('internal execution details'),'full prompt remains untouched');
  assert.equal(c.artifactAnnotationMessageMarkup('ordinary prompt'),null);
  assert.equal(c.artifactAnnotationMessageMarkup('请根据用户在 HTML Artifact 上的点选，修改现有产物。\n\x60\x60\x60json\ninvalid\n\x60\x60\x60'),null);
})().catch(error=>{console.error(error);process.exit(1);});
`)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("artifact edit submit: %v\n%s", err, output)
	}
}

func TestArtifactAnnotationBrowserInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "../../scripts/e2e/artifact_annotation_ui_check.cjs")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("artifact annotation browser interactions: %v\n%s", err, output)
	}
}
