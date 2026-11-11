package tui

import (
	"strings"
	"testing"
)

func TestSubAgentListSummaryCountsAgentsRatherThanQueryMetadata(t *testing.T) {
	output := "agent_id=agt-one name=\"架构\" status=completed load_state=not_loaded\n" +
		"agent_id=agt-two name=\"审查\" status=failed load_state=not_loaded stop_reason=max_tokens\n" +
		"query_page offset=0 shown=2 known_total=5\n" +
		"more_saved_records=true next_offset=2; query another page\n" +
		"history_incomplete=true scan_limit_reached=false read_errors=1; inspect details"
	got := summarizeToolResult(ToolEvent{ToolName: "SubAgentList", Output: output})
	for _, want := range []string{"2 agents", "more available", "history incomplete"} {
		if !strings.Contains(got, want) {
			t.Fatalf("metadata was counted as a child or incompleteness hidden: %q; want %q", got, want)
		}
	}
	if strings.Contains(got, "5 agents") {
		t.Fatalf("query footer counted as agents: %q", got)
	}
}

func TestSubAgentOutputSummaryRetainsIncompleteReason(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"output limit", "status=failed stop_reason=max_tokens", "output limit · incomplete"},
		{"queued", "status=queued", "queued"},
		{"continuing", "status=running execution_phase=continuing", "continuing output"},
		{"unknown persisted state", "status=unknown load_state=not_loaded", "state unavailable"},
		{"timeout", "status=failed stop_reason=timeout", "timed out"},
		{"cancelled", "status=killed stop_reason=cancelled", "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := "agent_id=agt-one name=\"战略路线\" " + tc.header + "\npartial_output=true continuation_attempts=3\n---diagnostic---\nOutput incomplete.\n---output---\n保留的正文"
			got := summarizeToolResult(ToolEvent{ToolName: "SubAgentOutput", Output: out})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("incorrect child state summary: %q; want %q", got, tc.want)
			}
			if strings.Contains(got, "completed") || strings.Contains(got, "Start failed") {
				t.Fatalf("incomplete child misreported: %q", got)
			}
		})
	}
}

func TestSubAgentOutputSummaryDoesNotTrustQuotedNameAsStatus(t *testing.T) {
	out := "agent_id=agt-one name=\"review status=completed\" status=failed stop_reason=max_tokens\n---output---\npartial"
	got := summarizeToolResult(ToolEvent{ToolName: "SubAgentOutput", Output: out})
	if strings.Contains(got, "completed") || !strings.Contains(got, "output limit") {
		t.Fatalf("quoted name injected child state: %q", got)
	}
}

func TestSubAgentOutputKeepsDiagnosticWhenHidingEnvelope(t *testing.T) {
	out := "agent_id=agt-one name=\"战略路线\" status=failed stop_reason=max_tokens\npartial_output=true continuation_attempts=3\n---diagnostic---\nOutput limit reached; partial result.\n---output---\n已保存的中文结果\n---end (elapsed=2s)---"
	got := stripSubAgentOutputEnvelope(out)
	if !strings.Contains(got, "Output limit reached; partial result.") || !strings.Contains(got, "已保存的中文结果") {
		t.Fatalf("diagnostic or partial output hidden: %q", got)
	}
	if strings.Contains(got, "agent_id=") {
		t.Fatalf("internal identity header leaked into transcript preview: %q", got)
	}
}
