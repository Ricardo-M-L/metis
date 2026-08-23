// METIS Desktop - local Artifact cards, session gallery, and sandboxed preview.
//
// Artifact content is never parsed from tool output and is never assigned to
// innerHTML. The trusted Desktop shell only consumes structured presentation
// metadata and backend-owned manifests. HTML runs in a sandboxed iframe whose
// URL is restricted to this origin or a loopback-only preview server.

const artifactState = {
  sessionId: '',
  artifacts: [],
  loading: false,
  requestSequence: 0,
  active: null,
  activeVersion: 0,
  previewURL: '',
  restoreFocus: null,
  deletePending: false
};

function artifactValue(source, names, fallback) {
  if (!source || typeof source !== 'object') return fallback;
  for (const name of names) {
    if (source[name] !== undefined && source[name] !== null) return source[name];
  }
  return fallback;
}

function artifactNumber(value, fallback = 0) {
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : fallback;
}

function normalizeArtifactVersion(raw) {
  if (typeof raw === 'number' || typeof raw === 'string') {
    const number = artifactNumber(raw, 0);
    return number > 0 ? { number, createdAt: '', sizeBytes: 0 } : null;
  }
  if (!raw || typeof raw !== 'object') return null;
  const number = artifactNumber(artifactValue(raw, ['number', 'version', 'id', 'Number', 'Version'], 0), 0);
  if (!number) return null;
  return {
    number,
    createdAt: String(artifactValue(raw, ['createdAt', 'created_at', 'CreatedAt'], '') || ''),
    sizeBytes: artifactNumber(artifactValue(raw, ['sizeBytes', 'size_bytes', 'size', 'SizeBytes'], 0), 0),
    digest: String(artifactValue(raw, ['sha256', 'digest', 'SHA256'], '') || '')
  };
}

function normalizeArtifact(raw) {
  if (!raw || typeof raw !== 'object') return null;
  const id = String(artifactValue(raw, ['id', 'artifactId', 'artifact_id', 'ID'], '') || '').trim();
  if (!id) return null;
  const suppliedVersions = artifactValue(raw, ['versions', 'Versions'], []);
  const versions = (Array.isArray(suppliedVersions) ? suppliedVersions : [])
    .map(normalizeArtifactVersion)
    .filter(Boolean);
  let currentVersion = artifactNumber(artifactValue(raw,
    ['currentVersion', 'current_version', 'latestVersion', 'latest_version', 'version', 'CurrentVersion'], 0), 0);
  if (!currentVersion && versions.length) currentVersion = Math.max(...versions.map(v => v.number));
  if (!currentVersion) currentVersion = 1;
  if (!versions.some(v => v.number === currentVersion)) {
    versions.push({ number: currentVersion, createdAt: '', sizeBytes: 0, digest: '' });
  }
  versions.sort((a, b) => b.number - a.number);
  return {
    id,
    title: String(artifactValue(raw, ['title', 'name', 'Title', 'Name'], 'Untitled artifact') || 'Untitled artifact'),
    description: String(artifactValue(raw, ['description', 'summary', 'Description', 'Summary'], '') || ''),
    sessionId: String(artifactValue(raw, ['sessionId', 'session_id', 'SessionID'], '') || ''),
    currentVersion,
    versions,
    createdAt: String(artifactValue(raw, ['createdAt', 'created_at', 'CreatedAt'], '') || ''),
    updatedAt: String(artifactValue(raw, ['updatedAt', 'updated_at', 'UpdatedAt'], '') || ''),
    sizeBytes: artifactNumber(artifactValue(raw, ['sizeBytes', 'size_bytes', 'size', 'SizeBytes'], 0), 0),
    mediaType: String(artifactValue(raw, ['mediaType', 'media_type', 'mimeType', 'mime_type'], 'text/html') || 'text/html')
  };
}

