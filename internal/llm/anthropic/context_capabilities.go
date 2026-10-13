package anthropic

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// ContextCapabilities resolves cached route metadata independently of the
// Anthropic wire format. Authenticated endpoint discovery runs only through
// RefreshContextCapabilities, outside the provider's generation hot path.
func (a *Anthropic) ContextCapabilities() (resolved llm.ContextCapabilities) {
	window, budgetSource := llm.CurrentContextBudget(a.ContextBudgetPolicy, a.ContextWindow, a.ContextBudgetSource)
	defer func() { resolved = a.ContextBudgetPolicy.Apply(llm.ApplyContextBudgetSource(resolved, budgetSource)) }()
	if a.ContextResolver != nil {
		return a.ContextResolver.Resolve(window, a.fallbackContextWindow)
	}
	return llm.ResolveContextCapabilities(window, a.CatalogProvider, a.Model, a.CatalogProviderStrict, a.fallbackContextWindow)
}

func (a *Anthropic) RefreshContextCapabilities(ctx context.Context) error {
	if err := a.ContextBudgetPolicy.Refresh(ctx); err != nil {
		return err
	}
	if a.ContextResolver == nil {
		return nil
	}
	return a.ContextResolver.Refresh(ctx)
}

var _ llm.ContextCapabilitiesProvider = (*Anthropic)(nil)
