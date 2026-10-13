package openai

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func (o *OpenAI) ContextCapabilities() (resolved llm.ContextCapabilities) {
	window, budgetSource := llm.CurrentContextBudget(o.ContextBudgetPolicy, o.ContextWindow, o.ContextBudgetSource)
	defer func() { resolved = o.ContextBudgetPolicy.Apply(llm.ApplyContextBudgetSource(resolved, budgetSource)) }()
	if o.ContextResolver != nil {
		return o.ContextResolver.Resolve(window, o.fallbackContextWindow)
	}
	return llm.ResolveContextCapabilities(window, o.CatalogProvider, o.Model, o.CatalogProviderStrict, o.fallbackContextWindow)
}

func (o *OpenAI) RefreshContextCapabilities(ctx context.Context) error {
	if err := o.ContextBudgetPolicy.Refresh(ctx); err != nil {
		return err
	}
	if o.ContextResolver == nil {
		return nil
	}
	return o.ContextResolver.Refresh(ctx)
}

func (r *Responses) ContextCapabilities() (resolved llm.ContextCapabilities) {
	defer func() { resolved = r.ContextBudgetPolicy.Apply(resolved) }()
	configuredWindow, budgetSource := r.ContextWindow, r.ContextBudgetSource
	if r.ContextBudgetPolicy != nil {
		configuredWindow, budgetSource = r.ContextBudgetPolicy.Current()
	}
	// Subscription limits are a separate contract from public API limits.
	// Keep this boundary even if a generic resolver is attached accidentally.
	if r.Name() == "openai-codex" {
		if r.CodexContextResolver != nil {
			return llm.ApplyContextBudgetSource(r.CodexContextResolver.Resolve(r.Model, configuredWindow, r.fallbackContextWindow), budgetSource)
		}
		window, found := CodexModelContextWindow(r.Model)
		source := "subscription_model"
		if !found {
			window, source = r.fallbackContextWindow(), "fallback"
		}
		out := llm.ContextCapabilities{Window: window, DefaultWindow: window, Source: source}
		if configuredWindow > 0 {
			out.Window, out.Source = configuredWindow, "configured"
		}
		return llm.ApplyContextBudgetSource(out, budgetSource)
	}
	if r.ContextResolver != nil {
		return llm.ApplyContextBudgetSource(r.ContextResolver.Resolve(configuredWindow, r.fallbackContextWindow), budgetSource)
	}
	return llm.ApplyContextBudgetSource(llm.ResolveContextCapabilities(configuredWindow, r.CatalogProvider, r.Model, r.CatalogProviderStrict, r.fallbackContextWindow), budgetSource)
}

func (r *Responses) RefreshContextCapabilities(ctx context.Context) error {
	if r.ContextBudgetPolicy != nil {
		if err := r.ContextBudgetPolicy.Refresh(ctx); err != nil {
			return err
		}
	}
	if r.Name() == "openai-codex" {
		if r.CodexContextResolver != nil {
			return r.CodexContextResolver.Refresh(ctx)
		}
		return nil
	}
	if r.ContextResolver == nil {
		return nil
	}
	return r.ContextResolver.Refresh(ctx)
}

var (
	_ llm.ContextCapabilitiesProvider = (*OpenAI)(nil)
	_ llm.ContextCapabilitiesProvider = (*Responses)(nil)
)
