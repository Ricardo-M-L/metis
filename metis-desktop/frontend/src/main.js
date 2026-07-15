// Metis Desktop — Codex-style UI (ES module)

import {
  DeleteScheduledTask,
  GetCurrentModel,
  GetProjectDir,
  GetScheduledTasks,
  GetSessionMessages,
  GetSessions,
  GetSettings,
  PauseScheduledTask,
  ResumeScheduledTask,
  RunScheduledTask,
  SaveSettings,
  SendMessage
} from '../wailsjs/go/main/App.js';
import { Quit, WindowMinimise, WindowToggleMaximise } from '../wailsjs/runtime/runtime.js';

var currentModel = '';
var approvalMode = 'acceptEdits';
var messages = [];
var sessions = [];
var currentThreadId = null;
var appSettings = {};
var sendInFlight = false;
var viewRevision = 0;
var sessionsRequestRevision = 0;
var settingsRevision = 0;
var settingsSaveQueue = Promise.resolve();
var lastSettingsSaveError = null;
var quitting = false;
var activeView = 'chat';
var scheduledTasks = [];
var scheduledRequestRevision = 0;
var scheduledBusy = Object.create(null);

// --- Init ---
window.onload = function() {
  setupWindowControls();
  setReadyStatus();
  loadSessions();
  detectProject();
  setupInputHandler();
  loadSettings();
};

function setupWindowControls() {
  var controls = [
    { id: 'windowClose', action: requestQuit },
    { id: 'windowMinimise', action: WindowMinimise },
    { id: 'windowMaximise', action: WindowToggleMaximise }
  ];
  controls.forEach(function(control) {
    var el = document.getElementById(control.id);
    if (!el) return;
    function invoke(event) {
      if (event) event.stopPropagation();
      control.action();
    }
    el.addEventListener('click', invoke);
    el.addEventListener('keydown', function(event) {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        invoke(event);
      }
    });
  });
}

function requestQuit() {
  if (quitting) return;
  quitting = true;
  // A settings change is persisted immediately, but the Wails call is
  // asynchronous. Wait for the current queue before terminating so a quick
  // click on the red window control cannot discard the latest change.
  var pendingSave = lastSettingsSaveError ? saveCurrentSettings() : settingsSaveQueue;
  pendingSave.then(function() {
    if (lastSettingsSaveError) {
      quitting = false;
      return;
    }
    Quit();
  });
}

function setupInputHandler() {
  var input = document.getElementById('inputField');
  if (input) {
    input.addEventListener('keydown', function(e) {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendMessage(); }
    });
    input.addEventListener('input', function() { autoResize(input); updateSendBtn(); });
    input.focus();
  }
  GetCurrentModel().then(function(m) {
    currentModel = m;
    var el = document.getElementById('modelName');
    if (el) el.textContent = m;
  }).catch(function() {});
}

function setReadyStatus() {
  var badge = document.getElementById('statusBadge');
  if (!badge) return;
  badge.textContent = 'Ready';
  badge.style.background = '#4ade80';
}

// --- Sessions ---
function loadSessions() {
  var requestRevision = ++sessionsRequestRevision;
  GetSessions().then(function(result) {
    if (requestRevision !== sessionsRequestRevision) return;
    sessions = result || [];
    renderSessions();
  }).catch(function() {
    if (requestRevision !== sessionsRequestRevision) return;
    sessions = [];
    renderSessions();
  });
}

