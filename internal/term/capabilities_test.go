package term

import "testing"

// clearTermEnv resets every var the detector reads, so a developer's
// real iTerm2 / tmux session doesn't leak into a "no terminal" case.
func clearTermEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"TERM_PROGRAM", "TERM_PROGRAM_VERSION", "LC_TERMINAL",
		"KITTY_WINDOW_ID", "ALACRITTY_LOG", "WT_SESSION", "ConEmuPID",
		"ConEmuANSI", "TMUX", "STY", "SSH_CONNECTION", "SSH_TTY", "TERM",
	} {
		t.Setenv(k, "")
	}
}

func TestDetectTerminal_TermProgramSwitch(t *testing.T) {
	cases := map[string]TerminalKind{
		"iTerm.app":      TerminalITerm2,
		"Apple_Terminal": TerminalAppleTerminal,
		"ghostty":        TerminalGhostty,
		"WezTerm":        TerminalWezTerm,
		"vscode":         TerminalVSCode,
		"rio":            TerminalRio,
	}
	for tp, want := range cases {
		t.Run(tp, func(t *testing.T) {
			clearTermEnv(t)
			t.Setenv("TERM_PROGRAM", tp)
			if got := DetectTerminal(); got != want {
				t.Errorf("TERM_PROGRAM=%q → %v, want %v", tp, got, want)
			}
		})
	}
}

func TestDetectTerminal_LCTerminalFallback(t *testing.T) {
	// LC_TERMINAL is preserved by tmux. Should be checked when
	// TERM_PROGRAM is missing or empty.
	clearTermEnv(t)
	t.Setenv("LC_TERMINAL", "iTerm2")
	if DetectTerminal() != TerminalITerm2 {
		t.Errorf("expected iTerm2 from LC_TERMINAL fallback")
	}
}

func TestDetectTerminal_TermProgramWinsOverLCTerminal(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("LC_TERMINAL", "iTerm2")
	if DetectTerminal() != TerminalGhostty {
		t.Errorf("TERM_PROGRAM should win over LC_TERMINAL")
	}
}

func TestDetectTerminal_KittyByWindowID(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("KITTY_WINDOW_ID", "42")
	if DetectTerminal() != TerminalKitty {
		t.Error("KITTY_WINDOW_ID should detect Kitty")
	}
}

func TestDetectTerminal_KittyByTERM(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("TERM", "xterm-kitty")
	if DetectTerminal() != TerminalKitty {
		t.Error("TERM=xterm-kitty should detect Kitty")
	}
}

func TestDetectTerminal_AlacrittyByLog(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("ALACRITTY_LOG", "/tmp/alacritty.log")
	if DetectTerminal() != TerminalAlacritty {
		t.Error("ALACRITTY_LOG should detect Alacritty")
	}
}

func TestDetectTerminal_WTSession(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("WT_SESSION", "abc-123")
	if DetectTerminal() != TerminalWindowsTerminal {
		t.Error("WT_SESSION should detect Windows Terminal")
	}
}

func TestDetectTerminal_ConEmu(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("ConEmuPID", "1234")
	if DetectTerminal() != TerminalConEmu {
		t.Error("ConEmuPID should detect ConEmu")
	}
}

func TestDetectTerminal_Unknown(t *testing.T) {
	clearTermEnv(t)
	if DetectTerminal() != TerminalUnknown {
		t.Errorf("no markers → TerminalUnknown")
	}
}

func TestIsTmux(t *testing.T) {
	clearTermEnv(t)
	if IsTmux() {
		t.Error("no TMUX → false")
	}
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	if !IsTmux() {
		t.Error("TMUX set → true")
	}
}

func TestIsScreen(t *testing.T) {
	clearTermEnv(t)
	if IsScreen() {
		t.Error("no STY → false")
	}
	t.Setenv("STY", "12345.pts-0.host")
	if !IsScreen() {
		t.Error("STY set → true")
	}
}

func TestIsSSH_UsesConnectionNotTTY(t *testing.T) {
	clearTermEnv(t)
	if IsSSH() {
		t.Error("no env → false")
	}
	// SSH_TTY alone shouldn't trigger — it persists in tmux.
	t.Setenv("SSH_TTY", "/dev/pts/0")
	if IsSSH() {
		t.Error("SSH_TTY alone must not signal active SSH (claude-code-sourcemap rationale)")
	}
	t.Setenv("SSH_CONNECTION", "1.2.3.4 5000 1.2.3.5 22")
	if !IsSSH() {
		t.Error("SSH_CONNECTION should signal SSH")
	}
}

