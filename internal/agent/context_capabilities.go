package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// ErrContextCapabilitiesBudget means fresh capacity metadata proves that the
// configured generation budget cannot be sent safely. Run stops before the
// provider request rather than silently changing that budget.
var ErrContextCapabilitiesBudget = errors.New("context capabilities conflict with configured output budget")

// ErrContextCapabilitiesBusy prevents a request from using an outdated limit
// when a concurrent compaction owns the binding's mutable state. Idle status
// synchronization may defer that update, but a new provider request may not.
var ErrContextCapabilitiesBusy = errors.New("context capabilities changed while compaction is in progress")

// providerContextCapabilities is a memory-only read. Legacy providers expose
// just one window; do not invent a maximum or output ceiling for that value.
func providerContextCapabilities(provider llm.Provider) llm.ContextCapabilities {
	if provider == nil {
		return llm.ContextCapabilities{}
	}
	capabilities := llm.ContextCapabilities{}
	if metadata, ok := provider.(llm.ContextCapabilitiesProvider); ok {
		capabilities = metadata.ContextCapabilities()
	}
	if capabilities.Window <= 0 && capabilities.BudgetSource == "" && capabilities.RouteKey == "" {
		capabilities.Window = max(provider.MaxContextTokens(), 0)
		capabilities.DefaultWindow = 0
		capabilities.MaxWindow = 0
		capabilities.MaxInputTokens = 0
		capabilities.MaxOutputTokens = 0
		capabilities.Source = "provider_fallback"
	}
	if capabilities.Source == "" {
		capabilities.Source = "provider_fallback"
	}
	capabilities.DefaultWindow = max(capabilities.DefaultWindow, 0)
	capabilities.MaxWindow = max(capabilities.MaxWindow, 0)
	capabilities.MaxInputTokens = max(capabilities.MaxInputTokens, 0)
	capabilities.MaxOutputTokens = max(capabilities.MaxOutputTokens, 0)
	return capabilities
}

// RefreshCapabilities refreshes the existing provider's metadata at a request
// boundary, then atomically installs its context binding. Provider refresh owns
// its TTL and bounded I/O; no network or user callback runs under Loop.mu.
// Failure or cancellation keeps the previously installed binding intact.
//
// Compactor summary/circuit state is mutable while its network calls run with
// mu released. TryLock avoids waiting on that state, including synchronous
// hook re-entry. A changed request binding reports busy until the caller can
// retry at a later boundary; idle status synchronization may simply defer it.
func (l *Loop) RefreshCapabilities(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.RLock()
	provider, model, revision := l.Provider, l.Model, l.routingRevision
	providerID := identifyContextProvider(provider)
	l.mu.RUnlock()
	if provider == nil {
		return nil
	}
	if metadata, ok := provider.(llm.ContextCapabilitiesProvider); ok {
		if err := metadata.RefreshContextCapabilities(ctx); err != nil {
			return err
		}
	}
	capabilities := providerContextCapabilities(provider)
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.installContextCapabilities(ctx, model, revision, providerID, capabilities, true)
}

// SyncContextCapabilities adopts metadata already present in the provider's
// cache. Status samplers may call it while idle: it performs no network call
// and never waits for an in-flight compaction to finish.
func (l *Loop) SyncContextCapabilities() {
	_ = l.syncCachedContextCapabilities(context.Background(), false)
}

func (l *Loop) syncCachedContextCapabilities(ctx context.Context, requestBoundary bool) error {
	if l == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.RLock()
	provider, model, revision := l.Provider, l.Model, l.routingRevision
	providerID := identifyContextProvider(provider)
	l.mu.RUnlock()
	if provider != nil {
		return l.installContextCapabilities(ctx, model, revision, providerID, providerContextCapabilities(provider), requestBoundary)
	}
	return nil
}

// refreshCapabilitiesForRequest allows a brief status-sampler critical section
// or an owned manual compaction to finish without sending an obsolete request.
// Retry reads cached metadata only: discovery is attempted once per boundary.
// Every wait is tied to this request's context and owns no background goroutine.
func (l *Loop) refreshCapabilitiesForRequest(ctx context.Context) error {
	err := l.RefreshCapabilities(ctx)
	for errors.Is(err, ErrContextCapabilitiesBusy) {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		err = l.syncCachedContextCapabilities(ctx, true)
	}
	return err
}

