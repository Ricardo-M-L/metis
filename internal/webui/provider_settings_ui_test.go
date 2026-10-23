package webui

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

// Execute the complete module rather than extracting functions from chat.js.
// The deadline also catches forgotten pending responses in the async fixtures.
func runProviderSettingsUI(t *testing.T, assertions string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, desktopStatsNode(t), "-e", providerSettingsUIFixture+assertions)
	cmd.Stdin = bytes.NewReader(desktopStatsSources(t, "providers.js", "index.html", "chat.js", "navigation.js"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provider settings browser behavior: %v\n%s", err, output)
	}
}

const providerSettingsUIFixture = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const clone = value => JSON.parse(JSON.stringify(value));
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve, reject; const promise = new Promise((res, rej) => {resolve=res; reject=rej}); return {promise, resolve, reject}; };
const response = (data, status=200) => ({ok:status>=200&&status<300, status, json:async()=>clone(data)});
const provider = (id='gateway') => ({id, kind:'custom', custom:true, displayName:'Gateway', catalogProvider:'openai', transport:'openai_chat', baseUrl:'https://gateway.invalid/v1', model:'model-a', models:[{id:'model-a', name:'Model A', contextWindow:1000000, maxOutputTokens:64000, outputBudget:12000, supportsVision:false},{id:'model-b', name:'Model B', contextWindow:500000, maxOutputTokens:32000, outputBudget:8000}], credentialKind:'api_key', credentialConfigured:true, credentialSource:'config', hasCredential:true, editable:true, readOnly:false});
const envelope = (revision='revision-1', items=[provider()]) => ({revision, providers:items, savedDefault:{provider:'gateway',model:'model-a'}, current:{provider:'gateway',model:'model-b'}});
function browser(lang='en') {
  const root={dataset:{},innerHTML:'',addEventListener(){},removeEventListener(){},querySelector(){return null},querySelectorAll(){return []},contains(){return true}};
  const c={console,AbortController,TextEncoder,URL,URLSearchParams,setTimeout,clearTimeout,Promise,document:{documentElement:{lang},getElementById:()=>null,querySelector:()=>null,addEventListener(){},removeEventListener(){}},confirm:()=>true};
  c.window=c;
  vm.createContext(c);vm.runInContext(sources['providers.js'],c);
  assert.equal(typeof c.MetisProviderSettings?.createController,'function','the complete provider module exposes its state controller');
  return {c,root};
}
function fixture(lang='en',catalog=[]) {
  const {c,root}=browser(lang),calls=[],queued=[],changes=[];
  const controller=c.MetisProviderSettings.createController({
    fetch:(url,options={})=>{
      const call={url,options,body:options.body?JSON.parse(options.body):undefined};calls.push(call);
      if(url==='/api/providers/catalog'&&(!options.method||options.method==='GET')) return Promise.resolve(response({providers:catalog}));
      const item=queued.shift();assert.ok(item,'unexpected HTTP request: '+url);
      return item.promise;
    },confirm:()=>true,onChange:(state,reason)=>changes.push({state:clone(state),reason}),
  });
  const next=()=>{const item=deferred();queued.push(item);return item};
  const answer=async(data,action,status=200)=>{const item=next();const task=action();item.resolve(response(data,status));await task;};
  const load=async(data=envelope())=>answer(data,()=>controller.load());
  const writes=()=>calls.filter(call=>call.options.method&&call.options.method!=='GET');
  return {c,root,controller,calls,changes,next,answer,load,writes,state:()=>controller.getState()};
}
function finish(test) {
  // A pending Promise alone does not keep Node alive. Make an accidentally
  // uncompleted async test fail instead of silently exiting successfully.
  const watchdog=setTimeout(()=>{console.error('provider UI test did not complete');process.exit(1)},25000);
  Promise.resolve().then(test).then(()=>clearTimeout(watchdog),error=>{clearTimeout(watchdog);console.error(error);process.exitCode=1});
}
`

func TestProviderSettingsUIMultiModelDraftAndDiscoveryImport(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load();
  f.controller.edit('gateway');
  assert.equal(f.state().view,'editor');
  assert.equal(f.controller.hasUnsavedChanges(),false);
  f.controller.updateModel(0,{name:'Hand tuned A',contextWindow:1100000,maxOutputTokens:128000,outputBudget:16000,supportsVision:false});
  f.controller.updateModel(1,{outputBudget:9000});
  assert.equal(f.controller.hasUnsavedChanges(),true);
  assert.equal(f.state().envelope.providers[0].models[0].name,'Model A','editing must not mutate the saved snapshot');
  assert.equal(f.state().draft.models[1].outputBudget,9000);
  f.controller.addModel({id:'temporary',name:'Temporary',contextWindow:0,maxOutputTokens:0,outputBudget:0});
  f.controller.removeModel(f.state().draft.models.findIndex(model=>model.id==='temporary'));
  assert.equal(f.state().draft.models.length,2);
  const discovery=f.next(),discovering=f.controller.discover();
  assert.equal(f.state().discovering,true);
  discovery.resolve(response({models:[{id:'model-a',name:'Upstream A',contextWindow:272000,maxOutputTokens:32000},{id:'model-c',name:'Model C',contextWindow:1000000,maxOutputTokens:64000}]}));
  await discovering;
  const count=f.writes().length;
  f.controller.importModels(['model-a','model-c']);
  f.controller.importModels(['model-c']);
  assert.equal(f.writes().length,count,'import only edits the local draft');
  const models=clone(f.state().draft.models);
  assert.equal(models.length,3,'the same discovered model is imported only once');
  assert.deepEqual(models[0],{id:'model-a',name:'Hand tuned A',contextWindow:1100000,maxOutputTokens:128000,outputBudget:16000,supportsVision:false},'an upstream duplicate must preserve every manual override, including false');
  assert.equal(models[1].outputBudget,9000,'other model settings survive discovery');
  assert.equal(models[2].id,'model-c');
  assert.deepEqual(models[2],{id:'model-c',name:'Model C'},'discovery imports the identity without converting recommendations into permanent manual capability overrides');
  assert.equal(f.state().draft.model,'model-a','import never silently changes the selected model');
  const request=f.calls.find(call=>call.url==='/api/providers/models');
  assert.equal(request.body.id,'gateway');
  assert.equal(request.body.transport,'openai_chat');
  assert.equal(request.body.baseUrl,'https://gateway.invalid/v1');
  assert.equal(request.body.apiKey||'','');
  assert.equal(f.calls.some(call=>call.url==='/api/providers/default'),false);
});
`)
}

