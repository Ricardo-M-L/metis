package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestReviewerOutputRecoveryCumulativePrefixAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segments [][]llm.StreamEvent
		want     string
	}{
		{"exact_previous_prefix", [][]llm.StreamEvent{
			agentRecoverySegment("PART_A ", "max_tokens"),
			agentRecoverySegment("PART_A FINAL_TAIL", "end_turn"),
		}, "PART_A FINAL_TAIL"},
		{"cumulative_prefix", [][]llm.StreamEvent{
			agentRecoverySegment("PART_A ", "max_tokens"),
			agentRecoverySegment("PART_B ", "max_tokens"),
			agentRecoverySegment("PART_A PART_B FINAL_TAIL", "end_turn"),
		}, "PART_A PART_B FINAL_TAIL"},
		{"short_matching_start_is_not_a_complete_replay", [][]llm.StreamEvent{
			agentRecoverySegment("PART_A ", "max_tokens"),
			agentRecoverySegment("PART", "end_turn"),
		}, "PART_A PART"},
		{"semantic_repeat_is_preserved", [][]llm.StreamEvent{
			agentRecoverySegment("PART_A ", "max_tokens"),
			agentRecoverySegment("Compared with PART_A, FINAL_TAIL", "end_turn"),
		}, "PART_A Compared with PART_A, FINAL_TAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &agentOutputRecoveryProvider{budget: 4096, segments: tc.segments}
			dir := t.TempDir()
			roster := agent.NewRoster(2)
			tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "output-model", "system").WithRoster(roster).WithSessionPersistence(dir, "output-parent")
			result, err := tool.Execute(context.Background(), map[string]any{"prompt": "answer in full"})
			if err != nil || result == nil || result.IsError {
				t.Fatalf("Execute=%#v %v", result, err)
			}
			meta, _ := result.Presentation["subagent"].(map[string]any)
			id, _ := meta["agentId"].(string)
			saved, err := agent.LoadSubAgentSnapshot(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			var durable strings.Builder
			for _, m := range saved.Messages {
				if m.Role == llm.RoleAssistant {
					for _, b := range m.Content {
						if b.Type == "text" {
							durable.WriteString(b.Text)
						}
					}
				}
			}
			teammate, ok := roster.LookupByAgentID(id)
			if !ok {
				t.Fatal("teammate disappeared")
			}
			snap := teammate.Snapshot()
			if result.Output != tc.want || saved.Terminal.Output != tc.want || snap.Output != tc.want || durable.String() != tc.want {
				t.Fatalf("output/history diverged: result=%q teammate=%q terminal=%q history=%q want=%q", result.Output, snap.Output, saved.Terminal.Output, durable.String(), tc.want)
			}
		})
	}
}
