package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type liveContextCapabilitiesProvider struct {
	mu           sync.Mutex
	capabilities llm.ContextCapabilities
	next         llm.ContextCapabilities
	refreshErr   error
	refreshCalls int
	streamCheck  func() error
	requestCheck func(llm.Request) error
	refreshHook  func(context.Context) error
}

func (*liveContextCapabilitiesProvider) Name() string    { return "live-context" }
func (*liveContextCapabilitiesProvider) ModelID() string { return "same-model" }
func (p *liveContextCapabilitiesProvider) MaxContextTokens() int {
	return p.ContextCapabilities().Window
}
func (p *liveContextCapabilitiesProvider) ContextCapabilities() llm.ContextCapabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capabilities
}
func (p *liveContextCapabilitiesProvider) RefreshContextCapabilities(ctx context.Context) error {
	p.mu.Lock()
	p.refreshCalls++
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return err
	}
	if p.refreshErr != nil {
		err := p.refreshErr
		p.mu.Unlock()
		return err
	}
	if p.next.Window > 0 {
		p.capabilities = p.next
	}
	hook := p.refreshHook
	p.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return nil
}
func (*liveContextCapabilitiesProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("expected streaming")
}
func (p *liveContextCapabilitiesProvider) Stream(_ context.Context, request llm.Request) (llm.StreamReader, error) {
	if p.requestCheck != nil {
		if err := p.requestCheck(request); err != nil {
			return nil, err
		}
	}
	if p.streamCheck != nil {
		if err := p.streamCheck(); err != nil {
			return nil, err
		}
	}
	return &activeContextTestStream{events: activeContextResponse("answer", 200, 0, 0, 10)}, nil
}

// A metadata-bearing provider can be discovered after Loop construction. The
// very first request must use one coherent window for schemas and compaction.
func TestContextCapabilitiesRunRefreshesColdBinding(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{
		capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"},
		next:         llm.ContextCapabilities{Window: 272_000, DefaultWindow: 272_000, MaxWindow: 872_000, MaxOutputTokens: 128_000, Source: "endpoint"},
	}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "unchanged system", 4)
	loop.Model = provider.ModelID()
	loop.ContextWindow = 200_000
	loop.Compactor = NewCompactor(DefaultCompactionConfig(), loop.Model, loop.ContextWindow, provider)
	loop.Compactor.MaxOutputTokens = 4_000
	provider.streamCheck = func() error {
		window, _, _ := loop.ContextStatusSnapshot()
		if window != 272_000 || loop.Compactor.MaxContextTokens != window {
			return errors.New("cold metadata never reached the request and compactor binding")
		}
		return nil
	}
	runActiveContextTurn(t, loop, "hello")
}

func capabilitiesLoop(provider *liveContextCapabilitiesProvider) *Loop {
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "preserved system", 4)
	loop.Model = provider.ModelID()
	loop.contextCapabilities = provider.ContextCapabilities()
	loop.ContextWindow = loop.contextCapabilities.Window
	loop.SystemSections = []llm.SystemSection{{Name: "base", Body: "preserved system", Cache: true}}
	loop.Compactor = NewCompactor(DefaultCompactionConfig(), loop.Model, loop.ContextWindow, provider)
	loop.Compactor.MaxOutputTokens = 4_000
	loop.Compactor.ApplyWindowTier(loop.ContextWindow - 4_000)
	return loop
}

