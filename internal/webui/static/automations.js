// Scheduled-task workspace. All task state comes from the persistent HTTP API.
const automationState = {
  active: false, loaded: false, loading: false, jobs: [], scheduler: {}, workspace: '', model: '', modelSource: '',
  query: '', filter: 'all', selectedId: '', runs: [], run: null, latestRun: null, latestRunError: '', error: '', runsError: '',
  generation: 0, runsGeneration: 0, detailGeneration: 0, latestGeneration: 0, sessionGeneration: 0, controller: null, runsController: null,
  timer: null, runRefreshTimer: null, pending: new Set(), editor: null, deletion: null,
};

function automationText(en, zh) { return typeof uiText === 'function' ? uiText(en, zh) : zh; }
function automationEscape(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
}
function automationIcon(name) {
  const paths = {clock:'M10 4v6l4 2M18 10a8 8 0 1 1-16 0 8 8 0 0 1 16 0', plus:'M10 4v12M4 10h12', search:'m14 14 4 4M16 9A7 7 0 1 1 2 9a7 7 0 0 1 14 0', play:'m6 3 11 7-11 7Z', pause:'M7 4v12M13 4v12', edit:'m12 4 4 4M3 17l4-1L17 6a2 2 0 0 0-3-3L4 13Z', trash:'M3 5h14M8 2h4M5 5l1 12h8l1-12M8 8v6M12 8v6', close:'m5 5 10 10M15 5 5 15', arrow:'M4 10h12M11 5l5 5-5 5', refresh:'M17 8A7 7 0 1 0 17 13M17 3v5h-5'};
  return '<svg class="automation-icon" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="'+(paths[name] || paths.clock)+'"/></svg>';
}
function automationButton(action, label, options = {}) {
  return '<button type="button" class="automation-button '+(options.primary ? 'is-primary ' : '')+(options.danger ? 'is-danger ' : '')+'" data-automation-action="'+action+'"'+(options.id ? ' data-id="'+automationEscape(options.id)+'"' : '')+(options.jobId ? ' data-job-id="'+automationEscape(options.jobId)+'"' : '')+(options.runId ? ' data-run-id="'+automationEscape(options.runId)+'"' : '')+(options.disabled ? ' disabled' : '')+'>'+(options.icon ? automationIcon(options.icon) : '')+'<span>'+automationEscape(label)+'</span></button>';
}
function automationFormatTime(value, timezone) {
  if (!value || String(value).startsWith('0001-')) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  try { return new Intl.DateTimeFormat(document.documentElement.lang === 'en' ? 'en' : 'zh-CN', {month:'short',day:'numeric',hour:'2-digit',minute:'2-digit', ...(timezone ? {timeZone:timezone} : {})}).format(date); }
  catch (_) { return date.toLocaleString(); }
}
function automationScheduleLabel(schedule) {
  const s = schedule || {};
  if (s.kind === 'once') return automationText('Once · ', '仅一次 · ')+automationFormatTime(s.at, s.timezone);
  if (s.kind === 'cron') return 'Cron · '+(s.cron || '—');
  const seconds = Number(s.intervalSeconds || 0);
  if (seconds && seconds % 86400 === 0) return automationText('Every '+seconds/86400+' day(s)', '每 '+seconds/86400+' 天');
  if (seconds && seconds % 3600 === 0) return automationText('Every '+seconds/3600+' hour(s)', '每 '+seconds/3600+' 小时');
  if (seconds && seconds % 60 === 0) return automationText('Every '+seconds/60+' minute(s)', '每 '+seconds/60+' 分钟');
  return automationText('Every '+seconds+' seconds', '每 '+seconds+' 秒');
}
function automationStatus(job) {
  if (job.running) return {kind:'running',label:automationText('Running','运行中')};
  if (job.paused) return {kind:'paused',label:automationText('Paused','已暂停')};
  if (!job.enabled) return {kind:'disabled',label:automationText('Disabled','已停用')};
  if (job.lastError) return {kind:'failed',label:automationText('Last run failed','上次运行失败')};
  return {kind:'active',label:automationText('Scheduled','等待运行')};
}
function automationModelLabel(job) {
  if ((job?.modelSource || automationState.modelSource) === 'workspace-default') return automationText('Uses the task workspace’s default model','使用任务工作区的默认模型');
  return job?.model || automationState.model || '—';
}
function automationRunLabel(status) {
  return ({running:automationText('Running','运行中'),succeeded:automationText('Completed','已完成'),failed:automationText('Failed','失败'),cancelled:automationText('Cancelled','已取消'),interrupted:automationText('Interrupted','已中断'),skipped:automationText('Skipped','已跳过')})[status] || status || '—';
}
async function automationRequest(path, options = {}) {
  const response = await fetch('/api/automations'+path, {cache:'no-store', ...options, headers:{...(options.body ? {'Content-Type':'application/json'} : {}), ...(options.headers || {})}});
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || automationText('Request failed','请求失败')+' ('+response.status+')');
  return data;
}
function automationRoot() { return document.getElementById('automationsPage'); }
function initAutomationsPage() {
  const root = automationRoot();
  if (!root || root.dataset.automationsReady && root.dataset.automationsLanguage === document.documentElement.lang) return root;
  root.dataset.automationsReady = 'true';
  root.dataset.automationsLanguage = document.documentElement.lang;
  root.innerHTML = '<div class="automations-page"><header class="automations-heading"><div><h1>'+automationText('Scheduled tasks','定时任务')+'</h1><p>'+automationText('Let recurring work happen on your schedule.','把重复的工作，交给下一次运行。')+'</p></div>'+automationButton('create',automationText('New task','新建任务'),{primary:true,icon:'plus'})+'</header><div id="automationScheduler"></div><div class="automations-toolbar"><label class="automation-search">'+automationIcon('search')+'<input id="automationSearch" type="search" autocomplete="off" placeholder="'+automationText('Search tasks…','搜索任务…')+'" aria-label="'+automationText('Search scheduled tasks','搜索定时任务')+'"></label><div class="automation-filters" role="group" aria-label="'+automationText('Filter tasks','筛选任务')+'">'+['all','active','paused'].map(filter => '<button type="button" data-automation-action="filter" data-id="'+filter+'" aria-pressed="'+(filter === automationState.filter)+'">'+({all:automationText('All','全部'),active:automationText('Enabled','已启用'),paused:automationText('Paused / disabled','暂停 / 停用')})[filter]+'</button>').join('')+'</div>'+automationButton('refresh',automationText('Refresh','刷新'),{icon:'refresh'})+'</div><div id="automationError" role="alert" hidden></div><div class="automations-layout"><div id="automationList" aria-live="polite"></div><aside id="automationDetail" aria-label="'+automationText('Task details','任务详情')+'" hidden></aside></div></div>';
  if (!root.dataset.automationsBound) { root.addEventListener('click', automationPageClick); root.dataset.automationsBound = 'true'; }
  root.querySelector('#automationSearch').value = automationState.query;
  root.querySelector('#automationSearch').addEventListener('input', event => { automationState.query = event.target.value; renderAutomationList(); });
  return root;
}
function showAutomationsPage() {
  automationState.active = true;
  const root = initAutomationsPage();
  if (!root) return;
  root.hidden = false;
  renderAutomations();
  if (!automationState.timer) automationState.timer = setInterval(() => {
    if (automationState.active && document.visibilityState !== 'hidden' && !automationState.loading && automationState.pending.size === 0) void loadAutomations(true);
  }, 15000);
  void loadAutomations();
}
function hideAutomationsPage() {
  automationState.active = false;
  automationState.sessionGeneration++;
  automationState.generation++;
  automationState.runsGeneration++;
  automationState.detailGeneration++;
  automationState.latestGeneration++;
  automationState.controller?.abort();
  automationState.runsController?.abort();
  clearInterval(automationState.timer);
  clearTimeout(automationState.runRefreshTimer);
  automationState.timer = null;
  automationState.runRefreshTimer = null;
  automationState.loading = false;
  closeAutomationEditor(true);
  closeAutomationDelete(true);
}
async function loadAutomations(background = false) {
  const generation = ++automationState.generation;
  automationState.controller?.abort();
  const controller = new AbortController();
  automationState.controller = controller;
  automationState.loading = true;
  if (!background) renderAutomations();
  try {
    const data = await automationRequest('', {signal:controller.signal});
    if (generation !== automationState.generation || !automationState.active) return;
    automationState.jobs = Array.isArray(data.automations) ? data.automations : [];
    automationState.scheduler = data.scheduler || {};
    automationState.workspace = data.workspace || '';
    automationState.model = data.model || '';
    automationState.modelSource = data.modelSource || '';
    automationState.loaded = true;
    automationState.error = '';
    if (!automationState.jobs.some(job => job.id === automationState.selectedId)) {
      automationState.selectedId = '';
      automationState.runs = [];
      automationState.run = null;
      automationState.latestRun = null;
      automationState.latestRunError = '';
      automationState.latestGeneration++;
      clearTimeout(automationState.runRefreshTimer);
      automationState.runRefreshTimer = null;
    }
    if (automationState.selectedId) void loadAutomationRuns(automationState.selectedId, background);
  } catch (error) {
    if (generation !== automationState.generation || controller.signal.aborted || !automationState.active) return;
    automationState.error = error.message;
  } finally {
    if (generation === automationState.generation) { automationState.loading = false; renderAutomations(); }
  }
}
function renderAutomations() {
  const root = automationRoot();
  if (!root || !root.dataset.automationsReady) return;
  const scheduler = automationState.scheduler;
  const schedulerBusy = automationState.pending.has('scheduler');
  const ready = scheduler.available === true;
  const scheduleLabel = !automationState.loaded ? automationText('Checking scheduler…','正在读取调度状态…') : !ready ? automationText('Scheduler unavailable','调度器暂不可用') : scheduler.enabled ? automationText('Scheduler enabled','自动调度已启用') : automationText('Scheduler paused','自动调度未启用');
  root.querySelector('#automationScheduler').innerHTML = '<div class="automation-scheduler"><div><strong>'+automationEscape(scheduleLabel)+'</strong><p>'+automationText('Tasks run while the METIS backend is open. Quitting the app stops scheduling.','任务仅在 METIS 后端运行期间执行。退出应用后不会继续调度。')+'</p>'+(scheduler.lastError ? '<p class="automation-error-text">'+automationEscape(scheduler.lastError)+'</p>' : '')+'</div>'+automationButton('scheduler', scheduler.enabled ? automationText('Pause scheduling','暂停调度') : automationText('Enable scheduling','启用调度'), {disabled:!ready || schedulerBusy})+'</div>';
  const error = root.querySelector('#automationError');
  error.hidden = !automationState.error;
  error.innerHTML = automationState.error ? '<p>'+automationEscape(automationState.error)+'</p><span>'+automationText('The operation did not finish. Refresh to check the latest state.','操作未完成，可以刷新以核对最新状态。')+'</span>'+automationButton('refresh',automationText('Retry','重试')) : '';
  root.querySelectorAll('[data-automation-action="filter"]').forEach(button => button.setAttribute('aria-pressed', String(button.dataset.id === automationState.filter)));
  renderAutomationList();
  renderAutomationDetail();
}
function filteredAutomations() {
  const query = automationState.query.trim().toLocaleLowerCase();
  return automationState.jobs.filter(job => (!query || (job.name+' '+job.prompt).toLocaleLowerCase().includes(query)) && (automationState.filter === 'all' || (automationState.filter === 'active' ? job.enabled && !job.paused : !job.enabled || job.paused)));
}
function renderAutomationList() {
  const root = automationRoot();
  const list = root && root.querySelector('#automationList');
  if (!list) return;
  const jobs = filteredAutomations();
  if (!automationState.loaded) {
    list.innerHTML = '<div class="automation-empty"><h2>'+automationText(automationState.loading ? 'Loading tasks…' : 'Tasks unavailable', automationState.loading ? '正在加载任务…' : '暂时无法读取任务')+'</h2><p>'+automationText('Your saved tasks will appear here.','已保存的任务会显示在这里。')+'</p></div>';
    return;
  }
  if (!jobs.length) {
    const filtered = automationState.jobs.length > 0;
    list.innerHTML = '<div class="automation-empty">'+automationIcon(filtered ? 'search' : 'clock')+'<h2>'+automationText(filtered ? 'No matching tasks' : 'Make time for the work that repeats',filtered ? '没有匹配的任务' : '让重复的工作自动开始')+'</h2><p>'+automationText(filtered ? 'Try another search or filter.' : 'Create a task with a prompt and a schedule. You can inspect every run here.',filtered ? '试试其他关键词，或切换筛选条件。' : '写下要做的事，设置运行时间。每次运行的结果都可以在这里查看。')+'</p>'+(!filtered ? automationButton('create',automationText('Create your first task','创建第一个任务'),{primary:true,icon:'plus'}) : automationButton('clear-search',automationText('Clear filters','清除筛选')))+'</div>';
    return;
  }
  list.innerHTML = '<div class="automation-list-count">'+automationText(jobs.length+' tasks',jobs.length+' 个任务')+'</div>'+jobs.map(job => {
    const status = automationStatus(job);
    return '<button type="button" class="automation-task-row'+(job.id === automationState.selectedId ? ' is-selected' : '')+'" data-automation-action="select" data-id="'+automationEscape(job.id)+'" aria-pressed="'+String(job.id === automationState.selectedId)+'"><span class="automation-row-heading"><strong>'+automationEscape(job.name)+'</strong><span class="automation-status is-'+status.kind+'">'+status.label+'</span></span><span class="automation-prompt-preview">'+automationEscape(job.prompt)+'</span><span class="automation-row-meta"><span>'+automationIcon('clock')+automationEscape(automationScheduleLabel(job.schedule))+'</span><span>'+automationText('Next: ','下次：')+automationEscape(job.enabled && !job.paused ? automationFormatTime(job.nextRunAt, job.schedule?.timezone) : '—')+'</span></span></button>';
  }).join('');
}
function renderAutomationDetail() {
  const root = automationRoot();
  const panel = root && root.querySelector('#automationDetail');
  if (!panel) return;
  const job = automationState.jobs.find(item => item.id === automationState.selectedId);
  panel.hidden = !job;
  if (!job) { panel.innerHTML = ''; return; }
  const busy = automationState.pending.has(job.id);
  const paused = job.paused || !job.enabled;
  const status = automationStatus(job);
  panel.innerHTML = '<div class="automation-detail-head"><div><span class="automation-eyebrow">'+automationText('TASK DETAILS','任务详情')+'</span><h2>'+automationEscape(job.name)+'</h2></div>'+automationButton('close-detail',automationText('Close','收起'))+'</div><div class="automation-detail-actions">'+automationButton('run',automationText(job.running ? 'Running…' : 'Run now',job.running ? '运行中…' : '立即运行'),{id:job.id,icon:'play',disabled:busy || job.running || !automationState.scheduler.available})+automationButton('toggle',paused ? automationText('Resume','恢复') : automationText('Pause','暂停'),{id:job.id,icon:paused ? 'play' : 'pause',disabled:busy})+automationButton('edit',automationText('Edit','编辑'),{id:job.id,icon:'edit',disabled:busy})+'</div><div id="automationLatestResult"></div><dl class="automation-metadata"><div><dt>'+automationText('Status','状态')+'</dt><dd>'+status.label+'</dd></div><div><dt>'+automationText('Schedule','运行计划')+'</dt><dd>'+automationEscape(automationScheduleLabel(job.schedule))+'</dd></div><div><dt>'+automationText('Time zone','时区')+'</dt><dd>'+automationEscape(job.schedule?.timezone || automationText('Server local time','服务器本地时间'))+'</dd></div><div><dt>'+automationText('Next run','下次运行')+'</dt><dd>'+automationEscape(job.enabled && !job.paused ? automationFormatTime(job.nextRunAt,job.schedule?.timezone) : '—')+'</dd></div><div><dt>'+automationText('Last run','上次运行')+'</dt><dd>'+automationEscape(automationFormatTime(job.lastRunAt,job.schedule?.timezone))+'</dd></div><div><dt>'+automationText('Run count','累计运行')+'</dt><dd>'+Number(job.runCount || 0)+(job.repeat ? '<br><span class="automation-muted">'+automationText('Automatic scheduling stops after '+Number(job.repeat)+' total runs. Run now can still add runs.','累计达到 '+Number(job.repeat)+' 次后停止自动调度；立即运行仍可额外执行。')+'</span>' : '')+'</dd></div><div><dt>'+automationText('Workspace','工作区')+'</dt><dd class="automation-path">'+automationEscape(job?.workDir || automationState.workspace || '—')+'</dd></div><div><dt>'+automationText('Model','模型')+'</dt><dd>'+automationEscape(automationModelLabel(job))+'</dd></div></dl><section class="automation-prompt-section"><h3>'+automationText('Instructions','任务指令')+'</h3><p>'+automationEscape(job.prompt)+'</p></section><details class="automation-access"><summary>'+automationText('Tool permissions','工具权限')+'</summary><p>'+automationText('Pre-authorized','预授权')+'：'+automationEscape((job.allowTools || []).join(', ') || automationText('None','无'))+'</p><p>'+automationText('Disabled','禁用')+'：'+automationEscape((job.disabledTools || []).join(', ') || automationText('None','无'))+'</p></details>'+(job.lastError ? '<div class="automation-inline-error" role="status">'+automationEscape(job.lastError)+'</div>' : '')+'<section class="automation-runs"><h3>'+automationText('Run history','运行记录')+'</h3><div id="automationRuns"></div></section><div class="automation-detail-footer">'+automationButton('delete',automationText('Delete task','删除任务'),{id:job.id,icon:'trash',danger:true,disabled:busy})+'</div>';
  renderAutomationLatestResult();
  renderAutomationRuns();
}
async function loadAutomationRuns(id, background = false) {
  const generation = ++automationState.runsGeneration;
  automationState.runsController?.abort();
  const controller = new AbortController();
  automationState.runsController = controller;
  if (!background) { automationState.runsError = ''; renderAutomationRuns(true); }
  let latestRunID = '';
  try {
    const data = await automationRequest('/'+encodeURIComponent(id)+'/runs', {signal:controller.signal});
    if (generation !== automationState.runsGeneration || id !== automationState.selectedId || !automationState.active) return;
    automationState.runs = Array.isArray(data.runs) ? data.runs : [];
    automationState.runs.forEach(run => {
      if (run.sessionId) automationSessionRuns.set(run.sessionId, {jobId:run.jobId || id,runId:run.id});
    });
    automationState.runsError = '';
    const latest = automationState.runs[0];
    if (!latest) {
      automationState.latestGeneration++;
      automationState.latestRun = null;
      automationState.latestRunError = '';
    } else if (!automationState.latestRun || automationState.latestRun.id !== latest.id || latest.status === 'running' || automationState.latestRun.status === 'running') {
      latestRunID = latest.id;
    }
  } catch (error) {
    if (controller.signal.aborted || generation !== automationState.runsGeneration || id !== automationState.selectedId || !automationState.active) return;
    automationState.runsError = error.message;
  }
  if (generation !== automationState.runsGeneration || id !== automationState.selectedId || !automationState.active) return;
  renderAutomationRuns();
  renderAutomationLatestResult();
  if (latestRunID) await loadAutomationLatestRun(id, latestRunID);
  scheduleAutomationRunRefresh(id);
}

