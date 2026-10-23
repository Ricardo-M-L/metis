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
	for _, scenario := range []string{"unattended", "webfetch", "read_missing", "ls_directory", "ls_errors", "ls_history", "history", "classification_guards"} {
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
} else if (scenario === 'ls_directory') {
  const path = '/project/native/system/src';
  const input = JSON.stringify({path});
  const row = start('list-directory', 'LS', input);
  assert.equal(row.querySelector('.tc-title').textContent, '列出目录');
  assert.equal(row.querySelector('.tc-summary').textContent, path);
  assert.equal(row.dataset.variant, 'read', 'LS uses the directory icon rather than a generic tool icon');
  context.handleToolResult({tool:'LS', id:'list-directory', output:'entry/\npackage.json'});
  assert.equal(row.dataset.state, 'ok');
  assert.equal(row.querySelector('.tc-summary').textContent, path);
  context.lang = 'en'; context.refreshActivityGroupLanguage();
  assert.equal(row.querySelector('.tc-title').textContent, 'List directory');
  assert.match(row.querySelector('.tc-disclosure').getAttribute('aria-label'), /List directory.*native\/system\/src.*Completed/);
} else if (scenario === 'ls_errors') {
  const path = '/project/native/system/src';
  for (const [id, output, zh, en] of [
    ['missing-directory', 'open ' + path + ': no such file or directory', '目录不存在', 'Directory not found'],
    ['missing-directory-semantic', 'Directory not found: ' + path + '.\nNearest existing parent: /project/native/system\nUse LS on that parent first.\nOriginal error: open ' + path + ': no such file or directory', '目录不存在', 'Directory not found'],
    ['file-directory', 'open ' + path + ': not a directory', '该路径不是目录', 'Path is not a directory'],
    ['file-directory-semantic', path + ' is a regular file, not a directory. Use Read to view its contents, or pass the parent directory to LS.', '该路径不是目录', 'Path is not a directory'],
    ['denied-directory', 'open ' + path + ': permission denied', '无权读取目录', 'Cannot access directory'],
    ['denied-directory-windows', 'open C:\\project\\src: Access is denied.', '无权读取目录', 'Cannot access directory'],
    ['missing-directory-windows', 'open C:\\project\\src: The system cannot find the path specified.', '目录不存在', 'Directory not found'],
  ]) {
    const input = JSON.stringify({path});
    const row = start(id, 'LS', input);
    context.handleToolResult({tool:'LS', id, isError:true, output});
    assert.equal(row.querySelector('.tc-title').textContent, '列出目录');
    assert.equal(row.querySelector('.tc-summary').textContent, zh + ' · ' + path);
    assert.doesNotMatch(row.querySelector('.tc-summary').textContent, /^open /);
    context.lang = 'en'; context.refreshActivityGroupLanguage();
    assert.equal(row.querySelector('.tc-title').textContent, 'List directory');
    assert.equal(row.querySelector('.tc-summary').textContent, en + ' · ' + path);
    assertFailureEvidence(row, output, input);
    context.lang = 'zh'; context.refreshActivityGroupLanguage();
  }
  const longPath = '/Users/a/Documents/' + 'long-project-name/'.repeat(8) + 'native/system/src';
  const row = start('long-missing-directory', 'LS', JSON.stringify({path:longPath}));
  const output = 'open ' + longPath + ': no such file or directory';
  context.handleToolResult({tool:'LS', id:'long-missing-directory', isError:true, output});
  const summary = row.querySelector('.tc-summary').textContent;
  assert(summary.length < 110, 'long absolute paths stay compact in the error summary');
  assert(summary.endsWith('native/system/src'), 'the useful path tail remains visible');
  assertFailureEvidence(row, output, JSON.stringify({path:longPath}));
  context.handleToolResult({tool:'LS', id:'missing-before-start', isError:true, output:'open /missing: no such file or directory'});
  const beforeStart = area.querySelectorAll('.call-row').at(-1);
  assert.equal(beforeStart.querySelector('.tc-title').textContent, '列出目录');
  assert.equal(beforeStart.querySelector('.tc-summary').textContent, '目录不存在，查看详情');
} else if (scenario === 'ls_history') {
  const path = '/project/native/system/src';
  const output = 'open ' + path + ': no such file or directory';
  context.renderHistoryMessages([
    {role:'user', content:[{type:'text', text:'List the directory'}]},
    {role:'assistant', content:[{type:'tool_use', name:'LS', tool_use_id:'old-ls', input:{path}}]},
    {role:'user', content:[{type:'tool_result', tool_use_id:'old-ls', is_error:true, content:output}]},
  ]);
  flushFrames();
  const row = area.querySelector('.call-row');
  assert.equal(row.querySelector('.tc-title').textContent, '列出目录');
  assert.equal(row.querySelector('.tc-summary').textContent, '目录不存在 · ' + path);
  assertFailureEvidence(row, output);
  context.lang = 'en'; context.refreshActivityGroupLanguage();
  assert.equal(row.querySelector('.tc-title').textContent, 'List directory');
  assert.equal(row.querySelector('.tc-summary').textContent, 'Directory not found · ' + path);
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
  const lsOk = start('successful-ls', 'LS', JSON.stringify({path:'/project'}));
  context.handleToolResult({tool:'LS', id:'successful-ls', output:'no such file or directory\npermission denied'});
  context.refreshActivityGroupLanguage();
  assert.equal(lsOk.dataset.state, 'ok', 'directory entries that look like errors are still successful output');
  assert.equal(lsOk.querySelector('.tc-summary').textContent, '/project');
  const newlinePath = '/project/: permission denied\nfilename';
  const newlineInput = JSON.stringify({path:newlinePath});
  const newlineOutput = 'open ' + newlinePath + ': too many open files';
  const newlineRow = start('newline-in-directory-path', 'LS', newlineInput);
  context.handleToolResult({tool:'LS', id:'newline-in-directory-path', isError:true, output:newlineOutput});
  assert.equal(newlineRow.querySelector('.tc-summary').textContent, newlineOutput.split('\n')[0],
    'a diagnostic-looking first line inside a Unix filename is not the OS error suffix');
  assertFailureEvidence(newlineRow, newlineOutput, newlineInput);
  for (const [id, tool, output] of [
    ['command-http', 'Bash', fetch403],
    ['command-missing', 'Bash', missingFile],
    ['read-permission', 'Read', 'open /var/tmp/no such file or directory: permission denied'],
    ['nested-http', 'WebFetch', 'Proxy connection failed\nHTTP 404 Not Found'],
    ['different-policy', 'Write', 'denied by unattended admission policy: dangerous_pattern:rm'],
    ['unknown-list-error', 'LS', 'Filesystem temporarily unavailable; retry later'],
    ['path-named-missing', 'LS', 'open /project/no such file or directory: too many open files'],
    ['path-named-not-directory', 'LS', 'open /project/name is a regular file, not a directory.txt: too many open files'],
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
