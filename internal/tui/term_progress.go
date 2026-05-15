package tui

// term_progress.go — OSC 9;4 terminal progress reporting.
//
// Modern terminals (iTerm2 3.6.6+, Ghostty 1.2.0+, ConEmu) parse the
// OSC 9;4 escape sequence and render a progress indicator on the tab
// title / taskbar — NOT in the terminal content area. metis emits
// "indeterminate" while compaction is in flight so the user has a
// visual signal the session is alive even when the in-pane spinner
// is paused or scrolled off-screen.
//
// Mirrors claude-code's services/compact path via Messages.tsx:603
// (`state = hasToolsInProgress ? 'indeterminate' : 'completed'`) and
// ink/useTerminalNotification.ts (`OSC 9;4;3;BEL`).
//
// Sequence reference:
//
//	OSC 9;4;<state>;<pct> BEL
//	  state: 0 = clear, 1 = running, 2 = error, 3 = indeterminate, 4 = paused
//
// Windows Terminal is explicitly excluded — it interprets OSC 9;4 as
// notification rather than progress, so emitting would pop toasts on
// every compaction.

import (
	"os"
	"strconv"
	"strings"
)

// ProgressState is the int code embedded in the OSC 9;4 sequence.
type ProgressState int

const (
	ProgressClear         ProgressState = 0
	ProgressRunning       ProgressState = 1
	ProgressIndeterminate ProgressState = 3
)

// terminalSupportsProgress reports whether the current terminal is
// known to interpret OSC 9;4 as a progress indicator.
//
//   - iTerm2 3.6.6+ (TERM_PROGRAM_VERSION)
//   - Ghostty 1.2.0+
//   - ConEmu (any version — env vars ConEmuANSI / ConEmuPID set)
//   - Excludes Windows Terminal (WT_SESSION) — uses the same sequence
//     for notifications.
//   - Requires stdout to be a TTY (no point emitting into a pipe).
func terminalSupportsProgress() bool {
	if !isStdoutTTY() {
		return false
	}
	if os.Getenv("WT_SESSION") != "" {
		return false // Windows Terminal — would pop notification toasts.
	}
	if os.Getenv("ConEmuANSI") != "" || os.Getenv("ConEmuPID") != "" {
		return true
	}
	prog := os.Getenv("TERM_PROGRAM")
	ver := os.Getenv("TERM_PROGRAM_VERSION")
	switch prog {
	case "iTerm.app":
		return semverGte(ver, 3, 6, 6)
	case "ghostty":
		return semverGte(ver, 1, 2, 0)
	}
	return false
}

// SetTerminalProgress writes the OSC 9;4 escape sequence to stdout.
// Silently no-ops when the terminal doesn't support the sequence so
// callers don't need to guard every invocation. Concurrent calls are
// fine — os.Stdout.Write is goroutine-safe.
//
// When metis runs inside tmux (TMUX env set), the OSC is wrapped in
// a DCS passthrough envelope (`\x1bPtmux;...\x1b\\`) so tmux forwards
// it to the outer terminal unmodified. Mirrors claude-code's
// wrapForMultiplexer (ink/termio/osc.ts:35). Requires the user to set
// `allow-passthrough on` in .tmux.conf for tmux 3.3+; otherwise tmux
// silently drops the DCS (no worse than emitting raw OSC).
func SetTerminalProgress(state ProgressState) {
	if !terminalSupportsProgress() {
		return
	}
	// OSC 9;4;<state>;<pct> BEL — pct omitted for indeterminate/clear.
	var s string
	if state == ProgressRunning {
		s = "\x1b]9;4;1;0\x07"
	} else {
		s = "\x1b]9;4;" + strconv.Itoa(int(state)) + ";\x07"
	}
	_, _ = os.Stdout.WriteString(wrapForMultiplexer(s))
}

// wrapForMultiplexer escapes a terminal escape sequence so it tunnels
// through tmux / GNU screen to the outer terminal. No-op when neither
// multiplexer is active.
func wrapForMultiplexer(sequence string) string {
	if os.Getenv("TMUX") != "" {
		// tmux DCS passthrough: each \x1b inside the payload must be
		// doubled, and the whole thing wrapped in `\x1bPtmux;...\x1b\\`.
		escaped := strings.ReplaceAll(sequence, "\x1b", "\x1b\x1b")
		return "\x1bPtmux;" + escaped + "\x1b\\"
	}
	if os.Getenv("STY") != "" {
		// GNU screen — simpler DCS, no escape doubling.
		return "\x1bP" + sequence + "\x1b\\"
	}
	return sequence
}

// isStdoutTTY returns true when stdout is connected to a terminal
// (not a pipe / redirected to file). Mirrors process.stdout.isTTY
// from CC's Node implementation; reading from os.Stdout directly via
// Stat() avoids the isatty dep.
func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// semverGte returns true when ver (a string like "3.6.10") is >=
// major.minor.patch. Forgiving parser: empty parts treated as 0,
// trailing pre-release suffixes ignored. False on unparseable input.
func semverGte(ver string, major, minor, patch int) bool {
	if ver == "" {
		return false
	}
	// Strip pre-release / build metadata.
	if i := strings.IndexAny(ver, "-+"); i >= 0 {
		ver = ver[:i]
	}
	parts := strings.Split(ver, ".")
	get := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return -1 // unparseable → fail the comparison
		}
		return n
	}
	a, b, c := get(0), get(1), get(2)
	if a < 0 || b < 0 || c < 0 {
		return false
	}
	if a != major {
		return a > major
	}
	if b != minor {
		return b > minor
	}
	return c >= patch
}
