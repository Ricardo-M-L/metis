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
// with the same id. (Defensive — providers should issue unique ids,
// but transcript repair must still respect chronology when they do not.)
func TestRepairOrphanedToolUses_StaleResultBeforeUseDoesNotSatisfy(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkToolResult("id-A", "stale", false)}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("id-A", "Read")}},
	}
	out := RepairOrphanedToolUses(in)
	if len(out) != len(in)+1 {
		t.Fatalf("stale result must not satisfy a later use: in=%d out=%d", len(in), len(out))
	}
	stub := out[len(out)-1].Content
	if len(stub) != 1 || stub[0].ToolUseID != "id-A" {
		t.Fatalf("expected one id-A repair result; got %+v", stub)
	}
}

func TestRepairOrphanedToolUses_ReusedIDNeedsNewResult(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("provider-reused-id", "Read")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkToolResult("provider-reused-id", "first result", false)}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{mkToolUse("provider-reused-id", "Read")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{mkText("continue")}},
	}

	out := RepairOrphanedToolUses(in)
	if len(out) != len(in)+1 {
		t.Fatalf("the consumed first-turn result must not satisfy the reused id: in=%d out=%d", len(in), len(out))
	}
	stub := out[len(out)-1].Content
	if len(stub) != 1 || stub[0].ToolUseID != "provider-reused-id" {
		t.Fatalf("expected one repair result for the second use; got %+v", stub)
	}
	if twice := RepairOrphanedToolUses(out); len(twice) != len(out) {
		t.Fatalf("repair of a reused id must remain idempotent: once=%d twice=%d", len(out), len(twice))
	}
}

func TestRepairOrphanedToolUses_RepairsEveryDuplicateUse(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			mkToolUse("provider-duplicate-id", "Read"),
			mkToolUse("provider-duplicate-id", "Read"),
		}},
	}

	out := RepairOrphanedToolUses(in)
	if len(out) != len(in)+1 {
		t.Fatalf("expected one synthetic user message; in=%d out=%d", len(in), len(out))
	}
	stub := out[len(out)-1].Content
	if len(stub) != 2 {
		t.Fatalf("each unmatched use needs its own result; got %d blocks", len(stub))
	}
	for i, block := range stub {
		if block.ToolUseID != "provider-duplicate-id" {
			t.Errorf("stub[%d].tool_use_id=%q want provider-duplicate-id", i, block.ToolUseID)
		}
	}
	if twice := RepairOrphanedToolUses(out); len(twice) != len(out) {
		t.Fatalf("duplicate-id repair must remain idempotent: once=%d twice=%d", len(out), len(twice))
	}
}

func TestRepairOrphanedToolUses_ResultIsConsumedOnce(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			mkToolUse("provider-duplicate-id", "Read"),
			mkToolUse("provider-duplicate-id", "Read"),
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			mkToolResult("provider-duplicate-id", "one result", false),
		}},
	}

	out := RepairOrphanedToolUses(in)
	if len(out) != len(in)+1 {
		t.Fatalf("one result must satisfy only one use: in=%d out=%d", len(in), len(out))
	}
	stub := out[len(out)-1].Content
	if len(stub) != 1 || stub[0].ToolUseID != "provider-duplicate-id" {
		t.Fatalf("expected one remaining duplicate-id repair result; got %+v", stub)
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
