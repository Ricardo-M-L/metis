package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// continuationProvider is finite and entirely local: each native tool call has
// its own ID, and the final response is distinguishable from a cap rescue.
type continuationProvider struct {
	toolCalls int
	toolName  string
	repeat    bool
	requests  []llm.Request
	before    func(int, llm.Request)
}

func (*continuationProvider) Name() string          { return "iteration-continuation-test" }
func (*continuationProvider) ModelID() string       { return "test-model" }
func (*continuationProvider) MaxContextTokens() int { return 200_000 }
func (*continuationProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("test provider only streams")
}
func (p *continuationProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, req)
	n := len(p.requests)
	if p.before != nil {
		p.before(n, req)
	}
	if len(req.Tools) == 0 {
		return textStream("cap rescue rather than task completion"), nil
	}
	if n > p.toolCalls {
		return &loopRegressionStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "all requested work completed"},
			{Type: "message_delta", StopReason: "end_turn", OutputTokens: 10},
			{Type: "message_stop"},
		}}, nil
	}
	payload := n
	if p.repeat {
		payload = 1
	}
	name := p.toolName
	if name == "" {
		name = "ContinuationStep"
	}
	id := fmt.Sprintf("step-%03d", n)
	input := fmt.Sprintf(`{"step":%d}`, payload)
	return &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: id, ToolName: name},
		{Type: "tool_input_delta", ToolUseID: id, InputDelta: input},
		{Type: "tool_use_stop", ToolUseID: id},
		{Type: "message_delta", StopReason: "tool_use", OutputTokens: 10},
		{Type: "message_stop"},
	}}, nil
}

type continuationStepTool struct {
	tools.BaseTool
	mu     sync.Mutex
	calls  map[int]int
	name   string
	output string
	after  func()
}