func TestContextCapabilitiesRefreshPreservesIdentityAndCompactionState(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{
		capabilities: llm.ContextCapabilities{Window: 16_000, Source: "fallback"},
		next:         llm.ContextCapabilities{Window: 272_000, DefaultWindow: 272_000, MaxWindow: 872_000, MaxOutputTokens: 128_000, Source: "endpoint"},
	}
	loop := capabilitiesLoop(provider)
	loop.Compactor.LastSummary = "preserved accumulated summary"
	loop.Compactor.consecutiveFailures = 2
	loop.Compactor.MicrocompactDir = t.TempDir()
	tracker := budget.NewTracker(12, budget.Rates{InputPerMTok: 2})
	loop.Budget = tracker
	tracker.AddUsage(1000, 0, 0, 0)
	before := loop.ProviderRuntimeState()
	previousCompactor := loop.Compactor
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	caps, threshold, trigger := loop.ContextDetailsSnapshot()
	if caps != provider.next || threshold != loop.Compactor.Threshold || trigger != loop.Compactor.TriggerTokens() {
		t.Fatalf("incoherent context details: %+v threshold=%v trigger=%d", caps, threshold, trigger)
	}
	after := loop.ProviderRuntimeState()
	if after.Provider != before.Provider || after.Model != before.Model || after.System != before.System || !reflect.DeepEqual(after.SystemSections, before.SystemSections) || loop.Budget != tracker {
		t.Fatal("metadata refresh changed runtime identity, prompt, or budget tracker")
	}
	if loop.Compactor == previousCompactor || loop.Compactor.MaxContextTokens != caps.Window || loop.Compactor.MaxOutputTokens != 4_000 || loop.Compactor.SnipMaxToolResultChars != 6_000 {
		t.Fatalf("compactor binding not refreshed without inflating output budget: %+v", loop.Compactor)
	}
	if loop.Compactor.LastSummary != previousCompactor.LastSummary || loop.Compactor.consecutiveFailures != 2 || loop.Compactor.MicrocompactDir != previousCompactor.MicrocompactDir {
		t.Fatal("refresh lost compactor state")
	}
	tracker.AddUsage(1000, 0, 0, 0)
	if got := tracker.SpentUSD(); got != .004 {
		t.Fatalf("budget rates/spend changed: %v", got)
	}
	stable := loop.Compactor
	revision := loop.routingRevision
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loop.Compactor != stable || loop.routingRevision != revision {
		t.Fatal("unchanged metadata replaced the binding")
	}
}

func TestContextCapabilitiesRefreshCancellationAndFailurePreserveBinding(t *testing.T) {
	for _, kind := range []string{"already-cancelled", "cancelled-during-refresh", "refresh-error"} {
		t.Run(kind, func(t *testing.T) {
			provider := &liveContextCapabilitiesProvider{
				capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"},
				next:         llm.ContextCapabilities{Window: 272_000, Source: "endpoint"},
			}
			loop := capabilitiesLoop(provider)
			before := loop.ContextMetadataSnapshot()
			previousCompactor := loop.Compactor
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "already-cancelled":
				cancel()
			case "cancelled-during-refresh":
				provider.refreshHook = func(context.Context) error { cancel(); return nil }
			case "refresh-error":
				provider.refreshErr = errors.New("metadata unavailable")
			}
			if err := loop.RefreshCapabilities(ctx); err == nil {
				t.Fatal("expected refresh failure")
			}
			if loop.ContextMetadataSnapshot() != before || loop.Compactor != previousCompactor {
				t.Fatal("failed refresh changed installed context")
			}
		})
	}
}

func TestContextCapabilitiesSyncDefersDuringCompactionAndDoesNotRefreshNetwork(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}}
	loop := capabilitiesLoop(provider)
	provider.mu.Lock()
	provider.capabilities = llm.ContextCapabilities{Window: 272_000, DefaultWindow: 272_000, MaxWindow: 872_000, Source: "endpoint"}
	provider.mu.Unlock()
	loop.compactMu.Lock()
	done := make(chan llm.ContextCapabilities, 1)
	go func() { loop.SyncContextCapabilities(); done <- loop.ContextMetadataSnapshot() }()
	select {
	case caps := <-done:
		if caps.Window != 200_000 || caps.Source != "fallback" {
			t.Fatal("displayed an uninstalled capability update")
		}
	case <-time.After(time.Second):
		loop.compactMu.Unlock()
		t.Fatal("status waited for compaction")
	}
	loop.compactMu.Unlock()
	loop.SyncContextCapabilities()
	if caps := loop.ContextMetadataSnapshot(); caps.Window != 272_000 || caps.MaxWindow != 872_000 {
		t.Fatalf("deferred refresh lost: %+v", caps)
	}
	provider.mu.Lock()
	calls := provider.refreshCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatal("status sampler invoked provider refresh I/O")
	}
}