func TestSupportsProgressBar_iTerm2VersionGate(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY in test env")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")

	t.Setenv("TERM_PROGRAM_VERSION", "3.6.5")
	if SupportsProgressBar() {
		t.Error("iTerm2 3.6.5 < 3.6.6 → unsupported")
	}
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.6")
	if !SupportsProgressBar() {
		t.Error("iTerm2 3.6.6 → supported")
	}
	t.Setenv("TERM_PROGRAM_VERSION", "3.7.0")
	if !SupportsProgressBar() {
		t.Error("iTerm2 3.7.0 → supported")
	}
}

func TestSupportsProgressBar_GhosttyVersionGate(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY in test env")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "ghostty")

	t.Setenv("TERM_PROGRAM_VERSION", "1.1.9")
	if SupportsProgressBar() {
		t.Error("Ghostty 1.1.9 < 1.2.0 → unsupported")
	}
	t.Setenv("TERM_PROGRAM_VERSION", "1.2.0")
	if !SupportsProgressBar() {
		t.Error("Ghostty 1.2.0 → supported")
	}
}

func TestSupportsProgressBar_WindowsTerminal(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY in test env")
	}
	clearTermEnv(t)
	t.Setenv("WT_SESSION", "x")
	if !SupportsProgressBar() {
		t.Error("WT_SESSION → supports unconditionally")
	}
}

func TestSupportsProgressBar_AppleTerminalUnsupported(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY in test env")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	if SupportsProgressBar() {
		t.Error("Apple Terminal must NOT support OSC 9;4 (silently drops)")
	}
}

func TestSupportsHyperlink_KnownTerminals(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY in test env")
	}
	clearTermEnv(t)

	for tp, kind := range map[string]TerminalKind{
		"iTerm.app": TerminalITerm2,
		"ghostty":   TerminalGhostty,
		"WezTerm":   TerminalWezTerm,
		"vscode":    TerminalVSCode,
	} {
		t.Run(tp, func(t *testing.T) {
			clearTermEnv(t)
			t.Setenv("TERM_PROGRAM", tp)
			if !SupportsHyperlink() {
				t.Errorf("%v should support OSC 8 hyperlink", kind)
			}
		})
	}
}

func TestSupportsHyperlink_AppleTerminalUnsupported(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY in test env")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	if SupportsHyperlink() {
		t.Error("Apple Terminal must NOT support OSC 8 (renders as visible noise)")
	}
}

func TestPreferSTTerminator_KittyOnly(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("KITTY_WINDOW_ID", "1")
	if !PreferSTTerminator() {
		t.Error("Kitty must use ST terminator (silently drops BEL-terminated OSCs)")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if PreferSTTerminator() {
		t.Error("iTerm2 should default to BEL")
	}
}

func TestAtLeast_VariousFormats(t *testing.T) {
	cases := []struct {
		v       string
		major   int
		minor   int
		patch   int
		want    bool
		comment string
	}{
		{"3.6.6", 3, 6, 6, true, "exact match"},
		{"3.6.5", 3, 6, 6, false, "patch below"},
		{"3.7.0", 3, 6, 6, true, "minor above"},
		{"4.0.0", 3, 6, 6, true, "major above"},
		{"v3.6.6", 3, 6, 6, true, "leading v"},
		{"3.6.6-beta1", 3, 6, 6, true, "trailing pre-release"},
		{"3.6", 3, 6, 6, false, "no patch → unparseable"},
		{"", 3, 6, 6, false, "empty → false"},
		{"abc", 3, 6, 6, false, "garbage → false"},
		{"3.6.x", 3, 6, 0, false, "x patch → unparseable"},
	}
	for _, c := range cases {
		t.Run(c.v+"_"+c.comment, func(t *testing.T) {
			if got := atLeast(c.v, c.major, c.minor, c.patch); got != c.want {
				t.Errorf("atLeast(%q, %d, %d, %d) = %v, want %v",
					c.v, c.major, c.minor, c.patch, got, c.want)
			}
		})
	}
}

func TestTerminalKind_String(t *testing.T) {
	if TerminalITerm2.String() != "iterm2" {
		t.Error("string mapping broken")
	}
	if TerminalUnknown.String() != "unknown" {
		t.Error("unknown should stringify to 'unknown'")
	}
}
