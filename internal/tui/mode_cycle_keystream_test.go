package tui

// mode_cycle_keystream_test.go — in-process E2E for the Shift+Tab mode
// cycle (2026-05-11). Pairs with mode_badge_test.go: that file pins
// modeIcon/renderHints output for each mode, this one pins the KEY
// PATH — that pressing Shift+Tab actually walks the gate through the
// new claude-code-aligned sequence.
//
// Why this is "E2E enough": the test enters at Model.Update with a
// real tea.KeyPressMsg, traverses tui_update.go::Update → handleKey
// → "shift+tab" case → cyclePermissionMode → gate.SetMode, then
// pulls renderHints to verify the bottom-of-screen badge reflects
// the new mode. This is the same code path the real terminal hits;
// only the bubbletea runtime + pty layer (which we know are
// unreliable under tmux) are bypassed.
//
// The cycle being locked: ask → acceptEdits → plan → bypass → deny → ask.
// This is the order from keybind_permission.go::cyclePermissionMode
// after the 2026-05-11 ModeAuto removal — must match claude-code's
// getNextPermissionMode.ts:39.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// pressShiftTab injects a Shift+Tab keypress through Model.Update,
// then clears the debounce timestamp so the next press isn't
// swallowed. cyclePermissionMode's 200 ms debouncer exists to
// collapse twin escape sequences some terminals emit per keypress —
// when we drive the model directly there's no such pairing, so
// resetting lastModeCycle simulates "user waited long enough".
func pressShiftTab(t *testing.T, m *Model) {
	t.Helper()
	msg := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	if got := msg.String(); got != "shift+tab" {
		t.Fatalf("bubbletea encoding sanity check failed: KeyTab+ModShift → %q, want shift+tab (test won't reach cyclePermissionMode)", got)
	}
	updated, _ := m.Update(msg)
	*m = *(updated.(*Model))
	m.lastModeCycle = time.Time{} // let the next press through
}

// modeCycleTestModel builds a minimal Model for keypress-driven
// cycling. startTime is backdated past modeCycleStartupGrace so the
// first Shift+Tab isn't swallowed by the "alt-screen escape burst"
// guard; permission gate starts at the documented default ("ask").
func modeCycleTestModel() *Model {
	return &Model{
		gate:      permission.New(permission.ModeAsk),
		startTime: time.Now().Add(-time.Hour),
	}
}

// TestModeCycle_FullKeystreamWalk — the headline E2E. Five Shift+Tab
// presses must walk the gate through every mode and back to ask.
// Locks both the order AND the wraparound. A future refactor that
// drops a mode, reorders, or breaks the wrap will trip immediately.
func TestModeCycle_FullKeystreamWalk(t *testing.T) {
	m := modeCycleTestModel()
	want := []permission.Mode{
		permission.ModeAcceptEdits, // ask → acceptEdits
		permission.ModePlan,        // acceptEdits → plan
		permission.ModeBypass,      // plan → bypass
		permission.ModeDeny,        // bypass → deny
		permission.ModeAsk,         // deny → ask (wraparound)
	}
	for i, expected := range want {
		pressShiftTab(t, m)
		if got := m.gate.Mode(); got != expected {
			t.Fatalf("step %d: gate.Mode() = %q, want %q (full sequence: ask → acceptEdits → plan → bypass → deny → ask)",
				i+1, got, expected)
		}
	}
}