func TestProviderSettingsUIDiscoveryResponsesBelongToExactDraft(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load(envelope('revision-1',[provider(),provider('second')]));
  f.controller.edit('gateway');
  const oldEndpoint=f.next(),oldDiscovery=f.controller.discover();
  f.controller.updateProvider({baseUrl:'https://new-route.invalid/v1'});
  oldEndpoint.resolve(response({models:[{id:'stale-route',name:'Must not import'}]}));
  await oldDiscovery;
  assert.equal(f.state().draft.baseUrl,'https://new-route.invalid/v1');
  assert.equal((f.state().discovery||[]).some(model=>model.id==='stale-route'),false,'a response for the old endpoint must not become a candidate for the edited endpoint');
  const oldProvider=f.next(),oldProviderRequest=f.controller.discover();
  f.controller.edit('second');
  oldProvider.resolve(response({models:[{id:'wrong-provider'}]}));
  await oldProviderRequest;
  assert.equal(f.state().draft.id,'second');
  assert.equal((f.state().discovery||[]).some(model=>model.id==='wrong-provider'),false,'switching provider invalidates the old request even if abort is ignored');
  const leaving=f.next(),leavingRequest=f.controller.discover();
  f.controller.updateProvider({credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
  f.controller.dispose();
  leaving.resolve(response({models:[{id:'late-disposed'}]}));
  await leavingRequest;
  assert.equal(f.controller.hasUnsavedChanges(),false);
  assert.equal(JSON.stringify(f.state()).includes('synthetic-ui-only-key'),false,'dispose clears transient credentials');
  assert.equal(JSON.stringify(f.state()).includes('late-disposed'),false,'disposed requests cannot repopulate UI state');
});
`)
}

func TestProviderSettingsUISaveAndDefaultUseRevisionedEnvelopes(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load();f.controller.edit('gateway');
  f.controller.updateModel(1,{name:'Edited B',contextWindow:1200000,outputBudget:10000});
  f.controller.updateProvider({displayName:'Edited gateway',credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
  const reply=f.next(),saving=f.controller.save();
  assert.ok(f.state().pending);
  const count=f.writes().length;
  assert.equal(await f.controller.save(),false);
  assert.equal(f.writes().length,count,'a second click cannot issue a second save');
  assert.equal(f.controller.cancel(),false,'an in-flight save cannot be presented as cancelled');
  const call=f.writes().at(-1);
  assert.equal(call.url,'/api/providers');assert.equal(call.options.method,'POST');
  assert.equal(call.body.expectedRevision,'revision-1');
  assert.equal(call.body.id,'gateway');assert.equal(call.body.displayName,'Edited gateway');
  assert.equal(call.body.baseUrl,'https://gateway.invalid/v1');
  assert.equal(call.body.credentialAction,'replace');assert.equal(call.body.apiKey,'synthetic-ui-only-key');
  assert.equal(call.body.models.length,2);assert.equal(call.body.models[0].supportsVision,false);
  assert.equal(call.body.models[1].contextWindow,1200000);assert.equal(call.body.models[1].outputBudget,10000);
  assert.equal(call.body.model,'model-a');
  assert.equal('makeDefault' in call.body,false,'ordinary Save cannot implicitly change the default');
  const saved=provider();saved.displayName='Normalized gateway';saved.models=clone(call.body.models);
  const nextEnvelope=envelope('revision-2',[saved]);
  reply.resolve(response(nextEnvelope));await saving;
  assert.deepEqual(clone(f.state().envelope),nextEnvelope,'save replaces the whole envelope rather than treating it as one provider');
  assert.equal(f.state().view,'list');assert.equal(Boolean(f.state().pending),false);
  assert.equal(f.controller.hasUnsavedChanges(),false);
  assert.equal(JSON.stringify(f.state()).includes('synthetic-ui-only-key'),false);
  const next=f.next(),makingDefault=f.controller.setDefault('gateway','model-b');
  const defaultCall=f.writes().at(-1);
  assert.equal(defaultCall.url,'/api/providers/default');
  assert.deepEqual(defaultCall.body,{id:'gateway',model:'model-b',expectedRevision:'revision-2'});
  const changed=envelope('revision-3',[saved]);changed.savedDefault={provider:'gateway',model:'model-b'};
  next.resolve(response(changed));await makingDefault;
  assert.deepEqual(clone(f.state().envelope),changed);
  assert.equal(f.calls.some(call=>call.url==='/api/providers/activate'),false,'saving a default cannot apply a session route');
});
`)
}

func TestProviderSettingsUIConflictCancelAndValidationKeepUserIntent(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load();f.controller.edit('gateway');
  f.controller.updateModel(0,{contextWindow:1.5});
  const before=f.writes().length;
  assert.equal(await f.controller.save(),false);assert.equal(f.writes().length,before,'fractional capacity is rejected before HTTP');
  f.controller.updateModel(0,{contextWindow:1000000});
  f.controller.updateProvider({displayName:'Unsaved gateway',credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
  await f.answer({error:'provider settings changed; reload before saving'},()=>f.controller.save(),409);
  assert.equal(f.state().draft.displayName,'Unsaved gateway');
  assert.equal(f.controller.hasUnsavedChanges(),true,'a CAS conflict preserves the draft for review');
  assert.equal(f.state().envelope.revision,'revision-1');
  assert.equal(Boolean(f.state().pending),false);
  assert.ok(f.state().error,'the conflict is visible');
  assert.notEqual(f.controller.cancel(),false);
  assert.equal(f.controller.hasUnsavedChanges(),false);
  assert.equal(f.state().view,'list');
  assert.equal(JSON.stringify(f.state()).includes('synthetic-ui-only-key'),false,'cancel clears secret in both draft and original state');
  assert.equal(f.state().envelope.providers[0].displayName,'Gateway','cancel leaves the saved data intact');
  f.controller.edit('gateway');
  assert.equal(f.state().draft.apiKey||'','');
  assert.equal(f.state().draft.credentialAction,'keep');
  f.controller.updateProvider({displayName:'Blocked navigation'});
  const confirms=[];
  const g=fixture();await g.load();
  // A controller with a refusing confirmation keeps the editor and its draft.
  const refusing=g.c.MetisProviderSettings.createController({fetch:async()=>response(envelope()),confirm:message=>{confirms.push(message);return false},onChange(){}});
  await refusing.load();refusing.edit('gateway');refusing.updateProvider({displayName:'Keep me'});
  assert.equal(await refusing.beforeNavigate({page:'chat'},{page:'settings',tab:'providers'}),false);
  assert.equal(refusing.getState().draft.displayName,'Keep me');assert.equal(confirms.length,1);
});
`)
}

func TestProviderSettingsUICredentialActionsNeverReplayDraftSecrets(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  for(const action of ['keep','clear']) {
    const f=fixture();await f.load();f.controller.edit('gateway');
    assert.equal(f.state().draft.apiKey||'','','a saved credential is never copied into the form');
    f.controller.updateProvider({credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
    f.controller.updateProvider({credentialAction:action,displayName:'Changed credential action'});
    const reply=f.next(),saving=f.controller.save(),call=f.writes().at(-1);
    assert.equal(call.body.credentialAction,action);
    assert.equal(call.body.apiKey||'','','switching away from Replace must not replay the previous key');
    const saved=provider();saved.credentialConfigured=action!=='clear';
    reply.resolve(response(envelope('revision-2',[saved])));await saving;
    assert.equal(JSON.stringify(f.state()).includes('synthetic-ui-only-key'),false);
    assert.equal(f.state().envelope.providers[0].credentialConfigured,action!=='clear');
  }
});
`)
}