func TestContextCapabilitiesSourceUpdateKeepsBindingAndInvalidOutputIsRejected(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}}
	loop := capabilitiesLoop(provider)
	compactor, revision := loop.Compactor, loop.routingRevision
	provider.mu.Lock()
	provider.capabilities = llm.ContextCapabilities{Window: 200_000, DefaultWindow: 200_000, Source: "endpoint"}
	provider.mu.Unlock()
	loop.SyncContextCapabilities()
	if caps := loop.ContextMetadataSnapshot(); caps.Source != "endpoint" || caps.DefaultWindow != 200_000 {
		t.Fatalf("metadata source not refreshed: %+v", caps)
	}
	if loop.Compactor != compactor || loop.routingRevision != revision || loop.Compactor.MaxOutputTokens != 4_000 {
		t.Fatal("metadata-only update replaced request state or inflated output")
	}
	provider.mu.Lock()
	provider.capabilities.MaxWindow = 800_000
	provider.capabilities.MaxOutputTokens = 128_000
	provider.mu.Unlock()
	loop.SyncContextCapabilities()
	if loop.Compactor != compactor || loop.routingRevision == revision || loop.Compactor.MaxOutputTokens != 4_000 {
		t.Fatal("physical limit update must fence stale usage without changing compaction or output budget")
	}
	provider.mu.Lock()
	provider.capabilities.MaxOutputTokens = 2_000
	provider.mu.Unlock()
	before := loop.ContextMetadataSnapshot()
	if err := loop.RefreshCapabilities(context.Background()); err == nil {
		t.Fatal("discovered output limit below configured budget was accepted")
	}
	if loop.Compactor.MaxOutputTokens != 4_000 || loop.ContextMetadataSnapshot() != before {
		t.Fatal("invalid discovered limit silently changed reservation or metadata")
	}
}

func TestContextCapabilitiesRefreshRejectsStaleRequestUsage(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}}
	loop := capabilitiesLoop(provider)
	loop.AppendUser("question")
	_, anchor := loop.buildRequestWithContext(nil)
	provider.mu.Lock()
	provider.capabilities = llm.ContextCapabilities{Window: 272_000, Source: "endpoint"}
	provider.mu.Unlock()
	loop.SyncContextCapabilities()
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, msg(llm.RoleAssistant, "old response"))
	loop.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 10}, anchor)
	valid := loop.activeContext.valid
	loop.mu.Unlock()
	if valid {
		t.Fatal("old capability-generation response became the active snapshot")
	}
	_, current := loop.buildRequestWithContext(nil)
	if current.maxContext != 272_000 {
		t.Fatalf("anchor cap=%d, want installed cap", current.maxContext)
	}
}

func TestContextCapabilitiesConcurrentCachedUpdatesAndSnapshots(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "endpoint"}}
	loop := capabilitiesLoop(provider)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			provider.mu.Lock()
			window := 200_000 + (i%2)*72_000
			provider.capabilities = llm.ContextCapabilities{Window: window, DefaultWindow: window, MaxWindow: window + 600_000, Source: "endpoint"}
			provider.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			loop.SyncContextCapabilities()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			caps, threshold, trigger := loop.ContextDetailsSnapshot()
			if trigger != int(float64(caps.Window-4_000)*threshold) {
				t.Errorf("mixed capability/compactor versions: %+v threshold=%v trigger=%d", caps, threshold, trigger)
				return
			}
			_ = loop.ToolSpecsSnapshot()
		}
	}()
	wg.Wait()
}

func TestContextCapabilitiesLegacyProviderDoesNotInventLimits(t *testing.T) {
	provider := &activeContextTestProvider{name: "legacy", model: "model"}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 2)
	loop.Model = "model"
	loop.SyncContextCapabilities()
	if caps := loop.ContextMetadataSnapshot(); caps.Window != 128_000 || caps.DefaultWindow != 0 || caps.MaxWindow != 0 || caps.MaxOutputTokens != 0 || caps.Source != "provider_fallback" {
		t.Fatalf("invented legacy provider capabilities: %+v", caps)
	}
}

func TestContextCapabilitiesRunRefreshesEachRequestBoundary(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "endpoint"}}
	loop := capabilitiesLoop(provider)
	requests := 0
	provider.streamCheck = func() error {
		requests++
		if requests == 1 {
			provider.mu.Lock()
			provider.next = llm.ContextCapabilities{Window: 272_000, DefaultWindow: 272_000, MaxWindow: 872_000, Source: "endpoint"}
			provider.mu.Unlock()
			if !loop.SteerInject("continue") {
				return errors.New("steering was not accepted")
			}
		} else if requests == 2 {
			caps, _, _ := loop.ContextDetailsSnapshot()
			if caps.Window != 272_000 {
				return errors.New("second request retained the previous capability version")
			}
		} else {
			return errors.New("unexpected extra request")
		}
		return nil
	}
	runActiveContextTurn(t, loop, "start")
	if requests != 2 {
		t.Fatalf("requests=%d, want 2", requests)
	}
}

