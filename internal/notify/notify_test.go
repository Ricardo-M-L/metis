package notify

// notify_test.go — pins the channel-matrix selection rules and the
// per-channel OSC sequence shapes. Each terminal speaks a slightly
// different protocol; if the wire bytes drift, the corresponding
// test breaks loudly.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/term"
)

// captureNotify swaps the notify dest to a fresh buffer for the test
// and restores it on cleanup. Returns the buffer so the test can
// inspect emitted bytes.
func captureNotify(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	SetNotifyDest(&buf)
	t.Cleanup(func() { SetNotifyDest(os.Stderr) })
	return &buf
}

// stubInteractionPast — back-date the recent-interaction marker so
// the 6s guard inside SendNotification doesn't suppress test
// emissions.
func stubInteractionPast(t *testing.T) {
	t.Helper()
	lastInteractionMu.Lock()
	defer lastInteractionMu.Unlock()
	lastInteractionAt = time.Now().Add(-(RecentInteractionThreshold + time.Second))
}

// clearTerminalEnv — wipe the env vars autoChannel reads so a single
// test starts from a clean slate. Without this, running on a real
// developer machine leaks $TERM_PROGRAM into auto-mode tests.
func clearTerminalEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"TERM_PROGRAM", "TERM_PROGRAM_VERSION", "LC_TERMINAL",
		"KITTY_WINDOW_ID", "ALACRITTY_LOG", "WT_SESSION", "ConEmuPID",
		"ConEmuANSI", "TMUX", "STY", "SSH_CONNECTION", "SSH_TTY", "TERM",
	} {
		t.Setenv(k, "")
	}
}

// ─────────────────────────────────────────────────────────────────────
// SelectChannel — env override table
// ─────────────────────────────────────────────────────────────────────

func TestSelectChannel_ManualOverrides(t *testing.T) {
	cases := []struct {
		env  string
		want Channel
	}{
		{"off", ChannelOff},
		{"disabled", ChannelOff},
		{"false", ChannelOff},
		{"0", ChannelOff},
		{"no", ChannelOff},
		{"iterm2", ChannelITerm2},
		{"iterm2_with_bell", ChannelITerm2WithBell},
		{"iterm2-with-bell", ChannelITerm2WithBell},
		{"kitty", ChannelKitty},
		{"ghostty", ChannelGhostty},
		{"bell", ChannelBell},
		{"terminal_bell", ChannelBell},
		// case-insensitive + trim
		{"  ITERM2  ", ChannelITerm2},
		{"Kitty", ChannelKitty},
	}
	for _, c := range cases {
		t.Run("env="+c.env, func(t *testing.T) {
			clearTerminalEnv(t)
			t.Setenv("METIS_NOTIFY_CHANNEL", c.env)
			if got := SelectChannel(); got != c.want {
				t.Errorf("SelectChannel() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestSelectChannel_AutoIterm2(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "auto")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if got := SelectChannel(); got != ChannelITerm2 {
		t.Errorf("auto + iTerm.app should pick iTerm2 channel; got %d", got)
	}
}

func TestSelectChannel_AutoWezTerm(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "auto")
	t.Setenv("TERM_PROGRAM", "WezTerm")
	if got := SelectChannel(); got != ChannelITerm2 {
		t.Errorf("WezTerm should reuse iTerm2 channel; got %d", got)
	}
}

func TestSelectChannel_AutoGhostty(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "auto")
	t.Setenv("TERM_PROGRAM", "ghostty")
	if got := SelectChannel(); got != ChannelGhostty {
		t.Errorf("ghostty should pick its own channel; got %d", got)
	}
}

func TestSelectChannel_AutoKittyMarker(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "auto")
	t.Setenv("KITTY_WINDOW_ID", "1")
	if got := SelectChannel(); got != ChannelKitty {
		t.Errorf("KITTY_WINDOW_ID set should pick Kitty channel; got %d", got)
	}
}

func TestSelectChannel_AutoAlacrittyMarker(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "auto")
	t.Setenv("ALACRITTY_LOG", "/tmp/alacritty.log")
	if got := SelectChannel(); got != ChannelITerm2 {
		t.Errorf("ALACRITTY_LOG set should pick iTerm2 channel (Alacritty supports OSC 9); got %d", got)
	}
}

func TestSelectChannel_AutoNothingSet(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "auto")
	// (no TERM_PROGRAM, no markers)
	if got := SelectChannel(); got != ChannelOff {
		t.Errorf("auto with no markers should be Off; got %d", got)
	}
}

