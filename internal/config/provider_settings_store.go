package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/auth"
)

const providerSettingsFilename = "provider-settings.json"
const providerSettingsMaxBytes = 4 << 20

type storedProviderSettings struct {
	Binding         auth.EndpointBinding    `json:"binding"`
	DisplayName     string                  `json:"displayName"`
	CatalogProvider string                  `json:"catalogProvider"`
	Models          []ProviderModelSettings `json:"models"`
}
type providerSettingsFile struct {
	Schema    int                               `json:"schema"`
	Providers map[string]storedProviderSettings `json:"providers"`
}
type providerSettingsState struct {
	configBytes, sidecarBytes   []byte
	configExists, sidecarExists bool
	cfg                         *Config
	metadata                    toml.MetaData
	sidecar                     providerSettingsFile
}

// Test seams inject failures at each publication without weakening production
// validation. Rollback deliberately uses the real atomic writer.
var providerSettingsWriteFile = atomicWritePrivateFile
var providerSettingsCredentialTransaction = auth.WithProviderSettingsCredentials

func ReadProviderSettings() (ProviderSettingsSnapshot, error) {
	return ReadProviderSettingsForWorkspace(true)
}

// ReadProviderSettingsForWorkspace keeps user-owned registrations separate
// from untrusted project overlays. The trust projection never changes CAS.
func ReadProviderSettingsForWorkspace(projectTrusted bool) (ProviderSettingsSnapshot, error) {
	var snapshot ProviderSettingsSnapshot
	err := withUserConfigWriteLock(func(path string) error {
		return providerSettingsCredentialTransaction(func(credentials *auth.ProviderSettingsCredentials) error {
			state, err := readProviderSettingsState(path)
			if err != nil {
				return err
			}
			snapshot, err = providerSettingsSnapshotForWorkspace(state, credentials, projectTrusted)
			return err
		})
	})
	return snapshot, err
}