func TestProviderSettingsUIMountEscapesValuesAndSupportsKeyboardForm(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  for(const lang of ['en','zh-CN']) {
    const {c}=browser(lang),listeners={},calls=[],attrs={},pending=[];
    const secret={value:'',disabled:false},keyLabel={hidden:true},indicator={textContent:''},output={innerHTML:''};
    let markup='';
    const root={isConnected:true,dataset:{},
      get innerHTML(){return markup},set innerHTML(value){markup=value;secret.value=''},
      addEventListener:(type,handler)=>{listeners[type]=handler},removeEventListener(){},
      setAttribute:(name,value)=>{attrs[name]=value},
      querySelector:selector=>({'[data-field="apiKey"]':secret,'[data-key-label]':keyLabel,'[data-dirty-indicator]':indicator,'[data-provider-feedback]':output}[selector]||null),
      querySelectorAll:()=>[],
    };
    c.document.getElementById=id=>id==='providerSettingsRoot'?root:null;
    const hostile=provider();
    hostile.displayName='Gateway <img src=x onerror=bad()>';
    hostile.models[0].name='Model <script>bad()</script>';
    hostile.baseUrl='https://gateway.invalid/v1?x=" autofocus onfocus="bad()';
    c.fetch=(url,options={})=>{
      calls.push({url,options,body:options.body&&JSON.parse(options.body)});
      if(!options.method||options.method==='GET')return Promise.resolve(response(envelope('revision-1',[hostile])));
      const item=deferred();pending.push(item);return item.promise;
    };
    assert.match(c.metisProviders.render(),/id="providerSettingsRoot"/);
    c.metisProviders.mount();await tick();
    assert.equal(typeof listeners.click,'function');assert.equal(typeof listeners.submit,'function');
    assert.equal(attrs['aria-busy'],'false');
    assert.match(markup,/Gateway &lt;img src=x onerror=bad\(\)&gt;/);
    assert.doesNotMatch(markup,/<img|<script>/);
    assert.ok(markup.includes(lang==='zh-CN'?'已保存默认':'Saved default'));
    assert.ok(markup.includes(lang==='zh-CN'?'当前会话':'Current conversation'));
    const click=(action,id)=>listeners.click({target:{closest:()=>({disabled:false,dataset:{action,id}})}});
    click('edit','gateway');
    assert.match(markup,/data-provider-form/);
    assert.match(markup,/<button type="submit"[^>]*data-save/,'a native form submit supports Enter without custom keyboard emulation');
    assert.match(markup,/<summary>/,'advanced options use native keyboard-operable disclosure elements');
    assert.match(markup,/type="password"[^>]*data-field="apiKey"[^>]*autocomplete="new-password"/);
    assert.match(markup,/value="https:\/\/gateway.invalid\/v1\?x=&quot; autofocus onfocus=&quot;bad\(\)"/,'attribute values cannot create new DOM attributes');
    assert.match(markup,/Model &lt;script&gt;bad\(\)&lt;\/script&gt;/);
    assert.doesNotMatch(markup,/<img|<script>/);
    assert.ok(markup.includes(lang==='zh-CN'?'完整上下文容量':'Full context capacity'));
    assert.ok(markup.includes(lang==='zh-CN'?'实际生成预算':'Generation output budget'));
    for(const button of markup.matchAll(/<button\b([^>]*)>/g)) assert.match(button[1],/\btype="(?:button|submit)"/,'all actions declare form behavior');
    const input=(field,value)=>listeners.input({target:{dataset:{field},value,matches:()=>false}});
    input('credentialAction','replace');input('apiKey','synthetic-ui-only-key');input('displayName','A keyboard edit');
    const modelInput=(field,value)=>listeners.input({target:{dataset:{modelIndex:'0',modelField:field},value,matches:()=>false}});
    modelInput('supportsReasoning','yes');modelInput('effort-high','deep');
    assert.equal(c.metisProviders.hasUnsavedChanges(),true);
    assert.equal(keyLabel.hidden,false);
    click('add-model');
    assert.equal(secret.value,'synthetic-ui-only-key','a structural repaint retains the unsaved key in the password control');
    assert.equal(markup.includes('synthetic-ui-only-key'),false,'the key is never interpolated into HTML');
    // Remove the newly added blank model before submitting the form.
    listeners.click({target:{closest:()=>({disabled:false,dataset:{action:'remove-model',index:'2'}})}});
    let prevented=0;
    listeners.submit({preventDefault(){prevented++}});
    listeners.submit({preventDefault(){prevented++}});
    assert.equal(prevented,2);
    const writes=calls.filter(call=>call.options.method==='POST');
    assert.equal(writes.length,1,'repeated keyboard submit shares the same pending guard as the Save button');
    assert.equal(writes[0].url,'/api/providers');assert.equal(writes[0].body.expectedRevision,'revision-1');
    assert.equal(writes[0].body.displayName,'A keyboard edit');
    assert.equal(writes[0].body.apiKey,'synthetic-ui-only-key');
    assert.equal(writes[0].body.models[0].supportsReasoning,true,'the capability select must become a boolean in the saved DTO');
    assert.equal(writes[0].body.models[0].reasoningEfforts.high,'deep','the wire effort editor must update the nested mapping');
    pending[0].resolve(response(envelope('revision-2',[provider()])));await tick();
    assert.equal(c.metisProviders.hasUnsavedChanges(),false);
    assert.equal(attrs['aria-busy'],'false');
    c.metisProviders.dispose();
  }
  assert.match(sources['index.html'],/providers\.js\?v=__BUILD__/,'the tested module is included in the shipped page');
  assert.match(sources['index.html'],/providers\.css\?v=__BUILD__/);
});
`)
}

func TestProviderSettingsUIApplyReservesPendingBeforeSessionSelection(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const {c}=browser(),ready=deferred(),reply=deferred(),calls=[];
  let selections=0;
  const controller=c.MetisProviderSettings.createController({
    fetch:(url,options={})=>{
      calls.push({url,options,body:options.body&&JSON.parse(options.body)});
      return options.method==='POST'?reply.promise:Promise.resolve(response(envelope()));
    },sessionTarget:()=>{selections++;return ready.promise},onChange(){},
  });
  await controller.load();
  const first=controller.apply('gateway','model-b'),second=controller.apply('gateway','model-b');
  ready.resolve({isCurrent:()=>true});await tick();
  assert.equal(selections,1,'the first Apply must reserve the pending guard while preparing its session target');
  const writes=calls.filter(call=>call.options.method==='POST');
  assert.equal(writes.length,1,'rapid Apply clicks must not both activate a route after the same asynchronous session check');
  assert.equal(writes[0].url,'/api/models');
  reply.resolve(response({provider:'gateway',model:'model-b'}));
  assert.equal(await first,true);assert.equal(await second,false);
  assert.equal(Boolean(controller.getState().pending),false);
  for(const target of [async()=>false,async()=>{throw Error('session target unavailable')},async()=>({isCurrent:()=>false})]) {
    let posts=0;
    const rejected=c.MetisProviderSettings.createController({
      fetch:async(_url,options={})=>{if(options.method==='POST')posts++;return response(envelope())},
      sessionTarget:target,onChange(){},
    });
    await rejected.load();
    assert.equal(await rejected.apply('gateway','model-b'),false,'cancelled, failed or stale session preparation must not apply a route');
    assert.equal(posts,0);
    assert.equal(Boolean(rejected.getState().pending),false,'every early return releases the Apply guard');
  }
});
`)
}

