package tui

import (
	"bytes"
	"strings"
	"testing"
)

// SendProgress lives in notify.go; we test the version-gating + tmux
// wrapping via SetNotifyDest / progressSupported.

func TestSendProgress_NoOpOnAppleTerminal(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	var buf bytes.Buffer
	SetNotifyDest(&buf)
	t.Cleanup(func() { SetNotifyDest(nil) })
	SendProgress(ProgressIndeterminate, 0)
	if buf.Len() != 0 {
		t.Errorf("Apple Terminal must not receive OSC 9;4, wrote %q", buf.String())
	}
}

func TestSendProgress_FiresOnWT(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("WT_SESSION", "x")
	var buf bytes.Buffer
	SetNotifyDest(&buf)
	t.Cleanup(func() { SetNotifyDest(nil) })
	SendProgress(ProgressIndeterminate, 0)
	if !strings.Contains(buf.String(), "\x1b]9;4;3") {
		t.Errorf("WT should receive OSC 9;4: %q", buf.String())
	}
}

func TestSendProgress_VersionGated_iTerm2(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")

	t.Setenv("TERM_PROGRAM_VERSION", "3.6.5")
	var buf1 bytes.Buffer
	SetNotifyDest(&buf1)
	t.Cleanup(func() { SetNotifyDest(nil) })
	SendProgress(ProgressIndeterminate, 0)
	if buf1.Len() != 0 {
		t.Errorf("iTerm2 3.6.5 < 3.6.6 → no OSC 9;4 (got %q)", buf1.String())
	}

	t.Setenv("TERM_PROGRAM_VERSION", "3.6.6")
	var buf2 bytes.Buffer
	SetNotifyDest(&buf2)
	SendProgress(ProgressIndeterminate, 0)
	if !strings.Contains(buf2.String(), "\x1b]9;4;3") {
		t.Errorf("iTerm2 3.6.6 should fire: %q", buf2.String())
	}
}

func TestSendProgress_TmuxWrapping(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("WT_SESSION", "x")
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	var buf bytes.Buffer
	SetNotifyDest(&buf)
	t.Cleanup(func() { SetNotifyDest(nil) })
	SendProgress(ProgressIndeterminate, 0)
	out := buf.String()
	if !strings.HasPrefix(out, "\x1bPtmux;") {
		t.Errorf("tmux DCS prefix missing: %q", out)
	}
}

func TestHyperlink_SupportedTerminal(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	got := Hyperlink("click here", "https://example.com")
	if !strings.Contains(got, "\x1b]8;id=") {
		t.Errorf("missing OSC 8 prefix: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("missing url: %q", got)
	}
	if !strings.Contains(got, "click here") {
		t.Errorf("missing text: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b]8;;\x07") {
		t.Errorf("missing close: %q", got)
	}
}

func TestHyperlink_UnsupportedTerminalFallsBack(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	got := Hyperlink("click here", "https://example.com")
	want := "click here (https://example.com)"
	if got != want {
		t.Errorf("fallback: %q, want %q", got, want)
	}
}

func TestHyperlink_EmptyURLPassesThrough(t *testing.T) {
	if got := Hyperlink("just text", ""); got != "just text" {
		t.Errorf("empty url: %q", got)
	}
}

func TestHyperlink_SameURLSameID(t *testing.T) {
	if !IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTermEnv(t)
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	a := Hyperlink("a", "https://x")
	b := Hyperlink("b", "https://x")
	idA := strings.Split(strings.TrimPrefix(a, "\x1b]8;id="), ";")[0]
	idB := strings.Split(strings.TrimPrefix(b, "\x1b]8;id="), ";")[0]
	if idA != idB {
		t.Errorf("same url should yield same id, got %q vs %q", idA, idB)
	}
}

func TestOSC8ID_StableHexLength(t *testing.T) {
	id := osc8ID("https://example.com/path?q=1")
	if len(id) != 8 {
		t.Errorf("id len = %d, want 8", len(id))
	}
}
