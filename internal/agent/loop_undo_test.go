package agent

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestLoop_UndoLastTurn covers the integration boundary: agent.Loop holds
// the canonical history under l.mu, and /undo must roll it back atomically.
// The pure transcript-pkg tests already cover the slicing logic; here we
// verify Loop.UndoLastTurn drops the right chunk + handles the empty case.

func newLoopForUndoTest() *Loop {
	return NewLoop(&captureProvider{}, tools.NewRegistry(),
		permission.New(permission.ModeAuto), nil, "sys", 5)
}

// TestLoop_RestoreReplacesHistory locks in the cron scheduler's
// per-mode history swap. Reset clears, Restore loads — no other path
// to setting Messages after construction.
func TestLoop_RestoreReplacesHistory(t *testing.T) {
	l := newLoopForUndoTest()
	l.AppendUser("a")
	l.AppendUser("b")
	if len(l.Messages) != 2 {
		t.Fatalf("precondition messages=%d, want 2", len(l.Messages))
	}
	saved := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "older"}}},
	}
	l.Restore(saved)
	if len(l.Messages) != 1 || l.Messages[0].Content[0].Text != "older" {
		t.Errorf("Restore didn't replace history; got %+v", l.Messages)
	}
	// Restore(nil) must clear (matches isolated-mode reset semantics).
	l.Restore(nil)
	if l.Messages != nil {
		t.Errorf("Restore(nil) should clear; got %+v", l.Messages)
	}
}

func TestLoop_UndoLastTurn_DropsLastUserAssistantPair(t *testing.T) {
	l := newLoopForUndoTest()
	l.AppendUser("first")
	l.Messages = append(l.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "first reply"}},
	})
	l.AppendUser("second")
	l.Messages = append(l.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "second reply"}},
	})

	if !l.UndoLastTurn() {
		t.Fatal("UndoLastTurn returned false; expected to find a turn to undo")
	}
	hist := l.History()
	if len(hist) != 2 {
		t.Fatalf("history len after Undo = %d, want 2", len(hist))
	}
	if hist[0].Content[0].Text != "first" || hist[1].Content[0].Text != "first reply" {
		t.Errorf("after Undo, first turn should remain intact; got %+v", hist)
	}
}

func TestLoop_UndoLastTurn_EmptyHistoryReturnsFalse(t *testing.T) {
	l := newLoopForUndoTest()
	if l.UndoLastTurn() {
		t.Error("UndoLastTurn on empty history should return false")
	}
}

func TestLoop_UndoLastTurn_PreservesIterIdx(t *testing.T) {
	// Counter state is intentionally NOT rewound — hooks and detectors
	// should observe a monotonic iteration timeline.
	l := newLoopForUndoTest()
	l.AppendUser("first")
	l.iterIdx = 7
	_ = l.UndoLastTurn()
	if got := l.IterIdx(); got != 7 {
		t.Errorf("IterIdx after Undo = %d, want 7 (preserved)", got)
	}
}

func TestLoop_CountTurns_ExposesTranscriptHelper(t *testing.T) {
	l := newLoopForUndoTest()
	if got := l.CountTurns(); got != 0 {
		t.Errorf("empty loop CountTurns = %d, want 0", got)
	}
	l.AppendUser("hi")
	l.AppendUser("again")
	if got := l.CountTurns(); got != 2 {
		t.Errorf("CountTurns = %d, want 2", got)
	}
}