func TestProviderSettingsUICatalogSwitchingPreservesManualConnection(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const presets=[
    {id:'alpha',name:'Alpha AI',api:'https://alpha.invalid/v1',transport:'openai_chat',models:[{id:'alpha-model',name:'Alpha model',contextWindow:272000,maxContextWindow:1000000,maxOutputTokens:64000,supportsVision:true,supportsReasoning:true,reasoningEfforts:{high:'high'}}]},
    {id:'beta',name:'Beta AI',api:'https://beta.invalid/v1',transport:'anthropic_messages',models:[{id:'beta-model',name:'Beta model'}]},
  ];
  const f=fixture('en',presets);await f.load();
  assert.equal(f.controller.create(),true);await tick();
  assert.equal(f.state().catalog.length,2);
  f.controller.selectCatalog('alpha');
  assert.equal(f.state().draft.id,'alpha');assert.equal(f.state().draft.displayName,'Alpha AI');
  assert.equal(f.state().draft.baseUrl,'https://alpha.invalid/v1');assert.equal(f.state().draft.transport,'openai_chat');
  assert.equal(f.state().draft.models.length,0,'choosing a provider preset does not silently import its model catalog');
  f.controller.selectCatalog('beta');
  assert.equal(f.state().draft.id,'beta','an untouched preset ID follows the new preset');
  assert.equal(f.state().draft.displayName,'Beta AI');assert.equal(f.state().draft.baseUrl,'https://beta.invalid/v1');
  assert.equal(f.state().draft.transport,'anthropic_messages');
  f.controller.updateProvider({id:'my-gateway',displayName:'Hand tuned gateway',baseUrl:'https://handwritten.invalid/endpoint',transport:'openai_responses'});
  f.controller.selectCatalog('alpha');
  assert.equal(f.state().draft.catalogProvider,'alpha');
  assert.equal(f.state().draft.id,'my-gateway');assert.equal(f.state().draft.displayName,'Hand tuned gateway');
  assert.equal(f.state().draft.baseUrl,'https://handwritten.invalid/endpoint');assert.equal(f.state().draft.transport,'openai_responses','switching presets cannot overwrite a protocol explicitly edited by the user');
  f.controller.importModels(['alpha-model']);
  assert.deepEqual(clone(f.state().draft.models),[{id:'alpha-model',name:'Alpha model'}],'catalog import must not turn a client profile, vision flag or effort recommendation into persistent capability overrides');
  assert.equal(f.calls.some(call=>call.url==='/api/providers/models'),false,'using cached presets never initiates endpoint discovery');
  assert.equal(f.calls.some(call=>call.url==='/api/providers'&&call.options.method==='POST'),false,'presets and imports remain local until Save');
});
`)
}

func TestProviderSettingsUIGeminiPresetDoesNotPersistUnsupportedMapping(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const preset={id:'google',name:'Google Gemini',api:'https://generativelanguage.googleapis.com',transport:'gemini_native',models:[{id:'gemini-preview',name:'Gemini preview',contextWindow:1000000,maxOutputTokens:65536,supportsVision:true}]};
  const f=fixture('en',[preset]);await f.load();f.controller.create();await tick();
  assert.equal(f.controller.selectCatalog('google'),true);
  assert.equal(f.state().connectionPreset,'google','the connection preset remains selected in this editor');
  assert.equal(f.state().draft.baseUrl,preset.api);assert.equal(f.state().draft.transport,'gemini_native');
  assert.equal(f.state().draft.catalogProvider,'','native Gemini does not consume runtime catalog mappings');
  f.controller.importModels(['gemini-preview']);
  assert.deepEqual(clone(f.state().draft.models),[{id:'gemini-preview',name:'Gemini preview'}]);
  const saved={...provider('google'),transport:'gemini_native',baseUrl:preset.api,model:'gemini-preview',models:[{id:'gemini-preview',name:'Gemini preview'}],catalogProvider:''};
  await f.answer(envelope('revision-gemini',[saved]),()=>f.controller.save());
  const write=f.writes()[0];assert.equal(write.body.catalogProvider,'');assert.equal(write.body.baseUrl,preset.api);
  assert.deepEqual(write.body.models,[{id:'gemini-preview',name:'Gemini preview'}]);
});
`)
}

