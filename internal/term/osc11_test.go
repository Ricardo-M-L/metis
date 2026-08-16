//go:build !windows

package term

// osc11_test.go — pin parseOSC11Response across the variants real
// terminals emit. DetectTerminalBackground itself is harness-tested
// from the e2e tmux script (live TTY required).

import (
	"math"
	"testing"
)

func TestParseOSC11_StandardResponse(t *testing.T) {
	// xterm-style 4-hex-digit response on a black background.
	resp := []byte("\x1b]11;rgb:0000/0000/0000\x07")
	r, g, b, err := parseOSC11Response(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !approxEq(r, 0) || !approxEq(g, 0) || !approxEq(b, 0) {
		t.Errorf("black: r=%v g=%v b=%v; want all 0", r, g, b)
	}
}

func TestParseOSC11_WhiteBackground(t *testing.T) {
	resp := []byte("\x1b]11;rgb:ffff/ffff/ffff\x07")
	r, g, b, err := parseOSC11Response(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !approxEq(r, 1) || !approxEq(g, 1) || !approxEq(b, 1) {
		t.Errorf("white: r=%v g=%v b=%v; want all 1.0", r, g, b)
	}
}

func TestParseOSC11_TwoDigitPerChannel(t *testing.T) {
	// kitty / wezterm sometimes emit 2-hex-digit form.
	resp := []byte("\x1b]11;rgb:80/80/80\x07")
	r, g, b, err := parseOSC11Response(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !approxEq(r, 128.0/255.0) || !approxEq(g, 128.0/255.0) || !approxEq(b, 128.0/255.0) {
		t.Errorf("mid-grey 2-digit: r=%v g=%v b=%v", r, g, b)
	}
}

func TestParseOSC11_ESCBackslashTerminator(t *testing.T) {
	// Some terminals use ST (ESC \) instead of BEL.
	resp := []byte("\x1b]11;rgb:1234/5678/9abc\x1b\\")
	r, g, b, err := parseOSC11Response(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !approxEq(r, 0x1234/65535.0) || !approxEq(g, 0x5678/65535.0) || !approxEq(b, 0x9abc/65535.0) {
		t.Errorf("hex parse off: r=%v g=%v b=%v", r, g, b)
	}
}

func TestParseOSC11_NoRGBPrefix(t *testing.T) {
	if _, _, _, err := parseOSC11Response([]byte("garbage")); err == nil {
		t.Error("expected error on no rgb: prefix")
	}
}

func TestParseOSC11_OnlyTwoChannels(t *testing.T) {
	if _, _, _, err := parseOSC11Response([]byte("\x1b]11;rgb:00/ff\x07")); err == nil {
		t.Error("expected error on 2-channel response")
	}
}

func TestParseOSC11_LightLuminance(t *testing.T) {
	// White background should compute luminance > 0.5.
	resp := []byte("\x1b]11;rgb:ffff/ffff/ffff\x07")
	r, g, b, _ := parseOSC11Response(resp)
	y := 0.299*r + 0.587*g + 0.114*b
	if y <= 0.5 {
		t.Errorf("white luminance = %v; expected > 0.5", y)
	}
}

func TestParseOSC11_DarkLuminance(t *testing.T) {
	// Default dark terminal background ~ #1a1a1a-ish.
	resp := []byte("\x1b]11;rgb:1a1a/1a1a/1a1a\x07")
	r, g, b, _ := parseOSC11Response(resp)
	y := 0.299*r + 0.587*g + 0.114*b
	if y > 0.5 {
		t.Errorf("dark luminance = %v; expected ≤ 0.5", y)
	}
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 0.001
}
