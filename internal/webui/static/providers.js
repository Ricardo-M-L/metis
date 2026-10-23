(function () {
  'use strict';
  const copy = value => JSON.parse(JSON.stringify(value));
  const text = (en, zh) => typeof document !== 'undefined' && document.documentElement.lang === 'zh-CN' ? zh : en;
  const supportsCatalogMapping = transport => ['openai_chat','openai_responses','anthropic_messages'].includes(transport);
  const modelDisclosureKeys = new WeakMap();
  let nextModelDisclosureKey = 0;
  const modelDisclosureKey = model => {if(!modelDisclosureKeys.has(model))modelDisclosureKeys.set(model,'model-'+(++nextModelDisclosureKey));return modelDisclosureKeys.get(model);};
  const providerIdentity = id => ({google:'gemini','anthropic-claudeai':'anthropic'}[String(id).trim().toLowerCase()] || String(id).trim().toLowerCase());
  const html = value => String(value == null ? '' : value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  function removalReason(provider, defaultProvider) {
    if (!provider) return text('This provider is no longer available. Reload the list.', '此提供商已不存在，请重新加载列表。');
    if (provider.default || provider.id===defaultProvider) return text('Choose another default provider before removing this one.', '请先选择其他默认提供商，再移除此项。');
    if (provider.readOnly) return provider.removeReason || provider.readOnlyReason || text('This provider is managed outside model settings.', '此提供商由模型设置以外的配置管理。');
    const removable=typeof provider.removable==='boolean'?provider.removable:provider.custom===true;
    return removable?'':provider.removeReason || text('This provider cannot be removed from model settings.', '此提供商暂时无法从模型设置中移除。');
  }
  function createController(options = {}) {
    const fetcher = options.fetch || ((...args) => fetch(...args));
    const confirm = options.confirm || (message => window.confirm(message));
    const state = { envelope: {revision:'',providers:[],savedDefault:{},current:{}}, view:'list', intent:'', draft:null, original:null,
      dirty:false, pending:'', error:'', message:'', loading:false, discovery:[], discovering:false, discoveryError:'', catalog:[], catalogLoading:false, catalogRefreshing:false, catalogError:'', connectionPreset:'' };
    let epoch = 0, loadGeneration = 0, catalogGeneration = 0, discoveryEpoch = 0, discoveryAbort = null, disposed = false, catalogDefaults = null, editIdentity = null;
    const notify = reason => { if (!disposed) options.onChange?.(state, reason); };
    const fingerprint = draft => JSON.stringify(draft);
    const provider = id => state.envelope.providers.find(item => item.id === id);
    const clearSecret = () => { if (state.draft) state.draft.apiKey = ''; };
    const invalidateDiscovery = () => { discoveryEpoch++; discoveryAbort?.abort(); discoveryAbort=null; state.discovering=false; state.discovery=[]; state.discoveryError=''; };
    const errorMessage = error => {
      let message = error?.message || text('Request failed. Please retry.', '请求失败，请重试。');
      if (state.draft?.apiKey) message = message.split(state.draft.apiKey).join('[redacted]');
      return message;
    };
    async function request(path, method, body, signal) {
      const response = await fetcher(path, {method:method || 'GET', ...(body ? {headers:{'Content-Type':'application/json'},body:JSON.stringify(body)} : {}), ...(signal ? {signal} : {})});
      const result = await response.json();
      if (!response.ok) {
        const error = new Error(response.status === 409 ? text('Configuration changed or this action is unavailable. Reload the list before retrying; your draft is preserved.', '配置已变化或当前操作不可用。请重新加载后重试；草稿已保留。') + (result.error ? ' '+result.error : '') : result.error || text('Request failed', '请求失败'));
        error.status = response.status; throw error;
      }
      return result;
    }
    function install(envelope) {
      if (!envelope || typeof envelope.revision !== 'string' || !Array.isArray(envelope.providers)) throw Error(text('Invalid provider settings response', '模型设置响应无效'));
      state.envelope = copy(envelope);
    }
    function changed(reason='draft') { state.dirty = fingerprint(state.draft) !== fingerprint(state.original); state.error=''; notify(reason); }
    function canLeave() {
      if (state.pending) { state.error=text('Wait for the current operation to finish.', '请等待当前操作完成。'); notify('feedback'); return false; }
      return !state.dirty || confirm(text('Discard unsaved model settings?', '放弃尚未保存的模型设置？'));
    }
    function leave() { epoch++; loadGeneration++; catalogGeneration++; state.loading=false;state.catalogLoading=false;state.catalogRefreshing=false;state.catalogError=''; invalidateDiscovery(); clearSecret(); state.draft=null; state.original=null; state.dirty=false; state.view='list'; state.intent=''; editIdentity=null; catalogDefaults=null; state.connectionPreset=''; }
    function loadCatalog(owner) {
      const generation=++catalogGeneration;state.catalogLoading=true;state.catalogError='';notify('catalog-loading');
      request('/api/providers/catalog').then(result=>{if(!disposed&&epoch===owner&&generation===catalogGeneration)state.catalog=Array.isArray(result.providers)?result.providers:[];})
        .catch(error=>{if(!disposed&&epoch===owner&&generation===catalogGeneration)state.catalogError=errorMessage(error);})
        .finally(()=>{if(!disposed&&epoch===owner&&generation===catalogGeneration){state.catalogLoading=false;notify('catalog');}});
    }
    async function mutation(kind, path, method, body) {
      if (state.pending) return false;
      const owner=epoch;
      state.pending=kind; state.error=''; state.message=''; notify('pending');
      try {
        const result=await request(path,method,body);
        if (disposed || epoch!==owner) return false;
        install(result); leave(); state.message=text('Settings saved.', '设置已保存。'); return true;
      } catch(error) { if (!disposed && epoch===owner) state.error=errorMessage(error); return false; }
      finally { if (!disposed && epoch===owner || !disposed && state.view==='list') { state.pending=''; notify('complete'); } }
    }
    function validate() {
      const d=state.draft;
      if (!d || !/^[a-z0-9][a-z0-9_-]*$/.test(d.id)) return text('Enter a valid provider ID.', '请填写有效的提供商 ID。');
      if (state.intent==='create' && state.envelope.providers.some(item=>providerIdentity(item.id)===providerIdentity(d.id))) return text('This provider ID already exists. Edit the existing provider or choose another ID.', '此提供商 ID 已存在，请编辑已有提供商或使用其他 ID。');
      if (state.intent==='update' && d.id!==editIdentity) return text('An existing provider ID cannot be changed.', '已有提供商 ID 不能更改。');
      if (!d.models?.length) return text('Add at least one model.', '请至少添加一个模型。');
      const ids=new Set();
      for (const model of d.models) {
        if (!model.id?.trim() || /\s/.test(model.id) || ids.has(model.id.trim())) return text('Model IDs must be nonempty and unique, without spaces.', '模型 ID 不能为空、重复或包含空格。');
        ids.add(model.id.trim());
        for (const field of ['contextWindow','maxOutputTokens','outputBudget']) if (model[field] != null && model[field]!=='' && (!Number.isSafeInteger(Number(model[field])) || Number(model[field])<0)) return text('Token limits must be nonnegative whole numbers; leave unknown values blank.', 'Token 数必须为非负整数；未知值请留空。');
        if (Number(model.outputBudget)>0 && Number(model.maxOutputTokens)>0 && Number(model.outputBudget)>Number(model.maxOutputTokens)) return text('Output budget exceeds the declared maximum output.', '实际生成预算超过了声明的最大输出。');
        if (d.credentialKind==='oauth' && Number(model.outputBudget)>0) return text('This OAuth endpoint manages the output budget. Leave it blank.', '此 OAuth 端点管理实际输出预算，请留空。');
        if (String(d.transport).includes('gemini') && (model.supportsReasoning===true || Object.keys(model.reasoningEfforts||{}).length)) return text('This Gemini protocol does not support reasoning overrides.', '当前 Gemini 协议不支持思考能力覆盖。');
      }
      if (!ids.has(d.model)) return text('Choose a default model from this provider’s models.', '请选择该提供商模型列表中的默认模型。');
      if (d.credentialAction==='replace' && !d.apiKey?.trim()) return text('Enter the replacement API key.', '请填写新的 API 密钥。');
      return '';
    }
    const api = {
      getState:()=>state,
      async load() {
        if (state.pending) return false;
        const owner=epoch, generation=++loadGeneration; state.loading=true; state.error=''; notify('loading');
        try { const result=await request('/api/providers'); if (disposed || owner!==epoch || generation!==loadGeneration) return false; install(result); return true; }
        catch(error) { if (!disposed && owner===epoch && generation===loadGeneration) state.error=errorMessage(error); return false; }
        finally { if (!disposed && owner===epoch && generation===loadGeneration) {state.loading=false;notify('loaded');} }
      },
      edit(id) {
        if (state.loading || !canLeave()) return false;
        const item=provider(id); if (!item) return false;
        leave(); state.view='editor'; state.intent='update'; editIdentity=item.id;
        state.draft={...copy(item),models:copy(item.models?.length ? item.models : item.model ? [{id:item.model,name:''}] : []),credentialAction:'keep',apiKey:''};
        state.connectionPreset=state.draft.catalogProvider||'';
        state.original=copy(state.draft);state.error='';state.message='';notify('editor');loadCatalog(epoch);return true;
      },
      create() {
        if (state.loading || !canLeave()) return false;
        leave();state.view='editor';state.intent='create';state.draft={id:'',displayName:'',transport:'openai_chat',baseUrl:'',model:'',models:[],custom:true,editable:true,credentialAction:'keep',apiKey:''};
        state.original=copy(state.draft);state.error='';state.message='';notify('editor');
        catalogDefaults={id:'',displayName:'',transport:'openai_chat',baseUrl:''};
        loadCatalog(epoch);
        return true;
      },
      updateProvider(patch) {
        if (!state.draft || state.pending) return false;
        if (state.intent==='update' && 'id' in patch && patch.id!==editIdentity) {state.error=text('An existing provider ID cannot be changed.', '已有提供商 ID 不能更改。');notify('feedback');return false;}
        if (['id','transport','baseUrl','apiKey','credentialAction'].some(key=>key in patch && patch[key]!==state.draft[key])) invalidateDiscovery();
        Object.assign(state.draft,patch); if (patch.credentialAction && patch.credentialAction!=='replace') clearSecret(); changed();return true;
      },
      updateModel(index,patch) {
        if(!state.draft||state.pending||!state.draft.models[index])return false;
        const model=state.draft.models[index], previousID=model.id;Object.assign(model,patch);
        if('id' in patch && (!state.draft.model || state.draft.model===previousID))state.draft.model=patch.id;
        if('supportsReasoning' in patch){
          if(patch.supportsReasoning!==true)delete model.reasoningEfforts;
          else if(['openai_chat','openai_responses','openai_codex_responses'].includes(state.draft.transport)&&!model.reasoningEfforts)model.reasoningEfforts={low:'low',medium:'medium',high:'high'};
        }
        changed();return true;
      },
      addModel(model={id:'',name:''}) {if(!state.draft||state.pending)return false;state.draft.models.push(copy(model));if(!state.draft.model&&model.id)state.draft.model=model.id;changed('models');return true;},
      removeModel(index) {if(!state.draft||state.pending)return false;const removed=state.draft.models.splice(index,1)[0];if(removed?.id===state.draft.model)state.draft.model=state.draft.models[0]?.id||'';changed('models');return true;},
      selectCatalog(id) {
        if(!id&&state.draft&&!state.pending){invalidateDiscovery();state.connectionPreset='';state.draft.catalogProvider='';catalogDefaults=null;changed('catalog-selection');return true;}
        const entry=state.catalog.find(item=>item.id===id);if(!entry||!state.draft||state.pending)return false;
        invalidateDiscovery();
        const defaults={id:entry.id,displayName:entry.name||entry.id,transport:entry.transport||'openai_chat',baseUrl:entry.baseUrl||entry.api||''};
        for(const [field,value] of Object.entries(defaults))if((field!=='id'||state.intent!=='update')&&(!state.draft[field]||catalogDefaults&&state.draft[field]===catalogDefaults[field]))state.draft[field]=value;
        catalogDefaults=defaults;state.connectionPreset=entry.id;
        state.draft.catalogProvider=supportsCatalogMapping(state.draft.transport)?entry.id:'';
        state.discovery=copy(entry.models||[]);changed('catalog-selection');return true;
      },
      async refreshCatalog() {
        if(!state.draft||state.pending||state.catalogRefreshing)return false;
        const owner=epoch,generation=++catalogGeneration;
        state.catalogLoading=true;state.catalogRefreshing=true;state.catalogError='';notify('catalog-loading');
        try {
          // Explicit public-directory download: never attach a provider key or
          // replace connection/model edits with a newly advertised template.
          const result=await request('/api/providers/catalog/refresh','POST');
          if(disposed||owner!==epoch||generation!==catalogGeneration)return false;
          if(!Array.isArray(result.providers))throw Error(text('Invalid connection preset directory.', '连接预设目录响应无效。'));
          state.catalog=copy(result.providers);return true;
        }catch(error){if(!disposed&&owner===epoch&&generation===catalogGeneration)state.catalogError=errorMessage(error);return false;}
        finally{if(!disposed&&owner===epoch&&generation===catalogGeneration){state.catalogLoading=false;state.catalogRefreshing=false;notify('catalog');}}
      },
      async discover() {
        if(!state.draft||state.pending||state.discovering)return false;
        const d=state.draft, owner=epoch, generation=++discoveryEpoch;
        discoveryAbort=typeof AbortController!=='undefined'?new AbortController():null;
        state.discovering=true;state.discoveryError='';notify('discovery');
        try {
          const body=d.credentialKind==='oauth'?{id:d.id}:{id:d.id,transport:d.transport,baseUrl:d.baseUrl,apiKey:d.credentialAction==='replace'?d.apiKey:'',clearCredential:d.credentialAction==='clear'};
          const result=await request('/api/providers/models','POST',body,discoveryAbort?.signal);
          if(disposed||owner!==epoch||generation!==discoveryEpoch)return false;
          state.discovery=(result.models||[]).filter(item=>item&&typeof item.id==='string');
          state.discoverySource=result.source||'provider';return true;
        } catch(error) {if(!disposed&&owner===epoch&&generation===discoveryEpoch)state.discoveryError=errorMessage(error);return false;}
        finally {if(!disposed&&owner===epoch&&generation===discoveryEpoch){state.discovering=false;notify('discovery');}}
      },
      importModels(ids) {
        if(!state.draft||state.pending)return false;
        const existing=new Set(state.draft.models.map(model=>model.id));
        for(const model of state.discovery)if(ids.includes(model.id)&&!existing.has(model.id)){
          const selected={id:model.id,name:model.name||''};
          // Import identity only. Discovery/cached capabilities remain dynamic;
          // a user must explicitly enter an advanced value to override them.
          state.draft.models.push(selected);existing.add(model.id);
        }
        if(!state.draft.model)state.draft.model=state.draft.models[0]?.id||'';
        changed('models');return true;
      },
      async save() {
        if(state.pending)return false;
        const invalid=validate();if(invalid){state.error=invalid;notify('feedback');return false;}
        const d=state.draft;
        const body={expectedRevision:state.envelope.revision,intent:state.intent,id:d.id.trim(),displayName:d.displayName||'',catalogProvider:d.credentialKind==='oauth'||supportsCatalogMapping(d.transport)?d.catalogProvider||'':'',transport:d.transport,baseUrl:d.baseUrl||'',model:d.model,models:d.models.map(model=>{
          const item={id:model.id.trim(),name:model.name||''};
          for(const field of ['contextWindow','maxOutputTokens','outputBudget'])if(Number(model[field])>0)item[field]=Number(model[field]);
          if(typeof model.supportsVision==='boolean')item.supportsVision=model.supportsVision;
          if(typeof model.supportsReasoning==='boolean')item.supportsReasoning=model.supportsReasoning;
          const efforts=Object.fromEntries(Object.entries(model.reasoningEfforts||{}).filter(([key,value])=>['low','medium','high'].includes(key)&&String(value).trim()).map(([key,value])=>[key,String(value).trim()]));
          if(Object.keys(efforts).length)item.reasoningEfforts=efforts;
          return item;
        }),credentialAction:d.credentialAction||'keep'};
        if(body.credentialAction==='replace')body.apiKey=d.apiKey;
        return mutation('save','/api/providers','POST',body);
      },
      async setDefault(id, model) {if(state.dirty){state.error=text('Save or discard your draft first.', '请先保存或放弃草稿。');notify('feedback');return false;}return mutation('default','/api/providers/default','POST',{id,model:model||provider(id)?.model,expectedRevision:state.envelope.revision});},
      async remove(id) {
        if(state.pending)return false;
        const reason=removalReason(provider(id),state.envelope.savedDefault?.provider);
        if(reason){state.error=reason;notify('feedback');return false;}
        if(!confirm(text('Remove this provider and its saved credentials?', '移除此提供商及其已保存凭据？')))return false;
        return mutation('delete','/api/providers','DELETE',{id,expectedRevision:state.envelope.revision});
      },
      async apply(id,model) {
        if(state.pending||state.dirty)return false;
        const owner=epoch;
        state.pending='apply';state.error='';notify('pending');
        try {
          const target=options.sessionTarget ? await options.sessionTarget() : null;
          if(target===false||disposed||owner!==epoch||target&&!target.isCurrent())return false;
          const result=await request('/api/models','POST',{provider:id,model});if(disposed||epoch!==owner||target&& !target.isCurrent())return false;
          state.envelope.current={provider:result.provider||id,model:result.model||model};state.message=text('Applied to the current conversation.', '已应用到当前会话。');options.applied?.(result);return true;
        }catch(error){if(!disposed&&epoch===owner)state.error=errorMessage(error);return false;}
        finally{if(!disposed&&epoch===owner){state.pending='';notify('complete');}}
      },
      cancel() {if(!canLeave())return false;leave();state.error='';state.message='';notify('list');return true;},
      hasUnsavedChanges:()=>state.dirty,
      beforeNavigate(next,previous) {
        if(previous?.page!=='settings'||previous.tab!=='providers'||next?.page==='settings'&&next.tab==='providers')return true;
        if(!canLeave())return false;leave();notify('list');return true;
      },
      dispose() {disposed=true;epoch++;invalidateDiscovery();clearSecret();state.draft=null;state.original=null;state.dirty=false;}
    };
    return api;
  }
  window.MetisProviderSettings={createController};
  let controller=null, root=null, selected=new Set(), discoveryQuery='', renderedView=null;
  const t=text;
  const feedback=(message,kind='status')=>message?`<p class="ps-feedback ${kind==='alert'?'error':''}" role="${kind}">${html(message)}</p>`:'';
  const badge=(label,kind='')=>`<span class="ps-badge ${kind}">${html(label)}</span>`;
  function connectionLabel(p){return p.credentialConfigured ? p.credentialKind==='oauth'?t('Signed in','已登录'):t('Credential configured','已配置凭据') : t('No credential','未配置凭据');}
  function summary(state) {
    const saved=state.envelope.savedDefault||{},current=state.envelope.current||{};
    if(!saved.provider&&!saved.model&&!current.provider&&!current.model)return '';
    return `<div class="ps-default-summary"><span>${t('Saved default','已保存默认')}<strong>${html(saved.provider||'—')} · ${html(saved.model||'—')}</strong></span><span>${t('Current conversation','当前会话')}<strong>${html(current.provider||'—')} · ${html(current.model||'—')}</strong></span></div>`;
  }
  function removeButton(provider,state) {
    const reason=removalReason(provider,state.envelope.savedDefault?.provider);
    return `<button type="button" data-action="delete" data-id="${html(provider.id)}" class="ps-danger" ${reason?`disabled title="${html(reason)}"`:''}>${t('Remove','移除')}</button>`;
  }
  function renderList(state) {
    return `<div class="ps-heading"><div><h2>${t('Model settings','模型设置')}</h2><p>${t('Manage providers and models, then choose what this conversation uses.','管理提供商和模型，再选择当前会话使用的模型。')}</p></div><button type="button" data-action="new" class="ps-primary">+ ${t('Add provider','添加提供商')}</button></div>${summary(state)}
      ${feedback(state.error,'alert')}${feedback(state.message)}
      <div class="ps-list-tools"><label>${t('Find a provider','查找提供商')}<input type="search" data-provider-search autocomplete="off" placeholder="${t('Search providers…','搜索提供商…')}"></label><button type="button" data-action="reload">${t('Refresh','刷新')}</button></div>
      ${state.loading?feedback(t('Loading providers…','正在加载提供商…')):''}
      <div class="ps-provider-list">${state.envelope.providers.map(p=>`<article class="ps-provider" data-provider-search-value="${html((p.id+' '+(p.displayName||'')).toLowerCase())}"><div class="ps-provider-main"><div class="ps-provider-title"><strong>${html(p.displayName||p.id)}</strong>${p.custom?badge(t('Custom','自定义')):''}${p.default?badge(t('Default','默认')):''}${state.envelope.current?.provider===p.id?badge(t('In use','使用中'),'active'):''}</div><div class="ps-provider-meta"><span>${html(p.id)}</span><span>${(p.models?.length|| (p.model?1:0))} ${t('models','个模型')}</span><span><i class="ps-status-dot ${p.credentialConfigured?'ready':''}" aria-hidden="true"></i>${html(connectionLabel(p))}</span></div></div><div class="ps-card-actions"><button type="button" data-action="edit" data-id="${html(p.id)}">${p.readOnly?t('View','查看'):t('Edit','编辑')}</button>${removeButton(p,state)}</div></article>`).join('')||feedback(t('No providers yet. Add one to get started.','还没有提供商，添加后即可开始。'))}</div>`;
  }
  function renderConnectionPresets(state) {
    return `<div class="ps-preset-picker"><label>${t('Connection preset','连接预设')}<select data-catalog ${state.catalogLoading||!state.catalog.length?'disabled':''}><option value="">${t('Custom connection','自定义连接')}</option>${state.catalog.map(item=>`<option value="${html(item.id)}" ${state.connectionPreset===item.id?'selected':''}>${html(item.name||item.id)}</option>`).join('')}</select></label><button type="button" data-action="refresh-catalog" ${state.catalogRefreshing?'disabled':''}>${state.catalogRefreshing?t('Updating presets…','正在更新预设…'):t('Update connection presets','更新连接预设')}</button><div class="ps-preset-feedback" role="status" aria-live="polite">${state.catalogError?feedback(state.catalogError,'alert'):''}${!state.catalogLoading&&!state.catalog.length?`<p class="ps-muted">${t('No local connection presets yet. Update the public directory, or enter the connection manually.','尚无本地连接预设。可更新公开目录，也可继续手动填写连接。')}</p>`:''}<p class="ps-muted">${t('Updates download a public directory without API keys or model requests. They do not change your draft.','更新仅下载公开目录，不使用 API 密钥、不调用模型，也不会更改当前草稿。')}</p></div></div>`;
  }
  function modelFields(model,index,transport,oauth) {
    const numeric=(field,label,description,disabled=false)=>`<label>${label}<input type="number" min="0" step="1" inputmode="numeric" data-model-index="${index}" data-model-field="${field}" value="${html(model[field]||'')}" placeholder="${t('Inherit / unknown','继承 / 未知')}" ${disabled?'disabled':''}><small>${description}</small></label>`;
    const openAI=['openai_chat','openai_responses','openai_codex_responses'].includes(transport),gemini=String(transport).includes('gemini');
    return `<div class="ps-model" data-model-row="${index}"><div class="ps-model-fields"><label>${t('Model ID','模型 ID')}<input data-model-index="${index}" data-model-field="id" value="${html(model.id)}" required autocomplete="off" spellcheck="false"></label><label>${t('Display name','显示名称')}<input data-model-index="${index}" data-model-field="name" value="${html(model.name||'')}" autocomplete="off"></label><button type="button" data-action="remove-model" data-index="${index}" class="ps-danger">${t('Remove','移除')}</button></div><details class="ps-model-advanced" data-model-disclosure="${modelDisclosureKey(model)}"><summary>${t('Model capabilities and generation budget','模型能力与生成预算')}</summary><div class="ps-fields">
      ${numeric('contextWindow',t('Full context capacity (tokens)','完整上下文容量（tokens）'),t('Declare a known model capacity. Leave unknown values blank.','仅填写已知的模型完整容量，未知请留空。'))}
      ${numeric('maxOutputTokens',t('Declared maximum output (tokens)','声明最大输出（tokens）'),t('The model capability, not a per-request setting.','模型能力上限，并非每次实际生成量。'))}
      ${numeric('outputBudget',t('Generation output budget (tokens)','实际生成预算（tokens）'),oauth?t('Managed by this OAuth endpoint.','由此 OAuth 端点管理。'):t('Maximum output requested at runtime; blank inherits the default.','运行时实际请求的最大输出，留空继承默认值。'),oauth)}
      <label>${t('Image input','图像输入')}<select data-model-index="${index}" data-model-field="supportsVision"><option value="auto" ${typeof model.supportsVision!=='boolean'?'selected':''}>${t('Auto / inherit','自动 / 继承')}</option><option value="yes" ${model.supportsVision===true?'selected':''}>${t('Supported','支持')}</option><option value="no" ${model.supportsVision===false?'selected':''}>${t('Not supported','不支持')}</option></select></label>
      <label>${t('Reasoning support','思考能力')}<select data-model-index="${index}" data-model-field="supportsReasoning"><option value="auto" ${typeof model.supportsReasoning!=='boolean'?'selected':''}>${t('Auto / inherit','自动 / 继承')}</option><option value="yes" ${model.supportsReasoning===true?'selected':''} ${gemini?'disabled':''}>${t('Supported','支持')}</option><option value="no" ${model.supportsReasoning===false?'selected':''}>${t('Not supported','不支持')}</option></select><small>${gemini?t('This protocol has no reasoning override.','当前协议不支持思考覆盖。'):t('Leave automatic unless the model’s capability is known.','不了解模型能力时请保留自动。')}</small></label></div>
      ${openAI?`<div class="ps-reasoning-map" data-reasoning-map="${index}" ${model.supportsReasoning===true?'':'hidden'}><p class="ps-muted">${t('Map canonical effort levels to this endpoint’s wire values. Blank levels are omitted; Default sends no override.','将思考强度映射到端点参数值。留空的档位不提供；“默认”不发送覆盖。')}</p><div class="ps-fields">${['low','medium','high'].map(level=>`<label>${({low:t('Low','低强度'),medium:t('Medium','中强度'),high:t('High','高强度')})[level]}<input data-model-index="${index}" data-model-field="effort-${level}" value="${html(model.reasoningEfforts?.[level]||'')}" autocomplete="off" spellcheck="false" placeholder="${level}"></label>`).join('')}</div></div>`:''}</details></div>`;
  }
  function renderEditor(state) {
    const d=state.draft,readonly=d.readOnly===true;
    const existing=state.intent==='update';
    const oauth=d.credentialKind==='oauth';
    return `<button type="button" data-action="back" class="ps-back">← ${t('All providers','所有提供商')}</button><div class="ps-heading"><div><h2>${html(d.displayName||d.id||t('Add provider','添加提供商'))}</h2><p>${t('Saving changes does not switch the current conversation.','保存配置不会切换当前会话。')}</p></div><span data-dirty-indicator class="ps-muted">${state.dirty?t('Unsaved changes','尚未保存'):''}</span></div>
      ${summary(state)}${readonly?feedback(t('Read-only configuration: ','只读配置：')+(d.readOnlyReason||t('managed outside this editor','由编辑器以外的配置管理'))):''}<div data-provider-feedback>${feedback(state.error,'alert')}${feedback(state.message)}</div>
      <form class="ps-editor" data-provider-form><fieldset ${readonly?'disabled':''}><fieldset ${oauth||d.editable===false?'disabled':''}>
      <div class="ps-fields"><label>${t('Provider ID','提供商 ID')}<input data-field="id" value="${html(d.id)}" ${existing?'readonly':''} required pattern="[a-z0-9][a-z0-9_-]*" autocomplete="off" spellcheck="false"></label><label>${t('Display name','显示名称')}<input data-field="displayName" value="${html(d.displayName||'')}" autocomplete="off"></label></div>
      <details class="ps-connection" ${existing?'':'open'}><summary>${t('Connection','连接设置')}</summary><div class="ps-fields"><label>${t('Protocol','协议')}<select data-field="transport">${['openai_chat','openai_responses','anthropic_messages','gemini_native'].map(value=>`<option value="${value}" ${d.transport===value?'selected':''}>${value}</option>`).join('')}</select></label><label>${t('API base URL','API 基础地址')}<input data-field="baseUrl" value="${html(d.baseUrl||'')}" type="url" autocomplete="off" placeholder="https://api.example.com/v1"></label>${renderConnectionPresets(state)}</div></details>
      ${!oauth?`<section class="ps-credentials"><h3>${t('Credential','凭据')}</h3><p class="ps-muted">${html(connectionLabel(d))}${d.credentialSource?' · '+html(d.credentialSource):''}</p><label>${t('API key action','API 密钥操作')}<select data-field="credentialAction"><option value="keep" ${d.credentialAction==='keep'?'selected':''}>${t('Keep saved credential','保留已保存凭据')}</option><option value="replace" ${d.credentialAction==='replace'?'selected':''}>${t('Set / replace API key','设置 / 替换 API 密钥')}</option><option value="clear" ${d.credentialAction==='clear'?'selected':''}>${t('Remove saved credential','移除已保存凭据')}</option></select></label><label data-key-label ${d.credentialAction==='replace'?'':'hidden'}>${t('New API key','新 API 密钥')}<input type="password" data-field="apiKey" autocomplete="new-password" value="" placeholder="${t('Enter API key…','输入 API 密钥…')}"></label><p class="ps-muted">${t('Changing the endpoint requires a new key or explicit removal. Saved keys are never displayed.','修改端点后须重新输入密钥或明确移除。已保存密钥不会回显。')}</p></section>`:''}</fieldset>
      <section class="ps-models"><div class="ps-section-heading"><h3>${t('Models','模型')}</h3><button type="button" data-action="add-model">+ ${t('Add manually','手动添加')}</button></div><div class="ps-model-list">${d.models.map((model,index)=>modelFields(model,index,d.transport,oauth)).join('')||feedback(t('Add models manually or import them below.','请手动添加模型，或从下方发现列表导入。'))}</div>
      <label class="ps-model-default">${t('Provider default model','提供商默认模型')}<select data-field="model" data-default-model>${d.models.filter(m=>m.id).map(model=>`<option value="${html(model.id)}" ${d.model===model.id?'selected':''}>${html(model.name||model.id)}</option>`).join('')||'<option value="">—</option>'}</select></label>
      <details class="ps-discovery"><summary>${t('Discover and import models','发现并导入模型')}</summary><p class="ps-muted">${t('Uses this draft’s endpoint and credential. Existing models keep their manual settings.','使用当前草稿的端点和凭据；同 ID 的已有模型保留手动设置。')}</p><button type="button" data-action="discover" ${state.discovering?'disabled':''}>${state.discovering?t('Fetching models…','正在获取模型…'):t('Fetch available models','获取可用模型')}</button><div data-discovery role="status" aria-live="polite">${renderDiscovery(state)}</div></details></section></fieldset>
      ${oauth?`<section class="ps-oauth"><h3>OAuth</h3><p>${html(connectionLabel(d))}</p><p>${t('Manage this sign-in in the terminal. Model selection remains available below.','请在终端管理此登录。仍可在下方选择当前会话的模型。')}</p><code>${html(d.setupCommand||'metis login '+d.id)}</code></section>`:''}
      <div class="ps-editor-footer"><button type="button" data-action="back">${t('Cancel','取消')}</button>${!readonly?`<button type="submit" data-save class="ps-primary">${t('Save changes','保存配置')}</button>`:''}</div></form>
      ${existing?`<section class="ps-use"><h3>${t('Use a saved model','使用已保存模型')}</h3><p class="ps-muted">${t('Save changes first. A default affects new conversations; applying affects only this conversation.','请先保存修改。默认值用于新会话；立即应用仅影响当前会话。')}</p><label>${t('Model','模型')}<select data-use-model>${(state.envelope.providers.find(p=>p.id===d.id)?.models||[{id:d.model}]).map(model=>`<option value="${html(model.id)}">${html(model.name||model.id)}</option>`).join('')}</select></label><div class="ps-use-actions"><button type="button" data-action="default">${t('Set as saved default','设为默认')}</button><button type="button" data-action="apply">${t('Apply to this conversation','立即应用到当前会话')}</button>${!oauth?`<button type="button" data-action="probe">${t('Check metadata connection','检查元数据连接')}</button>`:''}<button type="button" data-action="budget">${t('Context budget','上下文预算')}</button></div><div id="providerResult-${html(d.id)}" role="status" hidden></div><div id="providerBudget-${html(d.id)}" class="provider-discovery" hidden></div></section>`:''}`;
  }
  function renderDiscovery(state) {
    if(state.discoveryError)return feedback(state.discoveryError,'alert');
    if(state.discovering)return feedback(t('Fetching models…','正在获取模型…'));
    if(!state.discovery.length)return `<p class="ps-muted">${t('No discovered models yet. You can always add a model ID manually.','暂无发现的模型，可随时手动添加模型 ID。')}</p>`;
    const needle=discoveryQuery.toLowerCase();
    const matches=state.discovery.filter(model=>(model.id+' '+(model.name||'')).toLowerCase().includes(needle));
    return `<label>${t('Search discovered models','搜索发现的模型')}<input type="search" data-discovery-search value="${html(discoveryQuery)}" autocomplete="off" placeholder="${t('Search models…','搜索模型…')}"></label><div class="ps-discovery-choices">${matches.slice(0,100).map(model=>`<label><input type="checkbox" data-import-id="${html(model.id)}" ${selected.has(model.id)?'checked':''}><span><strong>${html(model.id)}</strong>${model.name&&model.name!==model.id?`<small>${html(model.name)}</small>`:''}<small>${html([model.contextWindow?`${t('Catalog window','目录窗口')} ${Number(model.contextWindow).toLocaleString()}`:'',model.maxOutputTokens?`${t('Catalog output','目录输出')} ${Number(model.maxOutputTokens).toLocaleString()}`:''].filter(Boolean).join(' · '))}</small></span></label>`).join('')||feedback(t('No matching models','没有匹配模型'))}</div><p class="ps-muted">${t('Import keeps capability lookup automatic. Only explicit edits create overrides.','导入后继续自动识别能力，只有明确编辑高级字段才会创建覆盖。')}</p>${matches.length>100?feedback(t('First 100 matches. Refine the search for more.','显示前 100 项，请缩小搜索范围。')):''}<button type="button" data-action="import">${t('Import selected','导入所选模型')}</button>`;
  }
  function updateDraftView(state) {
    const indicator=root?.querySelector('[data-dirty-indicator]');if(indicator)indicator.textContent=state.dirty?t('Unsaved changes','尚未保存'):'';
    root?.querySelectorAll('[data-action="apply"],[data-action="default"]').forEach(button=>button.disabled=!!state.dirty||!!state.pending);
    const label=root?.querySelector('[data-key-label]');if(label)label.hidden=state.draft?.credentialAction!=='replace';
    const secret=root?.querySelector('[data-field="apiKey"]');if(secret&&state.draft?.credentialAction!=='replace')secret.value='';
    const select=root?.querySelector('[data-default-model]');if(select&&select!==document.activeElement&&state.draft)select.innerHTML=state.draft.models.filter(m=>m.id).map(model=>`<option value="${html(model.id)}" ${state.draft.model===model.id?'selected':''}>${html(model.name||model.id)}</option>`).join('');
    const output=root?.querySelector('[data-provider-feedback]');if(output)output.innerHTML=feedback(state.error,'alert')+feedback(state.message);
    root?.querySelectorAll('[data-reasoning-map]').forEach(panel=>{
      const model=state.draft?.models[Number(panel.dataset.reasoningMap)];panel.hidden=model?.supportsReasoning!==true;
      panel.querySelectorAll('input').forEach(input=>{const level=input.dataset.modelField?.slice(7);if(input!==document.activeElement)input.value=model?.reasoningEfforts?.[level]||'';});
    });
  }
  function paint(state,reason) {
    if(!root?.isConnected)return;
    if(reason==='editor'||reason==='list'||reason==='draft'&&!state.discovery.length&&!state.discovering){selected.clear();discoveryQuery='';}
    if((reason==='draft'||reason==='feedback')&&state.view==='editor'){updateDraftView(state);return;}
    const active=document.activeElement,focused=active&&root.contains?.(active)?{field:active.dataset.field,modelField:active.dataset.modelField,index:active.dataset.modelIndex,start:active.selectionStart,end:active.selectionEnd}:null;
    const disclosureIdentity=details=>details.dataset?.modelDisclosure?'model:'+details.dataset.modelDisclosure:'section:'+details.className;
    const disclosures=new Map(Array.from(root.querySelectorAll('details')).map(details=>[disclosureIdentity(details),details.open]));
    root.innerHTML=state.view==='editor'&&state.draft?renderEditor(state):renderList(state);
    root.querySelectorAll('details').forEach(details=>{const key=disclosureIdentity(details);if(disclosures.has(key))details.open=disclosures.get(key);});
    const secret=root.querySelector('[data-field="apiKey"]');if(secret&&state.draft?.credentialAction==='replace')secret.value=state.draft.apiKey||'';
    root.querySelectorAll('button,input,select').forEach(control=>{if(state.pending)control.disabled=true;});
    root.setAttribute('aria-busy',String(!!state.pending||state.loading));updateDraftView(state);
    if(renderedView!==state.view){renderedView=state.view;root.scrollIntoView?.({block:'start',inline:'nearest',behavior:'instant'});}
    if(focused){
      const next=Array.from(root.querySelectorAll('input,select')).find(el=>focused.field?el.dataset.field===focused.field:focused.modelField&&el.dataset.modelField===focused.modelField&&el.dataset.modelIndex===focused.index);
      if(next&&!next.disabled){next.focus?.({preventScroll:true});if(focused.start!=null)try{next.setSelectionRange(focused.start,focused.end);}catch(_){}}
    }else if(reason==='editor')root.querySelector('input:not([readonly]):not(:disabled)')?.focus?.({preventScroll:true});
  }
  function mount() {
    const next=document.getElementById('providerSettingsRoot');if(!next)return;
    if(root===next&&controller)return;
    const previousSecret=root?.querySelector('[data-field="apiKey"]');if(previousSecret)previousSecret.value='';
    controller?.dispose();root=next;selected=new Set();discoveryQuery='';renderedView=null;
    controller=createController({onChange:paint,sessionTarget:async()=>{
      const target=typeof ensureComposerSessionSettingsTarget==='function'?await ensureComposerSessionSettingsTarget():undefined;
      return target===null?false:target;
    },applied:result=>{const name=document.getElementById('modelName');if(name)name.textContent=(result.model||'').split('/').pop();if(typeof loadEffort==='function')void loadEffort();if(typeof pollStatus==='function')void pollStatus();}});
    root.addEventListener('submit',event=>{event.preventDefault();void controller.save();});
    root.addEventListener('input',event=>{
      const el=event.target;
      if(el.matches('[data-provider-search]')){root.querySelectorAll('[data-provider-search-value]').forEach(row=>row.hidden=!row.dataset.providerSearchValue.includes(el.value.toLowerCase()));return;}
      if(el.matches('[data-discovery-search]')){discoveryQuery=el.value;const position=el.selectionStart;root.querySelector('[data-discovery]').innerHTML=renderDiscovery(controller.getState());const next=root.querySelector('[data-discovery-search]');next.focus();try{next.setSelectionRange(position,position);}catch(_){}return;}
      if(el.dataset.field)controller.updateProvider({[el.dataset.field]:el.value});
      if(el.dataset.modelField){
        let value=el.value;const index=Number(el.dataset.modelIndex),field=el.dataset.modelField;
        if(field==='supportsVision'||field==='supportsReasoning')value=value==='auto'?null:value==='yes';
        if(field.startsWith('effort-')){const mapping={...controller.getState().draft.models[index].reasoningEfforts},level=field.slice(7);if(value.trim())mapping[level]=value;else delete mapping[level];controller.updateModel(index,{reasoningEfforts:mapping});}
        else controller.updateModel(index,{[field]:value});
      }
    });
    root.addEventListener('change',event=>{const el=event.target;if(el.dataset.importId){if(el.checked)selected.add(el.dataset.importId);else selected.delete(el.dataset.importId);}if(el.matches('[data-catalog]'))controller.selectCatalog(el.value);});
    root.addEventListener('keydown',event=>{if(event.key==='Escape'&&controller.getState().view==='editor'){event.preventDefault();controller.cancel();}});
    root.addEventListener('click',event=>{
      const button=event.target.closest('button[data-action]');if(!button||button.disabled)return;
      const state=controller.getState(),action=button.dataset.action,id=button.dataset.id||state.draft?.id;
      if(action==='new')controller.create();else if(action==='edit')controller.edit(id);else if(action==='back')controller.cancel();
      else if(action==='reload')void controller.load();else if(action==='delete')void controller.remove(id);
      else if(action==='refresh-catalog')void controller.refreshCatalog();
      else if(action==='add-model')controller.addModel();else if(action==='remove-model')controller.removeModel(Number(button.dataset.index));
      else if(action==='discover')void controller.discover();else if(action==='import'){const ids=[...selected];selected.clear();controller.importModels(ids);}
      else if(['default','apply','budget','probe'].includes(action)){
        const model=root.querySelector('[data-use-model]')?.value||state.draft?.model;
        if(action==='default')void controller.setDefault(id,model);else if(action==='apply')void controller.apply(id,model);
        else if(action==='budget'&&typeof openProviderContextBudget==='function')void openProviderContextBudget(id,model);
        else if(action==='probe'&&typeof probeProvider==='function')probeProvider(id,button);
      }
    });
    void controller.load();
  }
  window.metisProviders={render:()=>'<div id="providerSettingsRoot" class="provider-settings"></div>',mount,
    hasUnsavedChanges:()=>controller?.hasUnsavedChanges()||false,
    beforeNavigate:(next,previous)=>controller?.beforeNavigate(next,previous)!==false,
    dispose:()=>{const secret=root?.querySelector('[data-field="apiKey"]');if(secret)secret.value='';controller?.dispose();if(root)root.innerHTML='';controller=null;root=null;renderedView=null;},
    refresh:()=>controller?.load()};
  if(typeof window.addEventListener==='function')window.addEventListener('beforeunload',event=>{if(controller?.hasUnsavedChanges()){event.preventDefault();event.returnValue='';}});
})();
