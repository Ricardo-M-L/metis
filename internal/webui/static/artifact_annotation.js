// Point at a saved HTML Artifact and hand a canonical revision to normal chat.
// The picker is backend-owned and runs on a separate capability origin in an
// opaque sandbox. Messages provide target IDs only; displayed text and the
// revision prompt always come from the authenticated backend.
const artifactAnnotationState = {
  mode: 'off', generation: 0, scope: null, channel: '', targets: new Map(),
  selected: null, controller: null, frameWindow: null, hint: null, comparisons: new Map()
};

function artifactAnnotationText(en, zh) {
  return typeof uiText === 'function' ? uiText(en, zh)
    : (document.documentElement.lang === 'zh-CN' ? zh : en);
}

function artifactAnnotationScope() {
  const item = artifactState.active;
  const sessionId = String(currentSessionId || '');
  if (!item || !sessionId || (item.sessionId && item.sessionId !== sessionId)) return null;
  const version = Number(artifactState.activeVersion);
  if (!Number.isInteger(version) || version <= 0) return null;
  return { artifactId: item.id, sessionId, version, sessionGeneration: artifactState.sessionGeneration,
    previewSequence: artifactState.previewSequence };
}

function artifactAnnotationScopeIsCurrent(scope, generation) {
  const current = artifactAnnotationScope();
  return !!scope && !!current && generation === artifactAnnotationState.generation &&
    current.artifactId === scope.artifactId && current.sessionId === scope.sessionId &&
    current.version === scope.version && current.sessionGeneration === scope.sessionGeneration &&
    current.previewSequence === scope.previewSequence;
}

function artifactAnnotationComparisonKey(sessionId, artifactId) {
  return JSON.stringify([sessionId, artifactId]);
}

function artifactRevisionComparison() {
  const scope = artifactAnnotationScope();
  const item = artifactState.active;
  if (!scope || !item || !Number.isInteger(item.currentVersion)) return null;
  const versions = item.versions.map(version => version.number)
    .filter(version => Number.isInteger(version) && version > 0);
  if (!versions.includes(item.currentVersion)) return null;
  const recorded = artifactAnnotationState.comparisons.get(
    artifactAnnotationComparisonKey(scope.sessionId, scope.artifactId));
  let beforeVersion = recorded?.beforeVersion;
  if (!Number.isInteger(beforeVersion) || beforeVersion >= item.currentVersion || !versions.includes(beforeVersion)) {
    // Persisted immutable versions remain comparable after Desktop restarts.
    // Only this active Artifact's manifest supplies the fallback pair.
    beforeVersion = Math.max(0, ...versions.filter(version => version < item.currentVersion));
  }
  return beforeVersion > 0 ? {beforeVersion, afterVersion:item.currentVersion} : null;
}

function refreshArtifactRevisionComparison() {
  const controls = document.getElementById('artifactCompareControls');
  const before = document.getElementById('artifactCompareBefore');
  const after = document.getElementById('artifactCompareAfter');
  if (!controls || !before || !after) return;
  controls.setAttribute('aria-label', artifactAnnotationText('Before and after', '修改前后对照'));
  const scope = artifactAnnotationScope();
  const comparison = artifactRevisionComparison();
  controls.hidden = !comparison;
  if (!comparison) return;
  before.textContent = artifactAnnotationText('Before · v', '修改前 · v') + comparison.beforeVersion;
  after.textContent = artifactAnnotationText('After · v', '修改后 · v') + comparison.afterVersion;
  before.setAttribute('aria-pressed', String(scope.version === comparison.beforeVersion));
  after.setAttribute('aria-pressed', String(scope.version === comparison.afterVersion));
}

async function compareArtifactRevision(side) {
  if (side !== 'before' && side !== 'after') return;
  const comparison = artifactRevisionComparison();
  if (!comparison) return;
  const version = side === 'before' ? comparison.beforeVersion : comparison.afterVersion;
  await selectArtifactVersion(version);
  refreshArtifactRevisionComparison();
}

