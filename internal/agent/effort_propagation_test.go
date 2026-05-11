package agent

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// captureProvider is a Provider that records the last Request and replies
// with a tiny end-of-turn stream. We only need it to verify Loop.Run
// populates the request fields the slash commands set.
type captureProvider struct {
	mu      sync.Mutex
	lastReq llm.Request
}

func (p *captureProvider) Name() string          { return "capture" }
func (p *captureProvider) MaxContextTokens() int { return 200_000 }
func (p *captureProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	return &llm.Response{StopReason: "end_turn"}, nil
}
func (p *captureProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	return &captureStream{}, nil
}

func (p *captureProvider) snapshot() llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq
}

// captureStream emits a single message_stop, mimicking an empty assistant turn
// that ends cleanly so Loop.Run returns without further iterations.
type captureStream struct {
	emitted bool
}

func (s *captureStream) Close() error { return nil }
func (s *captureStream) Recv() (llm.StreamEvent, error) {
	if s.emitted {
		return llm.StreamEvent{}, io.EOF
	}
	s.emitted = true
	return llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
}

func TestLoopRun_PropagatesEffortToProviderRequest(t *testing.T) {
	p := &captureProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Effort = llm.EffortHigh
	loop.AppendUser("ping")

	out := make(chan Event, 16)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	close(out)
	for range out { /* drain */
	}

	got := p.snapshot()
	if got.Effort != llm.EffortHigh {
		t.Errorf("provider received Effort=%q, want high", got.Effort)
	}
	// Without /fast, MaxTokens should NOT be overridden by the loop
	// (the provider still applies its default).
	if got.MaxTokens != 0 {
		t.Errorf("MaxTokens should default to 0 (provider applies default); got %d", got.MaxTokens)
	}
}

func TestLoopRun_FastModeForcesLowEffortAndCapsMaxTokens(t *testing.T) {
	p := &captureProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Effort = llm.EffortHigh // user's persistent preference
	loop.Fast = true
	loop.AppendUser("ping")

	out := make(chan Event, 16)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	close(out)
	for range out { /* drain */
	}

	got := p.snapshot()
	if got.Effort != llm.EffortLow {
		t.Errorf("Fast=true should force EffortLow; got %q", got.Effort)
	}
	if got.MaxTokens <= 0 {
		t.Errorf("Fast=true should set a MaxTokens override; got %d", got.MaxTokens)
	}
	// The cap should be sane — at most 4096 per the loop's logic.
	if got.MaxTokens > 4096 {
		t.Errorf("Fast cap exceeded: MaxTokens=%d", got.MaxTokens)
	}

	// Persistent /effort preference must NOT have been mutated.
	if loop.Effort != llm.EffortHigh {
		t.Errorf("loop.Effort got mutated to %q; should remain high", loop.Effort)
	}
}

func TestLoopRun_NoEffortNoFastSendsDefault(t *testing.T) {
	p := &captureProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("ping")

	out := make(chan Event, 16)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	close(out)
	for range out { /* drain */
	}

	got := p.snapshot()
	if got.Effort != llm.EffortDefault {
		t.Errorf("no Effort set should mean EffortDefault on the wire; got %q", got.Effort)
	}
	if got.MaxTokens != 0 {
		t.Errorf("MaxTokens should be 0 (use provider default); got %d", got.MaxTokens)
	}
}
