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
      ? {ok:true,json:async()=>({models:[{id:'model-a',name:'Model A',contextWindow:272000,maxContextWindow:872000,maxInputTokens:144000,maxOutputTokens:128000,source:'endpoint_pi'},{id:'model-b',name:'Model B',maxInputTokens:500000},{id:'profile-model',name:'Client profile',contextWindow:272000,maxContextWindow:872000,maxOutputTokens:128000,windowKind:'client_profile',source:'endpoint_pi'}],source:'provider'})}
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
  assert.match(panelList.innerHTML,/默认窗口 272,000 tokens/);
  assert.match(panelList.innerHTML,/预算允许上限 872,000 tokens/);
  assert.match(panelList.innerHTML,/最大输入 144,000 tokens/);
  assert.match(panelList.innerHTML,/最大输出 128,000 tokens/);
  assert.equal(c.providerDiscoveryCache.get('card:fixture')[0].source,'endpoint_pi');
  const modelButtons=Array.from(panelList.innerHTML.matchAll(/<button[^>]*class="provider-model-choice"[^>]*>([\s\S]*?)<\/button>/g),match=>match[1]);
  const inputOnlyButton=modelButtons[1];
  assert.match(inputOnlyButton,/最大输入 500,000 tokens/);
  assert.doesNotMatch(inputOnlyButton,/默认窗口|最大支持|最大输出/);
  const profileButton=modelButtons[2];
  assert.match(profileButton,/客户端建议窗口 272,000 tokens/);
  assert.match(profileButton,/客户端目录最大值 872,000 tokens/);
  assert.doesNotMatch(profileButton,/默认窗口|预算允许上限/,'a client profile is not the model hard capacity');
  assert.equal(c.providerDiscoveryCache.get('card:fixture')[2].windowKind,'client_profile');
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
