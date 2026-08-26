package tui

import (
	"strings"
	"testing"
)

// TestClassifyCodeSpan verifies the 2-colour inline-code policy
// (image #8 user feedback: 5 colours was too noisy). Numbers + units
// → cyan. Everything else (identifiers, function calls, namespaces,
// constants, ordinary words, CJK) → orange.
func TestClassifyCodeSpan(t *testing.T) {
	const (
		sgrOrange = "38;2;255;184;108" // Dracula orange — all non-numeric code spans
		sgrCyan   = "38;2;139;233;253" // Dracula cyan — numeric / unit-bearing
	)
	cases := []struct {
		text    string
		wantSGR string
		kind    string
	}{
		// number-ish → cyan
		{"800ms", sgrCyan, "number"},
		{"5xx", sgrCyan, "number"},
		{"1.5s", sgrCyan, "number"},
		{"64k", sgrCyan, "number"},
		{"200", sgrCyan, "number"},

		// everything else → orange
		{"EXIT_ALT_SCREEN", sgrOrange, "constant"},
		{"SIGKILL", sgrOrange, "constant"},
		{"resetTerminal()", sgrOrange, "function"},
		{"process.exit(0)", sgrOrange, "function"},
		{"os.Exit", sgrOrange, "namespace"},
		{"permission.Gate", sgrOrange, "namespace"},
		{"/dev/tty", sgrOrange, "abs-path"},
		{"hello world", sgrOrange, "phrase"},
		{"中文短语", sgrOrange, "cjk"},
	}

	for _, tc := range cases {
		t.Run(tc.kind+"/"+tc.text, func(t *testing.T) {
			got := classifyCodeSpan(tc.text).Render(tc.text)
			if !strings.Contains(got, tc.wantSGR) {
				t.Fatalf("classifyCodeSpan(%q): expected SGR %q (kind=%s) in output, got %q",
					tc.text, tc.wantSGR, tc.kind, got)
			}
		})
	}
}

func TestInlineCodeHasForegroundWithoutBackground(t *testing.T) {
	const text = "use `ark-code-latest` for this request"

	// The current full-screen TUI path.
	mdRendererMu.Lock()
	mdRendererNarrow = nil
	mdRendererWide = nil
	mdRendererMu.Unlock()
	fullScreen := renderAssistantBody(text, 80)
	assertInlineCodeHasNoBackground(t, "full-screen TUI", fullScreen)

	// The line-oriented REPL path must not reintroduce Glamour's default
	// grey code badge either.
	legacyRenderer, err := MarkdownRenderer(80)
	if err != nil {
		t.Fatalf("create REPL markdown renderer: %v", err)
	}
	legacy, err := legacyRenderer.Render(text)
	if err != nil {
		t.Fatalf("render REPL markdown: %v", err)
	}
	assertInlineCodeHasNoBackground(t, "REPL", legacy)
}

func assertInlineCodeHasNoBackground(t *testing.T, path, rendered string) {
	t.Helper()
	if !strings.Contains(rendered, "38;5;203") {
		t.Fatalf("%s: inline code lost its red foreground: %q", path, rendered)
	}
	if strings.Contains(rendered, "48;5;236") || strings.Contains(rendered, "48;2;48;48;48") {
		t.Fatalf("%s: inline code still has Glamour's grey background: %q", path, rendered)
	}
}
