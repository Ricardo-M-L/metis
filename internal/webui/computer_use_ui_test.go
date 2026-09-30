package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestComputerUseSettingsBrowserBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	const harness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const start = source.indexOf('// Computer Use settings only read status on entry.');
assert(start >= 0);
let zh = false, panelPresent = true;
const calls = [];
const panel = {innerHTML: '', attrs: {}, setAttribute(k, v) { this.attrs[k] = v; }};
const installed = {installed:true, enabled:false, running:false, source:'local', version:'1.2.3',
  message:'<script>untrusted</script>', path:'/tmp/<helper>', description:{platform:'darwin', permissions:{}}};
const c = {
  document: {getElementById: id => id === 'computerUsePanel' && panelPresent ? panel : null},
  uiText: (en, cn) => zh ? cn : en,
  escHtml: value => String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;'),
  fetch: async (url, options) => {calls.push({url, options}); return {ok:true, json:async () => installed};}
};
vm.createContext(c);
vm.runInContext(source.slice(start), c);
(async () => {
  // Rendering an installed state never launches, downloads or grants access.
  let markup = c.renderComputerUseTab();
  assert.equal(calls.length, 0);
  assert.match(markup, /Unknown/);
  await c.loadComputerUse();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, '/api/computer-use');
  assert.equal(calls[0].options.method, undefined);
  assert.equal(calls[0].options.cache, 'no-store');
  assert.match(panel.innerHTML, /Local build \(experimental\)/);
  assert.match(panel.innerHTML, /Unknown — refresh after installing/);
  assert.match(panel.innerHTML, />Disabled</);
  assert.match(panel.innerHTML, />Stopped</);
  assert.doesNotMatch(panel.innerHTML, />Ready</);
  assert.match(panel.innerHTML, /&lt;script&gt;untrusted&lt;\/script&gt;/);
  assert.doesNotMatch(panel.innerHTML, /<script>/);
  assert.match(panel.innerHTML, /\/tmp\/&lt;helper&gt;/);
  assert.match(panel.innerHTML, /permissions-accessibility/);
  assert.match(panel.innerHTML, /Request access/);
  assert.doesNotMatch(panel.innerHTML, /Open System Settings/);

  // A deliberate button action posts only the fixed action. Double clicks
  // cannot initiate concurrent installs, and a delayed response does not
  // replace another settings page after navigation.
  let release;
  c.fetch = (url, options) => { calls.push({url, options}); return new Promise(resolve => {release = resolve;}); };
  const pending = c.computerUseAction('enable');
  assert.equal(calls.length, 2);
  assert.equal(calls[1].options.method, 'POST');
  assert.equal(calls[1].options.body, '{"action":"enable"}');
  assert.equal(panel.attrs['aria-busy'], 'true');
  await c.computerUseAction('enable');
  await c.loadComputerUse();
  assert.equal(calls.length, 2);
  panelPresent = false;
  const prior = panel.innerHTML;
  release({ok:true, json:async () => ({...installed, enabled:true, running:true,
    description:{platform:'darwin', permissions:{accessibility:'granted', screenRecording:'denied'}}})});
  await pending;
  assert.equal(panel.innerHTML, prior);
  panelPresent = true;
  c.paintComputerUse();
  assert.match(panel.innerHTML, />Running</);
  assert.match(panel.innerHTML, /Not granted/);
  assert.doesNotMatch(panel.innerHTML, /permissions-accessibility/);
  assert.match(panel.innerHTML, /permissions-screen-recording/);
  assert.equal(panel.attrs['aria-busy'], 'false');

  // A named request is sent once. The operating system still owns approval.
  c.fetch = async (url, options) => {
    calls.push({url, options});
    return {ok:true, json:async () => ({...installed, enabled:true, running:true})};
  };
  await c.computerUseAction('permissions-accessibility');
  assert.equal(calls.length, 3);
  assert.equal(calls[2].options.body, '{"action":"permissions-accessibility"}');
  await c.computerUseAction('/tmp/arbitrary-helper');
  assert.equal(calls.length, 3);

  // Errors retain explicitly labelled last-known status and stay visible.
  c.fetch = async () => ({ok:false, json:async () => ({error:'No compatible release <published>'})});
  await c.computerUseAction('disable');
  assert.match(panel.innerHTML, /role="alert"/);
  assert.match(panel.innerHTML, /No compatible release &lt;published&gt;/);
  assert.match(panel.innerHTML, /Last known status/);
  zh = true;
  c.paintComputerUse();
  assert.match(panel.innerHTML, /本地构建（实验性）/);
  assert.match(panel.innerHTML, /授权仍由你完成/);
  assert.match(panel.innerHTML, /请求权限/);
  c.fetch = async () => ({ok:false, json:async () => ({error:'installed Computer Use helper did not return a permission descriptor; update or reinstall the component'})});
  await c.computerUseAction('permissions-screen-recording');
  assert.match(panel.innerHTML, /请更新或重新安装组件后重试/);
  assert.doesNotMatch(panel.innerHTML, /did not return a permission descriptor/);
  zh = false;
  c.paintComputerUse();
  assert.match(panel.innerHTML, /Update or reinstall the component/);
  zh = true;

  // Non-macOS status has no misleading macOS permission shortcut.
  c.fetch = async () => ({ok:true, json:async () => ({...installed,
    description:{platform:'linux', permissions:{accessibility:'unsupported', screenRecording:'unknown'}}})});
  await c.loadComputerUse();
  assert.doesNotMatch(panel.innerHTML, /onclick="computerUseAction\('permissions-/);
  assert.match(panel.innerHTML, /不支持/);
  c.fetch = async () => ({ok:true, json:async () => ({...installed,
    description:{platform:'darwin', permissions:{accessibility:'unsupported', screenRecording:'not-required'}}})});
  await c.loadComputerUse();
  assert.doesNotMatch(panel.innerHTML, /onclick="computerUseAction\('permissions-/);

  // Native permission spelling remains meaningful in either language.
  assert.equal(c.computerUsePermissionLabel('notGranted'), '未授权');
  assert.equal(c.computerUsePermissionLabel('runtime'), '运行时检查');
  zh = false;
  assert.equal(c.computerUsePermissionLabel('notGranted'), 'Not granted');
  assert.equal(c.computerUsePermissionLabel('runtime'), 'Checked when running');

  // Stop must interrupt an unresolved install/enable even before a running
  // connection exists. Its acknowledged state wins over the old enable reply.
  c.fetch = async () => ({ok:true, json:async () => installed});
  await c.loadComputerUse();
  const interruptionCalls = [];
  let finishEnable, finishStop;
  c.fetch = (url, options) => {
    const action = JSON.parse(options.body).action;
    interruptionCalls.push(action);
    return new Promise(resolve => {
      if (action === 'enable') finishEnable = resolve;
      else if (action === 'stop') finishStop = resolve;
      else throw new Error('unexpected action ' + action);
    });
  };
  const enabling = c.computerUseAction('enable');
  const stopButton = () => panel.innerHTML.match(/<button[^>]*onclick="computerUseAction\('stop'\)"[^>]*>/)[0];
  assert.doesNotMatch(stopButton(), /disabled/);
  const stopping = c.computerUseAction('stop');
  assert.deepEqual(interruptionCalls, ['enable', 'stop']);
  assert.match(stopButton(), /disabled/);
  await c.computerUseAction('stop');
  await c.computerUseAction('enable');
  await c.computerUseAction('disable');
  await c.loadComputerUse();
  assert.deepEqual(interruptionCalls, ['enable', 'stop']);
  finishStop({ok:true, json:async () => ({...installed, enabled:true, running:false})});
  await stopping;
  assert.equal(panel.attrs['aria-busy'], 'false');
  assert.match(panel.innerHTML, />Stopped</);
  const stoppedView = panel.innerHTML;
  finishEnable({ok:true, json:async () => ({...installed, enabled:true, running:true})});
  await enabling;
  assert.equal(panel.innerHTML, stoppedView);
  assert.doesNotMatch(panel.innerHTML, />Running</);
})().catch(err => {console.error(err); process.exitCode = 1;});
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Computer Use browser behavior: %v\n%s", err, output)
	}
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `onclick="showSettingsTab('computer-use', this)"`) ||
		!strings.Contains(string(chat), "case 'computer-use': content.innerHTML = renderComputerUseTab(); loadComputerUse(); break;") {
		t.Fatal("Computer Use settings navigation is not wired to the read-only status view")
	}
}

