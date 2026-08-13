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
func (p *captureProvider) ModelID() string       { return "" }
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
	loop.SetEffort(llm.EffortHigh)
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
	loop.SetEffort(llm.EffortHigh) // user's persistent preference
	loop.SetFast(true)
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
	if got := loop.EffortValue(); got != llm.EffortHigh {
		t.Errorf("loop effort got mutated to %q; should remain high", got)
	}
}

func TestLoopEffortConcurrentRequestSnapshots(t *testing.T) {
	p := &captureProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	levels := []llm.Effort{
		llm.EffortDefault,
		llm.EffortLow,
		llm.EffortMedium,
		llm.EffortHigh,
	}
	valid := func(e llm.Effort) bool {
		for _, candidate := range levels {
			if e == candidate {
				return true
			}
		}
		return false
	}

	start := make(chan struct{})
	errCh := make(chan llm.Effort, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			loop.SetEffort(levels[i%len(levels)])
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			// buildRequest is the same locked snapshot path used by Run. The
			// accessor is exercised too because TUI rendering can happen at
			// the same time as the provider loop assembles the next request.
			if got := loop.buildRequest(nil).Effort; !valid(got) {
				select {
				case errCh <- got:
				default:
				}
				return
			}
			if got := loop.EffortValue(); !valid(got) {
				select {
				case errCh <- got:
				default:
				}
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errCh)
	if got, ok := <-errCh; ok {
		t.Fatalf("concurrent effort snapshot returned invalid value %q", got)
	}
}

func TestLoopFastConcurrentRequestSnapshots(t *testing.T) {
	p := &captureProvider{}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			loop.SetFast(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			_ = loop.buildRequest(nil)
			_ = loop.FastEnabled()
		}
	}()
	close(start)
	wg.Wait()
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