func TestSelectChannel_UnknownValueFallsBackToAuto(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "junk-value")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if got := SelectChannel(); got != ChannelITerm2 {
		t.Errorf("unknown env value should fall back to auto; got %d", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Per-channel OSC sequences — pin the wire shape
// ─────────────────────────────────────────────────────────────────────

func TestEmitITerm2_ShapesOSC9(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "iterm2")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "turn finished")

	got := buf.String()
	if !strings.HasPrefix(got, "\x1b]9;") {
		t.Errorf("expected OSC 9 prefix \\x1b]9;; got %q", got)
	}
	if !strings.HasSuffix(got, "\x07") {
		t.Errorf("expected BEL terminator; got %q", got)
	}
	if !strings.Contains(got, "metis: turn finished") {
		t.Errorf("expected 'title: body' inside OSC 9; got %q", got)
	}
}

func TestEmitKitty_EmitsThreeSequences(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "kitty")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	got := buf.String()
	// 3 separate OSC 99 sequences with ST terminator, sharing the same id.
	if c := strings.Count(got, "\x1b]99;"); c != 3 {
		t.Errorf("expected 3 OSC 99 segments (title/body/focus); got %d in %q", c, got)
	}
	if c := strings.Count(got, "\x1b\\"); c != 3 {
		t.Errorf("expected 3 ST terminators; got %d", c)
	}
	if !strings.Contains(got, "p=title") {
		t.Errorf("first segment should carry p=title; got %q", got)
	}
	if !strings.Contains(got, "p=body") {
		t.Errorf("second segment should carry p=body; got %q", got)
	}
	if !strings.Contains(got, "a=focus") {
		t.Errorf("third segment should carry a=focus action; got %q", got)
	}
}

func TestEmitGhostty_ShapesOSC777(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "ghostty")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "long turn done")

	got := buf.String()
	if !strings.HasPrefix(got, "\x1b]777;notify;") {
		t.Errorf("expected OSC 777 notify prefix; got %q", got)
	}
	if !strings.Contains(got, ";metis;long turn done\x07") {
		t.Errorf("expected ;<title>;<body>BEL shape; got %q", got)
	}
}

func TestEmitBell_RawBELOnly(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "bell")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	if got := buf.String(); got != "\x07" {
		t.Errorf("bell channel should emit raw BEL only; got %q", got)
	}
}

func TestEmitITerm2WithBell_BothFire(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "iterm2_with_bell")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	got := buf.String()
	if !strings.Contains(got, "\x1b]9;") {
		t.Errorf("iterm2_with_bell should emit OSC 9; got %q", got)
	}
	if !strings.HasSuffix(got, "\x07") {
		t.Errorf("iterm2_with_bell should end with raw BEL; got %q", got)
	}
}

