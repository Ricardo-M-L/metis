package tui

import (
	"os"

	"golang.org/x/term"
)

// termSavedState is an opaque alias for the saved termios snapshot.
// Aliased so that other files in the package (Model in tui.go,
// hard-exit path in keybind_main.go) can carry a pointer to it
// without each importing golang.org/x/term — the import stays
// confined here and in tui.go's RunTUI.
type termSavedState = term.State

// resetTerminal sends the full set of "go back to a sane terminal"
// escape sequences plus a termios sane-restore. Mirrors what `reset(1)`
// does for the modes a TUI typically toggles. We call this as a defer
// in RunTUI so the user always lands back in a usable shell, even if
// bubbletea v2's cleanup misses one of these (v2.0.6 is an alpha and
// occasionally drops kitty-keyboard disable on quit, leaving the shell
// echoing `^[[99;5u` for Ctrl+C).
//
// Sending these is idempotent: if bubbletea already cleaned up, the
// terminal will just no-op the second copy.
//
// Sequences (in send order):
//
//	CSI < u            — disable kitty keyboard protocol
//	CSI > 4 ; 0 m      — disable modifyOtherKeys (xterm)
//	CSI ? 2004 l       — disable bracketed paste
//	CSI ? 1000 ; 1002 ; 1003 ; 1006 l — disable all mouse tracking modes
//	CSI ? 25 h         — show cursor (in case bubbletea hid it)
//	CSI ? 1049 l       — exit alternate screen
//	CSI 0 m            — reset SGR (colors / bold / etc.)
//
// Followed by `term.Restore` of the saved termios snapshot if we have
// one. Without that, raw mode (no echo, no canonical) leaks back.
func resetTerminal(saved *term.State) {
	const reset = "" +
		"\x1b[<u" +
		"\x1b[>4;0m" +
		"\x1b[?2004l" +
		"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l" +
		"\x1b[?25h" +
		"\x1b[?1049l" +
		"\x1b[0m"
	// Stderr is the right channel: bubbletea v2 also writes terminal
	// control to stderr, so the shell prompt that comes next sees the
	// resets in the same stream-order. Some terminals (alacritty) buffer
	// stdout vs stderr separately; mixing them caused intermittent
	// drops in v2.0.6.
	_, _ = os.Stderr.WriteString(reset)
	if saved != nil {
		_ = term.Restore(int(os.Stdin.Fd()), saved)
	}
	// Drain any in-flight bytes still sitting in the stdin buffer.
	// The disable sequences above tell the terminal to STOP emitting
	// mouse reports, but events the terminal already queued *before*
	// the disable propagates will still arrive — and once we exit,
	// the shell prints them as raw text ("81;113;41M81;113;41M...").
	// The 2026-05-15 fix wired resetTerminal into the hard-exit path
	// but didn't flush the buffer. User repro 2026-05-16: hovering the
	// mouse during Ctrl+C×2 reliably produced the pollution.
	//
	// Skipped when saved==nil (no real TTY — CI / piped) since there's
	// no stdin to drain in that case anyway.
	if saved != nil {
		_ = drainStdin(int(os.Stdin.Fd()))
	}
}

// snapshotTerminal captures the current termios so resetTerminal can
// restore it after RunTUI returns. Returns nil when stdin isn't a TTY
// (CI / piped input) — resetTerminal then skips the termios restore.
func snapshotTerminal() *term.State {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	st, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		return nil
	}
	return st
}
