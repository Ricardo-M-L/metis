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

	maxLine := 0
	for _, ln := range strings.Split(out, "\n") {
		w := visibleWidth(ln)
		if w > maxLine {
			maxLine = w
		}
	}

	if maxLine <= 120 {
		t.Fatalf("widest rendered line is %d cols — table still being squashed under the old 120 cap.\noutput:\n%s", maxLine, out)
	}
	t.Logf("widest rendered line = %d cols (target: > 120)\n%s", maxLine, out)
}

// visibleWidth strips ANSI escape sequences and returns the cell width
// (CJK-aware via go-runewidth, already a dep of metis).
func visibleWidth(s string) int {
	return runewidth.StringWidth(stripANSI(s))
}
