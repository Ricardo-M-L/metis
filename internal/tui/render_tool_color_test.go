package tui

import (
	"strings"
	"testing"
)

// TestToolCallHeader_ArgsDefaultFg — the args preview inside the
// `glob(...)` parens (and equivalent for every tool) renders at default
// fg, not styleMuted. User screenshot 38 / 2026-05-18 flagged the
// path inside `glob(**/.metis/**/*.toml)` as still grey after the
// screenshot 36 pass — the args are the information the user came to
// read; only the brackets stay muted as structural chrome.
func TestToolCallHeader_ArgsDefaultFg(t *testing.T) {
	te := ToolEvent{
		Kind:     "start",
		ToolName: "Glob",
		Input:    map[string]any{"pattern": "**/.metis/**/*.toml"},
	}
	out := renderToolEvent(te, false)

	// The muted dim grey style we use for chrome (textMuted #606060).
	const mutedSGR = "38;2;96;96;96"
	args := toolArgsPreview(te.ToolName, te.Input)
	if args == "" {
		t.Skip("no args preview for this fixture — test premise invalid")
	}
	// Locate the args text and confirm the byte directly before it
	// closes any muted wrapper (so the args aren't inside one).
	idx := strings.Index(out, args)
	if idx < 0 {
		t.Fatalf("args %q missing from output:\n%s", args, out)
	}
	before := out[:idx]
	if last := strings.LastIndex(before, mutedSGR); last >= 0 {
		tail := before[last:]
		if !strings.Contains(tail, "\x1b[m") && !strings.Contains(tail, "\x1b[0m") {
			t.Errorf("args %q appears inside an unclosed muted wrapper at byte %d; got:\n%s",
				args, last, out)
		}
	}
}

// TestToolResultHeader_SummaryDefaultFg — the "✓ 0s · Read X (N lines)"
// summary text after the ✓/✗ glyph must render at default fg, not
// styleDim. User screenshot 36 / 2026-05-17 flagged the prior dim
// rendering as too low-contrast on the most-scanned line per tool
// call. The leaf glyph + ✓/✗ keep their structural colors.
func TestToolResultHeader_SummaryDefaultFg(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Input:    map[string]any{"file_path": "foo.go"},
		Output:   "   1\tpackage foo\n",
	}
	out := renderToolEvent(te, false)

	const dimSGR = "38;2;160;160;160"
	summary := summarizeToolResult(te)
	if summary == "" {
		t.Skip("summarizeToolResult returned empty — nothing to assert against")
	}
	// Locate the summary text and confirm it doesn't open with the dim
	// SGR escape. We accept dim escapes elsewhere in the output (leaf
	// glyph, gutter prefix in the body preview), but the summary itself
	// must not be inside a dim wrapper.
	idx := strings.Index(out, summary)
	if idx < 0 {
		t.Fatalf("summary %q missing from output:\n%s", summary, out)
	}
	// Walk backwards to the nearest ANSI sequence; assert it's not
	// the dim color. styleAccent (✓) closes its style before the
	// summary, so the byte directly before should NOT be dim.
	before := out[:idx]
	if last := strings.LastIndex(before, dimSGR); last >= 0 {
		// Acceptable only if a SGR reset closes it before the summary.
		tail := before[last:]
		if !strings.Contains(tail, "\x1b[m") && !strings.Contains(tail, "\x1b[0m") {
			t.Errorf("summary %q appears to be inside an unclosed dim wrapper at byte %d:\n%s",
				summary, last, out)
		}
	}
}

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

// TestGlobPreview_WholeLineDefault — Glob output IS the answer (the
// file path), so it renders at default fg, not dim. Flipped from the
// earlier "wholly dim coordinate" rule after user screenshot 36 /
// 2026-05-17: "/Users/.../restored-src/... 这些为啥还是灰色的还不是
// 白色". Treating the path as low-priority structural chrome misled
// the user into rescanning the line, since the path IS the
// information they ran the glob to get.
func TestGlobPreview_WholeLineDefault(t *testing.T) {
	out := renderToolOutputPreview("Glob", "internal/agent/loop.go\ninternal/agent/fork.go", false)

	const dimSGR = "38;2;160;160;160"
	if strings.Contains(out, dimSGR) {
		t.Errorf("Glob paths should NOT be dim-wrapped (the path IS the answer); got:\n%s", out)
	}
}