func TestProviderSettingsUIReasoningInheritanceClearsWireMapping(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  for(const choice of [false,null]) {
    const saved=provider();saved.models[0].supportsReasoning=true;saved.models[0].reasoningEfforts={low:'fast',medium:'balanced',high:'deep'};
    const f=fixture();await f.load(envelope('revision-1',[saved]));f.controller.edit('gateway');
    f.controller.updateModel(0,{supportsReasoning:choice});
    assert.equal(f.state().draft.models[0].reasoningEfforts,undefined,'turning reasoning off or inheriting cannot retain a stale endpoint effort map');
    f.controller.updateModel(0,{id:'renamed-model'});
    assert.equal(f.state().draft.model,'renamed-model','renaming the selected model keeps the provider default attached to the same row');
    const reply=f.next(),saving=f.controller.save(),call=f.writes().at(-1);
    assert.equal(call.body.model,'renamed-model');
    assert.equal('reasoningEfforts' in call.body.models[0],false);
    if(choice===false)assert.equal(call.body.models[0].supportsReasoning,false,'an explicit false capability survives JSON serialization');
    else assert.equal('supportsReasoning' in call.body.models[0],false,'Auto removes the override rather than saving a false capability');
    assert.equal(call.body.models[0].supportsVision,false);
    reply.resolve(response(envelope('revision-2',[saved])));await saving;
  }
});
`)
}

func TestProviderSettingsUIOAuthModelsEditableWithoutCredentialEditor(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const {c}=browser('zh-CN'),listeners={},calls=[];
  const oauth=provider('openai-codex');Object.assign(oauth,{custom:false,credentialKind:'oauth',editable:false,readOnly:false,transport:'openai_codex_responses',baseUrl:'https://oauth.invalid/fixed'});
  for(const model of oauth.models)delete model.outputBudget;
  let markup='';
  const root={isConnected:true,innerHTML:'',dataset:{},
    get innerHTML(){return markup},set innerHTML(value){markup=value},
    addEventListener:(type,handler)=>{listeners[type]=handler},setAttribute(){},querySelector:()=>null,querySelectorAll:()=>[],
  };
  c.document.getElementById=id=>id==='providerSettingsRoot'?root:null;
  c.fetch=async(url,options={})=>{
    calls.push({url,options,body:options.body&&JSON.parse(options.body)});
    return response(url==='/api/providers/models'?{models:[{id:'new-oauth-model'}]}:envelope('revision-1',[oauth]));
  };
  c.metisProviders.mount();await tick();
  const click=action=>listeners.click({target:{closest:()=>({disabled:false,dataset:{action,id:'openai-codex'}})}});
  click('edit');
  // Resolve inherited fieldset disabling in the actual rendered HTML. This
  // catches a model editor accidentally nested in the OAuth connection lock.
  const disabled=attribute=>{
    const fieldsets=[];
    for(const match of markup.matchAll(/<\/?fieldset\b[^>]*>|<(?:input|select)\b[^>]*>/g)) {
      const token=match[0];
      if(/^<\/fieldset/.test(token)){fieldsets.pop();continue}
      if(/^<fieldset/.test(token)){fieldsets.push(/\bdisabled(?:\s|>|=)/.test(token));continue}
      if(token.includes(attribute))return fieldsets.some(Boolean)||/\bdisabled(?:\s|>|=)/.test(token);
    }
    throw Error('missing control: '+attribute);
  };
  assert.equal(disabled('data-field="baseUrl"'),true,'OAuth endpoint stays locked');
  assert.equal(disabled('data-field="transport"'),true,'OAuth protocol stays locked');
  assert.equal(disabled('data-model-field="id"'),false,'OAuth model identities remain editable');
  assert.equal(disabled('data-model-field="contextWindow"'),false);
  assert.equal(disabled('data-model-field="outputBudget"'),true,'OAuth output sizing remains managed by its endpoint');
  assert.doesNotMatch(markup,/type="password"|data-field="credentialAction"/);
  assert.match(markup,/<button type="submit"[^>]*data-save/);
  const input=(field,value)=>listeners.input({target:{dataset:{modelIndex:'0',modelField:field},value,matches:()=>false}});
  input('name','Local OAuth model name');input('contextWindow','1000000');
  click('discover');await tick();
  assert.deepEqual(calls.find(call=>call.url==='/api/providers/models').body,{id:'openai-codex'},'OAuth discovery never submits a draft endpoint or API key');
  listeners.submit({preventDefault(){}});await tick();
  const saved=calls.find(call=>call.url==='/api/providers'&&call.options.method==='POST');
  assert.ok(saved,'OAuth model changes can be saved');
  assert.equal(saved.body.models[0].name,'Local OAuth model name');
  assert.equal(saved.body.models[0].contextWindow,1000000);
  assert.equal(saved.body.credentialAction,'keep');assert.equal(saved.body.apiKey||'','');
  c.metisProviders.dispose();
});
`)
}

func TestProviderSettingsUINavigationGuardBlocksEntryAndHistory(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const {c}=browser();vm.runInContext(sources['navigation.js'],c);
  const enters=[],starts=[],changes=[],leaves=[],guardCalls=[];
  let allowed=false;
  const nav=c.createMetisNavigation({initialRoute:{page:'settings',tab:'providers'},
    beforeNavigate:(next,previous)=>{guardCalls.push({next:clone(next),previous:clone(previous)});return allowed},
    start:route=>starts.push(clone(route)),changed:route=>changes.push(clone(route)),
  });
  nav.register('settings',{enter:route=>{enters.push(clone(route));return true},leave:route=>leaves.push(clone(route))});
  nav.register('session',{enter:route=>{enters.push(clone(route));return true}});
  const original=clone(nav.snapshot());
  assert.equal(await nav.navigate({page:'session',sessionId:'B'}),false);
  assert.deepEqual(clone(nav.snapshot()),original,'a refused leave cannot append history or move its cursor');
  assert.equal(nav.pending(),null);assert.equal(starts.length,0);assert.equal(enters.length,0);assert.equal(changes.length,0);assert.equal(leaves.length,0);
  assert.deepEqual(guardCalls[0],{next:{page:'session',sessionId:'B',view:'chat'},previous:{page:'settings',tab:'providers'}});
  allowed=true;
  assert.equal(await nav.navigate({page:'session',sessionId:'B'}),true);
  assert.equal(enters.length,1);assert.equal(leaves.length,1);assert.equal(nav.current().sessionId,'B');
  const committed=clone(nav.snapshot()),beforeEntries=enters.length;
  allowed=false;assert.equal(await nav.back(),false);
  assert.deepEqual(clone(nav.snapshot()),committed,'Back uses the same guard without consuming the history entry');
  assert.equal(enters.length,beforeEntries);
  allowed=true;assert.equal(await nav.back(),true);assert.equal(nav.current().page,'settings');
  assert.equal(await nav.forward(),true);assert.equal(nav.current().sessionId,'B');
  const plain=c.createMetisNavigation({initialRoute:{page:'settings',tab:'general'}});
  let plainEntries=0;
  for(const page of ['settings','session'])plain.register(page,{enter:()=>{plainEntries++;return true}});
  assert.equal(await plain.navigate({page:'session',sessionId:'A'}),true);
  assert.equal(await plain.back(),true);assert.equal(await plain.forward(),true);
  assert.equal(plainEntries,3,'pages without an optional guard keep their previous navigation behavior');
});
`)
}

func TestProviderSettingsUIListLoadsCannotRewindNewerState(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load();
  const older=f.next(),oldLoad=f.controller.load();
  const newer=f.next(),newLoad=f.controller.load();
  const latest=provider();latest.displayName='Newest list';
  newer.resolve(response(envelope('revision-3',[latest])));assert.equal(await newLoad,true);
  older.resolve(response(envelope('revision-2',[provider()])));assert.equal(await oldLoad,false);
  assert.equal(f.state().envelope.revision,'revision-3','the last requested list owns the snapshot even if the previous request resolves last');
  assert.equal(f.state().envelope.providers[0].displayName,'Newest list');
  assert.equal(f.state().loading,false);

  const beforeEdit=f.next(),loadingBeforeEdit=f.controller.load();
  assert.equal(f.controller.edit('gateway'),false,'editing is blocked while its list refresh is still pending');
  assert.equal(f.controller.cancel(),true,'leaving the old list invalidates that request');
  assert.equal(f.controller.edit('gateway'),true);
  f.controller.updateProvider({displayName:'Unsent editor draft'});
  beforeEdit.resolve(response(envelope('obsolete-after-edit',[provider()])));assert.equal(await loadingBeforeEdit,false);
  assert.equal(f.state().view,'editor');assert.equal(f.state().draft.displayName,'Unsent editor draft');
  assert.equal(f.state().envelope.revision,'revision-3','an earlier list cannot change the revision attached to a new editor');

  assert.equal(f.controller.cancel(),true);
  const beforeMutation=f.next(),loadingBeforeMutation=f.controller.load();
  const defaultReply=f.next(),changingDefault=f.controller.setDefault('gateway','model-b');
  const committed=envelope('revision-4',[latest]);committed.savedDefault={provider:'gateway',model:'model-b'};
  defaultReply.resolve(response(committed));assert.equal(await changingDefault,true);
  beforeMutation.resolve(response(envelope('obsolete-after-mutation',[provider()])));assert.equal(await loadingBeforeMutation,false);
  assert.deepEqual(clone(f.state().envelope),committed,'a successful mutation invalidates already pending reads of the old configuration');
  assert.equal(f.state().loading,false);assert.equal(Boolean(f.state().pending),false);
});
`)
}

