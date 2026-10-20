// Persistent jobs use the coordinator and saved principals. Cron remains a
// separate page: a paused job does not toggle the cron scheduler.
const durableOpsState = {active:false, loading:false, pending:false, data:null, error:'', actionError:'', tab:'jobs', review:null, generation:0, controller:null, timer:null};
function durableOpsText(en, zh) { return typeof uiText === 'function' ? uiText(en, zh) : zh; }
function durableOpsEscape(value) { return String(value == null ? '' : value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function durableOpsRoot() { return document.getElementById('durableOpsPage'); }
function durableOpsTime(value) { if (!value || String(value).startsWith('0001-')) return '—'; const date=new Date(value); return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString(); }
function durableOpsMoney(value) { return typeof value === 'number' ? '$'+value.toFixed(4) : '—'; }
function durableOpsStatus(value) { return ({active:'已启用',paused:'已暂停',retired:'已退役',pending:'等待接单',claimed:'已领取',acknowledged:'已交接',running:'执行中',succeeded:'已完成',completed:'已完成',failed:'失败',blocked:'已阻塞',waiting:'等待中',quality_paused:'质量观察已暂停',accepted:'已接单',verified:'已验收',uncertain:'结果待确认',settled:'已结算',unresolved:'未结算',enabled:'已启用',disabled:'已停用'})[value] || value || '—'; }
function durableOpsWaitingReason(value) { const label=({job_paused:'岗位已暂停',job_busy:'岗位正在处理其他任务',budget_exhausted:'预算不足',usage_unresolved:'尚有用量未结算',usage_check_failed:'用量检查失败',job_definition_changed:'岗位配置已变化',route_unavailable:'岗位路由不可用',job_unavailable:'岗位不可用',host_unavailable:'宿主不可用',run_cancelled:'运行已取消'})[value]; return label ? label+'（'+value+'）' : value; }
function durableOpsArray(value) { return Array.isArray(value) ? value : []; }
async function durableOpsRequest(path='', options={}) {
  const response=await fetch('/api/operations'+path,{cache:'no-store',...options,headers:{...(options.body?{'Content-Type':'application/json'}:{}),...(options.headers||{})}});
  const data=await response.json().catch(()=>({}));
  if (!response.ok) throw new Error(data.error || durableOpsText('Unable to read job state','无法读取岗位状态'));
  return data;
}
function showDurableOpsPage() {
  durableOpsState.active=true;
  const root=durableOpsRoot(); if (!root) return;
  root.hidden=false;
  if (!root.dataset.bound) { root.addEventListener('click',durableOpsClick); root.dataset.bound='true'; }
  renderDurableOps(); void loadDurableOps();
  if (!durableOpsState.timer) durableOpsState.timer=setInterval(()=>{if(durableOpsState.active && document.visibilityState!=='hidden' && !durableOpsState.loading && !durableOpsState.pending && !durableOpsState.review)void loadDurableOps(true);},15000);
}
function durableOpsWorkspaceChanged(path) {
  if(path && durableOpsState.data?.workspace===path)return;
  durableOpsState.generation++;durableOpsState.controller?.abort();durableOpsState.loading=false;
  durableOpsState.data=null;durableOpsState.review=null;durableOpsState.error='';durableOpsState.actionError='';
  if(durableOpsState.active){renderDurableOps();void loadDurableOps();}
}
function hideDurableOpsPage() {
  durableOpsState.active=false; durableOpsState.generation++; durableOpsState.controller?.abort();
  clearInterval(durableOpsState.timer);durableOpsState.timer=null;durableOpsState.loading=false;durableOpsState.review=null;
}
async function loadDurableOps(background=false) {
  const generation=++durableOpsState.generation;durableOpsState.controller?.abort();
  const controller=new AbortController();durableOpsState.controller=controller;durableOpsState.loading=true;
  if(!background)renderDurableOps();
  try { const data=await durableOpsRequest('',{signal:controller.signal});if(generation!==durableOpsState.generation)return;durableOpsState.data=data;durableOpsState.error=''; }
  catch(error) { if(generation!==durableOpsState.generation||error.name==='AbortError')return;durableOpsState.error=error.message; }
  finally { if(generation===durableOpsState.generation){durableOpsState.loading=false;renderDurableOps();} }
}
function durableOpsButton(action,label,index,disabled=false) {
  return '<button type="button" class="automation-button" data-durable-action="'+action+'"'+(index!==undefined?' data-index="'+index+'"':'')+(disabled||durableOpsState.pending?' disabled':'')+'>'+durableOpsEscape(label)+'</button>';
}
function durableOpsField(label,value) { return '<div><dt>'+durableOpsEscape(label)+'</dt><dd>'+durableOpsEscape(value == null || value === '' ? '—' : value)+'</dd></div>'; }
function durableOpsHint(value) { return value?'<p class="durable-ops-hint">'+durableOpsEscape(value)+'</p>':''; }
function durableOpsCard(title,state,body,actions='') { return '<article class="durable-ops-card"><header><h3>'+durableOpsEscape(title)+'</h3><span class="durable-ops-status">'+durableOpsEscape(durableOpsStatus(state))+'</span></header>'+body+(actions?'<footer>'+actions+'</footer>':'')+'</article>'; }
function durableOpsJob(job,index) {
  const status=job.state||job.status;const budget=job.budget||{},usage=job.usage||{};
  const spent=usage.estimated_usd??usage.spent_usd??job.spent_usd;const limit=budget.max_usd;
  const cap=Number(limit)>0?'<progress max="'+Number(limit)+'" value="'+Math.min(Number(limit),Math.max(0,Number(spent)||0))+'" aria-label="已使用预算"></progress>':'';
  const body='<dl>'+durableOpsField('岗位 ID',job.id)+durableOpsField('工作区',job.workspace)+durableOpsField('估算费用 / 预算',durableOpsMoney(spent)+' / '+(limit>0?durableOpsMoney(limit):'未设置上限'))+durableOpsField('输入 tokens / 上限',(usage.input_tokens??'—')+' / '+(budget.max_input_tokens>0?budget.max_input_tokens:'未设置上限'))+durableOpsField('输出 tokens / 上限',(usage.output_tokens??'—')+' / '+(budget.max_output_tokens>0?budget.max_output_tokens:'未设置上限'))+durableOpsField('未结算请求',usage.unresolved_requests??job.unresolved_requests??0)+durableOpsField('运行记录',job.run_count??durableOpsArray(job.run_ids).join(', '))+'</dl>'+cap+durableOpsHint(job.recovery_hint);
  const action=status==='paused'?'resume_job':'pause_job';
  return durableOpsCard(job.name||job.id,status,body,['active','paused'].includes(status)?durableOpsButton(action,status==='paused'?'恢复岗位':'暂停岗位',index):'');
}
function durableOpsHandoff(item) {
  const admission=item.admission||{},business=item.business||{},acceptance=business.acceptance||{};
  const reason=admission.waiting_reason||admission.reason||item.waiting_reason;
  const completedTime=durableOpsTime(business.completed_at);
  const receiptTime=completedTime==='—'?durableOpsTime(business.updated_at):completedTime;
  const body='<dl>'+durableOpsField('消息 ID',item.id)+durableOpsField('接收岗位',item.to||item.job_id)+durableOpsField('接单状态',durableOpsStatus(admission.state||item.state))+durableOpsField('等待原因',durableOpsWaitingReason(reason))+durableOpsField('上次检查',durableOpsTime(admission.last_checked_at))+durableOpsField('业务回执',durableOpsStatus(business.state))+durableOpsField('宿主验收',(business.acceptance_passed??acceptance.passed)===true?'通过':(business.acceptance_passed??acceptance.passed)===false?'未通过':'尚无验收')+durableOpsField('回执时间',receiptTime)+durableOpsField('验收来源',business.acceptance_source||acceptance.source)+durableOpsField('验收结果',acceptance.summary||business.failure?.summary||business.failure_code)+durableOpsField('验收证据',business.evidence_sha256||acceptance.evidence_sha256)+'</dl>'+durableOpsHint(admission.recovery_hint||item.recovery_hint);
  return durableOpsCard(item.id,item.state,body);
}
const durableOpsLabels={hot_operations:'活动操作数',hot_result_bytes:'活动结果字节',pending_operations:'未决操作数',hot_record_limit:'活动记录上限',hot_result_byte_limit:'活动结果容量',archived_segments:'归档分段',archived_result_bytes:'归档结果字节',client_request_id:'请求 ID',estimated_usd:'估算费用（USD）',actual_usd:'账单费用（USD）',usage_reported:'已回传用量',paused_reason:'暂停原因',policy:'质量规则',window_size:'观察窗口',minimum_samples:'最少样本',max_failure_rate:'最大失败比例',action:'异常处理',verifier:'宿主验收器',active_operations:'活动操作数',archived_operations:'归档操作数',active_result_bytes:'活动结果字节',archive_bytes:'归档字节',operation_limit:'活动操作上限',result_bytes_limit:'活动结果上限',total_operations:'总操作数',remaining_operations:'剩余操作数',remaining_result_bytes:'剩余结果字节',id:'ID',status:'状态',state:'状态',tool_name:'工具',item_id:'工作项',run_id:'运行 ID',provider:'提供商',model:'模型',started_at:'开始时间',updated_at:'更新时间',completed_at:'完成时间',recovery_hint:'恢复建议',code:'原因',reason:'原因',summary:'摘要',spent_usd:'费用（USD）',input_tokens:'输入 tokens',output_tokens:'输出 tokens',unresolved_requests:'未结算请求',request_id:'请求 ID',provider_request_id:'上游请求 ID',reconciliation_status:'对账状态',operations:'操作',usage_requests:'用量请求',active_records:'活动记录',archived_records:'归档记录',result_bytes:'结果字节',result_bytes_limit:'结果容量',records_limit:'记录容量',warning:'预警',warnings:'预警',archive_segments:'归档分段',total_records:'总记录',record_count:'记录数',active_count:'活动记录',archived_count:'归档记录',max_active_records:'活动记录上限',max_result_bytes:'结果容量上限',near_capacity:'接近容量',quality:'上线质量',feedback_count:'反馈数',failure_count:'失败数',success_count:'成功数',degraded:'质量下降',paused:'质量观察暂停',hold_reason:'暂停原因',consecutive_failures:'连续失败',last_feedback_at:'最近反馈',baseline_rate:'基准通过率',observed_rate:'当前通过率',count:'数量',pending:'等待中',uncertain:'待确认'};
function durableOpsMetadata(value,depth=0) {
  if(value==null)return '—';if(depth>4)return '';
  if(Array.isArray(value))return value.length?value.slice(0,100).map(item=>'<div class="durable-ops-record">'+durableOpsMetadata(item,depth+1)+'</div>').join('')+(value.length>100?'<p class="durable-ops-muted">仅显示前 100 条，共 '+value.length+' 条；完整记录请通过 CLI 查看。</p>':''):'<p class="durable-ops-muted">暂无记录</p>';
  if(typeof value==='object')return '<dl>'+Object.entries(value).filter(([key])=>durableOpsLabels[key]).map(([key,item])=>'<div><dt>'+durableOpsEscape(durableOpsLabels[key])+'</dt><dd>'+durableOpsMetadata(item,depth+1)+'</dd></div>').join('')+'</dl>';
  return durableOpsEscape(typeof value==='boolean'?(value?'是':'否'):String(value));
}
function durableOpsRun(run) {
  const operations=durableOpsArray(run.operations),usage=durableOpsArray(run.usage_requests);
  const uncertain=operations.filter(op=>op.status==='uncertain'||op.status==='started');
  const unresolved=usage.filter(req=>!['settled','reconciled','cancelled'].includes(req.status||req.state));
  const body='<dl>'+durableOpsField('运行 ID',run.id)+durableOpsField('估算费用',durableOpsMoney(run.estimated_usd))+durableOpsField('账单金额未知请求',run.actual_cost_unknown_requests??'—')+durableOpsField('待确认操作',uncertain.length)+durableOpsField('未结算请求',unresolved.length)+'</dl>'+durableOpsHint(run.recovery_hint)+'<details><summary>存储容量与归档</summary>'+durableOpsMetadata(run.storage||{})+'</details><details><summary>待确认操作（'+uncertain.length+'）</summary>'+durableOpsMetadata(uncertain)+durableOpsHint(uncertain.length?'先核查外部实际结果，再使用 CLI 的 operation resolve 处理此运行中的具体操作。':'')+'</details><details><summary>未结算用量（'+unresolved.length+'）</summary>'+durableOpsMetadata(unresolved)+durableOpsHint(unresolved.length?'补充可信供应商回执或检查对账适配器后刷新；不要把未知用量直接记成零。':'')+'</details>';
  return durableOpsCard(run.id,run.status,body);
}
function durableOpsLearning(item,index) {
  const body='<dl>'+durableOpsField('类型',item.kind)+durableOpsField('当前版本',item.active_version)+durableOpsField('初始基准',item.baseline_version)+durableOpsField('回滚目标',item.rollback_version)+durableOpsField('启用批次',item.activation_epoch)+'</dl>'+durableOpsMetadata(item.quality||{})+durableOpsHint(item.recovery_hint)+durableOpsHint(item.rollback_version?'':'当前版本没有可恢复的上一个版本。');
  return durableOpsCard(item.name,item.suspended?'paused':item.quality?.paused?'quality_paused':item.state||item.status||'active',body,durableOpsButton('rollback_learning','回滚版本…',index,!item.active_version||!item.rollback_version||item.active_version===item.rollback_version)+durableOpsButton('resume_learning','恢复质量观察…',index,!item.active_version||(!item.suspended&&!item.quality?.paused)));
}
function durableOpsReview() {
  const review=durableOpsState.review;if(!review)return '';
  const learning=review.action.includes('learning');
  return '<section class="durable-ops-review" role="region" aria-label="确认岗位操作"><h2>确认'+durableOpsEscape(review.label)+'</h2><p>对象：<strong>'+durableOpsEscape(review.displayName)+'</strong></p>'+durableOpsHint(review.impact)+durableOpsHint(learning?'当前版本 '+review.expected_version+'，启用批次 '+review.expected_epoch+'。提交时会重新检查版本；若状态已经变化，请刷新后再确认。':review.action==='pause_job'?'暂停后停止领取新任务；已在执行的工作按协调器规则处理。':'恢复后岗位可以重新领取符合配置和预算的任务。')+(learning?'<label for="durableOpsReason">操作理由</label><textarea id="durableOpsReason" maxlength="512" rows="3" placeholder="说明回滚或恢复的依据，最多 512 UTF-8 字节">'+durableOpsEscape(review.noteDraft||'')+'</textarea>':'')+(durableOpsState.actionError?'<p role="alert" class="durable-ops-error">'+durableOpsEscape(durableOpsState.actionError)+'</p>':'')+'<footer>'+durableOpsButton('confirm','确认'+review.label)+durableOpsButton('cancel','取消')+'</footer></section>';
}
function renderDurableOps() {
  const root=durableOpsRoot();if(!root||!durableOpsState.active)return;
  const data=durableOpsState.data||{},tabs=[['jobs','长期岗位'],['handoffs','交接与业务回执'],['runs','运行与待处理'],['learning','学习版本']];
  const tab=durableOpsState.tab,records=durableOpsArray(data[tab]);
  const renderers={jobs:durableOpsJob,handoffs:durableOpsHandoff,runs:durableOpsRun,learning:durableOpsLearning};
  root.setAttribute('aria-busy',String(durableOpsState.loading||durableOpsState.pending));
  root.innerHTML='<div class="durable-ops-page"><header class="automations-heading"><div><h1>'+durableOpsText('Persistent jobs','岗位管理')+'</h1><p>'+durableOpsText('Persistent work, handoff receipts and recovery in the selected workspace.','查看所选工作区的长期岗位、业务回执和恢复状态。')+'</p></div>'+durableOpsButton('refresh',durableOpsState.loading?'刷新中…':'刷新',undefined,durableOpsState.loading)+'</header><p class="durable-ops-muted">长期岗位由常驻 worker 接单；定时任务在独立的「定时任务」入口管理。</p><p class="durable-ops-muted">'+durableOpsEscape(data.workspace||'')+(data.generated_at?' · '+durableOpsEscape(durableOpsTime(data.generated_at)):'')+'</p>'+(durableOpsState.error?'<div class="durable-ops-error" role="alert">'+durableOpsEscape(durableOpsState.error)+(durableOpsState.data?'<p>保留上次读取的状态，操作前请重新刷新。</p>':'')+'</div>':'')+durableOpsArray(data.warnings).map(w=>'<p class="durable-ops-hint">'+durableOpsEscape(w)+'</p>').join('')+'<div class="durable-ops-tabs" role="group" aria-label="岗位状态分类">'+tabs.map(([key,label])=>'<button type="button" class="automation-button" data-durable-action="tab" data-tab="'+key+'" aria-pressed="'+(tab===key)+'">'+label+' <span>'+durableOpsArray(data[key]).length+'</span></button>').join('')+'</div>'+durableOpsReview()+'<div class="durable-ops-list" aria-live="polite">'+(!durableOpsState.data&&durableOpsState.loading?'<p class="durable-ops-muted">正在读取持久化状态…</p>':records.length?records.map(renderers[tab]).join(''):'<div class="durable-ops-empty"><h2>'+({jobs:'尚无长期岗位',handoffs:'暂无交接消息',runs:'暂无协调器运行记录',learning:'暂无已启用的学习版本'})[tab]+'</h2><p>'+({jobs:'通过 CLI 的 job create 建立长期岗位，再让 coordinator serve 订阅该岗位。',handoffs:'岗位收到交接消息后，将在这里显示等待原因和宿主验收结果。',runs:'岗位开始处理业务后，可以在这里检查容量、未结算用量和待确认操作。',learning:'完成评估并启用学习版本后，这里会显示上线质量和版本恢复入口。'})[tab]+'</p></div>')+'</div></div>';
}
async function durableOpsClick(event) {
  const button=event.target.closest('[data-durable-action]');if(!button||button.disabled)return;
  const action=button.dataset.durableAction;
  if(action==='refresh'){if(!durableOpsState.pending)void loadDurableOps();return;}
  if(action==='tab'){if(['jobs','handoffs','runs','learning'].includes(button.dataset.tab)){durableOpsState.tab=button.dataset.tab;renderDurableOps();}return;}
  if(action==='cancel'){durableOpsState.review=null;durableOpsState.actionError='';renderDurableOps();return;}
  if(action==='confirm'){
    if(!durableOpsState.review||durableOpsState.pending)return;
    const {label,displayName,impact,noteDraft,...request}=durableOpsState.review;
    if(request.action.includes('learning')){request.note=document.getElementById('durableOpsReason')?.value.trim()||'';durableOpsState.review.noteDraft=request.note;if(!request.note){durableOpsState.actionError='请填写操作理由。';renderDurableOps();return;}if(new TextEncoder().encode(request.note).length>512){durableOpsState.actionError='操作理由最多 512 字节（UTF-8 编码）。';renderDurableOps();return;}}
    durableOpsState.pending=true;durableOpsState.actionError='';renderDurableOps();
    try{await durableOpsRequest('/action',{method:'POST',body:JSON.stringify(request)});durableOpsState.review=null;await loadDurableOps();}
    catch(error){durableOpsState.actionError=error.message;}
    finally{durableOpsState.pending=false;renderDurableOps();}return;
  }
  if(durableOpsState.pending||durableOpsState.error)return;
  const index=Number(button.dataset.index);if(!Number.isInteger(index)||index<0)return;
  if(action==='pause_job'||action==='resume_job'){
    const job=durableOpsArray(durableOpsState.data?.jobs)[index];if(!job)return;
    durableOpsState.review={action,id:job.id,expected_workspace:durableOpsState.data.workspace,label:action==='pause_job'?'暂停岗位':'恢复岗位',displayName:job.name||job.id};
  }else if(action==='rollback_learning'||action==='resume_learning'){
    const item=durableOpsArray(durableOpsState.data?.learning)[index];if(!item)return;
    if(action==='rollback_learning'&&(!item.rollback_version||item.rollback_version===item.active_version))return;
    durableOpsState.review={action,expected_workspace:durableOpsState.data.workspace,kind:item.kind,name:item.name,displayName:item.name,expected_version:String(item.active_version||''),expected_epoch:item.activation_epoch||0,label:action==='rollback_learning'?'回滚版本':'恢复质量观察',impact:action==='rollback_learning'?'将恢复到上一个版本 '+item.rollback_version+'，并暂停自动晋升和质量观察，等待宿主复核。':item.suspended?'将重新启用此学习版本，并恢复质量观察和自动晋升。':'将恢复此版本的质量观察和自动晋升。'};
  }else return;
  durableOpsState.actionError='';renderDurableOps();const root=durableOpsRoot();
  root.querySelector('.durable-ops-review')?.scrollIntoView?.({block:'nearest',behavior:'smooth'});
}
