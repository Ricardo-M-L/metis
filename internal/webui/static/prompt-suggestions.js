// Suggested text is presentation only until the user explicitly accepts it.
function createPromptSuggestionController(options) {
  let generation = 0, text = '', owner = '', lastKey = '', enabled = true;
  let controller = null, timer = null;
  function clear() {
    const displayed = !!text && (!options.visible || options.visible(text));
    generation++;
    if (controller) controller.abort();
    if (timer !== null) clearTimeout(timer);
    controller = null; timer = null; text = ''; owner = '';
    if (displayed) options.display('');
  }
  function visible() {
    return enabled && !!text && owner === options.session() && options.eligible()
      && (!options.visible || options.visible(text));
  }
  function accept() {
    if (!visible()) return false;
    const accepted = text;
    clear();
    options.fill(accepted);
    return true;
  }
  async function request(sessionId, turnKey) {
    const key = sessionId + ':' + turnKey;
    if (!enabled || !sessionId || !turnKey || sessionId !== options.session() || !options.eligible() || key === lastKey) return;
    clear();
    lastKey = key;
    const mine = generation, abort = new AbortController();
    controller = abort;
    const timeout = setTimeout(() => abort.abort(), 8000);
    timer = timeout;
    try {
      const data = await options.request(sessionId, abort.signal);
      if (generation !== mine || abort.signal.aborted || sessionId !== options.session() || !options.eligible() || !enabled) return;
      const candidate = typeof data.suggestion === 'string' ? data.suggestion.trim() : '';
      if (data.enabled === false || !candidate || [...candidate].length > 160 || /[\r\n\u0000-\u001f\u007f]/.test(candidate)) return;
      owner = sessionId;
      text = candidate;
      options.display(text);
    } catch (_) {
      // An optional suggestion never turns a completed task into an error.
    } finally {
      clearTimeout(timeout);
      if (generation === mine) { controller = null; timer = null; }
    }
  }
  function handleKey(event) {
    if (event.isComposing || event.keyCode === 229) { clear(); return false; }
    if (event.key === 'Tab') {
      if (event.shiftKey || event.altKey || event.ctrlKey || event.metaKey || !accept()) return false;
      event.preventDefault();
      return true;
    }
    const dismiss = event.key === 'Escape' && (visible() || controller !== null);
    clear();
    if (dismiss) event.preventDefault();
    return dismiss;
  }
  return {request, clear, accept, handleKey, hasSuggestion: visible,
    setEnabled(value) { enabled = !!value; if (!enabled) clear(); }};
}

let composerPromptSuggestions = null;
let pendingComposerSuggestion = null;
let composerSuggestionComposing = false;

function composerSuggestionEligible() {
  const input = document.getElementById('inputField');
  const sessionId = typeof currentSessionId === 'undefined' ? '' : currentSessionId;
  const busy = typeof isViewedTurnRunning === 'function' ? isViewedTurnRunning() : typeof turnRunning !== 'undefined' && turnRunning;
  return !!input && !!sessionId && input.value === '' && !busy && !composerSuggestionComposing
    && !(typeof attachments !== 'undefined' && attachments.length)
    && !(typeof queuedTurns !== 'undefined' && queuedTurns.length)
    && !(typeof pendingAsk !== 'undefined' && pendingAsk)
    && !(typeof pendingSessionId !== 'undefined' && pendingSessionId)
    && !(typeof pendingForegroundRequest !== 'undefined' && pendingForegroundRequest && pendingForegroundRequest.sessionId === sessionId);
}

function clearComposerPromptSuggestion() {
  pendingComposerSuggestion = null;
  if (composerPromptSuggestions) composerPromptSuggestions.clear();
}

function markComposerPromptSuggestion(state, sessionId, turnKey) {
  if (state !== 'completed' || !sessionId || !turnKey) { clearComposerPromptSuggestion(); return; }
  pendingComposerSuggestion = {sessionId, turnKey};
  refreshComposerPromptSuggestion();
}

function refreshComposerPromptSuggestion() {
  if (!composerPromptSuggestions) return;
  if (!composerSuggestionEligible()) {
    composerPromptSuggestions.clear();
    return;
  }
  if (!pendingComposerSuggestion) return;
  const pending = pendingComposerSuggestion;
  pendingComposerSuggestion = null;
  if (pending.sessionId !== currentSessionId) return;
  void composerPromptSuggestions.request(pending.sessionId, pending.turnKey);
}

function setComposerPromptSuggestionsEnabled(value) {
  clearComposerPromptSuggestion();
  if (composerPromptSuggestions) composerPromptSuggestions.setEnabled(value === true || value === 'true');
}

if (typeof document !== 'undefined') {
  const input = document.getElementById('inputField');
  if (input) {
    const originalPlaceholder = input.placeholder;
    composerPromptSuggestions = createPromptSuggestionController({
      session: () => currentSessionId,
      eligible: composerSuggestionEligible,
      visible: text => input.placeholder === text,
      async request(sessionId, signal) {
        const response = await fetch('/api/prompt-suggestion', {method: 'POST', signal,
          headers: {'Content-Type': 'application/json'}, body: JSON.stringify({sessionId})});
        if (!response.ok) throw new Error('suggestion unavailable');
        return response.json();
      },
      display(text) {
        input.placeholder = text || (typeof uiText === 'function'
          ? uiText('Describe what you want to build', '描述你想要构建的内容') : originalPlaceholder);
        input.setAttribute('aria-autocomplete', text ? 'inline' : 'list');
        if (text) input.setAttribute('aria-description', typeof uiText === 'function'
          ? uiText('Press Tab to accept this suggestion. Enter does not accept it.', '按 Tab 接受这条建议，Enter 不会自动接受。')
          : 'Press Tab to accept this suggestion.');
        else input.removeAttribute('aria-description');
      },
      fill(text) {
        input.value = text;
        input.setSelectionRange(text.length, text.length);
        autoResize(input);
        updateCommandMenu(text);
      }
    });
    input.addEventListener('compositionstart', () => { composerSuggestionComposing = true; clearComposerPromptSuggestion(); });
    input.addEventListener('compositionend', () => { composerSuggestionComposing = false; });
  }
}
