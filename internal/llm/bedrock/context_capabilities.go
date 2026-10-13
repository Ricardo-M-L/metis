package bedrock

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func (b *Bedrock) ContextCapabilities() llm.ContextCapabilities {
	return llm.UnknownModelContextBudget(b.ContextBudgetPolicy, b.ContextWindow)
}

func (b *Bedrock) RefreshContextCapabilities(ctx context.Context) error {
	return b.ContextBudgetPolicy.Refresh(ctx)
}

var _ llm.ContextCapabilitiesProvider = (*Bedrock)(nil)
