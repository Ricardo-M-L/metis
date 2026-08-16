package tui

import (
	"reflect"
	"testing"
)

// TestScrubEscapeLeaks exercises the regex set against the sequences
// we've seen leak into the textarea. The user's actual screenshot was
// `11;rgb:158e/193a/1e75\<66;80;12M` — that and adjacent variants are
// the priority cases; we also assert that ordinary input survives
// untouched and that semicolon-laden user prose isn't false-positive
// stripped.
func TestScrubEscapeLeaks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"user screenshot", `11;rgb:158e/193a/1e75\<66;80;12M`, ""},
		{"OSC 11 with leading bracket", `]11;rgb:158e/193a/1e75\`, ""},
		// Variant from image #47 — both `]` and one `1` got eaten,
		// leaving just `1;rgb:...`. We accept any digit prefix.
		{"single-digit OSC leak", `1;rgb:158e/193a/`, ""},
		{"single-digit OSC complete", `1;rgb:158e/193a/1e75\`, ""},
		// Box-drawing run from image #50 — X10 mouse triplets
		// reinterpreted as Latin-1 → UTF-8 box chars after a
		// scroll burst. Strip 3+ consecutive.
		{"box-drawing run", "▀▀▀▀▀▀", ""},
		{"two boxes preserved", "say ▀▀ hi", "say ▀▀ hi"},
		{"bare SGR mouse", `<66;80;12M`, ""},
		{"OSC 4 palette", `]4;0;rgb:0000/0000/0000\`, ""},
		{"DEC mode toggle", `?2004h?25l`, ""},
		{"cursor pos report", `24;80R`, ""},
		{"OSC + mouse + tail", `]11;rgb:abc\<35;10;5Mhello`, "hello"},
		{"OSC sandwiched", `hello]11;rgb:abc\world`, "helloworld"},

		// User input that happens to contain similar tokens — must
		// pass through unchanged.
		{"plain text", `hello world`, "hello world"},
		{"number sentence", `type 10 sheets`, "type 10 sheets"},
		{"version-like dotted", `my version is 1.10;abc`, "my version is 1.10;abc"},
		{"timestamps", `at 12:34:56`, "at 12:34:56"},

		// Regression — user video 2026-05-07 21:18: typing "1" five
		// times oscillated between "1" and "11" because the OSC color-
		// reset pattern `\]?(?:110|111|112)(?:\x1b\\|\\)?` was matching
		// bare digit runs (both anchor and terminator were optional).
		// Plain digit runs must pass through unchanged now.
		{"bare 110", "110", "110"},
		{"bare 111", "111", "111"},
		{"bare 112", "112", "112"},
		{"phone-style digits", "call 11122223333", "call 11122223333"},
		{"110 in sentence", "the 110th run", "the 110th run"},

		// Real OSC 110-style leaks (with anchor + terminator) still get
		// scrubbed.
		{"OSC 111 with bracket and ST", "]111\x1b\\", ""},
		{"OSC 110 with ESC anchor and ST", "\x1b110\x1b\\", ""},

		// --- NEW (2026-08-15): partial-prefix leaks from screenshot ---
		// When bubbletea's parser consumes more of the OSC 11 prefix
		// than expected (`]11;rgb:1` or even `]11;rgb:`), the remaining
		// text is just hex fragments that the old patterns couldn't match.
		{"screenshot hex body", "58e/193a/1e75", ""},
		{"screenshot hex with ST", "58e/193a/1e75\\", ""},
		{"screenshot hex with BEL", "58e/193a/1e75\x1b\\", ""},
		{"semicolon rgb prefix preserved", ";rgb:158e/193a/1e75\\", ""},
		{"rgb colon prefix preserved", "rgb:158e/193a/1e75\\", ""},
		{"first digit consumed", "158e/193a/1e75\\", ""},
		// Adjacent hex bodies from multiple interleaved OSC 11 replies.
		// The multi-pass loop must catch the second body after the
		// first is stripped.
		{"two adjacent hex bodies", "58e/193a/1e7558e/193a/1e75", ""},
		{"hex body mixed with text", "hello58e/193a/1e75world", "helloworld"},
		{"CJK with hex body", "说中文58e/193a/1e75测试", "说中文测试"},

		// Date / ratio formats must NOT be stripped — the {2,4} group
		// size prevents matching single-char groups like `1/22/2024`.
		{"date not stripped", "1/22/2024", "1/22/2024"},
		{"ratio not stripped", "a/b/c", "a/b/c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubEscapeLeaks(tc.in)
			if got != tc.want {
				t.Fatalf("scrubEscapeLeaks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpandPastedImages covers the happy path (single + multiple
// tags in the same buffer), missing-index gracefully left as-is, and
// the no-op case where the tag is wrapped in surrounding text. We
// compare the index map by deep equality to verify nothing mutates it.
func TestExpandPastedImages(t *testing.T) {
	idx := map[int]string{
		1: "/cache/a.png",
		2: "/cache/b.png",
	}
	preIdx := map[int]string{1: idx[1], 2: idx[2]}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "no images here", "no images here"},
		{"single", "look at [Image #1] please", "look at [image: /cache/a.png] please"},
		{"two", "[Image #1] and [Image #2]", "[image: /cache/a.png] and [image: /cache/b.png]"},
		{"missing", "[Image #99] gone", "[Image #99] gone"},
		{"adjacent", "[Image #1][Image #2]", "[image: /cache/a.png][image: /cache/b.png]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandPastedImages(tc.in, idx)
			if got != tc.want {
				t.Fatalf("expandPastedImages(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if !reflect.DeepEqual(idx, preIdx) {
		t.Fatalf("expandPastedImages mutated the index map: got %v, want %v", idx, preIdx)
	}
}
