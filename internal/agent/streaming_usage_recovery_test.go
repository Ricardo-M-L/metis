package agent

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestLoopFailedStreamBillsCorrectedUsageOnce(t *testing.T) {
	for _, mode := range []string{"recv_error", "error_event"} {
		t.Run(mode, func(t *testing.T) {
			configureStreamRecoveryTest(t, true)
			providerErr := errors.New("terminal provider stream failure")
			corrected := usageTotals{in: 179, out: 74, cacheCreate: 512}
			stream := incompleteRecoveryToolStream(true)
			stream.events = append([]llm.StreamEvent{
				{Type: "message_start", InputTokens: 556, HasInputTokens: true},
			}, stream.events...)
			stream.events = append(stream.events, llm.StreamEvent{
				Type: "message_delta", InputTokens: corrected.in, OutputTokens: corrected.out,
				CacheCreationInputTokens: corrected.cacheCreate,
				HasInputTokens:           true, HasOutputTokens: true, HasCacheCreationInputTokens: true,
			})
			stream.err = providerErr
			if mode == "error_event" {
				stream.events = append(stream.events, llm.StreamEvent{Type: "error", Err: providerErr})
			}
			probe := &recoveryTestTool{}
			p := &recoveryTestProvider{t: t, stream: func(context.Context, int) (llm.StreamReader, error) {
				return stream, nil
			}}
			loop := newStreamRecoveryTestLoop(p, probe)
			loop.Budget = recoveryUsageBudget(0)
			var persisted []float64
			loop.Budget.SetPersistence(func(spend float64) error {
				persisted = append(persisted, spend)
				return nil
			})
			out := make(chan Event, 128)
			if err := loop.Run(context.Background(), out); !errors.Is(err, providerErr) {
				t.Fatalf("terminal stream error changed: %v", err)
			}
			if len(p.requests) != 1 || !stream.closed || probe.calls.Load() != 0 {
				t.Fatalf("failed draft requested or executed work: requests=%d closed=%t tools=%d", len(p.requests), stream.closed, probe.calls.Load())
			}
			wantCost := recoveryUsageCost(corrected)
			if len(persisted) != 1 || persisted[0] != wantCost || loop.Budget.SpentUSD() != wantCost {
				t.Fatalf("failed attempt did not bill only its corrected usage once: persisted=%v spent=%f want=%f", persisted, loop.Budget.SpentUSD(), wantCost)
			}
			if events := recoveryUsageEvents(out); !reflect.DeepEqual(events, []usageTotals{corrected}) {
				t.Fatalf("failed attempt token ledger=%+v want=%+v", events, corrected)
			}
			stats := loop.CacheStats.Snapshot()
			if len(stats) != 1 || stats[0].Input != corrected.in || stats[0].Output != corrected.out || stats[0].CacheCreate != corrected.cacheCreate || stats[0].CacheRead != corrected.cacheRead {
				t.Fatalf("failed attempt accounting callback retained initial input or ran twice: %+v", stats)
			}
		})
	}
}

func TestLoopRecoveryBillsEachCorrectedAttemptAndKeepsFinalContext(t *testing.T) {
	configureStreamRecoveryTest(t, true)
	synctest.Test(t, func(t *testing.T) {
		reports := []usageTotals{
			{in: 179, out: 74, cacheCreate: 512},
			{in: 91, out: 18, cacheCreate: 256, cacheRead: 9},
		}
		var streams []*recoveryTestStream
		probe := &recoveryTestTool{}
		p := &recoveryTestProvider{t: t, stream: func(_ context.Context, call int) (llm.StreamReader, error) {
			if call > len(reports) {
				t.Fatal("unexpected extra request")
			}
			reported := reports[call-1]
			stream := &recoveryTestStream{events: []llm.StreamEvent{{
				Type: "message_start", InputTokens: 556 + 100*(call-1),
				HasInputTokens: true,
			}}}
			if call == 1 {
				stream.events = append(stream.events, incompleteRecoveryToolStream(true).events...)
			}
			// Each physical request has its own final absolute counters. The
			// second request must not inherit the failed draft's usage.
			stream.events = append(stream.events, llm.StreamEvent{
				Type: "message_delta", InputTokens: reported.in, OutputTokens: reported.out,
				CacheCreationInputTokens: reported.cacheCreate, CacheReadInputTokens: reported.cacheRead,
				HasInputTokens: true, HasOutputTokens: true,
				HasCacheCreationInputTokens: true, HasCacheReadInputTokens: true,
			})
			if call == 1 {
				stream.err = io.ErrUnexpectedEOF
			} else {
				stream.events = append(stream.events,
					llm.StreamEvent{Type: "text_delta", TextDelta: "final complete"},
					llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"},
				)
			}
			streams = append(streams, stream)
			return stream, nil
		}}
		loop := newStreamRecoveryTestLoop(p, probe)
		loop.Budget = recoveryUsageBudget(0)
		var persisted []float64
		loop.Budget.SetPersistence(func(spend float64) error {
			persisted = append(persisted, spend)
			return nil
		})
		out := make(chan Event, 256)
		if err := loop.Run(context.Background(), out); err != nil {
			t.Fatal(err)
		}
		if len(p.requests) != 2 || !reflect.DeepEqual(p.requests[0], p.requests[1]) || probe.calls.Load() != 0 {
			t.Fatalf("recovery changed the request or executed the failed draft: requests=%d tools=%d", len(p.requests), probe.calls.Load())
		}
		for i, stream := range streams {
			if !stream.closed {
				t.Fatalf("attempt %d stream was not closed", i+1)
			}
		}
		firstCost := recoveryUsageCost(reports[0])
		totalCost := firstCost + recoveryUsageCost(reports[1])
		if !reflect.DeepEqual(persisted, []float64{firstCost, totalCost}) || loop.Budget.SpentUSD() != totalCost {
			t.Fatalf("retry ledger retained initial input or merged attempt snapshots: persisted=%v spent=%f want=%f", persisted, loop.Budget.SpentUSD(), totalCost)
		}
		if events := recoveryUsageEvents(out); !reflect.DeepEqual(events, reports) {
			t.Fatalf("per-attempt corrected token ledger=%+v want=%+v", events, reports)
		}
		stats := loop.CacheStats.Snapshot()
		if len(stats) != len(reports) {
			t.Fatalf("accounting callback count=%d want=%d", len(stats), len(reports))
		}
		for i, stat := range stats {
			got := usageTotals{in: stat.Input, out: stat.Output, cacheCreate: stat.CacheCreate, cacheRead: stat.CacheRead}
			if got != reports[i] {
				t.Fatalf("attempt %d accounting snapshot=%+v want=%+v", i+1, got, reports[i])
			}
		}
		assertNoUncommittedRecoveryHistory(t, loop.History())
		final := reports[len(reports)-1]
		history := loop.History()
		wantContext := final.in + final.cacheCreate + final.cacheRead + assistantContextTokens(p, history[len(history)-1], final.out)
		if !loop.activeContext.valid || loop.activeContext.tokens != wantContext {
			t.Fatalf("retry billing polluted final context: got=%+v want=%d", loop.activeContext, wantContext)
		}
	})
}