func readProviderSettingsBytes(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > providerSettingsMaxBytes {
		return nil, false, errors.New("provider settings must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, false, errors.New("provider settings changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, providerSettingsMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > providerSettingsMaxBytes {
		return nil, false, errors.New("provider settings file is too large")
	}
	return data, true, nil
}

func readProviderSettingsState(path string) (providerSettingsState, error) {
	var state providerSettingsState
	var err error
	state.configBytes, state.configExists, err = readProviderSettingsBytes(path)
	if err != nil {
		return state, fmt.Errorf("read user provider configuration: %w", err)
	}
	state.cfg = defaults()
	state.metadata, err = toml.Decode(string(state.configBytes), state.cfg)
	if err != nil {
		return state, errors.New("user provider configuration contains invalid TOML")
	}
	state.sidecarBytes, state.sidecarExists, err = readProviderSettingsBytes(filepath.Join(filepath.Dir(path), providerSettingsFilename))
	if err != nil {
		return state, fmt.Errorf("read saved provider settings: %w", err)
	}
	state.sidecar = providerSettingsFile{Schema: 1, Providers: map[string]storedProviderSettings{}}
	if len(state.sidecarBytes) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(state.sidecarBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&state.sidecar); err != nil {
			return state, errors.New("saved provider settings contain invalid JSON")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return state, errors.New("saved provider settings contain trailing JSON")
		}
		if state.sidecar.Schema != 1 || state.sidecar.Providers == nil {
			return state, errors.New("unsupported provider settings format")
		}
		if err := validateStoredProviderSettings(state.sidecar); err != nil {
			return state, err
		}
	}
	return state, nil
}

func canonicalSettingsBinding(id, transport, endpoint string) (auth.EndpointBinding, error) {
	if id == "openai-codex" && transport == "openai_codex_responses" {
		transport = "openai_responses"
	}
	if endpoint == "" {
		switch id {
		case "anthropic":
			endpoint = "https://api.anthropic.com"
		case "openai":
			endpoint = "https://api.openai.com/v1"
		case "gemini":
			endpoint = "https://generativelanguage.googleapis.com"
		case "openai-codex":
			endpoint = "https://chatgpt.com/backend-api/codex"
		}
	}
	if transport == "" {
		switch id {
		case "openai":
			transport = "openai_chat"
		case "gemini":
			transport = "gemini_native"
		case "openai-codex":
			transport = "openai_responses"
		default:
			transport = "anthropic_messages"
		}
	}
	return auth.NormalizeEndpointBinding(id, transport, endpoint)
}

func providerSettingsRaw(cfg *Config, id string) (ProviderSettingsProvider, string, string, bool) {
	p := ProviderSettingsProvider{ID: id, Kind: "builtin", DisplayName: id, Models: []ProviderModelSettings{}}
	var env, inline string
	switch id {
	case "anthropic":
		r := cfg.Provider.Anthropic
		p.Transport, p.BaseURL, p.Model, p.CatalogProvider = "anthropic_messages", r.BaseURL, r.Model, r.CatalogProvider
		env, inline = r.APIKeyEnv, r.APIKey
	case "openai":
		r := cfg.Provider.OpenAI
		p.Transport = "openai_chat"
		if strings.EqualFold(strings.TrimSpace(r.WireProtocol), "responses") {
			p.Transport = "openai_responses"
		}
		p.BaseURL, p.Model, p.CatalogProvider = r.BaseURL, r.Model, r.CatalogProvider
		env, inline = r.APIKeyEnv, r.APIKey
	case "gemini":
		r := cfg.Provider.Gemini
		p.Transport, p.BaseURL, p.Model, p.CatalogProvider = "gemini_native", r.BaseURL, r.Model, r.CatalogProvider
		env, inline = r.APIKeyEnv, r.APIKey
		if (env == "" || os.Getenv(env) == "") && os.Getenv("GOOGLE_API_KEY") != "" {
			env = "GOOGLE_API_KEY"
		}
	case "openai-codex":
		p.Kind = "oauth"
		p.Transport, p.Model = "openai_responses", cfg.Provider.OpenAICodex.Model
	default:
		r, ok := cfg.Provider.Custom[id]
		if !ok {
			return p, "", "", false
		}
		p.Kind = "custom"
		p.Transport, p.BaseURL, p.Model, p.CatalogProvider = r.Transport, r.BaseURL, r.Model, r.CatalogProvider
		env, inline = r.APIKeyEnv, r.APIKey
	}
	binding, err := canonicalSettingsBinding(id, p.Transport, p.BaseURL)
	if err != nil {
		p.ReadOnly, p.ReadOnlyReason = true, "This provider route is not supported by API-key settings"
	} else {
		p.Transport, p.BaseURL = binding.Transport, binding.BaseURL
	}
	if p.Kind == "custom" {
		switch p.Transport {
		case "anthropic_messages", "openai_chat", "openai_responses", "gemini_native":
		default:
			p.ReadOnly, p.ReadOnlyReason = true, "This provider uses cloud credentials and must be edited in its configuration"
		}
	}
	return p, env, inline, true
}

func providerSettingsSnapshot(state providerSettingsState, credentials *auth.ProviderSettingsCredentials) (ProviderSettingsSnapshot, error) {
	return providerSettingsSnapshotForWorkspace(state, credentials, true)
}

func providerSettingsSnapshotForWorkspace(state providerSettingsState, credentials *auth.ProviderSettingsCredentials, projectTrusted bool) (ProviderSettingsSnapshot, error) {
	snapshot := ProviderSettingsSnapshot{DefaultProvider: state.cfg.Provider.Default, Providers: []ProviderSettingsProvider{}}
	ids := []string{"anthropic", "openai", "gemini", "openai-codex"}
	custom := make([]string, 0, len(state.cfg.Provider.Custom))
	for id := range state.cfg.Provider.Custom {
		custom = append(custom, id)
	}
	sort.Strings(custom)
	ids = append(ids, custom...)
	for _, id := range ids {
		p, env, inline, _ := providerSettingsRaw(state.cfg, id)
		p.Registered = p.Kind == "custom" || credentials.HasSavedProvider(id) || settingsConnectionDiffersFromDefaults(state.cfg, id, inline)
		binding, bindErr := canonicalSettingsBinding(id, p.Transport, p.BaseURL)
		if saved, ok := state.sidecar.Providers[id]; ok && bindErr == nil && saved.Binding == binding {
			p.DisplayName, p.Models = saved.DisplayName, cloneSettingsModels(saved.Models)
			p.Registered = true
		}
		if p.DisplayName == "" {
			p.DisplayName = id
		}
		modelListed := false
		for _, entry := range p.Models {
			modelListed = modelListed || entry.ID == p.Model
		}
		if !modelListed && p.Model != "" {
			p.Models = append(p.Models, ProviderModelSettings{ID: p.Model, Name: p.Model})
		}
		p.CredentialSource = "none"
		if env != "" && os.Getenv(env) != "" {
			p.CredentialSource, p.HasCredential = "env", true
			p.Registered = true
		} else if entry, ok := credentials.ManagedAPIKey(id); ok && entry.Key != "" {
			p.CredentialSource = "managed"
			if entry.Endpoint != nil && bindErr == nil {
				storedBinding, err := auth.NormalizeEndpointBinding(entry.Endpoint.Provider, entry.Endpoint.Transport, entry.Endpoint.BaseURL)
				p.HasCredential = err == nil && storedBinding == binding
			}
			if entry.Endpoint == nil && isOfficialSettingsBinding(binding) {
				p.HasCredential = true
			}
		} else if inline != "" {
			p.CredentialSource, p.HasCredential = "inline", true
		} else if credentials.HasOAuth(id) {
			p.CredentialSource, p.HasCredential = "oauth", true
		}
		semantic := []string{"provider", id}
		if p.Kind == "custom" {
			semantic = []string{"provider", "custom", id}
		}
		if projectTrusted {
			source, err := providerOverrideSource(semantic)
			if err != nil {
				return snapshot, err
			}
			if source != "" {
				p.Registered = true
				p.ReadOnly, p.ReadOnlyReason = true, "Controlled by "+source
			}
		}
		p.Removable = p.Registered && !p.ReadOnly
		switch {
		case !p.Registered:
			p.RemoveReason = "This provider has not been added"
		case p.ReadOnly:
			p.RemoveReason = p.ReadOnlyReason
		case p.Kind != "custom" && settingsBuiltinEnvironmentPreventsRemoval(state.cfg, id):
			p.Removable = false
			p.RemoveReason = "Remove the active environment credential before deleting this provider"
		default:
			if _, err := renderSettingsRemoveProvider(state.configBytes, p.ID, p.Kind); err != nil {
				p.Removable = false
				p.RemoveReason = "Use a simple provider table without child tables before deleting this provider"
			}
		}
		if id == snapshot.DefaultProvider {
			snapshot.DefaultModel = p.Model
		}
		snapshot.Providers = append(snapshot.Providers, p)
	}
	snapshot.Revision = providerSettingsRevision(state, credentials)
	return snapshot, nil
}

// Removing a user table reinstates CLI defaults. Check both the current env
// binding and the default binding that removal would expose, so a shadowed
// OPENAI_API_KEY (for example) cannot recreate the provider after deletion.
func settingsBuiltinEnvironmentPreventsRemoval(cfg *Config, id string) bool {
	_, current, _, _ := providerSettingsRaw(cfg, id)
	_, restored, _, _ := providerSettingsRaw(defaults(), id)
	return current != "" && os.Getenv(current) != "" || restored != "" && os.Getenv(restored) != ""
}

// The CLI starter file is a template, not a registration. Only meaningful
// connection changes count here; numeric generation preferences alone do not.
func settingsConnectionDiffersFromDefaults(cfg *Config, id, inline string) bool {
	if inline != "" {
		return true
	}
	current, env, _, ok := providerSettingsRaw(cfg, id)
	if !ok {
		return false
	}
	baseline, defaultEnv, _, _ := providerSettingsRaw(defaults(), id)
	if current.BaseURL != baseline.BaseURL || current.Transport != baseline.Transport {
		if _, err := canonicalSettingsBinding(id, current.Transport, current.BaseURL); err == nil {
			return true
		}
	}
	if current.Model != "" && current.Model != baseline.Model && validContextBudgetLabel(current.Model, 1024) && strings.IndexFunc(current.Model, unicode.IsSpace) < 0 {
		return true
	}
	if current.CatalogProvider != "" && current.CatalogProvider != baseline.CatalogProvider && validContextBudgetLabel(current.CatalogProvider, 128) && strings.IndexFunc(current.CatalogProvider, unicode.IsSpace) < 0 {
		return true
	}
	return env != "" && env != defaultEnv && validSingleLineTOMLString(env) && strings.IndexFunc(env, unicode.IsSpace) < 0
}

func providerSettingsRevision(state providerSettingsState, credentials *auth.ProviderSettingsCredentials) string {
	environment := map[string]string{}
	ids := []string{"anthropic", "openai", "gemini", "openai-codex"}
	for id := range state.cfg.Provider.Custom {
		ids = append(ids, id)
	}
	for _, id := range ids {
		_, env, _, _ := providerSettingsRaw(state.cfg, id)
		if env != "" && os.Getenv(env) != "" {
			environment[id] = os.Getenv(env)
		}
	}
	fingerprint, _ := json.Marshal(struct {
		Config, Sidecar             []byte
		ConfigExists, SidecarExists bool
		Credentials                 string
		Environment                 map[string]string
	}{state.configBytes, state.sidecarBytes, state.configExists, state.sidecarExists, credentials.Fingerprint(), environment})
	digest := sha256.Sum256(fingerprint)
	return hex.EncodeToString(digest[:])
}

func isOfficialSettingsBinding(binding auth.EndpointBinding) bool {
	official, err := canonicalSettingsBinding(binding.Provider, binding.Transport, "")
	return err == nil && official == binding && (binding.Provider == "openai" || binding.Provider == "anthropic" || binding.Provider == "gemini")
}

func cloneSettingsModels(models []ProviderModelSettings) []ProviderModelSettings {
	result := append([]ProviderModelSettings(nil), models...)
	for i := range result {
		if result[i].SupportsVision != nil {
			v := *result[i].SupportsVision
			result[i].SupportsVision = &v
		}
		if result[i].SupportsReasoning != nil {
			v := *result[i].SupportsReasoning
			result[i].SupportsReasoning = &v
		}
		if result[i].ReasoningEfforts != nil {
			m := map[string]string{}
			for k, v := range result[i].ReasoningEfforts {
				m[k] = v
			}
			result[i].ReasoningEfforts = m
		}
	}
	return result
}

func LookupProviderModelSettings(provider, transport, baseURL, model string) (ProviderModelSettings, bool, error) {
	binding, err := canonicalSettingsBinding(provider, transport, baseURL)
	if err != nil {
		return ProviderModelSettings{}, false, err
	}
	home, err := auth.ResolveCredentialHome("")
	if err != nil {
		return ProviderModelSettings{}, false, err
	}
	raw, _, err := readProviderSettingsBytes(filepath.Join(home, providerSettingsFilename))
	if err != nil {
		return ProviderModelSettings{}, false, err
	}
	if len(raw) == 0 {
		return ProviderModelSettings{}, false, nil
	}
	var file providerSettingsFile
	if err := json.Unmarshal(raw, &file); err != nil || file.Schema != 1 {
		return ProviderModelSettings{}, false, errors.New("invalid saved provider model settings")
	}
	if err := validateStoredProviderSettings(file); err != nil {
		return ProviderModelSettings{}, false, err
	}
	saved, ok := file.Providers[provider]
	if !ok || saved.Binding != binding {
		return ProviderModelSettings{}, false, nil
	}
	for _, entry := range saved.Models {
		if entry.ID == model {
			copy := cloneSettingsModels([]ProviderModelSettings{entry})
			return copy[0], true, nil
		}
	}
	return ProviderModelSettings{}, false, nil
}

func validateStoredProviderSettings(file providerSettingsFile) error {
	if file.Schema != 1 || file.Providers == nil {
		return errors.New("invalid saved provider settings schema")
	}
	for id, saved := range file.Providers {
		binding, err := canonicalSettingsBinding(id, saved.Binding.Transport, saved.Binding.BaseURL)
		if err != nil || saved.Binding != binding {
			return errors.New("saved provider settings contain an invalid route binding")
		}
		if len(saved.Models) == 0 {
			return errors.New("saved provider settings require a model list")
		}
		p := ProviderSettingsProvider{ID: id, Transport: binding.Transport, BaseURL: binding.BaseURL, DisplayName: saved.DisplayName, CatalogProvider: saved.CatalogProvider, Model: saved.Models[0].ID, Models: saved.Models}
		if _, err := validateSettingsProvider(p); err != nil {
			return fmt.Errorf("invalid saved provider model settings: %w", err)
		}
	}
	return nil
}

func validateSettingsProvider(p ProviderSettingsProvider) (ProviderSettingsProvider, error) {
	if err := validateProviderID(p.ID); err != nil {
		return p, err
	}
	if len(p.ID) > 128 {
		return p, errors.New("provider id is too long")
	}
	switch p.ID {
	case "anthropic", "openai", "gemini":
		p.Kind = "builtin"
	case "openai-codex":
		p.Kind = "oauth"
	default:
		p.Kind = "custom"
	}
	binding, err := canonicalSettingsBinding(p.ID, p.Transport, p.BaseURL)
	if err != nil {
		return p, err
	}
	p.Transport, p.BaseURL = binding.Transport, binding.BaseURL
	switch p.ID {
	case "anthropic":
		if p.Transport != "anthropic_messages" {
			return p, errors.New("Anthropic requires Messages transport")
		}
	case "gemini":
		if p.Transport != "gemini_native" {
			return p, errors.New("Gemini requires native transport")
		}
	case "openai":
		if p.Transport != "openai_chat" && p.Transport != "openai_responses" {
			return p, errors.New("OpenAI requires Chat or Responses transport")
		}
	case "openai-codex":
		expected, _ := canonicalSettingsBinding(p.ID, "openai_responses", "")
		if binding != expected {
			return p, fmt.Errorf("%w: Codex OAuth endpoint is fixed", ErrProviderSettingsReadOnly)
		}
	default:
		if err := validateCustomProviderSpec(CustomProviderSpec{ID: p.ID, Transport: p.Transport, BaseURL: p.BaseURL, Model: p.Model}); err != nil {
			return p, err
		}
	}
	if !validSingleLineTOMLString(p.DisplayName) || len(p.DisplayName) > 256 {
		return p, errors.New("provider display name must be bounded single-line text")
	}
	if p.DisplayName == "" {
		p.DisplayName = p.ID
	}
	if p.CatalogProvider != "" && (!validContextBudgetLabel(p.CatalogProvider, 128) || strings.IndexFunc(p.CatalogProvider, unicode.IsSpace) >= 0) {
		return p, errors.New("invalid catalog provider")
	}
	if !validContextBudgetLabel(p.Model, 1024) || strings.IndexFunc(p.Model, unicode.IsSpace) >= 0 {
		return p, errors.New("invalid provider model")
	}
	if len(p.Models) == 0 || len(p.Models) > 256 {
		return p, errors.New("provider requires between 1 and 256 saved models")
	}
	seen := map[string]bool{}
	found := false
	for _, model := range p.Models {
		if !validContextBudgetLabel(model.ID, 1024) || strings.IndexFunc(model.ID, unicode.IsSpace) >= 0 || seen[model.ID] {
			return p, errors.New("saved model ids must be unique non-empty text without whitespace")
		}
		seen[model.ID] = true
		found = found || model.ID == p.Model
		if !validSingleLineTOMLString(model.Name) || len(model.Name) > 512 {
			return p, errors.New("invalid saved model name")
		}
		for _, n := range []int{model.ContextWindow, model.MaxOutputTokens, model.OutputBudget} {
			if n < 0 || n > maxContextBudget {
				return p, errors.New("saved model token limits must be between 0 and 100000000")
			}
		}
		if p.Kind == "oauth" && model.OutputBudget != 0 {
			return p, errors.New("Codex OAuth does not support a request output budget")
		}
		if len(model.ReasoningEfforts) > 0 && (model.SupportsReasoning == nil || !*model.SupportsReasoning) {
			return p, errors.New("reasoning effort mappings require supportsReasoning=true")
		}
		if p.Transport == "gemini_native" && ((model.SupportsReasoning != nil && *model.SupportsReasoning) || len(model.ReasoningEfforts) > 0) {
			return p, errors.New("Gemini reasoning overrides are not supported")
		}
		if p.Transport == "anthropic_messages" && len(model.ReasoningEfforts) > 0 {
			return p, errors.New("Anthropic does not use OpenAI reasoning effort mappings")
		}
		for key, value := range model.ReasoningEfforts {
			if key != "low" && key != "medium" && key != "high" {
				return p, errors.New("reasoning effort keys must be low, medium, or high")
			}
			if len(value) == 0 || len(value) > 64 {
				return p, errors.New("reasoning wire effort must be a short token")
			}
			for _, r := range value {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
					return p, errors.New("reasoning wire effort must be an ASCII token")
				}
			}
		}
		if model.ContextWindow > 0 && model.MaxOutputTokens > model.ContextWindow {
			return p, errors.New("model maximum output exceeds its complete context capacity")
		}
		if model.MaxOutputTokens > 0 && model.OutputBudget > model.MaxOutputTokens {
			return p, errors.New("model output budget exceeds its declared maximum output")
		}
	}
	if !found {
		return p, fmt.Errorf("%w: the saved default model must remain in the model list", ErrProviderSettingsReadOnly)
	}
	return p, nil
}