func (l *Loop) installContextCapabilities(ctx context.Context, model string, revision uint64, providerID contextProviderIdentity, capabilities llm.ContextCapabilities, requestBoundary bool) error {
	if capabilities.Window <= 0 && capabilities.BudgetSource == "" && capabilities.RouteKey == "" {
		return nil
	}
	l.mu.RLock()
	if l.routingRevision != revision || l.Model != model || identifyContextProvider(l.Provider) != providerID {
		l.mu.RUnlock()
		return nil
	}
	outputBudget := 0
	if l.Compactor != nil {
		outputBudget = l.Compactor.MaxOutputTokens
	}
	if err := validateContextCapabilitiesBudget(capabilities, outputBudget); err != nil {
		l.mu.RUnlock()
		return err
	}
	bindingChanged := l.contextCapabilitiesBindingChangedLocked(capabilities)
	unchanged := !bindingChanged && l.contextCapabilities == capabilities
	l.mu.RUnlock()
	if unchanged {
		return nil
	}
	if !l.compactMu.TryLock() {
		if requestBoundary && bindingChanged {
			return ErrContextCapabilitiesBusy
		}
		return nil
	}
	defer l.finishCompactorCriticalSection()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.routingRevision != revision || l.Model != model || identifyContextProvider(l.Provider) != providerID {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	outputBudget = 0
	if l.Compactor != nil {
		outputBudget = l.Compactor.MaxOutputTokens
	}
	if err := validateContextCapabilitiesBudget(capabilities, outputBudget); err != nil {
		return err
	}
	bindingChanged = l.contextCapabilitiesBindingChangedLocked(capabilities)
	l.contextCapabilities = capabilities
	if !bindingChanged {
		return nil
	}
	if old := l.Compactor; old != nil && (old.MaxContextTokens != capabilities.Window || old.MaxInputTokens != capabilities.MaxInputTokens || old.WindowKind != capabilities.WindowKind) {
		next := *old
		next.ProtectedTools = append([]string(nil), old.ProtectedTools...)
		next.MaxContextTokens = capabilities.Window
		next.WindowKind = capabilities.WindowKind
		next.MaxInputTokens = capabilities.MaxInputTokens
		next.MaxOutputTokens = outputBudget
		next.ApplyWindowTier(next.effectiveInputCap())
		l.Compactor = &next
	}
	l.ContextWindow = capabilities.Window
	l.routingRevision++
	l.invalidateActiveContextLocked()
	l.estTokens.Store(int64(estimateActiveHistoryTokens(l.Messages)) + l.requestOverheadTokens.Load())
	return nil
}

func validateContextCapabilitiesBudget(capabilities llm.ContextCapabilities, outputBudget int) error {
	if capabilities.MaxOutputTokens > 0 && outputBudget > capabilities.MaxOutputTokens {
		return fmt.Errorf("%w: configured output budget (%d) exceeds advertised output limit (%d)", ErrContextCapabilitiesBudget, outputBudget, capabilities.MaxOutputTokens)
	}
	if capabilities.Window > 0 && capabilities.WindowKind != llm.ContextWindowInput && outputBudget >= capabilities.Window {
		return fmt.Errorf("%w: configured output budget (%d) must be smaller than context window (%d)", ErrContextCapabilitiesBudget, outputBudget, capabilities.Window)
	}
	return nil
}

func (l *Loop) contextCapabilitiesBindingChangedLocked(capabilities llm.ContextCapabilities) bool {
	return l.ContextWindow != capabilities.Window ||
		l.contextCapabilities.WindowKind != capabilities.WindowKind ||
		l.contextCapabilities.MaxInputTokens != capabilities.MaxInputTokens ||
		l.contextCapabilities.MaxOutputTokens != capabilities.MaxOutputTokens ||
		l.contextCapabilities.MaxWindow != capabilities.MaxWindow ||
		l.contextCapabilities.RouteKey != capabilities.RouteKey ||
		(l.Compactor != nil && (l.Compactor.MaxContextTokens != capabilities.Window || l.Compactor.MaxInputTokens != capabilities.MaxInputTokens || l.Compactor.WindowKind != capabilities.WindowKind))
}

// ContextMetadataSnapshot returns only the capability version already bound
// to this loop. It performs no refresh/network call and never waits for a
// compaction. Uninstalled provider metadata cannot change the displayed cap.
func (l *Loop) ContextMetadataSnapshot() llm.ContextCapabilities {
	capabilities, _, _ := l.ContextDetailsSnapshot()
	return capabilities
}

// ContextDetailsSnapshot reads the installed capability tuple and matching
// compaction threshold in one critical section. Metadata and the displayed
// window can therefore never come from different provider-cache versions.
func (l *Loop) ContextDetailsSnapshot() (capabilities llm.ContextCapabilities, threshold float64, trigger int) {
	if l == nil {
		return llm.ContextCapabilities{}, 0, 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	capabilities = l.contextCapabilities
	if capabilities.Window != l.ContextWindow {
		// Direct embedders can set the initial ContextWindow before assembly.
		// Until the first refresh, its provenance and other limits are unknown.
		capabilities = llm.ContextCapabilities{Source: "provider_fallback"}
	}
	capabilities.Window = l.ContextWindow
	if l.Compactor != nil {
		threshold = l.Compactor.Config.Threshold
		trigger = l.Compactor.TriggerTokens()
	}
	return capabilities, threshold, trigger
}