function renderSessions() {
  var list = document.getElementById('sessionList');
  if (!list) return;
  list.replaceChildren();
  if (!sessions.length) {
    var empty = document.createElement('div');
    empty.className = 'session-empty';
    empty.textContent = 'No sessions yet';
    list.appendChild(empty);
    return;
  }

  var group = document.createElement('div');
  group.className = 'session-group';
  var header = document.createElement('div');
  header.className = 'session-group-header';
  var icon = document.createElement('span');
  icon.className = 'session-group-icon';
  icon.textContent = '\ud83d\udcc1';
  var name = document.createElement('span');
  name.className = 'session-group-name';
  name.textContent = 'metis';
  header.append(icon, name);
  group.appendChild(header);

  for (var i = 0; i < sessions.length; i++) {
    var s = sessions[i];
    var title = s.Title || 'Untitled';
    var id = s.ID || '';
    var item = document.createElement('div');
    item.className = 'session-item';
    if (id === currentThreadId) item.classList.add('active');
    item.dataset.id = id;
    item.tabIndex = 0;
    item.setAttribute('role', 'button');
    item.setAttribute('aria-label', 'Open conversation ' + title);

    var itemName = document.createElement('span');
    itemName.className = 'session-item-name';
    itemName.textContent = title;
    var status = document.createElement('span');
    status.className = 'session-item-status';
    item.append(itemName, status);

    item.addEventListener('click', function(event) { selectSession(event.currentTarget); });
    item.addEventListener('keydown', function(event) {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        selectSession(event.currentTarget);
      }
    });
    group.appendChild(item);
  }
  list.appendChild(group);
}

function selectSession(el) {
  activeView = 'chat';
  setComposerVisible(true);
  var selectedID = el.getAttribute('data-id');
  var selectedRevision = ++viewRevision;
  currentThreadId = selectedID;
  for (var i = 0; i < sessions.length; i++) {
    if (sessions[i].ID === currentThreadId) {
      var titleEl = document.getElementById('topbarTitle');
      if (titleEl) titleEl.textContent = sessions[i].Title || 'Session';
      break;
    }
  }
  renderSessions();
  var area = document.getElementById('chatArea');
  if (area) area.innerHTML = '<div class="welcome"><div class="welcome-text">Loading conversation...</div></div>';
  GetSessionMessages(selectedID).then(function(result) {
    if (viewRevision !== selectedRevision || currentThreadId !== selectedID) return;
    renderTranscript(result || []);
  }).catch(function(e) {
    if (viewRevision !== selectedRevision || currentThreadId !== selectedID) return;
    messages = [];
    if (area) area.innerHTML = '';
    addMessage('assistant', 'Could not load this conversation: ' + (e.message || e));
  });
}

// --- Chat ---
function doNewChat() {
  activeView = 'chat';
  setComposerVisible(true);
  viewRevision++;
  currentThreadId = null;
  messages = [];
  var area = document.getElementById('chatArea');
  if (area) {
    area.innerHTML = '<div class="welcome" id="welcomeScreen"><div class="welcome-icon">M</div><div class="welcome-text">What should we do?</div><div class="welcome-sub">I can help you with coding, writing, analysis, and more.</div></div>';
  }
  var titleEl = document.getElementById('topbarTitle');
  if (titleEl) titleEl.textContent = 'Metis';
  renderSessions();
}

function sendMessage() {
  var input = document.getElementById('inputField');
  if (!input || sendInFlight) return;
  var text = input.value.trim();
  if (!text) return;
  var requestThreadId = currentThreadId || '';
  var requestViewRevision = viewRevision;
  var welcome = document.getElementById('welcomeScreen');
  if (welcome) welcome.remove();
  addMessage('user', text);
  input.value = '';
  autoResize(input);
  var sendBtn = document.getElementById('sendBtn');
  if (sendBtn) sendBtn.disabled = true;
  sendInFlight = true;
  SendMessage(text, requestThreadId, approvalMode).then(function(response) {
    var requestViewIsCurrent = viewRevision === requestViewRevision && (currentThreadId || '') === requestThreadId;
    if (!response) {
      if (requestViewIsCurrent) addNotice('error', 'Metis returned an empty response.');
      return;
    }
    if (requestViewIsCurrent) {
      if (response.threadId) currentThreadId = response.threadId;
      if (response.text) addMessage('assistant', response.text);
      if (response.warning) addNotice('warning', response.warning);
      if (response.error) addNotice('error', response.error);
      if (!response.text && !response.warning && !response.error) {
        addMessage('assistant', 'Metis completed without a text response.');
      }
    }
    loadSessions();
  }).catch(function(e) {
    if (viewRevision === requestViewRevision && (currentThreadId || '') === requestThreadId) {
      addNotice('error', e.message || e);
    }
  }).finally(function() {
    sendInFlight = false;
    updateSendBtn();
    if (viewRevision === requestViewRevision) input.focus();
  });
}

