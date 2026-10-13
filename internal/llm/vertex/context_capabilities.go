package vertex

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func (v *Vertex) ContextCapabilities() llm.ContextCapabilities {
	return llm.UnknownModelContextBudget(v.ContextBudgetPolicy, v.ContextWindow)
}

func (v *Vertex) RefreshContextCapabilities(ctx context.Context) error {
	return v.ContextBudgetPolicy.Refresh(ctx)
}

var _ llm.ContextCapabilitiesProvider = (*Vertex)(nil)