func TestProviderSettingsUIExplicitCatalogRefreshPreservesManualDraft(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load();f.controller.create();await tick();
  assert.deepEqual(clone(f.state().catalog),[]);
  assert.equal(f.calls.some(call=>call.url==='/api/providers/catalog/refresh'),false,'opening the editor only reads the local catalog');
  f.controller.updateProvider({id:'manual-gateway',displayName:'Manual gateway',baseUrl:'https://manual.invalid/v1',transport:'openai_responses',credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
  f.controller.addModel({id:'manual-model',name:'Manual model',contextWindow:1000000,outputBudget:12000,supportsVision:false});
  const draft=clone(f.state().draft),original=clone(f.state().original),dirty=f.state().dirty,snapshot=clone(f.state().envelope);
  const reply=f.next(),refreshing=f.controller.refreshCatalog();
  assert.equal(f.state().catalogLoading,true);
  const count=f.calls.length;
  assert.equal(await f.controller.refreshCatalog(),false,'a second refresh cannot duplicate the in-flight request');
  assert.equal(f.calls.length,count);
  const request=f.calls.at(-1);
  assert.equal(request.url,'/api/providers/catalog/refresh');assert.equal(request.options.method,'POST');
  assert.equal(request.options.body,undefined,'catalog refresh sends no draft or credential body');
  assert.equal(JSON.stringify(request.options).includes('synthetic-ui-only-key'),false);
  const catalog=[{id:'new-vendor',name:'New vendor',api:'https://new.invalid/v1',transport:'anthropic_messages',models:[{id:'new-model',name:'New model'}]}];
  reply.resolve(response({providers:catalog}));assert.equal(await refreshing,true);
  assert.deepEqual(clone(f.state().catalog),catalog);
  assert.deepEqual(clone(f.state().draft),draft,'a refreshed preset list cannot select a preset or change manual connection/model/credential fields');
  assert.deepEqual(clone(f.state().original),original);assert.equal(f.state().dirty,dirty);
  assert.deepEqual(clone(f.state().envelope),snapshot);
  assert.equal(f.state().catalogLoading,false);assert.equal(f.state().catalogError,'');
});
`)
}

func TestProviderSettingsUICatalogRefreshFailureKeepsCacheAndCanRetry(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const cached=[{id:'cached',name:'Cached vendor',api:'https://cached.invalid/v1',models:[{id:'cached-model'}]}];
  const f=fixture('en',cached);await f.load();f.controller.edit('gateway');await tick();
  f.controller.updateProvider({displayName:'Keep this unsaved name',credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
  const draft=clone(f.state().draft),original=clone(f.state().original),dirty=f.state().dirty;
  const failed=f.next(),first=f.controller.refreshCatalog();
  failed.resolve(response({error:'catalog offline'},502));assert.equal(await first,false);
  assert.deepEqual(clone(f.state().catalog),cached,'a failed refresh keeps the usable local catalog');
  assert.deepEqual(clone(f.state().draft),draft);assert.deepEqual(clone(f.state().original),original);assert.equal(f.state().dirty,dirty);
  assert.match(f.state().catalogError,/catalog offline/,'the catalog failure is visible separately from form validation');
  assert.equal(f.state().catalogLoading,false);
  const retry=f.next(),second=f.controller.refreshCatalog();
  const updated=[{id:'fresh',name:'Fresh vendor',models:[]}];
  retry.resolve(response({providers:updated}));assert.equal(await second,true);
  assert.deepEqual(clone(f.state().catalog),updated);assert.equal(f.state().catalogError,'');
  assert.equal(f.state().catalogLoading,false);assert.deepEqual(clone(f.state().draft),draft);
  assert.equal(f.calls.filter(call=>call.url==='/api/providers/catalog/refresh').length,2);
});
`)
}