func (s *continuationStepTool) Name() string {
	if s.name != "" {
		return s.name
	}
	return "ContinuationStep"
}
func (*continuationStepTool) Description() string { return "perform one numbered step" }
func (*continuationStepTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"step": map[string]any{"type": "integer"}}}
}
func (*continuationStepTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*continuationStepTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (s *continuationStepTool) Execute(_ context.Context, args map[string]any) (*tools.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	step := int(args["step"].(float64))
	if s.calls == nil {
		s.calls = make(map[int]int)
	}
	s.calls[step]++
	if s.after != nil {
		s.after()
	}
	if s.output != "" {
		return &tools.Result{Output: s.output}, nil
	}
	return &tools.Result{Output: fmt.Sprintf("step %d completed: %s", step, strings.Repeat("evidence ", 40))}, nil
}

func newContinuationTestLoop(cap, toolCalls int) (*Loop, *continuationProvider, *continuationStepTool) {
	p := &continuationProvider{toolCalls: toolCalls}
	step := &continuationStepTool{}
	registry := tools.NewRegistry()
	registry.Register(step)
	l := NewLoop(p, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", cap)
	l.ContinueOnIterationLimit = true
	l.CacheStats = NewCacheStatsRing(toolCalls + 10)
	l.AppendUser("perform all numbered steps, preserve completed results, then report completion")
	return l, p, step
}

func continuationResultCount(history []llm.Message) int {
	n := 0
	for _, msg := range history {
		for _, block := range msg.Content {
			if block.Type == "tool_result" {
				n++
			}
		}
	}
	return n
}

func continuationEvents(out chan Event) []Event {
	close(out)
	var events []Event
	for ev := range out {
		events = append(events, ev)
	}
	return events
}

func assertContinuationDone(t *testing.T, events []Event, reason string) {
	t.Helper()
	var stops []string
	for _, ev := range events {
		if ev.Kind == EventLoopDone {
			stops = append(stops, ev.StopReason)
		}
	}
	if len(stops) != 1 || stops[0] != reason {
		t.Fatalf("LoopDone reasons = %v, want exactly [%s]", stops, reason)
	}
}

func TestIterationContinuation_SegmentsPreserveOneRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cap      int
		steps    int
		noOpComp bool
	}{
		{name: "100_iterations_two_segments", cap: 100, steps: 125},
		{name: "305_steps_four_segments", cap: 100, steps: 305},
		{name: "small_cap_three_segments", cap: 2, steps: 5},
		{name: "no_op_compactor_three_segments", cap: 2, steps: 5, noOpComp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, p, step := newContinuationTestLoop(tc.cap, tc.steps)
			summarizer := &summarizerProvider{}
			if tc.noOpComp {
				l.Compactor = NewCompactor(Config{}, "summarizer-model", 200_000, summarizer)
			}
			persistedResults := 0
			l.HistoryCheckpoint = func(history []llm.Message) error {
				persistedResults = continuationResultCount(history)
				// This callback may inspect history; continuation injection must
				// not hold Loop.mu while invoking a durability hook.
				_ = l.History()
				return nil
			}
			p.before = func(n int, req llm.Request) {
				if got := continuationResultCount(req.Messages); got != n-1 || persistedResults != got {
					t.Fatalf("request %d has %d prior results, checkpoint has %d", n, got, persistedResults)
				}
			}
			out := make(chan Event, 8192)
			if err := l.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			events := continuationEvents(out)
			assertContinuationDone(t, events, "end_turn")
			if got := len(p.requests); got != tc.steps+1 {
				t.Fatalf("provider requests = %d, want %d; cap stopped unfinished work", got, tc.steps+1)
			}
			wantContinuations := tc.steps / tc.cap
			var notices, injections int
			for _, ev := range events {
				if ev.Kind == EventInfo && strings.Contains(ev.Info, "iteration segment") {
					notices++
					wantTotal := notices * tc.cap
					if !strings.Contains(ev.Info, fmt.Sprintf("%d iterations and %d output tokens", wantTotal, wantTotal*10)) {
						t.Fatalf("continuation lost cumulative accounting: %s", ev.Info)
					}
				}
				if ev.Kind == EventContextInjected && ev.Source == "iteration_continuation" {
					injections++
				}
				if ev.Kind == EventInfo && (strings.Contains(ev.Info, "iter nudge") || strings.Contains(ev.Info, "rescue iteration") || strings.Contains(ev.Info, "context compacted")) {
					t.Fatalf("soft segment emitted hard-budget instruction: %s", ev.Info)
				}
			}
			if notices != wantContinuations || injections != wantContinuations {
				t.Fatalf("continuation notices=%d injections=%d, want %d each", notices, injections, wantContinuations)
			}
			var reminders, realUsers int
			for _, msg := range l.History() {
				for _, block := range msg.Content {
					if strings.Contains(block.Text, "<iteration-continuation>") {
						reminders++
						if !block.Synthetic || !strings.Contains(block.Text, "<system-reminder>") {
							t.Fatal("continuation must be a synthetic system reminder")
						}
					} else if msg.Role == llm.RoleUser && block.Type == "text" && !block.Synthetic {
						realUsers++
					}
				}
			}
			if reminders != wantContinuations || realUsers != 1 {
				t.Fatalf("reminders=%d real user prompts=%d; want %d and 1", reminders, realUsers, wantContinuations)
			}
			if got := summarizer.count(); got != 0 {
				t.Fatalf("iteration boundary forced %d summarizations", got)
			}
			for i := 1; i <= tc.steps; i++ {
				if got := step.calls[i]; got != 1 {
					t.Errorf("step %d executed %d times, want exactly once", i, got)
				}
			}
			stats := l.CacheStats.Snapshot()
			if len(stats) != tc.steps+1 {
				t.Fatalf("cache stats length = %d, want %d", len(stats), tc.steps+1)
			}
			for i, stat := range stats {
				if stat.Turn != i+1 {
					t.Fatalf("cache stat %d turn = %d, want cumulative %d", i, stat.Turn, i+1)
				}
			}
		})
	}
}

func TestIterationContinuation_BoundaryCancellationAndCheckpointFailure(t *testing.T) {
	for _, failCheckpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint_failure_%t", failCheckpoint), func(t *testing.T) {
			l, p, _ := newContinuationTestLoop(2, 10)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("durable store unavailable")
			l.HistoryCheckpoint = func(history []llm.Message) error {
				if continuationResultCount(history) == 2 {
					if failCheckpoint {
						return sentinel
					}
					cancel()
				}
				return nil
			}
			out := make(chan Event, 512)
			err := l.Run(ctx, out)
			want := error(context.Canceled)
			if failCheckpoint {
				want = sentinel
			}
			if !errors.Is(err, want) {
				t.Fatalf("Run error = %v, want %v", err, want)
			}
			if len(p.requests) != 2 {
				t.Fatalf("provider requests after boundary failure = %d, want 2", len(p.requests))
			}
			if got := continuationResultCount(l.History()); got != 2 {
				t.Fatalf("completed results = %d, want 2", got)
			}
		})
	}
}

