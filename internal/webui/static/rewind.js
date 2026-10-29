// Rewind is the user-facing restore operation. Automatic code checkpoints are
// its backing data; their compatibility controls do not need a composer chip.
let rewindDialog = null;

function rewindReadOnlyView() {
  return typeof subAgentConversationState !== 'undefined' && !!subAgentConversationState;
}

function rewindViewTarget() {
  return { sessionId: String(currentSessionId || ''),
    workspaceId: typeof activeWorkspaceId === 'string' ? activeWorkspaceId : '',
    generation: typeof resumeSessionGeneration === 'number' ? resumeSessionGeneration : 0 };
}

function rewindViewMatches(state) {
  const target = rewindViewTarget();
  return !!state && state === rewindDialog && !rewindReadOnlyView() &&
    state.sessionId === target.sessionId && state.workspaceId === target.workspaceId && state.generation === target.generation;
}

function syncRewindView() {
  if (rewindDialog && !rewindViewMatches(rewindDialog)) closeRewindDialog(true, false);
}

function closeRewindDialog(force = false, restoreFocus = true) {
  const state = rewindDialog;
  if (!state || state.pending && !force) return;
  const current = rewindViewMatches(state);
  rewindDialog = null;
  state.abort?.abort();
  state.overlay.remove();
  if (restoreFocus && current && state.trigger?.isConnected) state.trigger.focus();
}

function rewindFriendlyError(error) {
  const message = String(error?.message || '无法回退，请重试。');
  if (/busy|a turn is running|stop.*running|stop.*turn/i.test(message)) return '当前会话或工作区仍有任务运行，请先停止任务再回退。';
  if (/stale|revision|changed.*history|history.*changed/i.test(message)) return '会话已经变化，请关闭后重新选择回退点。';
  if (/parent.*managed|managed.*parent|parent_owned/i.test(message)) return '此子代理由主代理管理，请返回主对话操作。';
  return message.length > 400 ? message.slice(0, 399) + '…' : message;
}

function rewindSelectedPoint(state) {
  return state.points.find(point => point.turn === state.turn) || null;
}

