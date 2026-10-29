package tui

import (
	"strings"
	"testing"
)

// Inline code uses one theme accent regardless of token shape.
func TestClassifyCodeSpan(t *testing.T) {
	accent := strings.Split(styleAccent.Render("sample"), "sample")[0]
	for _, text := range []string{"800ms", "5xx", "1.5s", "64k", "200", "EXIT_ALT_SCREEN", "SIGKILL", "resetTerminal()", "process.exit(0)", "os.Exit", "permission.Gate", "/dev/tty", "hello world", "中文短语"} {
		t.Run(text, func(t *testing.T) {
			got := classifyCodeSpan(text).Render(text)
			if !strings.Contains(got, accent) {
				t.Fatalf("classifyCodeSpan(%q): expected theme accent in %q", text, got)
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
	accent := strings.Split(styleAccent.Render("sample"), "sample")[0]
	if !strings.Contains(rendered, accent) {
		t.Fatalf("%s: inline code lost its theme accent: %q", path, rendered)
	}
	if strings.Contains(rendered, "48;5;236") || strings.Contains(rendered, "48;2;48;48;48") {
		t.Fatalf("%s: inline code still has Glamour's grey background: %q", path, rendered)
	}
}
