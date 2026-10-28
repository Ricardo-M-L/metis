package webui

import (
	"os"
	"os/exec"
	"testing"
)

func TestPromptSuggestionComposerBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for composer behavior tests")
	}
	source, err := os.ReadFile("static/prompt-suggestions.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source) + `
const assert = require('node:assert/strict');
(async () => {
  let session = 'a', value = '', ghost = '', busy = false, calls = 0, sent = 0;
  global.sendMessage = () => { sent++; };
  let response = async () => ({suggestion: '给这个改动补测试', enabled: true});
  const c = createPromptSuggestionController({
    session: () => session,
    eligible: () => !busy && value === '',
    request: (id, signal) => { calls++; return response(id, signal); },
    display: text => { ghost = text; },
    fill: text => { value = text; },
  });
  await c.request('a', 'turn-1');
  assert.equal(value, '');
  assert.equal(ghost, '给这个改动补测试');
  let prevented = false;
  assert.equal(c.handleKey({key:'Tab', preventDefault(){prevented=true;}}), true);
  assert.equal(prevented, true);
  assert.equal(value, '给这个改动补测试');
  assert.equal(ghost, '');
  assert.equal(sent, 0, 'Tab must never submit');
  assert.equal(c.handleKey({key:'Tab', preventDefault(){throw Error('ordinary Tab was stolen');}}), false);
  value = '';
  await c.request('a', 'turn-1');
  assert.equal(calls, 1, 'duplicate completion must not spend another request');
  await c.request('a', 'turn-2');
  assert.equal(c.handleKey({key:'Tab', shiftKey:true, preventDefault(){throw Error('Shift+Tab stolen');}}), false);
  assert.equal(value, '');
  c.handleKey({key:'Escape', preventDefault(){}});
  assert.equal(ghost, '');
  let resolve;
  response = () => new Promise(r => resolve = r);
  const pending = c.request('a', 'turn-3');
  c.clear();
  value = 'typed';
  value = '';
  resolve({suggestion:'stale'});
  await pending;
  assert.equal(ghost, '', 'typing and erasing must invalidate the old request');
  const switched = c.request('a', 'turn-4');
  session = 'b';
  resolve({suggestion:'wrong session'});
  await switched;
  assert.equal(ghost, '');
  session = 'a';
  value = 'my draft';
  const before = calls;
  await c.request('a', 'turn-5');
  assert.equal(calls, before);
  assert.equal(value, 'my draft');
  value = ''; busy = true;
  await c.request('a', 'turn-6');
  assert.equal(calls, before);
  busy = false;
  response = async () => ({suggestion:'继续检查'});
  await c.request('a', 'turn-7');
  c.handleKey({key:'Tab', isComposing:true, preventDefault(){throw Error('IME was interrupted');}});
  assert.equal(value, '');
  assert.equal(ghost, '');
  await c.request('a', 'turn-8');
  c.setEnabled(false);
  assert.equal(ghost, '');
  const disabledCalls = calls;
  await c.request('a', 'turn-9');
  assert.equal(calls, disabledCalls);
  c.setEnabled(true);
  response = async () => { throw new Error('offline'); };
  await c.request('a', 'turn-10');
  assert.equal(ghost, '');
  response = async () => ({suggestion:'继续测试'});
  await c.request('a', 'turn-11');
  c.handleKey({key:'Enter', preventDefault(){throw Error('Enter was stolen');}});
  assert.equal(value, '', 'Enter on a ghost must not accept or submit it');
  assert.equal(ghost, '');
  console.log('prompt suggestion composer behavior passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	if output, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("composer behavior failed: %v\n%s", err, output)
	}
}
