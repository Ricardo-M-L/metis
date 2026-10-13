package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrContextBudgetPolicy stops generation when a saved operating policy cannot
// be read safely. Unlike unavailable discovery, it must not silently retain an
// obsolete manual budget for the next request.
var ErrContextBudgetPolicy = errors.New("cannot refresh configured context budget")

// ContextBudgetPolicyOptions captures one immutable route/model binding. The
// lookup runs only at request boundaries; Current never touches disk/network.
type ContextBudgetPolicyOptions struct {
	Owner         ContextBudgetOwner
	LegacyWindow  int
	InitialWindow int
	InitialSource string
	Lookup        func(context.Context) (int, bool, error)
}

// ContextBudgetOwner is immutable and contains only public ids and a digest.
// It lets UI consumers reject metadata sampled from another provider/model.
type ContextBudgetOwner struct {
	ProviderID string
	Model      string
	RouteKey   string
}

type contextBudgetSelection struct {
	window int
	source string
}

// ContextBudgetPolicy publishes a coherent operating-budget tuple for all
// readers of a shared provider. Refresh never mutates provider.ContextWindow,
// so status reads and parallel tasks do not race with a settings update.
type ContextBudgetPolicy struct {
	owner        ContextBudgetOwner
	legacyWindow int
	lookup       func(context.Context) (int, bool, error)
	refreshMu    sync.Mutex
	selection    atomic.Pointer[contextBudgetSelection]
}

func NewContextBudgetPolicy(opts ContextBudgetPolicyOptions) *ContextBudgetPolicy {
	p := &ContextBudgetPolicy{owner: opts.Owner, legacyWindow: opts.LegacyWindow, lookup: opts.Lookup}
	source := opts.InitialSource
	if source == "" {
		source = ContextBudgetAutomatic
		if opts.LegacyWindow > 0 {
			source = ContextBudgetLegacyProvider
		}
	}
	p.selection.Store(&contextBudgetSelection{window: opts.InitialWindow, source: source})
	return p
}

// Apply stamps immutable ownership only. The caller must annotate the budget
// source captured together with its operating window, so concurrent Refresh
// cannot mix a new source with an older window while a capability is resolved.
func (p *ContextBudgetPolicy) Apply(capabilities ContextCapabilities) ContextCapabilities {
	if p != nil {
		capabilities.ProviderID = p.owner.ProviderID
		capabilities.Model = p.owner.Model
		capabilities.RouteKey = p.owner.RouteKey
	}
	return capabilities
}

func (p *ContextBudgetPolicy) Current() (int, string) {
	if p == nil {
		return 0, ContextBudgetAutomatic
	}
	selection := p.selection.Load()
	if selection == nil {
		return 0, ContextBudgetAutomatic
	}
	return selection.window, selection.source
}

// CurrentContextBudget preserves direct constructor/legacy-provider behavior
// when no exact route/model policy has been attached by runtime construction.
func CurrentContextBudget(policy *ContextBudgetPolicy, window int, source string) (int, string) {
	if policy == nil {
		return window, source
	}
	return policy.Current()
}

// UnknownModelContextBudget exposes only an operator's operating budget for
// cloud routes without model-capacity discovery. Unknown automatic limits stay
// zero; no model/default/input/output capacity is manufactured from a budget.
func UnknownModelContextBudget(policy *ContextBudgetPolicy, legacyWindow int) ContextCapabilities {
	source := ContextBudgetAutomatic
	if legacyWindow > 0 {
		source = ContextBudgetLegacyProvider
	}
	window, budgetSource := CurrentContextBudget(policy, legacyWindow, source)
	out := ContextCapabilities{Window: max(window, 0), Source: "unknown", BudgetSource: budgetSource}
	if window > 0 {
		out.WindowKind = ContextWindowTotal
		out.RequestedWindow, out.Source = window, "configured"
	}
	return policy.Apply(out)
}

// Refresh rejects a failed/cancelled lookup without publishing partial state.
// Missing records retain compatibility with the captured provider-wide value;
// an explicit zero record overrides that value and restores automatic sizing.
func (p *ContextBudgetPolicy) Refresh(ctx context.Context) error {
	if p == nil || p.lookup == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	window, found, err := p.lookup(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrContextBudgetPolicy, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if window < 0 {
		return fmt.Errorf("%w: negative saved budget", ErrContextBudgetPolicy)
	}
	source := ContextBudgetAutomatic
	if !found {
		window = p.legacyWindow
		if window > 0 {
			source = ContextBudgetLegacyProvider
		}
	} else if window > 0 {
		source = ContextBudgetPerModel
	}
	p.selection.Store(&contextBudgetSelection{window: window, source: source})
	return nil
}
