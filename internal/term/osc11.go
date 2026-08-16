//go:build !windows

package term

// osc11.go — OSC 11 ("dynamic colors") query for the terminal's
// current background color. Used at startup to pick a sensible
// default theme without forcing the user to set METIS_THEME.
//
// Protocol (ECMA-48 / xterm dynamic colors):
//
//   client → terminal:   ESC ] 11 ; ? BEL
//   terminal → client:   ESC ] 11 ; rgb:RRRR/GGGG/BBBB BEL    (or ST)
//
// Most modern terminals respond: iTerm2, kitty, WezTerm, Ghostty,
// Alacritty, Apple Terminal (10.12+), Windows Terminal, gnome-terminal,
// Konsole. tmux passes the query through if `set-option allow-passthrough on`.
// Some headless / pipe-redirected contexts won't respond — we wait
// 200ms then give up and fall back to darkTheme.
//
// Why we don't use `COLORFGBG` env var: it's set by rxvt-unicode and
// few others, and gets cached by the shell so it lies after the user
// flips the terminal theme mid-session. OSC11 always reflects the
// live state.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// queryOSC11Timeout caps the total time spent waiting for the
// terminal's response. 200ms is conservative: a real terminal answers
// in single-digit ms; anything longer is a non-responding context
// where we want to fall back fast.
const queryOSC11Timeout = 200 * time.Millisecond

// DetectTerminalBackground sends the OSC 11 query to /dev/tty and
// returns (isLight, ok). ok=false means we couldn't determine — the
// caller should keep its existing default. Light/dark is decided by
// luminance using the standard ITU-R BT.601 weights:
//
//	Y = 0.299·R + 0.587·G + 0.114·B
//
// > 0.5 of full-scale → light; ≤ 0.5 → dark.
func DetectTerminalBackground() (isLight bool, ok bool) {
	// 1. open /dev/tty in NON-BLOCKING mode.
	// Without O_NONBLOCK, f.Read blocks forever when another process
	// (e.g. metis itself) already owns this TTY in raw mode and
	// consumes our OSC 11 response.
	// (2026-08-16 regression: go test ./internal/tui hung for
	// minutes because the TTY was already claimed.)
	tty, err := os.OpenFile("/dev/tty", unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, false
	}
	defer tty.Close()

	fd := int(tty.Fd())
	if !term.IsTerminal(fd) {
		return false, false
	}

	// HARD GUARD: on macOS, if another process owns this TTY in raw
	// mode, opening /dev/tty yields a POLLNVAL fd (revents=0x20).
	// unix.Read on such a fd blocks indefinitely even with
	// O_NONBLOCK — the kernel doesn't return EAGAIN. Detect this
	// early and bail out. We check POLLERR|POLLHUP|POLLNVAL because
	// all three mean "fd is not usable for reading."
	pfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, pollErr := unix.Poll(pfds, 1) // 1ms probe
	if pollErr != nil && pollErr != unix.EINTR {
		return false, false
	}
	if pfds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return false, false
	}

	// Switch into raw mode so the terminal's response isn't echoed
	// back to the screen as visible `^[]11;rgb:...^G` characters
	// before bubbletea enters alt-screen.
	if oldState, terr := term.MakeRaw(fd); terr == nil {
		defer term.Restore(fd, oldState)
	}

	// 2. send OSC 11 query.
	if _, err := tty.WriteString("\x1b]11;?\x07"); err != nil {
		return false, false
	}

	// 3. read response with a hard poll-based timeout.
	buf, err := readWithPoll(fd, queryOSC11Timeout, 64)
	if err != nil {
		return false, false
	}
	r, g, b, perr := parseOSC11Response(buf)
	if perr != nil {
		return false, false
	}
	y := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	return y > 0.5, true
}

// readWithPoll reads from an fd using unix.Poll for a hard timeout.
// We use unix.Poll rather than a Go timer + select because the
// runtime cannot interrupt a syscall blocked in read(2). Poll returns
// within pollMs, giving us a reliable timeout. We also check for
// POLLNVAL (invalid open) which on macOS shared TTY makes unix.Read
// block forever.
func readWithPoll(fd int, dur time.Duration, maxBytes int) ([]byte, error) {
	buf := make([]byte, 0, maxBytes)
	tmp := make([]byte, 1)
	deadline := time.Now().Add(dur)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errors.New("osc11: timeout")
		}

		pfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		pollMs := int(remaining.Milliseconds())
		if pollMs < 1 {
			pollMs = 1
		}
		_, pollErr := unix.Poll(pfds, pollMs)
		if pollErr != nil && pollErr != unix.EINTR {
			return nil, pollErr
		}
		if time.Now().After(deadline) {
			return nil, errors.New("osc11: timeout")
		}
		if pfds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return nil, errors.New("osc11: fd error")
		}
		if pfds[0].Revents&unix.POLLIN == 0 {
			return nil, errors.New("osc11: timeout")
		}

		n, readErr := unix.Read(fd, tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if tmp[0] == 0x07 {
				return buf, nil
			}
			if len(buf) >= 2 && buf[len(buf)-2] == 0x1b && buf[len(buf)-1] == '\\' {
				return buf, nil
			}
			if len(buf) >= maxBytes {
				return buf, errors.New("osc11: response too long")
			}
		}
		if readErr == unix.EAGAIN || readErr == unix.EWOULDBLOCK {
			continue
		}
		if readErr != nil {
			return buf, readErr
		}
	}
}

// parseOSC11Response extracts (r, g, b) as 16-bit values normalized
// to [0,1] from a response of the form:
//
//	ESC ] 11 ; rgb:RRRR/GGGG/BBBB BEL
//
// xterm sometimes uses 4 hex digits per channel (16-bit), sometimes
// 2 (8-bit). We accept both.
func parseOSC11Response(buf []byte) (r, g, b float64, err error) {
	idx := bytes.Index(buf, []byte("rgb:"))
	if idx < 0 {
		return 0, 0, 0, errors.New("osc11: no rgb prefix in response")
	}
	body := string(buf[idx+len("rgb:"):])
	// Strip trailing terminator (BEL or ESC\).
	body = strings.TrimRight(body, "\x07\\")
	body = strings.TrimSuffix(body, "\x1b")
	parts := strings.Split(body, "/")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("osc11: expected 3 channels, got %d in %q", len(parts), body)
	}
	parse := func(s string) (float64, error) {
		s = strings.TrimSpace(s)
		// keep only hex chars
		hex := make([]byte, 0, len(s))
		for i := 0; i < len(s); i++ {
			c := s[i]
			if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
				hex = append(hex, c)
			}
		}
		if len(hex) == 0 || len(hex) > 4 {
			return 0, fmt.Errorf("invalid hex channel %q", s)
		}
		v, e := strconv.ParseUint(string(hex), 16, 32)
		if e != nil {
			return 0, e
		}
		switch len(hex) {
		case 1:
			return float64(v) / 15.0, nil
		case 2:
			return float64(v) / 255.0, nil
		case 3:
			return float64(v) / 4095.0, nil
		case 4:
			return float64(v) / 65535.0, nil
		}
		return 0, fmt.Errorf("unreachable")
	}
	if r, err = parse(parts[0]); err != nil {
		return 0, 0, 0, err
	}
	if g, err = parse(parts[1]); err != nil {
		return 0, 0, 0, err
	}
	if b, err = parse(parts[2]); err != nil {
		return 0, 0, 0, err
	}
	return r, g, b, nil
}
