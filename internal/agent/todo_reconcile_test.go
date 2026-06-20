package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// countingTextProvider returns a text-only (no tool_use) response every
// call and counts how many times the loop asked it — so we can see whether
// the end-of-turn reconciliation triggered an extra iteration.
type countingTextProvider struct{ calls int }

func (p *countingTextProvider) Name() string          { return "count-stub" }
func (p *countingTextProvider) ModelID() string       { return "" }
func (p *countingTextProvider) MaxContextTokens() int { return 200_000 }
func (p *countingTextProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{StopReason: "end_turn"}, nil
}
func (p *countingTextProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	return &textOnlyStream{}, nil
}

type textOnlyStream struct{ step int }

func (s *textOnlyStream) Close() error { return nil }
func (s *textOnlyStream) Recv() (llm.StreamEvent, error) {
	s.step++
	switch s.step {
	case 1:
		return llm.StreamEvent{Type: "message_start", InputTokens: 1}, nil
	case 2:
		return llm.StreamEvent{Type: "text_delta", TextDelta: "done"}, nil
	case 3:
		return llm.StreamEvent{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1}, nil
	case 4:
		return llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
	}
	return llm.StreamEvent{}, io.EOF
}

func loopMessagesContain(l *Loop, substr string) bool {
	for _, m := range l.Messages {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, substr) {
				return true
			}
		}
	}
	return false
}

// With an open todo, a turn that ends (no tool calls) must run ONE more
// iteration carrying a reminder so the model can mark the finished item
// completed — otherwise the bottom task strip shows a stale in_progress row.
func TestLoopRun_ReconcilesOpenTodosAtTurnEnd(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "reconcile-open"
	tasks.SetCurrentSessionID(sid)
	if _, err := tasks.ReplaceAll(sid, []tasks.Item{
		{Status: "completed", Content: "did A"},
		{Status: "in_progress", Content: "summarize and suggest PR"},
	}); err != nil {
		t.Fatal(err)
	}

	p := &countingTextProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("go")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if p.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (1 normal + 1 reconcile)", p.calls)
	}
	if !loopMessagesContain(loop, "open items") {
		t.Errorf("end-of-turn todo reminder was not injected into the conversation")
	}
}

// When every todo is already completed, no reconciliation iteration runs.
func TestLoopRun_NoReconcileWhenAllTodosDone(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "reconcile-done"
	tasks.SetCurrentSessionID(sid)
	if _, err := tasks.ReplaceAll(sid, []tasks.Item{
		{Status: "completed", Content: "did A"},
	}); err != nil {
		t.Fatal(err)
	}

	p := &countingTextProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("go")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no reconcile when all done)", p.calls)
	}
}

// The reconciliation fires at most once per turn — a model that keeps
// stopping without marking the todo must NOT loop forever.
func TestLoopRun_ReconcileIsOncePerTurn(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "reconcile-once"
	tasks.SetCurrentSessionID(sid)
	if _, err := tasks.ReplaceAll(sid, []tasks.Item{
		{Status: "in_progress", Content: "still open"},
	}); err != nil {
		t.Fatal(err)
	}

	p := &countingTextProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("go")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	// 1 normal + exactly 1 reconcile, then it gives up — never the 5-iter cap.
	if p.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (reconcile is once-per-turn, no infinite loop)", p.calls)
	}
}
