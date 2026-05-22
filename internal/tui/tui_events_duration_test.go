package tui

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// TestToolEvent_DurationParallelSameName — image #48 repro:
// 5 parallel sub-agent Reads with the same ToolName but distinct
// ToolUseIDs. Pre-fix the matching loop took the LATEST Start for
// every Result → all Durations ≈ 0ms. After the ID-based match the
// Duration of each Result reflects ITS OWN Start, not the most
// recent.
func TestToolEvent_DurationParallelSameName(t *testing.T) {
	m := &Model{}

	now := time.Now()
	// Emit 5 starts in quick succession, each ~10ms after the prior.
	// We synthesise the spacing via faked-out StartTime after handle.
	for i := 1; i <= 5; i++ {
		m.handleAgentEvent(agent.Event{
			Kind:      agent.EventToolStart,
			ToolUseID: idForN(i),
			ToolName:  "sub: Read",
			ToolInput: map[string]any{"path": "/p"},
		})
		// Backdate StartTime to simulate i*10ms ago.
		m.toolEvents[len(m.toolEvents)-1].StartTime = now.Add(-time.Duration((6-i)*10) * time.Millisecond)
	}
	if len(m.toolEvents) != 5 {
		t.Fatalf("expected 5 toolEvents; got %d", len(m.toolEvents))
	}

	// Emit Results in REVERSE order — Result for id-1 (oldest Start,
	// should report ~50ms) arrives LAST. Without the ID match, that
	// last Result would mis-match against any remaining Start; with
	// the ID match each Result pairs with its own Start.
	for i := 5; i >= 1; i-- {
		m.handleAgentEvent(agent.Event{
			Kind:      agent.EventToolResult,
			ToolUseID: idForN(i),
			ToolName:  "sub: Read",
			ToolResult: &agent.ToolResult{
				Output: "ok",
			},
		})
	}

	// Verify each event landed on Kind=result AND Duration is non-zero
	// (proves the ID-match path fired, not the legacy "use latest"
	// path which would have made one Result steal another's Start
	// leaving 4 Starts orphaned).
	resultCount := 0
	for _, te := range m.toolEvents {
		if te.Kind != "result" {
			continue
		}
		resultCount++
		if te.Duration <= 0 {
			t.Errorf("Result with ID=%s has Duration=%v, want > 0", te.ID, te.Duration)
		}
	}
	if resultCount != 5 {
		t.Errorf("expected all 5 events to be Result; got %d result, %d total", resultCount, len(m.toolEvents))
	}
}

// idForN — tiny helper to build distinct toolUseIDs for the test.
func idForN(n int) string {
	switch n {
	case 1:
		return "toolu_one"
	case 2:
		return "toolu_two"
	case 3:
		return "toolu_three"
	case 4:
		return "toolu_four"
	default:
		return "toolu_five"
	}
}

// TestToolEvent_DurationLegacyFallback — events without ToolUseID
// (synthetic / replayed) should still pair via the ToolName fallback
// and report a sensible Duration. Guards the fallback branch.
func TestToolEvent_DurationLegacyFallback(t *testing.T) {
	m := &Model{}
	m.handleAgentEvent(agent.Event{
		Kind:     agent.EventToolStart,
		ToolName: "Read",
		// No ToolUseID — legacy / replay path.
	})
	m.toolEvents[0].StartTime = time.Now().Add(-25 * time.Millisecond)
	m.handleAgentEvent(agent.Event{
		Kind:       agent.EventToolResult,
		ToolName:   "Read",
		ToolResult: &agent.ToolResult{Output: "ok"},
	})
	if got := m.toolEvents[0].Duration; got <= 0 {
		t.Errorf("legacy fallback should still measure Duration; got %v", got)
	}
}
