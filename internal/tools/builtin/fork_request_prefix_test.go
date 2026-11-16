package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Run the real parent dispatch and Fork.Execute. The provider is the only
// external boundary replaced; helper-only snapshot tests miss this path.
func TestForkPreservesActualParentRequest(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	p := &forkRequestProvider{}
	registry := tools.NewRegistry()
	gate := permission.New(permission.ModeBypass)
	registry.Register(forkDescriptionTool{})
	registry.Register(NewFork(gate, p, registry))
	parent := agent.NewLoop(p, registry, gate, agent.NewHookRegistry(), "raw parent system", 12)
	parent.Model = "fixture-model"
	parent.SystemSections = []llm.SystemSection{
		{Name: "base", Body: "typed parent instructions", Cache: true},
		{Name: "env", Body: "frozen environment", Volatile: true},
	}
	parent.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: "parent-affinity", PermissionMode: "bypass", WorkingDirectory: "/fixture"}
	}
	parent.SetEffort(llm.EffortHigh)
	parent.AppendUser("Delegate this context to Fork, then summarize the child.")
	out := make(chan agent.Event, 256)
	if err := parent.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want parent, child, parent", len(requests))
	}
	first, child := requests[0], requests[1]
	if child.System != first.System || !reflect.DeepEqual(child.SystemSections, first.SystemSections) {
		t.Errorf("warm fork changed the shared system prefix: parent=%q/%+v child=%q/%+v", first.System, first.SystemSections, child.System, child.SystemSections)
	}
	if !reflect.DeepEqual(child.Tools, first.Tools) {
		t.Error("warm fork changed the shared tool definitions")
	}
	if child.Effort != first.Effort || child.Model != first.Model || child.SessionID != first.SessionID {
		t.Errorf("warm fork lost request identity: effort=%q model=%q session=%q", child.Effort, child.Model, child.SessionID)
	}
	if len(child.Messages) <= len(first.Messages) || !reflect.DeepEqual(child.Messages[:len(first.Messages)], first.Messages) {
		t.Fatal("warm fork changed the parent conversation prefix")
	}
	// The outstanding Fork call must be answered before the child directive.
	tail := child.Messages[len(first.Messages):]
	if len(tail) != 3 || tail[0].Content[0].Type != "tool_use" || tail[1].Content[0].Type != "tool_result" || tail[1].Content[0].ToolUseID != "fork-call" {
		t.Fatalf("invalid fork continuation: %+v", tail)
	}
}

type forkRequestProvider struct {
	fakeProvider
	mu       sync.Mutex
	requests []llm.Request
}

func (p *forkRequestProvider) ModelID() string { return "fixture-model" }
func (p *forkRequestProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	// Detach the capture so a child cannot make the comparison pass by changing
	// a parent's shared map or slice after the request crossed this boundary.
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var captured llm.Request
	if err := json.Unmarshal(data, &captured); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.requests = append(p.requests, captured)
	n := len(p.requests)
	p.mu.Unlock()
	if n == 1 {
		return &fakeStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "fork-call", ToolName: "Fork"},
			{Type: "tool_input_delta", ToolUseID: "fork-call", InputDelta: `{"directive":"Return the child result."}`},
			{Type: "tool_use_stop", ToolUseID: "fork-call"},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "complete"},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

func (p *forkRequestProvider) snapshot() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

type forkDescriptionTool struct{ tools.BaseTool }

func (forkDescriptionTool) Name() string { return "Fixture" }
func (forkDescriptionTool) Description() string {
	return "Full fixture tool instructions for the parent and its warm children."
}
func (forkDescriptionTool) ShortDescription() string { return "Short fixture description." }
func (forkDescriptionTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}
}
func (forkDescriptionTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (forkDescriptionTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "fixture"
}
func (forkDescriptionTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "fixture"}, nil
}