func TestIterationContinuation_USDBudgetSurvivesSegment(t *testing.T) {
	l, p, _ := newContinuationTestLoop(2, 10)
	l.Budget = budget.NewTracker(30, budget.Rates{OutputPerMTok: 1_000_000})
	out := make(chan Event, 512)
	if err := l.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	assertContinuationDone(t, continuationEvents(out), "budget_usd")
	if got := len(p.requests); got != 3 {
		t.Fatalf("requests = %d, want 3 before cumulative USD cap", got)
	}
	if got := l.Budget.SpentUSD(); got != 30 {
		t.Fatalf("spent = %g, want cumulative $30", got)
	}
}

func TestIterationContinuation_NewLoopRetainsExplicitHardCap(t *testing.T) {
	l, p, _ := newContinuationTestLoop(2, 10)
	// Raw NewLoop retains the historical bounded grace/rescue contract.
	raw := NewLoop(p, l.Registry, l.Gate, nil, "sys", 2)
	if raw.ContinueOnIterationLimit {
		t.Fatal("raw NewLoop unexpectedly enabled soft continuation")
	}
	raw.AppendUser("bounded task")
	out := make(chan Event, 512)
	if err := raw.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	assertContinuationDone(t, continuationEvents(out), "end_turn")
	if len(p.requests) != 4 || !requestContains(p.requests[3], "hit the iteration cap") || len(p.requests[3].Tools) != 0 {
		t.Fatal("explicit hard cap no longer follows grace -> tool-less final summary")
	}
}

func TestIterationContinuation_LoopDetectorSurvivesSegment(t *testing.T) {
	for _, signature := range []bool{true, false} {
		t.Run(fmt.Sprintf("signature_%t", signature), func(t *testing.T) {
			l, p, step := newContinuationTestLoop(2, 10)
			l.Detector = NewLoopDetector()
			if signature {
				p.repeat = true
				l.Detector.SignatureMaxRepeats = 2
				step.output = "same completed result"
			} else {
				l.Detector.GlobalThreshold = 3
			}
			out := make(chan Event, 512)
			if err := l.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			assertContinuationDone(t, continuationEvents(out), "loop_detected")
			wantReason := LoopGlobalCircuitBreaker
			if signature {
				wantReason = LoopSignatureRepeat
			}
			if got := l.Detector.AbortReason(); got != wantReason {
				t.Fatalf("detector reason = %q, want %q", got, wantReason)
			}
			if len(p.requests) != 3 || continuationResultCount(l.History()) != 3 || l.Detector.Stats().GlobalCount != 3 {
				t.Fatal("segment reset the detector or discarded completed results")
			}
		})
	}
}

func TestIterationContinuation_StuckResetBudgetSurvivesSegments(t *testing.T) {
	l, p, step := newContinuationTestLoop(2, 20)
	p.toolName = "Bash"
	step.name = "Bash"
	step.output = "--- FAIL: TestContinuation (0.01s)\nFAIL"
	l.Registry = tools.NewRegistry()
	l.Registry.Register(step)
	out := make(chan Event, 1024)
	if err := l.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	events := continuationEvents(out)
	assertContinuationDone(t, events, "stuck_after_reset")
	resets := 0
	for _, ev := range events {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "injecting reset reminder") {
			resets++
		}
	}
	wantCalls := stuckSigThreshold * (stuckMaxResets + 1)
	if resets != stuckMaxResets || len(p.requests) != wantCalls || continuationResultCount(l.History()) != wantCalls {
		t.Fatalf("reset notices=%d, requests=%d; want %d and %d", resets, len(p.requests), stuckMaxResets, wantCalls)
	}
}

