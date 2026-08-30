package builtin

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type forkInvocationProvider struct {
	mu        sync.Mutex
	responses []*llm.Response
}

func (p *forkInvocationProvider) Name() string          { return "fork-invocation-test" }
func (p *forkInvocationProvider) MaxContextTokens() int { return 200_000 }
func (p *forkInvocationProvider) ModelID() string       { return "test" }
func (p *forkInvocationProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.responses) == 0 {
		return &llm.Response{StopReason: "end_turn"}, nil
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}
func (*forkInvocationProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("stream unsupported")
}

func TestForkPreparesRealReadAndGrepAfterItsOwnGateAllows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWritePinnedTestFile(t, path, "fork-visible needle\n")

	// The concrete tools deliberately capture a parent gate that denies these
	// paths. The fork's own gate is authoritative and must not re-enter it, while
	// Execute must still consume an exact one-shot path binding.
	parentGate := permission.New(permission.ModeDefault)
	parentGate.AppendRules(
		permission.Rule{Tool: "Read", Match: path, Verb: permission.DecisionDeny, Source: "parent:test-deny"},
		permission.Rule{Tool: "Grep", Match: "needle", Verb: permission.DecisionDeny, Source: "parent:test-deny"},
	)
	registry := tools.NewRegistry()
	registry.Register(Read{gate: parentGate, authorizer: newReadPathAuthorizer()})
	registry.Register(NewGrep(parentGate))

	provider := &forkInvocationProvider{responses: []*llm.Response{
		{
			StopReason: "tool_use",
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUseID: "read-1", ToolName: "Read", ToolInput: map[string]any{"path": path}},
				{Type: "tool_use", ToolUseID: "grep-1", ToolName: "Grep", ToolInput: map[string]any{"root": dir, "pattern": "needle"}},
			},
		},
		{StopReason: "end_turn", Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	}}
	result, err := agent.RunForkedAgent(context.Background(), agent.ForkedAgentParams{
		Cache:      agent.CacheSafeParams{Provider: provider, Model: "test"},
		Prompt:     "inspect",
		CanUseTool: agent.AllowAll,
		Registry:   registry,
		MaxTurns:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.NewMessages) < 3 || len(result.NewMessages[2].Content) != 2 {
		t.Fatalf("unexpected fork transcript: %+v", result.NewMessages)
	}
	readResult := result.NewMessages[2].Content[0]
	grepResult := result.NewMessages[2].Content[1]
	if readResult.IsError || !strings.Contains(readResult.ToolResult, "fork-visible needle") {
		t.Fatalf("Read result = %+v", readResult)
	}
	if grepResult.IsError || !strings.Contains(grepResult.ToolResult, "notes.txt") {
		t.Fatalf("Grep result = %+v", grepResult)
	}
}
