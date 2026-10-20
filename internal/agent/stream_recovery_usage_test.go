package agent

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func reportedRecoveryUsageStream(usage usageTotals, failure error, draft bool) *recoveryTestStream {
	events := []llm.StreamEvent{{Type: "message_start", InputTokens: usage.in, CacheCreationInputTokens: usage.cacheCreate, CacheReadInputTokens: usage.cacheRead}}
	if draft {
		events = append(events, incompleteRecoveryToolStream(true).events...)
	}
	// These are snapshots within one request, not independent billable deltas.
	events = append(events, llm.StreamEvent{Type: "message_delta", OutputTokens: usage.out / 2}, llm.StreamEvent{Type: "message_delta", OutputTokens: usage.out})
	if failure == nil {
		events = append(events, llm.StreamEvent{Type: "text_delta", TextDelta: "final complete"}, llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"})
	}
	return &recoveryTestStream{events: events, err: failure}
}

func recoveryUsageCost(usage usageTotals) float64 {
	return float64(usage.in + 2*usage.out + 3*usage.cacheRead + 4*usage.cacheCreate)
}

func recoveryUsageBudget(cap float64) *budget.Tracker {
	return budget.NewTracker(cap, budget.Rates{InputPerMTok: 1_000_000, OutputPerMTok: 2_000_000, CacheReadPerMTok: 3_000_000, CacheWritePerMTok: 4_000_000})
}

func recoveryUsageEvents(out chan Event) []usageTotals {
	close(out)
	var usage []usageTotals
	for event := range out {
		if event.Kind == EventTokens {
			usage = append(usage, usageTotals{in: event.InputTokens, out: event.OutputTokens, cacheCreate: event.CacheCreationInputTokens, cacheRead: event.CacheReadInputTokens})
		}
	}
	return usage
}

func TestLoopFailedAndCancelledStreamsPersistReportedUsage(t *testing.T) {
	for _, mode := range []string{"recv_error", "error_event", "cancelled", "recovery_disabled"} {
		t.Run(mode, func(t *testing.T) {
			configureStreamRecoveryTest(t, mode != "recovery_disabled")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reported := usageTotals{in: 100, out: 9, cacheRead: 11, cacheCreate: 7}
			providerErr := errors.New("terminal provider stream failure")
			wantErr := providerErr
			probe := &recoveryTestTool{}
			p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
				stream := reportedRecoveryUsageStream(reported, providerErr, true)
				if mode == "error_event" {
					stream.events = append(stream.events, llm.StreamEvent{Type: "error", Err: providerErr})
				} else if mode == "cancelled" {
					stream.err, stream.onError = io.ErrUnexpectedEOF, cancel
				}
				return stream, nil
			}}
			if mode == "cancelled" {
				wantErr = context.Canceled
			}
			loop := newStreamRecoveryTestLoop(p, probe)
			loop.Budget = recoveryUsageBudget(0)
			var persisted []float64
			loop.Budget.SetPersistence(func(spend float64) error { persisted = append(persisted, spend); return nil })
			out := make(chan Event, 128)
			err := loop.Run(ctx, out)
			if !errors.Is(err, wantErr) || len(p.requests) != 1 || probe.calls.Load() != 0 {
				t.Fatalf("failed request changed error/retry/tool behavior: requests=%d tools=%d err=%v", len(p.requests), probe.calls.Load(), err)
			}
			if len(persisted) != 1 || persisted[0] != recoveryUsageCost(reported) || loop.Budget.SpentUSD() != persisted[0] {
				t.Fatalf("failed stream usage missing/doubled: persisted=%v spent=%f", persisted, loop.Budget.SpentUSD())
			}
			if events := recoveryUsageEvents(out); len(events) != 1 || events[0] != reported {
				t.Fatalf("failed stream token ledger=%+v want=%+v", events, reported)
			}
		})
	}
}

func TestLoopStreamRetriesBillEachAttemptButSnapshotOnlyFinalRequest(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		reports := []usageTotals{{in: 2000, out: 3, cacheRead: 200, cacheCreate: 10}, {in: 3000, out: 5, cacheRead: 300, cacheCreate: 20}, {in: 800, out: 16, cacheRead: 120, cacheCreate: 80}}
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
			if call > len(reports) {
				t.Fatal("unexpected extra request")
			}
			if call < len(reports) {
				return reportedRecoveryUsageStream(reports[call-1], io.ErrUnexpectedEOF, true), nil
			}
			return reportedRecoveryUsageStream(reports[call-1], nil, false), nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		loop.Budget = recoveryUsageBudget(0)
		var persisted []float64
		loop.Budget.SetPersistence(func(spend float64) error { persisted = append(persisted, spend); return nil })
		out := make(chan Event, 256)
		if err := loop.Run(context.Background(), out); err != nil {
			t.Fatal(err)
		}
		if len(p.requests) != 3 || probe.calls.Load() != 0 || len(persisted) != 3 {
			t.Fatalf("unexpected request/tool/ledger count: requests=%d tools=%d persisted=%v", len(p.requests), probe.calls.Load(), persisted)
		}
		cost := 0.0
		for i, report := range reports {
			cost += recoveryUsageCost(report)
			if math.Abs(persisted[i]-cost) > 1e-9 {
				t.Fatalf("attempt %d lost/doubled usage: %v want=%f", i, persisted, cost)
			}
		}
		if events := recoveryUsageEvents(out); len(events) != 3 || events[0] != reports[0] || events[1] != reports[1] || events[2] != reports[2] {
			t.Fatalf("per-request token deltas changed: %+v", events)
		}
		assertNoUncommittedRecoveryHistory(t, loop.History())
		final := reports[len(reports)-1]
		history := loop.History()
		wantContext := final.in + final.cacheCreate + final.cacheRead + assistantContextTokens(p, history[len(history)-1], final.out)
		if !loop.activeContext.valid || loop.activeContext.tokens != wantContext {
			t.Fatalf("cumulative billing polluted final prompt snapshot: got=%+v want=%d", loop.activeContext, wantContext)
		}
	})
}