function artifactFromPresentation(source) {
  if (!source || typeof source !== 'object') return null;
  let presentation = artifactValue(source, ['presentation', 'Presentation'], null);
  const meta = artifactValue(source, ['meta', 'Meta'], null);
  if (!presentation && meta && typeof meta === 'object') {
    presentation = artifactValue(meta, ['presentation', 'Presentation'], null);
  }
  if (!presentation) presentation = source;
  if (!presentation || typeof presentation !== 'object') return null;
  const kind = String(artifactValue(presentation, ['kind', 'type', 'card', 'Kind'], '') || '').toLowerCase();
  const nested = artifactValue(presentation, ['artifact', 'data', 'Artifact'], null);
  if (kind !== 'artifact' && !nested) return null;
  return normalizeArtifact(nested && typeof nested === 'object'
    ? Object.assign({}, presentation, nested)
    : presentation);
}

function artifactListPayload(data) {
  const raw = Array.isArray(data) ? data : artifactValue(data || {}, ['artifacts', 'items', 'data', 'Artifacts'], []);
  return (Array.isArray(raw) ? raw : []).map(normalizeArtifact).filter(Boolean);
}

function artifactByID(id) {
  return artifactState.artifacts.find(item => item.id === String(id || '')) || null;
}

function upsertArtifact(item) {
  const normalized = normalizeArtifact(item);
  if (!normalized) return null;
  const index = artifactState.artifacts.findIndex(candidate => candidate.id === normalized.id);
  if (index < 0) artifactState.artifacts.unshift(normalized);
  else artifactState.artifacts[index] = Object.assign({}, artifactState.artifacts[index], normalized);
  renderArtifactGallery();
  updateArtifactTabCount();
  return index < 0 ? artifactState.artifacts[0] : artifactState.artifacts[index];
}

function artifactAPIPath(id, action, version) {
  const base = '/api/artifacts/' + encodeURIComponent(String(id || ''));
  const suffix = action ? '/' + action : '';
  const query = artifactNumber(version, 0) > 0 ? '?version=' + encodeURIComponent(String(version)) : '';
  return base + suffix + query;
}

function safeArtifactURL(value) {
  if (!value) return '';
  try {
    const parsed = new URL(String(value), window.location.href);
    if (parsed.username || parsed.password) return '';
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return '';
    const host = parsed.hostname.toLowerCase().replace(/^\[|\]$/g, '');
    const loopback = host === 'localhost' || host === '127.0.0.1' || host === '::1';
    if (parsed.origin !== window.location.origin && !loopback) return '';
    if (parsed.origin === window.location.origin && !parsed.pathname.startsWith('/api/artifacts/')) return '';
    return parsed.href;
  } catch (_) {
    return '';
  }
}

function formatArtifactDate(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(date);
}

function formatArtifactSize(bytes) {
  const size = artifactNumber(bytes, 0);
  if (!size) return '';
  if (size < 1024) return size + ' B';
  if (size < 1024 * 1024) return (size / 1024).toFixed(size < 10240 ? 1 : 0) + ' KB';
  return (size / (1024 * 1024)).toFixed(1) + ' MB';
}

function artifactVersionLabel(item, version) {
  const selected = artifactNumber(version, item.currentVersion);
  const details = item.versions.find(candidate => candidate.number === selected);
  const bits = ['Version ' + selected];
  const date = details && formatArtifactDate(details.createdAt);
  const size = (details && details.sizeBytes) || item.sizeBytes;
  if (date) bits.push(date);
  if (size) bits.push(formatArtifactSize(size));
  return bits.join(' · ');
}

function createArtifactMark(className) {
  const mark = document.createElement('span');
  mark.className = className || 'artifact-mark';
  mark.setAttribute('aria-hidden', 'true');
  const core = document.createElement('span');
  core.textContent = '◇';
  mark.appendChild(core);
  return mark;
}

function artifactButton(label, className, handler) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = className || '';
  button.textContent = label;
  button.addEventListener('click', event => {
    event.stopPropagation();
    handler(event);
  });
  return button;
}

