package llm

import "context"

// ContextCapabilities separates the working budget, serving-route limits,
// client recommendations and independently known complete model capacity.
// Zero means that a capacity was not reported; input and output limits must
// never be added together to manufacture a total context window.
type ContextCapabilities struct {
	ProviderID           string `json:"providerId,omitempty"`
	Model                string `json:"model,omitempty"`
	RouteKey             string `json:"routeKey,omitempty"`
	Window               int    `json:"window"`
	WindowKind           string `json:"windowKind,omitempty"`
	DefaultWindow        int    `json:"defaultWindow,omitempty"`
	ProfileDefaultWindow int    `json:"profileDefaultWindow,omitempty"`
	ProfileMaxWindow     int    `json:"profileMaxWindow,omitempty"`
	MaxWindow            int    `json:"maxWindow,omitempty"`
	MaxInputTokens       int    `json:"maxInputTokens,omitempty"`
	MaxOutputTokens      int    `json:"maxOutputTokens,omitempty"`
	Source               string `json:"source,omitempty"`
	ModelWindow          int    `json:"modelWindow,omitempty"`
	ModelMaxInputTokens  int    `json:"modelMaxInputTokens,omitempty"`
	ModelMaxOutputTokens int    `json:"modelMaxOutputTokens,omitempty"`
	ModelSource          string `json:"modelSource,omitempty"`
	ModelOutputSource    string `json:"modelOutputSource,omitempty"`
	RequestedWindow      int    `json:"requestedWindow,omitempty"`
	LimitSource          string `json:"limitSource,omitempty"`
	BudgetSource         string `json:"budgetSource,omitempty"`
}

const (
	ContextWindowTotal          = "total"
	ContextWindowInput          = "input"
	ContextBudgetAutomatic      = "automatic"
	ContextBudgetPerModel       = "per_model"
	ContextBudgetLegacyProvider = "legacy_provider"
)

// ApplyContextBudgetSource annotates the budget's configuration origin without
// overwriting capability provenance or hiding a clamped manual request.
func ApplyContextBudgetSource(capabilities ContextCapabilities, source string) ContextCapabilities {
	if source != "" {
		capabilities.BudgetSource = source
	}
	return capabilities
}

type ContextCapabilitiesProvider interface {
	ContextCapabilities() ContextCapabilities
	RefreshContextCapabilities(context.Context) error
}
