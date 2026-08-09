package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type loopRetryBoundaryProvider struct {
	calls     int
	firstErr  error
	succeedOn int
}

func (p *loopRetryBoundaryProvider) Name() string          { return "retry-boundary" }
func (p *loopRetryBoundaryProvider) ModelID() string       { return "test-model" }
func (p *loopRetryBoundaryProvider) MaxContextTokens() int { return 200_000 }
func (p *loopRetryBoundaryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("Complete not used")
}
func (p *loopRetryBoundaryProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	if p.calls < p.succeedOn {
		return nil, p.firstErr
	}
	return textStream("recovered"), nil
}

func TestLoopDoesNotRetryProviderRetryBudgetAfterExhaustion(t *testing.T) {
	root := errors.New("dial tcp: connection refused")
	p := &loopRetryBoundaryProvider{
		firstErr: &transport.RetryExhaustedError{
			Err:      &transport.RetryableError{Err: root},
			Attempts: 3,
		},
		succeedOn: 2,
	}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("hello")
	err := loop.Run(context.Background(), make(chan Event, 16))
	if err == nil || !errors.Is(err, root) {
		t.Fatalf("Run err = %v, want wrapped root cause", err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want no second retry round", p.calls)
	}
}

func TestLoopRetainsOneRecoveryForProviderWithoutInternalRetry(t *testing.T) {
	p := &loopRetryBoundaryProvider{
		firstErr:  errors.New("dial tcp: connection refused"),
		succeedOn: 2,
	}
	loop := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("hello")
	if err := loop.Run(context.Background(), make(chan Event, 16)); err != nil {
		t.Fatalf("plain transient provider did not recover: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want one loop-level retry", p.calls)
	}
}
