package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestToolDetailShortcutSurvivesViewportClamp(t *testing.T) {
	const shortcut = "(ctrl+O to expand)"
	for _, width := range []int{24, 40, 80} {
		for _, result := range []struct {
			name, prefix string
			isError      bool
		}{
			{name: "success"},
			{name: "error", isError: true},
			{name: "denied", prefix: "denied: ", isError: true},
		} {
			for _, hiddenOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("width=%d/%s/hiddenOutput=%t", width, result.name, hiddenOutput), func(t *testing.T) {
					output := "one"
					if hiddenOutput {
						output += "\ntwo\nthree\nfour\nfive\nsix\nseven"
					}
					te := ToolEvent{
						ID: "narrow-detail", Kind: "end", ToolName: "Bash",
						Input:  map[string]any{"command": "printf one", "description": "Inspect records"},
						Output: result.prefix + output, IsError: result.isError,
					}
					// The real viewport clamps physical rows after tool rendering.
					// Inspecting the raw renderer alone misses a clipped shortcut.
					rendered := renderToolEventAtWidth(te, false, width)
					visible := stripANSI(clampBlock(rendered, width))
					if count := strings.Count(visible, shortcut); count != 1 {
						t.Fatalf("shortcut must remain complete and appear once after viewport clipping, got %d:\n%s", count, visible)
					}
					if count := strings.Count(visible, "ctrl+O"); count != 1 {
						t.Fatalf("viewport contains duplicate or partial shortcut hints:\n%s", visible)
					}
					m := &Model{width: width + 2, toolEvents: []ToolEvent{te}}
					if ids := m.expandableToolEventIDs(); len(ids) != 1 || ids[0] != te.ID {
						t.Fatalf("visible detail entry is not a reachable expansion target: %v", ids)
					}
					expanded := stripANSI(clampBlock(renderToolEventAtWidth(te, true, width), width))
					if strings.Contains(expanded, "ctrl+O") || !strings.Contains(expanded, "printf one") {
						t.Fatalf("expanded view lost the command or still advertises hidden detail:\n%s", expanded)
					}
					if hiddenOutput && !strings.Contains(expanded, "seven") {
						t.Fatalf("expanded view lost the formerly hidden output:\n%s", expanded)
					}
				})
			}
		}
	}
}
