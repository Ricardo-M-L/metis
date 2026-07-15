package runtime

import (
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// RebindLoopRuntime refreshes provider/session state captured by built-in
// tools and by the lazy pricing resolver after an in-process /model, /resume,
// /branch or /new boundary. The existing registry stays intact so dynamic MCP
// and plugin tools are not discarded.
func RebindLoopRuntime(loop *agent.Loop, provider llm.Provider, model, system, parentSessionID string) {
	if loop == nil {
		return
	}
	builtin.RebindProviderTools(loop.Registry, provider, model, system, parentSessionID)
	if loop.Budget != nil {
		loop.Budget.SetRatesResolver(modelRatesResolver(model))
	}
}

func modelRatesResolver(model string) func() (budget.Rates, bool) {
	return func() (budget.Rates, bool) {
		cli := catalog.Default()
		if cli == nil {
			return budget.Rates{}, false
		}
		cost, ok := cli.LookupCostByModelID(model)
		if !ok {
			// A cold catalogue should be retried; a warmed catalogue with no
			// matching price is a final zero-rate answer.
			return budget.Rates{}, cli.Stat().InMemory
		}
		return budget.Rates{
			InputPerMTok:      cost.Input,
			OutputPerMTok:     cost.Output,
			CacheReadPerMTok:  cost.CacheRead,
			CacheWritePerMTok: cost.CacheWrite,
		}, true
	}
}
