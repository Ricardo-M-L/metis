package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func outputLimitStream(text, reason string) llm.StreamReader {
	return &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: text},
		{Type: "message_delta", StopReason: reason},
		{Type: "message_stop", StopReason: reason, InputTokens: 10, OutputTokens: 20},
	}}
}

type outputRecoveryProbe struct {
	tools.BaseTool
	calls *atomic.Int32
}

func (outputRecoveryProbe) Name() string                { return "OutputRecoveryProbe" }
func (outputRecoveryProbe) Description() string         { return "Count a test side effect." }
func (outputRecoveryProbe) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (outputRecoveryProbe) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "test"
}
func (outputRecoveryProbe) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (p outputRecoveryProbe) Execute(context.Context, map[string]any) (*tools.Result, error) {
	p.calls.Add(1)
	return &tools.Result{Output: "side effect performed"}, nil
}

func TestSubAgentOutputRecoveryPreservesHistoryAndUsesTextOnlyRequest(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("FIRST_PART unique body", "max_tokens"), textStream(" FINAL_TAIL")}}
	dir, transcript, loop, teammate := continuationFixture(t, provider)
	var sideEffects atomic.Int32
	loop.Registry.Register(outputRecoveryProbe{calls: &sideEffects})
	loop.RecoverTextToolCalls = true
	out := make(chan Event, 2048)
	if err := RunSubAgentWithContinuations(context.Background(), out, loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("requests=%d; want one bounded continuation", len(requests))
	}
	if len(requests[0].Tools) == 0 || len(requests[1].Tools) != 0 {
		t.Fatalf("initial tools=%d recovery tools=%d", len(requests[0].Tools), len(requests[1].Tools))
	}
	lastBody := ""
	for _, message := range requests[1].Messages {
		lastBody += textOf(message)
	}
	if strings.Count(lastBody, "FIRST_PART unique body") != 1 {
		t.Fatalf("partial output lost or duplicated in continuation history")
	}
	if sideEffects.Load() != 0 {
		t.Fatal("text continuation executed a tool")
	}
	close(out)
	var stops []string
	var outputTokens int
	for event := range out {
		if event.Kind == EventLoopDone {
			stops = append(stops, event.StopReason)
		}
		if event.Kind == EventTokens {
			outputTokens += event.OutputTokens
		}
	}
	if len(stops) != 1 || stops[0] != "end_turn" {
		t.Fatalf("terminal events=%v", stops)
	}
	if outputTokens != 20 {
		t.Fatalf("partial request usage lost: %d", outputTokens)
	}
	saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, message := range saved.Messages {
		if message.Role == llm.RoleAssistant {
			body += textOf(message)
		}
	}
	if body != "FIRST_PART unique body FINAL_TAIL" {
		t.Fatalf("durable body=%q", body)
	}
}

func TestSubAgentOutputRecoveryHasThreeContinuationCeiling(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		outputLimitStream("part A", "max_tokens"), outputLimitStream("part B", "length"),
		outputLimitStream("part C", "max_output_tokens"), outputLimitStream("part D", "max_tokens"),
	}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	out := make(chan Event, 4096)
	if err := RunSubAgentWithContinuations(context.Background(), out, loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.capturedRequests()); got != 4 {
		t.Fatalf("requests=%d, want initial plus exactly three continuations", got)
	}
	close(out)
	stops := []string{}
	for event := range out {
		if event.Kind == EventLoopDone {
			stops = append(stops, event.StopReason)
		}
	}
	if len(stops) != 1 || stops[0] != "max_tokens" {
		t.Fatalf("exhausted response reported success: %v", stops)
	}
}

func TestSubAgentOutputRecoveryStopsWhenContinuationMakesNoProgress(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("same complete segment", "max_tokens"), outputLimitStream("same complete segment", "max_tokens")}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 4096), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.capturedRequests()); got != 2 {
		t.Fatalf("requests=%d, want stop after duplicate recovery segment", got)
	}
}

func TestSubAgentOutputRecoveryDoesNotResetIterationBudget(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("partial", "max_tokens")}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	loop.MaxIters = 1
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 2048), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.capturedRequests()); got != 1 {
		t.Fatalf("iteration cap exceeded: requests=%d", got)
	}
}

