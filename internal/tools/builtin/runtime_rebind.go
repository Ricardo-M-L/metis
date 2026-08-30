package builtin

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// RebindProviderTools updates the built-ins that capture provider/session
// state at registry construction time. The registry itself is intentionally
// preserved: rebuilding it would discard dynamically registered MCP and
// plugin tools during an in-process /model or /resume operation. promptState
// is optional for source compatibility; when supplied it gives the Agent's
// minimal prompt builder the exact configured provider label and workspace.
func RebindProviderTools(reg *tools.Registry, provider llm.Provider, model, system, parentSessionID string, promptState ...AgentRuntimePromptState) {
	if reg == nil {
		return
	}
	if tool, ok := reg.Get("Agent"); ok {
		switch current := tool.(type) {
		case Agent:
			rebindAgentProvider(&current, reg, provider, model, system, parentSessionID, promptState)
			reg.Replace(current)
		case *Agent:
			updated := *current
			rebindAgentProvider(&updated, reg, provider, model, system, parentSessionID, promptState)
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

func rebindAgentProvider(a *Agent, reg *tools.Registry, provider llm.Provider, model, system, parentSessionID string, promptState []AgentRuntimePromptState) {
	a.provider = provider
	a.model = model
	a.system = system
	a.parentSessionID = parentSessionID

	// A full prompt assembled for the new provider/model is always a safe
	// compatibility fallback. Never retain a minimal prompt assembled for the
	// old runtime. When a builder is wired, replace the fallback with a freshly
	// assembled minimal prompt instead.
	a.minimalSystem = system
	a.promptProviderName = ""
	if provider != nil {
		a.promptProviderName = provider.Name()
	}
	applyAgentRuntimePromptState(a, promptState)
	if a.minimalPromptBuilder != nil {
		a.minimalSystem = a.minimalPromptFor(reg, a.promptWorkDir)
	}
}

func applyAgentRuntimePromptState(a *Agent, promptState []AgentRuntimePromptState) {
	if a == nil || len(promptState) == 0 {
		return
	}
	state := promptState[len(promptState)-1]
	if state.ProviderName != "" {
		a.promptProviderName = state.ProviderName
	}
	if state.WorkingDirectory != "" {
		a.promptWorkDir = state.WorkingDirectory
	}
	if state.MinimalPromptBuilder != nil {
		a.minimalPromptBuilder = state.MinimalPromptBuilder
	}
}

// RebindAgentPrompts updates the prompt pair captured by the cold Agent tool
// after the parent registry's final visibility policies have been applied.
// Without this second phase the parent Loop can correctly omit unavailable web
// routing while newly spawned sub-agents keep the provisional "all tools"
// guidance assembled before the registry existed.
func RebindAgentPrompts(reg *tools.Registry, system, minimalSystem string, promptState ...AgentRuntimePromptState) {
	if reg == nil {
		return
	}
	tool, ok := reg.Get("Agent")
	if !ok {
		return
	}
	switch current := tool.(type) {
	case Agent:
		current.system = system
		current.minimalSystem = minimalSystem
		applyAgentRuntimePromptState(&current, promptState)
		reg.Replace(current)
	case *Agent:
		updated := *current
		updated.system = system
		updated.minimalSystem = minimalSystem
		applyAgentRuntimePromptState(&updated, promptState)
		reg.Replace(&updated)
	}
}
