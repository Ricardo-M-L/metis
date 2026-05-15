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
	// On East-Asian locale terminals (CN/JP/KR), ambiguous-width chars
	// like ─│┼ render as 2 cells. metisCodeBlockStyle forces ASCII
	// separators (- | +) which are always 1 cell, so the rendered
	// table must fit inside a 200-col terminal in both locales.
	if maxLineEA > 200 {
		t.Fatalf("widest rendered line on EA locale is %d cols — overflows 200-col terminal, table will wrap ugly on CN/JP terminals.\n%s",
			maxLineEA, strings.Join(perLine, "\n"))
	}
	t.Logf("widest line: narrow=%d cols, EA=%d cols (terminal = 200 cols)\n%s",
		maxLineNarrow, maxLineEA, strings.Join(perLine, "\n"))
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