func TestComputerUseNativePermissionRequestAndStatus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	const harness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const start = source.indexOf('// Computer Use settings only read status on entry.');
assert(start >= 0);
const parent = {};
const win = {parent};
const calls = [];
const nativeCalls = [];
const panel = {innerHTML: '', setAttribute() {}};
let accessibility = 'notGranted';
const status = {installed:true, enabled:false, running:false, source:'official', version:'1.2.3',
  description:{platform:'darwin', permissions:{accessibility:'runtime', screenRecording:'runtime'}}};
const c = {
  window: win,
  document: {getElementById: id => id === 'computerUsePanel' ? panel : null},
  uiText: (en, cn) => cn,
  escHtml: value => String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;'),
  fetch: async (url, options) => {calls.push({url, options}); return {ok:true, json:async () => status};},
  requestNative: async (action, payload, timeout) => {
    nativeCalls.push({action, payload, timeout});
    if (action === 'get-computer-use-permission-status') {
      return {accessibility, screenRecording:'notGranted'};
    }
    if (action === 'request-computer-use-permission') {
      assert.equal(payload.kind, 'accessibility');
      accessibility = 'granted';
      return {kind:'accessibility', granted:true, settingsOpened:false};
    }
    throw new Error('unexpected native action');
  }
};
vm.createContext(c);
vm.runInContext(source.slice(start), c);
(async () => {
  await c.loadComputerUse();
  assert.match(panel.innerHTML, /未授权/);
  assert.match(panel.innerHTML, /permissions-accessibility/);
  assert.doesNotMatch(panel.innerHTML, />runtime</);
  assert.deepEqual(nativeCalls.map(call => call.action), ['get-computer-use-permission-status']);
  assert.equal(calls.length, 1);
  await c.computerUseAction('permissions-accessibility');
  assert.deepEqual(nativeCalls.map(call => call.action), [
    'get-computer-use-permission-status',
    'request-computer-use-permission',
    'get-computer-use-permission-status'
  ]);
  assert.equal(nativeCalls[1].timeout, 120000);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].options.method, undefined);
  assert.doesNotMatch(panel.innerHTML, /permissions-accessibility/);
  assert.match(panel.innerHTML, /已授权/);

  // A Desktop grant does not erase an independent component denial.
  status.description.permissions.accessibility = 'notGranted';
  await c.loadComputerUse();
  assert.match(panel.innerHTML, /桌面应用 · 已授权/);
  assert.match(panel.innerHTML, /组件预检 · 未授权/);
  assert.match(panel.innerHTML, /helper-permissions-accessibility/);
  await c.computerUseAction('helper-permissions-accessibility');
  assert.equal(calls.at(-1).options.body, '{"action":"permissions-accessibility"}');

  // A failed native preflight must not leave a stale "granted" label.
  c.requestNative = async () => { throw new Error('native bridge unavailable'); };
  await c.loadComputerUse();
  assert.doesNotMatch(panel.innerHTML, /桌面应用 · 已授权/);
  assert.match(panel.innerHTML, /桌面应用 · 未知/);
  assert.match(panel.innerHTML, /helper-permissions-accessibility/);

  c.requestNative = async () => ({accessibility:'unavailable', screenRecording:'notGranted'});
  await c.loadComputerUse();
  assert.match(panel.innerHTML, /桌面应用 · 不可用/);
  assert.doesNotMatch(panel.innerHTML, /onclick="computerUseAction\('permissions-accessibility'\)/);
  assert.match(panel.innerHTML, /helper-permissions-accessibility/);

  status.description.platform = 'linux';
  await c.loadComputerUse();
  assert.doesNotMatch(panel.innerHTML, /onclick="computerUseAction\('permissions-accessibility'\)/);
  assert.doesNotMatch(panel.innerHTML, /helper-permissions-accessibility/);
})().catch(err => {console.error(err); process.exitCode = 1;});
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Computer Use native permission behavior: %v\n%s", err, output)
	}
}
