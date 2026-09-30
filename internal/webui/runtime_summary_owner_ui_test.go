package webui

import "testing"

func TestDesktopRuntimeSummaryOnlyShowsSelectedSessionContext(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'))['chat.js'];
const start = source.indexOf('function showRuntimeSummary(kind)');
const end = source.indexOf('async function compactCurrentSession(', start);
assert.ok(start >= 0 && end > start, 'cannot isolate runtime summary');
const toasts = [];
let language = 'en';
const c = {
  currentSessionId: 'A',
  lastStatusSnapshot: {contextSessionId:'A', contextUsed:64000, contextWindow:128000,
    workspace:'workspace', executionStrategy:'direct', subAgents:0, backgroundTasks:0},
  fmtTokens(value) { return String(value); },
  uiText(en, zh) { return language === 'zh-CN' ? zh : en; },
  showToast(message) { toasts.push(message); }
};
vm.createContext(c);
vm.runInContext(source.slice(start, end), c);
const show = kind => {c.showRuntimeSummary(kind); return toasts.pop()};
assert.equal(show('context'), 'Context: 64000 / 128000 tokens');
assert.match(show('status'), /context 50%/);

// A blank composer and another selected session cannot inherit A's Loop.
for (const selected of [null, 'B']) {
  c.currentSessionId = selected;
  assert.equal(show('context'), 'Context: unavailable for this session');
  assert.doesNotMatch(show('status'), /context|50%|64000|128000/);
}

// When B owns the reported context, show B; switching back to A hides B.
c.lastStatusSnapshot = {...c.lastStatusSnapshot,
  contextSessionId:'B', contextUsed:24000, contextWindow:96000};
assert.equal(show('context'), 'Context: 24000 / 96000 tokens');
assert.match(show('status'), /context 25%/);
c.currentSessionId = 'A';
assert.equal(show('context'), 'Context: unavailable for this session');
assert.doesNotMatch(show('status'), /context|25%|24000|96000/);

// Legacy snapshots without provenance cannot authorize context disclosure.
c.lastStatusSnapshot.contextSessionId = undefined;
assert.equal(show('context'), 'Context: unavailable for this session');
assert.doesNotMatch(show('status'), /context|24000|96000/);

// The command toast uses the configured language for owned and missing context.
language = 'zh-CN';
c.currentSessionId = 'B';
c.lastStatusSnapshot.contextSessionId = 'B';
assert.equal(show('context'), '上下文：24000 / 96000 词元');
assert.match(show('status'), /上下文 25%/);
c.currentSessionId = null;
assert.equal(show('context'), '当前会话暂无上下文数据');
assert.doesNotMatch(show('status'), /上下文|24000|96000/);
`, "chat.js")
}
