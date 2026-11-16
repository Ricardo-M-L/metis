package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDefaultSmallDeferredToolCanRunWithoutDiscovery(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "")
	p := &queuedStreamProvider{streams: []llm.StreamReader{
		toolUseStream("echo-call", "mcp__fixture__echo", `{}`),
		textStream("done"),
	}}
	registry := tools.NewRegistry()
	registry.Register(&fatTool{name: "mcp__fixture__echo", schemaPad: 64})
	l := NewLoop(p, registry, permission.New(permission.ModeBypass), nil, "system", 8)
	l.ContextWindow = 200_000
	l.AppendUser("call the small echo tool")
	if err := l.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	requests := p.capturedRequests()
	if hasToolSearchSpec(requests[0].Tools) {
		t.Error("default mode added a discovery round for a small, fully affordable tool")
	}
	found := false
	for _, message := range l.History() {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID == "echo-call" {
				found = true
				if block.IsError {
					t.Errorf("default small-tool invocation required discovery: %s", block.ToolResult)
				}
			}
		}
	}
	if !found {
		t.Fatal("tool was not actually dispatched")
	}
}

func TestDefaultDeferredBudgetAndExplicitOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, mode          string
		window, schemaBytes int
		lazy                bool
	}{
		{"small default", "", 200_000, 64, false},
		{"large default", "", 16_000, 5_000, true},
		{"unknown window", "", 0, 5_000, false},
		{"explicit always", "true", 200_000, 64, true},
		{"explicit disabled", "false", 16_000, 50_000, false},
		{"explicit auto", "auto:5", 16_000, 2_400, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENABLE_TOOL_SEARCH", tc.mode)
			registry := tools.NewRegistry()
			registry.Register(&fatTool{name: "mcp__fixture__echo", schemaPad: tc.schemaBytes})
			l := &Loop{Registry: registry, ContextWindow: tc.window}
			if got := hasToolSearchSpec(l.toolSpecs()); got != tc.lazy {
				t.Fatalf("lazy tool projection = %v, want %v", got, tc.lazy)
			}
		})
	}
}