func TestContextCapabilitiesRefreshCannotReplaceConcurrentModelSwitch(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{
		capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"},
		next:         llm.ContextCapabilities{Window: 272_000, Source: "endpoint"},
	}
	loop := capabilitiesLoop(provider)
	nextProvider := &activeContextTestProvider{name: "new-provider", model: "new-model"}
	provider.refreshHook = func(context.Context) error {
		loop.RebindProviderModel(nextProvider, "new-model")
		return nil
	}
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, model, window := loop.ProviderModelSnapshot()
	if current != nextProvider || model != "new-model" || window != 128_000 || loop.ContextMetadataSnapshot().Window != window {
		t.Fatal("obsolete discovery replaced the concurrent model switch")
	}
}

type blockedSummaryCapabilitiesProvider struct {
	*blockingCompactProvider
	mu           sync.Mutex
	capabilities llm.ContextCapabilities
}

func (p *blockedSummaryCapabilitiesProvider) ContextCapabilities() llm.ContextCapabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capabilities
}

func (p *blockedSummaryCapabilitiesProvider) MaxContextTokens() int {
	return p.ContextCapabilities().Window
}
func (*blockedSummaryCapabilitiesProvider) RefreshContextCapabilities(ctx context.Context) error {
	return ctx.Err()
}

func TestContextCapabilitiesStatusDoesNotWaitForLongSummary(t *testing.T) {
	provider := &blockedSummaryCapabilitiesProvider{
		blockingCompactProvider: &blockingCompactProvider{name: "same-provider", model: "same-model", started: make(chan struct{}), release: make(chan struct{})},
		capabilities:            llm.ContextCapabilities{Window: 128_000, Source: "fallback"},
	}
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	loop := &Loop{
		Provider: provider, Model: provider.ModelID(), ContextWindow: 128_000,
		contextCapabilities: provider.ContextCapabilities(),
		Compactor:           NewCompactor(cfg, provider.ModelID(), 128_000, provider), Messages: unifiedPipelineHistory(),
	}
	done := make(chan error, 1)
	go func() {
		_, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
		done <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(3 * time.Second):
		close(provider.release)
		t.Fatal("summary never entered its blocking provider call")
	}
	provider.mu.Lock()
	provider.capabilities = llm.ContextCapabilities{Window: 272_000, MaxWindow: 872_000, Source: "endpoint"}
	provider.mu.Unlock()
	status := make(chan llm.ContextCapabilities, 1)
	go func() { loop.SyncContextCapabilities(); caps, _, _ := loop.ContextDetailsSnapshot(); status <- caps }()
	select {
	case caps := <-status:
		if caps.Window != 128_000 {
			t.Error("status observed an uninstalled context during compaction")
		}
	case <-time.After(time.Second):
		t.Error("status blocked behind the provider's long summary")
	}
	close(provider.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("summary did not join after release")
	}
	loop.SyncContextCapabilities()
	if caps := loop.ContextMetadataSnapshot(); caps.Window != 272_000 {
		t.Fatal("metadata stayed stale after compaction finished")
	}
}

func TestContextCapabilitiesRejectsWindowBelowConfiguredOutputBudget(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{
		capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"},
		next:         llm.ContextCapabilities{Window: 3_000, Source: "endpoint"},
	}
	loop := capabilitiesLoop(provider)
	before := loop.ContextMetadataSnapshot()
	if err := loop.RefreshCapabilities(context.Background()); err == nil {
		t.Fatal("invalid window/output binding was accepted")
	}
	if loop.ContextMetadataSnapshot() != before || loop.Compactor.MaxOutputTokens != 4_000 {
		t.Fatal("invalid window silently changed configured budget")
	}
}

