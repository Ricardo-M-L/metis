// The backend owns file authorization. UI tabs and caches are session-scoped.
const sessionFileState = { sessionId: '', files: [], tabs: [], listGeneration: 0, generation: 0, controller: null, active: null, preview: null, raw: false, wrap: false, maximized: false, restoreFocus: null };

function sessionFilePathFromLink(value) {
  let path = String(value || '').trim();
  if (!path || /^[a-z][a-z\d+.-]*:/i.test(path) || path.startsWith('//') || /[\x00-\x1f]/.test(path)) return '';
  try { path = decodeURIComponent(path); } catch (_) { return ''; }
  if (/^[a-z][a-z\d+.-]*:/i.test(path) || path.startsWith('//') || /[\x00-\x1f]/.test(path)) return '';
  return path.split('#')[0].replace(/:\d+(?::\d+)?$/, '').replace(/^\/private\/tmp\//, '/tmp/').replace(/^\.\//, '');
}
function matchSessionFile(value) {
  if (sessionFileState.sessionId !== String(currentSessionId || '')) return null;
  const path = sessionFilePathFromLink(value);
  if (!path) return null;
  const matches = sessionFileState.files.filter(file => {
    const known = sessionFilePathFromLink(file.path);
    return known === path || (!path.startsWith('/') && !path.split('/').includes('..') && known.endsWith('/' + path));
  });
  return matches.length === 1 ? matches[0] : null;
}
function sessionFileIcon(name) {
  const icon = document.createElement('span');
  icon.className = 'sf-icon sf-icon-' + name;
  icon.setAttribute('aria-hidden', 'true');
  return icon;
}
function sessionFileTypeIcon(file) {
  const language = sessionFileLanguage(file).id;
  const icon = sessionFileIcon(language === 'bash' ? 'terminal' : (file.kind === 'markdown' || language === 'plaintext') ? 'file-text' : 'file-code-2');
  icon.classList.add('sf-file-type');
  icon.dataset.language = language;
  return icon;
}
function currentSessionFileTab() { return sessionFileState.tabs.find(tab => tab.file.id === sessionFileState.active?.id); }
function saveSessionFileView() {
  const tab = currentSessionFileTab(), content = document.getElementById('sessionFileContent');
  // Loading/error placeholders can clamp the scrollport to zero. They do not
  // represent the last reading position of the cached document.
  if (tab && content && sessionFileState.preview) { tab.scrollTop = content.scrollTop; tab.scrollLeft = content.scrollLeft; }
}
function resetSessionFiles() {
  sessionFileState.listGeneration++;
  closeSessionFile(false);
  sessionFileState.sessionId = '';
  sessionFileState.files = [];
  sessionFileState.tabs = [];
  sessionFileState.active = null;
  sessionFileState.preview = null;
  document.getElementById('sessionFileContent').replaceChildren();
  document.getElementById('sessionFileSearch').value = '';
  document.getElementById('sessionFileActionStatus').textContent = '';
  renderSessionFileTabs();
  renderSessionFileList();
  updateSessionFileToolbar();
}
async function loadSessionFiles(sessionId = currentSessionId) {
  const sid = String(sessionId || '');
  if (sid !== sessionFileState.sessionId) resetSessionFiles();
  sessionFileState.sessionId = sid;
  const generation = ++sessionFileState.listGeneration;
  if (!sid) return;
  try {
    const res = await fetch('/api/session-files?sessionId=' + encodeURIComponent(sid));
    const data = await res.json();
    if (generation !== sessionFileState.listGeneration || sid !== String(currentSessionId || '')) return;
    if (!res.ok) throw new Error(data.error || 'Unable to list session files');
    sessionFileState.files = Array.isArray(data.files) ? data.files : [];
    renderSessionFileList();
    decorateSessionFiles();
    return true;
  } catch (error) {
    if (generation !== sessionFileState.listGeneration || sid !== String(currentSessionId || '')) return;
    document.getElementById('sessionFileListStatus').textContent = error.message;
    return false;
  }
}
function sessionFileButton(file) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'session-file-chip';
  button.append(sessionFileTypeIcon(file), document.createTextNode(file.name));
  button.title = file.path;
  button.dataset.sessionFileId = file.id;
  button.addEventListener('click', () => { void openSessionFile(file.id); });
  return button;
}
function renderSessionFileList() {
  const list = document.getElementById('sessionFileList');
  const query = document.getElementById('sessionFileSearch').value.trim().toLowerCase();
  const files = sessionFileState.files.filter(file => !query || file.path.toLowerCase().includes(query));
  list.replaceChildren(...files.map(file => {
    const button = sessionFileButton(file);
    button.classList.add('sf-picker-file');
    const path = document.createElement('small');
    path.textContent = file.path;
    button.append(path);
    return button;
  }));
  document.getElementById('sessionFileListStatus').textContent = files.length ? '' : query ? uiText('No matching files', '没有匹配的文件') : uiText('Files linked or created in this session will appear here.', '会话中引用或生成的文件会显示在这里。');
  document.getElementById('sessionFileCount').textContent = String(sessionFileState.files.length);
}
function renderSessionFileTabs() {
  const container = document.getElementById('sessionFileTabs');
  container.replaceChildren(...sessionFileState.tabs.map(tab => {
    const active = sessionFileState.active?.id === tab.file.id;
    const item = document.createElement('div');
    item.className = 'sf-tab' + (active ? ' is-active' : '');
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'sf-tab-select';
    button.setAttribute('role', 'tab');
    button.setAttribute('aria-selected', String(active));
    button.setAttribute('aria-controls', 'sessionFileContent');
    button.tabIndex = active ? 0 : -1;
    button.title = tab.file.path;
    button.append(sessionFileTypeIcon(tab.file), document.createTextNode(tab.file.name));
    button.addEventListener('click', () => { void openSessionFile(tab.file.id); });
    button.addEventListener('keydown', event => {
      const index = sessionFileState.tabs.indexOf(tab), length = sessionFileState.tabs.length;
      let next = -1;
      if (event.key === 'ArrowRight') next = (index + 1) % length;
      if (event.key === 'ArrowLeft') next = (index + length - 1) % length;
      if (event.key === 'Home') next = 0;
      if (event.key === 'End') next = length - 1;
      if (event.key === 'Delete') { event.preventDefault(); closeSessionFileTab(tab.file.id); }
      if (next >= 0) {
        event.preventDefault();
        void openSessionFile(sessionFileState.tabs[next].file.id);
        container.querySelector('[aria-selected="true"]')?.focus();
      }
    });
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'sf-tab-close';
    close.title = uiText('Close ', '关闭 ') + tab.file.name;
    close.setAttribute('aria-label', close.title);
    close.append(sessionFileIcon('x'));
    close.addEventListener('click', () => closeSessionFileTab(tab.file.id));
    item.append(button, close);
    return item;
  }));
  container.querySelector('[aria-selected="true"]')?.scrollIntoView({block: 'nearest', inline: 'nearest'});
}
function decorateSessionFiles() {
  document.querySelectorAll('#chatArea .message-assistant .message-content').forEach(content => {
    const found = new Map();
    content.querySelectorAll('[data-session-file-path], code.md-inline-code').forEach(node => {
      const file = matchSessionFile(node.dataset.sessionFilePath || node.textContent);
      if (!file) return;
      found.set(file.id, file);
      let button = node;
      if (node.tagName !== 'BUTTON') {
        button = document.createElement('button'); button.textContent = node.textContent; node.replaceWith(button);
      }
      button.type = 'button'; button.disabled = false;
      button.className = 'session-file-link';
      button.dataset.sessionFilePath = file.path; button.title = file.path;
      button.onclick = () => { void openSessionFile(file.id); };
    });
    const body = content.parentElement;
    body.querySelector('.session-file-chips')?.remove();
    if (found.size) {
      const chips = document.createElement('div');
      chips.className = 'session-file-chips';
      chips.append(...Array.from(found.values(), sessionFileButton));
      content.after(chips);
    }
  });
}
function showSessionFilePanel() {
  const panel = document.getElementById('sessionFilePanel');
  if (panel.hidden) sessionFileState.restoreFocus = document.activeElement;
  panel.hidden = false;
  if (typeof closeToolDetail === 'function') closeToolDetail();
}
function setSessionFilePicker(open) {
  document.getElementById('sessionFilePicker').hidden = !open;
  document.getElementById('sessionFileAdd').setAttribute('aria-expanded', String(open));
  if (open) { renderSessionFileList(); document.getElementById('sessionFileSearch').focus(); }
}
function toggleSessionFilePicker() { setSessionFilePicker(document.getElementById('sessionFilePicker').hidden); }
async function openSessionFiles() {
  const sid = String(currentSessionId || '');
  const pending = loadSessionFiles();
  const generation = sessionFileState.generation;
  await pending;
  if (sid !== String(currentSessionId || '') || generation !== sessionFileState.generation) return;
  if (sessionFileState.active && !sessionFileState.preview) {
    await openSessionFile(sessionFileState.active.id);
    if (document.getElementById('sessionFilePanel').hidden || sid !== String(currentSessionId || '')) return;
  }
  showSessionFilePanel();
  setSessionFilePicker(true);
  if (!sessionFileState.active) renderSessionFileEmpty();
}
function renderSessionFileEmpty() {
  const empty = document.createElement('div');
  empty.className = 'sf-empty';
  empty.append(sessionFileIcon('folder-open'));
  const title = document.createElement('strong'), hint = document.createElement('p');
  title.textContent = uiText('Open a session file', '打开会话文件');
  hint.textContent = uiText('Select a file with +, or click a file in the conversation.', '点击 + 选择文件，或打开对话中的文件链接。');
  empty.append(title, hint);
  document.getElementById('sessionFileContent').className = 'session-file-content';
  document.getElementById('sessionFileContent').replaceChildren(empty);
}
function updateSessionFileToolbar() {
  const file = sessionFileState.active, language = file ? sessionFileLanguage(file) : {label: ''};
  const path = document.getElementById('sessionFilePath');
  path.textContent = file?.path || ''; path.title = file?.path || '';
  document.getElementById('sessionFileLanguage').textContent = language.label;
  document.getElementById('sessionFileTitle').textContent = file?.name || uiText('Session files', '会话文件');
  const mode = document.getElementById('sessionFileMode');
  mode.hidden = file?.kind !== 'markdown';
  mode.textContent = sessionFileState.raw ? uiText('Preview', '预览') : uiText('Source', '源码');
  mode.setAttribute('aria-label', sessionFileState.raw ? uiText('Show Markdown preview', '显示 Markdown 预览') : uiText('Show source', '显示源码'));
  document.getElementById('sessionFileWrap').hidden = !file || !sessionFileState.raw;
  document.getElementById('sessionFileWrap').setAttribute('aria-pressed', String(sessionFileState.wrap));
  document.getElementById('sessionFileCopy').disabled = !sessionFileState.preview;
  document.getElementById('sessionFileRefresh').disabled = !file;
  document.getElementById('sessionFileCodeHeader').hidden = !file || !sessionFileState.raw;
  document.getElementById('sessionFileCodeLanguage').textContent = language.label;
}
async function openSessionFile(id, options = {}) {
  const file = sessionFileState.files.find(item => item.id === id), sid = sessionFileState.sessionId;
  if (!file || sid !== String(currentSessionId || '')) return;
  saveSessionFileView();
  sessionFileState.controller?.abort();
  const generation = ++sessionFileState.generation;
  let tab = sessionFileState.tabs.find(item => item.file.id === id);
  if (!tab) {
    if (sessionFileState.tabs.length >= 12) sessionFileState.tabs.shift();
    tab = {file, preview: null, raw: file.kind !== 'markdown', wrap: false, scrollTop: 0, scrollLeft: 0};
    sessionFileState.tabs.push(tab);
  }
  tab.file = file;
  sessionFileState.active = file;
  sessionFileState.raw = tab.raw; sessionFileState.wrap = tab.wrap;
  sessionFileState.preview = options.refresh ? null : tab.preview;
  document.getElementById('sessionFileActionStatus').textContent = '';
  showSessionFilePanel(); setSessionFilePicker(false);
  renderSessionFileTabs(); updateSessionFileToolbar();
  if (sessionFileState.preview) { renderSessionFileContent(); return; }
  const target = document.getElementById('sessionFileContent');
  target.textContent = uiText('Loading file…', '正在读取文件…');
  target.className = 'session-file-content is-loading';
  const controller = new AbortController();
  sessionFileState.controller = controller;
  try {
    const res = await fetch('/api/session-files/content?sessionId=' + encodeURIComponent(sid) + '&id=' + encodeURIComponent(id), {signal: controller.signal});
    const data = await res.json();
    if (generation !== sessionFileState.generation || sid !== String(currentSessionId || '')) return;
    if (!res.ok) throw new Error(data.error || 'Unable to preview file');
    tab.preview = data; sessionFileState.preview = data;
    renderSessionFileContent();
  } catch (error) {
    if (generation !== sessionFileState.generation || sid !== String(currentSessionId || '') || error.name === 'AbortError') return;
    tab.preview = null;
    target.textContent = error.message;
    target.className = 'session-file-content is-error';
  } finally {
    if (generation === sessionFileState.generation) sessionFileState.controller = null;
  }
}
function renderSessionFileContent() {
  const preview = sessionFileState.preview;
  if (!preview) return;
  const target = document.getElementById('sessionFileContent');
  target.replaceChildren();
  target.className = 'session-file-content' + (sessionFileState.raw ? ' is-code' : ' is-markdown');
  updateSessionFileToolbar();
  let node;
  if (sessionFileState.raw) node = renderSessionFileSource(preview.content, sessionFileState.active, {wrap: sessionFileState.wrap});
  else {
    node = document.createElement('article');
    node.className = 'session-file-markdown message-content';
    // Shared Markdown escapes raw HTML; no scripts or remote images are created.
    try { node.innerHTML = formatContent(preview.content); }
    catch (_) { node.textContent = preview.content; }
    node.querySelectorAll('.md-codeblock').forEach(block => {
      const code = block.querySelector('code');
      if (!code) return;
      const language = block.querySelector('.md-code-head span')?.textContent || '';
      // Highlight only escaped source; keep the original code text/copy path.
      code.innerHTML = sessionFileHighlightedLines(code.textContent, {name: 'snippet.' + language}).join('\n');
    });
    node.querySelectorAll('li').forEach(item => {
      const first = item.firstChild;
      const task = first?.nodeType === 3 && first.textContent.match(/^\[([ xX])\]\s/);
      if (!task) return;
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox'; checkbox.disabled = true; checkbox.checked = task[1] !== ' ';
      checkbox.setAttribute('aria-label', checkbox.checked ? uiText('Completed', '已完成') : uiText('Not completed', '未完成'));
      first.textContent = first.textContent.slice(task[0].length);
      item.classList.add('sf-task-item');
      item.prepend(checkbox);
    });
    node.querySelectorAll('[data-session-file-path]').forEach(link => {
      const file = matchSessionFile(link.dataset.sessionFilePath);
      if (file) { link.disabled = false; link.onclick = () => { void openSessionFile(file.id); }; }
    });
  }
  if (preview.truncated) {
    const warning = document.createElement('p');
    warning.className = 'session-file-warning';
    warning.textContent = uiText('Large file: preview and copy contain only the displayed portion.', '文件较大，预览和复制只包含已展示部分。');
    target.append(warning);
  }
  target.append(node);
  const tab = currentSessionFileTab();
  target.scrollTop = tab?.scrollTop || 0; target.scrollLeft = tab?.scrollLeft || 0;
}
function toggleSessionFileMode() {
  const tab = currentSessionFileTab();
  if (!tab || tab.file.kind !== 'markdown') return;
  saveSessionFileView();
  tab.raw = !tab.raw; sessionFileState.raw = tab.raw; tab.scrollTop = 0;
  renderSessionFileContent();
}
function toggleSessionFileWrap() {
  const tab = currentSessionFileTab();
  if (!tab) return;
  saveSessionFileView();
  tab.wrap = !tab.wrap; sessionFileState.wrap = tab.wrap;
  renderSessionFileContent();
}
async function copySessionFileContent() {
  if (!sessionFileState.preview) return;
  const status = document.getElementById('sessionFileActionStatus'), generation = sessionFileState.generation;
  try {
    await navigator.clipboard.writeText(sessionFileState.preview.content);
    if (generation === sessionFileState.generation) status.textContent = uiText('Copied', '已复制');
  } catch (_) {
    if (generation === sessionFileState.generation) status.textContent = uiText('Copy failed. Select and copy manually.', '复制失败，请选中文本手动复制。');
  }
}
async function refreshSessionFile() {
  const sid = sessionFileState.sessionId, id = sessionFileState.active?.id, generation = sessionFileState.generation;
  if (!id) return;
  const loaded = await loadSessionFiles(sid);
  if (!loaded || sid !== sessionFileState.sessionId || generation !== sessionFileState.generation) return;
  if (!sessionFileState.files.some(file => file.id === id)) {
    closeSessionFileTab(id);
    document.getElementById('sessionFileActionStatus').textContent = uiText('File is no longer available.', '该文件已不在当前会话文件列表中。');
    return;
  }
  await openSessionFile(id, {refresh: true});
}
function closeSessionFileTab(id) {
  const index = sessionFileState.tabs.findIndex(tab => tab.file.id === id);
  if (index < 0) return;
  const active = sessionFileState.active?.id === id;
  sessionFileState.tabs.splice(index, 1);
  if (active) {
    sessionFileState.generation++; sessionFileState.controller?.abort();
    sessionFileState.active = null; sessionFileState.preview = null;
    sessionFileState.tabs = sessionFileState.tabs.filter(tab => sessionFileState.files.some(file => file.id === tab.file.id));
    const next = sessionFileState.tabs[Math.min(index, sessionFileState.tabs.length - 1)];
    if (next) {
      void openSessionFile(next.file.id);
      document.getElementById('sessionFileTabs').querySelector('[aria-selected="true"]')?.focus();
      return;
    }
    updateSessionFileToolbar(); renderSessionFileEmpty();
  }
  renderSessionFileTabs();
  const tabs = document.getElementById('sessionFileTabs');
  (tabs.querySelector('[aria-selected="true"]') || document.getElementById('sessionFileAdd')).focus();
}
function toggleSessionFileMaximize() {
  sessionFileState.maximized = !sessionFileState.maximized;
  document.getElementById('sessionFilePanel').classList.toggle('is-maximized', sessionFileState.maximized);
  const button = document.getElementById('sessionFileMaximize');
  const label = sessionFileState.maximized ? uiText('Restore split view', '恢复分栏') : uiText('Maximize file preview', '最大化文件预览');
  button.title = label; button.setAttribute('aria-label', label);
  button.setAttribute('aria-pressed', String(sessionFileState.maximized));
  button.replaceChildren(sessionFileIcon(sessionFileState.maximized ? 'minimize-2' : 'maximize-2'));
}
function closeSessionFile(restoreFocus = true) {
  saveSessionFileView();
  sessionFileState.generation++; sessionFileState.controller?.abort(); sessionFileState.controller = null;
  document.getElementById('sessionFilePanel').hidden = true;
  setSessionFilePicker(false);
  if (sessionFileState.maximized) toggleSessionFileMaximize();
  if (restoreFocus && sessionFileState.restoreFocus?.isConnected) sessionFileState.restoreFocus.focus();
  sessionFileState.restoreFocus = null;
}
function sessionFilePanelKeydown(event) {
  if (event.key !== 'Escape') return;
  if (!document.getElementById('sessionFilePicker').hidden) { setSessionFilePicker(false); document.getElementById('sessionFileAdd').focus(); }
  else if (sessionFileState.maximized) toggleSessionFileMaximize();
  else closeSessionFile();
  event.preventDefault(); event.stopPropagation();
}
function setSessionFileWidth(width) {
  const panel = document.getElementById('sessionFilePanel');
  const sidebar = document.querySelector('.sidebar')?.getBoundingClientRect().width || 0;
  const max = Math.max(340, window.innerWidth - sidebar - 360);
  const value = Math.round(Math.max(340, Math.min(width, max)));
  panel.style.width = value + 'px';
  const resize = document.getElementById('sessionFileResize');
  resize.setAttribute('aria-valuenow', String(value)); resize.setAttribute('aria-valuemax', String(Math.round(max)));
}
function startSessionFileResize(event) {
  if (event.button !== 0 || sessionFileState.maximized || window.innerWidth <= 980) return;
  const handle = event.currentTarget;
  event.preventDefault(); handle.setPointerCapture(event.pointerId);
  const move = e => setSessionFileWidth(window.innerWidth - e.clientX);
  const stop = () => { handle.removeEventListener('pointermove', move); handle.removeEventListener('pointerup', stop); handle.removeEventListener('pointercancel', stop); };
  handle.addEventListener('pointermove', move); handle.addEventListener('pointerup', stop); handle.addEventListener('pointercancel', stop);
}
function sessionFileResizeKeydown(event) {
  if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
  event.preventDefault();
  setSessionFileWidth(document.getElementById('sessionFilePanel').getBoundingClientRect().width + (event.key === 'ArrowLeft' ? 32 : -32));
}