function automationRunResultText(run) {
  return run?.error || run?.output || run?.summary || automationText('No text output was recorded for this run.','本次运行没有记录文本输出。');
}
function automationRunningText(run) {
  const latest = (run?.activity || []).slice(-4).map(item => {
    const label = item.kind === 'tool_start' ? automationText('Running','执行中')
      : item.kind === 'tool_failed' ? automationText('Failed','失败') : automationText('Finished','已完成');
    return label + ' · ' + (item.tool || automationText('Tool','工具'));
  });
  return [latest.join('\n'), run?.liveText || ''].filter(Boolean).join('\n\n')
    || automationText('Task started. Waiting for the first response…','任务已启动，正在等待首次响应…');
}

function renderAutomationLatestResult() {
  const target = automationRoot()?.querySelector('#automationLatestResult');
  if (!target) return;
  const job = automationState.jobs.find(item => item.id === automationState.selectedId);
  const run = automationState.latestRun;
  if (!run) {
    target.innerHTML = '<section class="automation-latest-result is-empty"><div class="automation-result-head"><h3>'+automationText('Latest execution result','最新执行结果')+'</h3><span>'+automationText('No completed run','暂无结果')+'</span></div><p>'+automationText('The final answer or failure reason will appear here after the task runs.','任务运行后，最终回答或失败原因会直接显示在这里。')+'</p></section>';
    return;
  }
  const status = automationRunLabel(run.status);
  const result = run.loading ? automationText('Loading the complete result…','正在读取完整结果…') : automationState.latestRunError ? automationText('Could not load the complete result: ','无法读取完整结果：')+automationState.latestRunError : run.status === 'running' ? automationRunningText(run) : automationRunResultText(run);
  const completedAt = run.finishedAt || run.startedAt;
  target.innerHTML = '<section class="automation-latest-result is-'+automationEscape(run.status || 'unknown')+'"><div class="automation-result-head"><div><h3>'+automationText('Latest execution result','最新执行结果')+'</h3><p>'+automationEscape(automationFormatTime(completedAt,job?.schedule?.timezone))+'</p></div><span class="automation-result-status">'+automationEscape(status)+'</span></div><pre'+(run.error ? ' class="is-error"' : '')+'>'+automationEscape(result)+'</pre>'+(run.sessionId ? '<div class="automation-result-actions">'+automationButton('open-session',automationText('Open conversation','打开会话'),{id:run.sessionId,jobId:run.jobId || automationState.selectedId,runId:run.id,icon:'arrow'})+'</div>' : '')+'</section>';
}