function renderTranscript(history) {
  messages = [];
  var area = document.getElementById('chatArea');
  if (!area) return;
  area.innerHTML = '';
  for (var i = 0; i < history.length; i++) {
    var item = history[i] || {};
    var role = item.Role === 'user' ? 'user' : 'assistant';
    var content = item.Content || '';
    if (content) addMessage(role, content);
  }
  if (!messages.length) {
    area.innerHTML = '<div class="welcome"><div class="welcome-icon">M</div><div class="welcome-text">No messages yet</div><div class="welcome-sub">Continue this conversation below.</div></div>';
  }
}

function addMessage(role, content) {
  messages.push({ role: role, content: content, time: new Date() });
  var area = document.getElementById('chatArea');
  if (!area) return;
  var t = new Date().toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
  if (role === 'user') {
    area.insertAdjacentHTML('beforeend', '<div class="message message-user"><div class="message-bubble">' + esc(content) + '</div></div>');
  } else {
    area.insertAdjacentHTML('beforeend', '<div class="message message-assistant"><div class="message-avatar">M</div><div class="message-body"><div class="message-content">' + fmt(content) + '</div><div class="message-time">' + t + '</div></div></div>');
  }
  area.scrollTop = area.scrollHeight;
}

function addNotice(kind, content) {
  content = String(content || '').trim();
  if (!content) return;
  messages.push({ role: kind, content: content, time: new Date() });
  var area = document.getElementById('chatArea');
  if (!area) return;
  var notice = document.createElement('div');
  notice.className = 'message message-notice message-notice-' + (kind === 'error' ? 'error' : 'warning');
  var label = document.createElement('span');
  label.className = 'message-notice-label';
  label.textContent = kind === 'error' ? 'Error' : 'Warning';
  var body = document.createElement('span');
  body.className = 'message-notice-content';
  body.textContent = content;
  notice.append(label, body);
  area.appendChild(notice);
  area.scrollTop = area.scrollHeight;
}

function fmt(text) {
  return text.split('\n').map(function(l) {
    if (l.startsWith('• ') || l.startsWith('- ') || /^\d+\./.test(l)) return '<p style="padding-left:16px">' + esc(l) + '</p>';
    return l ? '<p>' + esc(l) + '</p>' : '';
  }).join('');
}

// --- Input ---
function autoResize(el) { el.style.height = 'auto'; el.style.height = Math.min(el.scrollHeight, 200) + 'px'; }
function updateSendBtn() {
  var input = document.getElementById('inputField');
  var btn = document.getElementById('sendBtn');
  if (input && btn) btn.disabled = sendInFlight || !input.value.trim();
}

// The desktop client uses the model configured by Metis. Model discovery and
// provider switching belong in Settings; never substitute a hard-coded model.
function cycleModel() {
  var el = document.getElementById('modelName');
  if (el) el.textContent = currentModel;
}

// --- Approval ---
var approvalModes = ['acceptEdits', 'plan', 'bypass'];
var approvalLabels = {
  acceptEdits: 'Approve edits',
  plan: 'Plan only',
  bypass: 'Full access'
};

function approvalModeFromSettings() {
  if (appSettings.fullAccess) return 'bypass';
  if (appSettings.defaultPermissions === false) return 'plan';
  return 'acceptEdits';
}

function syncSettingsFromApprovalMode() {
  appSettings.fullAccess = approvalMode === 'bypass';
  appSettings.defaultPermissions = approvalMode !== 'plan';
}

function renderApprovalMode() {
  var label = document.getElementById('approvalLabel');
  if (label) label.textContent = approvalLabels[approvalMode] || approvalLabels.acceptEdits;
  var btn = document.getElementById('approvalBtn');
  if (!btn) return;
  btn.classList.toggle('active', approvalMode !== 'acceptEdits');
  btn.setAttribute('aria-label', 'Permission mode: ' + (approvalLabels[approvalMode] || approvalLabels.acceptEdits));
  btn.title = approvalMode === 'plan'
    ? 'Read-only planning; changes are denied'
    : approvalMode === 'bypass'
      ? 'Allow all tool actions without confirmation'
      : 'Allow workspace edits; interactive approvals are unavailable in desktop headless runs';
}

