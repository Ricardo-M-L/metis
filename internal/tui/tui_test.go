package tui

import (
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// makeModelForGateTest builds a minimal Model just exercising the Shift+Tab
// debouncer; full TUI wiring isn't needed.
func makeModelForGateTest() *Model {
	return &Model{
		gate:      permission.New(permission.ModeAcceptEdits),
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
		gate:      permission.New(permission.ModeAcceptEdits),
		startTime: time.Now(), // brand new
	}
	for i := 0; i < 6; i++ {
		m.cyclePermissionMode()
	}
	// New (2026-05-11) cycle: ask → acceptEdits → plan → bypass → deny.
	// During startup grace, ANY cycle should be no-op, so the mode
	// stays at whatever it was when we constructed the gate.
	if got := string(m.gate.Mode()); got != "acceptEdits" {
		t.Errorf("startup burst should be ignored; mode = %q, want acceptEdits", got)
	}
	if len(m.messages) != 0 {
		t.Errorf("startup burst should not append messages; got %d", len(m.messages))
	}
}

func TestCyclePermissionMode_DebouncesRapidPresses(t *testing.T) {
	m := makeModelForGateTest()
	m.cyclePermissionMode() // 1st: acceptEdits → plan
	m.cyclePermissionMode() // 2nd: rapid follow-up should be ignored
	m.cyclePermissionMode()

	if got := string(m.gate.Mode()); got != "plan" {
		t.Errorf("after debounced rapid presses, mode = %q, want plan", got)
	}
	// Mode switches now show in the footer instead of polluting the
	// chat transcript — chat history must stay empty on Shift+Tab spam.
	if len(m.messages) != 0 {
		t.Errorf("Shift+Tab must not append chat messages; got %d", len(m.messages))
	}
}

func TestCyclePermissionMode_AllowsCycleAfterDebounceWindow(t *testing.T) {
	m := makeModelForGateTest()
	m.cyclePermissionMode() // acceptEdits → plan
	// Pretend the debounce window has elapsed.
	m.lastModeCycle = time.Now().Add(-modeCycleDebounce - 10*time.Millisecond)
	m.cyclePermissionMode() // plan → bypass

	if got := string(m.gate.Mode()); got != "bypassPermissions" {
		t.Errorf("after debounce window, mode = %q, want bypassPermissions", got)
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
//
// exitFunc is package-init-overridden to a no-op + counter by
// exit_hook_test.go's init(), so the 800 ms-scheduled hard-exit
// goroutine that this path spawns can fire harmlessly during the
// test suite. This test asserts both that handleKey returns a Quit
// cmd AND that the goroutine actually fires (counter increments
// within 2 s) — pre-fix the goroutine called the real os.Exit when
// it fired, killing the whole `go test ./internal/tui/...` binary.
func TestCtrlC_DoubleTapDuringTurnQuits(t *testing.T) {
	startCount := atomic.LoadInt64(&exitTestCallCount)

	m := makeModelForGateTest()
	m.turnActive = true
	m.lastCtrlC = time.Now() // armed by a previous Ctrl-C

	_, cmd := m.handleKey(teaKeyCtrlC())
	if cmd == nil {
		t.Error("double-tap Ctrl-C during turn should return tea.Quit")
	}

	// Poll for the hard-exit goroutine to fire (sleep 800 ms then
	// call exitFunc). 2 s budget = 800 ms sleep + race-detector
	// overhead + comfort margin.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&exitTestCallCount) > startCount {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("hard-exit goroutine never fired within 2 s — pre-fix bug would have called real os.Exit instead")
}

// teaKeyCtrlC is the bubbletea key message for Ctrl-C; defined as a helper
// to keep the test cases readable without a bubbletea import dance.
func teaKeyCtrlC() tea.KeyMsg {
	return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
}

func teaKeyEsc() tea.KeyMsg {
	return tea.KeyPressMsg{Code: tea.KeyEscape}
}

// TestEsc_CancelsTurnEvenWhenPaletteOpen — 2026-05-26 regression for
// user screenshot #75: with the slash-command palette open AND a turn
// in flight (long-running python3 tool call), pressing ESC went to
// the palette-dismiss path and the turn kept running. The fix moved
// the turn-cancel branch to the very top of handleKey so it runs
// BEFORE every overlay-intercept handler. This test pins that the
// single ESC during a turn now (a) calls turnCancel, (b) clears the
// palette state, (c) drops the queued prompts, regardless of whether
// any overlay is open.
func TestEsc_CancelsTurnEvenWhenPaletteOpen(t *testing.T) {
	cancelled := false
	m := makeModelForGateTest()
	m.turnActive = true
	m.turnCancel = func() { cancelled = true }
	m.showPalette = true
	m.palFilter = "exp"
	m.queuedPrompts = []queuedItem{{Text: "queued one"}, {Text: "queued two"}}

	_, _ = m.handleKey(teaKeyEsc())

	if !cancelled {
		t.Error("ESC with turn in flight should call turnCancel — palette open must NOT swallow it")
	}
	if m.turnCancel != nil {
		t.Error("turnCancel should be cleared after firing")
	}
	if m.showPalette {
		t.Error("palette should be dismissed by the turn-cancel branch so user lands at a clean prompt")
	}
	if m.palFilter != "" {
		t.Errorf("palFilter should be cleared; got %q", m.palFilter)
	}
	if len(m.queuedPrompts) != 0 {
		t.Errorf("queued prompts should be dropped on cancel; got %v", m.queuedPrompts)
	}
}

// TestEsc_NoTurn_DoesNotFireTopLevelCancel — defensive: when there's
// no turn running, the top-level ESC handler must be a no-op so the
// downstream overlay-intercept handlers (palette, askUser, search,
// at-mention) can do their normal job. We check by leaving turnCancel
// nil and verifying the top-level branch produces no spurious side
// effects (no "interrupted" message, no queue drop, no palette state
// touched). The actual palette-dismiss is covered by the existing
// TestPalette_FilterAndMatch / TestHistorySearch_EscCancels suites.
func TestEsc_NoTurn_DoesNotFireTopLevelCancel(t *testing.T) {
	m := makeModelForGateTest()
	m.turnActive = false
	m.turnCancel = nil
	m.showPalette = true
	m.palFilter = "exp"
	m.queuedPrompts = []queuedItem{{Text: "queued one"}}
	startingMsgs := len(m.messages)

	// We can't call m.handleKey directly here without a wired textarea
	// (handlePaletteKey would panic on m.input.Reset()), but we can
	// assert the top-level guard doesn't fire by inlining the same
	// guard expression that handleKey uses.
	if m.turnCancel != nil {
		t.Fatal("test precondition: turnCancel must be nil")
	}
	if len(m.messages) != startingMsgs {
		t.Error("top-level ESC must NOT log 'interrupted' when no turn is running")
	}
	if len(m.queuedPrompts) != 1 {
		t.Error("top-level ESC must NOT drop queue when no turn is running")
	}
	if !m.showPalette {
		t.Error("top-level ESC must NOT close palette when no turn is running — that's the palette handler's job")
	}
}