async function loadAutomationLatestRun(jobID, runID) {
  const generation = ++automationState.latestGeneration;
  const summary = automationState.runs.find(run => run.id === runID) || {id:runID};
  automationState.latestRun = {...summary, loading:true};
  automationState.latestRunError = '';
  renderAutomationLatestResult();
  try {
    const run = await automationRequest('/'+encodeURIComponent(jobID)+'/runs/'+encodeURIComponent(runID));
    if (generation !== automationState.latestGeneration || jobID !== automationState.selectedId || !automationState.active) return;
    automationState.latestRun = run;
  } catch (error) {
    if (generation !== automationState.latestGeneration || jobID !== automationState.selectedId || !automationState.active) return;
    automationState.latestRun = summary;
    automationState.latestRunError = error.message;
  }
  if (generation === automationState.latestGeneration && jobID === automationState.selectedId && automationState.active) renderAutomationLatestResult();
}

function scheduleAutomationRunRefresh(jobID) {
  clearTimeout(automationState.runRefreshTimer);
  automationState.runRefreshTimer = null;
  const latest = automationState.runs[0];
  if (!automationState.active || jobID !== automationState.selectedId || latest?.status !== 'running') return;
  automationState.runRefreshTimer = setTimeout(() => {
    automationState.runRefreshTimer = null;
    if (automationState.active && automationState.selectedId === jobID) void loadAutomations(true);
  }, 2500);
}
function renderAutomationRuns(loading = false) {
  const target = automationRoot()?.querySelector('#automationRuns');
  if (!target) return;
  if (automationState.runsError) { target.innerHTML = '<p class="automation-error-text" role="alert">'+automationEscape(automationState.runsError)+'</p>'+automationButton('refresh-runs',automationText('Retry','重试')); return; }
  if (!automationState.runs.length) { target.innerHTML = '<p class="automation-muted">'+automationText(loading ? 'Loading run history…' : 'No runs yet. Scheduled and manual runs will appear here.',loading ? '正在加载运行记录…' : '还没有运行记录。定时或手动运行后，结果会显示在这里。')+'</p>'; return; }
  target.innerHTML = automationState.runs.map(run => '<div class="automation-run-row"><button type="button" data-automation-action="run-detail" data-id="'+automationEscape(run.id)+'"><span class="automation-run-line"><strong>'+automationEscape(automationRunLabel(run.status))+'</strong><time>'+automationEscape(automationFormatTime(run.startedAt))+'</time></span><span class="automation-muted">'+automationText(run.trigger === 'manual' ? 'Manual run' : 'Scheduled run',run.trigger === 'manual' ? '手动运行' : '定时运行')+'</span>'+(run.error || run.summary ? '<span class="automation-run-summary'+(run.error ? ' is-error' : '')+'">'+automationEscape(run.error || run.summary)+'</span>' : '')+'</button>'+(run.sessionId ? automationButton('open-session',automationText('Open conversation','打开会话'),{id:run.sessionId,jobId:run.jobId || automationState.selectedId,runId:run.id,icon:'arrow'}) : '')+'</div>').join('')+'<div id="automationRunOutput"></div>';
  renderAutomationRunOutput();
}
async function loadAutomationRun(id) {
  const jobId = automationState.selectedId;
  const generation = ++automationState.detailGeneration;
  automationState.run = {id,loading:true};
  renderAutomationRunOutput();
  try {
    const run = await automationRequest('/'+encodeURIComponent(jobId)+'/runs/'+encodeURIComponent(id));
    if (generation !== automationState.detailGeneration || jobId !== automationState.selectedId || !automationState.active) return;
    automationState.run = run;
  } catch (error) {
    if (generation !== automationState.detailGeneration || jobId !== automationState.selectedId || !automationState.active) return;
    automationState.run = {id,error:error.message};
  }
  renderAutomationRunOutput();
}
function renderAutomationRunOutput() {
  const target = automationRoot()?.querySelector('#automationRunOutput');
  if (!target) return;
  const run = automationState.run;
  target.innerHTML = !run ? '' : '<section class="automation-run-output"><h4>'+automationText('Run output','运行输出')+'</h4>'+(run.loading ? '<p>'+automationText('Loading…','正在加载…')+'</p>' : '<pre>'+automationEscape(run.status === 'running' ? automationRunningText(run) : automationRunResultText(run))+'</pre>')+'</section>';
}
async function automationMutation(key, operation) {
  if (automationState.pending.has(key)) return false;
  automationState.pending.add(key); automationState.error = ''; renderAutomations();
  try { const result = await operation(); if (automationState.active) await loadAutomations(true); return result; }
  catch (error) { automationState.error = error.message; return false; }
  finally { automationState.pending.delete(key); renderAutomations(); }
}
async function automationPageClick(event) {
  const button = event.target.closest('[data-automation-action]');
  if (!button || button.disabled || !automationRoot()?.contains(button)) return;
  const action = button.dataset.automationAction;
  const id = button.dataset.id;
  const job = automationState.jobs.find(item => item.id === id);
  switch (action) {
    case 'create': openAutomationEditor(null, button); break;
    case 'edit': if (job) openAutomationEditor(job, button); break;
    case 'refresh': await loadAutomations(); break;
    case 'filter': automationState.filter = id; renderAutomations(); break;
    case 'clear-search': automationState.query = ''; automationState.filter = 'all'; automationRoot().querySelector('#automationSearch').value = ''; renderAutomations(); break;
    case 'select': clearTimeout(automationState.runRefreshTimer); automationState.runRefreshTimer = null; automationState.selectedId = id; automationState.runs = []; automationState.run = null; automationState.latestRun = null; automationState.latestRunError = ''; automationState.detailGeneration++; automationState.latestGeneration++; renderAutomations(); await loadAutomationRuns(id); break;
    case 'close-detail': automationState.selectedId = ''; automationState.runsGeneration++; automationState.detailGeneration++; automationState.latestGeneration++; automationState.runsController?.abort(); clearTimeout(automationState.runRefreshTimer); automationState.runRefreshTimer = null; renderAutomations(); break;
    case 'refresh-runs': await loadAutomationRuns(automationState.selectedId); break;
    case 'run-detail': await loadAutomationRun(id); break;
    case 'run': if (job) { const accepted = await automationMutation(id, () => automationRequest('/'+encodeURIComponent(id)+'/run', {method:'POST'})); if (accepted?.runId && automationState.active && automationState.selectedId === id) { automationState.latestGeneration++; automationState.latestRun = {id:accepted.runId, jobId:id, trigger:'manual', status:'running', startedAt:new Date().toISOString()}; automationState.latestRunError = ''; renderAutomationLatestResult(); await loadAutomationRuns(id, true); } } break;
    case 'toggle': if (job) await automationMutation(id, () => automationRequest('/'+encodeURIComponent(id), {method:'PATCH',body:JSON.stringify(job.paused || !job.enabled ? {paused:false,enabled:true} : {paused:true})})); break;
    case 'scheduler': await automationMutation('scheduler', () => automationRequest('/scheduler', {method:'PATCH',body:JSON.stringify({enabled:!automationState.scheduler.enabled})})); break;
    case 'delete': if (job) openAutomationDelete(job,button); break;
    case 'open-session': await openAutomationRunSession(id, button.dataset.jobId, button.dataset.runId); break;
  }
}

