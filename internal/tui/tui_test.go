package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// makeModelForGateTest builds a minimal Model just exercising the Shift+Tab
// debouncer; full TUI wiring isn't needed.
func makeModelForGateTest() *Model {
	return &Model{
		gate:      permission.New(permission.ModeAuto),
		startTime: time.Now().Add(-time.Hour), // far past startup grace
	}
}

// TestFormatTokens locks in claude-code's display style so the spinner
// and statusbar both show consistent "3.1k" / "12k" / "1.2M" labels.
// Boundary cases at 1000, 10000, 1000000 matter — those are where the
// format switches resolution.
func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1.0k"},
		{3100, "3.1k"},
		{9999, "10.0k"}, // rounds up at the boundary
		{10000, "10k"},
		{12345, "12k"},
		{999999, "999k"},
		{1000000, "1.0M"},
		{1234567, "1.2M"},
	}
	for _, c := range cases {
		if got := formatTokens(c.in); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCyclePermissionMode_StartupGraceSwallowsBurst(t *testing.T) {
	// Simulate bubbletea reading a burst of escape sequences right after
	// alt-screen init — this used to surface as five "mode: …" lines
	// before the user touched the keyboard.
	m := &Model{
		gate:      permission.New(permission.ModeAuto),
		startTime: time.Now(), // brand new
	}
	for i := 0; i < 6; i++ {
		m.cyclePermissionMode()
	}
	if got := string(m.gate.Mode()); got != "auto" {
		t.Errorf("startup burst should be ignored; mode = %q, want auto", got)
	}
	if len(m.messages) != 0 {
		t.Errorf("startup burst should not append messages; got %d", len(m.messages))
	}
}

func TestCyclePermissionMode_DebouncesRapidPresses(t *testing.T) {
	m := makeModelForGateTest()
	m.cyclePermissionMode() // 1st: auto → bypass
	m.cyclePermissionMode() // 2nd: rapid follow-up should be ignored
	m.cyclePermissionMode()

	if got := string(m.gate.Mode()); got != "bypass" {
		t.Errorf("after debounced rapid presses, mode = %q, want bypass", got)
	}
	// Mode switches now show in the footer instead of polluting the
	// chat transcript — chat history must stay empty on Shift+Tab spam.
	if len(m.messages) != 0 {
		t.Errorf("Shift+Tab must not append chat messages; got %d", len(m.messages))
	}
}

func TestCyclePermissionMode_AllowsCycleAfterDebounceWindow(t *testing.T) {
	m := makeModelForGateTest()
	m.cyclePermissionMode() // auto → bypass
	// Pretend the debounce window has elapsed.
	m.lastModeCycle = time.Now().Add(-modeCycleDebounce - 10*time.Millisecond)
	m.cyclePermissionMode() // bypass → plan

	if got := string(m.gate.Mode()); got != "plan" {
		t.Errorf("after debounce window, mode = %q, want plan", got)
	}
}

// Ctrl-C semantics:
// - turn active → cancel the turn, do NOT quit
// - idle, single press → arm the quit timer
// - idle, second press within window → quit
// - idle, second press past window → re-arm

func TestCtrlC_CancelsTurnInsteadOfQuitting(t *testing.T) {
	cancelled := false
	m := makeModelForGateTest()
	m.turnActive = true
	m.turnCancel = func() { cancelled = true }

	_, cmd := m.handleKey(teaKeyCtrlC())
	if cmd != nil {
		t.Errorf("Ctrl-C during active turn should NOT return tea.Quit; got cmd=%v", cmd)
	}
	if !cancelled {
		t.Error("Ctrl-C during active turn should call turnCancel()")
	}
	if m.turnCancel != nil {
		t.Error("turnCancel should be cleared after firing")
	}
}

// TestCtrlC_IdleQuitsImmediately covers the user-requested single-press
// exit: when no turn is in flight, Ctrl-C should quit on the first
// press (not wait for a double-tap). The previous "arm-then-quit"
// behaviour was confusing in practice because OSC garbage in the
// input made the user think the input wasn't actually empty.
func TestCtrlC_IdleQuitsImmediately(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = false

	_, cmd := m.handleKey(teaKeyCtrlC())
	if cmd == nil {
		t.Error("idle Ctrl-C should return tea.Quit on first press")
	}
}

// TestCtrlC_DuringTurnArmsQuitTimer: while a turn is running, the
// first Ctrl-C is "interrupt this turn" (not exit). The double-tap
// escape hatch within ctrlCQuitWindow is preserved for the rare case
// that turn cancellation itself hangs.
func TestCtrlC_DuringTurnArmsQuitTimer(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = true
	m.turnCancel = func() {}

	_, cmd := m.handleKey(teaKeyCtrlC())
	if cmd != nil {
		t.Error("first Ctrl-C during turn should NOT quit; should cancel turn")
	}
	if m.lastCtrlC.IsZero() {
		t.Error("first Ctrl-C during turn should arm lastCtrlC for the second-press escape")
	}
}

// TestCtrlC_DoubleTapDuringTurnQuits: second Ctrl-C within the window
// (during the same active turn) should exit — the escape hatch when
// the cancellation path itself is wedged.
func TestCtrlC_DoubleTapDuringTurnQuits(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = true
	m.lastCtrlC = time.Now() // armed by a previous Ctrl-C

	_, cmd := m.handleKey(teaKeyCtrlC())
	if cmd == nil {
		t.Error("double-tap Ctrl-C during turn should return tea.Quit")
	}
}

// teaKeyCtrlC is the bubbletea key message for Ctrl-C; defined as a helper
// to keep the test cases readable without a bubbletea import dance.
func teaKeyCtrlC() tea.KeyMsg {
	return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
}
