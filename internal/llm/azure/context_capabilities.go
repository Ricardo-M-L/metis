package azure

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func (a *Azure) ContextCapabilities() llm.ContextCapabilities {
	return llm.UnknownModelContextBudget(a.ContextBudgetPolicy, a.ContextWindow)
}

func (a *Azure) RefreshContextCapabilities(ctx context.Context) error {
	return a.ContextBudgetPolicy.Refresh(ctx)
}

var _ llm.ContextCapabilitiesProvider = (*Azure)(nil)