func TestProviderSettingsUICatalogResponsesBelongToCurrentEditor(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const cached=[{id:'cached',name:'Local vendor',models:[]}];
  for(const transition of ['cancel-and-edit','dispose']) {
    const f=fixture('en',cached);await f.load();f.controller.create();await tick();
    f.controller.updateProvider({id:'temporary',credentialAction:'replace',apiKey:'synthetic-ui-only-key'});
    const late=f.next(),refreshing=f.controller.refreshCatalog();
    if(transition==='cancel-and-edit') {
      assert.equal(f.controller.cancel(),true);assert.equal(f.controller.edit('gateway'),true);await tick();
    }else f.controller.dispose();
    const stateBefore=clone(f.state());
    late.resolve(response({providers:[{id:'wrong-editor',name:'Must be ignored'}]}));
    assert.equal(await refreshing,false);
    assert.deepEqual(clone(f.state()),stateBefore,'a completed refresh cannot mutate an editor that replaced its owner');
    assert.equal(JSON.stringify(f.state()).includes('synthetic-ui-only-key'),false);
    if(transition==='cancel-and-edit')assert.equal(f.state().catalogLoading,false);
  }
  // A delayed local read is older than the explicit refresh requested after it.
  const {c}=browser(),local=deferred(),remote=deferred();
  const controller=c.MetisProviderSettings.createController({
    fetch:(url,options={})=>url==='/api/providers/catalog'?local.promise:url==='/api/providers/catalog/refresh'?remote.promise:Promise.resolve(response(envelope())),
    confirm:()=>true,onChange(){},
  });
  await controller.load();controller.create();
  const refreshing=controller.refreshCatalog(),fresh=[{id:'fresh-explicit-refresh',models:[]}];
  remote.resolve(response({providers:fresh}));assert.equal(await refreshing,true);
  local.resolve(response({providers:cached}));await tick();
  assert.deepEqual(clone(controller.getState().catalog),fresh,'the older local read cannot replace the explicit refresh result within the same editor');
  assert.equal(controller.getState().catalogLoading,false);
});
`)
}

func TestProviderSettingsUICreateCannotOverwriteExistingProviderIdentity(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const f=fixture();await f.load(envelope('revision-1',[provider(),provider('openai'),provider('gemini'),provider('anthropic')]));
  const original=clone(f.state().envelope);
  for(const id of ['gateway','openai','google','anthropic-claudeai']) {
    assert.equal(f.controller.create(),true);
    f.controller.updateProvider({id,baseUrl:'https://replacement.invalid/v1',credentialAction:'replace',apiKey:'synthetic-new-key'});
    f.controller.addModel({id:'replacement-model'});
    assert.equal(await f.controller.save(),false,'create must reject an existing provider namespace before HTTP: '+id);
    assert.equal(f.writes().length,0);
    assert.match(f.state().error,/already exists/i);
    assert.deepEqual(clone(f.state().envelope),original,'duplicate creation preserves existing models and credentials');
  }
  f.controller.create();f.controller.updateProvider({id:'new-gateway',baseUrl:'https://new.invalid/v1'});f.controller.addModel({id:'new-model'});
  const reply=f.next(),saving=f.controller.save();
  assert.equal(f.writes().at(-1).body.intent,'create');
  reply.resolve(response(envelope('revision-2',[...original.providers,provider('new-gateway')]),201));assert.equal(await saving,true);
  f.controller.edit('gateway');
  assert.equal(f.controller.updateProvider({id:'renamed-gateway',displayName:'Must not apply partial patch'}),false);
  assert.equal(f.state().draft.id,'gateway');assert.equal(f.state().draft.displayName,'Gateway');
  f.controller.updateProvider({displayName:'Updated gateway'});
  const updated=f.next(),updating=f.controller.save();
  assert.equal(f.writes().at(-1).body.intent,'update');assert.equal(f.writes().at(-1).body.id,'gateway');
  updated.resolve(response(envelope('revision-3',original.providers)));assert.equal(await updating,true);
});
`)
}

