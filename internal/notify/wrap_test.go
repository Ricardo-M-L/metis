package notify

// wrap_test.go — pins the multiplexer DCS wrapping (tmux + screen)
// and progress-bar (OSC 9;4) details. Previously split between
// internal/tui/yank_osc52_test.go (wrap) and the same file as the
// hyperlink tests (progress). Merged here because both exercise
// wrapForMultiplexer + SendProgress, both of which live in notify.

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/term"
)

func TestWrapForMultiplexer_TmuxBasicEnvelope(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	inner := "\x1b]52;c;aGVsbG8=\x07" // bare OSC 52 with "hello"
	got := wrapForMultiplexer(inner)
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Fatalf("missing DCS prefix; got %q", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Fatalf("missing ST terminator; got %q", got)
	}
}

func TestWrapForMultiplexer_NoTmuxNoWrap(t *testing.T) {
	clearTerminalEnv(t)
	inner := "\x1b]52;c;aGVsbG8=\x07"
	got := wrapForMultiplexer(inner)
	if got != inner {
		t.Fatalf("no tmux/screen → no wrapping; got %q", got)
	}
}

func TestWrapForMultiplexer_TmuxDoublesEveryESC(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("TMUX", "x")
	cases := []struct {
		name           string
		inner          string
		expectInnerESC int
	}{
		{
			name:           "single ESC OSC52 + BEL",
			inner:          "\x1b]52;c;eA==\x07",
			expectInnerESC: 2,
		},
		{
			name:           "double ESC OSC52 + ST",
			inner:          "\x1b]52;c;eA==\x1b\\",
			expectInnerESC: 4,
		},
		{
			name:           "no ESC at all",
			inner:          "no escape here",
			expectInnerESC: 0,
		},
		{
			name:           "many ESCs",
			inner:          "\x1b\x1b\x1b\x1b",
			expectInnerESC: 8,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapForMultiplexer(c.inner)
			body := strings.TrimSuffix(strings.TrimPrefix(got, "\x1bPtmux;"), "\x1b\\")
			gotESC := strings.Count(body, "\x1b")
			if gotESC != c.expectInnerESC {
				t.Errorf("ESC count in payload = %d, want %d (full output: %q)", gotESC, c.expectInnerESC, got)
			}
		})
	}
}

func TestWrapForMultiplexer_ScreenWrapping(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("STY", "12345.pts-0.host")
	inner := "\x1b]52;c;aGVsbG8=\x07"
	got := wrapForMultiplexer(inner)
	if !strings.HasPrefix(got, "\x1bP") {
		t.Errorf("screen DCS prefix missing: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Errorf("screen ST terminator missing: %q", got)
	}
	if strings.HasPrefix(got, "\x1bPtmux;") {
		t.Errorf("screen should NOT use tmux; prefix: %q", got)
	}
}

func TestOSC52_KittyUsesST(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("KITTY_WINDOW_ID", "1")
	if !term.PreferSTTerminator() {
		t.Fatal("Kitty must use ST terminator (BEL is silently dropped)")
	}
}

func TestOSC52_BareSequenceShape(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("hello world"))
	if enc != "aGVsbG8gd29ybGQ=" {
		t.Fatalf("base64 sanity: %q", enc)
	}
	bare := "\x1b]52;c;" + enc + "\x07"
	if !strings.HasPrefix(bare, "\x1b]52;c;") {
		t.Errorf("OSC start: %q", bare)
	}
	if !strings.HasSuffix(bare, "\x07") {
		t.Errorf("BEL terminator: %q", bare)
	}
}

func TestWrapForMultiplexer_PlainPayloadOnlyHeaderESC(t *testing.T) {
	clearTerminalEnv(t)
	t.Setenv("TMUX", "x")
	bare := "\x1b]52;c;aGVsbG8=\x07"
	wrapped := wrapForMultiplexer(bare)
	body := strings.TrimSuffix(strings.TrimPrefix(wrapped, "\x1bPtmux;"), "\x1b\\")
	if n := strings.Count(body, "\x1b"); n != 2 {
		t.Errorf("plain payload should yield 2 ESCs after doubling, got %d", n)
	}
}

// ─────────────────────────────────────────────────────────────────────
// SendProgress (OSC 9;4) — version gating + multiplexer wrapping.
// ─────────────────────────────────────────────────────────────────────

func TestSendProgress_NoOpOnAppleTerminal(t *testing.T) {
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
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
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
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
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
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
	if !term.IsTerminal() {
		t.Skip("not a TTY")
	}
	clearTerminalEnv(t)
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