func TestLoopStreamRetryStopsBeforeNextRequestWhenReportedBudgetExhausted(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		reported := usageTotals{in: 100, out: 9}
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			return reportedRecoveryUsageStream(reported, io.ErrUnexpectedEOF, true), nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		loop.Budget = recoveryUsageBudget(1)
		var persisted []float64
		loop.Budget.SetPersistence(func(spend float64) error { persisted = append(persisted, spend); return nil })
		err := loop.Run(context.Background(), make(chan Event, 128))
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(p.requests) != 1 || len(persisted) != 1 || persisted[0] != recoveryUsageCost(reported) || probe.calls.Load() != 0 {
			t.Fatalf("exhausted budget retried or lost usage: requests=%d persisted=%v tools=%d err=%v", len(p.requests), persisted, probe.calls.Load(), err)
		}
	})
}

func TestLoopStreamUsagePersistenceFailureStopsRecoveryAndKeepsErrorChain(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		providerErr := errors.New(`{"type":"error","error":{"type":"invalid_request_error","message":"stream error: stream disconnected before completion: stream closed before response.completed"}}`)
		ledgerErr := errors.New("ledger unavailable")
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			return reportedRecoveryUsageStream(usageTotals{in: 100}, providerErr, true), nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		loop.Budget = recoveryUsageBudget(0)
		loop.Budget.SetPersistence(func(float64) error { return ledgerErr })
		err := loop.Run(context.Background(), make(chan Event, 128))
		if !errors.Is(err, providerErr) || !errors.Is(err, ledgerErr) || !strings.Contains(err.Error(), "persist task spend") || len(p.requests) != 1 || probe.calls.Load() != 0 {
			t.Fatalf("ledger failure discarded error or retried: requests=%d tools=%d err=%v", len(p.requests), probe.calls.Load(), err)
		}
		// Joining a durable-ledger failure must not leave a pure provider JSON
		// error at the outer boundary that a task classifier could blindly retry.
		if _, single := err.(interface{ Unwrap() error }); single {
			t.Fatal("ledger failure exposed only a single retryable cause")
		}
	})
}

func TestLoopStreamRetryRechecksSharedBudgetAfterBackoff(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
			return reportedRecoveryUsageStream(usageTotals{in: 1}, io.ErrUnexpectedEOF, true), nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		loop.Budget = recoveryUsageBudget(10)
		charged := make(chan struct{})
		go func() {
			defer close(charged)
			time.Sleep(100 * time.Millisecond)
			if err := loop.Budget.AddUsage(9, 0, 0, 0); err != nil {
				t.Error(err)
			}
		}()
		err := loop.Run(context.Background(), make(chan Event, 128))
		<-charged
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(p.requests) != 1 || loop.Budget.SpentUSD() != 10 || probe.calls.Load() != 0 {
			t.Fatalf("shared cap during backoff admitted another request: requests=%d spend=%f tools=%d err=%v", len(p.requests), loop.Budget.SpentUSD(), probe.calls.Load(), err)
		}
	})
}

func TestLoopFailedStreamDoesNotInventUnreportedUsage(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	providerErr := errors.New("terminal provider stream failure")
	probe := &recoveryTestTool{}
	p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
		return &recoveryTestStream{err: providerErr}, nil
	}}
	loop := newStreamRecoveryTestLoop(p, probe)
	loop.Budget = recoveryUsageBudget(0)
	writes := 0
	loop.Budget.SetPersistence(func(float64) error { writes++; return nil })
	out := make(chan Event, 32)
	if err := loop.Run(context.Background(), out); !errors.Is(err, providerErr) {
		t.Fatal(err)
	}
	if len(recoveryUsageEvents(out)) != 0 || writes != 0 || loop.Budget.SpentUSD() != 0 {
		t.Fatal("failed stream without usage invented a token or spend report")
	}
}

func TestLoopCompletedStreamLedgerFailureCannotAuthorizeTools(t *testing.T) {
	configureStreamRecoveryTest(t, false)
	ledgerErr := errors.New("ledger unavailable")
	probe := &recoveryTestTool{}
	p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
		stream := reportedRecoveryUsageStream(usageTotals{in: 100, out: 9}, nil, true)
		stream.events[len(stream.events)-1].StopReason = "tool_use"
		return stream, nil
	}}
	loop := newStreamRecoveryTestLoop(p, probe)
	loop.Budget = recoveryUsageBudget(0)
	writes := 0
	loop.Budget.SetPersistence(func(float64) error { writes++; return ledgerErr })
	err := loop.Run(context.Background(), make(chan Event, 128))
	if !errors.Is(err, ledgerErr) || writes != 1 || len(p.requests) != 1 || probe.calls.Load() != 0 {
		t.Fatalf("unrecorded completed response authorized work: requests=%d writes=%d tools=%d err=%v", len(p.requests), writes, probe.calls.Load(), err)
	}
}