func TestSubAgentOutputRecoveryDoesNotContinueFilteredOrEmptyOutputs(t *testing.T) {
	for _, tc := range []struct{ text, stop string }{{"partial", "content_filter"}, {"", "max_tokens"}, {"partial", "provider_protocol_error"}} {
		t.Run(tc.stop+tc.text, func(t *testing.T) {
			provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream(tc.text, tc.stop)}}
			_, transcript, loop, teammate := continuationFixture(t, provider)
			if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 2048), loop, teammate, transcript); err != nil {
				t.Fatal(err)
			}
			if got := len(provider.capturedRequests()); got != 1 {
				t.Fatalf("unsafe response continued: requests=%d", got)
			}
		})
	}
}

func TestSubAgentOutputRecoveryRejectsTextAlongsideTruncatedToolCall(t *testing.T) {
	stream := &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "prefix prose before a truncated call"},
		{Type: "tool_use_start", ToolUseID: "truncated", ToolName: "OutputRecoveryProbe"},
		{Type: "tool_input_delta", ToolUseID: "truncated", InputDelta: "{"},
		{Type: "tool_use_stop", ToolUseID: "truncated", InputDelta: "{}"},
		{Type: "message_delta", StopReason: "max_tokens"}, {Type: "message_stop"},
	}}
	provider := &queuedStreamProvider{streams: []llm.StreamReader{stream}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	var sideEffects atomic.Int32
	loop.Registry.Register(outputRecoveryProbe{calls: &sideEffects})
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 2048), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.capturedRequests()); got != 1 || sideEffects.Load() != 0 {
		t.Fatalf("truncated call crossed boundary: requests=%d effects=%d", got, sideEffects.Load())
	}
}

func TestSubAgentOutputRecoveryNeverExecutesNativeOrTextProtocolCalls(t *testing.T) {
	for _, mode := range []string{"native", "text"} {
		t.Run(mode, func(t *testing.T) {
			var attempted llm.StreamReader
			if mode == "native" {
				attempted = toolUseStream("forbidden", "OutputRecoveryProbe", "{}")
			} else {
				attempted = textStream(`<tool_call><function=OutputRecoveryProbe><parameter=ignored>value</parameter></function></tool_call>`)
			}
			provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("partial answer", "max_tokens"), attempted}}
			_, transcript, loop, teammate := continuationFixture(t, provider)
			var effects atomic.Int32
			loop.Registry.Register(outputRecoveryProbe{calls: &effects})
			loop.RecoverTextToolCalls = true
			out := make(chan Event, 4096)
			if err := RunSubAgentWithContinuations(context.Background(), out, loop, teammate, transcript); err != nil {
				t.Fatal(err)
			}
			if len(provider.capturedRequests()) != 2 || effects.Load() != 0 {
				t.Fatalf("unsafe call executed or retried: requests=%d effects=%d", len(provider.capturedRequests()), effects.Load())
			}
			close(out)
			stop := ""
			for event := range out {
				if event.Kind == EventLoopDone {
					stop = event.StopReason
				}
			}
			if stop != "provider_protocol_error" {
				t.Fatalf("unaccepted recovery call stop=%q", stop)
			}
		})
	}
}

func TestSubAgentOutputRecoveryDoesNotReplayExecutedToolEffects(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		toolUseStream("one-effect", "OutputRecoveryProbe", "{}"),
		outputLimitStream("partial after effect", "max_tokens"),
		textStream(" final answer"),
	}}
	dir, transcript, loop, teammate := continuationFixture(t, provider)
	var effects atomic.Int32
	loop.Registry.Register(outputRecoveryProbe{calls: &effects})
	loop.RecoverTextToolCalls = true
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 4096), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	requests := provider.capturedRequests()
	if effects.Load() != 1 || len(requests) != 3 || len(requests[2].Tools) != 0 {
		t.Fatalf("tool effect replayed: effects=%d requests=%d", effects.Load(), len(requests))
	}
	saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	calls, results := 0, 0
	for _, message := range saved.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" {
				calls++
			}
			if block.Type == "tool_result" {
				results++
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("durable tool occurrence duplicated: calls=%d results=%d", calls, results)
	}
}

