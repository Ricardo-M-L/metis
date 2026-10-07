package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Exercise the actual live/history renderer so a readable compact error never
// changes the outcome or replaces the raw evidence kept by the inspector.
func TestToolErrorSummaryPreservesEvidence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	fixture, _, ok := strings.Cut(toolRowBrowserScript, "async function runScenario() {")
	if !ok {
		t.Fatal("missing tool browser fixture boundary")
	}
	for _, scenario := range []string{"unattended", "webfetch", "read_missing", "history", "classification_guards"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(node, "-e", fixture+toolErrorSummaryBrowserScript, scenario)
			cmd.Stdin = bytes.NewReader(source)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("tool error summary: %v\n%s", err, output)
			}
		})
	}
}

const toolErrorSummaryBrowserScript = `
const denied = 'denied by unattended admission policy: unauthorized';
const fetch403 = 'HTTP 403 Forbidden\n<html>Access denied by upstream</html>';
const fetch404 = 'HTTP 404 Not Found\n<html>Missing document</html>';
const missingFile = 'denied: lstat /var/tmp/metis/fixture-checks.md: no such file or directory';
const start = (id, tool, input) => {
  context.handleToolStart({tool, id, input});
  flushFrames();
  return area.querySelectorAll('.call-row').at(-1);
};
function assertFailureEvidence(row, output, input) {
  assert.equal(row.dataset.state, 'error', 'translation must preserve the failed outcome');
  assert(row.querySelector('.tc-summary').classList.contains('err'));
  assert.equal(context.toolDetails[rowKey(row)].output, output, 'inspector stores the original result');
  inspectRow(row);
  context.switchDetailTab('output');
  assert.equal(details.detailsBody.innerHTML, '<div class="details-pre">' + output + '</div>',
    'the output tab still displays the full original diagnostic');
  if (input !== undefined) {
    assert.equal(context.toolDetails[rowKey(row)].input, input, 'input is unchanged');
    assert.equal(row.querySelector('.tc-io-text[data-out]').textContent, output,
      'expanding the row still exposes raw output');
  }
}
if (scenario === 'unattended') {
  const input = JSON.stringify({path:'report.html', content:'report'});
  const row = start('write-denied', 'Write', input);
  context.handleToolResult({tool:'Write', id:'write-denied', isError:true, output:denied});
  assert.match(row.querySelector('.tc-summary').textContent, /无人值守任务未获该操作授权/);
  assertFailureEvidence(row, denied, input);
  context.lang = 'en'; context.refreshActivityGroupLanguage();
  assert.match(row.querySelector('.tc-summary').textContent, /Unattended task is not authorized/);
  context.lang = 'zh'; context.refreshActivityGroupLanguage();
  assert.match(row.querySelector('.tc-summary').textContent, /无人值守任务未获该操作授权/);
  context.handleToolResult({tool:'Agent', id:'denied-before-start', traceCallId:'denied-call',
    isError:true, output:denied});
  const beforeStart = area.querySelectorAll('.call-row').at(-1);
  assert.match(beforeStart.querySelector('.tc-summary').textContent, /无人值守任务未获该操作授权/);
  assertFailureEvidence(beforeStart, denied);
} else if (scenario === 'webfetch') {
  for (const [id, output, zh, en] of [
    ['forbidden', fetch403, /网页拒绝访问.*403/, /Website refused access.*403/],
    ['missing', fetch404, /页面不存在.*404/, /Page not found.*404/],
  ]) {
    const input = JSON.stringify({url:'https://example.com/' + id});
    const row = start(id, 'WebFetch', input);
    context.handleToolResult({tool:'WebFetch', id, isError:true, output});
    assert.match(row.querySelector('.tc-summary').textContent, zh);
    assertFailureEvidence(row, output, input);
    context.lang = 'en'; context.refreshActivityGroupLanguage();
    assert.match(row.querySelector('.tc-summary').textContent, en);
    context.lang = 'zh'; context.refreshActivityGroupLanguage();
    assert.match(row.querySelector('.tc-summary').textContent, zh);
  }
} else if (scenario === 'read_missing') {
  for (const [id, output] of [
    ['missing-read', missingFile],
    ['missing-read-zh', '读取失败：文件不存在'],
  ]) {
    const input = JSON.stringify({path:'/var/tmp/metis/fixture-checks.md'});
    const row = start(id, 'Read', input);
    context.handleToolResult({tool:'Read', id, isError:true, output});
    assert.equal(row.querySelector('.tc-summary').textContent, '文件不存在，查看详情');
    assertFailureEvidence(row, output, input);
    context.lang = 'en'; context.refreshActivityGroupLanguage();
    assert.equal(row.querySelector('.tc-summary').textContent, 'File not found; inspect details');
    context.lang = 'zh'; context.refreshActivityGroupLanguage();
  }
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Read the missing file'}]},
    {role:'assistant', content:[{type:'tool_use', name:'Read', tool_use_id:'saved-missing', input:{path:'/var/tmp/metis/fixture-checks.md'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'saved-missing', is_error:true, content:missingFile}]},
  ]);
  flushFrames();
  const row = area.querySelector('.call-row');
  assert.equal(row.querySelector('.tc-summary').textContent, '文件不存在，查看详情');
  assertFailureEvidence(row, missingFile);
} else if (scenario === 'history') {
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'Read and save the report'}]},
    {role:'assistant', content:[{type:'tool_use', name:'WebFetch', tool_use_id:'old-fetch', input:{url:'https://example.com/missing'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'old-fetch', is_error:true, content:fetch404}]},
    {role:'assistant', content:[{type:'tool_use', name:'Write', tool_use_id:'old-write', input:{path:'report.html', content:'report'}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'old-write', is_error:true, content:denied}]},
    {role:'assistant', content:[{type:'text', text:'The page could not be read or saved.'}]},
  ]);
  flushFrames();
  const [fetchRow, writeRow] = area.querySelectorAll('.call-row');
  assert.match(fetchRow.querySelector('.tc-summary').textContent, /页面不存在.*404/);
  assert.match(writeRow.querySelector('.tc-summary').textContent, /无人值守任务未获该操作授权/);
  assertFailureEvidence(fetchRow, fetch404);
  assertFailureEvidence(writeRow, denied);
  assert.match(area.querySelector('.activity-group-summary').getAttribute('aria-label'), /2 项失败/);
} else if (scenario === 'classification_guards') {
  const ok = start('successful-page', 'WebFetch', JSON.stringify({url:'https://example.com/guide'}));
  context.handleToolResult({tool:'WebFetch', id:'successful-page', output:'Guide: HTTP 404 and denied by unattended admission policy: unauthorized'});
  assert.equal(ok.dataset.state, 'ok', 'successful page content must not be classified as a failure');
  assert.doesNotMatch(ok.querySelector('.tc-summary').textContent, /页面不存在|未获该操作授权/);
  const readOk = start('successful-read', 'Read', JSON.stringify({path:'diagnostic-guide.md'}));
  context.handleToolResult({tool:'Read', id:'successful-read', output:missingFile});
  context.refreshActivityGroupLanguage();
  assert.equal(readOk.dataset.state, 'ok', 'success remains success even when file content quotes a missing-file diagnostic');
  assert.equal(readOk.querySelector('.tc-summary').textContent, 'diagnostic-guide.md');
  assert.equal(context.toolDetails[rowKey(readOk)].output, missingFile);
  for (const [id, tool, output] of [
    ['command-http', 'Bash', fetch403],
    ['command-missing', 'Bash', missingFile],
    ['read-permission', 'Read', 'open /var/tmp/no such file or directory: permission denied'],
    ['nested-http', 'WebFetch', 'Proxy connection failed\nHTTP 404 Not Found'],
    ['different-policy', 'Write', 'denied by unattended admission policy: dangerous_pattern:rm'],
  ]) {
    const row = start(id, tool, '{}');
    context.handleToolResult({tool, id, isError:true, output});
    assert.equal(row.querySelector('.tc-summary').textContent, output.split('\n')[0],
      'unrecognized diagnostics retain their original meaning');
    assertFailureEvidence(row, output);
  }
}
console.log('tool error summary verified: ' + scenario);
`