func SaveProviderSettings(mutation ProviderSettingsMutation) (ProviderSettingsSnapshot, error) {
	return SaveProviderSettingsForWorkspace(mutation, true)
}

// SaveProviderSettingsForWorkspace uses the same project trust policy as its
// read projection. The transaction always edits user-owned files only.
func SaveProviderSettingsForWorkspace(mutation ProviderSettingsMutation, projectTrusted bool) (ProviderSettingsSnapshot, error) {
	var snapshot ProviderSettingsSnapshot
	err := withUserConfigWriteLock(func(path string) error {
		var state providerSettingsState
		var originalConfig, originalSidecar []byte
		var configExisted, sidecarExisted bool
		var credentialView *auth.ProviderSettingsCredentials
		published := false
		err := providerSettingsCredentialTransaction(func(credentials *auth.ProviderSettingsCredentials) error {
			var err error
			state, err = readProviderSettingsState(path)
			if err != nil {
				return err
			}
			originalConfig, originalSidecar = append([]byte(nil), state.configBytes...), append([]byte(nil), state.sidecarBytes...)
			configExisted, sidecarExisted = state.configExists, state.sidecarExists
			credentialView = credentials
			current, err := providerSettingsSnapshotForWorkspace(state, credentials, projectTrusted)
			if err != nil {
				return err
			}
			if mutation.ExpectedRevision == "" || mutation.ExpectedRevision != current.Revision {
				return ErrProviderSettingsConflict
			}
			id := mutation.ID
			if mutation.Provider != nil {
				if id != "" && id != mutation.Provider.ID {
					return errors.New("provider id does not match payload")
				}
				id = mutation.Provider.ID
			}
			existing := (*ProviderSettingsProvider)(nil)
			for i := range current.Providers {
				if current.Providers[i].ID == id {
					existing = &current.Providers[i]
					break
				}
			}
			if existing != nil && existing.ReadOnly {
				return fmt.Errorf("%w: %s", ErrProviderSettingsReadOnly, existing.ReadOnlyReason)
			}
			updated := append([]byte(nil), state.configBytes...)
			switch mutation.Action {
			case "upsert":
				if mutation.Provider == nil {
					return errors.New("provider payload is required")
				}
				p, err := validateSettingsProvider(*mutation.Provider)
				if err != nil {
					return err
				}
				semantic := []string{"provider", id}
				if p.Kind == "custom" {
					semantic = []string{"provider", "custom", id}
				}
				if projectTrusted {
					if source, err := providerOverrideSource(semantic); err != nil {
						return err
					} else if source != "" {
						return fmt.Errorf("%w: provider is controlled by %s", ErrProviderSettingsReadOnly, source)
					}
				}
				if mutation.ClearCredential && mutation.APIKey != "" {
					return errors.New("cannot replace and clear a credential together")
				}
				if p.Kind == "oauth" && (mutation.APIKey != "" || mutation.ClearCredential) {
					return fmt.Errorf("%w: Codex uses OAuth sign-in", ErrProviderSettingsReadOnly)
				}
				_, env, inline, _ := providerSettingsRaw(state.cfg, id)
				if (mutation.APIKey != "" || mutation.ClearCredential) && env != "" && os.Getenv(env) != "" {
					return fmt.Errorf("%w: remove the active environment override first", ErrProviderSettingsCredentialSource)
				}
				if mutation.ClearCredential && inline != "" {
					return fmt.Errorf("%w: remove the inline API key before clearing managed credentials", ErrProviderSettingsCredentialSource)
				}
				binding, _ := canonicalSettingsBinding(p.ID, p.Transport, p.BaseURL)
				if existing != nil {
					oldBinding, _ := canonicalSettingsBinding(id, existing.Transport, existing.BaseURL)
					if oldBinding != binding && mutation.APIKey == "" && !mutation.ClearCredential && (existing.HasCredential || existing.CredentialSource == "managed") {
						return fmt.Errorf("%w: endpoint changes require a new API key or explicit credential clearing", ErrProviderSettingsReadOnly)
					}
				}
				updated, err = renderSettingsProvider(updated, p)
				if err != nil {
					return err
				}
				state.sidecar.Providers[id] = storedProviderSettings{Binding: binding, DisplayName: p.DisplayName, CatalogProvider: p.CatalogProvider, Models: cloneSettingsModels(p.Models)}
				if mutation.APIKey != "" {
					if err := credentials.SetAPIKeyBound(id, mutation.APIKey, p.Transport, p.BaseURL); err != nil {
						return err
					}
				}
				if mutation.ClearCredential {
					credentials.ClearAPIKey(id)
				}
				if mutation.MakeDefault {
					updated, err = renderSettingsDefaultForWorkspace(updated, id, projectTrusted)
					if err != nil {
						return err
					}
				}
			case "make_default":
				if existing == nil || !existing.Registered {
					return errors.New("unknown provider")
				}
				if mutation.Model != "" {
					found := false
					for _, model := range existing.Models {
						found = found || model.ID == mutation.Model
					}
					if !found {
						return errors.New("default model must be in the saved model list")
					}
					p := *existing
					p.Model = mutation.Model
					updated, err = renderSettingsProvider(updated, p)
					if err != nil {
						return err
					}
				}
				updated, err = renderSettingsDefaultForWorkspace(updated, id, projectTrusted)
				if err != nil {
					return err
				}
			case "delete":
				if existing == nil || !existing.Registered {
					return errors.New("unknown provider")
				}
				if current.DefaultProvider == id {
					return fmt.Errorf("%w: select another default before deleting this provider", ErrProviderSettingsReadOnly)
				}
				if !existing.Removable {
					return fmt.Errorf("%w: %s", ErrProviderSettingsReadOnly, existing.RemoveReason)
				}
				updated, err = renderSettingsRemoveProvider(updated, id, existing.Kind)
				if err != nil {
					return err
				}
				delete(state.sidecar.Providers, id)
				credentials.ClearProvider(id)
			default:
				return errors.New("unsupported provider settings action")
			}
			if _, err := toml.Decode(string(updated), &Config{}); err != nil {
				return errors.New("refusing invalid updated provider configuration")
			}
			if len(updated) > providerSettingsMaxBytes {
				return errors.New("updated provider configuration is too large")
			}
			sidecar, err := json.MarshalIndent(state.sidecar, "", "  ")
			if err != nil {
				return err
			}
			if len(sidecar) > providerSettingsMaxBytes {
				return errors.New("saved provider settings are too large")
			}
			writeConfig := !bytes.Equal(updated, state.configBytes)
			writeSidecar := !bytes.Equal(sidecar, state.sidecarBytes) && (state.sidecarExists || len(state.sidecar.Providers) > 0)
			published = writeConfig || writeSidecar
			if writeConfig {
				if err := providerSettingsWriteFile(path, updated); err != nil {
					return fmt.Errorf("save provider configuration: %w", err)
				}
				state.configBytes, state.configExists = updated, true
			}
			if writeSidecar {
				if err := providerSettingsWriteFile(filepath.Join(filepath.Dir(path), providerSettingsFilename), sidecar); err != nil {
					return fmt.Errorf("save provider model list: %w", err)
				}
				state.sidecarBytes, state.sidecarExists = sidecar, true
			}
			state.cfg = defaults()
			state.metadata, err = toml.Decode(string(updated), state.cfg)
			if err != nil {
				return err
			}
			snapshot, err = providerSettingsSnapshotForWorkspace(state, credentials, projectTrusted)
			return err
		})
		if err != nil && published {
			configErr := restoreProviderSettingsFile(path, originalConfig, configExisted)
			sidecarErr := restoreProviderSettingsFile(filepath.Join(filepath.Dir(path), providerSettingsFilename), originalSidecar, sidecarExisted)
			return errors.Join(err, configErr, sidecarErr)
		}
		if err == nil {
			snapshot.Revision = providerSettingsRevision(state, credentialView)
		}
		return err
	})
	return snapshot, err
}

