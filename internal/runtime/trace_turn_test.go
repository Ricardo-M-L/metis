package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestRunWithTraceTurnKeepsDeferredCleanupBoundUntilRunReturns(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldAdapter := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })

	const sessionID = "root-run-helper"
	adapter.SetSession(sessionID)
	RecordUserMessage(sessionID, "first")
	wantErr := errors.New("provider failed")
	err = RunWithTraceTurn(context.Background(), sessionID, func(turnCtx context.Context) error {
		invocationID := agent.TraceInvocationIDFromContext(turnCtx)
		if invocationID == "" {
			t.Fatal("run context has no trace invocation ID")
		}
		defer adapter.OnEvent(agent.Event{
			Kind:              agent.EventInfo,
			Info:              "deferred cleanup",
			TraceInvocationID: invocationID,
		})
		adapter.OnEvent(agent.Event{
			Kind:              agent.EventError,
			Err:               wantErr,
			TraceInvocationID: invocationID,
		})
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunWithTraceTurn error = %v, want %v", err, wantErr)
	}

	adapter.mu.Lock()
	originCount := len(adapter.invocationOrigin)
	_, turnActive := adapter.activeTurns[sessionID]
	adapter.mu.Unlock()
	if originCount != 0 {
		t.Fatalf("%d root invocation origins still live after run returned", originCount)
	}
	if turnActive {
		t.Fatal("active turn still live after run returned")
	}

	RecordUserMessage(sessionID, "second")
	events := store.Events(sessionID)
	if len(events) != 4 {
		t.Fatalf("events = %+v, want user/error/deferred-info/user", events)
	}
	if events[0].Turn != 1 || events[1].Turn != 1 || events[2].Turn != 1 || events[3].Turn != 2 {
		t.Fatalf("turn attribution = [%d %d %d %d], want [1 1 1 2]", events[0].Turn, events[1].Turn, events[2].Turn, events[3].Turn)
	}
	if events[2].Text != "deferred cleanup" {
		t.Fatalf("deferred event = %+v", events[2])
	}
}

func TestRunWithTraceTurnWithoutBindingDoesNotEndParentContext(t *testing.T) {
	oldAdapter := CurrentTraceAdapter()
	SetTraceAdapter(nil)
	t.Cleanup(func() { SetTraceAdapter(oldAdapter) })

	parentCtx := agent.WithTraceInvocationID(context.Background(), "parent-invocation")
	called := false
	if err := RunWithTraceTurn(parentCtx, "unbound", func(runCtx context.Context) error {
		called = true
		if got := agent.TraceInvocationIDFromContext(runCtx); got != "parent-invocation" {
			t.Fatalf("invocation ID = %q, want unchanged parent context", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("run callback was not called")
	}
}
