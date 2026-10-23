package webui

import "testing"

func TestProviderContextBudgetBrowserOwnershipAndSaving(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const source = sources['chat.js'], appSource = sources['app.js'];
function extract(start, next, content=source) {
  const from=content.indexOf(start),to=content.indexOf(next,from+start.length);
  assert.ok(from>=0&&to>from,start);return content.slice(from,to);
}
function panel() {
  const result={hidden:true,textContent:'',className:'',attrs:{},setAttribute(k,v){this.attrs[k]=v}};
  const controls={mode:{value:'automatic',disabled:false},input:{value:'',disabled:true,focus(){}},label:{hidden:true},result,button:{disabled:false}};
  let html='';
  const p={hidden:true,dataset:{},textContent:'',classList:{add(){},remove(){}},
    querySelector(selector){return ({'[data-budget-mode]':controls.mode,'[data-budget-window]':controls.input,'[data-budget-window-label]':controls.label,'[data-budget-result]':controls.result})[selector]||null},
    querySelectorAll(){return [controls.mode,controls.input,controls.button]},controls};
  for(const control of Object.values(controls)) control.closest=()=>p;
  Object.defineProperty(p,'innerHTML',{get(){return html},set(value){html=value;controls.mode.value=/value="manual" selected/.test(value)?'manual':'automatic';controls.input.value=(value.match(/data-budget-window[^>]*value="([^"]*)"/)||[])[1]||'';controls.input.disabled=controls.mode.value!=='manual';controls.result.hidden=true;controls.result.textContent=''}});
  return p;
}
const panels={'providerBudget-gateway':panel(),'inputField':{value:'Unsent draft',focus(){throw Error('composer must stay untouched')}}};
const calls=[],pending=[],polls=[];
const c={console,Map,Number,encodeURIComponent,document:{documentElement:{lang:'zh-CN'},getElementById:id=>panels[id]||null},
  escHtml:String,escAttr:String,providerErrorText:String,
  lastStatusSnapshot:{contextProviderId:'gateway',contextModel:'A',contextRouteKey:'route-1'},
  pollStatus:async()=>polls.push(true),
  fetch:(url,options)=>new Promise(resolve=>{calls.push({url,options,body:options&&JSON.parse(options.body)});pending.push(resolve)})};
vm.createContext(c);
vm.runInContext(extract('const DESKTOP_I18N =', 'function presetDisplayName(',appSource),c);
vm.runInContext(extract('function contextCapacitySourceLabel(value, dict)', 'function renderContextMeter(',appSource),c);
vm.runInContext(extract('function uiText(en, zh)', 'const SETTINGS_ZH ='),c);
vm.runInContext(extract('const providerContextBudgetStates =','let presetsCache ='),c);
vm.runInContext(extract('function openDiscoveredProviderContextBudget(button)','async function deleteProvider(id)'),c);
const response=(model,window=1050000,requestedWindow=0,routeKey='route-1')=>({providerId:'gateway',model,routeKey,requestedWindow,applied:true,
  capabilities:{providerId:'gateway',model,routeKey,window,profileDefaultWindow:272000,profileMaxWindow:872000,modelWindow:1050000,maxInputTokens:922000,maxOutputTokens:128000,source:'catalog_profile',modelSource:'catalog_issuer'}});
const answer=(index,data,ok=true)=>pending[index]({ok,status:ok?200:409,json:async()=>data});
(async()=>{
  const p=panels['providerBudget-gateway'];
  const a=c.openProviderContextBudget('gateway','A');
  const b=c.openProviderContextBudget('gateway','B');
  answer(1,response('B'));await b;
  answer(0,response('A'));await a;
  assert.match(p.innerHTML,/上下文预算 · B/,'late model A metadata must not replace model B settings');
  assert.match(p.innerHTML,/模型完整容量.*1,050,000 tokens/);
  assert.match(p.innerHTML,/客户端建议窗口.*272,000 tokens/);
  assert.match(p.innerHTML,/客户端目录最大值.*872,000 tokens/);
  assert.doesNotMatch(p.innerHTML,/预算允许上限/,'a client catalog ceiling must not become a hard route maximum');
  assert.equal(p.controls.mode.value,'automatic');
  assert.equal(p.controls.input.disabled,true);

  p.controls.mode.value='manual';c.changeProviderContextBudgetMode(p.controls.mode);
  p.controls.input.value='1000000';
  const saveB=c.saveProviderContextBudget(p.controls.button);
  assert.deepEqual(calls[2].body,{model:'B',window:1000000,expectedRouteKey:'route-1'});
  assert.equal(calls[2].url,'/api/providers/gateway/context-budget');
  assert.equal(p.controls.button.disabled,true);
  assert.equal(p.controls.result.hidden,false);
  answer(2,{...response('B',800000,1000000),capabilities:{...response('B').capabilities,window:800000,maxWindow:800000}});await saveB;
  assert.match(p.innerHTML,/请求窗口.*1,000,000 tokens.*实际窗口.*800,000 tokens/);
  assert.equal(p.controls.input.value,'1000000','clamping must retain the saved request');
  assert.equal(polls.length,0,'saving a non-active model must not refresh another route binding');

  // Switching the same provider card during a pending save owns a new view.
  p.controls.input.value='700000';
  const oldSave=c.saveProviderContextBudget(p.controls.button);
  const openA=c.openProviderContextBudget('gateway','A');
  answer(4,response('A'));await openA;
  answer(3,response('B',700000,700000));await oldSave;
  assert.match(p.innerHTML,/上下文预算 · A/);
  assert.doesNotMatch(p.innerHTML,/700,000/,'late save feedback must not rewrite newly selected model');

  // Auto is an explicit per-model choice; it never saves the provider model.
  const automatic=c.saveProviderContextBudget(p.controls.button);
  assert.deepEqual(calls[5].body,{model:'A',window:0,expectedRouteKey:'route-1'});
  answer(5,response('A'));await automatic;
  assert.equal(polls.length,1,'the same provider, exact model and route may refresh its own status');
  assert.equal(p.controls.result.textContent,'预算已保存并生效。');
  assert.equal(panels.inputField.value,'Unsent draft');
  assert.equal(calls.some(call=>/turns|\/providers\/model$/.test(call.url)),false,'budget changes never submit a prompt or select a model');
  assert.equal(calls.some(call=>call.body&&(call.body.apiKey||call.body.baseUrl||call.body.routeKey)),false);

  p.controls.mode.value='manual';p.controls.input.value='1.5';
  await c.saveProviderContextBudget(p.controls.button);
  assert.equal(calls.length,6,'fractional windows are rejected before HTTP');
  assert.equal(p.controls.result.attrs.role,'alert');
  p.controls.input.value='900000';
  const rejected=c.saveProviderContextBudget(p.controls.button);
  assert.equal(calls[6].body.expectedRouteKey,'route-1','GET scope must accompany POST to prevent saving to a changed route');
  answer(6,{error:'route changed'},false);await rejected;
  assert.match(p.controls.result.textContent,/保存上下文预算失败/);
  assert.equal(p.controls.input.value,'900000','failed saving keeps the draft budget');
  assert.equal(p.controls.button.disabled,false);
  assert.equal(polls.length,1);

  const staleRoute=c.openProviderContextBudget('gateway','A');
  answer(7,response('A',500000,0,'route-new'));await staleRoute;
  const saveNewRoute=c.saveProviderContextBudget(p.controls.button);
  answer(8,response('A',500000,0,'route-new'));await saveNewRoute;
  assert.equal(polls.length,1,'same model on another route is not the active binding');
  c.closeProviderContextBudget(p.controls.button);assert.equal(p.hidden,true);

  // A navigation repaint replaces the card DOM. Old responses are discarded.
  const detached=c.openProviderContextBudget('gateway','C');
  panels['providerBudget-gateway']=panel();
  answer(9,response('C'));await detached;
  assert.equal(panels['providerBudget-gateway'].innerHTML,'');
  const unknown=c.openProviderContextBudget('gateway','Unknown');
  answer(10,{...response('Unknown',0),capabilities:{window:0,source:'unknown'}});await unknown;
  assert.match(panels['providerBudget-gateway'].innerHTML,/当前窗口.*未确认/);
  assert.match(panels['providerBudget-gateway'].innerHTML,/自动窗口尚未确认/);
  assert.doesNotMatch(panels['providerBudget-gateway'].innerHTML,/模型完整容量|预算允许上限/);
  const budgetPanel=panels['providerBudget-gateway'];
  const openResponse=async(model,data)=>{const index=calls.length;const request=c.openProviderContextBudget('gateway',model);answer(index,data);await request};
  const saveResponse=async(data)=>{const index=calls.length;const request=c.saveProviderContextBudget(budgetPanel.controls.button);answer(index,data);await request;return calls[index]};
  const legacy={...response('Legacy',500000),configured:false,capabilities:{...response('Legacy',500000).capabilities,budgetSource:'legacy_provider',requestedWindow:600000}};
  await openResponse('Legacy',legacy);
  assert.equal(budgetPanel.controls.mode.value,'manual','no per-model record inherits the legacy manual budget');
  assert.equal(budgetPanel.controls.input.value,'600000','the legacy request, not its clamped result, populates the editor');
  assert.equal(budgetPanel.controls.input.disabled,false);
  const preserveLegacy=await saveResponse({...response('Legacy',500000,600000),configured:true});
  assert.deepEqual(preserveLegacy.body,{model:'Legacy',window:600000,expectedRouteKey:'route-1'},'ordinary Save must not implicitly erase an inherited manual budget');
  await openResponse('Legacy',{...legacy,configured:true,requestedWindow:0});
  assert.equal(budgetPanel.controls.mode.value,'automatic','an explicit zero per-model record takes precedence over legacy metadata');
  assert.equal(budgetPanel.controls.input.disabled,true);
  const preserveAuto=await saveResponse({...response('Legacy'),configured:true,requestedWindow:0});
  assert.equal(preserveAuto.body.window,0);
  await openResponse('Legacy',{...legacy,configured:true,requestedWindow:700000});
  assert.equal(budgetPanel.controls.mode.value,'manual');
  assert.equal(budgetPanel.controls.input.value,'700000','a positive saved per-model record takes precedence over the legacy request');
  const preserveRecord=await saveResponse({...response('Legacy',700000,700000),configured:true});
  assert.equal(preserveRecord.body.window,700000);

  for (const mismatch of [{routeKey:'other-route'},{providerId:'other-provider'},{model:'other-model'}]) {
    await openResponse('Mismatch',{...response('Mismatch'),capabilities:{...response('Mismatch').capabilities,...mismatch}});
    assert.match(budgetPanel.textContent,/读取上下文预算失败.*上下文能力所属配置已变化/);
    const count=calls.length;
    await c.saveProviderContextBudget(budgetPanel.controls.button);
    assert.equal(calls.length,count,'mismatched nested ownership must never become editable capability state');
  }
  await openResponse('PostMismatch',response('PostMismatch'));
  budgetPanel.controls.mode.value='manual';budgetPanel.controls.input.value='600000';
  await saveResponse({...response('PostMismatch',600000,600000),capabilities:{...response('PostMismatch').capabilities,routeKey:'wrong-post-route'}});
  assert.match(budgetPanel.controls.result.textContent,/保存上下文预算失败.*接入路线已变化/);
  assert.equal(budgetPanel.controls.input.value,'600000','a mismatched save response keeps the budget draft');
  const sourceLabels={codex_model_catalog:'模型目录',codex_subscription_models:'订阅接口客户端资料',subscription_model_estimate:'内置订阅表估计',gemini_model_input:'模型输入上限',estimated:'估计预算',configured_clamped:'显式配置（已约束）',user:'显式配置'};
  for(const [source,label] of Object.entries(sourceLabels)) assert.equal(c.contextCapacitySourceLabel(source),label);
  c.document.documentElement.lang='en';
  assert.equal(c.contextCapacitySourceLabel('codex_subscription_models'),'Subscription client profile');
  assert.equal(c.contextCapacitySourceLabel('configured_clamped'),'Explicit configuration (limited)');
})().catch(error=>{console.error(error);process.exitCode=1});
`, "chat.js", "app.js")
}
