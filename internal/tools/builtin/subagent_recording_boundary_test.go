package builtin

import (
	"context"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestForkDoesNotInheritEnclosingAgentUsageReceipts(t *testing.T) {
	dir := t.TempDir()
	header := agent.NewSubAgentHeader("agt-owner", "model", "parent", "", "", "")
	transcript, err := agent.NewSubAgentTranscript(dir, header.ID, header)
	if err != nil {
		t.Fatal(err)
	}
	_ = transcript.Close()
	recorder, err := agent.NewSubAgentUsageRecorder(dir, header.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "fork result"},
		{Type: "usage", InputTokens: 99, HasInputTokens: true},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}
	ctx := agent.WithParentSnapshot(agent.WithSubAgentUsageRecorder(context.Background(), recorder), agent.ParentSnapshot{
		System: "system", Model: "model", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "parent task"}}}},
	})
	result, err := NewFork(permission.New(permission.ModeBypass), provider, tools.NewRegistry()).Execute(ctx, map[string]any{"directive": "fork task"})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Fork.Execute = %+v, %v", result, err)
	}
	usage, err := agent.LoadSubAgentUsage(dir, header.ID)
	if err != nil || usage.Available {
		t.Fatalf("unidentified Fork was charged to enclosing Agent: %+v, %v", usage, err)
	}
}

func TestAgentPersistsInitialTaskBeforeFirstResponse(t *testing.T) {
	dir := t.TempDir()
	roster := agent.NewRoster(2)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := roster.CancelAndWait(ctx); err != nil {
			t.Errorf("join background child: %v", err)
		}
	})
	tool := NewAgent(permission.New(permission.ModeBypass), blockingFakeProvider{}, tools.NewRegistry(), "model", "system").
		WithRoster(roster).WithSessionPersistence(dir, "parent")
	agentID := executeUsageAgent(t, context.Background(), tool, map[string]any{"prompt": "full initial task", "run_in_background": true})
	snapshot, err := agent.LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Role != llm.RoleUser || len(snapshot.Messages[0].Content) != 1 || snapshot.Messages[0].Content[0].Text != "full initial task" {
		t.Fatalf("task missing before first response: %+v", snapshot.Messages)
	}
}
