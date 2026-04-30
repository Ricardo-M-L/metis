package tui

// diff_syntax.go applies chroma syntax highlighting to Edit-tool diff
// content lines. Without this, code shows as flat red-on-bg / green-on-bg
// blocks; with it, keywords/strings/comments get distinct hues even
// inside the diff highlight, matching claude-code's appearance.
//
// Approach:
//   - chroma colors the ANSI for the unchanged ("equal") rows and the
//     content text of +/- rows (we pre-strip the +/- prefix before
//     passing in)
//   - For +/- rows, we then re-apply the diff bg color via lipgloss.
//     The bg layers over chroma's fg colors cleanly because chroma
//     emits SGR for fg + reset, and lipgloss.Style.Render wraps with
//     additional SGR for bg.
//   - When chroma can't pick a lexer (no extension hint, unknown
//     language), we fall back to plain text — same render path as
//     before this file existed.

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

var (
	chromaFormatter chroma.Formatter
	chromaStyle     *chroma.Style
	chromaInitOnce  sync.Once
)

func chromaInit() {
	chromaInitOnce.Do(func() {
		// terminal256 fits broadly — true-color formatter exists but
		// some terminals (older ssh sessions) only do 256-color.
		chromaFormatter = formatters.Get("terminal256")
		if chromaFormatter == nil {
			chromaFormatter = formatters.Fallback
		}
		// "monokai" is dark-friendly and widely recognized; pairs
		// well with our dark theme. Light theme would benefit from
		// "github" or similar — follow-up.
		chromaStyle = styles.Get("monokai")
		if chromaStyle == nil {
			chromaStyle = styles.Fallback
		}
	})
}

// highlightLine applies chroma syntax coloring to a single source-code
// line. filename is used to pick the lexer (Go for .go, Python for .py,
// JS for .js, etc.). When no lexer matches, returns the input unchanged
// so callers don't need to special-case "couldn't highlight" — the
// styled fallback path renders identically to before.
func highlightLine(content, filename string) string {
	chromaInit()
	if content == "" {
		return content
	}

	// Pick lexer by filename extension. lexers.Match handles
	// shebang/extension matching internally.
	var l chroma.Lexer
	if filename != "" {
		l = lexers.Match(filename)
	}
	if l == nil {
		// Try by analysis (content-based) as a fallback for
		// extensionless files.
		l = lexers.Analyse(content)
	}
	if l == nil {
		return content // no lexer = plain text fallback
	}
	l = chroma.Coalesce(l)

	iter, err := l.Tokenise(nil, content)
	if err != nil {
		return content
	}
	var buf bytes.Buffer
	if err := chromaFormatter.Format(&buf, chromaStyle, iter); err != nil {
		return content
	}
	// chroma sometimes emits a trailing reset that survives line
	// joins; trim newlines so we don't break the gutter-content
	// column alignment.
	return strings.TrimRight(buf.String(), "\n\r")
}

// pickLanguageFromInput pulls a filename hint from the Edit tool's
// input map. Edit and Write put the path under "path" or "file_path"
// (different builds historical) — try both.
func pickLanguageFromInput(input map[string]any) string {
	if v, ok := input["path"].(string); ok && v != "" {
		return filepath.Base(v)
	}
	if v, ok := input["file_path"].(string); ok && v != "" {
		return filepath.Base(v)
	}
	return ""
}
