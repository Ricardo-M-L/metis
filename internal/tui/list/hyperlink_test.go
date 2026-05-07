package list

import "testing"

// TestScanOSC8AtCol covers the shape of OSC 8 hyperlink escape we emit
// from figures.go::osc8Link — `\x1b]8;;URL\x1b\\TEXT\x1b]8;;\x1b\\`.
// The scanner must:
//   1. return "" when col falls outside any link region
//   2. return the URL when col is within the visible TEXT
//   3. handle SGR runs in the middle (so styled hyperlinks work)
//   4. accept BEL (`\x07`) as the OSC terminator (some emitters use it)
//   5. handle multiple hyperlinks on one line
func TestScanOSC8AtCol(t *testing.T) {
	cases := []struct {
		name string
		line string
		col  int
		want string
	}{
		{
			name: "click inside link",
			line: "see \x1b]8;;https://x.com\x1b\\example\x1b]8;;\x1b\\ now",
			col:  6, // inside "example" (cols 4-10)
			want: "https://x.com",
		},
		{
			name: "click before link",
			line: "see \x1b]8;;https://x.com\x1b\\example\x1b]8;;\x1b\\ now",
			col:  1, // inside "see"
			want: "",
		},
		{
			name: "click after link",
			line: "see \x1b]8;;https://x.com\x1b\\example\x1b]8;;\x1b\\ now",
			col:  13, // inside "now"
			want: "",
		},
		{
			name: "BEL terminator",
			line: "see \x1b]8;;https://y.com\x07example\x1b]8;;\x07 done",
			col:  6,
			want: "https://y.com",
		},
		{
			name: "SGR styling inside link",
			line: "x \x1b]8;;https://z.com\x1b\\\x1b[34munder\x1b[0m\x1b]8;;\x1b\\ y",
			col:  4, // inside styled "under" (cols 2-6)
			want: "https://z.com",
		},
		{
			name: "two links — pick the right one",
			line: "\x1b]8;;https://a\x1b\\A\x1b]8;;\x1b\\ \x1b]8;;https://b\x1b\\B\x1b]8;;\x1b\\",
			col:  2, // "B"
			want: "https://b",
		},
		{
			name: "no link at all",
			line: "just plain text",
			col:  4,
			want: "",
		},
		{
			name: "click on first link",
			line: "\x1b]8;;https://a\x1b\\A\x1b]8;;\x1b\\ \x1b]8;;https://b\x1b\\B\x1b]8;;\x1b\\",
			col:  0,
			want: "https://a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanOSC8AtCol(tc.line, tc.col)
			if got != tc.want {
				t.Errorf("scanOSC8AtCol(_, %d) = %q; want %q", tc.col, got, tc.want)
			}
		})
	}
}

// TestURLAtPoint_RoundTrip — drive the public API end-to-end via a
// list with a real OSC 8 emit (matching what render_message.go would
// produce). Verifies item indexing + line splitting + scanner all
// agree on the click point.
func TestURLAtPoint_RoundTrip(t *testing.T) {
	link := "\x1b]8;;https://example.test\x1b\\click\x1b]8;;\x1b\\"
	l := NewList(
		&staticItem{content: "preamble"},
		&staticItem{content: "go " + link + " now"},
	)
	l.SetSize(80, 30)

	if got := l.URLAtPoint(1, 0, 4); got != "https://example.test" {
		t.Errorf("expected URL at item 1 col 4; got %q", got)
	}
	if got := l.URLAtPoint(1, 0, 0); got != "" {
		t.Errorf("col 0 is on 'go ' (no link); got %q", got)
	}
	if got := l.URLAtPoint(0, 0, 2); got != "" {
		t.Errorf("item 0 has no link; got %q", got)
	}
	if got := l.URLAtPoint(99, 0, 0); got != "" {
		t.Errorf("out-of-range item should return empty; got %q", got)
	}
}
