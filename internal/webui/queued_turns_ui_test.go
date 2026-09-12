package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestQueuedTurnCardAssetsMatchInteractiveContract(t *testing.T) {
	chatBytes, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	chat := string(chatBytes)
	for _, want := range []string{
		`class="queued-card`,
		`uiText('Steer now', '调整方向')`,
		`uiText('Edit message', '编辑消息')`,
		`uiText('Open in side chat', '在侧边聊天中打开')`,
		`uiText('Close queue', '关闭排队')`,
		`fetch('/api/steer'`,
		`fetch('/api/fork'`,
		`function replaceComposerWithQueuedTurn(`,
	} {
		if !strings.Contains(chat, want) {
			t.Errorf("chat.js missing queued-card contract %q", want)
		}
	}
	if strings.Contains(chat, `class="queued-count"`) || strings.Contains(chat, `class="queued-item"`) {
		t.Error("chat.js still renders the old Queued-count pill UI")
	}

	styleBytes, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	style := string(styleBytes)
	for _, want := range []string{
		".queued-card {",
		"grid-template-columns: 24px minmax(0, 1fr) auto 38px 38px;",
		".queued-menu {",
		".queued-menu.opens-up {",
		"border-radius: 24px;",
		"@media (max-width: 620px)",
		"grid-template-columns: 22px minmax(0, 1fr) 38px 38px 38px;",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing queued-card style %q", want)
		}
	}

	for _, name := range []string{"list-start", "list-end", "corner-down-left", "trash-2", "ellipsis", "pen-line", "message-circle-plus"} {
		asset, err := staticFS.ReadFile("static/icons/lucide/" + name + ".svg")
		if err != nil {
			t.Errorf("read %s icon: %v", name, err)
			continue
		}
		if !bytes.Contains(asset, []byte(`<svg xmlns="http://www.w3.org/2000/svg"`)) {
			t.Errorf("%s is not a standalone SVG icon asset", name)
		}
	}
}

func TestQueuedTurnCardBrowserInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const from = source.indexOf('function queuedTurnIcon(');
const to = source.indexOf('async function drainQueuedTurns(', from);
assert(from >= 0 && to > from);
const wrap = {style:{}, innerHTML:'', attrs:{}, setAttribute(k,v){this.attrs[k]=v;}};
const input = {value:'', focused:false, focus(){this.focused=true;}, setSelectionRange(){}};
let request = null, resumed = null, renderedAttachments = 0, messages = 0;
const c = {
  queuedTurns: [{text:'就是要再具体点', images:[]}], queuedSessionId:'S', queuedTurnMenuIndex:-1,
  queuedTurnPendingItem:null, turnRunning:true, runningSessionId:'S', currentSessionId:'S', attachments:[],
  document:{getElementById:id => id === 'queuedTurns' ? wrap : id === 'inputField' ? input : null,
    querySelector(){return null;}, addEventListener(){}},
  escHtml:String, escAttr:String, uiText:(en,zh)=>zh, renderAttachments(){renderedAttachments++;},
  onComposerInput(){}, showToast(){}, addMessage(){messages++;}, loadSessions:async()=>{},
  resumeSession:async id=>{resumed=id; c.currentSessionId=id;},
  fetch:async (url, options)=>{request={url,options}; return {ok:true,json:async()=>({sessionId:'branch'})};},
};
vm.createContext(c);
vm.runInContext(source.slice(from, to), c);

c.renderQueuedTurns();
assert.match(wrap.innerHTML, /class="queued-card/);
assert.match(wrap.innerHTML, /调整方向/);
assert.doesNotMatch(wrap.innerHTML, /Queued 1/);
c.toggleQueuedTurnMenu({stopPropagation(){}}, 0);
assert.match(wrap.innerHTML, /编辑消息/);
assert.match(wrap.innerHTML, /在侧边聊天中打开/);
assert.match(wrap.innerHTML, /关闭排队/);

// Editing swaps a non-empty composer draft into the queue instead of losing it.
input.value = '保留这个草稿';
c.editQueuedTurn({stopPropagation(){}}, 0);
assert.equal(input.value, '就是要再具体点');
assert.equal(c.queuedTurns.length, 1);
assert.equal(c.queuedTurns[0].text, '保留这个草稿');
assert.equal(input.focused, true);

(async () => {
  const steerItem = {text:'现在调整方向', images:[]};
  c.queuedTurns = [steerItem]; c.queuedSessionId = 'S'; c.currentSessionId = 'S'; c.runningSessionId = 'S';
  await c.steerQueuedTurn({stopPropagation(){}}, 0);
  assert.equal(request.url, '/api/steer');
  assert.deepEqual(JSON.parse(request.options.body), {sessionId:'S', input:'现在调整方向'});
  assert.equal(c.queuedTurns.length, 0);
  assert.equal(messages, 1);

  const sideItem = {text:'单独讨论这个', images:[]};
  c.queuedTurns = [sideItem]; c.queuedSessionId = 'S'; c.currentSessionId = 'S';
  await c.openQueuedTurnInSideChat({stopPropagation(){}}, 0);
  assert.equal(request.url, '/api/fork');
  assert.deepEqual(JSON.parse(request.options.body), {sessionId:'S', messageIndex:-1});
  assert.equal(resumed, 'branch');
  assert.equal(input.value, '单独讨论这个');
  assert.ok(renderedAttachments > 0);
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("queued turn browser harness: %v\n%s", err, out)
	}
}
