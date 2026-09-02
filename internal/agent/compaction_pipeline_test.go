package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func unifiedPipelineHistory() []llm.Message {
	messages := []llm.Message{msg(llm.RoleUser, "ancient greeting")}
	for i := 0; i < 10; i++ {
		messages = append(messages,
			msg(llm.RoleAssistant, strings.Repeat("old answer ", 12)),
			msg(llm.RoleUser, "old follow-up "+istr(i)),
		)
	}
	messages = append(messages,
		msg(llm.RoleUser, "current task"),
		msg(llm.RoleAssistant, "working"),
		msg(llm.RoleUser, "continue with the current task"),
		msg(llm.RoleAssistant, "latest answer"),
	)
	return messages
}

type blockingCompactProvider struct {
	name    string
	model   string
	started chan struct{}
	release chan struct{}
}

func (p *blockingCompactProvider) Name() string        { return p.name }
func (p *blockingCompactProvider) ModelID() string     { return p.model }
func (*blockingCompactProvider) MaxContextTokens() int { return 128_000 }
func (*blockingCompactProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("blocking compact provider expects streaming")
}
func (p *blockingCompactProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	select {
	case <-p.release:
		return &fakeStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "summary from old provider"},
			{Type: "message_stop"},
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCompactNowRejectsSummaryFromProviderReboundMidFlight(t *testing.T) {
	oldProvider := &blockingCompactProvider{
		name: "old", model: "old-model", started: make(chan struct{}), release: make(chan struct{}),
	}
	newProvider := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	before := unifiedPipelineHistory()
	loop := &Loop{
		Provider:      oldProvider,
		Model:         oldProvider.model,
		ContextWindow: oldProvider.MaxContextTokens(),
		Compactor:     NewCompactor(cfg, oldProvider.model, oldProvider.MaxContextTokens(), oldProvider),
		Messages:      cloneMessages(before),
	}

	done := make(chan error, 1)
	go func() {
		_, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
		done <- err
	}()
	select {
	case <-oldProvider.started:
	case <-time.After(3 * time.Second):
		t.Fatal("old provider summary did not start")
	}

	loop.RebindProviderModel(newProvider, "new-model")
	close(oldProvider.release)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "provider/model changed") {
			t.Fatalf("CompactNow error = %v, want routing-generation conflict", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CompactNow did not return after provider rebind")
	}
	if got := loop.History(); !reflect.DeepEqual(got, before) {
		t.Fatal("obsolete provider summary replaced live history")
	}
	provider, model, _ := loop.ProviderModelSnapshot()
	if provider != newProvider || model != "new-model" || loop.Compactor.Provider != newProvider {
		t.Fatalf("new runtime binding was lost: provider=%T model=%q compactor=%T", provider, model, loop.Compactor.Provider)
	}
}

func TestCompactNow_AllTriggersShareOneRetentionPipeline(t *testing.T) {
	triggers := []string{"auto", "manual", "overflow", "second-wind"}
	var want []llm.Message
	for _, trigger := range triggers {
		t.Run(trigger, func(t *testing.T) {
			p := &fakeSummarizer{}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = 0
			c := NewCompactor(cfg, "test", 2_000, p)
			loop := &Loop{Compactor: c, Model: "test", Messages: unifiedPipelineHistory()}
			result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: trigger, Force: true})
			if err != nil {
				t.Fatalf("CompactNow: %v", err)
			}
			if !result.Applied {
				t.Fatal("forced pipeline did not apply")
			}
			if p.calls != 1 {
				t.Fatalf("summary calls = %d, want exactly 1", p.calls)
			}
			if want == nil {
				want = result.History
			} else if !reflect.DeepEqual(result.History, want) {
				t.Fatalf("trigger %q produced different retention\n got: %#v\nwant: %#v", trigger, result.History, want)
			}
		})
	}
}

func TestCompactNow_StripsProviderStateFromReplacement(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	history := unifiedPipelineHistory()
	history[len(history)-1].Content = append(history[len(history)-1].Content, llm.ContentBlock{
		Type: "provider_state",
		ProviderHint: map[string]string{
			"openai.responses.response_id": "resp_from_old_prefix",
			"openai.responses.state_key":   "old-state-key",
		},
	})
	loop := &Loop{
		Compactor: NewCompactor(cfg, "test", 2_000, p),
		Model:     "test",
		Messages:  history,
	}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied {
		t.Fatal("forced compaction did not apply")
	}
	for _, message := range result.History {
		for _, block := range message.Content {
			if block.Type == "provider_state" {
				t.Fatalf("compacted replacement retained provider state from the old prefix: %#v", block.ProviderHint)
			}
		}
	}
}

