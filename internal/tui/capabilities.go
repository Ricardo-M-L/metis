package tui

// capabilities.go — terminal capability detection. Single source of
// truth for "does this terminal support feature X" used by OSC 52
// (clipboard), OSC 9;4 (progress), OSC 8 (hyperlinks), OSC 11 (bg
// query), tmux DCS passthrough, etc.
//
// Strategy: read $TERM_PROGRAM / $WT_SESSION / $LC_TERMINAL / $TERM
// and check version gates where the feature support is post-version
// only (iTerm2 ≥ 3.6.6 for OSC 9;4, Ghostty ≥ 1.2.0 for sync output,
// VTE ≥ 0.68 for synchronized output mode). Mirrors claude-code-
// sourcemap `restored-src/src/ink/terminal.ts`'s isProgressReporting
// Available / supportsExtendedKeys / isSynchronizedOutputSupported.
//
// Why centralize: previously detection was scattered across
// notify.go (TERM_PROGRAM switch), themes.go (KITTY_WINDOW_ID), and
// osc11.go (CI / NO_COLOR). Three forks meant a fix had to land in
// three places. This file is THE detection layer; everywhere else
// imports.
//
// All exported functions are pure: they read env once per call and
// don't cache. That keeps tests easy (t.Setenv reflects immediately)
// at a tiny cost in bench scenarios — the per-call cost is a few
// os.LookupEnv calls, sub-microsecond.

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// TerminalKind classifies the host terminal emulator. Detection
// hierarchy: TERM_PROGRAM > LC_TERMINAL > KITTY_WINDOW_ID > WT_SESSION
// > TERM substring match. Returns TerminalUnknown when none match.
type TerminalKind int

const (
	TerminalUnknown TerminalKind = iota
	TerminalITerm2
	TerminalAppleTerminal
	TerminalGhostty
	TerminalKitty
	TerminalWezTerm
	TerminalAlacritty
	TerminalRio
	TerminalWindowsTerminal
	TerminalVSCode
	TerminalConEmu
)

// String returns the canonical lowercase name. Used in debug dumps
// and analytics; do not rely on stability across versions.
func (t TerminalKind) String() string {
	switch t {
	case TerminalITerm2:
		return "iterm2"
	case TerminalAppleTerminal:
		return "apple_terminal"
	case TerminalGhostty:
		return "ghostty"
	case TerminalKitty:
		return "kitty"
	case TerminalWezTerm:
		return "wezterm"
	case TerminalAlacritty:
		return "alacritty"
	case TerminalRio:
		return "rio"
	case TerminalWindowsTerminal:
		return "windows_terminal"
	case TerminalVSCode:
		return "vscode"
	case TerminalConEmu:
		return "conemu"
	}
	return "unknown"
}

// DetectTerminal reads env vars and returns the host terminal kind.
//
// LC_TERMINAL is checked AFTER TERM_PROGRAM because tmux preserves
// LC_TERMINAL but not TERM_PROGRAM (so when in tmux, LC_TERMINAL
// reveals the outer terminal). claude-code-sourcemap does the same.
func DetectTerminal() TerminalKind {
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return TerminalITerm2
	case "Apple_Terminal":
		return TerminalAppleTerminal
	case "ghostty":
		return TerminalGhostty
	case "WezTerm":
		return TerminalWezTerm
	case "vscode":
		return TerminalVSCode
	case "rio":
		return TerminalRio
	}
	// LC_TERMINAL: preserved across tmux, useful when TERM_PROGRAM is
	// stripped. Values like "iTerm2" / "WezTerm".
	switch strings.ToLower(os.Getenv("LC_TERMINAL")) {
	case "iterm2":
		return TerminalITerm2
	case "wezterm":
		return TerminalWezTerm
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" || strings.Contains(os.Getenv("TERM"), "kitty") {
		return TerminalKitty
	}
	if os.Getenv("ALACRITTY_LOG") != "" || strings.Contains(os.Getenv("TERM"), "alacritty") {
		return TerminalAlacritty
	}
	if os.Getenv("WT_SESSION") != "" {
		return TerminalWindowsTerminal
	}
	if os.Getenv("ConEmuPID") != "" || os.Getenv("ConEmuANSI") != "" {
		return TerminalConEmu
	}
	return TerminalUnknown
}

// IsAppleTerminal is a shortcut for DetectTerminal() ==
// TerminalAppleTerminal. Used to gate features that misbehave on the
// stock macOS Terminal.app (e.g. OSC 11 query — Apple Terminal echos
// the query verbatim instead of replying, then streams garbled
// bytes; OSC 9;4 progress — silently dropped; shift+tab — sometimes
// captured by app).
func IsAppleTerminal() bool {
	return DetectTerminal() == TerminalAppleTerminal
}

