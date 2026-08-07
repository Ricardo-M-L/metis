package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// TestBurstAgentRowsAvoidHardScrollRendererPath reproduces the v0.4.7
// multi-agent frame duplication. A single spinner tick can drain ten Agent
// starts followed by ten immediate background-spawn results. The phase starts
// and ends at requesting even though the chat surface grows by dozens of rows.
//
// Ultraviolet's fullscreen hard-scroll operations are the unsafe part of that
// update on direct iTerm2: they can be applied against a stale physical cursor
// and leave the old frame above a second full frame. Metis therefore requires
// its Bubble Tea renderer configuration to avoid CSI L/M/S/T and bare ESC M.
func TestBurstAgentRowsAvoidHardScrollRendererPath(t *testing.T) {
	m := newActiveTransitionRendererModel(t, "requesting")
	m.eventCh = make(chan agent.Event, 32)
	p, out := startTransitionRendererAtSize(t, m, 100, 29)

	initial := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s, "renderer-user-marker") &&
			strings.Contains(s, "connecting")
	})
	transitionOffset := len(initial)

	for i := 0; i < 10; i++ {
		description := fmt.Sprintf("Search benchmark %02d", i)
		if i == 9 {
			description = "Search Terminal-Bench benchmark"
		}
		m.eventCh <- agent.Event{
			Kind:      agent.EventToolStart,
			ToolUseID: fmt.Sprintf("agent-%02d", i),
			ToolName:  "Agent",
			ToolInput: map[string]any{
				"description":       description,
				"prompt":            description,
				"run_in_background": true,
			},
		}
	}
	for i := 0; i < 10; i++ {
		m.eventCh <- agent.Event{
			Kind:      agent.EventToolResult,
			ToolUseID: fmt.Sprintf("agent-%02d", i),
			ToolName:  "Agent",
			ToolResult: &agent.ToolResult{
				Output: fmt.Sprintf("sub-agent spawned in background %02d", i),
			},
		}
	}

	p.Send(spinnerTick{})
	snapshot := waitForRendererOutput(t, out, rendererTestTimeout, func(s string) bool {
		return strings.Contains(s[transitionOffset:], "Search Terminal-Bench benchmark")
	})
	suffix := snapshot[transitionOffset:]

	hardScroll := regexp.MustCompile("(?:\x1b\\[[0-9;]*[LMST]|\x1bM)")
	if matches := hardScroll.FindAllString(suffix, -1); len(matches) != 0 {
		t.Fatalf("burst Agent rows emitted %d fullscreen hard-scroll operations %q; output=%q",
			len(matches), matches, suffix)
	}
}

// Compile-time guard: the probe above deliberately drives the regular
// spinner-tick branch rather than a test-only message.
var _ tea.Msg = spinnerTick{}
