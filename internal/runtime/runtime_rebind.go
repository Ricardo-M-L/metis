package runtime

import (
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// RebindProviderPrompt replaces the provider-managed system-prompt section
// after an in-process model/provider switch. Explicit --system/simple prompts
// are represented as a single "base" section and deliberately remain
// untouched; only the section-aware default prompt is provider-managed.
//
// The returned string is rendered from the returned sections so transports
// that consume Loop.System and transports that consume Loop.SystemSections see
// the same provider guidance.
func RebindProviderPrompt(system string, sections []llm.SystemSection, providerName, model string) (string, []llm.SystemSection) {
	if len(sections) == 0 {
		return system, nil
	}

	out := append([]llm.SystemSection(nil), sections...)
	managed := false
	providerIndex := -1
	insertAt := 0
	for i, section := range out {
		if section.Name == "provider_hint" {
			managed = true
			if providerIndex < 0 {
				providerIndex = i
			}
		}
		if isManagedBasePromptSection(section.Name) {
			managed = true
			insertAt = i + 1
		}
	}
	if !managed {
		return system, out
	}

	// Remove every old provider hint before inserting the freshly-derived one.
	// This also repairs any accidentally duplicated hint sections.
	filtered := make([]llm.SystemSection, 0, len(out)+1)
	for _, section := range out {
		if section.Name != "provider_hint" {
			filtered = append(filtered, section)
		}
	}
	if providerIndex >= 0 {
		insertAt = min(providerIndex, len(filtered))
	} else {
		insertAt = min(insertAt, len(filtered))
	}
	if hint := ProviderHintFor(providerName, model); hint != "" {
		section := llm.SystemSection{Name: "provider_hint", Body: hint, Cache: true}
		// In-place splice: grow by one zero element, then copy() shifts
		// [insertAt:] right into [insertAt+1:]. The trailing zero never
		// lands in the destination (dst is one element shorter than src
		// after the append), so copy overwrites exactly the shifted
		// region and nothing else. Read before editing — this idiom is
		// correct but easy to break.
		filtered = append(filtered, llm.SystemSection{})
		copy(filtered[insertAt+1:], filtered[insertAt:])
		filtered[insertAt] = section
	}

	renderable := make([]SystemPromptSection, 0, len(filtered))
	for _, section := range filtered {
		renderable = append(renderable, SystemPromptSection{
			Name: section.Name, Body: section.Body, Cache: section.Cache, Volatile: section.Volatile,
		})
	}
	return RenderSections(renderable), filtered
}

func isManagedBasePromptSection(name string) bool {
	switch name {
	case "identity", "language", "computer_use", "privacy", "style", "tool_redirects",
		"working_efficiently", "skills", "reversibility", "interaction_modes":
		return true
	default:
		return false
	}
}

// RebindToolAwarePrompt refreshes only the default-prompt sections whose body
// depends on the model-visible tool registry. Explicit --system/simple prompts
// use a single "base" section and are deliberately left byte-for-byte intact.
// The remaining default sections (provider hint, overlays, project context,
// addendum and env) retain their original order and cache metadata.
func RebindToolAwarePrompt(system string, sections []SystemPromptSection, ctx PromptCtx) (string, []SystemPromptSection) {
	if len(sections) == 0 {
		return system, nil
	}
	managed := false
	for _, section := range sections {
		if isManagedBasePromptSection(section.Name) {
			managed = true
			break
		}
	}
	if !managed {
		return system, append([]SystemPromptSection(nil), sections...)
	}

	computerUse := ComputerUseSection(ctx)
	toolRedirects := ToolRedirectsSection(ctx)
	out := make([]SystemPromptSection, 0, len(sections)+2)
	for _, section := range sections {
		switch section.Name {
		case "computer_use", "tool_redirects":
			// Drop the provisional copy. The canonical replacement is inserted
			// below even when the provisional section was absent.
			continue
		}
		out = append(out, section)
		if section.Name == "language" && computerUse.Name != "" {
			out = append(out, computerUse)
		}
		if section.Name == "style" && toolRedirects.Name != "" {
			out = append(out, toolRedirects)
		}
	}
	return RenderSections(out), out
}

// RebindLoopRuntime refreshes provider/session state captured by built-in
// tools and by the lazy pricing resolver after an in-process /model, /resume,
// /branch or /new boundary. The existing registry stays intact so dynamic MCP
// and plugin tools are not discarded.
type LoopRuntimeRebindOptions struct {
	ProviderName     string
	WorkingDirectory string
}

func RebindLoopRuntime(loop *agent.Loop, provider llm.Provider, model, system, parentSessionID string, options ...LoopRuntimeRebindOptions) {
	if loop == nil {
		return
	}
	promptState := builtin.AgentRuntimePromptState{}
	if len(options) > 0 {
		latest := options[len(options)-1]
		promptState.ProviderName = latest.ProviderName
		promptState.WorkingDirectory = latest.WorkingDirectory
	}
	builtin.RebindProviderTools(loop.Registry, provider, model, system, parentSessionID, promptState)
	if loop.Budget != nil {
		loop.Budget.SetRatesResolver(modelRatesResolver(model))
	}
	// Trace events must follow the session boundary too: without this,
	// web-UI session switches kept appending trajectory events to the
	// previously-active session's file.
	RebindTrace(parentSessionID)
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
