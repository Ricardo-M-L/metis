package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/tui/i18n"
)

func TestRunningToolHasExplicitLocalizedStateWithinViewport(t *testing.T) {
	t.Cleanup(func() { i18n.SetLocale("") })
	for locale, label := range map[string]string{"en": "running", "zh-CN": "运行中"} {
		i18n.SetLocale(locale)
		for _, tool := range []string{"Agent", "Bash", "SubAgentOutput"} {
			for _, nested := range []bool{false, true} {
				for _, width := range []int{24, 40, 80, 160} {
					t.Run(fmt.Sprintf("%s/%s/nested=%t/width=%d", locale, tool, nested, width), func(t *testing.T) {
						input := map[string]any{"description": "核对会话布局", "command": "printf ready", "agent_id": "fixture-agent"}
						event := ToolEvent{Kind: "start", ToolName: tool, Input: input}
						if nested {
							event.ToolName, event.SubAgentParentID = "sub: "+tool, "parent-agent"
						}
						rendered := renderToolEventAtWidth(event, false, width)
						visible := stripANSI(clampBlock(rendered, width))
						if strings.Count(visible, "\n") != 1 || !strings.HasSuffix(visible, " · "+label+"\n") {
							t.Fatalf("running state was clipped, ambiguous, or moved to another row:\n%s", visible)
						}
						if xansi.StringWidth(strings.TrimSuffix(rendered, "\n")) > width {
							t.Fatalf("running header exceeds viewport %d: %q", width, visible)
						}
						if strings.Contains(visible, glyphTreeLeaf) || !strings.Contains(rendered, styleDim.Render(" · "+label)) {
							t.Fatalf("running state gained a result row or lost its theme style:\n%q", rendered)
						}
						if width == 160 && strings.Contains(visible, "…") {
							t.Fatalf("unclipped running header still carries an ellipsis:\n%s", visible)
						}
						event.Kind = "end"
						if strings.Contains(stripANSI(renderToolEventAtWidth(event, false, width)), " · "+label) {
							t.Fatal("finished tool retained the running label")
						}
					})
				}
			}
		}
	}
}

func TestTaskSuccessConfirmationAppearsOnce(t *testing.T) {
	for _, tc := range []struct{ tool, output string }{
		{"TaskUpdate", "Updated task #7 status"},
		{"TaskCreate", "Task #7 created successfully: Verify layout"},
		{"TaskOutput", "Appended 12 bytes of output to task #7"},
		{"TaskStop", "Task #7 deleted"},
		{"TaskList", "(no tasks)"},
		{"TodoRead", "(no tasks yet — call TodoWrite to create one)"},
		{"GoalCreate", "Goal #1 created: Release [high]"},
		{"GoalUpdate", "Goal #1 updated: Release (priority=high, status=active)"},
		{"GoalDelete", "Goal #1 deleted"},
		{"GoalList", "(no goals)"},
	} {
		for _, nested := range []bool{false, true} {
			for _, expanded := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/nested=%t/expanded=%t", tc.tool, nested, expanded), func(t *testing.T) {
					event := ToolEvent{ID: "task-fixture", Kind: "end", ToolName: tc.tool,
						Output: tc.output, Duration: 8 * time.Millisecond}
					if nested {
						event.ToolName, event.SubAgentParentID = "sub: "+tc.tool, "parent-agent"
					}
					visible := strings.TrimRight(stripANSI(renderToolEventAtWidth(event, expanded, 100)), "\n")
					if strings.Count(visible, tc.output) != 1 || len(strings.Split(visible, "\n")) != 2 {
						t.Fatalf("short success confirmation should occupy one result row:\n%s", visible)
					}
					if summarizeToolResult(event) != "8ms · "+tc.output || event.Output != tc.output {
						t.Fatal("deduplication changed the exported summary or original result")
					}
					compact := stripANSI(renderToolEventPresentation(event, false, 100, false))
					if !strings.Contains(compact, "8ms · "+tc.output) {
						t.Fatalf("minimal mode lost its standalone confirmation:\n%s", compact)
					}
				})
			}
		}
	}
}

func TestTaskConfirmationOnNarrowViewportRemainsReachableAndComplete(t *testing.T) {
	const output = "Updated task #123456789 status"
	for _, width := range []int{24, 40} {
		event := ToolEvent{ID: "narrow-task", Kind: "end", ToolName: "sub: TaskUpdate",
			SubAgentParentID: "parent-agent", Output: output}
		collapsed := stripANSI(clampBlock(renderToolEventAtWidth(event, false, width), width))
		if strings.Count(collapsed, "Updated") != 1 {
			t.Fatalf("narrow confirmation still repeats its summary above the body:\n%s", collapsed)
		}
		model := &Model{width: width + 2, toolEvents: []ToolEvent{event}}
		if ids := model.expandableToolEventIDs(); len(ids) != 1 || ids[0] != event.ID {
			t.Fatalf("clipped confirmation cannot be expanded at width %d: %v", width, ids)
		}
		expanded := stripANSI(clampBlock(renderToolEventAtWidth(event, true, width), width))
		var body strings.Builder
		for _, row := range strings.Split(expanded, "\n") {
			if content, ok := strings.CutPrefix(row, "           "); ok {
				body.WriteString(strings.TrimPrefix(content, "↳ "))
			}
		}
		if body.String() != output {
			t.Fatalf("narrow expanded confirmation lost original text: %q\n%s", body.String(), expanded)
		}
	}
}

func TestTaskDetailsAndFailuresKeepTheirOriginalBody(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		isError      bool
	}{
		{"verifier nudge", "Updated task #7 status\n\nNUDGE: completed tasks have no verify step.\nRun the verifier.\nWait for its verdict.\nDo not replace the verdict.\nFINAL VERIFICATION EVIDENCE", false},
		{"long single line", "Updated task #7 status: " + strings.Repeat("完整详情/", 30) + "EVIDENCE-END", false},
		{"failure", "task #7 update failed\npermission denied\nretry details", true},
	} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", tc.name, nested), func(t *testing.T) {
				event := ToolEvent{ID: "task-details", Kind: "result", ToolName: "TaskUpdate", Output: tc.output, IsError: tc.isError}
				indent := "       "
				if nested {
					event.ToolName, event.SubAgentParentID = "sub: TaskUpdate", "parent-agent"
					indent += "    "
				}
				visible := stripANSI(clampBlock(renderToolEventAtWidth(event, true, 100), 100))
				var body strings.Builder
				for _, row := range strings.Split(visible, "\n") {
					if content, ok := strings.CutPrefix(row, indent); ok {
						body.WriteString(strings.TrimPrefix(content, "↳ "))
					}
				}
				if body.String() != strings.ReplaceAll(tc.output, "\n", "") {
					t.Fatalf("detailed task result lost body content:\n%s", visible)
				}
				if tc.isError && !strings.Contains(visible, "✗") {
					t.Fatalf("failed task update lost its failure indicator:\n%s", visible)
				}
				if event.Output != tc.output || event.IsError != tc.isError {
					t.Fatal("presentation changed persisted result or failure state")
				}
			})
		}
	}
}
