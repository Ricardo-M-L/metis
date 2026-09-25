package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestProviderDiscoveryBrowserActionsKeepFeedbackVisible(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
function extract(start, next) {
  const from = source.indexOf(start), to = source.indexOf(next, from + start.length);
  assert(from >= 0 && to > from, start);
  return source.slice(from, to);
}
const error = { hidden:true, textContent:'' };
const buttons = [{disabled:false},{disabled:false}];
const results = [];
const panelList = { innerHTML:'', insertAdjacentHTML(){} };
const panel = {
  hidden:true, textContent:'', innerHTML:'', dataset:{},
  classList:{add(){},remove(){}},
  querySelector(selector){ return selector === '.provider-discovery-list' ? panelList : null; },
};
const calls = [];
const c = {
  Map, providerDiscoveryCache:new Map(),
  uiText:(en,zh)=>zh, providerErrorText:s=>s,
  setProviderResult:(...args)=>results.push(args),
  escHtml:s=>String(s), escAttr:s=>String(s),
  document:{getElementById:id=>id === 'providerDiscovery-fixture' ? panel : null},
  fetch:async (url,options)=>{
    calls.push({url,body:JSON.parse(options.body)});
    return url.endsWith('/models')
      ? {ok:true,json:async()=>({models:[{id:'model-a',name:'Model A'},{id:'model-b',name:'Model B'}],source:'provider'})}
      : {ok:true,json:async()=>({model:'model-b'})};
  },
  loadProviders:async()=>{},
  runProviderProbe:async()=>{throw new Error('credential is not configured')},
  composerActionDialog:{kind:'provider-probe',providerId:'fixture',pending:false,overlay:{
    querySelector:s=>s === '.composer-action-error' ? error : null,
    querySelectorAll:()=>buttons,
  }},
};
vm.createContext(c);
vm.runInContext(extract('async function confirmComposerAction(event)', 'function handleKeydown(e)'),c);
vm.runInContext(extract('async function fetchProviderModels(id, origin, trigger)', 'async function deleteProvider(id)'),c);
(async()=>{
  await c.confirmComposerAction({preventDefault(){}});
  assert.equal(error.hidden,false);
  assert.match(error.textContent,/credential is not configured/);
  assert.equal(c.composerActionDialog.pending,false);
  assert.equal(buttons.every(button=>!button.disabled),true);
  assert.equal(results.at(-1)[2],'error');

  const trigger={disabled:false};
  await c.fetchProviderModels('fixture','card',trigger);
  assert.equal(trigger.disabled,false);
  assert.equal(panel.hidden,false);
  assert.match(panelList.innerHTML,/model-a/);
  assert.equal(calls[0].url,'/api/providers/models');
  assert.equal(calls[0].body.id,'fixture');
  await c.selectDiscoveredProviderModel({dataset:{modelId:'model-b'},disabled:false,closest:()=>panel});
  assert.equal(calls[1].url,'/api/providers/model');
  assert.equal(calls[1].body.model,'model-b');
  assert.equal(results.at(-1)[2],'success');
})().catch(err=>{console.error(err);process.exitCode=1});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provider browser actions: %v\n%s", err, out)
	}
}
