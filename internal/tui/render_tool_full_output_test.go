package tui

import (
	"fmt"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
)

func TestExpandedToolOutputSurvivesViewportWithoutLosingText(t *testing.T) {
	for _, width := range []int{24, 40, 80, 160} {
		for textIndex, text := range []string{
			strings.Repeat("segment/", 25) + "END-OF-LONG-OUTPUT",
			strings.Repeat("完整中文路径/", 24) + "结束标记",
			strings.Repeat("e\u0301👩‍💻🙂", 40) + "END",
		} {
			for _, kind := range []string{"Bash", "Read", "Grep", "SubAgentOutput", "error", "denied", "blocked"} {
				t.Run(fmt.Sprintf("%d/%s/text-%d", width, kind, textIndex), func(t *testing.T) {
					te := ToolEvent{ID: "long-output", Kind: "result", ToolName: kind, Output: text}
					switch kind {
					case "error":
						te.ToolName, te.IsError = "Bash", true
					case "denied", "blocked":
						te.ToolName, te.IsError = "Bash", true
						te.Output = kind + ": " + text
					}
					original := te.Output
					rendered := renderToolEventAtWidth(te, true, width)
					for _, row := range strings.Split(rendered, "\n") {
						if strings.HasPrefix(stripANSI(row), "       ") && xansi.StringWidth(row) > width {
							t.Fatalf("physical row exceeds viewport %d: %q", width, stripANSI(row))
						}
					}
					visible := stripANSI(clampBlock(rendered, width))
					var reconstructed strings.Builder
					for _, row := range strings.Split(visible, "\n") {
						if rest, ok := strings.CutPrefix(row, "       "); ok {
							reconstructed.WriteString(strings.TrimPrefix(rest, "↳ "))
						}
					}
					if reconstructed.String() != text {
						t.Fatalf("expanded text lost or changed across visual wraps:\nwant %q\n got %q\n%s", text, reconstructed.String(), visible)
					}
					if te.Output != original {
						t.Fatal("rendering changed raw tool output")
					}
				})
			}
		}
	}
}

func TestSingleClippedOutputLineOffersReachableDetails(t *testing.T) {
	for _, width := range []int{24, 40, 80, 160} {
		for _, isError := range []bool{false, true} {
			te := ToolEvent{
				ID: "long-only-line", Kind: "result", ToolName: "Bash", IsError: isError,
				Output: strings.Repeat("segment/", 40) + "END-OF-LONG-OUTPUT",
			}
			visible := stripANSI(clampBlock(renderToolEventAtWidth(te, false, width), width))
			if !strings.Contains(visible, "output details") || strings.Count(visible, "(ctrl+O to expand)") != 1 {
				t.Fatalf("width %d: clipped single row has no unique details entry:\n%s", width, visible)
			}
			m := &Model{width: width + 2, toolEvents: []ToolEvent{te}}
			if ids := m.expandableToolEventIDs(); len(ids) != 1 || ids[0] != te.ID {
				t.Fatalf("clipped one-line output cannot be expanded: %v", ids)
			}
		}
	}
}