async function openAutomationRunSession(sessionId, runJobId, runId) {
  const navigation = window.metisNavigation;
  if (!navigation || typeof loadSessions !== 'function') {
    automationState.error = automationText('Conversation navigation is unavailable. Reload the app and try again.','会话导航暂不可用，请重新加载后重试。');
    renderAutomations();
    return false;
  }
  const generation = ++automationState.sessionGeneration;
  const jobId = automationState.selectedId;
  // Navigation invalidates this shared token as soon as a new intent begins,
  // before a slow session activation has committed its destination page.
  const selection = typeof resumeSessionGeneration === 'undefined' ? null : resumeSessionGeneration;
  const isLatest = () => automationState.active && generation === automationState.sessionGeneration
    && jobId === automationState.selectedId && navigation === window.metisNavigation
    && (selection === null || selection === resumeSessionGeneration)
    && (!navigation.current || navigation.current().page === 'schedules')
    && (!navigation.pending || !navigation.pending());
  try {
    // Runs create sessions outside the conversation composer. Refresh their
    // saved titles and sidebar rows before committing the destination route.
    await loadSessions(false);
    if (!isLatest()) return false;
    if (runJobId && runId) automationSessionRuns.set(sessionId, {jobId:runJobId,runId});
    const opened = await navigation.navigate({page:'session',sessionId,view:'chat'});
    if (opened) watchAutomationSession(sessionId);
    return opened;
  } catch (error) {
    if (isLatest()) { automationState.error = error.message; renderAutomations(); }
    return false;
  }
}

