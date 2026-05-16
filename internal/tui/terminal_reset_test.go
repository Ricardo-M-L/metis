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
	"time"
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

// TestDrainStdin_DiscardsBufferedBytes — pins the 2026-05-16 stdin-drain
// fix: after the disable escape sequences are written, any mouse-report
// bytes already queued in stdin must be swallowed so the shell doesn't
// echo them as raw text ("81;113;41M81;113;41M..."). Uses a pipe stand-in
// for stdin because real /dev/stdin isn't writable in the test process.
func TestDrainStdin_DiscardsBufferedBytes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	// Stuff the pipe with realistic SGR mouse-report bytes — the exact
	// shape the user saw in the bug repro image.
	payload := strings.Repeat("\x1b[<81;113;41M", 40)
	if _, err := w.Write([]byte(payload)); err != nil {
		t.Fatalf("seed pipe: %v", err)
	}

	drained := drainStdin(int(r.Fd()))
	// drainStdin reports how much it swallowed. The exact byte count
	// can vary slightly between platforms (line buffering, pipe-page
	// chunking) — what matters is that it pulled a meaningful chunk
	// of the payload, not zero. Compare against the prefill size so
	// the test stays sensitive to "drain did nothing" regressions
	// without being brittle to small chunking differences.
	if drained < len(payload)/2 {
		t.Errorf("drainStdin read only %d/%d bytes — should have swallowed the bulk of the prefilled mouse reports", drained, len(payload))
	}
}

// TestDrainStdin_NoopOnEmptyBuffer — must return promptly when there's
// nothing to drain. Without nonblocking mode this would hang forever on
// an empty pipe, since the read would park waiting for input that's
// never coming. Verifies the wall-clock floor stays bounded.
func TestDrainStdin_NoopOnEmptyBuffer(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	start := time.Now()
	drainStdin(int(r.Fd()))
	elapsed := time.Since(start)

	// 30ms pre-sleep + the immediate-return nonblocking read = bounded.
	// 100ms gives us comfortable headroom while still failing fast if
	// someone removes the SetNonblock call and the test starts hanging.
	if elapsed > 100*time.Millisecond {
		t.Errorf("drainStdin on empty buffer took %v; should be under 100ms (the SetNonblock guard is missing or broken)", elapsed)
	}
}
