package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func newTraceTestStore(t *testing.T) *session.TraceStore {
	t.Helper()
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// Regression: with eventId the tool must return the requested event as
// the detailed record. The old implementation sorted all results
// newest-first, so events[0] became the newest CHILD and the requested
// parent could even be truncated away by limit.
func TestSessionEventReadEventIDReturnsRequestedEvent(t *testing.T) {
	store := newTraceTestStore(t)
	parent := &session.TraceEvent{SessionID: "s1", Kind: "tool_start", ToolName: "Bash", ToolUseID: "t1", Text: "go build"}
	if err := store.Append(parent); err != nil {
		t.Fatal(err)
	}
	child := &session.TraceEvent{SessionID: "s1", Kind: "tool_result", ToolName: "Bash", ToolUseID: "t1", ParentID: "t1", Text: "ok"}
	if err := store.Append(child); err != nil {
		t.Fatal(err)
	}

	old := CurrentTraceStore()
	SetTraceStore(store)
	defer SetTraceStore(old)

	tool := SessionEventRead{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"sessionId": "s1",
		"eventId":   parent.ID,
		"limit":     float64(1), // even a tiny limit must not hide the parent
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, parent.ID) {
		t.Fatalf("detail view must contain the requested event ID %q:\n%s", parent.ID, res.Output)
	}
	if !strings.Contains(res.Output, `"kind": "tool_start"`) {
		t.Fatalf("detail record should be the requested tool_start event:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "child:") {
		t.Fatalf("detail view should list children:\n%s", res.Output)
	}
}

func TestSessionEventSearchUnavailableWithoutStore(t *testing.T) {
	old := CurrentTraceStore()
	SetTraceStore(nil)
	defer SetTraceStore(old)

	tool := SessionEventSearch{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{"query": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "not enabled") {
		t.Fatalf("clean unavailability expected, got: %s", res.Output)
	}
}

func TestSessionTraceRendersTree(t *testing.T) {
	store := newTraceTestStore(t)
	store.Append(&session.TraceEvent{SessionID: "s1", Kind: "text", Text: "starting"})
	store.Append(&session.TraceEvent{SessionID: "s1", Kind: "tool_start", ToolName: "Bash", ToolUseID: "t1"})
	store.Append(&session.TraceEvent{SessionID: "s1", Kind: "tool_result", ToolName: "Bash", ToolUseID: "t1", ParentID: "t1", Text: "done", IsError: true})

	old := CurrentTraceStore()
	SetTraceStore(store)
	defer SetTraceStore(old)

	tool := SessionTrace{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{"sessionId": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "tool_start:Bash") || !strings.Contains(res.Output, "tool_result:Bash") {
		t.Fatalf("tree should render tool nodes:\n%s", res.Output)
	}
	// The nested result must be indented deeper than its tool_start.
	startIdx := strings.Index(res.Output, "tool_start:Bash")
	resultIdx := strings.Index(res.Output, "tool_result:Bash")
	indentStart := len(res.Output[startIdx:]) - len(strings.TrimLeft(res.Output[startIdx:], " "))
	_ = indentStart
	if resultIdx < startIdx {
		t.Fatalf("result should follow its tool_start:\n%s", res.Output)
	}
	resultLine := res.Output[:resultIdx]
	lastNL := strings.LastIndexByte(resultLine, '\n')
	resultIndent := resultIdx - lastNL - 1
	startLine := res.Output[:startIdx]
	lastNL2 := strings.LastIndexByte(startLine, '\n')
	startIndent := startIdx - lastNL2 - 1
	if resultIndent <= startIndent {
		t.Fatalf("tool_result must nest deeper (start=%d result=%d):\n%s", startIndent, resultIndent, res.Output)
	}
}
