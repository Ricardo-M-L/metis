package tui

// terminal_reset_test.go pins the 2026-05-15 fix for the Ctrl+C×2
// hard-exit terminal pollution.
//
// Symptom: after Ctrl+C×2 in the TUI, the user's shell would echo
// raw SGR mouse reports like `<81;121;39M` on every mouse motion,
// because resetTerminal (deferred in RunTUI) was skipped by the
// 800ms-fallback os.Exit and the terminal stayed in mouse-tracking
// mode (`\x1b[?1006h` still set).
//
// The fix wires resetTerminal into the hard-exit goroutine. This test
// guards the disable sequences are actually emitted when resetTerminal
// runs, so a future refactor doesn't silently drop them.

import (
	"os"
	"strings"
	"sync"
	"testing"
)

// captureStderr redirects os.Stderr through a pipe for the duration
// of fn, then returns whatever was written. Concurrency-safe for our
// single-test use because we Lock around the global swap.
var stderrSwapMu sync.Mutex

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	stderrSwapMu.Lock()
	defer stderrSwapMu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = orig
	})

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, _ := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if n == 0 {
				break
			}
		}
		done <- sb.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	os.Stderr = orig
	return out
}

func TestResetTerminal_EmitsMouseDisableSequences(t *testing.T) {
	out := captureStderr(t, func() {
		resetTerminal(nil) // nil saved is the hard-exit fallback path
	})
	// SGR 1006 mouse mode is the one whose enable sequence
	// (\x1b[?1006h) was leaving the shell echoing `<col;row;buttonM`.
	for _, want := range []string{
		"\x1b[?1006l", // disable SGR mouse — the bug-causing one
		"\x1b[?1000l", // disable basic mouse
		"\x1b[?1002l", // disable button-event mouse
		"\x1b[?1003l", // disable any-event mouse
		"\x1b[?1049l", // exit alternate screen
		"\x1b[?25h",   // show cursor
		"\x1b[?2004l", // disable bracketed paste
		"\x1b[0m",     // SGR reset
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing escape sequence %q in resetTerminal output", want)
		}
	}
}