function setApprovalMode(mode, persist) {
  approvalMode = approvalModes.indexOf(mode) >= 0 ? mode : 'acceptEdits';
  if (!Object.keys(appSettings).length) appSettings = defaultSettings();
  syncSettingsFromApprovalMode();
  renderApprovalMode();
  if (document.getElementById('settingsOverlay')?.classList.contains('visible')) renderActiveSettingsTab();
  if (persist) saveCurrentSettings();
}

function toggleApproval() {
  var next = approvalModes[(approvalModes.indexOf(approvalMode) + 1) % approvalModes.length];
  setApprovalMode(next, true);
}

// --- Settings ---
function activeSettingsTab() {
  return document.querySelector('.settings-item.active')?.getAttribute('data-tab') || 'general';
}

function renderActiveSettingsTab() {
  var content = document.getElementById('settingsContent');
  if (content) content.innerHTML = getSettingsTabContent(activeSettingsTab());
}

function openSettings() {
  if (!Object.keys(appSettings).length) appSettings = defaultSettings();
  renderActiveSettingsTab();
  var el = document.getElementById('settingsOverlay');
  if (el) el.classList.add('visible');
}
function closeSettings() { var el = document.getElementById('settingsOverlay'); if (el) el.classList.remove('visible'); }
function selectSettingsTab(el) {
  document.querySelectorAll('.settings-item').forEach(function(i) { i.classList.remove('active'); });
  el.classList.add('active');
  renderActiveSettingsTab();
}


function selectRadio(el) {
  el.parentElement.querySelectorAll('.radio-card').forEach(function(c) { c.classList.remove('selected'); });
  el.classList.add('selected');
}

// --- Project ---
function detectProject() {
  GetProjectDir().then(function(d) {
    var el = document.getElementById('projectName');
    if (el) el.textContent = d.split('/').pop() || 'metis';
  }).catch(function() {});
}

function setComposerVisible(visible) {
  var composer = document.getElementById('inputArea');
  if (composer) composer.classList.toggle('hidden', !visible);
}

function loadSettings() {
  var loadRevision = settingsRevision;
  GetSettings().then(function(json) {
    if (loadRevision !== settingsRevision) return;
    var loaded;
    try {
      loaded = JSON.parse(json);
    } catch(e) {
      loaded = {};
    }
    appSettings = Object.assign(defaultSettings(), loaded && typeof loaded === 'object' ? loaded : {});
    applySettings();
  }).catch(function() {
    if (loadRevision !== settingsRevision) return;
    appSettings = defaultSettings();
    applySettings();
  });
}

function defaultSettings() {
  return { theme: 'dark', workMode: 'coding', language: 'en', fileOpenTarget: 'terminal', defaultPermissions: true, autoReview: true, fullAccess: false, markdownEnabled: true, showTokens: true };
}

function applySettings() {
  if (appSettings.fullAccess) appSettings.defaultPermissions = true;
  if (appSettings.theme === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
  } else {
    document.documentElement.removeAttribute('data-theme');
  }
  approvalMode = approvalModeFromSettings();
  renderApprovalMode();
  if (document.getElementById('settingsOverlay')?.classList.contains('visible')) renderActiveSettingsTab();
}

function saveCurrentSettings() {
  settingsRevision++;
  var payload = JSON.stringify(appSettings);
  settingsSaveQueue = settingsSaveQueue
    .then(function() {
      return SaveSettings(payload).then(function() {
        lastSettingsSaveError = null;
      }).catch(function(error) {
        lastSettingsSaveError = error || new Error('Unknown settings error');
        addNotice('error', 'Could not save settings: ' + (lastSettingsSaveError.message || lastSettingsSaveError));
      });
    });
  return settingsSaveQueue;
}

function updateSetting(key, value) {
  appSettings[key] = value;
  if (key === 'fullAccess') {
    appSettings.fullAccess = Boolean(value);
    if (appSettings.fullAccess) appSettings.defaultPermissions = true;
    approvalMode = approvalModeFromSettings();
    renderApprovalMode();
  } else if (key === 'defaultPermissions') {
    appSettings.defaultPermissions = Boolean(value);
    if (!appSettings.defaultPermissions) appSettings.fullAccess = false;
    approvalMode = approvalModeFromSettings();
    renderApprovalMode();
  } else if (key === 'theme') {
    applySettings();
  }
  if (document.getElementById('settingsOverlay')?.classList.contains('visible')) renderActiveSettingsTab();
  saveCurrentSettings();
}

