package config

import "errors"

var ErrProviderSettingsConflict = errors.New("provider settings changed; reload before saving")

type ProviderModelSettings struct {
	ID                string            `json:"id"`
	Name              string            `json:"name,omitempty"`
	ContextWindow     int               `json:"contextWindow"`
	MaxOutputTokens   int               `json:"maxOutputTokens"`
	OutputBudget      int               `json:"outputBudget"`
	SupportsVision    *bool             `json:"supportsVision,omitempty"`
	SupportsReasoning *bool             `json:"supportsReasoning,omitempty"`
	ReasoningEfforts  map[string]string `json:"reasoningEfforts,omitempty"`
}

type ProviderSettingsProvider struct {
	ID               string                  `json:"id"`
	Kind             string                  `json:"kind"`
	DisplayName      string                  `json:"displayName"`
	CatalogProvider  string                  `json:"catalogProvider"`
	Transport        string                  `json:"transport"`
	BaseURL          string                  `json:"baseURL"`
	Model            string                  `json:"model"`
	Models           []ProviderModelSettings `json:"models"`
	CredentialSource string                  `json:"credentialSource"`
	HasCredential    bool                    `json:"hasCredential"`
	ReadOnly         bool                    `json:"readOnly"`
	ReadOnlyReason   string                  `json:"readOnlyReason,omitempty"`
	Registered       bool                    `json:"registered"`
	Removable        bool                    `json:"removable"`
	RemoveReason     string                  `json:"removeReason,omitempty"`
}

type ProviderSettingsSnapshot struct {
	Revision        string                     `json:"revision"`
	DefaultProvider string                     `json:"defaultProvider"`
	DefaultModel    string                     `json:"defaultModel"`
	Providers       []ProviderSettingsProvider `json:"providers"`
}

type ProviderSettingsMutation struct {
	Action           string                    `json:"action"`
	ExpectedRevision string                    `json:"expectedRevision"`
	ID               string                    `json:"id,omitempty"`
	Model            string                    `json:"model,omitempty"`
	Provider         *ProviderSettingsProvider `json:"provider,omitempty"`
	APIKey           string                    `json:"apiKey,omitempty"`
	ClearCredential  bool                      `json:"clearCredential,omitempty"`
	MakeDefault      bool                      `json:"makeDefault,omitempty"`
}

var ErrProviderSettingsReadOnly = errors.New("provider settings are read-only or conflict with the active configuration")
var ErrProviderSettingsCredentialSource = errors.New("credential is controlled by environment or inline configuration")
