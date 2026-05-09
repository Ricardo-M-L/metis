package tui

import (
	"encoding/base64"
	"strings"
	"testing"
)

// These tests pin the OSC 52 wire-format details that distinguish
// metis's clipboard from a naive `\x1b]52;c;<b64>\x07` write:
//   - tmux DCS passthrough wrapping when $TMUX is set
//   - ESC byte doubling inside the DCS payload (tmux uses bare ESC
//     as the close marker, so a literal ESC in the inner OSC 52
//     would terminate the wrapper early and corrupt downstream)
//   - Kitty-specific ST (`\x1b\\`) terminator vs default BEL (`\x07`)
//
// The DCS wrapping function is `wrapForMultiplexer` (notify.go) —
// shared with OSC 9;4 progress emission.

func TestWrapForMultiplexer_TmuxBasicEnvelope(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	inner := "\x1b]52;c;aGVsbG8=\x07" // bare OSC 52 with "hello"
	got := wrapForMultiplexer(inner)
	// Outer envelope: ESC P tmux ; <inner-with-doubled-ESCs> ESC \
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Fatalf("missing DCS prefix; got %q", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Fatalf("missing ST terminator; got %q", got)
	}
}

func TestWrapForMultiplexer_NoTmuxNoWrap(t *testing.T) {
	clearTermEnv(t)
	inner := "\x1b]52;c;aGVsbG8=\x07"
	got := wrapForMultiplexer(inner)
	if got != inner {
		t.Fatalf("no tmux/screen → no wrapping; got %q", got)
	}
}

func TestWrapForMultiplexer_TmuxDoublesEveryESC(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("TMUX", "x")
	cases := []struct {
		name           string
		inner          string
		expectInnerESC int
	}{
		{
			name:           "single ESC OSC52 + BEL",
			inner:          "\x1b]52;c;eA==\x07",
			expectInnerESC: 2, // 1 inner ESC * 2 = 2
		},
		{
			name:           "double ESC OSC52 + ST",
			inner:          "\x1b]52;c;eA==\x1b\\",
			expectInnerESC: 4, // 2 inner ESCs * 2 = 4
		},
		{
			name:           "no ESC at all",
			inner:          "no escape here",
			expectInnerESC: 0,
		},
		{
			name:           "many ESCs",
			inner:          "\x1b\x1b\x1b\x1b",
			expectInnerESC: 8, // 4 * 2 = 8
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapForMultiplexer(c.inner)
			// Strip outer envelope: prefix `\x1bPtmux;` and suffix `\x1b\\`.
			body := strings.TrimSuffix(strings.TrimPrefix(got, "\x1bPtmux;"), "\x1b\\")
			gotESC := strings.Count(body, "\x1b")
			if gotESC != c.expectInnerESC {
				t.Errorf("ESC count in payload = %d, want %d (full output: %q)", gotESC, c.expectInnerESC, got)
			}
		})
	}
}

func TestWrapForMultiplexer_ScreenWrapping(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("STY", "12345.pts-0.host")
	inner := "\x1b]52;c;aGVsbG8=\x07"
	got := wrapForMultiplexer(inner)
	// screen uses simpler `ESC P ... ESC \` (no `tmux;` prefix, no ESC doubling).
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
	clearTermEnv(t)
	t.Setenv("KITTY_WINDOW_ID", "1")
	if !PreferSTTerminator() {
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
	clearTermEnv(t)
	t.Setenv("TMUX", "x")
	bare := "\x1b]52;c;aGVsbG8=\x07"
	wrapped := wrapForMultiplexer(bare)
	body := strings.TrimSuffix(strings.TrimPrefix(wrapped, "\x1bPtmux;"), "\x1b\\")
	// 1 ESC in inner → 2 ESCs in doubled payload.
	if n := strings.Count(body, "\x1b"); n != 2 {
		t.Errorf("plain payload should yield 2 ESCs after doubling, got %d", n)
	}
}