function getSettingsTabContent(tab) {
  var s = appSettings;
  var tabs = {
    general: '<h2>General</h2>' +
      '<div class="settings-section"><div class="settings-section-title">Work Mode</div><div class="settings-section-desc">Choose how much technical detail Metis shows</div><div class="radio-cards">' +
      '<div class="radio-card' + (s.workMode === 'coding' ? ' selected' : '') + '" onclick="selectRadio(this); updateSetting(\'workMode\', \'coding\')"><div class="radio-card-title">For Coding</div><div class="radio-card-desc">More technical responses and controls</div></div>' +
      '<div class="radio-card' + (s.workMode === 'daily' ? ' selected' : '') + '" onclick="selectRadio(this); updateSetting(\'workMode\', \'daily\')"><div class="radio-card-title">For Daily Work</div><div class="radio-card-desc">Just as capable, with less technical detail</div></div>' +
      '</div></div>' +
      '<div class="settings-section"><div class="settings-section-title">Permissions</div><div class="settings-card">' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Allow Workspace Edits</div><div class="settings-card-desc">Turn off for read-only plan mode.</div></div><div class="toggle' + (s.defaultPermissions ? ' on' : '') + '" onclick="this.classList.toggle(\'on\'); updateSetting(\'defaultPermissions\', this.classList.contains(\'on\'))"></div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Auto-Review</div></div><div class="toggle' + (s.autoReview ? ' on' : '') + '" onclick="this.classList.toggle(\'on\'); updateSetting(\'autoReview\', this.classList.contains(\'on\'))"></div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Full Access</div><div class="settings-card-desc">Allow all tool actions without confirmation.</div></div><div class="toggle' + (s.fullAccess ? ' on' : '') + '" onclick="this.classList.toggle(\'on\'); updateSetting(\'fullAccess\', this.classList.contains(\'on\'))"></div></div>' +
      '</div></div>' +
      '<div class="settings-section"><div class="settings-section-title">General</div><div class="settings-card">' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Default File Open Target</div></div><div class="select-box" onclick="cycleOption(this, [\'terminal\',\'editor\',\'vscode\'], \'fileOpenTarget\')">' + esc(s.fileOpenTarget || 'terminal') + '</div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Language</div></div><div class="select-box" onclick="cycleOption(this, [\'en\',\'zh\',\'ja\'], \'language\')">' + esc(s.language || 'en') + '</div></div>' +
      '</div></div>',
    appearance: '<h2>Appearance</h2>' +
      '<div class="settings-section"><div class="settings-section-title">Theme</div><div class="settings-card">' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Dark Mode</div></div><div class="toggle' + (s.theme !== 'light' ? ' on' : '') + '" onclick="this.classList.toggle(\'on\'); updateSetting(\'theme\', this.classList.contains(\'on\') ? \'dark\' : \'light\')"></div></div>' +
      '</div></div>',
    config: '<h2>Configuration</h2>' +
      '<div class="settings-section"><div class="settings-section-title">Display</div><div class="settings-card">' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Markdown Rendering</div></div><div class="toggle' + (s.markdownEnabled !== false ? ' on' : '') + '" onclick="this.classList.toggle(\'on\'); updateSetting(\'markdownEnabled\', this.classList.contains(\'on\'))"></div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Show Token Usage</div></div><div class="toggle' + (s.showTokens !== false ? ' on' : '') + '" onclick="this.classList.toggle(\'on\'); updateSetting(\'showTokens\', this.classList.contains(\'on\'))"></div></div>' +
      '</div></div>',
    personalization: '<h2>Personalization</h2><div class="settings-section"><div class="settings-card"><div class="settings-card-row"><div><div class="settings-card-label">Memory</div><div class="settings-card-desc">Allow Metis to remember context across sessions.</div></div><div class="toggle on" onclick="this.classList.toggle(\'on\')"></div></div></div></div>',
    keyboard: '<h2>Keyboard Shortcuts</h2><div class="settings-section"><div class="settings-card">' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Send Message</div></div><div class="kbd">Enter</div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">New Line</div></div><div class="kbd">Shift+Enter</div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Toggle Sidebar</div></div><div class="kbd">Cmd+B</div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Settings</div></div><div class="kbd">Cmd+,</div></div>' +
      '</div></div>',
    mcp: '<h2>MCP Servers</h2><div class="settings-section"><div class="settings-card"><div class="settings-card-row"><div><div class="settings-card-label">No MCP servers configured</div><div class="settings-card-desc">Add MCP servers in your ~/.metis/config.toml under [mcp].</div></div></div></div></div>',
    browser: '<h2>Browser</h2><div class="settings-section"><div class="settings-card"><div class="settings-card-row"><div><div class="settings-card-label">Browser Automation</div><div class="settings-card-desc">Enable browser control for web tasks.</div></div><div class="toggle on" onclick="this.classList.toggle(\'on\')"></div></div></div></div>',
    git: '<h2>Git</h2><div class="settings-section"><div class="settings-card"><div class="settings-card-row"><div><div class="settings-card-label">Auto-commit</div><div class="settings-card-desc">Automatically commit changes after each task.</div></div><div class="toggle" onclick="this.classList.toggle(\'on\')"></div></div></div></div>',
    env: '<h2>Environment</h2><div class="settings-section"><div class="settings-card"><div class="settings-card-row"><div><div class="settings-card-label">Working Directory</div></div><div class="select-box">' + esc(appSettings.workDir || '~') + '</div></div>' +
      '<div class="settings-card-row"><div><div class="settings-card-label">Shell</div></div><div class="select-box">' + esc(appSettings.shell || '/bin/zsh') + '</div></div>' +
      '</div></div>'
  };
  return tabs[tab] || '<h2>' + tab.charAt(0).toUpperCase() + tab.slice(1) + '</h2><p>Settings for this section will appear here.</p>';
}

