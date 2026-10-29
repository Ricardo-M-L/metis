// Code-checkpoint controls belong to one saved session. A blank composer gets
// its own ID on the first change; it never mutates the previously active worker.
let checkpointControlGeneration = 0;
let checkpointRequestGeneration = 0;
let checkpointSnapshotOwner = '';
let checkpointSnapshot = null;
let checkpointMutation = null;

function checkpointViewTarget() {
  return {sessionId: String(currentSessionId || ''), workspaceId: String(activeWorkspaceId || ''),
    generation: typeof resumeSessionGeneration === 'number' ? resumeSessionGeneration : 0};
}

function checkpointViewOwner() {
  const target = checkpointViewTarget();
  return JSON.stringify([target.sessionId, target.workspaceId, target.generation]);
}

function checkpointTargetIsCurrent(target) {
  const current = checkpointViewTarget();
  return current.workspaceId === target.workspaceId && current.generation === target.generation &&
    (target.allocating || current.sessionId === target.sessionId);
}

function checkpointIsPending() {
  return !!checkpointMutation && checkpointTargetIsCurrent(checkpointMutation);
}

function checkpointPaintSwitch() {
  const button = document.getElementById('checkpointBtn');
  if (!button) return;
  const known = checkpointSnapshotOwner === checkpointViewOwner() && checkpointSnapshot;
  const pending = checkpointIsPending();
  const text = '检查点：' + (known ? checkpointSnapshot.enabled ? '开' : '关' : pending ? '处理中…' : '…');
  if (button.textContent !== text) button.textContent = text;
  button.disabled = pending || !known;
  button.setAttribute('aria-checked', String(!!known && checkpointSnapshot.enabled));
  button.setAttribute('aria-busy', String(pending));
  button.setAttribute('aria-label', known ? text + '，点击' + (checkpointSnapshot.enabled ? '关闭' : '开启') + '当前会话的代码检查点' : '正在确认当前会话的代码检查点');
  button.title = pending ? '正在更新当前会话的检查点…' : known ? '点击切换，仅影响当前会话' : '正在确认检查点状态';
}

function renderCheckpointControl(state) {
  const owner = checkpointViewOwner();
  if (owner !== checkpointSnapshotOwner) {
    checkpointSnapshotOwner = owner;
    checkpointSnapshot = null;
  }
  const valid = !!state && typeof state.enabled === 'boolean' &&
    typeof state.sessionId === 'string' && state.sessionId === String(currentSessionId || '');
  if (valid) checkpointSnapshot = state;
  checkpointPaintSwitch();
  if (typeof syncRewindView === 'function') syncRewindView();
  return valid;
}

function checkpointShortError(error) {
  const message = String(error && error.message || '检查点设置未更新').replace(/^checkpoint:\s*/i, '');
  if (/workspace busy|session busy/i.test(message)) return '当前会话正在运行，请结束任务后再切换检查点';
  return message.length > 90 ? message.slice(0, 89) + '…' : message;
}

async function checkpointReadStatus(target, request, notify) {
  const query = new URLSearchParams({sessionId: target.sessionId, workspaceId: target.workspaceId});
  const res = await fetch('/api/checkpoint?' + query.toString());
  const data = await res.json().catch(() => ({}));
  if (!checkpointTargetIsCurrent(target) || request !== checkpointRequestGeneration) return null;
  if (!res.ok) throw new Error(data.error || '无法读取检查点状态');
  if (!renderCheckpointControl(data)) throw new Error('检查点状态与当前会话不一致，请重试');
  if (notify) showToast('检查点：' + (data.enabled ? '开' : '关'));
  return data;
}

async function toggleCheckpointControl(event) {
  if (event) { event.preventDefault(); event.stopPropagation(); }
  if (checkpointIsPending()) return;
  if (checkpointSnapshotOwner !== checkpointViewOwner() || !checkpointSnapshot) return runCheckpointControl('status');
  // Preserve this explicit desired state across draft ID allocation. Reading
  // the new session's default must not accidentally invert the user's choice.
  return runCheckpointControl(checkpointSnapshot.enabled ? 'off' : 'on');
}

async function runCheckpointControl(input) {
  const command = String(input || 'status').trim().toLowerCase();
  if (!['status', 'on', 'off'].includes(command)) {
    showToast('用法：/checkpoint on · off · status');
    return;
  }
  if (checkpointIsPending()) return;
  const target = checkpointViewTarget();
  const request = ++checkpointRequestGeneration;
  if (command === 'status') {
    try { return await checkpointReadStatus(target, request, true); }
    catch (error) {
      if (checkpointTargetIsCurrent(target) && request === checkpointRequestGeneration) showToast(checkpointShortError(error));
    }
    return;
  }

  target.allocating = !target.sessionId;
  checkpointMutation = target;
  checkpointControlGeneration++;
  checkpointPaintSwitch();
  try {
    if (target.allocating) {
      if (typeof prepareDraftSession !== 'function' || !await prepareDraftSession({force: true})) return;
      if (!checkpointTargetIsCurrent(target) || request !== checkpointRequestGeneration) return;
      target.sessionId = String(currentSessionId || '');
      target.allocating = false;
      if (!target.sessionId) throw new Error('无法为当前会话保存检查点设置');
      checkpointPaintSwitch();
    }
    if (!checkpointTargetIsCurrent(target) || request !== checkpointRequestGeneration) return;
    const res = await fetch('/api/checkpoint', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({sessionId: target.sessionId, command})
    });
    const data = await res.json().catch(() => ({}));
    if (!checkpointTargetIsCurrent(target) || request !== checkpointRequestGeneration) return;
    if (!res.ok) throw new Error(data.error || '检查点设置未更新');
    if (!renderCheckpointControl(data.checkpoint) || data.checkpoint.enabled !== (command === 'on')) {
      throw new Error('检查点状态尚未确认，请重试');
    }
    showToast(data.checkpoint.enabled ? '检查点已开启' : '检查点已关闭');
    return data.checkpoint;
  } catch (error) {
    if (!checkpointTargetIsCurrent(target) || request !== checkpointRequestGeneration) return;
    showToast(checkpointShortError(error));
    // A new saved ID may now exist even when the change failed. Refresh that
    // session's actual state rather than displaying an unconfirmed toggle.
    try { await checkpointReadStatus(target, request, false); } catch (_) { /* later polling retries */ }
  } finally {
    checkpointControlGeneration++;
    if (checkpointMutation === target) checkpointMutation = null;
    checkpointPaintSwitch();
  }
}
