package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
)

func TestSubAgentToolNestsCompleteStyledBlock(t *testing.T) {
	command := "python3 - <<'PY'\nprint('synthetic fixture')\nPY"
	output := strings.Repeat("fixture evidence\n", 8)
	for _, event := range []ToolEvent{
		{Kind: "start", ToolName: "Bash", Input: map[string]any{"command": command, "description": "检查示例记录"}},
		{Kind: "end", ToolName: "Bash", Input: map[string]any{"command": command, "description": "检查示例记录"}, Output: output},
		{Kind: "end", ToolName: "Bash", Input: map[string]any{"command": command}, IsError: true, Output: "fatal: " + output},
		{Kind: "end", ToolName: "Bash", Input: map[string]any{"command": command}, IsError: true, Output: "denied: " + output},
		{Kind: "end", ToolName: "Bash", Input: map[string]any{"command": command}, IsError: true, Output: "[blocked] " + output},
		{Kind: "end", ToolName: "Edit", Input: map[string]any{"path": "fixture.txt", "old": "old evidence", "new": output}},
		{Kind: "end", ToolName: "Write", Input: map[string]any{"path": "fixture.txt", "content": output}},
		{Kind: "end", ToolName: "Read", Input: map[string]any{"path": "fixture.txt"}, Output: output},
	} {
		for _, width := range []int{24, 40, 80, 160} {
			for _, expanded := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t/%d/%t", event.ToolName, event.Kind, event.IsError, width, expanded), func(t *testing.T) {
					parentRows := strings.Split(renderToolEventAtWidth(event, expanded, width-4), "\n")
					child := event
					child.SubAgentParentID, child.ToolName = "fixture-parent", "sub: "+event.ToolName
					childRows := strings.Split(renderToolEventAtWidth(child, expanded, width), "\n")
					if len(childRows) != len(parentRows) {
						t.Fatalf("nested tool changed the block's row structure: %d vs %d", len(childRows), len(parentRows))
					}
					for i, row := range childRows {
						want := parentRows[i]
						if want != "" {
							want = "    " + want
						}
						// Compare styled bytes, so nesting cannot strip error colors,
						// syntax highlighting, or diff backgrounds from any row.
						if row != want {
							t.Fatalf("row %d did not inherit the same four-cell indent:\nwant %q\n got %q", i, want, row)
						}
					}
				})
			}
		}
	}
}

