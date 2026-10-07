// Composer statistics share one placement rule: start-aligned to the clicked
// pill, 8px above it, with a 12px viewport inset. They stay in their existing
// DOM seats so redraws and session changes retain the normal disclosure logic.
let composerStatPopoverObserver = null;
let composerStatPopoverTargets = [];
let composerStatPopoverFrame = false;

// Sidebar details and composer disclosures share the same transient surface.
// Close through their existing helpers so aria state and resize observers stay
// synchronized when focus, navigation, or another disclosure takes ownership.
function closeDesktopTransientPopovers(keep) {
  if (keep !== 'session-detail' && typeof hideSessionDetail === 'function') hideSessionDetail();
  if (keep !== 'session-stats' && typeof closeSessionStatsPopover === 'function') closeSessionStatsPopover();
  if (keep !== 'token-stats' && typeof closeTokenStatsPopover === 'function') closeTokenStatsPopover();
  if (keep !== 'context-meter' && typeof closeContextMeterPopover === 'function') closeContextMeterPopover();
  if (keep !== 'agent-status' && typeof closeStatusPopover === 'function') closeStatusPopover();
}

function closeUnfocusedComposerStatPopovers(target) {
  for (const [triggerID, panelID, close] of [
    ['sessionStatsTrigger', 'sessionStatsPopover', typeof closeSessionStatsPopover === 'function' ? closeSessionStatsPopover : null],
    ['tokenStatsTrigger', 'tokenStatsPopover', typeof closeTokenStatsPopover === 'function' ? closeTokenStatsPopover : null],
    ['contextMeter', 'contextMeterPopover', typeof closeContextMeterPopover === 'function' ? closeContextMeterPopover : null],
  ]) {
    const trigger = document.getElementById(triggerID);
    const panel = document.getElementById(panelID);
    if (close && panel && !panel.hidden && !panel.contains(target) && !(trigger && trigger.contains(target))) close();
  }
}

function positionComposerStatPopover(trigger, panel) {
  if (!trigger || !panel || panel.hidden) return;
  const anchor = trigger.getBoundingClientRect();
  const viewport = window.visualViewport;
  const leftEdge = viewport ? viewport.offsetLeft : 0;
  const topEdge = viewport ? viewport.offsetTop : 0;
  const width = viewport ? viewport.width : document.documentElement.clientWidth || window.innerWidth;
  const height = viewport ? viewport.height : document.documentElement.clientHeight || window.innerHeight;
  const margin = 12, gap = 8;
  panel.style.minWidth = Math.max(0, Math.min(300, width - margin * 2)) + 'px';
  panel.style.maxWidth = Math.max(0, Math.min(440, width - margin * 2)) + 'px';
  // Preserve the gap above the trigger in short windows. The panel scrolls
  // internally instead of overlapping its own button or escaping the screen.
  panel.style.maxHeight = Math.max(0, Math.min(height - margin * 2, anchor.top - topEdge - margin - gap)) + 'px';
  const bounds = panel.getBoundingClientRect();
  const left = Math.max(leftEdge + margin, Math.min(anchor.left, leftEdge + width - bounds.width - margin));
  const top = Math.max(topEdge + margin, Math.min(anchor.top - bounds.height - gap, topEdge + height - bounds.height - margin));
  panel.style.left = Math.round(left) + 'px';
  panel.style.top = Math.round(top) + 'px';
}

function refreshComposerStatPopovers() {
  const targets = [];
  for (const [triggerID, panelID] of [
    ['sessionStatsTrigger', 'sessionStatsPopover'],
    ['tokenStatsTrigger', 'tokenStatsPopover'],
    ['contextMeter', 'contextMeterPopover'],
  ]) {
    const trigger = document.getElementById(triggerID);
    const panel = document.getElementById(panelID);
    if (!trigger || !panel || panel.hidden) continue;
    positionComposerStatPopover(trigger, panel);
    targets.push(trigger, panel);
    const container = trigger.closest('.input-container');
    const dock = document.getElementById('composerRuntimeDock');
    if (container) targets.push(container);
    if (dock) targets.push(dock);
  }
  if (targets.length === composerStatPopoverTargets.length &&
      targets.every((target, i) => target === composerStatPopoverTargets[i])) return;
  if (composerStatPopoverObserver) composerStatPopoverObserver.disconnect();
  composerStatPopoverTargets = targets;
  if (targets.length && typeof ResizeObserver !== 'undefined') {
    if (!composerStatPopoverObserver) composerStatPopoverObserver = new ResizeObserver(scheduleComposerStatPopoverPosition);
    targets.forEach(target => composerStatPopoverObserver.observe(target));
  }
}

function scheduleComposerStatPopoverPosition() {
  if (composerStatPopoverFrame) return;
  composerStatPopoverFrame = true;
  window.requestAnimationFrame(() => {
    composerStatPopoverFrame = false;
    refreshComposerStatPopovers();
  });
}

window.addEventListener('resize', scheduleComposerStatPopoverPosition);
window.addEventListener('scroll', scheduleComposerStatPopoverPosition, true);
window.addEventListener('blur', () => closeDesktopTransientPopovers());
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', scheduleComposerStatPopoverPosition);
  window.visualViewport.addEventListener('scroll', scheduleComposerStatPopoverPosition);
}
