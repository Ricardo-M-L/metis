package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestDesktopParallelismSettingsRenderAndSave(t *testing.T) {
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
const start = source.indexOf('function renderDesktopParallelismPreference()');
const end = source.indexOf('function renderBusyEnterPreference()', start);
assert(start >= 0 && end > start);
const saves = [], toasts = [];
let renders = 0;
const c = {
  desktopPreferences: { rootTurnParallelism: 8, totalAgentParallelism: 16, subagentParallelism: 8 },
  document: { documentElement: { lang: 'en' } },
  uiText: (en, zh) => c.document.documentElement.lang === 'zh-CN' ? zh : en,
  escAttr: s => s,
  saveDesktopPreference: async (key, value) => { saves.push([key, value]); return { parallelismApplied: false }; },
  renderSettingsTab: () => renders++,
  showToast: s => toasts.push(s),
};
vm.createContext(c);
vm.runInContext(source.slice(start, end), c);
const english = c.renderDesktopParallelismPreference();
for (const key of ['rootTurnParallelism', 'totalAgentParallelism', 'subagentParallelism']) {
  assert(english.includes("saveDesktopParallelism(this, '" + key + "')"), key);
}
assert.match(english, /Total execution slots/);
assert.match(english, /shared execution pool/);
assert.match(english, /Waiting on background Bash releases the agent slot/);
assert.match(english, /Child execution slots per top-level turn/);
assert.match(english, /max="64"/);
assert.match(english, /max="32"/);
c.document.documentElement.lang = 'zh-CN';
const chinese = c.renderDesktopParallelismPreference();
assert.match(chinese, /总执行槽/);
assert.match(chinese, /代理等待后台 Bash 时会让出执行槽/);
assert.match(chinese, /每个顶层任务的子代理执行槽/);
assert.doesNotMatch(chinese, /运行中代理总数/);
assert.match(chinese, /排队任务/);
(async () => {
  await c.saveDesktopParallelism({value: '24'}, 'totalAgentParallelism');
  assert.deepEqual(saves, [['totalAgentParallelism', 24]]);
  assert.match(toasts[0], /重启 Desktop/);
  await c.saveDesktopParallelism({value: '33'}, 'subagentParallelism');
  assert.equal(saves.length, 1);
  assert.match(toasts[1], /1 到 32/);
  assert.equal(renders, 2);
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("parallelism settings UI: %v\n%s", err, output)
	}
}