func TestContextCapabilitiesRunRejectsInvalidBudgetBeforeProviderRequest(t *testing.T) {
	for _, capabilities := range []llm.ContextCapabilities{
		{Window: 200_000, MaxOutputTokens: 2_000, Source: "endpoint"},
		{Window: 3_000, Source: "endpoint"},
	} {
		t.Run(istr(capabilities.Window)+"-"+istr(capabilities.MaxOutputTokens), func(t *testing.T) {
			provider := &liveContextCapabilitiesProvider{
				capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"},
				next:         capabilities,
			}
			loop := capabilitiesLoop(provider)
			before := loop.ContextMetadataSnapshot()
			compactor := loop.Compactor
			requests := 0
			provider.streamCheck = func() error { requests++; return nil }
			loop.AppendUser("do not send an invalid generation budget")
			err := loop.Run(context.Background(), make(chan Event, 64))
			if !errors.Is(err, ErrContextCapabilitiesBudget) {
				t.Fatalf("Run error=%v, want budget conflict", err)
			}
			if requests != 0 {
				t.Fatalf("invalid budget reached %d provider requests", requests)
			}
			if loop.ContextMetadataSnapshot() != before || loop.Compactor != compactor || loop.Compactor.MaxOutputTokens != 4_000 {
				t.Fatal("budget rejection changed the installed binding")
			}
		})
	}
}

func TestContextCapabilitiesDirectBusyRefreshPreservesBinding(t *testing.T) {
	for _, name := range []string{"smaller-window", "input-ceiling", "invalid-output"} {
		t.Run(name, func(t *testing.T) {
			provider := &liveContextCapabilitiesProvider{
				capabilities: llm.ContextCapabilities{Window: 200_000, Source: "endpoint"},
				next:         llm.ContextCapabilities{Window: 16_000, Source: "endpoint"},
			}
			wantErr := ErrContextCapabilitiesBusy
			switch name {
			case "input-ceiling":
				provider.next.Window = 200_000
				provider.next.MaxInputTokens = 100_000
			case "invalid-output":
				provider.next.MaxOutputTokens = 2_000
				wantErr = ErrContextCapabilitiesBudget
			}
			loop := capabilitiesLoop(provider)
			before := loop.ContextMetadataSnapshot()
			requests := 0
			provider.streamCheck = func() error { requests++; return nil }
			loop.compactMu.Lock()
			err := loop.RefreshCapabilities(context.Background())
			if !errors.Is(err, wantErr) {
				t.Errorf("Run error=%v, want %v", err, wantErr)
			}
			if requests != 0 || loop.ContextMetadataSnapshot() != before {
				t.Error("busy or invalid metadata reached generation or changed the binding")
			}
			loop.compactMu.Unlock()
			if name == "invalid-output" {
				provider.next.MaxOutputTokens = 4_000
			}
			runActiveContextTurn(t, loop, "retry after compaction released")
			if requests != 1 || loop.ContextMetadataSnapshot() != provider.next {
				t.Fatalf("retry did not install the new binding: requests=%d caps=%+v", requests, loop.ContextMetadataSnapshot())
			}
		})
	}
}

func TestContextCapabilitiesRunWaitsForBusyBindingOrCancelsWithoutGeneration(t *testing.T) {
	for _, cancelWhileBusy := range []bool{false, true} {
		name := "released"
		if cancelWhileBusy {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			provider := &liveContextCapabilitiesProvider{
				capabilities: llm.ContextCapabilities{Window: 200_000, Source: "endpoint"},
				next:         llm.ContextCapabilities{Window: 16_000, MaxInputTokens: 10_000, Source: "endpoint"},
			}
			loop := capabilitiesLoop(provider)
			before := loop.ContextMetadataSnapshot()
			var requests atomic.Int32
			provider.streamCheck = func() error { requests.Add(1); return nil }
			refreshStarted := make(chan struct{})
			var once sync.Once
			provider.refreshHook = func(context.Context) error { once.Do(func() { close(refreshStarted) }); return nil }
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			loop.compactMu.Lock()
			locked := true
			defer func() {
				if locked {
					loop.compactMu.Unlock()
				}
			}()
			loop.AppendUser("wait for the current compaction before querying")
			done := make(chan error, 1)
			go func() { done <- loop.Run(ctx, make(chan Event, 64)) }()
			select {
			case <-refreshStarted:
			case <-ctx.Done():
				t.Fatal("request never reached capability refresh")
			}
			select {
			case err := <-done:
				t.Fatalf("busy request returned before release/cancellation: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			loop.SyncContextCapabilities()
			if requests.Load() != 0 || loop.ContextMetadataSnapshot() != before {
				t.Fatal("waiting request used or displayed an uninstalled capability")
			}
			provider.mu.Lock()
			refreshCalls := provider.refreshCalls
			provider.mu.Unlock()
			if refreshCalls != 1 {
				t.Fatalf("busy retries performed %d discovery calls, want one", refreshCalls)
			}
			if cancelWhileBusy {
				cancel()
			} else {
				loop.compactMu.Unlock()
				locked = false
			}
			select {
			case err := <-done:
				if cancelWhileBusy {
					if !errors.Is(err, context.Canceled) || requests.Load() != 0 {
						t.Fatalf("cancelled busy query: err=%v requests=%d", err, requests.Load())
					}
				} else if err != nil || requests.Load() != 1 || loop.ContextMetadataSnapshot() != provider.next {
					t.Fatalf("released query did not bind metadata before generation: err=%v requests=%d caps=%+v", err, requests.Load(), loop.ContextMetadataSnapshot())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("busy request did not join after release/cancellation")
			}
		})
	}
}

func TestContextCapabilitiesUnchangedBindingDoesNotWaitForCompactor(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "endpoint"}}
	loop := capabilitiesLoop(provider)
	loop.compactMu.Lock()
	defer loop.compactMu.Unlock()
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatalf("unchanged binding returned unnecessary contention error: %v", err)
	}
}