func TestIterationContinuation_TurnDeadlineSurvivesSegments(t *testing.T) {
	t.Setenv("METIS_TURN_MAX_SECONDS", "1")
	synctest.Test(t, func(t *testing.T) {
		l, p, step := newContinuationTestLoop(1, 10)
		step.after = func() { time.Sleep(600 * time.Millisecond) }
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out := make(chan Event, 512)
		if err := l.Run(ctx, out); err != nil {
			t.Fatal(err)
		}
		assertContinuationDone(t, continuationEvents(out), "turn_wall_clock")
		if ctx.Err() != nil || time.Since(started) != 1200*time.Millisecond || len(p.requests) != 2 {
			t.Fatalf("deadline was reset: elapsed=%s requests=%d parentErr=%v", time.Since(started), len(p.requests), ctx.Err())
		}
		if continuationResultCount(l.History()) != 2 {
			t.Fatal("wall-clock boundary discarded completed results")
		}
	})
}

func TestIterationContinuation_ReminderCheckpointFailureStopsBeforeRequest(t *testing.T) {
	l, p, _ := newContinuationTestLoop(2, 10)
	sentinel := errors.New("continuation checkpoint unavailable")
	l.HistoryCheckpoint = func(history []llm.Message) error {
		for _, msg := range history {
			for _, block := range msg.Content {
				if strings.Contains(block.Text, "<iteration-continuation>") {
					if continuationResultCount(history) != 2 {
						t.Fatal("continuation checkpoint contains incomplete tool history")
					}
					return sentinel
				}
			}
		}
		return nil
	}
	out := make(chan Event, 512)
	if err := l.Run(context.Background(), out); !errors.Is(err, sentinel) {
		t.Fatalf("Run error = %v, want failed continuation checkpoint", err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("requests = %d, want no request past failed reminder checkpoint", len(p.requests))
	}
	for _, ev := range continuationEvents(out) {
		if ev.Kind == EventLoopDone {
			t.Fatalf("failed checkpoint emitted successful terminal event: %s", ev.StopReason)
		}
	}
}

func TestIterationContinuation_ContextSourceSurvivesPresentation(t *testing.T) {
	ev := Event{Kind: EventContextInjected, Source: "iteration_continuation", ContextText: iterationContinuationMessage}
	presented := ev.PresentationCopy()
	if presented.Source != ev.Source || NormalizeContextSource(ev.Source) != ev.Source {
		t.Fatalf("continuation source lost during presentation: %q", presented.Source)
	}
}

func TestIterationContinuation_DiminishingRecoverySurvivesSegments(t *testing.T) {
	l, p, step := newContinuationTestLoop(2, 7)
	step.output = "tiny"
	out := make(chan Event, 1024)
	if err := l.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	events := continuationEvents(out)
	assertContinuationDone(t, events, "end_turn")
	var recoveries int
	for _, ev := range events {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "diminishing returns detected") {
			recoveries++
		}
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "iter nudge") {
			t.Fatalf("soft progress recovery emitted a hard-budget nudge: %s", ev.Info)
		}
	}
	if recoveries != 1 {
		t.Fatalf("diminishing recovery notices = %d, want exactly one advisory across segments", recoveries)
	}
	// The first segment arms progress on iteration 2. Its streak then crosses
	// the segment seam and reaches three low-output iterations on iteration 4.
	if len(p.requests) != 8 || requestContains(p.requests[3], diminishingReturnsRescueMessage) || !requestContains(p.requests[4], diminishingReturnsRescueMessage) {
		t.Fatal("progress advisory was disabled, reset at a segment seam, or stopped unfinished work")
	}
	if continuationResultCount(l.History()) != 7 {
		t.Fatal("advisory discarded completed tool results")
	}
}

func TestIterationContinuation_RepeatGuardSurvivesSegments(t *testing.T) {
	l, p, _ := newContinuationTestLoop(2, 5)
	p.repeat = true
	l.RepeatGuard = NewRepeatGuard(RepeatGuardConfig{Thresholds: []int{3, 5}})
	out := make(chan Event, 1024)
	if err := l.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	events := continuationEvents(out)
	assertContinuationDone(t, events, "end_turn")
	var reminders int
	for _, ev := range events {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "repeat-tool-reminder") {
			reminders++
		}
	}
	if reminders != 2 || len(p.requests) != 6 {
		t.Fatalf("repeat guard reminders=%d requests=%d, want 2 and 6 across segments", reminders, len(p.requests))
	}
}
