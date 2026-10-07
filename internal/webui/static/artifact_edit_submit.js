// Send an explicitly submitted artifact edit through the ordinary foreground
// turn. The main composer draft and its attachments belong to the user and
// must not be consumed by a preview action or mistaken for an AskUser reply.
async function submitArtifactAnnotationPrompt(prompt, reference, sessionId) {
  if (typeof prompt !== 'string' || !prompt.trim() || !reference ||
      typeof reference.artifactId !== 'string' || !reference.artifactId ||
      !Number.isInteger(reference.version) || reference.version < 1 ||
      !/^[a-f0-9]{64}$/.test(reference.digest || '') ||
      typeof reference.targetId !== 'string' || !reference.targetId) return false;
  if (!sessionId || String(currentSessionId || '') !== String(sessionId)) {
    showToast(uiText('Return to the artifact session before submitting this edit.', '请回到该产物所属会话后再提交修改。'));
    return false;
  }
  if (pendingAsk || isViewedTurnRunning() ||
      (turnRunning && runningSessionId && currentSessionId !== runningSessionId && !parallelTurnsEnabled())) {
    showToast(uiText('Finish the current task before submitting this edit. Your request is kept here.', '请先完成当前任务，再提交修改；这里的修改要求会保留。'));
    return false;
  }
  closeArtifactPreview({ navigation: false });
  switchView('chat');
  // runTurnItem sets the running state synchronously before its first await,
  // so a second submission cannot start another turn in the same session.
  void runTurnItem({ text: prompt, images: [] }).catch(error => {
    showToast(uiText('Unable to start the artifact edit: ', '产物修改未能启动：') + (error.message || ''));
  });
  return true;
}

function artifactAnnotationMessageMarkup(content) {
  if (typeof content !== 'string' ||
      !content.startsWith('请根据用户在 HTML Artifact 上的点选，修改现有产物。')) return null;
  const marker = '\n```json\n';
  const start = content.indexOf(marker);
  const end = start < 0 ? -1 : content.indexOf('\n```', start + marker.length);
  if (end < 0) return null;
  try {
    const request = JSON.parse(content.slice(start + marker.length, end)).metis_artifact_annotation;
    if (!request || typeof request.artifactId !== 'string' || !request.artifactId ||
        !Number.isInteger(request.version) || request.version < 1 ||
        !/^[a-f0-9]{64}$/.test(request.digest || '') ||
        typeof request.instruction !== 'string' || !request.instruction.trim() ||
        typeof request.title !== 'string' || typeof request.selection?.text !== 'string') return null;
    const title = Array.from(request.title).slice(0, 120).join('');
    const target = Array.from(request.selection.text).slice(0, 160).join('');
    // Only the presentation is shortened. messages/history keep the complete
    // canonical prompt so copy, branches, and task evidence remain intact.
    return '<div class="artifact-edit-request"><div class="artifact-edit-request-meta">' +
      escHtml(uiText('Edit ', '修改 ') + '「' + title + '」 · v' + request.version) + '</div>' +
      '<div class="artifact-edit-request-target">' + escHtml(uiText('Selected: ', '选中：') + target) + '</div>' +
      '<div class="artifact-edit-request-text">' + escHtml(request.instruction) + '</div></div>';
  } catch (_) { return null; }
}