func TestProviderSettingsUIModelDisclosuresKeepIndependentStableState(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  const {c}=browser(),listeners={},catalog=deferred(),scrolls=[];let markup='',details=[],scrollPosition=0;
  const root={isConnected:true,dataset:{},scrollIntoView:options=>{scrolls.push(clone(options));scrollPosition=0;},
    get innerHTML(){return markup},set innerHTML(value){markup=value;details=[...value.matchAll(/<details\b([^>]*)>/g)].map(match=>({className:match[1].match(/class="([^"]*)"/)?.[1]||'',dataset:{modelDisclosure:match[1].match(/data-model-disclosure="([^"]*)"/)?.[1]},open:/\sopen(?:\s|$)/.test(match[1])}));},
    addEventListener:(type,handler)=>listeners[type]=handler,removeEventListener(){},setAttribute(){},contains(){return false},querySelector(){return null},
    querySelectorAll:selector=>selector==='details'?details:selector==='details[open]'?details.filter(detail=>detail.open):[],
  };
  c.document.getElementById=id=>id==='providerSettingsRoot'?root:null;
  c.fetch=url=>url==='/api/providers/catalog'?catalog.promise:Promise.resolve(response(envelope()));
  c.metisProviders.mount();await tick();
  assert.equal(scrolls.length,1,'first mount aligns the list once');scrollPosition=850;
  const click=(action,index)=>listeners.click({target:{closest:()=>({disabled:false,dataset:{action,id:'gateway',index}})}});
  click('edit');
  assert.equal(scrollPosition,0,'entering the editor from a lower provider card shows its title and Back button');
  assert.equal(scrolls.length,2);scrollPosition=420;
  const models=()=>details.filter(detail=>detail.className==='ps-model-advanced');
  assert.equal(models().length,2);const [a,b]=models();
  assert.ok(a.dataset.modelDisclosure&&b.dataset.modelDisclosure&&a.dataset.modelDisclosure!==b.dataset.modelDisclosure,'each model needs its own disclosure identity');
  a.open=true;b.open=false;
  catalog.resolve(response({providers:[]}));await tick();
  assert.equal(scrollPosition,420,'an async catalog repaint preserves editor position');assert.equal(scrolls.length,2);
  assert.deepEqual(models().map(detail=>detail.open),[true,false],'an async catalog repaint must not expand every model');
  click('add-model');
  assert.equal(scrollPosition,420,'adding an advanced model row does not jump to the top');assert.equal(scrolls.length,2);
  assert.deepEqual(models().map(detail=>detail.open),[true,false,false],'a new model starts collapsed without changing others');
  assert.equal(models()[0].dataset.modelDisclosure,a.dataset.modelDisclosure);assert.equal(models()[1].dataset.modelDisclosure,b.dataset.modelDisclosure);
  listeners.input({target:{dataset:{modelIndex:'0',modelField:'id'},value:'renamed-model-a',matches:()=>false}});
  assert.equal(models()[0].dataset.modelDisclosure,a.dataset.modelDisclosure,'editing the wire model ID does not replace its disclosure identity');
  assert.deepEqual(models().map(detail=>detail.open),[true,false,false]);
  click('remove-model','0');
  assert.equal(models()[0].dataset.modelDisclosure,b.dataset.modelDisclosure,'removing a preceding row preserves the next model identity');
  assert.deepEqual(models().map(detail=>detail.open),[false,false],'the removed row cannot transfer its open state to the next row');
  click('back');
  assert.equal(scrollPosition,0,'returning to the list aligns its heading');assert.equal(scrolls.length,3);
  c.metisProviders.dispose();
});
`)
}

func TestProviderSettingsUIEmptyListCanAddAndRemoveOpenAIPreset(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  for(const lang of ['en','zh-CN']) {
    const {c}=browser(lang),listeners={},calls=[],confirmations=[];let markup='',items=[],revision=1;
    const root={isConnected:true,dataset:{},get innerHTML(){return markup},set innerHTML(value){markup=value},
      addEventListener:(type,handler)=>listeners[type]=handler,removeEventListener(){},setAttribute(){},contains(){return false},querySelector(){return null},querySelectorAll(){return []}};
    c.document.getElementById=id=>id==='providerSettingsRoot'?root:null;
    c.confirm=message=>{confirmations.push(message);return true};
    const snapshot=()=>({revision:'revision-'+revision,providers:items,savedDefault:{},current:{}});
    c.fetch=async(url,options={})=>{
      const body=options.body&&JSON.parse(options.body);calls.push({url,options,body});
      if(url==='/api/providers/catalog')return response({providers:[{id:'openai',name:'OpenAI',transport:'openai_chat',api:'https://api.openai.com/v1',models:[{id:'ui-openai-model',name:'OpenAI UI model'}]}]});
      assert.equal(url,'/api/providers');
      if(options.method==='POST'){
        assert.equal(body.intent,'create');assert.equal(body.id,'openai');assert.equal(body.credentialAction,'keep');assert.equal(body.apiKey,undefined);
        assert.deepEqual(body.models,[{id:'ui-openai-model',name:'OpenAI UI model'}]);
        items=[{...provider('openai'),...body,custom:false,removable:true,readOnly:false,credentialConfigured:false,credentialSource:'none'}];revision++;
        return response(snapshot(),201);
      }
      if(options.method==='DELETE'){
        assert.equal(body.id,'openai');assert.equal(body.expectedRevision,'revision-2');items=[];revision++;return response(snapshot());
      }
      return response(snapshot());
    };
    c.metisProviders.mount();await tick();
    assert.match(markup,lang==='zh-CN'?/还没有提供商/:/No providers yet/);
    assert.match(markup,/data-action="new"/,'an empty installation still offers Add provider');
    assert.doesNotMatch(markup,/ps-default-summary|data-provider-search-value=/,'an empty installation has no implicit provider or route summary');
    const click=(action,id)=>listeners.click({target:{closest:()=>({disabled:false,dataset:{action,id}})}});
    click('new');await tick();
    listeners.change({target:{value:'openai',dataset:{},matches:selector=>selector==='[data-catalog]'}});
    listeners.change({target:{checked:true,dataset:{importId:'ui-openai-model'},matches:()=>false}});
    click('import');listeners.submit({preventDefault(){}});await tick();
    assert.equal(items.length,1);
    assert.match(markup,/data-provider-search-value="openai /,'an explicitly added connection stays listed without a key');
    const remove=markup.match(/<button\b[^>]*data-action="delete"[^>]*data-id="openai"[^>]*>/)?.[0];
    assert.ok(remove,'an added provider has a removal control independently of its legacy kind');assert.doesNotMatch(remove,/\bdisabled\b/);
    click('delete','openai');await tick();
    assert.equal(confirmations.length,1);assert.doesNotMatch(confirmations[0],/custom|自定义/);
    assert.equal(calls.filter(call=>call.options.method==='DELETE').length,1);
    assert.match(markup,lang==='zh-CN'?/还没有提供商/:/No providers yet/);
    assert.doesNotMatch(markup,/data-provider-search-value=/);
    await c.metisProviders.refresh();await tick();
    assert.doesNotMatch(markup,/data-provider-search-value=/,'refresh cannot reconstruct undeclared provider placeholders');
    c.metisProviders.dispose();
  }
});
`)
}

func TestProviderSettingsUIRemovalUsesOwnershipAndDefaultInsteadOfKind(t *testing.T) {
	runProviderSettingsUI(t, `
finish(async()=>{
  for(const lang of ['en','zh-CN']) {
    const {c}=browser(lang),listeners={},calls=[],confirmations=[];let markup='';
    const normal={...provider('openai'),custom:false,removable:true};
    const defaultItem={...provider('default-route'),custom:false,removable:true,default:true};
    const environment={...provider('environment-route'),removable:false,removeReason:'Controlled by <ENV>'};
    const workspace={...provider('workspace-route'),removable:true,readOnly:true,readOnlyReason:'Controlled by workspace'};
    const value={revision:'revision-1',providers:[normal,defaultItem,environment,workspace],savedDefault:{provider:'default-route',model:'model-a'},current:{}};
    const root={isConnected:true,dataset:{},get innerHTML(){return markup},set innerHTML(value){markup=value},
      addEventListener:(type,handler)=>listeners[type]=handler,removeEventListener(){},setAttribute(){},contains(){return false},querySelector(){return null},querySelectorAll(){return []}};
    c.document.getElementById=id=>id==='providerSettingsRoot'?root:null;
    c.confirm=message=>{confirmations.push(message);return true};
    c.fetch=async(url,options={})=>{calls.push({url,options});return response(value)};
    c.metisProviders.mount();await tick();
    const button=id=>markup.match(new RegExp('<button\\b[^>]*data-action="delete"[^>]*data-id="'+id+'"[^>]*>'))?.[0];
    assert.doesNotMatch(button('openai'),/\bdisabled\b/,'a noncustom owned route is removable');
    assert.match(button('default-route'),/\bdisabled\b/);assert.match(button('default-route'),/title="[^"]+"/);
    assert.match(button('environment-route'),/\bdisabled\b/);assert.match(button('environment-route'),/Controlled by &lt;ENV&gt;/,'removal reasons must be escaped');
    assert.match(button('workspace-route'),/\bdisabled\b/);assert.match(button('workspace-route'),/Controlled by workspace/);
    // Even if a stale DOM or direct event bypasses the disabled control, the
    // state controller must not confirm or dispatch a forbidden removal.
    for(const id of ['default-route','environment-route','workspace-route','missing-route']) {
      listeners.click({target:{closest:()=>({disabled:false,dataset:{action:'delete',id}})}});await tick();
    }
    assert.equal(confirmations.length,0);assert.equal(calls.some(call=>call.options.method==='DELETE'),false);
    c.metisProviders.dispose();
  }
});
`)
}
