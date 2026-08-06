package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func pressCtrlJ(m *Model) {
	m.handleKey(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
}

// TestStartupBufferedEnters_DoNotGrowEmptyInput covers Enter presses queued
// while startup is still in canonical TTY mode (for example, while a
// self-update is downloading). The line discipline turns those Enters into
// LF bytes, which Bubble Tea decodes as ctrl+j rather than enter. Replaying a
// burst must not fill an otherwise-empty textarea with blank lines.
func TestStartupBufferedEnters_DoNotGrowEmptyInput(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	initialHeight := m.input.Height()

	for range 64 {
		pressCtrlJ(m)
	}

	if got := m.input.Value(); got != "" {
		t.Fatalf("buffered Enters changed empty input to %q", got)
	}
	if got := m.input.Height(); got != initialHeight {
		t.Fatalf("buffered Enters grew input height from %d to %d", initialHeight, got)
	}
}

// TestCtrlJ_StillAddsNewlineAfterText preserves the documented alternate
// newline shortcut for real composition. Only whitespace-only input is
// ignored; once text exists, ctrl+j continues to insert literal newlines.
func TestCtrlJ_StillAddsNewlineAfterText(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	typeRunes(t, m, "hello")

	pressCtrlJ(m)

	if got := m.input.Value(); got != "hello\n" {
		t.Fatalf("ctrl+j after text = %q, want %q", got, "hello\n")
	}
	if strings.TrimSpace(m.input.Value()) != "hello" {
		t.Fatalf("ctrl+j corrupted existing input: %q", m.input.Value())
	}
}