func TestCompactNow_InvalidatesRuntimeSnapshotAfterAppliedReplacement(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{
		Compactor: NewCompactor(cfg, "test", 2_000, p),
		Model:     "test",
		Messages:  unifiedPipelineHistory(),
		SystemSections: []llm.SystemSection{
			{Name: "base", Body: "BASE", Cache: true},
		},
		CurrentStateSnapshot: func() RuntimeStateSnapshot {
			return RuntimeStateSnapshot{PermissionMode: "default"}
		},
	}
	_ = loop.buildRequest(nil)
	if loop.runtimeStateRevision != 1 || !loop.runtimeStateReady {
		t.Fatalf("precondition: revision=%d ready=%v", loop.runtimeStateRevision, loop.runtimeStateReady)
	}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied {
		t.Fatal("forced compaction did not apply")
	}
	if loop.runtimeStateReady {
		t.Fatal("applied compaction retained a stale runtime-state baseline")
	}
	_ = loop.buildRequest(nil)
	if loop.runtimeStateRevision != 2 {
		t.Fatalf("post-compact revision=%d, want full refresh revision 2", loop.runtimeStateRevision)
	}
}

func TestCompactNow_AutoDecisionCannotBeCancelledByShallowTrim(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.45
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 1_000, p)
	loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}
	if !c.ShouldCompact(loop.Messages) {
		t.Fatalf("precondition: history estimate %d did not cross trigger %d", estimateTokens(loop.Messages), c.TriggerTokens())
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "auto"})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("full checkpoint was skipped: applied=%v calls=%d", result.Applied, p.calls)
	}
	if result.AfterTokens >= result.BeforeTokens {
		t.Fatalf("auto compaction was shallow: %d -> %d", result.BeforeTokens, result.AfterTokens)
	}
}

func newCheapPreparationLoop(t *testing.T) (*Loop, *Compactor, *fakeSummarizer, int) {
	t.Helper()
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.5
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	cfg.MicrocompactDir = t.TempDir()
	cfg.MicrocompactMinChars = 100
	cfg.KeepRecentToolResults = 0
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 2
	c := NewCompactor(cfg, "test", 16_000, p)
	before := []llm.Message{
		msg(llm.RoleUser, "seed request"),
		toolUseMsg("old-output", "Bash"),
		toolResultMsg("old-output", strings.Repeat("old tool output ", 1_000)),
		msg(llm.RoleAssistant, "the old command finished"),
		msg(llm.RoleUser, "continue with the current task"),
		msg(llm.RoleAssistant, "working on the current task"),
	}
	prepared := c.Microcompact(before)
	reduction := estimateTokens(before) - estimateTokens(prepared)
	if reduction <= 0 {
		t.Fatalf("test setup made no local token reduction: before=%d prepared=%d",
			estimateTokens(before), estimateTokens(prepared))
	}
	return &Loop{Compactor: c, Model: "test", Messages: cloneMessages(before)}, c, p, reduction
}

func TestCompactNow_AutoCheapPreparationIsInstalledWithoutProvider(t *testing.T) {
	loop, c, p, reduction := newCheapPreparationLoop(t)
	before := loop.History()
	// The authoritative preflight crossed the boundary, but subtracting the
	// concrete Microcompact reduction leaves it one token below the trigger.
	pressure := c.TriggerTokens() + reduction - 1
	if pressure < c.TriggerTokens() {
		t.Fatalf("test setup pressure=%d did not cross trigger=%d", pressure, c.TriggerTokens())
	}

	var persisted []llm.Message
	persistCalls := 0
	var events []Event
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger:                "auto",
		EstimatedContextTokens: pressure,
		Persist: func(history []llm.Message) error {
			persistCalls++
			persisted = cloneMessages(history)
			return nil
		},
		Emit: func(ev Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied {
		t.Fatal("cheap auto compaction was not installed")
	}
	if p.calls != 0 {
		t.Fatalf("cheap auto compaction made %d provider calls, want 0", p.calls)
	}
	if persistCalls != 1 || !reflect.DeepEqual(persisted, result.History) {
		t.Fatalf("cheap candidate was not durably persisted: calls=%d persisted=%#v result=%#v",
			persistCalls, persisted, result.History)
	}
	if !reflect.DeepEqual(loop.History(), result.History) || reflect.DeepEqual(result.History, before) {
		t.Fatal("cheap candidate was not installed as the live replacement")
	}
	starts, successes, ends := 0, 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case EventCompactionStart:
			starts++
		case EventContextCompacted:
			successes++
		case EventCompactionEnd:
			ends++
		}
	}
	if starts != 1 || successes != 1 || ends != 1 {
		t.Fatalf("cheap lifecycle starts=%d successes=%d ends=%d events=%#v",
			starts, successes, ends, events)
	}
}

