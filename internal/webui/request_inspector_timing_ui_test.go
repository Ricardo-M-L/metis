package webui

import "testing"

func TestRequestInspectorTimingIgnoresDiagnosticTimestamps(t *testing.T) {
	runSelectionRaceBrowserHarness(t, "static/trace.js", `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const c = {
  console,
  document: {documentElement: {lang: 'en'}},
  escHtml: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'),
};
vm.createContext(c);
vm.runInContext(source, c);
const event = (kind, seconds, text) => ({
  kind, turn: 1, ts: new Date(Date.UTC(2026, 10, 2, 12, 0, seconds)).toISOString(), text,
});
const original = [
  event('text', 0, 'answer'),
  event('tokens', 10, 'input=11 output=7 cache_write=3 cache_read=5'),
  event('loop_done', 10, ''),
];
const early = event('request_timing', -10, '{"phase":"pre_request_start"}');
const late = event('request_timing', 30, '{"phase":"request_complete"}');
function inspect(events) {
  c.events = events;
  vm.runInContext('traceRows = buildTraceRows(events); traceSelectedReq = 1;', c);
  const rows = vm.runInContext('traceRows', c);
  const html = {};
  for (const tab of ['timing', 'summary', 'usage']) {
    c.tab = tab;
    vm.runInContext('traceTab = tab', c);
    const insp = {style: {}, innerHTML: ''};
    // Executes the real requestBody through the real inspector renderer.
    c.renderRequestInspector(insp);
    assert.equal(insp.style.display, 'flex');
    html[tab] = insp.innerHTML;
  }
  return {rows, html};
}
const baseline = inspect(original);
assert.equal(baseline.rows.length, 2, 'loop_done is a boundary, not a duration endpoint');
assert.ok(baseline.html.timing.includes('<dt>Started</dt><dd>' + c.fmtTimestamp(original[0].ts) + '</dd>'));
assert.ok(baseline.html.timing.includes('<dt>Duration</dt><dd>10.0s</dd>'), 'text plus tokens establishes the 10s baseline without USER');
assert.ok(baseline.html.summary.includes('19 + 7 tok'));
assert.ok(baseline.html.summary.includes('Completed'));
for (const diagnostics of [[early], [late], [early, late]]) {
  const actual = inspect([...original, ...diagnostics]);
  assert.equal(actual.html.timing, baseline.html.timing, diagnostics.map(x => x.text).join(', ') + ': Started and Duration must not change');
  assert.equal(actual.html.summary, baseline.html.summary, 'tokens, calls and state must not change');
  assert.equal(actual.html.usage, baseline.html.usage, 'usage must not change');
  assert.deepEqual(actual.rows.filter(x => x.kind !== 'request_timing'), baseline.rows, 'existing rows must not change');
  const diagnosticRows = actual.rows.filter(x => x.kind === 'request_timing');
  assert.equal(diagnosticRows.length, diagnostics.length, 'diagnostic rows remain available');
  diagnosticRows.forEach((row, i) => {
    assert.equal(row.text, diagnostics[i].text);
    assert.equal(row.ts, diagnostics[i].ts);
    assert.ok(c.rowText(row).includes(diagnostics[i].text), 'diagnostic ledger text remains readable');
    assert.ok(c.inspectorBody(row, 'raw').includes(c.escHtml(c.formatTracePayload(diagnostics[i].text))), 'diagnostic raw inspector remains readable');
  });
}
// Preserve the existing text + loop_done behavior: only one displayed timestamp.
const textOnly = [original[0], original[2]];
const single = inspect(textOnly);
assert.equal(single.rows.length, 1);
assert.ok(single.html.timing.includes('<dt>Duration</dt><dd>—</dd>'));
assert.equal(inspect([...textOnly, early, late]).html.timing, single.html.timing);
`)
}