// IsTmux reports whether we're running inside tmux ($TMUX is set by
// tmux on attach). Used to wrap OSC sequences in DCS passthrough so
// the outer terminal sees them.
func IsTmux() bool {
	return os.Getenv("TMUX") != ""
}

// IsScreen reports whether we're running inside GNU screen ($STY is
// set by screen). screen has its own DCS passthrough syntax similar
// to tmux but with different escape rules; for now we treat
// IsTmux() || IsScreen() as "needs passthrough".
func IsScreen() bool {
	return os.Getenv("STY") != ""
}

// IsSSH reports whether the current session is an SSH login.
//
// We check SSH_CONNECTION, NOT SSH_TTY: claude-code-sourcemap
// specifically uses SSH_CONNECTION because SSH_TTY is preserved
// inside tmux even when the user later disconnects from SSH and
// re-attaches locally. SSH_CONNECTION is unset when tmux is detached
// from SSH, giving a more accurate "is currently SSH" signal.
func IsSSH() bool {
	return os.Getenv("SSH_CONNECTION") != ""
}

// SupportsProgressBar reports whether the terminal supports the
// OSC 9;4 progress sequence (the iTerm2-pioneered indeterminate /
// percentage progress that lights up the dock badge / taskbar /
// tab indicator).
//
// Anthropic version-gates: iTerm2 ≥ 3.6.6, Ghostty ≥ 1.2.0. WT
// supports unconditionally. Other terminals quietly drop the
// sequence; we still return false for them rather than send-and-pray
// because some old terminals print the sequence as garbled text.
func SupportsProgressBar() bool {
	if !IsTerminal() {
		return false
	}
	if os.Getenv("WT_SESSION") != "" {
		return true
	}
	if os.Getenv("ConEmuANSI") != "" || os.Getenv("ConEmuPID") != "" {
		return true
	}
	switch DetectTerminal() {
	case TerminalITerm2:
		// iTerm2 added OSC 9;4 in 3.6.6.
		return atLeast(os.Getenv("TERM_PROGRAM_VERSION"), 3, 6, 6)
	case TerminalGhostty:
		// Ghostty added OSC 9;4 in 1.2.0.
		return atLeast(os.Getenv("TERM_PROGRAM_VERSION"), 1, 2, 0)
	case TerminalRio:
		return true
	}
	return false
}

// SupportsHyperlink reports whether the terminal supports OSC 8
// `\x1b]8;<params>;<url>\x07<text>\x1b]8;;\x07`. Most modern
// terminals do; old Apple Terminal and conhost don't.
//
// Conservative: we only return true for terminals known to support
// it. False on unknown so we don't print the URL twice (some
// terminals render the bare sequence as visible noise + the text).
func SupportsHyperlink() bool {
	if !IsTerminal() {
		return false
	}
	switch DetectTerminal() {
	case TerminalITerm2, TerminalGhostty, TerminalKitty, TerminalWezTerm,
		TerminalAlacritty, TerminalRio, TerminalWindowsTerminal,
		TerminalVSCode:
		return true
	}
	return false
}

// PreferSTTerminator reports whether the terminal expects ST
// (`\x1b\\`) instead of BEL (`\x07`) to terminate OSC sequences.
// Kitty famously requires ST and silently drops OSC sequences
// terminated with BEL; iTerm2 and most others accept both.
//
// Default: BEL (most compatible). Override only for known-strict
// terminals.
func PreferSTTerminator() bool {
	return DetectTerminal() == TerminalKitty
}

// IsTerminal reports whether stderr is a TTY. Most capability
// queries are meaningless when stderr is redirected to a file or
// pipe (e.g. CI runs, `metis run > out.txt`).
//
// We probe stderr (not stdout) because stdout in `metis run` is
// often piped to consumers — but stderr is the channel where
// progress / hyperlinks / status info goes, and that's what
// capability gating cares about.
func IsTerminal() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// atLeast parses a "MAJOR.MINOR.PATCH[.X][-pre]" version string and
// reports whether it's ≥ the given major/minor/patch. Empty / un-
// parseable versions return false (conservative: assume the feature
// isn't supported).
//
// Tolerates leading 'v' (`v3.6.6`) and trailing junk after PATCH
// (`3.6.6-beta1`).
func atLeast(v string, major, minor, patch int) bool {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 4)
	if len(parts) < 3 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	// Patch may have trailing junk: "6-beta1" → 6.
	patchStr := parts[2]
	for i, r := range patchStr {
		if r < '0' || r > '9' {
			patchStr = patchStr[:i]
			break
		}
	}
	gotPatch, err := strconv.Atoi(patchStr)
	if err != nil {
		return false
	}
	if gotMajor != major {
		return gotMajor > major
	}
	if gotMinor != minor {
		return gotMinor > minor
	}
	return gotPatch >= patch
}

// isWindows is exposed for tests that need to verify Windows-specific
// gating. Not used in production code paths.
func isWindows() bool { return runtime.GOOS == "windows" }