function renderRewindDialog(state) {
  if (!rewindViewMatches(state)) { syncRewindView(); return; }
  const priorScroll = state.overlay.querySelector('.rewind-points')?.scrollTop || 0;
  const point = rewindSelectedPoint(state);
  const canRestoreCode = point?.codeAvailable === true;
  if (!canRestoreCode && state.mode !== 'conversation') state.mode = 'conversation';
  let body;
  if (state.completed) {
    body = '<p class="rewind-note">当前草稿已保留。选择是否将回退消息放入输入框。</p>' +
      '<textarea class="rewind-recovered-prompt" readonly aria-label="回退消息">' + escHtml(state.completed.prefill) + '</textarea>' +
      '<div class="rewind-actions"><button type="button" onclick="closeRewindDialog()">保留当前草稿</button>' +
      '<button type="button" class="rewind-confirm" onclick="useRewindPrompt()">使用回退消息</button></div>';
  } else if (state.loading) {
    body = '<p class="rewind-empty" role="status">正在读取恢复点…</p>';
  } else if (!state.points.length) {
    body = state.error ? '' : '<p class="rewind-empty">此会话还没有可回退的消息。</p>';
  } else {
    const entries = state.points.map(item => '<button type="button" class="rewind-point' + (item.turn === state.turn ? ' selected' : '') +
      '" aria-pressed="' + (item.turn === state.turn) + '" data-rewind-turn="' + item.turn + '" onclick="selectRewindTurn(' + item.turn + ')"' + (state.pending || state.requiresReopen ? ' disabled' : '') + '>' +
      '<span class="rewind-point-top"><strong>第 ' + item.turn + ' 轮</strong><small>' + (item.codeAvailable === true ? '代码恢复点' : '仅对话') + '</small></span>' +
      '<span class="rewind-point-prompt">' + escHtml(String(item.prompt || '（无文字消息）').slice(0, 240)) + '</span></button>').join('');
    const modes = [ ['both', '恢复代码和对话', '代码和对话回到此消息之前'],
      ['conversation', '仅恢复对话', '保留当前代码，将消息返回输入框'], ['code', '仅恢复代码', '保留当前对话和输入框草稿'] ];
    body = '<div class="rewind-points" role="group" aria-label="选择回退时间点">' + entries + '</div>' +
      '<fieldset class="rewind-modes"' + (state.pending || state.requiresReopen ? ' disabled' : '') + '><legend>恢复内容</legend>' + modes.map(([mode, label, hint]) => {
        const disabled = mode !== 'conversation' && !canRestoreCode;
        return '<label class="rewind-mode' + (disabled ? ' unavailable' : '') + '"><input type="radio" name="rewindMode" value="' + mode + '" onchange="selectRewindMode(this.value)"' +
          (state.mode === mode ? ' checked' : '') + (disabled ? ' disabled' : '') + '><span><strong>' + label + '</strong><small>' + hint + '</small></span></label>';
      }).join('') + '</fieldset>' +
      (!canRestoreCode ? '<p class="rewind-note" role="status">代码恢复不可用：' + escHtml(point?.codeUnavailableReason || '此时间点没有完整的代码恢复点。') + '</p>' : '') +
      (state.checkpoint?.enabled === false ? '<p class="rewind-note">此会话已关闭自动保存；已有恢复点仍可使用。</p>' : '') +
      '<div class="rewind-actions"><button type="button" onclick="closeRewindDialog()"' + (state.pending ? ' disabled' : '') + '>取消</button>' +
      '<button type="submit" class="rewind-confirm"' + (state.pending || state.requiresReopen || !point ? ' disabled' : '') + '>' + (state.pending ? '正在回退…' : '确认回退') + '</button></div>';
  }
  state.overlay.innerHTML = '<form class="rewind-dialog" role="dialog" aria-modal="true" aria-labelledby="rewindTitle" aria-describedby="rewindDescription" aria-busy="' + state.pending + '">' +
    '<header class="rewind-heading"><div><h2 id="rewindTitle">' + (state.completed ? '回退已完成' : '回退') + '</h2><p id="rewindDescription">' + (state.completed ? state.mode === 'conversation' ? '对话已恢复，代码保持当前状态。' : '代码和对话已恢复。' : '选择一条消息，回到发送前的状态。') + '</p></div>' +
    '<button type="button" class="rewind-close" aria-label="关闭回退窗口" onclick="closeRewindDialog()"' + (state.pending ? ' disabled' : '') + '>×</button></header>' +
    body + '<p class="rewind-error" role="alert"' + (state.error ? '' : ' hidden') + '>' + escHtml(state.error) + '</p></form>';
  state.overlay.querySelector('.rewind-points')?.scrollTo?.({ top: priorScroll });
  const form = state.overlay.querySelector('form');
  if (form) form.addEventListener('submit', confirmRewind);
}

function selectRewindTurn(turn) {
  const state = rewindDialog;
  if (!rewindViewMatches(state) || state.pending || state.requiresReopen || !state.points.some(point => point.turn === turn)) return;
  state.turn = turn;
  state.error = '';
  renderRewindDialog(state);
  state.overlay.querySelector('[data-rewind-turn="' + turn + '"]')?.focus();
}

function selectRewindMode(mode) {
  const state = rewindDialog;
  if (!rewindViewMatches(state) || state.pending || state.requiresReopen || !['conversation', 'code', 'both'].includes(mode)) return;
  if (mode !== 'conversation' && rewindSelectedPoint(state)?.codeAvailable !== true) return;
  state.mode = mode;
}