function refreshArtifactAnnotationAvailability() {
  const toggle = document.getElementById('artifactAnnotationToggle');
  if (!toggle) return;
  const item = artifactState.active;
  const scope = artifactAnnotationScope();
  const active = artifactAnnotationState.mode !== 'off';
  const html = item && /^text\/html(?:;|$)/i.test(item.mediaType || 'text/html');
  const latest = scope && scope.version === item.currentVersion;
  toggle.disabled = active ? artifactAnnotationState.mode === 'submitting'
    : !scope || !html || !latest || !artifactState.previewURL;
  toggle.setAttribute('aria-pressed', String(active));
  const label = document.getElementById('artifactAnnotationToggleLabel') || toggle.querySelector?.('span') || toggle;
  label.textContent = active ? artifactAnnotationText('Exit selection', '退出点选')
    : artifactAnnotationText('Point to edit', '指点修改');
  toggle.title = !latest && scope
    ? artifactAnnotationText('Select the latest version to request a change', '切换到最新版本后可指点修改')
    : artifactAnnotationText('Select an element and describe your change', '点选页面元素，告诉 METIS 怎么改');
  refreshArtifactRevisionComparison();
}

function renderArtifactAnnotation() {
  const state = artifactAnnotationState;
  [
    ['artifactVersionLabel', 'Version', '版本'],
    ['artifactDownloadBtn', 'Download', '下载'],
    ['artifactExportBtn', 'Export', '导出'],
    ['artifactOpenExternalBtn', 'Open externally', '在浏览器打开'],
    ['artifactDeleteBtn', 'Delete', '删除']
  ].forEach(([id, en, zh]) => {
    const node = document.getElementById(id);
    if (node) {
      node.textContent = artifactAnnotationText(en, zh);
      if (id !== 'artifactVersionLabel') {
        node.title = node.textContent;
        node.setAttribute('aria-label', node.textContent);
      }
    }
  });
  const close = document.getElementById('artifactCloseBtn');
  if (close) {
    close.title = artifactAnnotationText('Close preview', '关闭预览');
    close.setAttribute('aria-label', close.title);
  }
  const panel = document.getElementById('artifactAnnotationPanel');
  const selection = document.getElementById('artifactAnnotationSelection');
  const version = document.getElementById('artifactAnnotationVersion');
  const instruction = document.getElementById('artifactAnnotationInstruction');
  const apply = document.getElementById('artifactAnnotationApply');
  if (panel) {
    panel.hidden = state.mode === 'off';
    panel.setAttribute('aria-label', artifactAnnotationText('Point to edit', '指点修改'));
  }
  const title = document.getElementById('artifactAnnotationTitle');
  const instructionLabel = document.getElementById('artifactAnnotationInstructionLabel');
  const note = document.getElementById('artifactAnnotationNote');
  const cancel = document.getElementById('artifactAnnotationCancel');
  if (title) title.textContent = artifactAnnotationText('Point to edit', '指点修改');
  if (instructionLabel) instructionLabel.textContent = artifactAnnotationText('Change request', '修改要求');
  if (note) note.textContent = artifactAnnotationText('Changes are saved as a new version. The original is kept.', '修改会保存为新版本，原版会保留。');
  if (cancel) cancel.textContent = artifactAnnotationText('Exit selection', '退出点选');
  document.getElementById('artifactPreviewOverlay')?.classList.toggle('artifact-annotation-active', state.mode !== 'off');
  if (version) version.textContent = state.scope ? 'v' + state.scope.version : '';
  if (selection) {
    selection.setAttribute('aria-label', artifactAnnotationText('Selected element', '选中的元素'));
    selection.textContent = state.selected
      ? artifactAnnotationText('Selected: ', '选中：') + (state.selected.text || state.selected.selector)
      : artifactAnnotationText('Nothing selected yet', '尚未选中元素');
  }
  if (instruction) {
    instruction.disabled = state.mode === 'loading' || state.mode === 'submitting';
    instruction.placeholder = artifactAnnotationText('For example: make this button smaller and label it “Get started”.', '例如：缩小这个按钮，把文案改成“开始体验”');
  }
  if (apply) {
    apply.disabled = state.mode !== 'selecting' || !state.selected || !String(instruction?.value || '').trim();
    apply.textContent = state.mode === 'submitting'
      ? artifactAnnotationText('Preparing change…', '正在提交…')
      : artifactAnnotationText('Ask METIS to edit', '交给 METIS 修改');
  }
  refreshArtifactAnnotationAvailability();
}

