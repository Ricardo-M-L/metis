package agent

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// mkToolUse / mkToolResult / mkText keep test bodies small.
func mkToolUse(id, name string) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_use", ToolUseID: id, ToolName: name}
}
func mkToolResult(id, body string, isErr bool) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_result", ToolUseID: id, ToolResult: body, IsError: isErr}
}
func mkText(s string) llm.ContentBlock { return llm.ContentBlock{Type: "text", Text: s} }

func TestRepairOrphanedToolUses_NoOpOnPairedHistory(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkText("hi")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("id-A", "Read")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkToolResult("id-A", "file content", false)}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkText("done")}},
	}
	out := RepairOrphanedToolUses(in)
	if len(out) != len(in) {
		t.Fatalf("paired history should not grow: in=%d out=%d", len(in), len(out))
	}
}

// The session-8cfc076b scenario: assistant tool_use followed by user text
// without a tool_result in between.
func TestRepairOrphanedToolUses_HealsSessionOrphan(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkText("plan something")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			mkText("calling agent..."),
			mkToolUse("id-Agent", "Agent"),
		}},
		// Bug: next user message is plain text, not tool_result.
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkText("did you stop?")}},
	}
	out := RepairOrphanedToolUses(in)
	if len(out) != len(in)+1 {
		t.Fatalf("expected one synthetic user message appended; in=%d out=%d", len(in), len(out))
	}
	last := out[len(out)-1]
	if last.Role != llm.RoleUser {
		t.Errorf("synthetic message should be user role; got %v", last.Role)
	}
	if len(last.Content) != 1 {
		t.Fatalf("synthetic message should carry one tool_result; got %d blocks", len(last.Content))
	}
	tr := last.Content[0]
	if tr.Type != "tool_result" {
		t.Errorf("synthetic block type=%q want tool_result", tr.Type)
	}
	if tr.ToolUseID != "id-Agent" {
		t.Errorf("synthetic block tool_use_id=%q want id-Agent", tr.ToolUseID)
	}
	if !tr.IsError {
		t.Error("synthetic block must be IsError=true so the model knows the tool didn't actually run")
	}
}

func TestRepairOrphanedToolUses_MultipleOrphansInOneBatch(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			mkToolUse("id-1", "Read"),
			mkToolUse("id-2", "Read"),
			mkToolUse("id-3", "Read"),
		}},
		// Only id-2 got a result — id-1 and id-3 are orphans.
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			mkToolResult("id-2", "ok", false),
		}},
	}
	out := RepairOrphanedToolUses(in)
	if len(out) != len(in)+1 {
		t.Fatalf("expected one synthetic user message; in=%d out=%d", len(in), len(out))
	}
	stub := out[len(out)-1].Content
	if len(stub) != 2 {
		t.Fatalf("expected 2 stub tool_results (id-1 and id-3); got %d", len(stub))
	}
	got := map[string]bool{}
	for _, b := range stub {
		got[b.ToolUseID] = true
	}
	if !got["id-1"] || !got["id-3"] {
		t.Errorf("missing stubs: got=%v", got)
	}
	if got["id-2"] {
		t.Error("id-2 was satisfied; should not appear in stubs")
	}
}

func TestRepairOrphanedToolUses_Idempotent(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("id-X", "Bash")}},
	}
	once := RepairOrphanedToolUses(in)
	twice := RepairOrphanedToolUses(once)
	if len(twice) != len(once) {
		t.Errorf("repair should be idempotent; once=%d twice=%d", len(once), len(twice))
	}
}

func TestRepairOrphanedToolUses_EmptyAndNil(t *testing.T) {
	t.Parallel()
	if got := RepairOrphanedToolUses(nil); got != nil {
		t.Errorf("nil in → nil out; got %v", got)
	}
	if got := RepairOrphanedToolUses([]llm.Message{}); len(got) != 0 {
		t.Errorf("empty in → empty out; got %d entries", len(got))
	}
}

// Stale tool_result from a prior turn must not satisfy a later orphan
// with the same id. (Defensive — same-id collision is unlikely in
// practice but the comparison must respect chronology.)
//
// We model "stale result" by placing the result BEFORE the tool_use.
// Current implementation does a global satisfied-set sweep so this
// case is intentionally tolerant; verify the documented behavior.
func TestRepairOrphanedToolUses_StaleResultBeforeUse_Tolerated(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkToolResult("id-A", "stale", false)}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("id-A", "Read")}},
	}
	out := RepairOrphanedToolUses(in)
	// Current contract: any matching id satisfies. If a future
	// implementation tightens this to require ordering, update the
	// test to match.
	if len(out) != len(in) {
		t.Errorf("global-satisfied-set semantics expected; got len=%d", len(out))
	}
}

func TestLoopRepairOrphansInPlace_LocksAndMutates(t *testing.T) {
	t.Parallel()
	l := &Loop{}
	l.Messages = []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("id-orphan", "Bash")}},
	}
	l.repairOrphansInPlace()
	if len(l.Messages) != 2 {
		t.Fatalf("expected loop.Messages to grow to 2; got %d", len(l.Messages))
	}
	if l.Messages[1].Role != llm.RoleUser {
		t.Errorf("appended message should be user role")
	}
}

func TestLoopRestore_RepairsOrphansOnIncomingHistory(t *testing.T) {
	t.Parallel()
	l := &Loop{}
	// A buggy old session: orphaned Agent tool_use.
	incoming := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkText("hi")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("agt-1", "Agent")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkText("did you stop?")}},
	}
	l.Restore(incoming)
	if len(l.Messages) != len(incoming)+1 {
		t.Fatalf("Restore should heal orphans; in=%d out=%d", len(incoming), len(l.Messages))
	}
	stub := l.Messages[len(l.Messages)-1]
	if stub.Role != llm.RoleUser || len(stub.Content) != 1 || stub.Content[0].Type != "tool_result" {
		t.Errorf("appended stub doesn't look like a tool_result message: %+v", stub)
	}
	if stub.Content[0].ToolUseID != "agt-1" {
		t.Errorf("stub tool_use_id=%q want agt-1", stub.Content[0].ToolUseID)
	}
}
