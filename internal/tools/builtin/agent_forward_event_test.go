package builtin

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// TestForwardSubAgentEventClosedChannel guards against the "send on closed
// channel" panic that crashed background sub-agents when their parent turn
// ended. The parent's per-turn event channel is closed by the TUI the moment
// the parent loop.Run returns; a background sub-agent that outlives the turn
// may still be forwarding tool events. select+default does NOT protect against
// a send on a closed channel (only against a full buffer), so
// forwardSubAgentEvent must recover internally.
//
// Before the fix this panicked with "send on closed channel" exactly as seen
// in production (2026-08-10: two background agents died ~48s after spawn,
// one with that panic). After the fix the send is silently dropped.
func TestForwardSubAgentEventClosedChannel(t *testing.T) {
	parentOut := make(chan agent.Event, 1)

	// Close the channel to simulate the parent turn having ended.
	close(parentOut)

	ev := agent.Event{
		Kind:     agent.EventToolStart,
		ToolName: "Read",
	}

	// Must not panic.
	forwardSubAgentEvent(parentOut, "parent-tool-use-id", ev)
	forwardSubAgentEvent(parentOut, "parent-tool-use-id", agent.Event{
		Kind:     agent.EventToolResult,
		ToolName: "Bash",
	})

	// A nil channel is a no-op and must also not panic.
	forwardSubAgentEvent(nil, "parent-tool-use-id", ev)
}

// TestForwardSubAgentEventDeliversWhenOpen confirms the happy path still
// delivers forwarded events to a live parent channel and stamps the
// "sub: " prefix + SubAgentParentID.
func TestForwardSubAgentEventDeliversWhenOpen(t *testing.T) {
	parentOut := make(chan agent.Event, 4)
	ev := agent.Event{Kind: agent.EventToolStart, ToolName: "Grep"}

	forwardSubAgentEvent(parentOut, "tool-use-123", ev)

	select {
	case got := <-parentOut:
		if got.ToolName != "sub: Grep" {
			t.Fatalf("expected prefixed tool name %q, got %q", "sub: Grep", got.ToolName)
		}
		if got.SubAgentParentID != "tool-use-123" {
			t.Fatalf("expected SubAgentParentID %q, got %q", "tool-use-123", got.SubAgentParentID)
		}
	default:
		t.Fatal("expected a forwarded event on the parent channel, got none")
	}
}
