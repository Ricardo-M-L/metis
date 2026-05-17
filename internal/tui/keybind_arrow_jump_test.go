package tui

// keybind_arrow_jump_test.go — covers the "single-line non-empty input
// + ↑/↓ jumps to start/end" behaviour added 2026-05-07 (user video
// 21:18: "输入向上箭头他还是不能跳到这个输入框最开始的地方").
//
// The default bubbles textarea LineUp/LineDown is a no-op when there
// is only one row of text, so users perceived the keys as broken.
// We intercept that case and fall back to CursorStart/CursorEnd.
//
// Multi-row inputs still get the default LineUp/LineDown behaviour
// (cursor moves between rows), and empty / history-loaded inputs go
// through directHistoryUp/Down as before.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pressKey is a tiny helper that builds a tea.KeyPressMsg with a
// human-readable key code ("up", "down", "home", etc.) and feeds it
// through the model's main key handler. Mirrors the path a real
// keystroke takes: tea.KeyPressMsg → tui_update.Update → handleKey.
func pressKey(t *testing.T, m *Model, code string) {
	t.Helper()
	var key tea.Key
	switch code {
	case "up":
		key = tea.Key{Code: tea.KeyUp}
	case "down":
		key = tea.Key{Code: tea.KeyDown}
	case "home":
		key = tea.Key{Code: tea.KeyHome}
	case "end":
		key = tea.Key{Code: tea.KeyEnd}
	case "alt+y":
		key = tea.Key{Code: 'y', Mod: tea.ModAlt}
	default:
		t.Fatalf("pressKey: unknown code %q", code)
	}
	m.handleKey(tea.KeyPressMsg(key))
}

// TestArrowJump_SingleLineUpJumpsToStart — user typed "hello", cursor
// is at the end (col 5). Pressing ↑ should snap cursor to col 0.
func TestArrowJump_SingleLineUpJumpsToStart(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1
	m.input.SetValue("hello")
	m.input.CursorEnd()

	if got := m.input.Line(); got != 0 {
		t.Fatalf("precondition: should be on line 0; got %d", got)
	}
	if got := m.input.Column(); got != 5 {
		t.Fatalf("precondition: cursor should be at col 5; got %d", got)
	}

	pressKey(t, m, "up")

	if got := m.input.Column(); got != 0 {
		t.Errorf("↑ on single-line input should jump to col 0; got %d", got)
	}
}

// TestArrowJump_SingleLineDownJumpsToEnd — cursor at col 0 of "hello",
// pressing ↓ should land on col 5.
func TestArrowJump_SingleLineDownJumpsToEnd(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1
	m.input.SetValue("hello")
	m.input.CursorStart()

	if got := m.input.Column(); got != 0 {
		t.Fatalf("precondition: cursor should be at col 0; got %d", got)
	}

	pressKey(t, m, "down")

	// "hello" is 5 columns wide.
	if got := m.input.Column(); got != 5 {
		t.Errorf("↓ on single-line input should jump to col 5; got %d", got)
	}
}

// TestArrowJump_EmptyInputStillTriggersHistory — empty input + ↑ goes
// through directHistoryUp, NOT the new jump-to-start path. This is the
// established bash/zsh-style behaviour and must not regress.
func TestArrowJump_EmptyInputStillTriggersHistory(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.histDirectIdx = -1
	m.histAll = []string{"prev1", "prev2"} // skip filesystem load
	m.input.SetValue("")

	pressKey(t, m, "up")

	if got := m.input.Value(); got != "prev1" {
		t.Errorf("↑ on empty input should load history[0] (%q); got %q", "prev1", got)
	}
	if m.histDirectIdx != 0 {
		t.Errorf("histDirectIdx should be 0 after first ↑; got %d", m.histDirectIdx)
	}
}

// TestArrowJump_SingleLineUpAtCol0LoadsHistory — second ↑ press, when
// the cursor is already at column 0 of a non-empty input, must hand
// off to direct-history nav (claude-code parity, 2026-05-16 user
// screenshot 32). Otherwise the keystroke is wasted: CursorStart is a
// no-op at col 0 and textarea LineUp has no row above to land on.
func TestArrowJump_SingleLineUpAtCol0LoadsHistory(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1
	m.histAll = []string{"older1", "older2"} // skip filesystem load
	m.input.SetValue("draft text")
	m.input.CursorStart() // cursor at (line=0, col=0)

	if got := m.input.Column(); got != 0 {
		t.Fatalf("precondition: cursor should be at col 0; got %d", got)
	}

	pressKey(t, m, "up")

	if got := m.input.Value(); got != "older1" {
		t.Errorf("↑ at col 0 with non-empty input should load history[0] (%q); got %q",
			"older1", got)
	}
	if m.histDirectIdx != 0 {
		t.Errorf("histDirectIdx should be 0 after history load; got %d", m.histDirectIdx)
	}
	if m.histDirectDraft != "draft text" {
		t.Errorf("draft should be stashed for ↓ restore; got %q", m.histDirectDraft)
	}
}

// TestArrowJump_MultiLineUpAtOriginLoadsHistory — multi-line input,
// cursor at (line=0, col=0), ↑ should load history rather than fall
// through to a no-op textarea LineUp. Same fix as the single-line case;
// the multi-line path used to silently swallow the keystroke.
func TestArrowJump_MultiLineUpAtOriginLoadsHistory(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1
	m.histAll = []string{"older-prompt"}
	m.input.SetValue("first\nsecond")
	// Move all the way to start: row 0, col 0.
	m.input.CursorStart()
	for m.input.Line() > 0 {
		m.input.CursorUp()
	}

	if got := m.input.Line(); got != 0 {
		t.Fatalf("precondition: line should be 0; got %d", got)
	}
	if got := m.input.Column(); got != 0 {
		t.Fatalf("precondition: column should be 0; got %d", got)
	}

	pressKey(t, m, "up")

	if got := m.input.Value(); got != "older-prompt" {
		t.Errorf("↑ at (0,0) of multi-line input should load history; got %q", got)
	}
}

// TestArrowJump_MultiLineUpStaysAsLineMove — when the input has two
// rows of text and the cursor is on row 1, ↑ should move to row 0
// (default textarea behaviour), NOT jump to col 0.
func TestArrowJump_MultiLineUpStaysAsLineMove(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.input.Focus()
	m.histDirectIdx = -1
	m.input.SetValue("first\nsecond")
	m.input.CursorEnd()

	if got := m.input.LineCount(); got < 2 {
		t.Fatalf("precondition: expected 2 logical lines; got %d", got)
	}
	// CursorEnd may land us on row 1 OR keep row 0 depending on how
	// the textarea internalises CursorEnd vs MoveToEnd. If it left
	// us on row 0, manually move to row 1 so the test's premise
	// (cursor on the LAST row) holds.
	if m.input.Line() == 0 {
		m.input.CursorDown()
	}
	if got := m.input.Line(); got != 1 {
		t.Fatalf("precondition: cursor should be on line 1 before ↑; got %d", got)
	}

	pressKey(t, m, "up")

	if got := m.input.Line(); got != 0 {
		t.Errorf("↑ on multi-line input should move cursor up one row; landed on line %d", got)
	}
}