function cycleOption(el, options, settingKey) {
  var current = el.textContent.trim();
  var idx = options.indexOf(current);
  var next = options[(idx + 1) % options.length];
  el.textContent = next;
  updateSetting(settingKey, next);
}

// --- Helpers ---
function esc(s) { var d = document.createElement('div'); d.textContent = s; return d.innerHTML; }

// --- Global Exports for Vite ---
window.doNewChat = doNewChat;
window.sendMessage = sendMessage;
window.toggleApproval = toggleApproval;
window.cycleModel = cycleModel;
window.openSettings = openSettings;
window.closeSettings = closeSettings;
window.selectSettingsTab = selectSettingsTab;
window.selectRadio = selectRadio;
window.selectSession = selectSession;

function selectNav(el) {
  document.querySelectorAll('.sidebar-nav .nav-item').forEach(function(nav) {
    nav.classList.remove('active');
  });
  el.classList.add('active');
}

function showSearch() {
  activeView = 'search';
  setComposerVisible(false);
  viewRevision++;
  var area = document.getElementById('chatArea');
  if (area) {
    area.innerHTML = '<div class="welcome"><div class="welcome-icon">&#128269;</div><div class="welcome-text">Search</div><div class="welcome-sub">Search your workspace and past conversations.</div></div>';
  }
  var titleEl = document.getElementById('topbarTitle');
  if (titleEl) titleEl.textContent = 'Search';
  currentThreadId = null;
}

function showScheduled() {
  activeView = 'scheduled';
  setComposerVisible(false);
  var scheduledViewRevision = ++viewRevision;
  var area = document.getElementById('chatArea');
  if (area) {
    area.replaceChildren();
    var page = document.createElement('section');
    page.className = 'scheduled-page';

    var header = document.createElement('div');
    header.className = 'scheduled-header';
    var heading = document.createElement('div');
    var headingTitle = document.createElement('h2');
    headingTitle.textContent = 'Scheduled Tasks';
    var headingSub = document.createElement('p');
    headingSub.textContent = 'Durable Metis cron jobs. Actions use the same CLI and permissions as terminal runs.';
    heading.append(headingTitle, headingSub);
    var refresh = scheduledButton('Refresh', 'scheduled-btn-primary', function() {
      loadScheduledTasks(viewRevision);
    });
    refresh.id = 'scheduledRefresh';
    header.append(heading, refresh);

    var message = document.createElement('div');
    message.className = 'scheduled-message';
    message.id = 'scheduledMessage';
    message.hidden = true;
    var list = document.createElement('div');
    list.className = 'scheduled-list';
    list.id = 'scheduledList';
    var loading = document.createElement('div');
    loading.className = 'scheduled-empty';
    loading.textContent = 'Loading scheduled tasks...';
    list.appendChild(loading);
    page.append(header, message, list);
    area.appendChild(page);
  }
  var titleEl = document.getElementById('topbarTitle');
  if (titleEl) titleEl.textContent = 'Scheduled';
  currentThreadId = null;
  loadScheduledTasks(scheduledViewRevision);
}

