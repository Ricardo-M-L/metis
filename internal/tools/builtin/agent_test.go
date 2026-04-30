package builtin

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// fakeProvider returns a deterministic stream for any Stream call.
type fakeProvider struct {
	events []llm.StreamEvent
}

func (p *fakeProvider) Name() string                                                  { return "fake" }
func (p *fakeProvider) MaxContextTokens() int                                         { return 100000 }
func (p *fakeProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	return &fakeStream{events: p.events}, nil
}

type fakeStream struct {
	events []llm.StreamEvent
	idx    int
}

func (s *fakeStream) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}
func (s *fakeStream) Close() error { return nil }

func helloProvider() llm.Provider {
	return &fakeProvider{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "sub-agent"},
		{Type: "text_delta", TextDelta: " done"},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1},
		{Type: "message_stop"},
	}}
}

// --- tests ---------------------------------------------------------------

func TestAgentTool_RequiresPrompt(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	if _, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Error("expected error for missing prompt")
	}
}

func TestAgentTool_ReturnsSubAgentText(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	tool := NewAgent(gate, helloProvider(), reg, "model", "system")

	res, err := tool.Execute(context.Background(), map[string]any{"prompt": "do the thing"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("expected success, got error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "sub-agent done") {
		t.Errorf("expected concatenated text deltas, got %q", res.Output)
	}
}

func TestAgentTool_NestingLimitEnforced(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	tool := NewAgent(gate, helloProvider(), reg, "model", "system")

	// Pre-load context with a depth at the limit.
	ctx := context.WithValue(context.Background(), agentDepthKey{}, maxAgentDepth)
	res, err := tool.Execute(ctx, map[string]any{"prompt": "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "nesting limit") {
		t.Errorf("expected nesting-limit error, got: %+v", res)
	}
}

func TestAgentTool_NilProviderErrors(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), nil, nil, "", "")
	if _, err := tool.Execute(context.Background(), map[string]any{"prompt": "x"}); err == nil {
		t.Error("expected error when provider is nil")
	}
}

func TestAgentTool_Schema(t *testing.T) {
	tool := Agent{}
	schema := tool.InputSchema()
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "prompt" {
		t.Errorf("required = %v, want [prompt]", required)
	}
}

func TestAgentTool_CanUseGoesThroughGate(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeDeny), helloProvider(), tools.NewRegistry(), "m", "s")
	p, _ := tool.CanUse(context.Background(), map[string]any{"prompt": "x"})
	if p != tools.PermissionDeny {
		t.Errorf("deny mode should propagate; got %v", p)
	}
}

func TestAgentTool_EmptyOutputFallback(t *testing.T) {
	// Provider streams nothing but a clean stop — sub-agent has no text.
	gate := permission.New(permission.ModeBypass)
	prov := &fakeProvider{events: []llm.StreamEvent{
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}
	tool := NewAgent(gate, prov, tools.NewRegistry(), "m", "s")
	res, err := tool.Execute(context.Background(), map[string]any{"prompt": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "without text output") {
		t.Errorf("expected fallback message, got %q", res.Output)
	}
}