async function openRewindDialog(input = '', trigger = document.activeElement) {
  if (!['', 'last'].includes(String(input).trim().toLowerCase())) { showToast('用法：/rewind，选择时间点和恢复内容。'); return; }
  if (rewindReadOnlyView()) { showToast('此子代理由主代理管理，请返回主对话操作。'); return; }
  const target = rewindViewTarget();
  if (!target.sessionId) { showToast('请先打开一个已有会话。'); return; }
  if (typeof isViewedTurnRunning === 'function' && isViewedTurnRunning()) { showToast('请先停止当前任务，再选择回退点。'); return; }
  if (typeof pendingSessionId !== 'undefined' && pendingSessionId) { showToast('会话正在切换，请稍后再回退。'); return; }
  if (rewindDialog) return;
  const overlay = document.createElement('div');
  overlay.className = 'rewind-overlay';
  const state = { ...target, overlay, trigger, points: [], turn: 0, mode: 'both', revision: '', checkpoint: null,
    loading: true, pending: false, error: '', abort: typeof AbortController === 'function' ? new AbortController() : null };
  rewindDialog = state;
  overlay.addEventListener('click', event => { if (event.target === overlay) closeRewindDialog(); });
  overlay.addEventListener('keydown', rewindDialogKeydown);
  document.body.appendChild(overlay);
  renderRewindDialog(state);
  overlay.querySelector('.rewind-close')?.focus();
  try {
    const res = await fetch('/api/rewind?sessionId=' + encodeURIComponent(target.sessionId), { cache: 'no-store', signal: state.abort?.signal });
    const data = await res.json().catch(() => ({}));
    if (!rewindViewMatches(state)) { syncRewindView(); return; }
    if (!res.ok) throw new Error(data.error || '无法读取恢复点。');
    if (data.sessionId !== state.sessionId || !Array.isArray(data.points) || !data.revision) throw new Error('恢复点与当前会话不一致，请重新打开。');
    state.points = data.points.filter(point => Number.isSafeInteger(point.turn) && point.turn > 0);
    state.revision = String(data.revision);
    state.checkpoint = data.checkpoint;
    const latest = String(input).trim().toLowerCase() === 'last' ? state.points.find(point => point.latestEdit && point.codeAvailable === true) : null;
    state.turn = (latest || state.points[0])?.turn || 0;
    state.mode = rewindSelectedPoint(state)?.codeAvailable === true ? 'both' : 'conversation';
  } catch (error) {
    if (!rewindViewMatches(state) || error?.name === 'AbortError') return;
    state.error = rewindFriendlyError(error);
  } finally {
    if (rewindViewMatches(state)) {
      state.loading = false;
      renderRewindDialog(state);
      state.overlay.querySelector('[data-rewind-turn="' + state.turn + '"]')?.focus();
    }
  }
}

function useRewindPrompt() {
  const state = rewindDialog;
  if (!rewindViewMatches(state) || !state.completed) return;
  const input = document.getElementById('inputField');
  if (input) { input.value = state.completed.prefill; autoResize(input); }
  if (typeof saveComposerDraftForView === 'function') saveComposerDraftForView();
  closeRewindDialog();
  input?.focus();
}

function invalidateRewindContinuation(sessionId) {
  if (typeof backgroundContinuationGeneration === 'number') backgroundContinuationGeneration++;
  if (typeof backgroundContinuationGenerations !== 'undefined') backgroundContinuationGenerations.set(sessionId, (backgroundContinuationGenerations.get(sessionId) || 0) + 1);
  if (typeof resumeStatesBySession !== 'undefined') resumeStatesBySession.delete(sessionId);
  if (typeof pausedQueueSessions !== 'undefined') pausedQueueSessions.add(sessionId);
  // A pre-rewind status response must not restore the old resume token.
  if (typeof statusRequestGeneration === 'number') statusRequestGeneration++;
  if (typeof virtualHistoryRefreshGeneration === 'number') virtualHistoryRefreshGeneration++;
  if (typeof syncTurnControls === 'function') syncTurnControls();
  if (typeof updateSendBtn === 'function') updateSendBtn();
}