func TestCompactNow_AutoCheapPreparationPreservesAuthoritativePressureDelta(t *testing.T) {
	loop, c, p, reduction := newCheapPreparationLoop(t)
	// A naive prepared-history-only check would stop here. The authoritative
	// provider pressure still sits above the boundary after the exact local
	// reduction, so semantic summarization remains necessary.
	pressure := c.TriggerTokens() + reduction + 1
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger:                "auto",
		EstimatedContextTokens: pressure,
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("authoritative pressure was discarded: applied=%v calls=%d", result.Applied, p.calls)
	}
}

func TestCompactNow_AutoImagePruneDoesNotSubtractBase64FromAuthoritativePressure(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.85
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	cfg.MicrocompactDir = ""
	c := NewCompactor(cfg, "vision-test", 200_000, p)
	before := []llm.Message{
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: "text", Text: "inspect the old screenshot"},
				{Type: "image", MediaType: "image/png", Data: strings.Repeat("A", 210_000)},
			},
		},
		msg(llm.RoleAssistant, "old screenshot inspected"),
	}
	for i := 0; i < 5; i++ {
		before = append(before,
			msg(llm.RoleUser, "older follow-up "+istr(i)),
			msg(llm.RoleAssistant, "older answer "+istr(i)),
		)
	}
	before = append(before,
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: "text", Text: "continue from this newer screenshot"},
				{Type: "image", MediaType: "image/png", Data: "bmV3"},
			},
		},
		msg(llm.RoleAssistant, "working from the newer screenshot"),
	)
	prepared, pruned := PruneOldImages(before, keepRecentImagesFor(c.MaxContextTokens))
	if pruned != 1 {
		t.Fatalf("test setup pruned %d images, want 1", pruned)
	}
	rawReduction := estimateTokens(before) - estimateTokens(prepared)
	activeReduction := estimateActiveHistoryTokens(before) - estimateActiveHistoryTokens(prepared)
	if rawReduction <= activeReduction || activeReduction <= 0 {
		t.Fatalf("test setup did not expose image-unit mismatch: raw=%d active=%d",
			rawReduction, activeReduction)
	}
	pressure := c.TriggerTokens() + activeReduction + 1
	if pressure <= estimateTokens(before) {
		t.Fatalf("test setup pressure=%d is not authoritative over raw history=%d",
			pressure, estimateTokens(before))
	}

	loop := &Loop{Compactor: c, Model: "vision-test", Messages: cloneMessages(before)}
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger:                "auto",
		EstimatedContextTokens: pressure,
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("base64 reduction undercut authoritative pressure: applied=%v calls=%d raw_delta=%d active_delta=%d",
			result.Applied, p.calls, rawReduction, activeReduction)
	}
}

func TestCompactNow_ForcedCompactionSummarizesAfterCheapPreparation(t *testing.T) {
	loop, _, p, _ := newCheapPreparationLoop(t)
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "overflow",
		Force:   true,
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("forced compaction skipped semantic summary: applied=%v calls=%d", result.Applied, p.calls)
	}
}

func TestCompactNow_PersistFailureRollsBackHistoryAndSummary(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}
	loop.turnIdx = 7
	loop.iterIdx = 3

	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "manual",
		Force:   true,
		Persist: func([]llm.Message) error { return errors.New("disk full") },
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("persist error = %v", err)
	}
	if result.Applied {
		t.Fatal("failed durable commit reported Applied")
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("persist failure changed live history")
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("persist failure changed LastSummary: %q", c.LastSummary)
	}
	if loop.turnIdx != 7 || loop.iterIdx != 3 {
		t.Fatalf("manual pipeline reset counters: turn=%d iter=%d", loop.turnIdx, loop.iterIdx)
	}
}