type outputRecoveryCancelProvider struct {
	*queuedStreamProvider
	entered chan struct{}
}

type outputRecoveryPartialFailure struct{ loopRegressionStream }

func (s *outputRecoveryPartialFailure) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, context.Canceled
	}
	return s.loopRegressionStream.Recv()
}

func TestSubAgentOutputRecoveryPartialFailureKeepsCanonicalOutputConsistent(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("PART_A ", "max_tokens"), &outputRecoveryPartialFailure{loopRegressionStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "PART_A new partial"}}}}}}
	dir, transcript, loop, teammate := continuationFixture(t, provider)
	out := make(chan Event, 4096)
	err := RunSubAgentWithContinuations(context.Background(), out, loop, teammate, transcript)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected partial error: %v", err)
	}
	close(out)
	var visible strings.Builder
	for event := range out {
		if event.Kind == EventTextDelta {
			visible.WriteString(event.TextDelta)
		}
	}
	saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	var canonical strings.Builder
	for _, message := range saved.Messages {
		if message.Role == llm.RoleAssistant {
			canonical.WriteString(textOf(message))
		}
	}
	if visible.String() != "PART_A new partial" || canonical.String() != visible.String() {
		t.Fatalf("partial failure diverged: visible=%q canonical=%q", visible.String(), canonical.String())
	}
}

func (p *outputRecoveryCancelProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	stream, err := p.queuedStreamProvider.Stream(ctx, req)
	if len(p.capturedRequests()) == 2 {
		close(p.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return stream, err
}

func TestSubAgentOutputRecoveryRetainsOriginalCancellationAndLivePhase(t *testing.T) {
	provider := &outputRecoveryCancelProvider{queuedStreamProvider: &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("partial answer", "max_tokens"), textStream("unused")}}, entered: make(chan struct{})}
	dir, transcript, loop, teammate := continuationFixture(t, provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunSubAgentWithContinuations(ctx, make(chan Event, 4096), loop, teammate, transcript) }()
	select {
	case <-provider.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation did not start")
	}
	snapshot := teammate.Snapshot()
	if snapshot.ExecutionPhase != "continuing" || snapshot.ContinuationAttempts != 1 {
		t.Fatalf("live recovery machine state=%#v", snapshot)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not join cancelled provider")
	}
	snapshot = teammate.Snapshot()
	if snapshot.ExecutionPhase == "continuing" || snapshot.ContinuationAttempts != 1 || len(provider.capturedRequests()) != 2 {
		t.Fatalf("cancelled recovery state=%#v", snapshot)
	}
	saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, message := range saved.Messages {
		body += textOf(message)
	}
	if !strings.Contains(body, "partial answer") || !strings.Contains(body, "subagent_output_continuation") {
		t.Fatal("cancel lost durable output or continuation boundary")
	}
}

func TestSubAgentOutputRecoveryCheckpointFailureStopsBeforeAnotherRequest(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("partial answer", "max_tokens"), textStream("must not be requested")}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	loop.HistoryCheckpoint = func(history []llm.Message) error {
		for _, message := range history {
			for _, block := range message.Content {
				if block.ProviderHint["metis.output_continuation"] != "" {
					return errors.New("durable store unavailable")
				}
			}
		}
		return transcript.Checkpoint(history)
	}
	err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 4096), loop, teammate, transcript)
	if err == nil || !strings.Contains(err.Error(), "durable store unavailable") || len(provider.capturedRequests()) != 1 {
		t.Fatalf("checkpoint failure crossed provider boundary: %v requests=%d", err, len(provider.capturedRequests()))
	}
}

func TestSubAgentOutputRecoveryUsageAndTraceAreNotResetOrDoubleCounted(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("partial answer", "max_tokens"), outputLimitStream("final answer", "end_turn")}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	loop.Budget = budget.NewTracker(0, budget.Rates{OutputPerMTok: 1_000_000})
	var mu sync.Mutex
	physical := 0
	SetTraceHook(func(ev Event) {
		if ev.Kind == EventTokens {
			mu.Lock()
			physical++
			mu.Unlock()
		}
	})
	defer SetTraceHook(nil)
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 4096), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := physical
	mu.Unlock()
	if loop.Budget.SpentUSD() != 40 || count != 2 {
		t.Fatalf("usage lost or duplicated: spend=%v physical=%d", loop.Budget.SpentUSD(), count)
	}
}

