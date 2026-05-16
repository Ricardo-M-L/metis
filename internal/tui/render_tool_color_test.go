package tui

import (
	"strings"
	"testing"
)

// TestReadPreview_GutterDimContentDefault — Phase regression pin
// (image #23 user feedback 2026-05-16). Read's `<lineno>\t<content>`
// rows must dim the gutter but render the content at default fg.
// Whole-line dim was the visual-exhaustion bug we're fixing.
func TestReadPreview_GutterDimContentDefault(t *testing.T) {
	out := renderToolOutputPreview("Read", "   200\t}\n   201\t\n   202\t/**\n", false)

	// The dim grey we use for gutter/path prefixes is
	// styleDim → textSecondary (#a0a0a0 in the dark theme).
	// SGR for 8-bit 38;2;160;160;160 is the truecolor render.
	const dimSGR = "38;2;160;160;160"
	if !strings.Contains(out, dimSGR) {
		t.Errorf("expected gutter to use dim SGR %s; got:\n%s", dimSGR, out)
	}

	// The content character `}` must NOT be inside the dim style — it
	// should land between escape sequences as plain text. Heuristic:
	// find `}` and confirm the preceding byte is a SGR-reset (0m) or
	// follows the tab-end, not the dim-color SGR.
	stripped := stripANSI(out)
	if !strings.Contains(stripped, "}") {
		t.Errorf("content brace lost from output: %q", stripped)
	}
	// If `}` were also dim-wrapped, the dim SGR would appear AFTER the
	// tab (delimiting the dim region across the whole line). Verify
	// instead that the dim SGR's scope ENDS at or before the tab — by
	// finding the position of the first \x1b[0m (or 0; m) reset and
	// confirming the brace appears AFTER that reset.
	resetIdx := strings.Index(out, "\x1b[m")
	if resetIdx < 0 {
		resetIdx = strings.Index(out, "\x1b[0m")
	}
	if resetIdx < 0 {
		t.Fatalf("no SGR reset found in output — gutter style not closed:\n%s", out)
	}
	braceIdx := strings.Index(out, "}")
	if braceIdx < resetIdx {
		t.Errorf("content `}` (idx=%d) appears BEFORE dim-reset (idx=%d) — content still inside dim wrapper:\n%s",
			braceIdx, resetIdx, out)
	}
}

// TestGrepPreview_PathLineDimContentDefault — Grep matches as
// `path:line:content`. The path:line: prefix is the "where" (dim),
// the content past the second colon is the "what" (default fg).
func TestGrepPreview_PathLineDimContentDefault(t *testing.T) {
	line := "internal/agent/loop.go:42:return l.Provider.Stream(ctx, req)"
	out := renderToolOutputPreview("Grep", line, false)

	const dimSGR = "38;2;160;160;160"
	if !strings.Contains(out, dimSGR) {
		t.Errorf("expected path:line prefix to use dim SGR %s; got:\n%s", dimSGR, out)
	}

	// "return" is the match content — must appear AFTER the dim region
	// closes. Verify reset comes before "return".
	resetIdx := strings.Index(out, "\x1b[m")
	if resetIdx < 0 {
		resetIdx = strings.Index(out, "\x1b[0m")
	}
	contentIdx := strings.Index(out, "return")
	if resetIdx < 0 || contentIdx < 0 {
		t.Fatalf("missing reset (idx=%d) or content (idx=%d):\n%s", resetIdx, contentIdx, out)
	}
	if contentIdx < resetIdx {
		t.Errorf("`return` (idx=%d) appears BEFORE reset (idx=%d) — content still in dim wrapper:\n%s",
			contentIdx, resetIdx, out)
	}
}

// TestBashPreview_NoDimWrapper — Bash output has no coordinate
// gutter. Whole line renders at default fg so command output is
// crisp instead of greyed-out (the regression image #23 highlighted).
func TestBashPreview_NoDimWrapper(t *testing.T) {
	out := renderToolOutputPreview("Bash", "total 42\ndrwxr-xr-x 3 ricardo staff", false)

	// No dim SGR should appear in the body lines (only the "+N more
	// lines" footer would carry styleMuted, but we passed 2 lines so
	// no footer fires).
	const dimSGR = "38;2;160;160;160"
	if strings.Contains(out, dimSGR) {
		t.Errorf("Bash preview should NOT dim-wrap body lines; got:\n%s", out)
	}
}

// TestGlobPreview_WholeLineDim — Glob lists paths with no inline
// content; the whole line is "where", so dimming the whole thing is
// correct (matches CC behavior for Glob/file-list tools).
func TestGlobPreview_WholeLineDim(t *testing.T) {
	out := renderToolOutputPreview("Glob", "internal/agent/loop.go\ninternal/agent/fork.go", false)

	const dimSGR = "38;2;160;160;160"
	if !strings.Contains(out, dimSGR) {
		t.Errorf("Glob paths should be wholly dim (the line IS the coordinate); got:\n%s", out)
	}
}
