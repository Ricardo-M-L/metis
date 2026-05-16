package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestContainsMarkdownTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "gfm 2-col",
			in:   "| a | b |\n|---|---|\n| 1 | 2 |",
			want: true,
		},
		{
			name: "gfm aligned",
			in:   "| a | b | c |\n|:---|:---:|---:|\n| x | y | z |",
			want: true,
		},
		{
			name: "stray pipe in prose",
			in:   "use `a|b` to OR-match",
			want: false,
		},
		{
			name: "header without separator",
			in:   "| a | b |\n| 1 | 2 |",
			want: false,
		},
		{
			name: "empty",
			in:   "",
			want: false,
		},
		{
			name: "table after prose",
			in:   "Here is a comparison:\n\n| col | val |\n|---|---|\n| a | 1 |",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := containsMarkdownTable(tc.in)
			if got != tc.want {
				t.Fatalf("containsMarkdownTable(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestWideTableNotSquashed renders a CJK 6-column comparison table at
// terminal width 200 and asserts the rendered output uses substantially
// more than the old 120-col cap. Regression for the "half table" bug
// where capping WordWrap at 120 forced glamour to wrap every cell into
// 2-3 stacked lines (image #1 user feedback 2026-05-15).
func TestWideTableNotSquashed(t *testing.T) {
	// Reset renderer cache so this test isn't influenced by previous calls.
	mdRendererMu.Lock()
	mdRendererNarrow = nil
	mdRendererWide = nil
	mdRendererMu.Unlock()

	content := `## 核心架构对比

| 维度 | Claude Code | metis | 差距 |
|------|-------------|-------|------|
| TUI 框架 | 自研 Ink (React for terminal) | bubbletea + lipgloss，但聊天 UX 用 append-only println 模式 | 架构差异大 |
| Markdown 解析 | marked 解析 → 自写 formatToken 转 ANSI | glamour/v2 一把梭全量渲染 | 库不同 |
| 表格渲染 | 专门写了 MarkdownTable.tsx React 组件，flexbox + 按终端宽度动态分配列宽 | 走 glamour 内置渲染，列宽固定，复杂表格塞不下就截断 | 核心差距 |
`

	out := renderAssistantBody(content, 200)
	if out == "" {
		t.Fatal("renderAssistantBody returned empty")
	}

	maxLineNarrow, maxLineEA := 0, 0
	var perLine []string
	for _, ln := range strings.Split(out, "\n") {
		w := visibleWidth(ln)
		wEA := visibleWidthEA(ln)
		if w > maxLineNarrow {
			maxLineNarrow = w
		}
		if wEA > maxLineEA {
			maxLineEA = wEA
		}
		perLine = append(perLine, formatLine(w, wEA, ln))
	}

	if maxLineNarrow <= 120 {
		t.Fatalf("widest rendered line is %d cols — table still being squashed under the old 120 cap.\noutput:\n%s", maxLineNarrow, out)
	}
	// Primary assertion: in the user's actual terminal (macOS Terminal /
	// iTerm / VSCode integrated terminal with default en_US locale,
	// ambiguous-width = 1), the rendered table must fit inside the
	// 200-col window. This matches what lipgloss uses internally so
	// the column math stays consistent end-to-end.
	if maxLineNarrow > 200 {
		t.Fatalf("widest rendered line is %d narrow-cols — overflows 200-col terminal:\n%s",
			maxLineNarrow, strings.Join(perLine, "\n"))
	}
	// On a true CJK-locale terminal where ambiguous chars render as 2
	// cells (rare on macOS, common only on legacy CN/JP terminals),
	// the same table will visibly double. We don't gate the test on
	// that case — it would require flipping back to ASCIIBorder which
	// loses the Claude-Code look. Just log it for visibility.
	t.Logf("widest line: narrow=%d cols, EA=%d cols (terminal = 200 cols; narrow is what macOS shows)\n%s",
		maxLineNarrow, maxLineEA, strings.Join(perLine, "\n"))
}

// TestTableHasFullFrame verifies that the lipgloss-rendered table has
// a full Unicode box-drawing outer frame (top + bottom + left + right)
// and a header band visually distinct from body rows — matching Claude
// Code's look (image #5 user feedback 2026-05-15).
// TestTableCellInlineMarkdown asserts that `code` spans, **bold** and
// *italic* inside a table cell are rendered as ANSI rather than as
// literal `*`/backtick characters (image #6 user feedback).
func TestTableCellInlineMarkdown(t *testing.T) {
	mdRendererMu.Lock()
	mdRendererNarrow = nil
	mdRendererWide = nil
	mdRendererMu.Unlock()

	content := "| feature | impl |\n|---|---|\n| **bold** label | `Fork()` 调用 |\n"
	out := renderAssistantBody(content, 120)
	plain := stripANSI(out)

	// Literal markdown markers must be gone from the visible output.
	if strings.Contains(plain, "**") {
		t.Fatalf("cell still contains literal `**` after inline render:\n%s", plain)
	}
	if strings.Contains(plain, "`Fork()`") {
		t.Fatalf("cell still contains literal backtick-wrapped code span:\n%s", plain)
	}
	// SGR for bold (1m) must be present from `**bold**`.
	if !strings.Contains(out, "\x1b[1") {
		t.Fatalf("expected bold SGR in output, got:\n%s", out)
	}
	// `Fork()` is a function call → classifyCodeSpan picks the orange
	// Dracula colour (255,184,108).
	if !strings.Contains(out, "38;2;255;184;108") {
		t.Fatalf("expected function-call code-span orange SGR in output, got:\n%s", out)
	}
}

func TestTableHasFullFrame(t *testing.T) {
	mdRendererMu.Lock()
	mdRendererNarrow = nil
	mdRendererWide = nil
	mdRendererMu.Unlock()

	content := `| 维度 | A | B |
|------|---|---|
| 行1 | x | y |
| 行2 | a | b |
`
	out := renderAssistantBody(content, 100)
	plain := stripANSI(out)

	// Top frame must use ┌ and ┐ corners with ─ across.
	// Bottom frame must use └ and ┘ corners with ─ across.
	lines := strings.Split(strings.TrimSpace(plain), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected at least 5 lines for a framed table, got %d:\n%s", len(lines), plain)
	}
	top := strings.TrimSpace(lines[0])
	bot := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(top, "┌") || !strings.HasSuffix(top, "┐") {
		t.Fatalf("first line is not a Unicode top frame: %q", top)
	}
	if !strings.HasPrefix(bot, "└") || !strings.HasSuffix(bot, "┘") {
		t.Fatalf("last line is not a Unicode bottom frame: %q", bot)
	}

	// Header row: bold (no background — image #17 user feedback). The
	// only way to spot the header in the rendered output is the
	// bold SGR `\x1b[1` followed by white foreground 255;255;255 around
	// the header cell content.
	if !strings.Contains(out, "\x1b[1") {
		t.Fatalf("expected bold SGR for header row, got:\n%s", out)
	}
	if !strings.Contains(out, "38;2;255;255;255") {
		t.Fatalf("expected white header fg SGR (38;2;255;255;255), got:\n%s", out)
	}

	// Body rows must use │ vertical separators.
	bodyRows := 0
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "│") && strings.Count(ln, "│") >= 3 {
			bodyRows++
		}
	}
	if bodyRows < 3 { // header + 2 body rows = at least 3
		t.Fatalf("expected at least 3 │-piped rows (header + body), got %d:\n%s", bodyRows, plain)
	}
}

func formatLine(narrow, ea int, ln string) string {
	plain := stripANSI(ln)
	if len(plain) > 80 {
		plain = plain[:80] + "..."
	}
	return "[narrow=" + intToStr(narrow) + " EA=" + intToStr(ea) + "] " + plain
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// visibleWidth strips ANSI escape sequences and returns the cell width
// (CJK-aware via go-runewidth, already a dep of metis). Uses default
// East Asian Width = Narrow mode so ambiguous-width chars like the
// box-drawing ─ (U+2500) count as 1 cell — matches a Western locale
// terminal. On an East-Asian-Width=Wide terminal the same row will
// display ~2x wider; that's a separate locale concern.
func visibleWidth(s string) int {
	c := runewidth.NewCondition()
	c.EastAsianWidth = false
	return c.StringWidth(stripANSI(s))
}

// visibleWidthEA returns the cell width assuming East-Asian locale
// (ambiguous chars = 2 cells). This is what users with CN/JP terminals
// will see, and what determines actual overflow on those locales.
func visibleWidthEA(s string) int {
	c := runewidth.NewCondition()
	c.EastAsianWidth = true
	return c.StringWidth(stripANSI(s))
}