func TestSendNotification_OffEmitsNothing(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	if got := buf.String(); got != "" {
		t.Errorf("off channel should emit nothing; got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// 6-second recent-interaction guard
// ─────────────────────────────────────────────────────────────────────

func TestSendNotification_RecentKeyPressSuppresses(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "iterm2")
	buf := captureNotify(t)

	MarkUserInteraction() // user just pressed a key
	SendNotification("metis", "done")

	if got := buf.String(); got != "" {
		t.Errorf("recent interaction should suppress notification; got %q", got)
	}
}

func TestSendNotification_StaleKeyPressAllowsEmission(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "iterm2")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	if got := buf.String(); got == "" {
		t.Errorf("stale interaction should NOT suppress; got empty")
	}
}

// TestSendNotification_ZeroValueDoesNotSuppress — regression for the
// "first 6 seconds of every metis process were silenced" bug. When
// MarkUserInteraction has never been called, lastInteractionAt is the
// zero time; the guard must treat zero as "never," not "just now."
//
// This test resets lastInteractionAt to its zero value and verifies
// SendNotification still emits.
func TestSendNotification_ZeroValueDoesNotSuppress(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("METIS_NOTIFY_CHANNEL", "iterm2")
	// Force the "never recorded" state.
	lastInteractionMu.Lock()
	lastInteractionAt = time.Time{}
	lastInteractionMu.Unlock()
	t.Cleanup(func() { stubInteractionPast(t) })

	buf := captureNotify(t)
	SendNotification("metis", "first turn")

	if got := buf.String(); got == "" {
		t.Errorf("zero-value lastInteractionAt must not suppress (regression for first-6s silence bug)")
	}
}

// ─────────────────────────────────────────────────────────────────────
// tmux DCS passthrough wrapping (BEL exception)
// ─────────────────────────────────────────────────────────────────────

func TestWrapForMultiplexer_TmuxWrapsOSC(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("METIS_NOTIFY_CHANNEL", "iterm2")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	got := buf.String()
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Errorf("inside tmux, OSC must be DCS-wrapped; got %q", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Errorf("DCS wrap should end with ST (ESC \\); got %q", got)
	}
	// Inner ESCs must be doubled (tmux DCS passthrough requires it).
	if !strings.Contains(got, "\x1b\x1b]9;") {
		t.Errorf("inner ESC should be doubled inside tmux DCS; got %q", got)
	}
}

func TestWrapForMultiplexer_BellNotWrapped(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("METIS_NOTIFY_CHANNEL", "bell")
	stubInteractionPast(t)
	buf := captureNotify(t)

	SendNotification("metis", "done")

	if got := buf.String(); got != "\x07" {
		t.Errorf("BEL must be raw inside tmux (preserves bell-action window flag); got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// escapeOSCText — neutralize control bytes that would prematurely
// terminate the OSC sequence.
// ─────────────────────────────────────────────────────────────────────

func TestEscapeOSCText_StripsTerminators(t *testing.T) {
	in := "before\x07middle\x1bafter"
	got := escapeOSCText(in)
	if strings.ContainsAny(got, "\x07\x1b") {
		t.Errorf("escapeOSCText must strip BEL and ESC; got %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "middle") || !strings.Contains(got, "after") {
		t.Errorf("escapeOSCText should preserve readable parts; got %q", got)
	}
}

func TestEscapeOSCText_KeepsNewline(t *testing.T) {
	got := escapeOSCText("line1\nline2")
	if !strings.Contains(got, "\n") {
		t.Errorf("escapeOSCText should keep newlines; got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// SelectChannelName — the human-readable label exposed via
// `metis config show`.
// ─────────────────────────────────────────────────────────────────────

func TestSelectChannelName_LabelMatchesChannel(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"iterm2", "iterm2"},
		{"iterm2_with_bell", "iterm2_with_bell"},
		{"kitty", "kitty"},
		{"ghostty", "ghostty"},
		{"bell", "bell"},
		{"off", "off"},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			clearTerminalEnv(t)
			t.Setenv("METIS_NOTIFY_CHANNEL", c.env)
			if got := SelectChannelName(); got != c.want {
				t.Errorf("SelectChannelName() = %q, want %q", got, c.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// SendProgress — OSC 9;4 progress bar
// ─────────────────────────────────────────────────────────────────────

func TestSendProgress_NoOpOnUnsupportedTerminal(t *testing.T) {
	clearTerminalEnv(t)
	// No TERM_PROGRAM, no ConEmuPID → unsupported.
	buf := captureNotify(t)
	SendProgress(ProgressRunning, 50)
	if got := buf.String(); got != "" {
		t.Errorf("unsupported terminal should be no-op; got %q", got)
	}
}

func TestSendProgress_RunningEmitsCorrectShape(t *testing.T) {
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.6")
	buf := captureNotify(t)

	SendProgress(ProgressRunning, 42)

	got := buf.String()
	if got != "\x1b]9;4;1;42\x07" {
		t.Errorf("ProgressRunning(42) wire shape wrong; got %q", got)
	}
}

func TestSendProgress_ClearEmpty(t *testing.T) {
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.6")
	buf := captureNotify(t)

	SendProgress(ProgressClear, 0)

	if got := buf.String(); got != "\x1b]9;4;0;\x07" {
		t.Errorf("ProgressClear shape wrong; got %q", got)
	}
}

func TestSendProgress_ClampsPctToRange(t *testing.T) {
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TERM_PROGRAM_VERSION", "3.6.6")
	buf := captureNotify(t)

	SendProgress(ProgressRunning, 250) // out-of-range, should clamp to 100

	if got := buf.String(); got != "\x1b]9;4;1;100\x07" {
		t.Errorf("pct should clamp to 100; got %q", got)
	}
}

func TestSendProgress_IndeterminateNoPct(t *testing.T) {
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("TERM_PROGRAM_VERSION", "1.2.0")
	buf := captureNotify(t)

	SendProgress(ProgressIndeterminate, 0)

	if got := buf.String(); got != "\x1b]9;4;3;\x07" {
		t.Errorf("ProgressIndeterminate shape wrong; got %q", got)
	}
}