function createArtifactCard(item, context) {
  const card = document.createElement('article');
  card.className = context === 'chat' ? 'artifact-card artifact-chat-card' : 'artifact-card artifact-gallery-card';
  card.dataset.artifactId = item.id;
  card.tabIndex = 0;
  card.setAttribute('role', 'button');
  card.setAttribute('aria-label', 'Open artifact ' + item.title);
  card.addEventListener('click', () => previewArtifactByID(item.id));
  card.addEventListener('keydown', event => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      previewArtifactByID(item.id);
    }
  });

  const visual = document.createElement('div');
  visual.className = 'artifact-card-visual';
  visual.appendChild(createArtifactMark('artifact-mark'));
  const versionPill = document.createElement('span');
  versionPill.className = 'artifact-card-version';
  versionPill.textContent = 'v' + item.currentVersion;
  visual.appendChild(versionPill);
  card.appendChild(visual);

  const body = document.createElement('div');
  body.className = 'artifact-card-body';
  const eyebrow = document.createElement('span');
  eyebrow.className = 'artifact-card-eyebrow';
  eyebrow.textContent = 'METIS ARTIFACT';
  const title = document.createElement('strong');
  title.className = 'artifact-card-title';
  title.textContent = item.title;
  const description = document.createElement('span');
  description.className = 'artifact-card-description';
  description.textContent = item.description || 'Safe local HTML deliverable';
  const meta = document.createElement('span');
  meta.className = 'artifact-card-meta';
  const updated = formatArtifactDate(item.updatedAt || item.createdAt);
  meta.textContent = ['Version ' + item.currentVersion, updated, formatArtifactSize(item.sizeBytes)].filter(Boolean).join(' · ');
  body.append(eyebrow, title, description, meta);
  card.appendChild(body);

  const actions = document.createElement('div');
  actions.className = 'artifact-card-actions';
  actions.appendChild(artifactButton('Preview', 'artifact-primary-action', () => previewArtifactByID(item.id)));
  const more = document.createElement('span');
  more.className = 'artifact-card-arrow';
  more.setAttribute('aria-hidden', 'true');
  more.textContent = '↗';
  actions.appendChild(more);
  card.appendChild(actions);
  return card;
}

function renderArtifactPresentation(toolRow, presentation) {
  const item = artifactFromPresentation(presentation);
  if (!item || !toolRow || !toolRow.parentElement) return false;
  const saved = upsertArtifact(item) || item;
  const area = document.getElementById('chatArea');
  const selector = '.artifact-chat-card[data-artifact-id="' + CSS.escape(saved.id) + '"]';
  const existing = area && area.querySelector(selector);
  const card = createArtifactCard(saved, 'chat');
  if (existing) existing.replaceWith(card);
  else toolRow.insertAdjacentElement('afterend', card);
  updateEmptyLayout();
  autoScroll();
  return true;
}

function syncArtifactChatCards(items) {
  const area = document.getElementById('chatArea');
  if (!area) return;
  (items || []).forEach(item => {
    const selector = '.artifact-chat-card[data-artifact-id="' + CSS.escape(item.id) + '"]';
    if (area.querySelector(selector)) return;
    const card = createArtifactCard(item, 'chat');
    if (turnStatusEl && turnStatusEl.parentElement === area) area.insertBefore(card, turnStatusEl);
    else area.appendChild(card);
  });
  updateEmptyLayout();
}

function renderArtifactGallery() {
  const gallery = document.getElementById('artifactGallery');
  const empty = document.getElementById('artifactsEmpty');
  if (!gallery || !empty) return;
  gallery.replaceChildren();
  for (const item of artifactState.artifacts) gallery.appendChild(createArtifactCard(item, 'gallery'));
  const hasSession = !!artifactState.sessionId;
  empty.hidden = artifactState.loading || artifactState.artifacts.length > 0;
  const strong = empty.querySelector('strong');
  const detail = empty.querySelector('span:last-child');
  if (strong) strong.textContent = hasSession ? 'No artifacts in this session' : 'Open a session to view its artifacts';
  if (detail) detail.textContent = hasSession
    ? 'Ask METIS to build a previewable page, diagram, dashboard, or visual explanation.'
    : 'Artifacts are grouped with the conversation that created them.';
}

function updateArtifactTabCount() {
  const count = document.getElementById('artifactTabCount');
  if (!count) return;
  const total = artifactState.sessionId === String(currentSessionId || '') ? artifactState.artifacts.length : 0;
  count.textContent = String(total);
  count.hidden = total === 0;
}