func TestCompactNow_CompactErrorRollsBackLastSummary(t *testing.T) {
	p := &streamingProvider{resp: strings.Repeat("oversized checkpoint ", 5_000)}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	for i := range before {
		before[i].Content[0].Text += strings.Repeat(" original context", 40)
	}
	if estimateTokens(before) < 1_000 {
		t.Fatalf("precondition: history estimate %d is below strict progress gate", estimateTokens(before))
	}
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err == nil || !strings.Contains(err.Error(), "exceeds effective history budget") {
		t.Fatalf("CompactNow error = %v, want hard-fit failure", err)
	}
	if result.Applied {
		t.Fatal("failed compaction reported Applied")
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("failed compaction changed live history")
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("failed compaction changed LastSummary: %q", c.LastSummary)
	}
	if c.consecutiveFailures != 1 {
		t.Fatalf("real hard-fit failure count = %d, want 1", c.consecutiveFailures)
	}
}

func TestCompactNow_ContextTerminationDoesNotBurnCircuit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		makeCtx func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{name: "canceled", makeCtx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		}, wantErr: context.Canceled},
		{name: "deadline", makeCtx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Unix(1, 0))
		}, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultCompactionConfig()
			cfg.MaxSummaryRetries = 0
			cfg.MaxSummarizeInputTokens = 0
			c := NewCompactor(cfg, "test", 2_000, &errSummarizer{err: errors.New("provider should not be called")})
			c.LastSummary = "prior checkpoint"
			c.consecutiveFailures = 1
			loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}
			ctx, cancel := tc.makeCtx()
			defer cancel()

			result, err := loop.CompactNow(ctx, CompactOptions{Trigger: "manual", Force: true})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CompactNow error = %v, want %v", err, tc.wantErr)
			}
			if result.Applied {
				t.Fatal("terminated compaction reported Applied")
			}
			if c.consecutiveFailures != 1 {
				t.Fatalf("context termination changed failure count to %d, want 1", c.consecutiveFailures)
			}
			if c.LastSummary != "prior checkpoint" {
				t.Fatalf("context termination changed LastSummary: %q", c.LastSummary)
			}
		})
	}
}

func TestCompactNow_InternalSummaryTimeoutBurnsCircuit(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MaxSummaryRetries = 0
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, &errSummarizer{err: context.DeadlineExceeded})
	c.consecutiveFailures = 1
	loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}

	_, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "auto", Force: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CompactNow error = %v, want deadline exceeded", err)
	}
	if c.consecutiveFailures != 2 {
		t.Fatalf("internal timeout failure count = %d, want 2", c.consecutiveFailures)
	}
}

func TestCompactNow_SummaryFailureKeepsCircuitFailure(t *testing.T) {
	summaryErr := errors.New("summary backend failed")
	cfg := DefaultCompactionConfig()
	cfg.MaxSummaryRetries = 0
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, &errSummarizer{err: summaryErr})
	c.LastSummary = "prior checkpoint"
	c.consecutiveFailures = 1
	loop := &Loop{Compactor: c, Messages: unifiedPipelineHistory()}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "auto", Force: true})
	if err == nil || !strings.Contains(err.Error(), summaryErr.Error()) {
		t.Fatalf("CompactNow error = %v, want summary failure", err)
	}
	if result.Applied {
		t.Fatal("failed summary reported Applied")
	}
	if c.consecutiveFailures != 2 {
		t.Fatalf("summary failure count = %d, want 2", c.consecutiveFailures)
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("summary failure changed LastSummary: %q", c.LastSummary)
	}
}

