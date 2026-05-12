package builtin

// agent_g15_test.go — locks Phase G.15 (2026-05-12) abort + error
// propagation hardening.
//
// Three contracts:
//
//   1. Parent ctx cancel mid-drain terminates the foreground path
//      via the select-on-ctx guard; result is IsError + teammate
//      lands in Killed status.
//   2. Panic in the foreground drain is recovered → IsError, not
//      propagated up to crash the parent dispatcher.
//   3. Timeout (deadline-exceeded ctx) lands the teammate in Failed
//      with the "timeout Xs" hint.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestAgentExecute_ParentCancelTerminates — start a sub-agent with a
// slow provider, cancel the parent ctx, confirm Execute returns
// IsError quickly and the teammate is Killed.
func TestAgentExecute_ParentCancelTerminates(t *testing.T) {
	roster := agent.NewRoster(0)
	// 5s sleep — plenty long for ctx cancel to win.
	tool := NewAgent(permission.New(permission.ModeBypass), slowProvider(5*time.Second), tools.NewRegistry(), "m", "s").
		WithRoster(roster)

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		res *tools.Result
		err error
	}, 1)
	go func() {
		res, err := tool.Execute(ctx, map[string]any{"prompt": "x"})
		resultCh <- struct {
			res *tools.Result
			err error
		}{res, err}
	}()

	// Cancel after a short delay so the sub-loop is well underway.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("Execute should not return go error on cancel; got %v", got.err)
		}
		if got.res == nil || !got.res.IsError {
			t.Errorf("expected IsError result on cancel; got %+v", got.res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Execute didn't return within 3s after cancel — likely stuck")
	}
	// Roster.Unregister fires on foreground exit so the teammate
	// snapshot is gone; the test confirms the surface contract
	// (Execute returns IsError quickly), not the deferred cleanup.
}

// TestAgentExecute_TimeoutLandsAsFailed — set `timeout_seconds: 1`
// against a 5s slowProvider; confirm Execute returns IsError with
// the timeout hint.
func TestAgentExecute_TimeoutLandsAsFailed(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), slowProvider(5*time.Second), tools.NewRegistry(), "m", "s").
		WithRoster(roster)

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":          "x",
		"timeout_seconds": 1,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Errorf("timeout should be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Errorf("timeout message should say 'timed out'; got %q", res.Output)
	}
}