function setArtifactAnnotationHint(en, zh) {
  artifactAnnotationState.hint = {en, zh};
  const hint = document.getElementById('artifactAnnotationHint');
  if (hint) hint.textContent = artifactAnnotationText(en, zh);
}

function refreshArtifactAnnotationLanguage() {
  renderArtifactAnnotation();
  if (typeof refreshArtifactPreviewLanguage === 'function') refreshArtifactPreviewLanguage();
  const hint = artifactAnnotationState.hint;
  if (hint) setArtifactAnnotationHint(hint.en, hint.zh);
}

function resetArtifactAnnotation() {
  const state = artifactAnnotationState;
  state.generation++;
  state.controller?.abort();
  state.controller = null;
  state.mode = 'off'; state.scope = null; state.channel = '';
  state.targets = new Map(); state.selected = null; state.frameWindow = null; state.hint = null;
  const input = document.getElementById('artifactAnnotationInstruction');
  if (input) input.value = '';
  const frame = document.getElementById('artifactPreviewFrame');
  if (frame) { frame.onload = null; frame.setAttribute('sandbox', ''); }
  renderArtifactAnnotation();
}

async function endArtifactAnnotation() {
  const scope = artifactAnnotationState.scope;
  const item = artifactState.active;
  const shouldRestore = !!scope && artifactAnnotationScopeIsCurrent(scope, artifactAnnotationState.generation);
  resetArtifactAnnotation();
  if (shouldRestore && item) await loadArtifactPreviewURL(item, scope.version);
  refreshArtifactAnnotationAvailability();
}

function annotationPreviewManifest(data, scope) {
  if (!data || typeof data !== 'object' || Array.isArray(data) || data.version !== scope.version ||
      typeof data.digest !== 'string' || !/^[a-f0-9]{64}$/.test(data.digest) ||
      typeof data.channel !== 'string' || !/^[a-zA-Z0-9_-]{16,256}$/.test(data.channel) ||
      !Array.isArray(data.targets) || data.targets.length > 10000) throw new Error('Invalid selection preview');
  const safe = safeArtifactURL(data.url);
  if (!safe || new URL(safe).origin === window.location.origin) throw new Error('Unsafe selection preview origin');
  const targets = new Map();
  for (const target of data.targets) {
    if (!target || typeof target !== 'object' || typeof target.id !== 'string' ||
        !target.id || target.id.length > 256 || targets.has(target.id) ||
        typeof target.tag !== 'string' || !/^[a-z][a-z0-9-]{0,63}$/.test(target.tag) ||
        typeof target.selector !== 'string' || target.selector.length > 2048 ||
        typeof target.text !== 'string' || target.text.length > 1200) throw new Error('Invalid selection target');
    targets.set(target.id, { id: target.id, tag: target.tag, selector: target.selector, text: target.text });
  }
  return {url:safe,channel:data.channel,digest:data.digest,targets};
}

