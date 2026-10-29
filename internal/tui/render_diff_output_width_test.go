package tui

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/themes"
)

var diffPreviewRowPattern = regexp.MustCompile(`^ {7}(?: *[0-9]+| *↳) ([+\-]) (.*)$`)

func TestExpandedDiffPreviewPreservesFullContentAfterViewportClamp(t *testing.T) {
	for _, width := range []int{24, 40, 80, 160} {
		for textIndex, text := range []string{
			strings.Repeat("long-segment/", 24) + "END-OF-CODE",
			strings.Repeat("完整中文内容/", 24) + "尾部完整",
			strings.Repeat("e\u0301👩‍💻🙂", 40) + "END",
		} {
			for _, tool := range []string{"Write", "Edit"} {
				t.Run(fmt.Sprintf("%d/%s/text-%d", width, tool, textIndex), func(t *testing.T) {
					input := map[string]any{"path": "fixture.txt", "content": text}
					want := map[string]string{"+": text}
					if tool == "Edit" {
						input = map[string]any{"path": "fixture.txt", "old": "OLD-" + text, "new": "NEW-" + text}
						want = map[string]string{"-": "OLD-" + text, "+": "NEW-" + text}
					}
					original := make(map[string]any, len(input))
					for key, value := range input {
						original[key] = value
					}
					te := ToolEvent{ID: "full-diff", Kind: "result", ToolName: tool, Input: input}
					rendered := renderToolEventAtWidth(te, true, width)
					for _, row := range strings.Split(rendered, "\n") {
						if diffPreviewRowPattern.MatchString(stripANSI(row)) && xansi.StringWidth(row) != width {
							t.Fatalf("diff background must fill one physical row of width %d, got %d: %q", width, xansi.StringWidth(row), stripANSI(row))
						}
					}
					visible := stripANSI(clampBlock(rendered, width))
					got := map[string]string{}
					for _, row := range strings.Split(visible, "\n") {
						if match := diffPreviewRowPattern.FindStringSubmatch(row); match != nil {
							got[match[1]] += strings.TrimRight(match[2], " ")
						}
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("expanded diff lost text or grapheme across physical rows:\nwant %#v\n got %#v\n%s", want, got, visible)
					}
					if !reflect.DeepEqual(input, original) {
						t.Fatal("rendering changed raw tool input")
					}
				})
			}
		}
	}
}

func TestCollapsedDiffPreviewAlwaysOffersReachableHiddenDetails(t *testing.T) {
	for _, width := range []int{24, 40, 80, 160} {
		for _, tool := range []string{"Write", "Edit"} {
			for _, manyLines := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/many-%v", width, tool, manyLines), func(t *testing.T) {
					text := strings.Repeat("long-content-", 30)
					if manyLines {
						text = strings.Repeat("line\n", 30)
					}
					input := map[string]any{"path": "fixture.txt", "content": text}
					if tool == "Edit" {
						input = map[string]any{"path": "fixture.txt", "old": "before", "new": text}
					}
					te := ToolEvent{ID: "hidden-diff", Kind: "result", ToolName: tool, Input: input}
					visible := stripANSI(clampBlock(renderToolEventAtWidth(te, false, width), width))
					if strings.Count(visible, "(ctrl+O to expand)") != 1 {
						t.Fatalf("hidden diff needs exactly one visible details entry:\n%s", visible)
					}
					m := &Model{width: width + 2, toolEvents: []ToolEvent{te}}
					if ids := m.expandableToolEventIDs(); len(ids) != 1 || ids[0] != te.ID {
						t.Fatalf("hidden diff cannot be reached by Ctrl+O: %v", ids)
					}
				})
			}
		}
	}
}

func TestWrappedDiffKeepsWordHighlightsAndBackgroundOnEveryPhysicalRow(t *testing.T) {
	width := 24
	rendered := renderEditDiffAtWidth(map[string]any{
		"old": "prefix-" + strings.Repeat("A", 45) + "-end",
		"new": "prefix-" + strings.Repeat("B", 45) + "-end",
	}, true, width)
	for _, row := range strings.Split(rendered, "\n") {
		match := diffPreviewRowPattern.FindStringSubmatch(stripANSI(row))
		if match == nil {
			continue
		}
		bg, fg, changed := themes.Current().DiffAddBg, themes.Current().DiffAddFg, "B"
		if match[1] == "-" {
			bg, fg, changed = themes.Current().DiffDelBg, themes.Current().DiffDelFg, "A"
		}
		if !strings.Contains(row, bgAnsiPrefix(bg)) || xansi.StringWidth(row) != width {
			t.Fatalf("physical diff row lost its full background: %q", row)
		}
		if strings.Contains(match[2], changed) {
			strong := lipgloss.NewStyle().Background(bg).Foreground(fg).Bold(true).Render(changed)
			prefix, _, _ := strings.Cut(strong, changed)
			if !strings.Contains(row, prefix) {
				t.Fatalf("wrapped changed text lost word-level color/bold: %q", row)
			}
		}
	}
}