function loadScheduledTasks(scheduledViewRevision) {
  if (activeView !== 'scheduled') return Promise.resolve();
  var requestRevision = ++scheduledRequestRevision;
  var refresh = document.getElementById('scheduledRefresh');
  if (refresh) refresh.disabled = true;
  return GetScheduledTasks().then(function(result) {
    if (activeView !== 'scheduled' || viewRevision !== scheduledViewRevision || requestRevision !== scheduledRequestRevision) return;
    scheduledTasks = result || [];
    renderScheduledTasks();
  }).catch(function(error) {
    if (activeView !== 'scheduled' || viewRevision !== scheduledViewRevision || requestRevision !== scheduledRequestRevision) return;
    scheduledTasks = [];
    renderScheduledTasks();
    setScheduledMessage('error', 'Could not load scheduled tasks: ' + errorText(error));
  }).finally(function() {
    if (activeView === 'scheduled' && viewRevision === scheduledViewRevision && requestRevision === scheduledRequestRevision) {
      var currentRefresh = document.getElementById('scheduledRefresh');
      if (currentRefresh) currentRefresh.disabled = false;
    }
  });
}

function renderScheduledTasks() {
  var list = document.getElementById('scheduledList');
  if (!list || activeView !== 'scheduled') return;
  list.replaceChildren();
  if (!scheduledTasks.length) {
    var empty = document.createElement('div');
    empty.className = 'scheduled-empty';
    empty.textContent = 'No durable scheduled tasks yet. Create one with `metis cron add` or the scheduling tools.';
    list.appendChild(empty);
    return;
  }

  scheduledTasks.forEach(function(task) {
    var card = document.createElement('article');
    card.className = 'scheduled-card';

    var cardHeader = document.createElement('div');
    cardHeader.className = 'scheduled-card-header';
    var title = document.createElement('h3');
    title.textContent = task.Name || 'Untitled task';
    var status = document.createElement('span');
    status.className = 'scheduled-status';
    if (!task.Enabled) {
      status.classList.add('disabled');
      status.textContent = 'Disabled';
    } else if (task.Paused) {
      status.classList.add('paused');
      status.textContent = 'Paused';
    } else {
      status.classList.add('active');
      status.textContent = 'Scheduled';
    }
    cardHeader.append(title, status);

    var prompt = document.createElement('p');
    prompt.className = 'scheduled-prompt';
    prompt.textContent = task.Prompt || 'No prompt recorded.';

    var meta = document.createElement('div');
    meta.className = 'scheduled-meta';
    appendScheduledMeta(meta, 'Schedule', task.Schedule || 'Unknown');
    appendScheduledMeta(meta, 'Next run', task.Paused || !task.Enabled ? 'Not scheduled' : formatScheduledTime(task.NextRun));
    appendScheduledMeta(meta, 'Last run', formatScheduledTime(task.LastRun));
    appendScheduledMeta(meta, 'Runs', String(task.RunCount || 0) + (task.Repeat ? ' / ' + task.Repeat : ''));
    appendScheduledMeta(meta, 'Session', task.SessionMode || 'isolated');
    if (task.Silent) appendScheduledMeta(meta, 'Output', 'Audit log');

    var actions = document.createElement('div');
    actions.className = 'scheduled-actions';
    var busy = Boolean(scheduledBusy[task.ID]);
    if (task.Enabled) {
      if (task.Paused) {
        actions.appendChild(scheduledButton('Resume', '', function() { runScheduledAction(task, 'resume'); }, busy));
      } else {
        actions.appendChild(scheduledButton('Pause', '', function() { runScheduledAction(task, 'pause'); }, busy));
      }
    }
    actions.appendChild(scheduledButton(busy ? 'Working...' : 'Run now', 'scheduled-btn-primary', function() { runScheduledAction(task, 'run'); }, busy));
    actions.appendChild(scheduledButton('Delete', 'scheduled-btn-danger', function() { runScheduledAction(task, 'delete'); }, busy));

    card.append(cardHeader, prompt, meta, actions);
    list.appendChild(card);
  });
}