async function beginArtifactAnnotation() {
  const scope = artifactAnnotationScope();
  const item = artifactState.active;
  if (!scope || scope.version !== item.currentVersion || !artifactState.previewURL ||
      !/^text\/html(?:;|$)/i.test(item.mediaType || 'text/html')) return false;
  resetArtifactAnnotation();
  const state = artifactAnnotationState;
  const generation = state.generation;
  state.scope = scope; state.mode = 'loading'; state.controller = new AbortController();
  setArtifactAnnotationHint('Preparing selectable preview…', '正在准备点选预览…');
  renderArtifactAnnotation();
  try {
    const res = await fetch(artifactAPIPath(scope.artifactId, 'annotation-preview', scope.version, scope.sessionId),
      {headers:{Accept:'application/json'},signal:state.controller.signal});
    const data = await res.json().catch(() => ({}));
    if (!artifactAnnotationScopeIsCurrent(scope, generation)) return false;
    if (!res.ok) throw new Error(data.error || 'Selection preview: ' + res.status);
    const manifest = annotationPreviewManifest(data, scope);
    const frame = document.getElementById('artifactPreviewFrame');
    if (!frame) throw new Error('Preview is unavailable');
    state.channel = manifest.channel; state.targets = manifest.targets; state.scope.digest = manifest.digest;
    state.frameWindow = frame.contentWindow;
    if (!state.frameWindow) throw new Error('Preview is unavailable');
    frame.setAttribute('sandbox', 'allow-scripts');
    frame.onload = () => {
      if (!artifactAnnotationScopeIsCurrent(scope, generation)) return;
      state.mode = 'selecting';
      const previewState = document.getElementById('artifactPreviewState');
      if (previewState) previewState.hidden = true;
      setArtifactAnnotationHint('Click the part you want to change. Press Escape to exit.', '点击页面中想修改的位置，按 Esc 退出点选。');
      renderArtifactAnnotation();
    };
    frame.src = manifest.url;
    return true;
  } catch (error) {
    if (!artifactAnnotationScopeIsCurrent(scope, generation)) return false;
    const message = error.message || String(error);
    await endArtifactAnnotation();
    showToast(artifactAnnotationText('Unable to select this Artifact: ', '无法点选这个产物：') + message);
    return false;
  }
}

function toggleArtifactAnnotation() {
  if (artifactAnnotationState.mode === 'off') return beginArtifactAnnotation();
  return endArtifactAnnotation();
}

function artifactAnnotationMessage(event) {
  const state = artifactAnnotationState;
  const frame = document.getElementById('artifactPreviewFrame');
  if (state.mode === 'off' || event.origin !== 'null' ||
      !frame || !state.frameWindow || event.source !== frame.contentWindow || event.source !== state.frameWindow ||
      !artifactAnnotationScopeIsCurrent(state.scope, state.generation)) return;
  const data = event.data;
  if (!data || typeof data !== 'object' || Array.isArray(data) || data.channel !== state.channel || !state.channel) return;
  const allowed = data.type === 'metis-artifact-selection' ? ['type','channel','targetId','rect'] : ['type','channel'];
  if (Object.keys(data).some(key => !allowed.includes(key))) return;
  if (data.type === 'metis-artifact-annotation-exit') { endArtifactAnnotation(); return; }
  if (data.type === 'metis-artifact-annotation-ready') return;
  if (data.type !== 'metis-artifact-selection' || state.mode !== 'selecting' || typeof data.targetId !== 'string') return;
  if (data.rect !== undefined) {
    const rect = data.rect;
    if (!rect || typeof rect !== 'object' || Array.isArray(rect) ||
        Object.keys(rect).some(key => !['x','y','width','height'].includes(key)) ||
        !['x','y','width','height'].every(key => typeof rect[key] === 'number' && Number.isFinite(rect[key])) ||
        rect.width < 0 || rect.height < 0) return;
  }
  const target = state.targets.get(data.targetId);
  if (!target) return;
  state.selected = target;
  setArtifactAnnotationHint('Describe the change, or click another element.', '说说你想怎么改，也可以重新点选其他位置。');
  renderArtifactAnnotation();
  document.getElementById('artifactAnnotationInstruction')?.focus();
}