// A late storage error can arrive after files or transcript have changed.
// Reconcile from the existing read-only session route, without reusing a saved
// resume token or replacing a draft with a speculative recovered prompt.
async function reconcilePartialRewind(state) {
  invalidateRewindContinuation(state.sessionId);
  state.requiresReopen = true;
  state.pending = false;
  state.error = '恢复可能已执行，但恢复记录未完整保存。请核对当前代码和对话，关闭后重新选择恢复点。';
  renderRewindDialog(state);
  try {
    const response = await fetch('/api/sessions/' + encodeURIComponent(state.sessionId), { cache: 'no-store', signal: state.abort?.signal });
    const actual = await response.json().catch(() => ({}));
    if (!rewindViewMatches(state)) { syncRewindView(); return; }
    if (!response.ok || actual.session?.id !== state.sessionId || !Array.isArray(actual.messages)) throw new Error('reload failed');
    messages = [];
    renderHistoryMessages(actual.messages);
    if (typeof clearTodoPlan === 'function') clearTodoPlan();
    // Input text and attachments remain exactly as they were; this is only a
    // read of the durable transcript, never an optimistic success transition.
  } catch (_) {
    if (!rewindViewMatches(state)) { syncRewindView(); return; }
    state.error = '恢复可能已执行，但恢复记录未完整保存。当前对话也未能重新读取，请刷新会话并核对代码和对话后再操作。';
  }
  if (rewindViewMatches(state)) renderRewindDialog(state);
}

async function confirmRewind(event) {
  event?.preventDefault();
  const state = rewindDialog;
  if (!rewindViewMatches(state) || state.loading || state.pending || state.completed || state.requiresReopen || !rewindSelectedPoint(state)) return;
  if (state.mode !== 'conversation' && rewindSelectedPoint(state).codeAvailable !== true) return;
  state.pending = true;
  state.error = '';
  renderRewindDialog(state);
  try {
    const res = await fetch('/api/rewind', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId: state.sessionId, turn: state.turn, mode: state.mode, revision: state.revision }) });
    const data = await res.json().catch(() => ({}));
    if (!rewindViewMatches(state)) { syncRewindView(); return; }
    if (!res.ok && data.reloadRequired === true) { await reconcilePartialRewind(state); return; }
    if (!res.ok) throw new Error(data.error || '回退未完成。');
    if (data.sessionId !== state.sessionId || !Array.isArray(data.messages) || data.changed !== true) throw new Error('回退结果尚未确认，请重新读取会话。');
    invalidateRewindContinuation(state.sessionId);
    // A code-only restore never replaces the transcript, scroll position or draft.
    if (state.mode !== 'code') {
      messages = [];
      renderHistoryMessages(data.messages);
      if (typeof clearTodoPlan === 'function') clearTodoPlan();
      const composer = document.getElementById('inputField');
      const prefill = String(data.prefill || '');
      if (prefill && composer?.value) {
        state.completed = { prefill };
      } else if (composer) {
        composer.value = prefill;
        autoResize(composer);
      }
      if (typeof saveComposerDraftForView === 'function') saveComposerDraftForView();
    }
    // Do not await an unrelated sidebar refresh before acknowledging the result.
    if (typeof loadSessions === 'function') void loadSessions();
    if (typeof loadSessionStatsbar === 'function') void loadSessionStatsbar();
    showToast(state.mode === 'code' ? '代码已恢复，对话与草稿已保留。' : state.mode === 'conversation' ? '对话已恢复，当前代码已保留。' : '代码和对话已恢复。');
    state.pending = false;
    if (state.completed) renderRewindDialog(state);
    else closeRewindDialog();
  } catch (error) {
    if (!rewindViewMatches(state)) { syncRewindView(); return; }
    state.error = rewindFriendlyError(error);
    state.pending = false;
    renderRewindDialog(state);
  }
}

function rewindDialogKeydown(event) {
  const state = rewindDialog;
  if (!rewindViewMatches(state)) { syncRewindView(); return; }
  if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); closeRewindDialog(); return; }
  if (event.key !== 'Tab') return;
  const controls = Array.from(state.overlay.querySelectorAll('button:not(:disabled), input:not(:disabled), textarea:not(:disabled)'));
  if (!controls.length) return;
  const first = controls[0], last = controls[controls.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}