// Cron turns run in a separate CLI process, so they do not emit the Desktop's
// foreground SSE events. Their durable run record is the live progress source.
const automationSessionRuns = new Map();
let automationSessionWatch = null;

function stopAutomationSessionWatch() {
  if (automationSessionWatch?.timer) clearTimeout(automationSessionWatch.timer);
  automationSessionWatch = null;
}

function automationSessionCard(run, sessionId) {
  const area = document.getElementById('chatArea');
  if (!area || currentSessionId !== sessionId) return;
  let card = area.querySelector('.automation-live-run');
  if (!card) {
    area.insertAdjacentHTML('beforeend', '<section class="automation-live-run" role="status" aria-live="polite"></section>');
    card = area.querySelector('.automation-live-run');
  }
  const running = run.status === 'running';
  const label = automationRunLabel(run.status);
  const body = running ? automationRunningText(run)
    : run.error || automationText('The task ended without a text answer. Check its run record for details.','任务已结束，但没有文字回答。请查看运行记录了解详情。');
  card.classList.toggle('is-running', running);
  card.classList.toggle('is-error', !running && run.status !== 'succeeded');
  card.innerHTML = '<div class="automation-live-head"><span class="automation-live-indicator" aria-hidden="true"></span><strong>'
    +automationText('Scheduled task','定时任务')+'</strong><span>'+automationEscape(label)+'</span></div><pre>'
    +automationEscape(body)+'</pre>';
  if (typeof autoScroll === 'function') autoScroll();
}

