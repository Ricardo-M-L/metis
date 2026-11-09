package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestScheduledWakeupHistoryHydratesAsInternalInfo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	const harness = `
const assert = require('node:assert/strict');
const source = require('node:fs').readFileSync(0, 'utf8');
const start = source.indexOf('function appendHistoryEntries(');
const end = source.indexOf('\n// The viewport owns historical DOM', start);
assert.ok(start >= 0 && end > start);
const rows = [];
let activityHistoryTurn = 0;
const visibleTranscriptText = text => String(text);
const flushHistoryMessageBatch = () => {};
const finishActivityGroup = () => {};
const addMessage = (role,text) => rows.push({role,text});
const pendingToolRows = () => [];
const handleToolResult = () => {};
const toolDetails = {};
const turnStart = source.indexOf('function todoHistoryStartsUserTurn(');
const turnEnd = source.indexOf('function restoreTodoPlanFromHistory(', turnStart);
assert.ok(turnStart >= 0 && turnEnd > turnStart);
eval(source.slice(turnStart,turnEnd));
eval(source.slice(start,end));
const turn = appendHistoryEntries([
 {role:'user',content:[{type:'text',text:'human'}]},
 {role:'user',content:[{type:'text',text:'wakeup',synthetic:true}]},
 {role:'user',content:[{type:'tool_result',tool_use_id:'tool',content:'done'},{type:'text',text:'mixed human'},{type:'text',text:'mixed internal',synthetic:true}]}
], 'original');
assert.equal(turn,2,'synthetic wakeup must not increment human turns');
assert.deepEqual(rows,[{role:'user',text:'human'},{role:'info',text:'wakeup'},{role:'user',text:'mixed human'},{role:'info',text:'mixed internal'}]);
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hydration: %v\n%s", err, output)
	}
}
