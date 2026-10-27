package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestDraftTurnNavigationKeepsResponseInOriginatingView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	content, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(string(content))
	if err != nil {
		t.Fatal(err)
	}
	script := draftCreationBrowserHarness + `
(async () => {
  // Allocation establishes A before its turn starts. Navigating afterward
  // still cannot render A's delayed failure into B.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    await f.started();
    assert.equal(f.c.currentSessionId, 'A');
    f.c.currentSessionId = 'B';
    f.c.resumeSessionGeneration++;
    f.request.resolve(failed({ error: 'A failed' }));
    assert.equal(await turn, false);
    assert.deepEqual(f.errors, [], 'draft A failure was rendered as an error in B');
    assert.deepEqual(f.finishes, [], 'draft A finished B\'s visible turn');
    assert.equal(f.c.currentSessionId, 'B');
    assert(f.toasts.some(item => item.message.includes('A failed')), 'background failure must remain visible as a toast');
  }
  // Navigating through B and back creates a different blank composer. The
  // already allocated origin A is never allowed to claim that later view.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    await f.started();
    const originalNavigation = f.navigation.slice();
    f.c.currentSessionId = 'B';
    f.c.resumeSessionGeneration++;
    f.c.currentSessionId = null;
    f.c.resumeSessionGeneration++;
    f.request.resolve(ok({ sessionId: 'A', text: 'A answered' }));
    assert.equal(await turn, true);
    assert.equal(f.c.currentSessionId, null, 'old response claimed a new blank composer');
    assert(!f.messages.some(item => item.role === 'assistant'), 'old response inserted text into a new blank composer');
    assert.deepEqual(f.finishes, [], 'old response finished the new composer');
    assert.deepEqual(f.navigation, originalNavigation, 'old response replaced the current navigation entry');
  }
  // Without navigation, errors stay attached to the draft that started them.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    await f.started();
    f.request.resolve(failed({ error: 'A failed' }));
    assert.equal(await turn, false);
    assert.equal(f.errors.length, 1);
    assert.equal(f.errors[0].message, 'A failed');
    assert.equal(f.errors[0].view, 'A');
    assert.equal(f.finishes.length, 1);
    assert.equal(f.finishes[0].outcome, 'error');
  }
  // A successful response renders in its unchanged, preallocated origin.
  {
    const f = fixture();
    const turn = f.c.runTurnItem({ text: 'draft A' });
    await f.started();
    f.request.resolve(ok({ sessionId: 'A', text: 'A answered' }));
    assert.equal(await turn, true);
    assert.equal(f.c.currentSessionId, 'A');
    assert(f.messages.some(item => item.view === 'A' && item.role === 'assistant' && item.text === 'A answered'));
    assert.equal(f.finishes.length, 1);
    assert.equal(f.finishes[0].outcome, 'completed');
    assert.equal(f.navigation.length, 1, 'allocation records A once; completion must not add a duplicate navigation entry');
    assert.equal(f.navigation[0].id, 'A');
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("draft turn navigation UI checks failed: %v\n%s", err, output)
	}
}
