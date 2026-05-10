package tui

// render_thinking_reflow_test.go — locks the width-aware truncation
// of the folded "thinking" block. Before this fix, firstThinkingLine
// was hardcoded to 80 runes and the "(ctrl+o to expand)" suffix was
// appended unconditionally, so a narrow terminal saw the row wrap and
// the hint orphaned over the next message (image #7/#8 user reports
// 2026-05-10). claude-code's wrap-text re-measures per frame; metis
// now mirrors that by sizing against the live width.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFirstThinkingLine_RespectsWidth(t *testing.T) {
	body := strings.Repeat("a", 200)
	// Wide terminal: budget = 120 - 4 - 20 = 96 runes.
	wide := firstThinkingLine(body, 120)
	if got := ansi.StringWidth(wide); got > 96 {
		t.Errorf("wide width: got %d visible chars, want <=96", got)
	}
	// Narrow terminal: hint won't fit, so budget collapses to width-margin.
	narrow := firstThinkingLine(body, 30)
	if got := ansi.StringWidth(narrow); got > 30 {
		t.Errorf("narrow width: got %d visible chars, want <=30", got)
	}
}

func TestFirstThinkingLine_LegacyDefault(t *testing.T) {
	// width=0 (cold-render path before WindowSizeMsg) keeps the old
	// 80-rune cap so existing tests + tests building Models without a
	// real terminal don't break.
	body := strings.Repeat("a", 200)
	got := firstThinkingLine(body, 0)
	if w := ansi.StringWidth(got); w != 80 {
		t.Errorf("legacy width=0: got %d visible chars, want 80", w)
	}
}

func TestThinkingHintFits(t *testing.T) {
	cases := []struct {
		name  string
		width int
		want  bool
	}{
		{"wide", 120, true},
		{"medium", 60, true},
		{"narrow", 30, false},
		{"tiny", 20, false},
		{"legacy zero stays compatible", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := thinkingHintFits(c.width); got != c.want {
				t.Errorf("thinkingHintFits(%d) = %v, want %v", c.width, got, c.want)
			}
		})
	}
}

func TestRenderMessage_ThinkingFitsRow(t *testing.T) {
	// Build a thinking message and render it folded at varying widths.
	// The whole rendered row (after stripping ANSI) must not exceed
	// the requested width — that's what stops the (ctrl+o to expand)
	// hint from wrapping onto a phantom row over the next chat item.
	msg := Message{Role: "thinking", Content: strings.Repeat("the user is asking what model I'm using ", 8)}
	for _, width := range []int{40, 60, 80, 100, 120} {
		out := renderMessage(msg, width, false)
		row := strings.SplitN(out, "\n", 2)[0]
		w := ansi.StringWidth(row)
		if w > width {
			t.Errorf("width=%d: rendered row visible width=%d (overflows by %d)\n%q",
				width, w, w-width, row)
		}
	}
}

func TestRenderMessage_ThinkingFitsRow_CJK(t *testing.T) {
	// CJK regression: doublewidth chars used to overshoot because the
	// truncation budget was rune-based. Now firstThinkingLine measures
	// in terminal cells via truncateCells.
	msg := Message{Role: "thinking", Content: strings.Repeat("用户询问关于编程语言的问题", 8)}
	for _, width := range []int{40, 60, 80, 100, 120} {
		out := renderMessage(msg, width, false)
		row := strings.SplitN(out, "\n", 2)[0]
		w := ansi.StringWidth(row)
		if w > width {
			t.Errorf("width=%d: CJK row visible width=%d (overflows by %d)\n%q",
				width, w, w-width, row)
		}
	}
}

func TestTruncateCells_HonorsCJKWidth(t *testing.T) {
	// Each Chinese char is 2 cells. Want at most maxCells visible
	// width, including the "…" tail when truncated.
	body := "用户问的是关于Go语言的协程实现细节"
	got := truncateCells(body, 20)
	if w := ansi.StringWidth(got); w > 20 {
		t.Errorf("truncateCells(20): got %q (width=%d), want <=20", got, w)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated string missing ellipsis: %q", got)
	}
	// Short input should be returned as-is.
	short := "abc"
	if got := truncateCells(short, 80); got != short {
		t.Errorf("truncateCells short: got %q, want %q", got, short)
	}
}