async function submitArtifactAnnotation() {
  const state = artifactAnnotationState;
  const scope = state.scope;
  const generation = state.generation;
  const instruction = String(document.getElementById('artifactAnnotationInstruction')?.value || '').trim();
  if (state.mode !== 'selecting' || !state.selected || !scope ||
      !artifactAnnotationScopeIsCurrent(scope, generation) || !instruction) return false;
  if (Array.from(instruction).length > 4000) {
    setArtifactAnnotationHint('Keep the change request within 4,000 characters.', '修改要求请控制在 4000 字以内。');
    return false;
  }
  const target = state.selected;
  let comparisonAttempt = null;
  const rollbackComparison = () => {
    if (!comparisonAttempt) return;
    const { key, previous, baseline } = comparisonAttempt;
    if (state.comparisons.get(key) !== baseline) return;
    if (previous) state.comparisons.set(key, previous); else state.comparisons.delete(key);
  };
  state.mode = 'submitting'; state.controller = new AbortController();
  setArtifactAnnotationHint('Checking this version and selected element…', '正在核对产物版本和选中位置…');
  renderArtifactAnnotation();
  try {
    const res = await fetch(artifactAPIPath(scope.artifactId, 'annotate', 0, scope.sessionId), {
      method:'POST',headers:{Accept:'application/json','Content-Type':'application/json'},signal:state.controller.signal,
      body:JSON.stringify({version:scope.version,digest:scope.digest,targetId:target.id,instruction})
    });
    const data = await res.json().catch(() => ({}));
    if (!artifactAnnotationScopeIsCurrent(scope, generation)) return false;
    if (!res.ok) {
      if (res.status === 409) throw new Error(artifactAnnotationText(
        'This Artifact has changed. Open its latest version and select again.', '产物版本已变化，请打开最新版本后重新点选。'));
      throw new Error(data.error || 'Change request: ' + res.status);
    }
    const reference = data.reference;
    if (typeof data.prompt !== 'string' || !data.prompt.trim() || !reference ||
        reference.artifactId !== scope.artifactId || reference.version !== scope.version ||
        reference.digest !== scope.digest || reference.targetId !== target.id) throw new Error('Invalid change reference');
    if (typeof submitArtifactAnnotationPrompt !== 'function') throw new Error('Chat is unavailable');
    const key = artifactAnnotationComparisonKey(scope.sessionId, scope.artifactId);
    const previous = state.comparisons.get(key);
    const baseline = {beforeVersion:scope.version};
    state.comparisons.set(key, baseline);
    comparisonAttempt = {key, previous, baseline};
    const accepted = await submitArtifactAnnotationPrompt(data.prompt, reference, scope.sessionId);
    if (!accepted) {
      rollbackComparison();
      if (!artifactAnnotationScopeIsCurrent(scope, generation)) return false;
      state.mode = 'selecting';
      setArtifactAnnotationHint('Finish the current task before submitting this change. Your request is kept here.', '请完成当前任务后再提交，修改要求已为你保留。');
      renderArtifactAnnotation();
      return false;
    }
    if (artifactAnnotationScopeIsCurrent(scope, generation)) { resetArtifactAnnotation(); closeArtifactPreview(); }
    return true;
  } catch (error) {
    rollbackComparison();
    if (!artifactAnnotationScopeIsCurrent(scope, generation)) return false;
    state.mode = 'selecting';
    state.hint = null;
    const hint = document.getElementById('artifactAnnotationHint');
    if (hint) hint.textContent = error.message || String(error);
    renderArtifactAnnotation();
    return false;
  }
}

function artifactAnnotationHandleEscape(event) {
  if (event.key !== 'Escape' || artifactAnnotationState.mode === 'off') return false;
  event.preventDefault(); event.stopPropagation();
  endArtifactAnnotation();
  return true;
}

function initializeArtifactAnnotation() {
  document.getElementById('artifactAnnotationInstruction')?.addEventListener('input', renderArtifactAnnotation);
  refreshArtifactAnnotationLanguage();
}

window.addEventListener('message', artifactAnnotationMessage);
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', initializeArtifactAnnotation);
else initializeArtifactAnnotation();
