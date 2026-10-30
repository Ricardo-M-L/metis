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

func TestSubAgentOutputCompletedChineseNameAndResumedLatestResult(t *testing.T) {
	dir := t.TempDir()
	roster := agent.NewRoster(0)
	gate := permission.New(permission.ModeBypassPermissions)
	provider := func(text string) *fakeProvider {
		return &fakeProvider{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: text},
			{Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"},
		}}
	}
	first := NewAgent(gate, provider("first-generation-result"), tools.NewRegistry(), "model", "system").WithRoster(roster).WithSessionPersistence(dir, "parent")
	result, err := first.Execute(context.Background(), map[string]any{"name": "配置审查", "prompt": "first task"})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("first generation: %+v %v", result, err)
	}
	metadata := result.Presentation["subagent"].(map[string]any)
	id := metadata["agentId"].(string)
	output := NewSubAgentOutput(gate, roster)
	read, err := output.Execute(context.Background(), map[string]any{"name": "配置审查"})
	if err != nil || read == nil || read.IsError || !strings.Contains(read.Output, "first-generation-result") || !strings.Contains(read.Output, "status=completed") {
		t.Fatalf("completed Chinese name should remain readable: %+v %v", read, err)
	}
	second := NewAgent(gate, provider("latest-generation-result"), tools.NewRegistry(), "model", "system").WithRoster(roster).WithSessionPersistence(dir, "parent")
	result, err = second.Execute(context.Background(), map[string]any{"resume_from": id, "prompt": "continue task"})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("second generation: %+v %v", result, err)
	}
	for _, input := range []map[string]any{{"name": "配置审查"}, {"agent_id": id}} {
		read, err = output.Execute(context.Background(), input)
		if err != nil || read == nil || read.IsError || !strings.Contains(read.Output, "latest-generation-result") || strings.Contains(read.Output, "first-generation-result") {
			t.Fatalf("resumed output must use latest generation for %v: %+v %v", input, read, err)
		}
	}
	listed, err := NewSubAgentList(gate, roster).Execute(context.Background(), nil)
	if err != nil || strings.Count(listed.Output, "agent_id="+id) != 1 || !strings.Contains(listed.Output, "latest-generation-result") {
		t.Fatalf("list must contain only the latest stable identity: %+v %v", listed, err)
	}
}

func TestSubAgentStopFinishedIDDoesNotCancelReusedName(t *testing.T) {
	roster := agent.NewRoster(0)
	old := &agent.Teammate{Name: "配置审查", AgentID: "agt-old"}
	if err := roster.Register(old); err != nil {
		t.Fatal(err)
	}
	old.Finish(agent.StatusCompleted, "old result", nil, "end_turn")
	roster.UnregisterTeammate(old)
	replacement := &agent.Teammate{Name: old.Name, AgentID: "agt-new"}
	if err := roster.Register(replacement); err != nil {
		t.Fatal(err)
	}
	defer roster.UnregisterTeammate(replacement)
	cancelled := false
	replacement.SetCancel(func() { cancelled = true })
	stop := NewSubAgentStop(permission.New(permission.ModeBypassPermissions), roster)
	result, err := stop.Execute(context.Background(), map[string]any{"agent_id": old.AgentID, "name": replacement.Name})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "already completed") || cancelled {
		t.Fatalf("finished identity must not cancel replacement name: %+v %v cancelled=%v", result, err, cancelled)
	}
}

func TestMessageTeammateDoesNotQueueToFinishedReaderTarget(t *testing.T) {
	for _, address := range []string{"配置审查", "agt-finished"} {
		t.Run(address, func(t *testing.T) {
			roster := agent.NewRoster(0)
			finished := &agent.Teammate{Name: "配置审查", AgentID: "agt-finished"}
			if err := roster.Register(finished); err != nil {
				t.Fatal(err)
			}
			finished.Finish(agent.StatusCompleted, "done", nil, "end_turn")
			roster.UnregisterTeammate(finished)
			tool := NewMessageTeammate(permission.New(permission.ModeBypassPermissions), roster)
			result, err := tool.Execute(context.Background(), map[string]any{"to": address, "body": "do not lose this"})
			if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "no longer reachable") || len(finished.Mailbox) != 0 {
				t.Fatalf("finished reader target accepted peer message: %+v %v queued=%d", result, err, len(finished.Mailbox))
			}
		})
	}
}