function watchAutomationSession(sessionId) {
  const target = automationSessionRuns.get(sessionId);
  if (!target || currentSessionId !== sessionId) { stopAutomationSessionWatch(); return; }
  if (automationSessionWatch?.sessionId === sessionId && automationSessionWatch.runId === target.runId) return;
  stopAutomationSessionWatch();
  const watch = {sessionId, ...target, timer:null};
  automationSessionWatch = watch;
  const current = () => automationSessionWatch === watch && currentSessionId === sessionId;
  const again = delay => { if (current()) watch.timer = setTimeout(poll, delay); };
  async function poll() {
    if (!current()) return;
    try {
      const run = await automationRequest('/'+encodeURIComponent(watch.jobId)+'/runs/'+encodeURIComponent(watch.runId));
      if (!current()) return;
      if (run.status === 'running') {
        automationSessionCard(run, sessionId);
        again(document.visibilityState === 'hidden' ? 3000 : 900);
        return;
      }
      // Finish follows the CLI's final session checkpoint. Reload the saved
      // conversation once instead of leaving the initial prompt-only view.
      const synced = await syncViewedSessionHistory(sessionId, current);
      if (!current()) return;
      if (!synced) { again(1800); return; }
      if (run.status !== 'succeeded' || !run.output) automationSessionCard(run, sessionId);
      stopAutomationSessionWatch();
    } catch (error) {
      if (!current()) return;
      automationSessionCard({status:'running',liveText:automationText('Waiting for run updates: ','等待运行状态更新：')+error.message},sessionId);
      again(2500);
    }
  }
  void poll();
}

