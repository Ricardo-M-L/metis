package agent

// loop_halt_test.go covers the haltTurn field semantics. The full
// "halt actually stops the next iteration" path is integration-tested
// via internal/runtime e2e tests; here we just pin the state machine
// so a future refactor can't drop the signal silently.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestHaltTurn_RaisesSignal(t *testing.T) {
	l := NewLoop(&captureProvider{}, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	if l.haltRequested {
		t.Fatal("fresh loop should have no halt request")
	}
	l.haltTurn("forbidden path")
	if !l.haltRequested {
		t.Error("haltTurn didn't flip haltRequested")
	}
	if l.haltReason != "forbidden path" {
		t.Errorf("haltReason = %q, want %q", l.haltReason, "forbidden path")
	}
}

// TestHaltTurn_FirstReasonWins — multiple PreToolUse hooks in a single
// batch could each request halt; the first reason should be the one
// surfaced to the user. Subsequent halt() calls must NOT clobber it.
func TestHaltTurn_FirstReasonWins(t *testing.T) {
	l := NewLoop(&captureProvider{}, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	l.haltTurn("first reason")
	l.haltTurn("second reason")
	if l.haltReason != "first reason" {
		t.Errorf("first reason should win; got %q", l.haltReason)
	}
}

// TestHaltTurn_BlankReasonDoesNotOverwrite — a halt() with empty
// reason after a halt() with a real reason must not erase the real
// one. Otherwise a later "no-detail halt" would silently win.
func TestHaltTurn_BlankReasonDoesNotOverwrite(t *testing.T) {
	l := NewLoop(&captureProvider{}, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	l.haltTurn("real reason")
	l.haltTurn("")
	if l.haltReason != "real reason" {
		t.Errorf("blank halt() must not overwrite; got %q", l.haltReason)
	}
}