func TestCompactNow_LoopCheckpointReceivesExactBeforeAndAfter(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}

	var gotBefore, gotAfter []llm.Message
	legacyCalled := false
	loop.CompactionCheckpoint = func(checkpointBefore, checkpointAfter []llm.Message) error {
		gotBefore = cloneMessages(checkpointBefore)
		gotAfter = cloneMessages(checkpointAfter)
		return nil
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "auto",
		Force:   true,
		Persist: func([]llm.Message) error {
			legacyCalled = true
			return nil
		},
	})
	if err != nil || !result.Applied {
		t.Fatalf("CompactNow result=%+v err=%v", result, err)
	}
	if legacyCalled {
		t.Fatal("legacy final-only persist callback ran in addition to loop checkpoint")
	}
	if !reflect.DeepEqual(gotBefore, before) {
		t.Fatalf("checkpoint before differs from raw history\n got: %#v\nwant: %#v", gotBefore, before)
	}
	if !reflect.DeepEqual(gotAfter, result.History) {
		t.Fatalf("checkpoint after differs from result\n got: %#v\nwant: %#v", gotAfter, result.History)
	}
	if !reflect.DeepEqual(loop.History(), gotAfter) {
		t.Fatal("live history differs from durably checkpointed replacement")
	}
}

func TestCompactNow_LoopCheckpointFailureRollsBack(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}
	loop.CompactionCheckpoint = func(checkpointBefore, checkpointAfter []llm.Message) error {
		if !reflect.DeepEqual(checkpointBefore, before) {
			t.Fatal("checkpoint callback did not receive exact raw history")
		}
		if reflect.DeepEqual(checkpointAfter, before) {
			t.Fatal("checkpoint callback did not receive compacted replacement")
		}
		return errors.New("checkpoint disk full")
	}

	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "overflow", Force: true})
	if err == nil || !strings.Contains(err.Error(), "checkpoint disk full") {
		t.Fatalf("checkpoint error = %v", err)
	}
	if result.Applied {
		t.Fatal("failed checkpoint reported Applied")
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("checkpoint failure changed live history")
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("checkpoint failure changed LastSummary: %q", c.LastSummary)
	}
}

func TestCompactNow_EmitsSingleLifecycle(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}
	var events []Event
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "manual",
		Force:   true,
		Emit:    func(ev Event) { events = append(events, ev) },
	})
	if err != nil || !result.Applied {
		t.Fatalf("CompactNow result=%+v err=%v", result, err)
	}
	starts, successes, ends := 0, 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case EventCompactionStart:
			starts++
		case EventContextCompacted:
			successes++
		case EventCompactionEnd:
			ends++
		}
	}
	if starts != 1 || successes != 1 || ends != 1 {
		t.Fatalf("lifecycle starts=%d successes=%d ends=%d events=%#v", starts, successes, ends, events)
	}
}