// TestModeCycle_HintsReflectEachStep — every Shift+Tab must produce
// a renderHints output that names the new mode (or, for ask, omits
// the badge entirely per claude-code parity). Pins the integration
// between the cycle keypath and the renderer. If renderHints ever
// stops keying off m.gate.Mode() — or if the cycle walks but the
// display doesn't update — this fires.
func TestModeCycle_HintsReflectEachStep(t *testing.T) {
	m := modeCycleTestModel()

	type step struct {
		expectMode permission.Mode
		hintSubstr string // what renderHints should contain
		hintOmits  string // what it must NOT contain (anti-regression for ask)
	}
	walk := []step{
		{permission.ModeAcceptEdits, "acceptEdits mode", ""},
		{permission.ModePlan, "plan mode", ""},
		{permission.ModeBypass, "bypass mode", ""},
		{permission.ModeDeny, "deny mode", ""},
		// Wraparound back to ask: badge is suppressed (only "shift+tab"
		// hint remains — the claude-code-parity move).
		{permission.ModeAsk, "shift+tab", "ask mode"},
	}
	for i, s := range walk {
		pressShiftTab(t, m)
		if got := m.gate.Mode(); got != s.expectMode {
			t.Fatalf("step %d: gate.Mode() = %q, want %q", i+1, got, s.expectMode)
		}
		out := renderHints(m)
		if !strings.Contains(out, s.hintSubstr) {
			t.Errorf("step %d (mode=%s): renderHints should contain %q; got:\n%s",
				i+1, s.expectMode, s.hintSubstr, out)
		}
		if s.hintOmits != "" && strings.Contains(out, s.hintOmits) {
			t.Errorf("step %d (mode=%s): renderHints must NOT contain %q (claude-code-parity); got:\n%s",
				i+1, s.expectMode, s.hintOmits, out)
		}
	}
}

// TestModeCycle_BypassCarriesWarningGlyph — pin that the keystream
// path lands a real ⏵⏵ glyph in the rendered hint when the user
// reaches bypass. The path that breaks this: cycle works, but the
// renderer somehow strips the icon (e.g. someone changes modeIcon
// to return empty for bypass). Without the glyph the user has no
// visual cue they've entered the dangerous mode.
func TestModeCycle_BypassCarriesWarningGlyph(t *testing.T) {
	m := modeCycleTestModel()
	// ask → acceptEdits → plan → bypass (3 presses).
	for i := 0; i < 3; i++ {
		pressShiftTab(t, m)
	}
	if got := m.gate.Mode(); got != permission.ModeBypass {
		t.Fatalf("3 Shift+Tab from ask should land on bypass; got %q", got)
	}
	out := renderHints(m)
	if !strings.Contains(out, "⏵⏵") {
		t.Errorf("bypass hint must carry ⏵⏵ warning glyph; got:\n%s", out)
	}
}

// TestModeCycle_DebounceBlocksDoublePress — pressing Shift+Tab twice
// in rapid succession (within modeCycleDebounce) should advance the
// gate by exactly ONE step, not two. This is the "terminal sent the
// escape pair as two keypresses" failure mode. Without the gate this
// would walk past the user's intended mode.
func TestModeCycle_DebounceBlocksDoublePress(t *testing.T) {
	m := modeCycleTestModel()
	// First press advances: ask → acceptEdits.
	msg := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	updated, _ := m.Update(msg)
	*m = *(updated.(*Model))
	// Immediate second press — should be swallowed by the 200 ms
	// debounce window (we did NOT clear lastModeCycle).
	updated, _ = m.Update(msg)
	*m = *(updated.(*Model))

	if got := m.gate.Mode(); got != permission.ModeAcceptEdits {
		t.Errorf("rapid double Shift+Tab must debounce to single advance; mode = %q, want acceptEdits", got)
	}
}

// TestModeCycle_StartupGraceSwallowsFirstPress — fresh Model (no
// backdated startTime) ignores any Shift+Tab that lands inside the
// 800 ms grace window. This is the "alt-screen init escape burst"
// guard. Without it, users see the mode flip before they touched a
// key.
func TestModeCycle_StartupGraceSwallowsFirstPress(t *testing.T) {
	m := &Model{
		gate:      permission.New(permission.ModeAsk),
		startTime: time.Now(), // brand new
	}
	msg := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	updated, _ := m.Update(msg)
	*m = *(updated.(*Model))
	if got := m.gate.Mode(); got != permission.ModeAsk {
		t.Errorf("Shift+Tab during startup grace must not change mode; got %q, want ask", got)
	}
}
