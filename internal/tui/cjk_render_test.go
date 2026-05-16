package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncate_NeverEmitsInvalidUTF8 — the 2026-05-16 image #14 repro:
// `bash(ls -la /Users/ricardo/Documents/公司学习...)` showed grey
// corruption because the byte-based truncate sliced through a CJK
// codepoint. Pin the contract that ANY input produces valid UTF-8 out.
func TestTruncate_NeverEmitsInvalidUTF8(t *testing.T) {
	cases := []string{
		"ls -la /Users/ricardo/Documents/公司学习文件/我自己的agent的cli/metis/internal/tools/builtin",
		strings.Repeat("公", 100),                                                            // pure CJK
		"prefix-" + strings.Repeat("a", 30) + "中文",                                          // mixed at boundary
		"emoji 🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒🔒",                                                  // 4-byte UTF-8 (emoji)
		"短",   // shorter than max
		"hi", // pure ASCII shorter than max
	}
	for _, s := range cases {
		got := truncate(s, 45)
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, 45) emitted invalid UTF-8: %q", s, got)
		}
	}
}

// TestTruncate_PreservesCJKBoundary — specifically check the case
// where the byte position 42 lands mid-character. With the old
// byte-based code this would chop the second byte of a 3-byte CJK
// rune, producing 0xE5 0xAC 0x… followed by "…". Verify the rune
// at position 42 is intact in the output.
func TestTruncate_PreservesCJKBoundary(t *testing.T) {
	// 50 Chinese chars × 3 bytes = 150 bytes. Each rune is 3 bytes,
	// so byte position 42 falls inside the 15th rune (bytes 42-44).
	input := strings.Repeat("公", 50)
	got := truncate(input, 45)
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 output: % x", []byte(got))
	}
	// Output should be the first 44 runes plus "…", all "公".
	runes := []rune(got)
	if len(runes) != 45 {
		t.Errorf("expected 45 runes (44 cut + …), got %d", len(runes))
	}
	for i := 0; i < 44; i++ {
		if runes[i] != '公' {
			t.Errorf("rune[%d] = %q, want '公'", i, runes[i])
		}
	}
	if runes[44] != '…' {
		t.Errorf("last rune = %q, want '…'", runes[44])
	}
}

// TestTruncate_ASCIIUnchangedShortCase — quick-path sanity: ASCII
// strings shorter than max go through the cheap len() comparison
// (no rune allocation). Catches accidental changes to the fast path.
func TestTruncate_ASCIIUnchangedShortCase(t *testing.T) {
	got := truncate("hello world", 45)
	if got != "hello world" {
		t.Errorf("short ASCII mutated: got %q", got)
	}
}

// TestToolArgsPreview_Bash_CJKPath — end-to-end check of the
// rendering pipeline: the Bash tool with a CJK command argument
// produces a clean preview string with no invalid bytes. This is the
// direct reproduction of the user's image #14 bug.
func TestToolArgsPreview_Bash_CJKPath(t *testing.T) {
	input := map[string]any{
		"command": "ls -la /Users/ricardo/Documents/公司学习文件/我自己的agent的cli/metis",
	}
	got := toolArgsPreview("Bash", input)
	if !utf8.ValidString(got) {
		t.Errorf("toolArgsPreview emitted invalid UTF-8 (the image #14 corruption): %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long path should be truncated with …; got %q", got)
	}
}

// TestFormatContextPct_ClampsAbove99 — the 2026-05-16 image #13 repro:
// status bar showed "207139 tokens (107%)". EstimateContextTokens
// over-counts CJK because chars/4 isn't one-token-per-char on
// multi-byte glyphs. Anything past 99% is the same signal ("compact
// soon"), so we clamp the display to "99%+" rather than report a
// misleading raw number that looks like a bug to users.
func TestFormatContextPct_ClampsAbove99(t *testing.T) {
	cases := []struct {
		used, cap int
		want      string
	}{
		{0, 200000, "0%"},
		{40000, 200000, "20%"},
		{99000, 200000, "49%"},
		{198000, 200000, "99%"},                  // exact boundary — still numeric
		{198001, 200000, "99%"},                  // 99.0005% → integer 99
		{199500, 200000, "99%"},                  // 99.75% → integer 99
		{200000, 200000, "99%+"},                 // exactly cap — clamp signals "at limit"
		{207139, 200000, "99%+"},                 // the actual user-repro number
		{500000, 200000, "99%+"},                 // wildly over (defensive)
		{120000, 128000, "93%"},                  // DeepSeek 128K window normal case
		{145000, 128000, "99%+"},                 // DeepSeek over-cap
	}
	for _, c := range cases {
		got := formatContextPct(c.used, c.cap)
		if got != c.want {
			t.Errorf("formatContextPct(%d, %d) = %q, want %q", c.used, c.cap, got, c.want)
		}
	}
}

// TestFormatContextPct_ZeroCapReturnsEmpty — provider with no
// MaxContextTokens (returning 0) shouldn't render a divide-by-zero
// percentage. Empty string lets the caller skip the "(X%)" suffix.
func TestFormatContextPct_ZeroCapReturnsEmpty(t *testing.T) {
	if got := formatContextPct(50000, 0); got != "" {
		t.Errorf("zero cap should return empty; got %q", got)
	}
}