func TestCompactNow_EmitCanReadHistoryWithoutDeadlock(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}

	type outcome struct {
		result CompactResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := loop.CompactNow(context.Background(), CompactOptions{
			Trigger: "manual",
			Force:   true,
			Emit: func(Event) {
				_ = loop.History()
			},
		})
		done <- outcome{result: result, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil || !got.result.Applied {
			t.Fatalf("CompactNow result=%+v err=%v", got.result, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CompactNow deadlocked when Emit called History")
	}
}

func TestCompactNow_ReentrantCallbacksReturnInProgress(t *testing.T) {
	tests := []struct {
		name string
		wire func(*CompactOptions, func())
	}{
		{
			name: "emit",
			wire: func(opts *CompactOptions, nested func()) {
				opts.Emit = func(ev Event) {
					if ev.Kind == EventCompactionStart {
						nested()
					}
				}
			},
		},
		{
			name: "persist",
			wire: func(opts *CompactOptions, nested func()) {
				opts.Persist = func([]llm.Message) error {
					nested()
					return nil
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeSummarizer{}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = 0
			loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}

			var nestedErr error
			nestedCalls := 0
			opts := CompactOptions{Trigger: "manual", Force: true}
			tt.wire(&opts, func() {
				nestedCalls++
				_, nestedErr = loop.CompactNow(context.Background(), CompactOptions{Trigger: "nested", Force: true})
			})

			type outcome struct {
				result CompactResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := loop.CompactNow(context.Background(), opts)
				done <- outcome{result: result, err: err}
			}()

			select {
			case got := <-done:
				if got.err != nil || !got.result.Applied {
					t.Fatalf("outer CompactNow result=%+v err=%v", got.result, got.err)
				}
				if nestedCalls != 1 {
					t.Fatalf("nested calls=%d, want 1", nestedCalls)
				}
				if !errors.Is(nestedErr, ErrCompactionInProgress) {
					t.Fatalf("nested error=%v, want ErrCompactionInProgress", nestedErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("CompactNow deadlocked after callback re-entry")
			}
		})
	}
}

func TestCompactNow_CallbackSessionResetIsNonBlockingAndAuthoritative(t *testing.T) {
	resets := []struct {
		name string
		want []llm.Message
		run  func(*Loop, []llm.Message)
	}{
		{name: "reset", run: func(loop *Loop, _ []llm.Message) { loop.Reset() }},
		{
			name: "reset-session",
			want: []llm.Message{msg(llm.RoleUser, "replacement session")},
			run:  func(loop *Loop, history []llm.Message) { loop.ResetSession(history) },
		},
	}
	phases := []string{"emit", "pre-hook", "post-hook", "persist"}

	for _, reset := range resets {
		for _, phase := range phases {
			t.Run(reset.name+"/"+phase, func(t *testing.T) {
				p := &fakeSummarizer{}
				cfg := DefaultCompactionConfig()
				cfg.MaxSummarizeInputTokens = 0
				compactor := NewCompactor(cfg, "test", 2_000, p)
				compactor.LastSummary = "old session summary"
				loop := &Loop{Compactor: compactor, Messages: unifiedPipelineHistory()}
				opts := CompactOptions{Trigger: "manual", Force: true}

				var once sync.Once
				resetCalls := 0
				invokeReset := func() {
					once.Do(func() {
						resetCalls++
						reset.run(loop, cloneMessages(reset.want))
					})
				}
				switch phase {
				case "emit":
					opts.Emit = func(ev Event) {
						if ev.Kind == EventCompactionStart {
							invokeReset()
						}
					}
				case "pre-hook":
					hooks := NewHookRegistry()
					hooks.Register(PreCompactHandler(func(context.Context, HookContext, *PreCompact) {
						invokeReset()
					}))
					loop.Hooks = hooks
				case "post-hook":
					hooks := NewHookRegistry()
					hooks.Register(PostCompactHandler(func(context.Context, HookContext, *PostCompact) *ModifiedPostCompact {
						invokeReset()
						return nil
					}))
					loop.Hooks = hooks
				case "persist":
					opts.Persist = func([]llm.Message) error {
						invokeReset()
						return nil
					}
				}

				type outcome struct {
					result CompactResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := loop.CompactNow(context.Background(), opts)
					done <- outcome{result: result, err: err}
				}()

				select {
				case got := <-done:
					if got.err == nil || !strings.Contains(got.err.Error(), "history") {
						t.Fatalf("CompactNow result=%+v err=%v, want reset/history conflict", got.result, got.err)
					}
					if got.result.Applied {
						t.Fatal("stale compaction reported Applied after callback reset")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("callback reset deadlocked behind compactMu")
				}

				if resetCalls != 1 {
					t.Fatalf("reset calls=%d, want 1", resetCalls)
				}
				if got := loop.History(); !reflect.DeepEqual(got, reset.want) {
					t.Fatalf("callback reset was overwritten\n got: %#v\nwant: %#v", got, reset.want)
				}
				loop.mu.Lock()
				pending := loop.compactorResetPending
				loop.mu.Unlock()
				if pending || compactor.LastSummary != "" || compactor.consecutiveFailures != 0 {
					t.Fatalf("deferred compactor reset incomplete: pending=%v summary=%q failures=%d",
						pending, compactor.LastSummary, compactor.consecutiveFailures)
				}
			})
		}
	}
}

func TestCompactNow_PostHookConcurrentHistoryChangesAreNotLost(t *testing.T) {
	t.Run("append is merged after hook context", func(t *testing.T) {
		p := &fakeSummarizer{}
		cfg := DefaultCompactionConfig()
		cfg.MaxSummarizeInputTokens = 0
		entered := make(chan struct{})
		release := make(chan struct{})
		hooks := NewHookRegistry()
		hooks.Register(PostCompactHandler(func(context.Context, HookContext, *PostCompact) *ModifiedPostCompact {
			close(entered)
			<-release
			return &ModifiedPostCompact{AdditionalContext: "keep the release checklist"}
		}))
		loop := &Loop{
			Compactor: NewCompactor(cfg, "test", 2_000, p),
			Hooks:     hooks,
			Messages:  unifiedPipelineHistory(),
		}

		type outcome struct {
			result CompactResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
			done <- outcome{result: result, err: err}
		}()
		<-entered
		loop.AppendUser("concurrent note from the user")
		close(release)

		select {
		case got := <-done:
			if got.err != nil || !got.result.Applied {
				t.Fatalf("CompactNow result=%+v err=%v", got.result, got.err)
			}
			history := loop.History()
			if !historyHasText(history, "[post-compact hook context] keep the release checklist") {
				t.Fatal("PostCompact AdditionalContext was lost")
			}
			if !historyHasText(history, "concurrent note from the user") {
				t.Fatal("AppendUser during PostCompact was lost")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("CompactNow did not finish after PostCompact append")
		}
	})

	t.Run("restore wins with an explicit conflict", func(t *testing.T) {
		p := &fakeSummarizer{}
		cfg := DefaultCompactionConfig()
		cfg.MaxSummarizeInputTokens = 0
		c := NewCompactor(cfg, "test", 2_000, p)
		c.LastSummary = "prior checkpoint"
		entered := make(chan struct{})
		release := make(chan struct{})
		hooks := NewHookRegistry()
		hooks.Register(PostCompactHandler(func(context.Context, HookContext, *PostCompact) *ModifiedPostCompact {
			close(entered)
			<-release
			return &ModifiedPostCompact{AdditionalContext: "must not overwrite restore"}
		}))
		loop := &Loop{Compactor: c, Hooks: hooks, Messages: unifiedPipelineHistory()}
		restored := []llm.Message{msg(llm.RoleUser, "restored session is authoritative")}

		done := make(chan error, 1)
		go func() {
			_, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
			done <- err
		}()
		<-entered
		loop.Restore(restored)
		close(release)

		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "history replaced") {
				t.Fatalf("CompactNow error = %v, want explicit history conflict", err)
			}
			if got := loop.History(); !reflect.DeepEqual(got, restored) {
				t.Fatalf("Restore during PostCompact was overwritten\n got: %#v\nwant: %#v", got, restored)
			}
			if c.LastSummary != "prior checkpoint" {
				t.Fatalf("aborted compaction changed LastSummary: %q", c.LastSummary)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("CompactNow did not finish after PostCompact restore")
		}
	})
}

func TestCompactNow_PersistFailurePreservesConcurrentAppend(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	c := NewCompactor(cfg, "test", 2_000, p)
	c.LastSummary = "prior checkpoint"
	before := unifiedPipelineHistory()
	loop := &Loop{Compactor: c, Messages: cloneMessages(before)}

	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "manual",
		Force:   true,
		Persist: func([]llm.Message) error {
			_ = loop.History()
			loop.AppendUser("concurrent note while persistence failed")
			return errors.New("disk full after append")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full after append") {
		t.Fatalf("persist error = %v", err)
	}
	if result.Applied {
		t.Fatal("failed durable commit reported Applied")
	}
	want := append(cloneMessages(before), msg(llm.RoleUser, "concurrent note while persistence failed"))
	if got := loop.History(); !reflect.DeepEqual(got, want) {
		t.Fatalf("persist rollback lost concurrent append\n got: %#v\nwant: %#v", got, want)
	}
	if c.LastSummary != "prior checkpoint" {
		t.Fatalf("persist rollback changed LastSummary: %q", c.LastSummary)
	}
}

func TestCompactNow_PersistSuccessCompensatesConcurrentAppend(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{Compactor: NewCompactor(cfg, "test", 2_000, p), Messages: unifiedPipelineHistory()}

	var persisted [][]llm.Message
	loop.CompactionCheckpoint = func(_, after []llm.Message) error {
		persisted = append(persisted, cloneMessages(after))
		if len(persisted) == 1 {
			loop.AppendUser("concurrent note after first checkpoint")
		}
		return nil
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
	if err != nil || !result.Applied {
		t.Fatalf("CompactNow result=%+v err=%v", result, err)
	}
	if len(persisted) != 2 {
		t.Fatalf("checkpoint calls = %d, want initial + compensation", len(persisted))
	}
	if !historyHasText(result.History, "concurrent note after first checkpoint") {
		t.Fatal("result lost append made during persistence")
	}
	if !reflect.DeepEqual(persisted[len(persisted)-1], result.History) {
		t.Fatal("compensating replacement differs from final live history")
	}
}