async function loadArtifactsForSession(sessionId, options = {}) {
  const normalizedSession = String(sessionId || '');
  const sequence = ++artifactState.requestSequence;
  artifactState.sessionId = normalizedSession;
  artifactState.loading = !!normalizedSession;
  const loading = document.getElementById('artifactsLoading');
  if (loading) loading.hidden = !artifactState.loading;
  if (!normalizedSession) {
    artifactState.artifacts = [];
    artifactState.loading = false;
    renderArtifactGallery();
    updateArtifactTabCount();
    return [];
  }
  try {
    const res = await fetch('/api/artifacts?sessionId=' + encodeURIComponent(normalizedSession), {
      headers: { Accept: 'application/json' }
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'artifacts: ' + res.status);
    if (sequence !== artifactState.requestSequence || normalizedSession !== String(currentSessionId || '')) return [];
    artifactState.artifacts = artifactListPayload(data);
    if (options.rebuildCards) syncArtifactChatCards(artifactState.artifacts);
    return artifactState.artifacts;
  } catch (error) {
    if (sequence === artifactState.requestSequence) {
      artifactState.artifacts = [];
      if (!options.silent) showToast('Unable to load artifacts: ' + error.message);
    }
    return [];
  } finally {
    if (sequence === artifactState.requestSequence) {
      artifactState.loading = false;
      if (loading) loading.hidden = true;
      renderArtifactGallery();
      updateArtifactTabCount();
    }
  }
}

async function refreshArtifacts() {
  const button = document.getElementById('artifactRefreshBtn');
  if (button) button.disabled = true;
  await loadArtifactsForSession(currentSessionId, { rebuildCards: true });
  if (button) button.disabled = false;
}

function resetArtifactsForSession() {
  artifactState.requestSequence++;
  artifactState.sessionId = '';
  artifactState.artifacts = [];
  artifactState.loading = false;
  renderArtifactGallery();
  updateArtifactTabCount();
  if (document.querySelector('.main.artifacts-mode')) leaveArtifactsPanel();
}

function openArtifactsPanel() {
  const main = document.querySelector('.main');
  const app = document.querySelector('.app');
  if (!main || !app) return;
  if (typeof currentView !== 'undefined') currentView = 'artifacts';
  main.classList.remove('empty', 'trace-mode');
  main.classList.add('artifacts-mode');
  app.classList.remove('trace-mode');
  app.classList.add('artifacts-mode');
  document.getElementById('tracePanel').classList.remove('visible');
  document.getElementById('tabChat').classList.remove('active');
  document.getElementById('tabTrace').classList.remove('active');
  document.getElementById('tabArtifacts').classList.add('active');
  if (artifactState.sessionId !== String(currentSessionId || '')) {
    loadArtifactsForSession(currentSessionId, { rebuildCards: true });
  } else {
    renderArtifactGallery();
  }
}

function leaveArtifactsPanel() {
  const main = document.querySelector('.main');
  const app = document.querySelector('.app');
  if (main) main.classList.remove('artifacts-mode');
  if (app) app.classList.remove('artifacts-mode');
  const tab = document.getElementById('tabArtifacts');
  if (tab) tab.classList.remove('active');
}

async function fetchArtifactDetail(id) {
  const existing = artifactByID(id);
  try {
    const res = await fetch(artifactAPIPath(id, '', 0), { headers: { Accept: 'application/json' } });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'artifact: ' + res.status);
    const raw = artifactValue(data, ['artifact', 'data', 'Artifact'], data);
    return upsertArtifact(raw) || existing;
  } catch (error) {
    if (existing) return existing;
    throw error;
  }
}

async function previewArtifactByID(id, version) {
  artifactState.restoreFocus = document.activeElement;
  try {
    const item = await fetchArtifactDetail(id);
    if (!item) throw new Error('Artifact not found');
    artifactState.active = item;
    artifactState.activeVersion = artifactNumber(version, item.currentVersion) || item.currentVersion;
    openArtifactPreviewShell(item);
    await loadArtifactPreviewURL(item, artifactState.activeVersion);
  } catch (error) {
    showToast('Unable to open artifact: ' + error.message);
  }
}