func restoreProviderSettingsFile(path string, original []byte, existed bool) error {
	if existed {
		if err := atomicWritePrivateFile(path, original); err != nil {
			return fmt.Errorf("restore previous provider settings: %w", err)
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove incomplete provider settings: %w", err)
	}
	return nil
}

func renderSettingsDefault(original []byte, id string) ([]byte, error) {
	return renderSettingsDefaultForWorkspace(original, id, true)
}

func renderSettingsDefaultForWorkspace(original []byte, id string, projectTrusted bool) ([]byte, error) {
	if projectTrusted {
		source, err := ProviderDefaultOverrideSource()
		if err != nil {
			return nil, err
		}
		if source != "" {
			return nil, fmt.Errorf("%w: default provider is controlled by %s", ErrProviderSettingsReadOnly, source)
		}
	}
	var probe userConfigProbe
	md, err := toml.Decode(string(original), &probe)
	if err != nil {
		return nil, errors.New("invalid user provider configuration")
	}
	newline := "\n"
	if bytes.Contains(original, []byte("\r\n")) {
		newline = "\r\n"
	}
	lines := splitLines(string(original))
	start, end, found := findSettingsTable(lines, "provider")
	if found {
		lines, err = setSettingsTableString(lines, start, end, "default", id, md.IsDefined("provider", "default"), "provider.default", newline)
		if err != nil {
			return nil, err
		}
	} else {
		if md.IsDefined("provider", "default") {
			return nil, errors.New("cannot safely update provider.default: use a simple provider table and direct key assignment")
		}
		firstChild := -1
		structural := tomlStructuralLines(lines)
		for i, line := range lines {
			if !structural[i] {
				continue
			}
			path, ok := settingsTablePath(line)
			if ok && len(path) > 1 && path[0] == "provider" {
				firstChild = i
				break
			}
		}
		if firstChild < 0 && md.IsDefined("provider") {
			return nil, errors.New("cannot safely update provider.default: inline or dotted provider definitions are unsupported")
		}
		block := []string{"[provider]" + newline, "default = " + quoteTOMLString(id) + newline, newline}
		if firstChild >= 0 {
			lines = insertLines(lines, firstChild, block, newline)
		} else {
			lines = appendBlock(lines, block, newline)
		}
	}
	updated := []byte(strings.Join(lines, ""))
	var verified userConfigProbe
	if _, err := toml.Decode(string(updated), &verified); err != nil || verified.Provider.Default != id {
		return nil, errors.New("provider default update verification failed")
	}
	return updated, nil
}

func renderSettingsProvider(original []byte, p ProviderSettingsProvider) ([]byte, error) {
	var raw map[string]any
	md, err := toml.Decode(string(original), &raw)
	if err != nil {
		return nil, errors.New("invalid user provider configuration")
	}
	table := "provider." + p.ID
	semantic := []string{"provider", p.ID}
	if p.Kind == "custom" {
		table = "provider.custom." + p.ID
		semantic = []string{"provider", "custom", p.ID}
	}
	newline := "\n"
	if bytes.Contains(original, []byte("\r\n")) {
		newline = "\r\n"
	}
	lines := splitLines(string(original))
	_, _, found := findSettingsTable(lines, table)
	if !found {
		if md.IsDefined(semantic...) {
			return nil, fmt.Errorf("cannot safely update %s: use a canonical table", table)
		}
		if p.Kind == "custom" {
			for _, ancestor := range []string{"provider", "provider.custom"} {
				start, end, ok := findSettingsTable(lines, ancestor)
				if ok && (settingsAssignmentCount(lines, start+1, end, "custom") > 0 || settingsAssignmentCount(lines, start+1, end, p.ID) > 0) {
					return nil, errors.New("cannot safely update inline provider table")
				}
			}
		}
		lines = appendBlock(lines, []string{"[" + table + "]" + newline}, newline)
	}
	fields := [][2]string{{"model", p.Model}}
	if p.Kind != "oauth" {
		fields = append(fields, [2]string{"base_url", p.BaseURL}, [2]string{"catalog_provider", p.CatalogProvider})
		if p.Kind == "custom" {
			fields = append(fields, [2]string{"transport", p.Transport})
		}
		if p.ID == "openai" {
			protocol := "chat"
			if p.Transport == "openai_responses" {
				protocol = "responses"
			}
			fields = append(fields, [2]string{"wire_protocol", protocol})
		}
	}
	for _, field := range fields {
		start, end, _ := findSettingsTable(lines, table)
		defined := append(append([]string(nil), semantic...), field[0])
		lines, err = setSettingsTableString(lines, start, end, field[0], field[1], md.IsDefined(defined...), table+"."+field[0], newline)
		if err != nil {
			return nil, err
		}
	}
	result := []byte(strings.Join(lines, ""))
	var verified Config
	if _, err := toml.Decode(string(result), &verified); err != nil {
		return nil, errors.New("rendered provider configuration is invalid")
	}
	check, _, _, _ := providerSettingsRaw(&verified, p.ID)
	if check.Model != p.Model || (p.Kind != "oauth" && (check.BaseURL != p.BaseURL || check.Transport != p.Transport || check.CatalogProvider != p.CatalogProvider)) {
		return nil, errors.New("provider configuration verification failed")
	}
	return result, nil
}

func renderSettingsRemoveProvider(original []byte, id, kind string) ([]byte, error) {
	table := "provider." + id
	semantic := []string{"provider", id}
	if kind == "custom" {
		table = "provider.custom." + id
		semantic = []string{"provider", "custom", id}
	}
	var before map[string]any
	md, err := toml.Decode(string(original), &before)
	if err != nil {
		return nil, errors.New("invalid provider configuration")
	}
	if !md.IsDefined(semantic...) {
		return append([]byte(nil), original...), nil
	}
	lines := splitLines(string(original))
	start, end, found := findSettingsTable(lines, table)
	if !found {
		return nil, errors.New("cannot safely delete provider: use a simple standalone provider table")
	}
	result := []byte(strings.Join(append(append([]string(nil), lines[:start]...), lines[end:]...), ""))
	var after map[string]any
	verified, err := toml.Decode(string(result), &after)
	if err != nil || verified.IsDefined(semantic...) {
		return nil, errors.New("provider child tables prevent safe deletion")
	}
	return result, nil
}

func renderSettingsDelete(original []byte, id string) ([]byte, error) {
	lines := splitLines(string(original))
	start, end, found := findSettingsTable(lines, "provider.custom."+id)
	if !found {
		return nil, errors.New("cannot safely delete provider: use a canonical table")
	}
	result := []byte(strings.Join(append(append([]string(nil), lines[:start]...), lines[end:]...), ""))
	var verify Config
	if _, err := toml.Decode(string(result), &verify); err != nil {
		return nil, errors.New("provider deletion verification failed")
	}
	if _, exists := verify.Provider.Custom[id]; exists {
		return nil, errors.New("provider child tables prevent safe deletion")
	}
	return result, nil
}

// settingsTablePath decodes a single, structural TOML table header. Using the
// parser preserves the distinction between ["provider.custom.id"] (one key)
// and ["provider"."custom"."id"] (three keys), including TOML escapes. The
// caller excludes headers embedded in multiline string values.
func settingsTablePath(line string) ([]string, bool) {
	body, _ := trimLineEnding(line)
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "[") || strings.HasPrefix(body, "[[") {
		return nil, false
	}
	var table map[string]any
	if _, err := toml.Decode(body+"\n", &table); err != nil {
		return nil, false
	}
	var path []string
	for len(table) > 0 {
		if len(table) != 1 {
			return nil, false
		}
		for key, value := range table {
			child, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			path = append(path, key)
			table = child
		}
	}
	return path, len(path) > 0
}

