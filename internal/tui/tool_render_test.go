package tui

import (
	"strings"
	"testing"
	"time"
)

// TestSummarizeToolResult_PerTool locks in the per-tool summary phrasing
// so the chat surface stays consistent if someone tweaks one branch.
// Cases mirror claude-code's actual transcript samples (see
// 2026-04-29 *.txt) — `Read foo.py (350 lines)`, `Added 8 lines, removed
// 4 lines`, etc.
func TestSummarizeToolResult_PerTool(t *testing.T) {
	cases := []struct {
		name     string
		te       ToolEvent
		contains []string
	}{
		{
			name: "Read with path",
			te: ToolEvent{
				ToolName: "Read",
				Input:    map[string]any{"path": "/tmp/foo.py"},
				Output:   "line1\nline2\nline3\n",
				Duration: 12 * time.Millisecond,
			},
			contains: []string{"12ms", "Read foo.py", "(3 lines)"},
		},
		{
			// go-udiff's myers algorithm groups the unchanged "c" into
			// the replacement chunk, so this single mixed edit reports
			// 3 inserts (B!, c, d) and 2 deletes (b, c). Matches the
			// unified-diff line counts claude-code displays.
			name: "Edit add+remove",
			te: ToolEvent{
				ToolName: "Edit",
				Input: map[string]any{
					"old_string": "a\nb\nc\n",
					"new_string": "a\nB!\nc\nd\n",
				},
				Duration: 5 * time.Millisecond,
			},
			contains: []string{"Added 3 lines, removed 2 lines"},
		},
		{
			name: "Write with content",
			te: ToolEvent{
				ToolName: "Write",
				Input: map[string]any{
					"path":    "/tmp/new.go",
					"content": "package x\n\nfunc Y(){}\n",
				},
				Duration: 8 * time.Millisecond,
			},
			contains: []string{"Wrote new.go", "(3 lines)"},
		},
		{
			name: "Bash first-line",
			te: ToolEvent{
				ToolName: "Bash",
				Output:   "  \n  \nhello world\nmore\n",
				Duration: 100 * time.Millisecond,
			},
			contains: []string{"100ms", "hello world"},
		},
		{
			name: "Grep match count",
			te: ToolEvent{
				ToolName: "Grep",
				Output:   "match1\nmatch2\nmatch3",
				Duration: 50 * time.Millisecond,
			},
			contains: []string{"3 matches"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeToolResult(c.te)
			for _, want := range c.contains {
				if !strings.Contains(got, want) {
					t.Errorf("summarizeToolResult: %q missing %q", got, want)
				}
			}
		})
	}
}

// TestSummarizeToolResult_ReadError — error-path Read must NOT report
// "(N lines)" since the Output is the error message, not file content.
// Pre-fix: a stat error rendered as "Read foo.go (1 lines)" which read
// like a tiny successful read; post-fix it's "Read foo.go — failed".
// Image bug 2026-05-15.
func TestSummarizeToolResult_ReadError(t *testing.T) {
	te := ToolEvent{
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/missing.go"},
		Output:   "stat /tmp/missing.go: no such file or directory",
		IsError:  true,
		Duration: 0,
	}
	got := summarizeToolResult(te)
	if strings.Contains(got, "lines)") {
		t.Errorf("error summary should NOT claim a line count; got %q", got)
	}
	if !strings.Contains(got, "missing.go") {
		t.Errorf("error summary should still surface basename; got %q", got)
	}
	if !strings.Contains(got, "failed") {
		t.Errorf("error summary should say 'failed'; got %q", got)
	}
}

// TestTruncateMiddle_PreservesBothEnds — for path-bearing error lines
// the basename at the END is what tells the user what failed; the
// pre-fix tail-cut form hid it. Middle truncation keeps both ends
// visible. Image bug 2026-05-15. Uses the same 120-rune cap that
// renderErrorBody passes in production.
func TestTruncateMiddle_PreservesBothEnds(t *testing.T) {
	long := "stat /Users/foo/Documents/公司学习文件/opensource-contributions/claude-code-sourcemap/restored-src/src/coordinator/index.ts/loop.go: no such file or directory"
	got := truncateMiddle(long, 120)
	if len([]rune(got)) > 121 {
		t.Errorf("output too long: %d runes (target ≤120)", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "stat ") {
		t.Errorf("head not preserved: %q", got)
	}
	// The basename `loop.go` and the syscall error tail must survive.
	if !strings.Contains(got, "loop.go") {
		t.Errorf("basename loop.go lost in truncation: %q", got)
	}
	if !strings.Contains(got, "no such file") {
		t.Errorf("error tail lost in truncation: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("middle ellipsis missing: %q", got)
	}
}

// TestTruncateMiddle_ShortLeavesUntouched — strings shorter than
// maxRunes pass through unchanged. Guards against silly off-by-ones.
func TestTruncateMiddle_ShortLeavesUntouched(t *testing.T) {
	in := "stat /tmp/x: no such file"
	if got := truncateMiddle(in, 120); got != in {
		t.Errorf("short input mutated:\n  in:  %q\n  out: %q", in, got)
	}
}

// TestCountEditDiff verifies our line-count math against go-udiff for
// the kinds of inputs Edit tool typically gets — pure-add, pure-remove,
// mixed, identical (no-op).
func TestCountEditDiff(t *testing.T) {
	cases := []struct {
		name         string
		old, new     string
		wantA, wantR int
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, 0},
		{"pure add", "a\n", "a\nb\nc\n", 2, 0},
		{"pure remove", "a\nb\nc\n", "a\n", 0, 2},
		// myers groups the unchanged "c" into the replacement chunk:
		// 3 inserts (B!, c, d) and 2 deletes (b, c) — matches what
		// claude-code shows for similar edits.
		{"mixed", "a\nb\nc\n", "a\nB!\nc\nd\n", 3, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, r := countEditDiff(map[string]any{
				"old_string": c.old,
				"new_string": c.new,
			})
			if a != c.wantA || r != c.wantR {
				t.Errorf("got added=%d removed=%d, want added=%d removed=%d",
					a, r, c.wantA, c.wantR)
			}
		})
	}
}