func TestSubAgentExpandedEvidenceSurvivesViewport(t *testing.T) {
	text := strings.Repeat("完整路径/e\u0301👩‍💻/", 30) + "EVIDENCE-END"
	for _, width := range []int{24, 40, 80, 160} {
		for _, kind := range []string{"Bash", "error", "denied", "blocked", "Read", "Write", "Edit", "command"} {
			t.Run(fmt.Sprintf("%d/%s", width, kind), func(t *testing.T) {
				event := ToolEvent{Kind: "end", ToolName: kind, SubAgentParentID: "fixture-parent", Output: text}
				want := map[string]string{"body": text}
				switch kind {
				case "error", "denied", "blocked":
					event.ToolName, event.IsError = "Bash", true
					if kind != "error" {
						event.Output = kind + ": " + text
					}
				case "Write":
					event.Input = map[string]any{"path": "fixture.txt", "content": text}
					event.Output, want = "", map[string]string{"+": text}
				case "Edit":
					event.Input = map[string]any{"path": "fixture.txt", "old": "OLD-" + text, "new": "NEW-" + text}
					event.Output, want = "", map[string]string{"-": "OLD-" + text, "+": "NEW-" + text}
				case "command":
					event.ToolName, event.Output = "Bash", ""
					event.Input = map[string]any{"command": text, "description": "检查示例"}
				}
				event.ToolName = "sub: " + event.ToolName
				original := event
				rendered := renderToolEventAtWidth(event, true, width)
				visible := stripANSI(clampBlock(rendered, width))
				got := map[string]string{}
				for _, row := range strings.Split(visible, "\n") {
					row = strings.TrimPrefix(row, "    ")
					if kind == "Write" || kind == "Edit" {
						if match := diffPreviewRowPattern.FindStringSubmatch(row); match != nil {
							got[match[1]] += strings.TrimRight(match[2], " ")
						}
					} else if body, ok := strings.CutPrefix(row, "       "); ok && body != "command:" && !strings.HasPrefix(body, " ") {
						got["body"] += strings.TrimPrefix(body, "↳ ")
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("nested expanded evidence was lost after viewport clipping:\nwant %#v\n got %#v\n%s", want, got, visible)
				}
				if !reflect.DeepEqual(event, original) {
					t.Fatal("rendering changed the raw tool event")
				}
			})
		}
	}
}

func TestQuietBashCombinesDurationAndReachableCommandDetails(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, width := range []int{24, 40, 80, 160} {
			t.Run(fmt.Sprintf("nested=%t/width=%d", nested, width), func(t *testing.T) {
				event := ToolEvent{ID: "quiet-fixture", Kind: "end", ToolName: "Bash", Duration: 65 * time.Millisecond,
					Input: map[string]any{"command": "true", "description": "Check fixture"}}
				if nested {
					event.ToolName, event.SubAgentParentID = "sub: Bash", "fixture-parent"
				}
				rendered := renderToolEventAtWidth(event, false, width)
				visible := strings.TrimSpace(stripANSI(clampBlock(rendered, width)))
				if strings.Contains(visible, "No output") || strings.Contains(visible, "command details") {
					t.Fatalf("quiet success retained a redundant standalone label:\n%s", visible)
				}
				if !strings.Contains(visible, "65ms") || strings.Count(visible, "(ctrl+O to expand)") != 1 {
					t.Fatalf("quiet success lost its duration or unique detail shortcut:\n%s", visible)
				}
				if width >= 40 && len(strings.Split(visible, "\n")) != 2 {
					t.Fatalf("quiet success should use only its title and auxiliary row:\n%s", visible)
				}
				for _, row := range strings.Split(rendered, "\n") {
					if xansi.StringWidth(row) > width {
						t.Fatalf("quiet success exceeds %d columns: %q", width, row)
					}
				}
				model := &Model{width: width + 2, toolEvents: []ToolEvent{event}}
				if ids := model.expandableToolEventIDs(); len(ids) != 1 || ids[0] != event.ID {
					t.Fatalf("quiet command is no longer reachable with Ctrl+O: %v", ids)
				}
				expanded := stripANSI(clampBlock(renderToolEventAtWidth(event, true, width), width))
				if !strings.Contains(expanded, "command:") || !strings.Contains(expanded, "true") || strings.Contains(expanded, "ctrl+O") {
					t.Fatalf("expanding quiet success did not reveal its original command:\n%s", expanded)
				}
				// Compact/minimal mode retains its standalone summary and has
				// no details viewer, regardless of the rich transcript's layout.
				compact := stripANSI(renderToolEventPresentation(event, false, 160, false))
				if !strings.Contains(compact, summarizeToolResult(event)) || strings.Contains(compact, "ctrl+O") {
					t.Fatalf("quiet command changed compact summary semantics:\n%s", compact)
				}
			})
		}
	}
}

func TestQuietBashFailureRemainsExplicitInNarrowNestedBlock(t *testing.T) {
	for _, width := range []int{24, 40, 80, 160} {
		for _, expanded := range []bool{false, true} {
			event := ToolEvent{Kind: "end", ToolName: "sub: Bash", SubAgentParentID: "fixture-parent", IsError: true,
				Input: map[string]any{"command": "false", "description": "Check fixture"}, Duration: 65 * time.Millisecond}
			visible := stripANSI(clampBlock(renderToolEventAtWidth(event, expanded, width), width))
			prose := strings.Join(strings.Fields(visible), " ")
			if !strings.Contains(prose, "✗ 65ms · Failed (no diagnostic output)") {
				t.Fatalf("width=%d expanded=%t: quiet failure lost its status or missing diagnostic explanation:\n%s", width, expanded, visible)
			}
		}
	}
}