func TestContextCapabilitiesRunColdFullCapacityPreservesLargeRequest(t *testing.T) {
	full := llm.ContextCapabilities{
		Window: 1_050_000, DefaultWindow: 272_000, MaxWindow: 1_050_000,
		MaxInputTokens: 922_000, MaxOutputTokens: 128_000,
		ProfileDefaultWindow: 272_000, ProfileMaxWindow: 872_000, Source: "catalog_profile",
	}
	provider := &liveContextCapabilitiesProvider{
		capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}, next: full,
	}
	loop := capabilitiesLoop(provider)
	loop.Compactor.MaxOutputTokens = 8_192
	loop.Compactor.Threshold = .9
	largeHistory := strings.Repeat("abcd", 300_000)
	loop.Restore([]llm.Message{
		msg(llm.RoleUser, largeHistory), msg(llm.RoleAssistant, "earlier task completed"),
		msg(llm.RoleUser, "preserve the earlier task"), msg(llm.RoleAssistant, "preserved"),
	})
	loop.AppendUser("continue with the complete context")
	before := loop.History()
	requests := 0
	provider.requestCheck = func(request llm.Request) error {
		requests++
		caps, threshold, trigger := loop.ContextDetailsSnapshot()
		if caps != full || threshold != .9 || trigger != 829_800 {
			return errors.New("request used a partial profile or stale compaction binding")
		}
		if loop.Compactor.MaxContextTokens != full.Window || loop.Compactor.MaxInputTokens != full.MaxInputTokens || loop.Compactor.MaxOutputTokens != 8_192 {
			return errors.New("request did not share the full capacity and input ceiling with its compactor")
		}
		if !reflect.DeepEqual(request.Messages, before) {
			return errors.New("300k request was summarized or replaced before reaching the provider")
		}
		input := estimateRequestOverhead(request) + estimateActiveHistoryTokens(request.Messages)
		if input < 300_000 || input > 310_000 {
			return errors.New("fixture did not assemble the expected 300k provider request")
		}
		return nil
	}
	if err := loop.Run(context.Background(), make(chan Event, 64)); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(loop.History()) != len(before)+1 {
		t.Fatalf("expected one generation without early compaction: requests=%d history=%d", requests, len(loop.History()))
	}
	if caps := loop.ContextMetadataSnapshot(); caps != full {
		t.Fatalf("status did not preserve the full runtime/profile tuple: %+v", caps)
	}
}

