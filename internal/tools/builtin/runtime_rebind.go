package builtin

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// RebindProviderTools updates the built-ins that capture provider/session
// state at registry construction time. The registry itself is intentionally
// preserved: rebuilding it would discard dynamically registered MCP and
// plugin tools during an in-process /model or /resume operation.
func RebindProviderTools(reg *tools.Registry, provider llm.Provider, model, system, parentSessionID string) {
	if reg == nil {
		return
	}
	if tool, ok := reg.Get("Agent"); ok {
		switch current := tool.(type) {
		case Agent:
			current.provider = provider
			current.model = model
			current.system = system
			current.parentSessionID = parentSessionID
			reg.Replace(current)
		case *Agent:
			updated := *current
			updated.provider = provider
			updated.model = model
			updated.system = system
			updated.parentSessionID = parentSessionID
			reg.Replace(&updated)
		}
	}
	if tool, ok := reg.Get("Fork"); ok {
		switch current := tool.(type) {
		case Fork:
			current.provider = provider
			reg.Replace(current)
		case *Fork:
			updated := *current
			updated.provider = provider
			reg.Replace(&updated)
		}
	}
	if tool, ok := reg.Get("MetisInfo"); ok {
		switch current := tool.(type) {
		case MetisInfo:
			current.provider = provider
			current.model = model
			reg.Replace(current)
		case *MetisInfo:
			updated := *current
			updated.provider = provider
			updated.model = model
			reg.Replace(&updated)
		}
	}
}
