// Package runtime — early input capture during cold start.
//
// claude-code's utils/earlyInput.ts buffers terminal keystrokes during
// the ~200-500ms Node.js module-load phase, then replays them after
// Ink's input handler is ready. The win in CC is significant because
// Node startup is slow.
//
// In Go, metis cold-starts in ~50ms. The window is much smaller, but
// non-zero — between `metis chat` exec and bubbletea's first read, a
// fast typist can still drop characters that the terminal echoes to a
// phantom prompt before the TUI paints over it.
//
// Implementation: a tiny io.Reader wrapper that prepends an in-memory
// buffer (filled during init by a goroutine reading raw stdin) in
// front of os.Stdin. bubbletea accepts io.Reader via tea.WithInput, so
// we hand it our wrapped reader and it transparently sees the
// pre-buffered bytes followed by live input.
package runtime

import (
	"bytes"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// EarlyInput buffers up to N bytes of stdin between cold start and the
// moment bubbletea is ready to read. Wrap stdin via Reader() and pass
// the result to tea.WithInput. Stop() must be called before tea takes
// over (otherwise both compete for the same fd).
type EarlyInput struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	stopped     bool
	prevMode    *term.State
	stdinFile   *os.File
	captureDone chan struct{}
}

// NewEarlyInput starts a goroutine that reads raw stdin into an
// internal buffer until Stop() is called. Returns nil (and is a no-op)
// when stdin isn't a TTY — non-interactive callers (pipe, expect, CI)
// don't need this and would actively break if we tried to set raw mode.
//
// Why we cap the read at 4 KiB: a real human can't type more than ~50
// chars in the cold-start window. The cap prevents a paste-bombed
// stdin from holding the buffer indefinitely if Stop() is delayed.
func NewEarlyInput() *EarlyInput {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	prev, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil // can't take ownership — silently skip the optimization
	}
	ei := &EarlyInput{
		prevMode:    prev,
		stdinFile:   os.Stdin,
		captureDone: make(chan struct{}),
	}
	go ei.captureLoop()
	return ei
}

func (e *EarlyInput) captureLoop() {
	defer close(e.captureDone)
	// Bound the inner read so Stop() doesn't block on a Read syscall
	// indefinitely. We poll with a tight timeout; each iteration reads
	// available bytes (raw mode = no line buffering) into our buffer.
	// 4 KiB total cap: a paste of 4K bytes overflows and the rest goes
	// to the live tea input — no data loss, just no early capture.
	buf := make([]byte, 256)
	const cap = 4096
	for {
		e.mu.Lock()
		stopped := e.stopped
		bufLen := e.buf.Len()
		e.mu.Unlock()
		if stopped {
			return
		}
		if bufLen >= cap {
			// Buffer full — wait for Stop() but don't keep reading.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		// Use a syscall-level non-blocking read by polling. The tradeoff
		// is some idle CPU, but the goroutine only runs for ~50ms so
		// it's negligible. SetReadDeadline isn't supported on os.Stdin
		// across all platforms; this is the portable path.
		// Set a soft deadline to wake up periodically and re-check stop.
		_ = e.stdinFile.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
		n, err := e.stdinFile.Read(buf)
		_ = e.stdinFile.SetReadDeadline(time.Time{})
		if n > 0 {
			e.mu.Lock()
			room := cap - e.buf.Len()
			if n > room {
				n = room
			}
			e.buf.Write(buf[:n])
			e.mu.Unlock()
		}
		if err != nil && !isTimeoutErr(err) {
			return
		}
	}
}

// Stop terminates the capture goroutine and restores the terminal mode.
// Idempotent — safe to call multiple times. The trust-prompt path may
// call it before bubbletea hand-off does, so a second call is a no-op.
func (e *EarlyInput) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	already := e.stopped
	e.stopped = true
	e.mu.Unlock()
	if already {
		return
	}
	<-e.captureDone
	if e.prevMode != nil {
		_ = term.Restore(int(e.stdinFile.Fd()), e.prevMode)
		e.prevMode = nil
	}
}

// Reader returns an io.Reader that yields the captured pre-bytes
// FIRST, then forwards live reads to os.Stdin. Pass this to
// tea.WithInput. Safe to call after Stop().
func (e *EarlyInput) Reader() io.Reader {
	if e == nil {
		return os.Stdin
	}
	e.mu.Lock()
	pre := e.buf.Bytes()
	e.mu.Unlock()
	if len(pre) == 0 {
		return os.Stdin
	}
	return io.MultiReader(bytes.NewReader(pre), os.Stdin)
}

// CapturedBytes exposes the captured buffer for diagnostics / tests.
// Returns a copy so callers can't mutate the internal buffer.
func (e *EarlyInput) CapturedBytes() []byte {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]byte, e.buf.Len())
	copy(out, e.buf.Bytes())
	return out
}

// isTimeoutErr matches os.PathError{Err: errors.New("...timeout...")}.
// We don't import the syscall-specific error because behavior differs
// per OS; string match is the portable detection.
func isTimeoutErr(err error) bool {
	type timeout interface{ Timeout() bool }
	if te, ok := err.(timeout); ok && te.Timeout() {
		return true
	}
	return false
}
