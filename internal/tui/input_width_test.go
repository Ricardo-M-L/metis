package tui

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
)

// TestInputWidth_FillsTerminal regresses the 2026-05-09 bug where
// the textarea hard-capped at 100 cols, causing premature wrap when
// the user pasted CJK + paths into a wide terminal (image #10).
//
// Asserted property: on a wide terminal, the textarea should occupy
// approximately termW - small fixed overhead, NOT a 100-cell ceiling.
func TestInputWidth_FillsTerminal(t *testing.T) {
	cases := []struct {
		termW    int
		minWidth int // must hit at least this
	}{
		{80, 70},   // narrow: ~ termW - 4
		{120, 110}, // medium: most of width
		{180, 170}, // wide: not capped at 100 anymore
		{220, 200}, // very wide
	}
	for _, tc := range cases {
		m := &Model{width: tc.termW, input: textarea.New()}
		_ = renderInputLine(m)
		if got := m.input.Width(); got < tc.minWidth {
			t.Errorf("termW=%d: input width %d below floor %d (regression: 100-cell cap?)",
				tc.termW, got, tc.minWidth)
		}
	}
}

// TestInputWidth_NarrowDoesNotCrash — sub-30-col terminal still
// gives a usable input. The 20-cell floor in render_chrome.go
// protects us from the SetWidth(0) corner case that crashes some
// textarea implementations. The textarea library may further clamp
// to its own internal minimum (~14 cells); the test only asserts
// "non-zero, no crash".
func TestInputWidth_NarrowDoesNotCrash(t *testing.T) {
	m := &Model{width: 10, input: textarea.New()}
	_ = renderInputLine(m)
	if got := m.input.Width(); got < 10 {
		t.Errorf("narrow terminal should still produce a usable input; got %d", got)
	}
}
