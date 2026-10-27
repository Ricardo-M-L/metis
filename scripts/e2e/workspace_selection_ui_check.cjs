'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const sources = JSON.parse(fs.readFileSync(0, 'utf8'));
function extract(file, from, until) {
  const source = sources[file], start = source.indexOf(from), end = until ? source.indexOf(until, start + from.length) : source.length;
  assert(start >= 0 && end > start, from);
  return source.slice(start, end);
}
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const draftHeader = (id, workspaceId = 'a', workDir = '/alpha') => ({
  id, workspaceId, workDir,
  permissionSessionId: id, permissionMode: 'default', permissionModeAvailable: true,
});
function context() {
  const calls = [], toasts = [], rendered = [];
  const input = { value: 'old draft', style: {}, dispatchEvent() {}, focus() {} };
  const control = () => ({ style: {}, textContent: '', innerHTML: '', disabled: false, classList: { toggle() {} }, setAttribute() {} });
  const elements = new Map([['inputField', input], ['sendBtn', control()], ['chatArea', { innerHTML: 'old transcript' }], ['sessionStatsbar', { style: {}, textContent: '' }], ['wsGroupName', { textContent: 'Alpha' }], ['modelMenu', control()], ['modelName', control()], ['effortMenu', control()], ['effortBtn', control()]]);
  const c = {
    console, URLSearchParams, AbortController, Map, Set, Event: class Event {},
    window: { metisNavigation: { recordSession() {} } },
    document: { documentElement: { lang: 'en' }, getElementById: id => elements.get(id) || null, querySelector: () => null },
    workspaces: [{ id: 'a', name: 'Alpha', path: '/alpha', active: true }, { id: 'b', name: 'Beta', path: '/beta' }],
    activeWorkspaceId: 'a', currentSessionId: 'old-a', sessions: [{ id: 'old-a', workspaceId: 'a', workDir: '/alpha' }],
    lastStatusSnapshot: null, currentView: 'chat', messages: ['old transcript'], attachments: [{ data: 'old-image', mediaType: 'image/png' }],
    queuedTurns: [], queuedSessionId: null, queuedTurnMenuIndex: -1, queuedTurnPendingItem: null, drainingQueuedTurns: false,
    turnRunning: false, runningSessionId: null, desktopPreferences: { language: 'en' }, pendingAsk: null,
    backgroundContinuationGenerations: new Map(), backgroundContinuationGeneration: 0,
    pendingForegroundRequest: null, runningTurnNeedsHistorySync: false, runningTurnIncompleteReason: '', streamedTextThisTurn: false,
    draftSessionCreation: null, followOutput: true,
    sessionQueues: new Map(), foregroundRequests: new Map(), pendingStopSessions: new Set(), stopRequestPending: false,
    stopRequestsInFlight: new Set(), resumeStatesBySession: new Map(), pausedQueueSessions: new Set(),
    renderSessions() { rendered.push({ sessionId: c.currentSessionId, workspaceId: c.activeWorkspaceId }); },
    showToast: text => toasts.push(text), showError: text => toasts.push(text),
    parallelTurnsEnabled: () => true, saveSessionQueue() {}, restoreSessionQueue() {},
    resetTraceForSession() {}, resetSessionFiles() {}, resetArtifactsForSession() {}, renderQueuedTurns() {},
    resetTurnState() {}, syncTrackedRunningState() {}, updateScrollFollowUI() {}, updateEmptyLayout() {},
    closeMismatchedSubAgentDetails() {}, closeDesktopTransientPopovers() {},
    renderAttachments() {}, updateSendBtn() {}, applyLanguage() {}, switchView: view => { c.currentView = view; },
    setTimeout() {}, renderStatusSnapshot() {}, syncTurnControls() {}, loadSessionStatsbar() {},
    renderHistoryMessages: history => { c.messages = history; }, restoreCompactionHistory: async () => {}, loadArtifactsForSession: async () => {},
    loadEffort: async () => {}, pollStatus: async () => {},
    effortState: { effort: 'default' }, effortLabel: value => value, modelMenuOpen: false,
    permissionModeGeneration: 0, permissionModeSnapshot: null, permissionModePending: false,
    normalizedPermissionMode: value => value, approvalMode: 'default', confirmFullAccessMode: async () => true,
    syncApprovalChip: mode => { c.approvalMode = mode; }, settingByKey: () => null,
    PERMISSION_LABELS: { default: 'Default', plan: 'Plan', acceptEdits: 'Accept edits', fullAccess: 'Full access' }, PERMISSION_LABELS_ZH: { default: '默认确认', plan: '计划模式', acceptEdits: '自动确认编辑', fullAccess: '完全访问' },
    workspaceLabel: path => String(path || '').split('/').filter(Boolean).at(-1) || 'metis',
    detachRunningTurnView() { c.detached = (c.detached || 0) + 1; },
    closeWorkspaceMenu() {}, clearTodoPlan() {}, resumeAutoScroll() {}, beginUserTurn() {}, finishUserTurn() {},
    setTurnRunning: (value, id) => { c.turnRunning = value; c.runningSessionId = value ? id : null; },
    addMessage: (role, text) => { c.messages.push({ sessionId: c.currentSessionId, role, text }); },
    loadSessions: async () => {}, isViewedTurnRunning: () => c.foregroundRequests.has(c.currentSessionId),
    rememberForegroundRequest: request => { if (request.sessionId) c.foregroundRequests.set(request.sessionId, request); },
    finishForegroundRequest: request => { if (c.foregroundRequests.get(request.sessionId) === request) c.foregroundRequests.delete(request.sessionId); },
    fetch: async (url, options) => { calls.push({ url, options, body: options?.body && JSON.parse(options.body) }); return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) }; }
  };
  c.uiText = (en, zh) => c.document.documentElement.lang === 'zh-CN' ? zh : en;
  vm.createContext(c);
  vm.runInContext(extract('sessions.js', '// --- Sessions ---', 'function toggleWorkspaceMenu('), c);
  vm.runInContext(extract('chat.js', 'function applyResumeSnapshot(', 'function parallelTurnsEnabled('), c);
  vm.runInContext(extract('chat.js', 'function newChat(', '// The POST resolves'), c);
  vm.runInContext(extract('chat.js', 'async function prepareDraftSession()', 'function syncTurnControls()'), c);
  vm.runInContext(extract('chat.js', 'function sameSession(d)', 'let thinkingEl'), c);
  vm.runInContext(extract('chat.js', 'async function runTurnItem(item)', 'const MESSAGE_ACTION_ICONS'), c);
  vm.runInContext(extract('chat.js', 'async function stopTurn()', 'function startStreamingMessage()'), c);
  vm.runInContext(extract('chat.js', 'function updateSendBtn()', '// --- Model ---'), c);
  vm.runInContext(extract('chat.js', 'function saveSessionQueue()', '// Allocate an ID'), c);
  vm.runInContext(extract('app.js', 'function refreshSelectedWorkspaceTitle(', 'function toggleSearch('), c);
  vm.runInContext(extract('sessions.js', 'async function resumeSession(id)'), c);
  vm.runInContext(extract('sessions.js', 'function renderWorkspaceHeader(', 'function sessionState('), c);
  vm.runInContext(extract('chat.js', 'async function chooseEffort(', '// Events carry the session'), c);
  vm.runInContext(extract('chat.js', 'async function switchModel(', 'const APPROVAL_MODES'), c);
  vm.runInContext(extract('chat.js', 'async function setPermissionMode(', 'async function openPluginSettings('), c);
  vm.runInContext(extract('chat.js', 'function applySessionPermissionMode(', 'function permissionModeForTurn('), c);
  c.escHtml = c.escAttr = c.escOnclick = String;
  return { c, calls, toasts, rendered, elements };
}
(async () => {
  let checks = 0;
  {
    const { c, calls, elements } = context();
    await c.openWorkspace('b');
    assert.equal(calls[0].url, '/api/workspaces/select', 'normal folder click must select inside the current window');
    assert.equal(c.activeWorkspaceId, 'b');
    assert.equal(c.currentSessionId, null);
    assert.equal(c.messages.length, 0);
    assert.equal(elements.get('inputField').value, '', 'the old conversation draft must not move into the new workspace');
    assert.equal(c.attachments.length, 0);
    assert.equal(elements.get('wsGroupName').textContent, 'Beta');
    c.refreshSelectedWorkspaceTitle('old worker workspace');
    assert.equal(elements.get('wsGroupName').textContent, 'Beta', 'status must not replace the selected workspace title with a worker workspace');
    checks++;
  }
  // A failure must keep transcript, composer, selection and running view intact.
  {
    const { c, elements, toasts } = context();
    c.fetch = async () => ({ ok: false, json: async () => ({ error: 'directory unavailable' }) });
    assert.equal(await c.openWorkspace('b'), false);
    assert.equal(c.currentSessionId, 'old-a');
    assert.equal(c.activeWorkspaceId, 'a');
    assert.equal(elements.get('inputField').value, 'old draft');
    assert.equal(c.messages[0], 'old transcript');
    assert.equal(c.detached, undefined);
    assert.match(toasts[0], /Select workspace failed/);
    checks++;
  }
  // Serialize backend mutations; only the latest response may commit the UI.
  {
    const { c, calls, elements } = context(), slowA = deferred();
    c.fetch = async (url, options) => {
      const body = JSON.parse(options.body); calls.push({ url, body });
      if (body.id === 'a') return slowA.promise;
      return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) };
    };
    const a = c.openWorkspace('a'); await tick();
    const b = c.openWorkspace('b'); await tick();
    assert.equal(calls.length, 1, 'B navigation must not overtake the pending A backend mutation');
    assert.equal(elements.get('sendBtn').disabled, true);
    assert.equal(await c.prepareDraftSession(), false, 'sending during workspace navigation must not allocate in the old directory');
    assert.equal(await c.runTurnItem({ text: 'unsafe early send' }), false);
    slowA.resolve({ ok: true, json: async () => ({ activeId: 'a', workspace: c.workspaces[0] }) });
    assert.equal(await a, false); await b;
    assert.deepEqual(calls.map(call => call.body.id), ['a', 'b']);
    assert.equal(c.activeWorkspaceId, 'b');
    assert.equal(c.currentSessionId, null);
    assert.equal(elements.get('chatArea').innerHTML.includes('old transcript'), false);
    checks++;
  }
  // Workspace-scoped blank drafts and session drafts stay separate.
  {
    const { c, elements } = context();
    c.fetch = async (_url, options) => {
      const id = JSON.parse(options.body).id;
      return { ok: true, json: async () => ({ activeId: id, workspace: c.workspaces.find(item => item.id === id) }) };
    };
    await c.openWorkspace('b');
    elements.get('inputField').value = 'Beta draft'; c.attachments = [{ data: 'beta-image' }];
    await c.openWorkspace('a');
    elements.get('inputField').value = 'Alpha blank draft';
    await c.openWorkspace('b');
    assert.equal(elements.get('inputField').value, 'Beta draft');
    assert.equal(c.attachments[0].data, 'beta-image');
    assert.equal(c.sessionQueues.get('old-a').items.length, 0);
    checks++;
  }
  // Returning to the already-visible blank A must cancel a pending B intent.
  {
    const { c, calls } = context(), slowB = deferred();
    c.currentSessionId = null;
    c.fetch = async (url, options) => {
      const body = JSON.parse(options.body); calls.push({ url, body });
      if (body.id === 'b') return slowB.promise;
      return { ok: true, json: async () => ({ activeId: 'a', workspace: c.workspaces[0] }) };
    };
    const b = c.openWorkspace('b'); await tick();
    const a = c.openWorkspace('a'); await tick();
    slowB.resolve({ ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) });
    assert.equal(await b, false); await a;
    assert.deepEqual(calls.map(call => call.body.id), ['b', 'a']);
    assert.equal(c.activeWorkspaceId, 'a');
    checks++;
  }
  // Sending an allocated blank draft must not resurrect it on the next New chat.
  {
    const { c, elements } = context(); c.currentSessionId = null;
    c.saveComposerDraftForView();
    c.fetch = async () => ({ ok: true, json: async () => ({ id: 'created-a', workspaceId: 'a', workDir: '/alpha' }) });
    assert.equal(await c.prepareDraftSession(), true);
    elements.get('inputField').value = ''; c.attachments = [];
    c.newChat();
    assert.equal(elements.get('inputField').value, '');
    assert.equal(c.attachments.length, 0);
    checks++;
  }
  // Failed allocation retains the unsent blank draft and image attachment.
  {
    const { c, elements } = context(); c.currentSessionId = null;
    c.saveComposerDraftForView();
    c.fetch = async () => ({ ok: false, json: async () => ({ error: 'creation failed' }) });
    assert.equal(await c.prepareDraftSession(), false);
    c.newChat();
    assert.equal(elements.get('inputField').value, 'old draft');
    assert.equal(c.attachments[0].data, 'old-image');
    checks++;
  }
  // A workspace list fetched before selection must not restore the old active id.
  {
    const { c } = context(), oldList = deferred();
    c.fetch = async (url, options) => {
      if (url === '/api/workspaces') return oldList.promise;
      return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) };
    };
    const load = c.loadWorkspaces();
    await c.openWorkspace('b');
    oldList.resolve({ ok: true, json: async () => ({ activeId: 'a', workspaces: c.workspaces }) });
    await load;
    assert.equal(c.activeWorkspaceId, 'b');
    checks++;
  }
  // Late history cannot replace the blank view requested by a newer folder click.
  {
    const { c } = context(), oldHistory = deferred();
    c.turnRunning = true;
    c.fetch = async (url, options) => {
      if (url === '/api/sessions/old-a') return oldHistory.promise;
      return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) };
    };
    const resume = c.resumeSession('old-a'); await tick();
    await c.openWorkspace('b');
    oldHistory.resolve({ ok: true, json: async () => ({ session: c.sessions[0], messages: ['late A'] }) });
    await resume;
    assert.equal(c.activeWorkspaceId, 'b');
    assert.equal(c.currentSessionId, null);
    assert.equal(c.messages.length, 0);
    checks++;
  }
  // Idle history still activates its session so model/effort controls use B.
  {
    const { c, calls } = context();
    const betaSession = { id: 'old-b', workspaceId: 'b', workDir: '/beta' };
    c.sessions.push(betaSession);
    let activated = '';
    c.fetch = async (url, options) => {
      const body = options?.body && JSON.parse(options.body); calls.push({ url, body });
      if (url === '/api/sessions/activate') {
        activated = body.id;
        return { ok: true, json: async () => ({ session: betaSession, messages: ['B history'] }) };
      }
      return { ok: true, json: async () => ({ activeId: body.id, workspace: c.workspaces.find(item => item.id === body.id) }) };
    };
    await c.resumeSession('old-b');
    assert.equal(activated, 'old-b');
    assert.equal(c.activeWorkspaceId, 'b');
    assert.equal(c.currentSessionId, 'old-b');
    assert.deepEqual(calls.map(call => call.url), ['/api/sessions/activate', '/api/workspaces/select']);
    checks++;
  }
  // Explicit new-window action remains available and fully localized.
  {
    const { c, calls, toasts } = context();
    c.document.documentElement.lang = 'zh-CN';
    await c.openWorkspaceInNewWindow('b');
    assert.equal(calls[0].url, '/api/workspaces/open');
    assert.equal(c.currentSessionId, 'old-a');
    assert.equal(c.activeWorkspaceId, 'a');
    assert.match(toasts[0], /新桌面窗口/);
    const zh = c.renderWorkspaceHeader(c.workspaces[1], 0, 1);
    assert.match(zh, /选择工作区：Beta/);
    assert.match(zh, /在新窗口打开/);
    assert.doesNotMatch(zh, /in a new window|Workspace actions|Rename|Remove from list/);
    c.document.documentElement.lang = 'en';
    const en = c.renderWorkspaceHeader(c.workspaces[1], 0, 1);
    assert.match(en, /Select workspace Beta/);
    assert.match(en, /Open in a new window/);
    assert.match(en, /onclick="openWorkspace\('b'\)"/);
    checks++;
  }
  // Adding a directory selects it without asking the native shell to launch.
  {
    const { c, calls } = context();
    c.requestNative = async action => { assert.equal(action, 'choose-workspace'); return '/gamma'; };
    c.fetch = async (url, options) => {
      calls.push({ url, body: options?.body && JSON.parse(options.body) });
      const gamma = { id: 'c', name: 'Gamma', path: '/gamma' };
      if (url === '/api/workspaces' && options) return { ok: true, json: async () => gamma };
      if (url === '/api/workspaces') return { ok: true, json: async () => ({ workspaces: [...c.workspaces, gamma], activeId: 'a' }) };
      return { ok: true, json: async () => ({ activeId: 'c', workspace: gamma }) };
    };
    await c.addWorkspace();
    assert.equal(c.activeWorkspaceId, 'c');
    assert.equal(c.currentSessionId, null);
    assert.equal(calls.filter(call => call.url === '/api/workspaces/select').length, 1);
    assert.equal(calls.some(call => call.url === '/api/workspaces/open'), false);
    checks++;
  }
  // Allocation captures the selected id; a late A allocation cannot bind B.
  {
    const { c, calls } = context(), creation = deferred();
    c.currentSessionId = null;
    c.fetch = async (url, options) => {
      calls.push({ url, body: JSON.parse(options.body) });
      if (url === '/api/sessions') return creation.promise;
      return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) };
    };
    const prepared = c.prepareDraftSession(); await tick();
    assert.equal(calls[0].body.workspaceId, 'a');
    await c.openWorkspace('b');
    creation.resolve({ ok: true, json: async () => ({ id: 'created-a', workspaceId: 'a', workDir: '/alpha' }) });
    assert.equal(await prepared, false);
    assert.equal(c.currentSessionId, null);
    assert.equal(c.activeWorkspaceId, 'b');
    c.fetch = async (url, options) => { calls.push({ url, body: JSON.parse(options.body) }); return { ok: true, json: async () => ({ id: 'created-b', workspaceId: 'b', workDir: '/beta' }) }; };
    assert.equal(await c.prepareDraftSession(), true);
    assert.equal(calls.at(-1).body.workspaceId, 'b');
    assert.equal(c.currentSessionId, 'created-b');
    checks++;
  }
  // Allocation gives the first turn a durable owner. It must not attach its
  // SSE or fallback to the blank view requested by a later workspace click.
  {
    const { c, calls, elements } = context(), completion = deferred();
    c.currentSessionId = null;
    c.fetch = async (url, options) => {
      calls.push({ url, body: JSON.parse(options.body) });
      if (url === '/api/sessions') return { ok: true, json: async () => draftHeader('late-a') };
      if (url === '/api/turns') return completion.promise;
      return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) };
    };
    const turn = c.runTurnItem({ text: 'A first turn' }); await tick();
    const request = c.pendingForegroundRequest;
    assert.equal(calls[0].body.workspaceId, 'a');
    assert.equal(calls[0].url, '/api/sessions');
    assert.equal(calls.find(call => call.url === '/api/turns').body.sessionId, 'late-a');
    assert(request, 'the allocated origin must own its in-flight request');
    const runningOwner = c.runningSessionId;
    assert.equal(runningOwner, 'late-a');
    await c.openWorkspace('b');
    assert.equal(request.controller.signal.aborted, false, 'navigation must not cancel the original task');
    assert.equal(c.sameSession({ session: 'late-a' }), false, 'old A SSE must not claim B blank view');
    assert.equal(c.runningSessionId, runningOwner, 'old A SSE must not change the already-owned background task');
    assert.equal(c.currentSessionId, null, 'old A SSE must not claim the B blank composer');
    assert.equal(c.isViewedTurnRunning(), false, 'B blank view must not display A as its own running task');
    completion.resolve({ ok: true, json: async () => ({ sessionId: 'late-a', text: 'A final' }) }); await turn;
    assert.equal(c.currentSessionId, null);
    assert.equal(c.messages.length, 0, 'old A fallback must not enter B blank transcript');
    checks++;
  }
  // Failed navigation keeps the allocated first-turn owner valid, including
  // Desktop before its isolated-turn capability status has arrived.
  {
    const { c } = context(), completion = deferred();
    c.currentSessionId = null;
    c.parallelTurnsEnabled = () => false;
    c.fetch = async url => url === '/api/sessions'
      ? { ok: true, json: async () => draftHeader('fresh-a') }
      : url === '/api/turns' ? completion.promise
      : { ok: false, json: async () => ({ error: 'directory unavailable' }) };
    const turn = c.runTurnItem({ text: 'A first turn' }); await tick();
    assert.equal(await c.openWorkspace('b'), false);
    assert.equal(c.sameSession({ session: 'fresh-a' }), true, 'failed navigation must preserve the visible first-turn stream owner');
    completion.resolve({ ok: true, json: async () => ({ sessionId: 'fresh-a', text: 'A answer' }) }); await turn;
    assert.equal(c.currentSessionId, 'fresh-a');
    assert.equal(c.messages.at(-1).text, 'A answer');
    assert.equal(c.activeWorkspaceId, 'a');
    checks++;
  }
  // Direct links and legacy headers use effective server metadata even when
  // their workspace is outside the currently loaded sidebar registry.
  for (const sessionId of ['direct-link', 'legacy-header']) {
    const { c, calls } = context();
    const alpha = c.workspaces[0];
    c.workspaces = [c.workspaces[1]]; c.activeWorkspaceId = 'b';
    c.fetch = async (url, options) => {
      const body = options?.body && JSON.parse(options.body); calls.push({ url, body });
      if (url === '/api/sessions/activate') return { ok: true, json: async () => ({ session: { id: sessionId, workspaceId: 'a', workDir: '/alpha' }, messages: [] }) };
      return { ok: true, json: async () => ({ activeId: 'a', workspace: alpha }) };
    };
    await c.resumeSession(sessionId);
    assert.equal(c.currentSessionId, sessionId);
    assert.equal(c.activeWorkspaceId, 'a');
    assert.equal(c.workspaces.find(item => item.id === 'a').path, '/alpha');
    assert.equal(calls.find(call => call.url === '/api/workspaces/select').body.id, 'a');
    checks++;
  }
  // A failed history click likewise keeps the visible first turn attached.
  {
    const { c } = context(), completion = deferred();
    c.currentSessionId = null; c.parallelTurnsEnabled = () => false;
    c.fetch = async url => url === '/api/sessions'
      ? { ok: true, json: async () => draftHeader('fresh-a') }
      : url === '/api/turns' ? completion.promise
      : { ok: false, status: 404, json: async () => ({ error: 'session missing' }) };
    const turn = c.runTurnItem({ text: 'A first turn' }); await tick();
    assert.equal(await c.resumeSession('missing-history'), false);
    assert.equal(c.sameSession({ session: 'fresh-a' }), true);
    completion.resolve({ ok: true, json: async () => ({ sessionId: 'fresh-a', text: 'A answer' }) }); await turn;
    assert.equal(c.currentSessionId, 'fresh-a');
    assert.equal(c.messages.at(-1).text, 'A answer');
    checks++;
  }
  // Saved A history remains readable if its directory disappears while B runs.
  {
    const { c, toasts } = context(); c.currentSessionId = 'old-b'; c.activeWorkspaceId = 'b'; c.turnRunning = true;
    c.fetch = async url => url.startsWith('/api/sessions/')
      ? { ok: true, json: async () => ({ session: { id: 'saved-a', workspaceId: 'a', workDir: '/alpha' }, messages: ['A saved history'] }) }
      : { ok: false, json: async () => ({ error: 'directory does not exist' }) };
    assert.equal(await c.resumeSession('saved-a'), true);
    assert.equal(c.currentSessionId, 'saved-a');
    assert.equal(c.activeWorkspaceId, 'a');
    assert.equal(c.messages[0], 'A saved history');
    assert.equal(c.workspaces.find(item => item.id === 'a').available, false);
    assert.equal(await c.runTurnItem({ text: 'cannot run in deleted A' }), false);
    assert.match(toasts.at(-1), /directory is unavailable/);
    c.fetch = async () => ({ ok: true, json: async () => ({ activeId: 'b', workspaces: c.workspaces }) });
    await c.loadWorkspaces();
    assert.equal(c.activeWorkspaceId, 'a', 'server background B selection must not replace the unavailable history owner');
    checks++;
  }
  // Each blank-B composer control allocates and activates B before mutating
  // global parent runtime settings, without consuming its unsent draft.
  for (const action of ['model', 'effort', 'permission']) {
    const { c, calls, elements } = context(); c.currentSessionId = null; c.activeWorkspaceId = 'b';
    c.fetch = async (url, options) => {
      const body = JSON.parse(options.body); calls.push({ url, body });
      if (url === '/api/sessions') return { ok: true, json: async () => ({ id: 'new-b', workspaceId: 'b', workDir: '/beta' }) };
      if (url === '/api/sessions/activate') return { ok: true, json: async () => ({ session: { id: 'new-b' } }) };
      return { ok: true, json: async () => ({ model: 'beta-model', effort: 'high',
        ...(url === '/api/settings' ? { permissionSessionId: 'new-b', permissionMode: 'acceptEdits', permissionModeAvailable: true } : {}) }) };
    };
    if (action === 'model') await c.switchModel('provider', 'beta-model');
    if (action === 'effort') await c.chooseEffort('high');
    if (action === 'permission') assert.equal(await c.setPermissionMode('acceptEdits'), true);
    assert.equal(calls[0].url, '/api/sessions');
    assert.equal(calls[0].body.workspaceId, 'b');
    assert.equal(calls[1].url, '/api/sessions/activate');
    assert.equal(calls[1].body.id, 'new-b');
    assert.equal(calls[2].url, action === 'model' ? '/api/models' : action === 'effort' ? '/api/effort' : '/api/settings');
    assert.equal(elements.get('inputField').value, 'old draft');
    assert.equal(c.attachments[0].data, 'old-image');
    checks++;
  }
  // Busy legacy activation must never fall through to the previous A settings.
  for (const action of ['model', 'effort', 'permission']) {
    const { c, calls } = context(); c.currentSessionId = null; c.activeWorkspaceId = 'b'; c.parallelTurnsEnabled = () => false;
    c.fetch = async (url, options) => {
      calls.push({ url, body: JSON.parse(options.body) });
      return url === '/api/sessions' ? { ok: true, json: async () => ({ id: 'new-b', workspaceId: 'b', workDir: '/beta' }) }
        : { ok: false, status: 409, json: async () => ({ error: 'A is still running' }) };
    };
    if (action === 'model') await c.switchModel('provider', 'beta-model');
    if (action === 'effort') await c.chooseEffort('high');
    if (action === 'permission') assert.equal(await c.setPermissionMode('acceptEdits'), false);
    assert.deepEqual(calls.map(call => call.url), ['/api/sessions', '/api/sessions/activate']);
    checks++;
  }
  // A late control activation cannot mutate settings after switching to C.
  {
    const { c, calls } = context(), activation = deferred(); c.currentSessionId = null; c.activeWorkspaceId = 'b';
    c.fetch = async (url, options) => {
      const body = JSON.parse(options.body); calls.push({ url, body });
      if (url === '/api/sessions') return { ok: true, json: async () => ({ id: 'new-b', workspaceId: 'b', workDir: '/beta' }) };
      if (url === '/api/sessions/activate') return activation.promise;
      return { ok: true, json: async () => ({ activeId: 'a', workspace: c.workspaces[0] }) };
    };
    const setting = c.chooseEffort('high'); await tick();
    const select = c.openWorkspace('a'); await tick();
    activation.resolve({ ok: true, json: async () => ({}) }); await setting; await select;
    assert.equal(calls.some(call => call.url === '/api/effort'), false);
    assert.equal(c.activeWorkspaceId, 'a');
    checks++;
  }
  // Permission confirmation must retain the view where it was requested.
  {
    const { c, calls } = context(), confirmation = deferred();
    c.confirmFullAccessMode = () => confirmation.promise;
    c.fetch = async (url, options) => { calls.push({ url, body: JSON.parse(options.body) }); return { ok: true, json: async () => ({ activeId: 'b', workspace: c.workspaces[1] }) }; };
    const mode = c.setPermissionMode('fullAccess'); await tick();
    await c.openWorkspace('b'); confirmation.resolve(true);
    assert.equal(await mode, false);
    assert.equal(calls.some(call => call.url === '/api/settings'), false, 'A confirmation must not apply permission mode to B');
    checks++;
  }
  // Existing A task keeps its stream/stop owner while viewing B, then A history.
  {
    const { c, calls, elements } = context(), completion = deferred();
    c.fetch = async (url, options) => {
      const body = options?.body && JSON.parse(options.body); calls.push({ url, body });
      if (url === '/api/turns') return completion.promise;
      if (url === '/api/sessions/old-a') return { ok: true, json: async () => ({ session: c.sessions[0], messages: [] }) };
      if (url === '/api/stop') return { ok: true, json: async () => ({ stopping: true }) };
      const workspace = c.workspaces.find(item => item.id === body.id);
      return { ok: true, json: async () => ({ activeId: body.id, workspace }) };
    };
    const turn = c.runTurnItem({ text: 'A running' }); await tick();
    const request = c.pendingForegroundRequest;
    await c.openWorkspace('b');
    assert.equal(c.foregroundRequests.has('old-a'), true);
    assert.equal(request.controller.signal.aborted, false);
    assert.equal(c.sameSession({ session: 'old-a' }), false);
    await c.resumeSession('old-a');
    assert.equal(c.activeWorkspaceId, 'a');
    assert.equal(c.currentSessionId, 'old-a');
    assert.equal(elements.get('inputField').value, 'old draft', 'returning to A must restore A composer draft');
    assert.equal(c.sameSession({ session: 'old-a' }), true);
    await c.stopTurn();
    assert.equal(calls.find(call => call.url === '/api/stop').body.sessionId, 'old-a');
    assert.equal(calls.some(call => call.url === '/api/sessions/activate'), false, 'isolated Desktop history navigation must stay read-only');
    completion.resolve({ ok: true, json: async () => ({ sessionId: 'old-a', text: 'A done' }) }); await turn;
    checks++;
  }
  console.log(`${checks} workspace selection browser checks passed`);
})().catch(error => { console.error(error); process.exitCode = 1; });
