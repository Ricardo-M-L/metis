package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// TestDreaming_StartSetsOverride — EventDreamingStart must pin the
// spinner verb to "Dreaming...", matching the Phase C UX spec.
// Without this the user sees a generic "thinking" verb during a
// 10-30 s background dream and assumes the agent is hung.
func TestDreaming_StartSetsOverride(t *testing.T) {
	m := minimalModel(200_000)
	m.handleAgentEvent(agent.Event{Kind: agent.EventDreamingStart, Info: "scheduled"})
	if m.spinnerOverride != "Dreaming..." {
		t.Fatalf("spinnerOverride = %q, want 'Dreaming...'", m.spinnerOverride)
	}
}

// TestDreaming_EndClearsOverrideAndAppendsSummary — EventDreamingEnd
// must clear the override and append an inline summary message so the
// user sees "✻ context dreamed: +2 memories, +1 skill" in the
// transcript. The "compaction" role triggers the ✻ banner styling.
func TestDreaming_EndClearsOverrideAndAppendsSummary(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerOverride = "Dreaming..."
	m.handleAgentEvent(agent.Event{Kind: agent.EventDreamingEnd, Info: "+2 memories, +1 skill"})
	if m.spinnerOverride != "" {
		t.Errorf("spinnerOverride should clear, got %q", m.spinnerOverride)
	}
	if len(m.messages) == 0 {
		t.Fatalf("expected an inline summary message, got none")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "compaction" {
		t.Errorf("role = %q, want compaction (for ✻ banner)", last.Role)
	}
	if !strings.Contains(last.Content, "+2 memories, +1 skill") {
		t.Errorf("content missing summary: %q", last.Content)
	}
	if !strings.HasPrefix(last.Content, "context dreamed:") {
		t.Errorf("content should lead with 'context dreamed:', got %q", last.Content)
	}
}

// TestDreaming_NoChangesSkipsMessage — when the dream produced no
// memory or skill changes, the TUI suppresses the inline message
// entirely. A user-facing "nothing to save" note would be noise.
func TestDreaming_NoChangesSkipsMessage(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerOverride = "Dreaming..."
	before := len(m.messages)
	m.handleAgentEvent(agent.Event{Kind: agent.EventDreamingEnd, Info: "no changes"})
	if m.spinnerOverride != "" {
		t.Errorf("spinnerOverride should still clear, got %q", m.spinnerOverride)
	}
	if len(m.messages) != before {
		t.Fatalf("'no changes' should NOT append a message, got %d new",
			len(m.messages)-before)
	}
}

// TestDreaming_FailureShowsAsError — a "failed:" summary lands in the
// error role so the existing error-display chrome surfaces it
// distinctively (the user shouldn't have to look at a dim ✻ line to
// notice their dream broke).
func TestDreaming_FailureShowsAsError(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerOverride = "Dreaming..."
	m.handleAgentEvent(agent.Event{Kind: agent.EventDreamingEnd, Info: "failed: provider 429"})
	if len(m.messages) == 0 {
		t.Fatalf("expected error message, got none")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "error" {
		t.Errorf("role = %q, want error (for distinct display)", last.Role)
	}
	if !strings.Contains(last.Content, "provider 429") {
		t.Errorf("content missing failure reason: %q", last.Content)
	}
}

// TestDreaming_ExtrasRenderUnderSpinner — when spinnerOverride is
// "Dreaming...", renderSpinnerStatus must emit the dreaming sub-line
// in addition to the spinner row. Mirrors the compaction-extras
// pattern but with simpler chrome (no progress bar — dreaming has
// no reliable progress signal).
func TestDreaming_ExtrasRenderUnderSpinner(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerOverride = "Dreaming..."

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "Dreaming...") {
		t.Errorf("spinner row missing 'Dreaming...': %q", out)
	}
	if !strings.Contains(out, "└ Consolidating recent sessions") {
		t.Errorf("dreaming sub-line missing: %q", out)
	}
	// Sanity: compaction-only chrome must NOT leak in.
	if strings.Contains(out, "auto window") {
		t.Errorf("compaction sub-line leaked into dreaming render: %q", out)
	}
}
