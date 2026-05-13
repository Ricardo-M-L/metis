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
	// 1. open /dev/tty as read+write. We deliberately don't reuse
	// os.Stdin / os.Stdout — those may be pipes (`metis | less`) or
	// captured by the test harness, in which case the OSC query
	// would never reach a real TTY.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, false
	}
	defer tty.Close()

	// Switch the TTY into raw mode for the duration of the probe so the
	// terminal's response isn't echoed back to the screen as visible
	// `^[]11;rgb:...^G` characters before bubbletea enters alt-screen.
	// Without this, on cooked-mode startup (the default before any TUI
	// touches termios) the OSC11 reply leaks onto the user's prompt
	// area for one frame — looks like a string of garbage characters
	// before the welcome card paints.
	fd := int(tty.Fd())
	if term.IsTerminal(fd) {
		if oldState, terr := term.MakeRaw(fd); terr == nil {
			defer term.Restore(fd, oldState)
		}
	}

	// 2. send OSC11 ?-query. We use BEL terminator (\x07) — wider
	// support than ST (ESC \). Terminals that reject BEL would also
	// reject ST in our experience; this isn't worth a probe-and-retry.
	if _, err := tty.WriteString("\x1b]11;?\x07"); err != nil {
		return false, false
	}

	// 3. read response with deadline. We can't use io.ReadAll — that
	// would block forever on a non-responding TTY. SetReadDeadline
	// only works on files where the underlying FD supports it; on
	// macOS BSD a TTY does support it. As a backstop we also run the
	// read in a goroutine with a select.
	buf, err := readWithTimeout(tty, queryOSC11Timeout, 64)
	if err != nil {
		return false, false
	}
	r, g, b, perr := parseOSC11Response(buf)
	if perr != nil {
		return false, false
	}
	// ITU-R BT.601 luma. We use float64 once and discard.
	y := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	return y > 0.5, true
}

// readWithTimeout drains stdin until we see the BEL or ST terminator,
// or we hit `dur`. Reads in a goroutine so the timeout select can
// preempt a syscall that doesn't honor SetReadDeadline (rare but
// happens in screen / under specific tmux configs).
func readWithTimeout(f *os.File, dur time.Duration, maxBytes int) ([]byte, error) {
	type result struct {
		buf []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 0, 64)
		tmp := make([]byte, 1)
		_ = f.SetReadDeadline(time.Now().Add(dur))
		defer f.SetReadDeadline(time.Time{}) // clear so subsequent reads aren't truncated
		for {
			n, err := f.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				// terminators: BEL (0x07) or ST (ESC \).
				if tmp[0] == 0x07 {
					ch <- result{buf, nil}
					return
				}
				if len(buf) >= 2 && buf[len(buf)-2] == 0x1b && buf[len(buf)-1] == '\\' {
					ch <- result{buf, nil}
					return
				}
				if len(buf) >= maxBytes {
					ch <- result{buf, errors.New("osc11: response too long")}
					return
				}
			}
			if err != nil {
				ch <- result{buf, err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.buf, r.err
	case <-time.After(dur + 50*time.Millisecond):
		return nil, errors.New("osc11: timeout")
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
		// scale to [0,1]: 4-hex digit (max 0xffff) → /65535;
		// 2-hex digit → /255.
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
