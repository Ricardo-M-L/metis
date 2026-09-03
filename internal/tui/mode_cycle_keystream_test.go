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
// The cycle being locked: default → acceptEdits → plan → bypassPermissions →
// fullAccess → default.
// The first four entries retain the Claude Code-aligned sequence; METIS adds
// its separate fullAccess posture as the final red-marked step.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
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
	updated, cmd := m.Update(msg)
	*m = *(updated.(*Model))
	if cmd != nil {
		updated, _ = m.Update(cmd())
		*m = *(updated.(*Model))
	}
	m.lastModeCycle = time.Time{} // let the next press through
}

// modeCycleTestModel builds a minimal Model for keypress-driven
// cycling. startTime is backdated past modeCycleStartupGrace so the
// first Shift+Tab isn't swallowed by the "alt-screen escape burst"
// guard; permission gate starts at the documented default.
func modeCycleTestModel(t *testing.T) *Model {
	t.Helper()
	if !sandbox.Available() {
		t.Skip("bypassPermissions requires an available OS sandbox")
	}
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return &Model{
		gate:      permission.New(permission.ModeAsk),
		startTime: time.Now().Add(-time.Hour),
		ext:       ExternalHooks{Sandbox: manager},
	}
}

// TestModeCycle_FullKeystreamWalk — the headline E2E. Five Shift+Tab
// presses must walk Claude Code's interactive cycle back to default.
// Locks both the order AND the wraparound. A future refactor that
// drops a mode, reorders, or breaks the wrap will trip immediately.
func TestModeCycle_FullKeystreamWalk(t *testing.T) {
	m := modeCycleTestModel(t)
	want := []permission.Mode{
		permission.ModeAcceptEdits,       // default → acceptEdits
		permission.ModePlan,              // acceptEdits → plan
		permission.ModeBypassPermissions, // plan → bypassPermissions
		permission.ModeFullAccess,        // bypassPermissions → fullAccess
		permission.ModeDefault,           // fullAccess → default (wraparound)
	}
	for i, expected := range want {
		pressShiftTab(t, m)
		if got := m.gate.Mode(); got != expected {
			t.Fatalf("step %d: gate.Mode() = %q, want %q (full sequence: default → acceptEdits → plan → bypassPermissions → fullAccess → default)",
				i+1, got, expected)
		}
	}
}

func TestModeCycle_EnteringFullAccessDisablesSandbox(t *testing.T) {
	m := modeCycleTestModel(t)
	for range 4 { // default → acceptEdits → plan → bypassPermissions → fullAccess
		pressShiftTab(t, m)
	}

	if got := m.gate.Mode(); got != permission.ModeFullAccess {
		t.Fatalf("4 Shift+Tab from default = %q, want fullAccess", got)
	}
	if !m.ext.Sandbox.State().FullAccessRequired {
		t.Fatal("entering fullAccess through Shift+Tab did not disable the process sandbox")
	}
}

func TestModeCycle_FromFullAccessReturnsToDefault(t *testing.T) {
	m := modeCycleTestModel(t)
	if err := applyModelPermissionMode(m, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	if !m.ext.Sandbox.State().FullAccessRequired {
		t.Fatal("test setup did not enter fullAccess sandbox posture")
	}

	pressShiftTab(t, m)
	if got := m.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("Shift+Tab from fullAccess = %q, want safe default", got)
	}
	if m.ext.Sandbox.State().FullAccessRequired {
		t.Fatal("leaving fullAccess through Shift+Tab left the sandbox bypass active")
	}
}

func TestModeCycle_TurnActiveCanLeaveFullAccess(t *testing.T) {
	m := modeCycleTestModel(t)
	if err := applyModelPermissionMode(m, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	m.turnActive = true
	pressShiftTab(t, m)

	if got := m.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("active-turn Shift+Tab mode = %q, want default", got)
	}
	if m.ext.Sandbox.State().FullAccessRequired {
		t.Fatal("active-turn Shift+Tab did not restore the process sandbox")
	}
	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "running turn is active") {
			t.Fatalf("active-turn Shift+Tab still surfaced a refusal: %+v", m.messages)
		}
	}
}