func TestContextCapabilitiesProfileMetadataOnlyUpdatePreservesBinding(t *testing.T) {
	full := llm.ContextCapabilities{
		Window: 1_050_000, MaxInputTokens: 922_000, MaxOutputTokens: 128_000,
		ProfileDefaultWindow: 272_000, ProfileMaxWindow: 872_000, Source: "catalog_profile",
	}
	provider := &liveContextCapabilitiesProvider{capabilities: full}
	loop := capabilitiesLoop(provider)
	loop.SyncContextCapabilities()
	before, revision := loop.Compactor, loop.routingRevision
	before.LastSummary = "preserved summary"
	provider.mu.Lock()
	provider.capabilities.ProfileDefaultWindow = 300_000
	provider.capabilities.ProfileMaxWindow = 900_000
	provider.capabilities.Source = "catalog_route"
	provider.mu.Unlock()
	loop.SyncContextCapabilities()
	updated := loop.ContextMetadataSnapshot()
	if updated.Window != full.Window || updated.MaxInputTokens != full.MaxInputTokens || updated.ProfileDefaultWindow != 300_000 || updated.ProfileMaxWindow != 900_000 || updated.Source != "catalog_route" {
		t.Fatalf("profile metadata changed the active capacity: %+v", updated)
	}
	if loop.Compactor != before || loop.routingRevision != revision || loop.Compactor.LastSummary != "preserved summary" {
		t.Fatal("profile-only metadata update rebuilt request/compaction state")
	}
}

func TestContextCapabilitiesUnknownProfileDoesNotExpandIntoAdvertisedMaximum(t *testing.T) {
	profile := llm.ContextCapabilities{
		Window: 272_000, ProfileDefaultWindow: 272_000, ProfileMaxWindow: 872_000,
		MaxOutputTokens: 128_000, Source: "endpoint_profile",
	}
	provider := &liveContextCapabilitiesProvider{capabilities: profile}
	loop := capabilitiesLoop(provider)
	loop.Compactor.Threshold = .9
	loop.SyncContextCapabilities()
	caps, threshold, trigger := loop.ContextDetailsSnapshot()
	if caps != profile || loop.Compactor.MaxContextTokens != 272_000 || loop.Compactor.MaxInputTokens != 0 {
		t.Fatalf("unresolved profile maximum invented runtime/input capacity: caps=%+v compactor=%+v", caps, loop.Compactor)
	}
	if trigger != int(float64(272_000-4_000)*threshold) {
		t.Fatalf("unknown profile changed the compression denominator: %d", trigger)
	}
}

func TestContextCapabilitiesGenericSmallEndpointRemainsConstrained(t *testing.T) {
	small := llm.ContextCapabilities{
		Window: 64_000, DefaultWindow: 64_000, MaxWindow: 64_000,
		MaxInputTokens: 20_000, MaxOutputTokens: 8_192, Source: "endpoint",
	}
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}, next: small}
	loop := capabilitiesLoop(provider)
	loop.Compactor.Threshold = .9
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	caps, _, trigger := loop.ContextDetailsSnapshot()
	if caps != small || loop.Compactor.MaxContextTokens != small.Window || loop.Compactor.MaxInputTokens != 20_000 || trigger != 18_000 {
		t.Fatalf("generic small endpoint constraints were expanded: %+v trigger=%d", caps, trigger)
	}
}

func TestContextCapabilitiesFullProfileBusyCancellationKeepsInstalledTuple(t *testing.T) {
	full := llm.ContextCapabilities{
		Window: 1_050_000, DefaultWindow: 272_000, MaxWindow: 1_050_000,
		MaxInputTokens: 922_000, MaxOutputTokens: 128_000,
		ProfileDefaultWindow: 272_000, ProfileMaxWindow: 872_000, Source: "catalog_profile",
	}
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200_000, Source: "fallback"}, next: full}
	loop := capabilitiesLoop(provider)
	before, compactor := loop.ContextMetadataSnapshot(), loop.Compactor
	loop.compactMu.Lock()
	if err := loop.RefreshCapabilities(context.Background()); !errors.Is(err, ErrContextCapabilitiesBusy) {
		loop.compactMu.Unlock()
		t.Fatalf("full profile refresh error=%v, want busy", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := loop.RefreshCapabilities(ctx)
	after := loop.ContextMetadataSnapshot()
	loop.compactMu.Unlock()
	if !errors.Is(err, context.Canceled) || after != before || loop.Compactor != compactor {
		t.Fatalf("busy/cancel published an uninstalled full profile: err=%v snapshot=%+v", err, after)
	}
	if err := loop.RefreshCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if caps := loop.ContextMetadataSnapshot(); caps != full || loop.Compactor.MaxInputTokens != full.MaxInputTokens {
		t.Fatalf("released full profile tuple did not install coherently: %+v", caps)
	}
}