func TestSubAgentOutputRecoveryPreservesSharedBudgetStop(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("partial answer", "max_tokens"), textStream("must not be requested")}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	loop.Budget = budget.NewTracker(10, budget.Rates{OutputPerMTok: 1_000_000})
	out := make(chan Event, 4096)
	if err := RunSubAgentWithContinuations(context.Background(), out, loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	close(out)
	stop := ""
	for event := range out {
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if len(provider.capturedRequests()) != 1 || loop.Budget.SpentUSD() != 20 || stop != "budget_usd" {
		t.Fatalf("continuation reset shared spend: requests=%d spend=%v stop=%q", len(provider.capturedRequests()), loop.Budget.SpentUSD(), stop)
	}
}

func TestSubAgentOutputRecoveryParentInputIsNotConsumedWithoutRunning(t *testing.T) {
	for _, forceSteer := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "accepted-before-recovery-flag"}[forceSteer], func(t *testing.T) {
			stream := &gatedRegressionStream{started: make(chan struct{}), release: make(chan struct{}), events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "TAIL"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}
			provider := &queuedStreamProvider{streams: []llm.StreamReader{outputLimitStream("PARTIAL ", "max_tokens"), stream, toolUseStream("followup-effect", "OutputRecoveryProbe", "{}"), textStream("FOLLOWUP_DONE")}}
			dir, transcript, loop, teammate := continuationFixture(t, provider)
			var effects atomic.Int32
			loop.Registry.Register(outputRecoveryProbe{calls: &effects})
			done := make(chan error, 1)
			go func() {
				done <- RunSubAgentWithContinuations(context.Background(), make(chan Event, 4096), loop, teammate, transcript)
			}()
			select {
			case <-stream.started:
			case <-time.After(3 * time.Second):
				t.Fatal("recovery did not start")
			}
			receipt, err := teammate.SubmitContinuation("parent", "perform the follow-up effect")
			if err != nil || receipt.Status != "queued" {
				t.Fatalf("follow-up receipt=%#v err=%v", receipt, err)
			}
			if forceSteer {
				pending := transcript.PendingFollowups()
				if len(pending) != 1 || !loop.SteerInject(pending[0].UserText()) {
					t.Fatal("could not reproduce pre-flag steering race")
				}
			}
			close(stream.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("parent follow-up did not finish")
			}
			requests := provider.capturedRequests()
			if len(requests) != 4 || len(requests[1].Tools) != 0 || len(requests[2].Tools) == 0 || effects.Load() != 1 {
				t.Fatalf("durable parent input was swallowed: requests=%d effects=%d", len(requests), effects.Load())
			}
			if len(transcript.PendingFollowups()) != 0 {
				t.Fatal("executed follow-up left pending")
			}
			saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			occurrences := 0
			for _, message := range saved.Messages {
				occurrences += strings.Count(textOf(message), receipt.InputID)
			}
			if occurrences != 1 {
				t.Fatalf("follow-up persisted %d times", occurrences)
			}
		})
	}
}

func TestSubAgentOutputCapMetersConcreteRequestWithoutChangingModelCapacity(t *testing.T) {
	provider := &queuedStreamProvider{}
	_, _, loop, _ := continuationFixture(t, provider)
	loop.RequestMaxTokens = 1024
	loop.contextCapabilities.MaxOutputTokens = 8192
	req, anchor := loop.buildRequestWithContext(nil)
	if req.MaxTokens != 1024 || anchor.maxOutput != 1024 || loop.ContextMetadataSnapshot().MaxOutputTokens != 8192 {
		t.Fatalf("request cap and advertised capacity conflated: request=%d anchor=%d metadata=%d", req.MaxTokens, anchor.maxOutput, loop.ContextMetadataSnapshot().MaxOutputTokens)
	}
}