function openArtifactPreviewShell(item) {
  const overlay = document.getElementById('artifactPreviewOverlay');
  const title = document.getElementById('artifactPreviewTitle');
  const meta = document.getElementById('artifactPreviewMeta');
  const select = document.getElementById('artifactVersionSelect');
  if (!overlay || !title || !meta || !select) return;
  title.textContent = item.title;
  meta.textContent = artifactVersionLabel(item, artifactState.activeVersion);
  select.replaceChildren();
  item.versions.forEach(version => {
    const option = document.createElement('option');
    option.value = String(version.number);
    option.textContent = 'v' + version.number + (version.number === item.currentVersion ? ' · latest' : '');
    option.selected = version.number === artifactState.activeVersion;
    select.appendChild(option);
  });
  overlay.hidden = false;
  document.body.classList.add('artifact-modal-open');
  requestAnimationFrame(() => document.querySelector('.artifact-preview-close')?.focus());
}

async function resolveArtifactPreviewURL(item, version) {
  const endpoint = artifactAPIPath(item.id, 'preview', version);
  const res = await fetch(endpoint, { headers: { Accept: 'application/json' } });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || 'preview: ' + res.status);
  }
  const contentType = String(res.headers.get('Content-Type') || '').toLowerCase();
  let candidate = res.headers.get('Location') || '';
  if (contentType.includes('json')) {
    const data = await res.json();
    candidate = artifactValue(data, ['previewUrl', 'previewURL', 'url', 'href'], candidate);
  } else {
    // Some local backends serve the sandbox document directly at /preview.
    // Keep the iframe pointed at that endpoint; its response body never
    // enters the trusted document.
    candidate = res.url || endpoint;
  }
  const safe = safeArtifactURL(candidate);
  if (!safe) throw new Error('The preview server returned an unsafe URL');
  return safe;
}

async function loadArtifactPreviewURL(item, version) {
  const frame = document.getElementById('artifactPreviewFrame');
  const state = document.getElementById('artifactPreviewState');
  const meta = document.getElementById('artifactPreviewMeta');
  if (!frame || !state) return;
  frame.removeAttribute('src');
  state.hidden = false;
  state.textContent = 'Preparing preview…';
  try {
    const safe = await resolveArtifactPreviewURL(item, version);
    if (!artifactState.active || artifactState.active.id !== item.id || artifactState.activeVersion !== version) return;
    artifactState.previewURL = safe;
    frame.onload = () => { state.hidden = true; };
    frame.src = safe;
    if (meta) meta.textContent = artifactVersionLabel(item, version);
  } catch (error) {
    artifactState.previewURL = '';
    state.hidden = false;
    state.textContent = 'Preview unavailable: ' + error.message;
  }
}

async function selectArtifactVersion(value) {
  if (!artifactState.active) return;
  const version = artifactNumber(value, artifactState.active.currentVersion);
  artifactState.activeVersion = version;
  await loadArtifactPreviewURL(artifactState.active, version);
}

function closeArtifactPreview() {
  const overlay = document.getElementById('artifactPreviewOverlay');
  const frame = document.getElementById('artifactPreviewFrame');
  if (overlay) overlay.hidden = true;
  if (frame) {
    frame.onload = null;
    frame.removeAttribute('src');
  }
  document.body.classList.remove('artifact-modal-open');
  artifactState.previewURL = '';
  artifactState.active = null;
  artifactState.activeVersion = 0;
  const restore = artifactState.restoreFocus;
  artifactState.restoreFocus = null;
  if (restore && restore.isConnected && typeof restore.focus === 'function') restore.focus();
}

function triggerArtifactDownload(path, filename) {
  const safe = safeArtifactURL(path);
  if (!safe) {
    showToast('The artifact download URL was rejected');
    return;
  }
  const link = document.createElement('a');
  link.href = safe;
  link.rel = 'noopener noreferrer';
  if (filename) link.download = filename;
  document.body.appendChild(link);
  link.click();
  link.remove();
}

function downloadActiveArtifact() {
  if (!artifactState.active) return;
  triggerArtifactDownload(
    artifactAPIPath(artifactState.active.id, 'download', artifactState.activeVersion),
    artifactState.active.title.replace(/[^a-z0-9._-]+/gi, '-') + '-v' + artifactState.activeVersion + '.html'
  );
}