// TestFormatElapsed covers the spinner-row elapsed clock. Sub-second
// renders as ms, single-digit seconds as `X.Ys`, two-digit seconds as
// `Xs`, then the same Mm Ss / Hh Mm brackets that formatTurnDuration
// uses. The user reported the spinner reading e.g. `120s` instead of
// `2m 0s` for long turns — this regression-tests the M/H switchover.
func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{3 * time.Second, "3.0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{120 * time.Second, "2m 0s"},
		{55 * time.Minute, "55m 0s"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, c := range cases {
		if got := formatElapsed(c.d); got != c.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestFormatTurnDuration covers the three brackets in the turn-end
// summary phrasing: under a minute, under an hour, longer.
func TestFormatTurnDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{55 * time.Minute, "55m 0s"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, c := range cases {
		if got := formatTurnDuration(c.d); got != c.want {
			t.Errorf("formatTurnDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestLineCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\nb\nc\n", 3},
	}
	for _, c := range cases {
		if got := lineCount(c.in); got != c.want {
			t.Errorf("lineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"\n\nhello", "hello"},
		{"  \n  \n  real line  \nmore", "real line"},
		{"only-line", "only-line"},
	}
	for _, c := range cases {
		if got := firstNonEmptyLine(c.in); got != c.want {
			t.Errorf("firstNonEmptyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderEditDiff_TruncatesAt20Lines keeps a perf guarantee — Edit
// tools that wholesale rewrite a 200-line file shouldn't drown the chat
// surface; we cap at 20 visible lines plus a "+N more" tail.
func TestRenderEditDiff_TruncatesAt20Lines(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 50; i++ {
		oldB.WriteString("old line ")
		oldB.WriteString("X\n")
		newB.WriteString("new line ")
		newB.WriteString("Y\n")
	}
	out := renderEditDiff(map[string]any{
		"old_string": oldB.String(),
		"new_string": newB.String(),
	}, false)
	if !strings.Contains(out, "more diff lines") {
		t.Errorf("expected truncation marker; got:\n%s", out)
	}
	// Crude line-count check: count "\n" in the rendered output. The
	// actual diff body is ~20 lines plus the "+N more" tail.
	n := strings.Count(out, "\n")
	if n > 25 {
		t.Errorf("rendered too many lines (%d); cap should kick in", n)
	}
}

// TestBuildTurnRecap covers the deterministic recap synthesizer's
// behavior — short turns produce nothing, mixed-tool turns produce
// structured summaries.
func TestBuildTurnRecap(t *testing.T) {
	read := func(path string) ToolEvent {
		return ToolEvent{Kind: "result", ToolName: "Read", Input: map[string]any{"path": path}}
	}
	edit := func(path string) ToolEvent {
		return ToolEvent{Kind: "result", ToolName: "Edit", Input: map[string]any{"path": path}}
	}
	bash := func(cmd string) ToolEvent {
		return ToolEvent{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": cmd}}
	}

	cases := []struct {
		name     string
		events   []ToolEvent
		want     string
		wantNone bool
	}{
		{name: "empty", events: nil, wantNone: true},
		{name: "single tool", events: []ToolEvent{read("a.go")}, wantNone: true},
		{
			name:   "edit + read",
			events: []ToolEvent{read("a.go"), edit("b.go")},
			want:   "edited b.go · 1 reads",
		},
		{
			name:   "two edits same file",
			events: []ToolEvent{edit("foo.go"), edit("foo.go")},
			want:   "edited foo.go",
		},
		{
			name:   "bash + edit",
			events: []ToolEvent{edit("foo.go"), bash("go test ./...")},
			want:   "edited foo.go · ran `go test ./...`",
		},
		{
			name:   "many edits collapse",
			events: []ToolEvent{edit("a"), edit("b"), edit("c"), edit("d")},
			want:   "edited 4 files",
		},
		{
			name: "errors skipped",
			events: []ToolEvent{
				edit("foo.go"),
				{Kind: "result", ToolName: "Bash", IsError: true, Input: map[string]any{"command": "broken"}},
				read("bar.go"),
			},
			want: "edited foo.go · 1 reads",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildTurnRecap(c.events)
			if c.wantNone {
				if got != "" {
					t.Errorf("expected empty recap, got %q", got)
				}
				return
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRenderToolEvent_LeaderRowFormat ensures the ⏺/⎿ hierarchy holds
// on both the in-flight and completed paths.
func TestRenderToolEvent_LeaderRowFormat(t *testing.T) {
	start := renderToolEvent(ToolEvent{
		Kind:     "start",
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/foo.py"},
	}, false)
	if !strings.Contains(start, glyphBullet) {
		t.Errorf("in-flight leader missing bullet: %s", start)
	}
	if strings.Contains(start, glyphTreeLeaf) {
		t.Errorf("in-flight should not have tree-leaf yet: %s", start)
	}

	done := renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/foo.py"},
		Output:   "line1\nline2\n",
		Duration: 5 * time.Millisecond,
	}, false)
	if !strings.Contains(done, glyphBullet) {
		t.Errorf("done leader missing bullet: %s", done)
	}
	if !strings.Contains(done, glyphTreeLeaf) {
		t.Errorf("done missing tree-leaf summary: %s", done)
	}
}