function appendScheduledMeta(container, labelText, valueText) {
  var item = document.createElement('div');
  var label = document.createElement('span');
  label.textContent = labelText;
  var value = document.createElement('strong');
  value.textContent = valueText;
  item.append(label, value);
  container.appendChild(item);
}

function scheduledButton(label, className, action, disabled) {
  var button = document.createElement('button');
  button.type = 'button';
  button.className = 'scheduled-btn' + (className ? ' ' + className : '');
  button.textContent = label;
  button.disabled = Boolean(disabled);
  button.addEventListener('click', action);
  return button;
}

function runScheduledAction(task, action) {
  if (!task || !task.ID || scheduledBusy[task.ID]) return;
  if (action === 'delete' && !window.confirm('Delete scheduled task “' + (task.Name || task.ID) + '”?')) return;
  var actionViewRevision = viewRevision;
  scheduledBusy[task.ID] = true;
  renderScheduledTasks();
  setScheduledMessage('progress', action === 'run' ? 'Running “' + task.Name + '” now...' : 'Updating “' + task.Name + '”...');

  var operation;
  if (action === 'pause') operation = PauseScheduledTask(task.ID);
  else if (action === 'resume') operation = ResumeScheduledTask(task.ID);
  else if (action === 'delete') operation = DeleteScheduledTask(task.ID);
  else operation = RunScheduledTask(task.ID);

  Promise.resolve(operation).then(function(output) {
    var message = action === 'delete' ? 'Deleted “' + task.Name + '”.' :
      action === 'pause' ? 'Paused “' + task.Name + '”.' :
        action === 'resume' ? 'Resumed “' + task.Name + '”.' : 'Finished running “' + task.Name + '”.';
    if (action === 'run' && String(output || '').trim()) message += '\n\n' + String(output).trim();
    if (activeView === 'scheduled' && viewRevision === actionViewRevision) {
      setScheduledMessage('success', message);
    }
    if (activeView === 'scheduled') return loadScheduledTasks(viewRevision);
  }).catch(function(error) {
    if (activeView === 'scheduled' && viewRevision === actionViewRevision) {
      setScheduledMessage('error', 'Could not ' + action + ' “' + task.Name + '”: ' + errorText(error));
    }
  }).finally(function() {
    delete scheduledBusy[task.ID];
    if (activeView === 'scheduled') renderScheduledTasks();
  });
}

function setScheduledMessage(kind, content) {
  var message = document.getElementById('scheduledMessage');
  if (!message || activeView !== 'scheduled') return;
  message.hidden = false;
  message.className = 'scheduled-message scheduled-message-' + kind;
  message.textContent = content;
}

function formatScheduledTime(value) {
  if (!value) return 'Never';
  var parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString();
}

function errorText(error) {
  if (!error) return 'Unknown error';
  return String(error.message || error);
}

function showPlugins() {
  activeView = 'plugins';
  setComposerVisible(false);
  viewRevision++;
  var area = document.getElementById('chatArea');
  if (area) {
    area.innerHTML = '<div class="welcome"><div class="welcome-icon">&#129513;</div><div class="welcome-text">Plugins</div><div class="welcome-sub">Manage installed tools and integrations.</div></div>';
  }
  var titleEl = document.getElementById('topbarTitle');
  if (titleEl) titleEl.textContent = 'Plugins';
  currentThreadId = null;
}

// Ensure these are exported to window for Vite module scope
window.selectNav = selectNav;
window.showSearch = showSearch;
window.showScheduled = showScheduled;
window.showPlugins = showPlugins;
window.selectSession = selectSession;
window.updateSetting = updateSetting;
window.cycleOption = cycleOption;
