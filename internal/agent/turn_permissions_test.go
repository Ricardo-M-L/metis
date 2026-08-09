package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type turnPermissionProvider struct {
	gate      *permission.Gate
	decisions []permission.Decision
}

func (p *turnPermissionProvider) Name() string          { return "turn-permission" }
func (p *turnPermissionProvider) ModelID() string       { return "test-model" }
func (p *turnPermissionProvider) MaxContextTokens() int { return 100_000 }
func (p *turnPermissionProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not used")
}
func (p *turnPermissionProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	decision, _ := p.gate.Check(ctx, "Bash", "git status --short")
	p.decisions = append(p.decisions, decision)
	return &turnPermissionStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "done"},
		{Type: "message_stop", StopReason: "end_turn"},
	}}, nil
}

type turnPermissionStream struct {
	events []llm.StreamEvent
	idx    int
}

func (s *turnPermissionStream) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}
func (s *turnPermissionStream) Close() error { return nil }

func TestRun_TurnPermissionRulesApplyOnceAndThenDisappear(t *testing.T) {
	gate := permission.New(permission.ModeDefault)
	provider := &turnPermissionProvider{gate: gate}
	loop := NewLoop(provider, tools.NewRegistry(), gate, NewHookRegistry(), "system", 2)
	loop.Model = "test-model"

	loop.AppendUser("first")
	ctx := WithTurnPermissionRules(context.Background(), []permission.Rule{{
		Tool: "Bash", Match: "git status:*", Verb: permission.DecisionAllow,
	}})
	if err := loop.Run(ctx, make(chan Event, 64)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(gate.Snapshot()) != 0 {
		t.Fatalf("scoped rule leaked after Run: %+v", gate.Snapshot())
	}

	loop.AppendUser("second")
	if err := loop.Run(context.Background(), make(chan Event, 64)); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(provider.decisions) != 2 {
		t.Fatalf("provider checks = %v, want two", provider.decisions)
	}
	if provider.decisions[0] != permission.DecisionAllow {
		t.Fatalf("first decision = %v, want scoped allow", provider.decisions[0])
	}
	if provider.decisions[1] != permission.DecisionAsk {
		t.Fatalf("second decision = %v, want default ask after cleanup", provider.decisions[1])
	}
}