function exportActiveArtifact() {
  if (!artifactState.active) return;
  triggerArtifactDownload(
    artifactAPIPath(artifactState.active.id, 'export', artifactState.activeVersion),
    artifactState.active.title.replace(/[^a-z0-9._-]+/gi, '-') + '.zip'
  );
}

async function openActiveArtifactExternally() {
  if (!artifactState.active) return;
  try {
    const safe = artifactState.previewURL || await resolveArtifactPreviewURL(artifactState.active, artifactState.activeVersion);
    if (!safeArtifactURL(safe)) throw new Error('unsafe preview URL');
    const opened = window.open(safe, '_blank', 'noopener,noreferrer');
    if (!opened) showToast('Allow pop-ups to open this artifact externally');
  } catch (error) {
    showToast('Unable to open artifact externally: ' + error.message);
  }
}

function requestArtifactDeletion() {
  if (!artifactState.active || artifactState.deletePending) return;
  const overlay = document.getElementById('artifactDeleteOverlay');
  const title = document.getElementById('artifactDeleteTitle');
  const description = document.getElementById('artifactDeleteDescription');
  if (title) title.textContent = 'Delete “' + artifactState.active.title + '”?';
  if (description) description.textContent = 'This removes all ' + artifactState.active.versions.length + ' saved version' +
    (artifactState.active.versions.length === 1 ? '' : 's') + '. This action cannot be undone.';
  if (overlay) overlay.hidden = false;
  requestAnimationFrame(() => document.getElementById('artifactDeleteCancel')?.focus());
}

function cancelArtifactDeletion() {
  const overlay = document.getElementById('artifactDeleteOverlay');
  if (overlay) overlay.hidden = true;
  document.querySelector('.artifact-preview-close')?.focus();
}

async function confirmArtifactDeletion() {
  if (!artifactState.active || artifactState.deletePending) return;
  const id = artifactState.active.id;
  artifactState.deletePending = true;
  const confirmButton = document.getElementById('artifactDeleteConfirm');
  if (confirmButton) {
    confirmButton.disabled = true;
    confirmButton.textContent = 'Deleting…';
  }
  try {
    const res = await fetch(artifactAPIPath(id, '', 0), { method: 'DELETE', headers: { Accept: 'application/json' } });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'delete: ' + res.status);
    document.querySelectorAll('.artifact-card[data-artifact-id="' + CSS.escape(id) + '"]').forEach(card => card.remove());
    artifactState.artifacts = artifactState.artifacts.filter(item => item.id !== id);
    cancelArtifactDeletion();
    closeArtifactPreview();
    renderArtifactGallery();
    updateArtifactTabCount();
    showToast('Artifact and all saved versions deleted');
  } catch (error) {
    showToast('Unable to delete artifact: ' + error.message);
  } finally {
    artifactState.deletePending = false;
    if (confirmButton) {
      confirmButton.disabled = false;
      confirmButton.textContent = 'Delete all versions';
    }
  }
}

document.addEventListener('DOMContentLoaded', () => {
  // Trace owns the standard chat/trajectory switcher. Wrap it once so any
  // existing command or tab that returns to those views also tears down the
  // Artifact panel state.
  if (typeof window.switchView === 'function' && !window.switchView.artifactAware) {
    const baseSwitchView = window.switchView;
    const wrapped = function(view) {
      if (view !== 'artifacts') leaveArtifactsPanel();
      return baseSwitchView(view);
    };
    wrapped.artifactAware = true;
    window.switchView = wrapped;
  }
  document.getElementById('artifactPreviewOverlay')?.addEventListener('click', event => {
    if (event.target.id === 'artifactPreviewOverlay') closeArtifactPreview();
  });
  document.getElementById('artifactDeleteOverlay')?.addEventListener('click', event => {
    if (event.target.id === 'artifactDeleteOverlay') cancelArtifactDeletion();
  });
  document.addEventListener('keydown', event => {
    if (event.key !== 'Escape') return;
    if (!document.getElementById('artifactDeleteOverlay')?.hidden) cancelArtifactDeletion();
    else if (!document.getElementById('artifactPreviewOverlay')?.hidden) closeArtifactPreview();
  });
  renderArtifactGallery();
  updateArtifactTabCount();
});