function automationValidTimezone(timezone) {
  try { new Intl.DateTimeFormat('en',{timeZone:timezone}).format(); return true; } catch (_) { return false; }
}
function automationLocalDate(value, timezone) {
  const date = value ? new Date(value) : new Date(Date.now()+3600000);
  const parts = new Intl.DateTimeFormat('en-CA',{timeZone:timezone,year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hourCycle:'h23'}).formatToParts(date);
  const get = type => parts.find(part => part.type === type)?.value;
  return get('year')+'-'+get('month')+'-'+get('day')+'T'+get('hour')+':'+get('minute');
}
function automationDateToISO(value, timezone) {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(value)) throw new Error(automationText('Choose a valid date and time.','请选择有效的日期和时间。'));
  if (!automationValidTimezone(timezone)) throw new Error(automationText('Enter a valid IANA time zone.','请输入有效的 IANA 时区。'));
  const target = Date.parse(value+'Z');
  if (!Number.isFinite(target) || new Date(target).toISOString().slice(0,16) !== value) throw new Error(automationText('Choose a valid calendar date.','请选择有效的日期。'));
  let candidate = target;
  for (let i=0;i<4;i++) {
    const represented = Date.parse(automationLocalDate(candidate,timezone)+'Z');
    const difference = target-represented;
    if (difference === 0) return new Date(candidate).toISOString();
    candidate += difference;
  }
  throw new Error(automationText('This local time does not exist in the selected time zone. Choose another time.','该时区不存在这个本地时间（可能处于夏令时切换），请重新选择。'));
}
function automationFormPayload(form, existing) {
  const value = name => String(form.elements.namedItem(name)?.value || '').trim();
  const checked = name => Boolean(form.elements.namedItem(name)?.checked);
  const name = value('name'), prompt = value('prompt'), kind = value('kind'), timezone = value('timezone');
  if (!name || !prompt) throw new Error(automationText('Name and instructions are required.','请填写任务名称和指令。'));
  if (!automationValidTimezone(timezone)) throw new Error(automationText('Enter a valid IANA time zone.','请输入有效的 IANA 时区。'));
  const schedule = {kind,timezone};
  if (existing?.schedule?.jitterSeconds != null) schedule.jitterSeconds = existing.schedule.jitterSeconds;
  if (kind === 'interval') {
    const amount = Number(value('interval'));
    schedule.intervalSeconds = amount*Number(value('unit'));
    if (!Number.isFinite(amount) || amount <= 0 || !Number.isSafeInteger(schedule.intervalSeconds) || schedule.intervalSeconds < 30 || schedule.intervalSeconds > 315360000) throw new Error(automationText('Choose an interval between 30 seconds and 10 years.','运行间隔应为 30 秒至 10 年。'));
  } else if (kind === 'once') {
    const originalAt = existing?.schedule?.at;
    schedule.at = originalAt && (!existing.schedule.timezone || timezone === existing.schedule.timezone) && value('at') === automationLocalDate(originalAt,timezone) ? originalAt : automationDateToISO(value('at'),timezone);
    if (Date.parse(schedule.at) <= Date.now() && (!existing || existing.schedule?.at !== schedule.at)) throw new Error(automationText('Choose a future time for a new one-time run.','一次性任务请选择未来时间。'));
  } else if (kind === 'cron') {
    schedule.cron = value('cron');
    if (!schedule.cron) throw new Error(automationText('Enter a cron expression.','请填写 Cron 表达式。'));
  } else throw new Error(automationText('Choose a schedule.','请选择运行计划。'));
  const rules = key => value(key).split(/\r?\n/).map(rule => rule.trim()).filter(Boolean);
  const allowTools = rules('allowTools'), disabledTools = rules('disabledTools');
  if ([allowTools,disabledTools].some(list => list.length > 100 || list.some(rule => rule.length > 1000))) throw new Error(automationText('Use up to 100 rules, with at most 1,000 characters per rule.','每类最多填写 100 条规则，每条不超过 1,000 个字符。'));
  const repeat = kind === 'once' ? 1 : Number(value('repeat') || 0);
  if (!Number.isSafeInteger(repeat) || repeat < 0 || repeat > 1000000) throw new Error(automationText('Run limit must be an integer from 0 to 1,000,000.','运行次数应为 0 至 1,000,000 的整数。'));
  return {name,prompt,schedule,enabled:checked('enabled'),paused:existing ? Boolean(existing.paused) : false,repeat,silent:checked('silent'),sessionMode:value('sessionMode') || 'isolated',allowTools,disabledTools};
}
// PATCH only edited fields so renaming an expired one-shot task does not reschedule it.
function automationChangedFields(payload, existing) {
  const normalizeSchedule = schedule => {
    const s = schedule || {};
    return {kind:s.kind,timezone:s.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai',jitterSeconds:s.jitterSeconds || 0,
      ...(s.kind === 'interval' ? {intervalSeconds:s.intervalSeconds} : s.kind === 'once' ? {at:Date.parse(s.at)} : {cron:s.cron})};
  };
  const changed = {};
  for (const [key,value] of Object.entries(payload)) {
    const before = key === 'schedule' ? normalizeSchedule(existing.schedule) : existing[key] ?? ({repeat:0,silent:false,paused:false,allowTools:[],disabledTools:[],sessionMode:'isolated'})[key];
    const after = key === 'schedule' ? normalizeSchedule(value) : value;
    if (JSON.stringify(before) !== JSON.stringify(after)) changed[key] = value;
  }
  return changed;
}
function automationDialogFocus(dialog,event) {
  if (event.key !== 'Tab') return;
  const controls = Array.from(dialog.querySelectorAll('button:not(:disabled),input:not(:disabled),textarea:not(:disabled),select:not(:disabled),summary')).filter(el => !el.closest('[hidden]') && (el.tagName === 'SUMMARY' || !el.closest('details:not([open])')));
  if (!controls.length) return;
  const first = controls[0], last = controls[controls.length-1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}
function closeAutomationEditor(force = false) {
  const editor = automationState.editor;
  if (!editor || editor.pending && !force) return;
  automationState.editor = null; editor.overlay.remove();
  if (editor.trigger?.isConnected) editor.trigger.focus();
}
function openAutomationEditor(job, trigger) {
  if (automationState.editor?.pending) return;
  closeAutomationEditor();
  const timezone = job?.schedule?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai';
  const kind = job?.schedule?.kind || 'interval';
  const seconds = job?.schedule?.intervalSeconds || 86400;
  const unit = seconds % 86400 === 0 ? 86400 : seconds % 3600 === 0 ? 3600 : seconds % 60 === 0 ? 60 : 1;
  const overlay = document.createElement('div');
  overlay.className = 'automation-dialog-overlay';
  const field = (label,content) => '<label class="automation-field"><span>'+label+'</span>'+content+'</label>';
  overlay.innerHTML = '<section class="automation-dialog" role="dialog" aria-modal="true" aria-labelledby="automationEditorTitle"><header><h2 id="automationEditorTitle">'+(job ? automationText('Edit task','编辑任务') : automationText('New scheduled task','新建定时任务'))+'</h2><button type="button" data-close aria-label="'+automationText('Close','关闭')+'">'+automationIcon('close')+'</button></header><form class="automation-form"><div class="automation-form-body">'+field(automationText('Name','任务名称'),'<input name="name" maxlength="160" required value="'+automationEscape(job?.name || '')+'" placeholder="'+automationText('Morning repository review','每天早上的代码检查')+'">')+field(automationText('Instructions','任务指令'),'<textarea name="prompt" rows="4" maxlength="40000" required placeholder="'+automationText('Describe what to inspect, what to produce, and when to report.','描述要检查什么、产出什么，以及什么情况需要报告。')+'">'+automationEscape(job?.prompt || '')+'</textarea>')+'<div class="automation-form-grid">'+field(automationText('Schedule','运行计划'),'<select name="kind"><option value="interval">'+automationText('Repeat at an interval','按间隔重复')+'</option><option value="once">'+automationText('Run once','仅运行一次')+'</option><option value="cron">'+automationText('Cron expression','Cron 表达式')+'</option></select>')+field(automationText('Time zone','时区'),'<input name="timezone" list="automationTimezones" required value="'+automationEscape(timezone)+'"><datalist id="automationTimezones"><option value="Asia/Shanghai"><option value="UTC"><option value="America/Los_Angeles"><option value="Europe/London"></datalist>')+'</div><div data-schedule="interval" class="automation-form-grid">'+field(automationText('Every','每隔'),'<input name="interval" type="number" min="0.01" step="any" value="'+seconds/unit+'">')+field(automationText('Unit','时间单位'),'<select name="unit"><option value="1">'+automationText('Seconds','秒')+'</option><option value="60">'+automationText('Minutes','分钟')+'</option><option value="3600">'+automationText('Hours','小时')+'</option><option value="86400">'+automationText('Days','天')+'</option></select>')+'</div><p class="automation-field-hint" data-schedule="interval">'+automationText('Minimum interval: 30 seconds. Intervals use elapsed time; the time zone only changes displayed dates.','最短间隔为 30 秒。间隔按经过时间计算，时区只影响日期显示。')+'</p><div data-schedule="once">'+field(automationText('Run at (selected time zone)','执行时间（所选时区）'),'<input name="at" type="datetime-local" value="'+automationEscape(automationLocalDate(job?.schedule?.at,automationValidTimezone(timezone) ? timezone : 'UTC'))+'">')+'<p class="automation-field-hint automation-once-preview" aria-live="polite"></p></div><div data-schedule="cron">'+field(automationText('Cron expression','Cron 表达式'),'<input name="cron" autocomplete="off" spellcheck="false" value="'+automationEscape(job?.schedule?.cron || '0 9 * * 1-5')+'">')+'<p class="automation-field-hint">'+automationText('Minute · hour · day · month · weekday. Example: 0 9 * * 1-5 runs at 09:00 on weekdays.','分钟 · 小时 · 日 · 月 · 星期。例如 0 9 * * 1-5 表示工作日 09:00。')+'</p></div><div class="automation-runtime-note"><span>'+automationText('Workspace','工作区')+'</span><code>'+automationEscape(job?.workDir || automationState.workspace || '—')+'</code><span>'+automationText('Model','模型')+'</span><code>'+automationEscape(automationModelLabel(job))+'</code><p>'+automationText('Each task uses its bound workspace and that workspace’s default model. These settings are read-only here.','任务在绑定的工作区运行，并使用该工作区的默认模型；此处仅展示这些设置。')+'</p></div><details class="automation-advanced"><summary>'+automationText('Run limits and tool permissions','运行限制与工具权限')+'</summary><div class="automation-form-grid">'+field(automationText('Stop automatic scheduling after this count (0 = unlimited)','达到次数后停止自动调度（0 为不限）'),'<input name="repeat" type="number" min="0" max="1000000" step="1" value="'+Number(job?.repeat || 0)+'"><span class="automation-muted">'+automationText('Run now can still perform additional runs.','立即运行仍可额外执行。')+'</span>')+field(automationText('Conversation history','会话历史'),'<select name="sessionMode"><option value="isolated">'+automationText('Fresh conversation each run','每次运行使用独立会话')+'</option><option value="persistent">'+automationText('Continue this task’s conversation','延续此任务的会话')+'</option><option value="main">'+automationText('Use the shared main conversation','使用共享主会话')+'</option></select>')+'</div>'+field(automationText('Pre-authorized tool rules (one per line)','预授权工具规则（每行一条）'),'<textarea name="allowTools" rows="3" spellcheck="false" placeholder="Read&#10;Bash(git status:*)">'+automationEscape((job?.allowTools || []).join('\n'))+'</textarea>')+field(automationText('Disabled tools (one per line)','禁用工具（每行一条）'),'<textarea name="disabledTools" rows="2" spellcheck="false" placeholder="Write&#10;Edit">'+automationEscape((job?.disabledTools || []).join('\n'))+'</textarea>')+'<p class="automation-field-hint">'+automationText('Unattended runs cannot answer approval prompts. Pre-authorize only the tools needed; disabled rules take precedence.','无人值守运行不能回答权限询问。请仅预授权需要的工具，禁用规则优先。')+'</p><label class="automation-checkbox"><input name="silent" type="checkbox"'+(job?.silent ? ' checked' : '')+'>'+automationText('Keep runs quiet; inspect results in history','静默运行，在运行记录中查看结果')+'</label></details><label class="automation-checkbox"><input name="enabled" type="checkbox"'+(!job || job.enabled ? ' checked' : '')+'>'+automationText('Enable this task','启用此任务')+'</label><p class="automation-field-hint">'+automationText('Automatic runs also require the scheduler to be enabled and METIS to stay open.','自动运行还需要启用调度器，并保持 METIS 运行。')+'</p><p class="automation-form-error" role="alert" hidden></p></div><footer><button type="button" data-close>'+automationText('Cancel','取消')+'</button><button type="submit" class="automation-save">'+automationText(job ? 'Save changes' : 'Create task',job ? '保存修改' : '创建任务')+'</button></footer></form></section>';
  document.body.appendChild(overlay);
  const editor = {overlay,job,trigger,pending:false};
  automationState.editor = editor;
  const form = overlay.querySelector('form');
  form.elements.namedItem('kind').value = kind;
  form.elements.namedItem('unit').value = String(unit);
  form.elements.namedItem('sessionMode').value = job?.sessionMode || 'isolated';
  const updateKind = () => { const selected = form.elements.namedItem('kind').value; form.querySelectorAll('[data-schedule]').forEach(section => { section.hidden = section.dataset.schedule !== selected; }); form.elements.namedItem('repeat').disabled = selected === 'once'; };
  const updateAtPreview = () => { const preview = form.querySelector('.automation-once-preview'); try { preview.textContent = automationText('UTC instant: ','对应 UTC 时间：')+automationDateToISO(form.elements.namedItem('at').value,form.elements.namedItem('timezone').value.trim()); } catch (error) { preview.textContent = error.message; } };
  updateKind(); updateAtPreview();
  form.elements.namedItem('at').addEventListener('input',updateAtPreview);
  form.elements.namedItem('timezone').addEventListener('input',updateAtPreview);
  form.elements.namedItem('kind').addEventListener('change',updateKind);
  overlay.querySelectorAll('[data-close]').forEach(button => button.addEventListener('click',() => closeAutomationEditor()));
  overlay.addEventListener('click',event => { if (event.target === overlay) closeAutomationEditor(); });
  overlay.addEventListener('keydown',event => { if (event.key === 'Escape') { event.preventDefault(); closeAutomationEditor(); } automationDialogFocus(overlay,event); });
  form.addEventListener('keydown',event => { if (event.key === 'Enter' && (event.isComposing || event.keyCode === 229)) event.preventDefault(); });
  form.addEventListener('submit',event => { event.preventDefault(); void saveAutomationEditor(editor); });
  form.elements.namedItem('name').focus();
}
async function saveAutomationEditor(editor) {
  if (editor !== automationState.editor || editor.pending) return;
  const form = editor.overlay.querySelector('form');
  const error = form.querySelector('.automation-form-error');
  let payload;
  try { payload = automationFormPayload(form,editor.job); if (editor.job) payload = automationChangedFields(payload,editor.job); } catch (problem) { error.textContent = problem.message; error.hidden = false; return; }
  editor.pending = true; error.hidden = true;
  const controls = Array.from(editor.overlay.querySelectorAll('button,input,select,textarea'));
  const disabled = controls.map(control => control.disabled);
  controls.forEach(control => { control.disabled = true; });
  try {
    const job = await automationRequest(editor.job ? '/'+encodeURIComponent(editor.job.id) : '', {method:editor.job ? 'PATCH' : 'POST',body:JSON.stringify(payload)});
    if (editor !== automationState.editor) return;
    automationState.selectedId = job.id;
    automationState.runs = []; automationState.run = null;
    closeAutomationEditor(true);
    if (automationState.active) await loadAutomations();
  } catch (problem) {
    if (editor !== automationState.editor) return;
    error.textContent = problem.message; error.hidden = false;
  } finally {
    editor.pending = false;
    controls.forEach((control,index) => { control.disabled = disabled[index]; });
  }
}
function closeAutomationDelete(force = false) {
  const state = automationState.deletion;
  if (!state || state.pending && !force) return;
  automationState.deletion = null; state.overlay.remove();
  if (state.trigger?.isConnected) state.trigger.focus();
}
function openAutomationDelete(job,trigger) {
  if (automationState.deletion?.pending) return;
  closeAutomationDelete();
  const overlay = document.createElement('div'); overlay.className = 'automation-dialog-overlay';
  overlay.innerHTML = '<section class="automation-dialog automation-delete-dialog" role="dialog" aria-modal="true" aria-labelledby="automationDeleteTitle"><h2 id="automationDeleteTitle">'+automationText('Delete this task?','删除这个任务？')+'</h2><p><strong>'+automationEscape(job.name)+'</strong></p><p>'+automationText('Its saved schedule will be removed. This cannot be undone.','已保存的运行计划将被移除，此操作无法撤销。')+'</p><p class="automation-form-error" role="alert" hidden></p><footer><button type="button" data-keep>'+automationText('Keep task','保留任务')+'</button><button type="button" data-delete class="is-danger">'+automationText('Delete task','删除任务')+'</button></footer></section>';
  document.body.appendChild(overlay);
  const state = {job,trigger,overlay,pending:false}; automationState.deletion = state;
  overlay.querySelector('[data-keep]').addEventListener('click',() => closeAutomationDelete());
  overlay.querySelector('[data-delete]').addEventListener('click',() => { void confirmAutomationDelete(state); });
  overlay.addEventListener('click',event => { if (event.target === overlay) closeAutomationDelete(); });
  overlay.addEventListener('keydown',event => { if (event.key === 'Escape') closeAutomationDelete(); automationDialogFocus(overlay,event); });
  overlay.querySelector('[data-keep]').focus();
}
async function confirmAutomationDelete(state) {
  if (state !== automationState.deletion || state.pending) return;
  state.pending = true; state.overlay.querySelectorAll('button').forEach(button => { button.disabled = true; });
  const error = state.overlay.querySelector('.automation-form-error'); error.hidden = true;
  try {
    await automationRequest('/'+encodeURIComponent(state.job.id),{method:'DELETE'});
    if (state !== automationState.deletion) return;
    closeAutomationDelete(true);
    if (automationState.active) await loadAutomations();
  } catch (problem) { if (state === automationState.deletion) { error.textContent = problem.message; error.hidden = false; } }
  finally { state.pending = false; state.overlay.querySelectorAll('button').forEach(button => { button.disabled = false; }); }
}
