package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
)

func spinnerWidthFixture(width int, preview string) *Model {
	m := &Model{
		width:            width,
		spinnerActive:    true,
		spinnerStartedAt: time.Now().Add(-43 * time.Second),
		spinnerVerb:      "核对压缩核心算法",
		spinnerSub:       preview,
		spinnerPhase:     "requesting",
	}
	m.totalTokens.add(192, 0, 0, 0)
	return m
}

func spinnerMainRow(t *testing.T, m *Model) string {
	t.Helper()
	output := renderSpinnerStatus(m)
	rows := strings.Split(output, "\n")
	if len(rows) != 3 || rows[0] != "" || rows[2] != "" {
		t.Fatalf("spinner must occupy one logical row: %q", output)
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	if got := xansi.StringWidth(rows[1]); got > width {
		t.Fatalf("spinner is %d cells in a %d-cell pane: %q", got, width, rows[1])
	}
	return xansi.Strip(rows[1])
}

func TestSpinnerWidthBudgetUsesWideTerminalAndPreservesOriginalPreview(t *testing.T) {
	preview := "Agent · /Users/ricardo/Documents/公司学习文件/metis/internal/context/compaction_algorithm.go"
	m := spinnerWidthFixture(180, preview)
	row := spinnerMainRow(t, m)
	if !strings.Contains(row, preview) || strings.Contains(row, "…") {
		t.Fatalf("wide spinner clipped a preview that fits: %q", row)
	}
	if !strings.Contains(row, "(43s · ↑ 192 tokens)") {
		t.Fatalf("wide spinner lost its complete timing/counter: %q", row)
	}
	if m.spinnerSub != preview {
		t.Fatalf("renderer mutated source preview: %q", m.spinnerSub)
	}
}

func TestSpinnerWidthBudgetPreservesTaskTailAndStatus(t *testing.T) {
	preview := "Agent · /Users/ricardo/Documents/公司学习文件/" + strings.Repeat("research/", 12) + "algorithm.go"
	for _, width := range []int{40, 60, 80, 100, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := spinnerWidthFixture(width, preview)
			row := spinnerMainRow(t, m)
			if !strings.Contains(row, "43s") || !strings.Contains(row, "↑") || !strings.Contains(row, "192") {
				t.Fatalf("task text displaced timing/counter: %q", row)
			}
			if !strings.Contains(row, "…") || !strings.Contains(row, "algorithm.go") {
				t.Fatalf("middle shortening lost the task filename: %q", row)
			}
			if m.spinnerSub != preview {
				t.Fatal("renderer changed original preview")
			}
		})
	}
}

func TestSpinnerWidthBudgetDropsOptionalThoughtBeforeClockAndTokens(t *testing.T) {
	m := spinnerWidthFixture(40, strings.Repeat("large task ", 20)+"target.go")
	m.firstStreamAt = m.spinnerStartedAt.Add(8 * time.Second)
	row := spinnerMainRow(t, m)
	if strings.Contains(row, "thought for") || !strings.Contains(row, "43s") || !strings.Contains(row, "192") {
		t.Fatalf("optional thought time crowded out essential status: %q", row)
	}
	m.width = 240
	row = spinnerMainRow(t, m)
	if !strings.Contains(row, "thought for 8s") {
		t.Fatalf("wide status lost supplementary timing: %q", row)
	}
}

func TestSpinnerWidthBudgetNarrowCJKAndGraphemesStayOnOneRow(t *testing.T) {
	for _, preview := range []string{
		strings.Repeat("中文路径/", 30) + "最后文件.go",
		strings.Repeat("👩🏽‍💻/", 40) + "END.go",
		strings.Repeat("e\u0301/", 40) + "END.go",
	} {
		for width := 1; width <= 100; width++ {
			m := spinnerWidthFixture(width, preview)
			row := spinnerMainRow(t, m)
			if width >= 20 && (!strings.Contains(row, "43s") || !strings.Contains(row, "↑") || !strings.Contains(row, "192")) {
				t.Fatalf("%d columns lost clock or token count: %q", width, row)
			}
			withoutEmoji := strings.ReplaceAll(row, "👩🏽‍💻", "")
			if strings.ContainsAny(withoutEmoji, "👩🏽\u200d💻") {
				t.Fatalf("%d columns split a ZWJ grapheme: %q", width, row)
			}
			if strings.Contains(strings.ReplaceAll(row, "e\u0301", ""), "\u0301") {
				t.Fatalf("%d columns orphaned a combining mark: %q", width, row)
			}
		}
	}
}

func TestSpinnerWidthBudgetSanitizesOnlyPresentation(t *testing.T) {
	preview := "python3 - <<'PY'\r\n\tprint('中文 👩🏽‍💻')\nPY\x1b[31m DONE\x1b[0m\x07"
	m := spinnerWidthFixture(180, preview)
	row := spinnerMainRow(t, m)
	if strings.ContainsAny(row, "\r\t\a") || !strings.Contains(row, "print('中文 👩🏽‍💻') PY DONE") {
		t.Fatalf("multiline tool preview did not become one safe row: %q", row)
	}
	if m.spinnerSub != preview {
		t.Fatal("presentation sanitizer mutated tool input")
	}
}

func TestSpinnerWidthBudgetDoesNotGuessPathMeaning(t *testing.T) {
	for _, preview := range []string{
		"/Users/ricardo/Documents/project/external task description",
		"Bash · cd /Users/other/project && inspect --output /tmp/result.json",
		"Agent · 比较 /Users/ricardo 与 /Users/other 的访问策略",
		"Bash · printf '%s' 'a  b' && echo 'c   d'",
	} {
		row := spinnerMainRow(t, spinnerWidthFixture(240, preview))
		if !strings.Contains(row, preview) {
			t.Fatalf("renderer rewrote a legitimate path/command/description: %q", row)
		}
	}
	spinnerMainRow(t, spinnerWidthFixture(0, strings.Repeat("long path/", 20)))
}