func findSettingsTable(lines []string, name string) (start, end int, found bool) {
	target := strings.Split(name, ".")
	structural := tomlStructuralLines(lines)
	for i, line := range lines {
		if !structural[i] {
			continue
		}
		path, ok := settingsTablePath(line)
		if !ok || len(path) != len(target) {
			continue
		}
		match := true
		for j := range path {
			match = match && path[j] == target[j]
		}
		if !match {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if structural[j] && isAnyTableHeader(lines[j]) {
				return i, j, true
			}
		}
		return i, len(lines), true
	}
	return 0, 0, false
}

// settingsAssignment identifies one direct key and its value span. The small
// lexical scan finds '=' outside quoted keys; TOML then validates and decodes
// the key. Dotted keys produce a nested map and are deliberately rejected.
func settingsAssignment(line string) (key string, valueAt int, ok bool) {
	body, _ := trimLineEnding(line)
	quote := byte(0)
	escaped := false
	equals := -1
	for i := 0; i < len(body); i++ {
		c := body[i]
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if c == '#' {
			return "", 0, false
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '=' {
			equals = i
			break
		}
	}
	if equals < 0 {
		return "", 0, false
	}
	var decoded map[string]any
	if _, err := toml.Decode(body[:equals]+"= \"\"\n", &decoded); err != nil || len(decoded) != 1 {
		return "", 0, false
	}
	for parsed, value := range decoded {
		if _, single := value.(string); !single {
			return "", 0, false
		}
		key = parsed
	}
	valueAt = equals + 1
	for valueAt < len(body) && (body[valueAt] == ' ' || body[valueAt] == '\t') {
		valueAt++
	}
	return key, valueAt, true
}

func settingsAssignmentPositions(lines []string, start, end int, key string) []int {
	structural := tomlStructuralLines(lines)
	var result []int
	for i := start; i < end; i++ {
		if !structural[i] {
			continue
		}
		parsed, _, ok := settingsAssignment(lines[i])
		if ok && parsed == key {
			result = append(result, i)
		}
	}
	return result
}

func settingsAssignmentCount(lines []string, start, end int, key string) int {
	return len(settingsAssignmentPositions(lines, start, end, key))
}

func setSettingsTableString(lines []string, start, end int, key, value string, defined bool, field, newline string) ([]string, error) {
	positions := settingsAssignmentPositions(lines, start+1, end, key)
	if len(positions) > 1 {
		return nil, fmt.Errorf("cannot safely update %s: duplicate direct assignments", field)
	}
	if len(positions) == 0 {
		if defined {
			return nil, fmt.Errorf("cannot safely update %s: use a simple direct key assignment", field)
		}
		return insertLines(lines, start+1, []string{key + " = " + quoteTOMLString(value) + newline}, newline), nil
	}
	pos := positions[0]
	body, ending := trimLineEnding(lines[pos])
	_, valueAt, _ := settingsAssignment(lines[pos])
	const prefix = "value = "
	replacement, err := replaceStringAssignment(prefix+body[valueAt:]+ending, "value", value)
	if err != nil {
		return nil, fmt.Errorf("cannot safely update %s: %w", field, err)
	}
	lines[pos] = body[:valueAt] + strings.TrimPrefix(replacement, prefix)
	return lines, nil
}