func TestModeCycle_TurnActiveQueuesBehindExecutingToolWithoutBlockingUI(t *testing.T) {
	m := modeCycleTestModel(t)
	if err := applyModelPermissionMode(m, permission.ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	m.turnActive = true

	releaseTool, allowed, reason := m.gate.TryAcquireToolDispatchLease()
	if !allowed {
		t.Fatalf("acquire simulated tool dispatch lease: %s", reason)
	}
	released := false
	defer func() {
		if !released {
			releaseTool()
		}
	}()

	key := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	updated, cmd := m.Update(key)
	*m = *(updated.(*Model))
	if cmd == nil {
		t.Fatal("active-turn Shift+Tab did not return an asynchronous transition command")
	}
	if !m.permissionModePending || m.permissionModeTarget != permission.ModeDefault {
		t.Fatalf("pending transition = (%v, %q), want (true, default)", m.permissionModePending, m.permissionModeTarget)
	}
	if got := m.gate.Mode(); got != permission.ModeFullAccess {
		t.Fatalf("mode changed before executing tool boundary: got %q", got)
	}
	if hint := stripANSI(renderHints(m)); !strings.Contains(hint, "permission mode → default") || !strings.Contains(hint, "tool boundary") {
		t.Fatalf("pending transition hint missing: %q", hint)
	}

	resultCh := make(chan tea.Msg, 1)
	go func() { resultCh <- cmd() }()
	select {
	case <-resultCh:
		t.Fatal("permission transition crossed an executing tool boundary")
	case <-time.After(50 * time.Millisecond):
	}

	releaseTool()
	released = true
	var result tea.Msg
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("permission transition did not finish after tool boundary released")
	}
	updated, _ = m.Update(result)
	*m = *(updated.(*Model))
	if m.permissionModePending {
		t.Fatal("permission transition remained pending after completion")
	}
	if got := m.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("settled active-turn mode = %q, want default", got)
	}
	if m.ext.Sandbox.State().FullAccessRequired {
		t.Fatal("settled active-turn transition did not restore the sandbox")
	}
}

// TestModeCycle_HintsReflectEachStep — every Shift+Tab must produce
// a renderHints output that names the new mode (or, for ask, omits
// the badge entirely per claude-code parity). Pins the integration
// between the cycle keypath and the renderer. If renderHints ever
// stops keying off m.gate.Mode() — or if the cycle walks but the
// display doesn't update — this fires.
func TestModeCycle_HintsReflectEachStep(t *testing.T) {
	m := modeCycleTestModel(t)

	type step struct {
		expectMode permission.Mode
		hintSubstr string // what renderHints should contain
		hintOmits  string // what it must NOT contain (anti-regression for ask)
	}
	walk := []step{
		{permission.ModeAcceptEdits, "acceptEdits mode", ""},
		{permission.ModePlan, "plan mode", ""},
		{permission.ModeBypassPermissions, "bypassPermissions mode", ""},
		{permission.ModeFullAccess, "fullAccess mode", ""},
		// Wraparound back to ask: badge is suppressed (only "shift+tab"
		// hint remains — the claude-code-parity move).
		{permission.ModeDefault, "shift+tab", "default mode"},
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
	m := modeCycleTestModel(t)
	// default → acceptEdits → plan → bypassPermissions (3 presses).
	for i := 0; i < 3; i++ {
		pressShiftTab(t, m)
	}
	if got := m.gate.Mode(); got != permission.ModeBypassPermissions {
		t.Fatalf("3 Shift+Tab from default should land on bypassPermissions; got %q", got)
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
	m := modeCycleTestModel(t)
	// First press advances: default → acceptEdits.
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
