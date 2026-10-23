package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

func registeredProviderViews(cfg *config.Config, snapshot config.ProviderSettingsSnapshot) []providerView {
	if cfg == nil {
		return []providerView{}
	}
	byID := make(map[string]config.ProviderSettingsProvider, len(snapshot.Providers))
	for _, provider := range snapshot.Providers {
		byID[provider.ID] = provider
	}
	views := make([]providerView, 0)
	for _, view := range providerRouteViews(cfg) {
		saved, known := byID[view.ID]
		if view.Custom || view.CredentialConfigured || known && saved.Registered {
			views = append(views, view)
		}
	}
	for i := range views {
		view := &views[i]
		view.DisplayName = view.ID
		view.Models = []config.ProviderModelSettings{{ID: view.Model, Name: view.Model}}
		view.Editable = view.CredentialKind == "api_key"
		view.ReadOnly = !view.Editable
		view.CredentialSource = "none"
		if view.CredentialConfigured {
			view.CredentialSource = view.CredentialKind
		}
		if saved, ok := byID[view.ID]; ok {
			view.DisplayName, view.CatalogProvider = saved.DisplayName, saved.CatalogProvider
			view.CredentialSource = saved.CredentialSource
			view.ReadOnly, view.ReadOnlyReason = saved.ReadOnly, saved.ReadOnlyReason
			view.Removable, view.RemoveReason = saved.Removable, saved.RemoveReason
			view.Editable = !saved.ReadOnly && view.CredentialKind == "api_key"
			// A trusted project route may shadow a user route. Its endpoint and
			// model must not inherit unrelated user-side model settings.
			if view.Model == saved.Model && sameProviderModelEndpoint(*view, providerView{ID: saved.ID, Transport: saved.Transport, BaseURL: saved.BaseURL}) {
				view.Models = append([]config.ProviderModelSettings(nil), saved.Models...)
			}
		} else {
			view.ReadOnly, view.Editable = true, false
			view.ReadOnlyReason = "provider is controlled by workspace configuration"
			view.RemoveReason = view.ReadOnlyReason
		}
		if view.DisplayName == "" {
			view.DisplayName = view.ID
		}
		if view.Models == nil {
			view.Models = []config.ProviderModelSettings{}
		}
		// Invalid legacy endpoints can contain embedded credentials. Their
		// diagnostic display must never serialize those values back to a UI.
		if endpoint, err := url.Parse(view.BaseURL); err == nil && endpoint != nil {
			if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				endpoint.User, endpoint.RawQuery, endpoint.Fragment = nil, "", ""
				view.BaseURL = endpoint.String()
				view.ReadOnly, view.Editable = true, false
				view.ReadOnlyReason = "endpoint contains unsupported authentication or query parameters; edit the configuration file"
			}
		}
	}
	return views
}

func (s *Server) readProviderSettings() (config.ProviderSettingsSnapshot, error) {
	return config.ReadProviderSettingsForWorkspace(s != nil && s.trustProviderConfig)
}

func providerViewsForServer(s *Server, cfg *config.Config) []providerView {
	views, err := s.configuredProviderViews(cfg)
	if err != nil {
		return nil
	}
	return views
}

// providerSettingsResponse deliberately contains no credential values. The
// revision describes the files consumed by the transactional configuration
// store, so another Desktop or CLI writer cannot silently overwrite this form.
func (s *Server) providerSettingsResponse(snapshot config.ProviderSettingsSnapshot) (map[string]any, error) {
	// Mutation results use the store's conservative project policy. Re-project
	// them through this Desktop's workspace trust before publishing membership.
	scoped, err := s.readProviderSettings()
	if err != nil {
		return nil, err
	}
	snapshot = scoped
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		return nil, errors.New("config unreadable")
	}
	views := registeredProviderViews(cfg, snapshot)
	s.stateMu.RLock()
	current := map[string]any{"provider": s.activeProviderName, "model": s.activeModel}
	activeID := s.activeSessionID
	s.stateMu.RUnlock()
	savedDefault := map[string]string{"provider": "", "model": ""}
	currentConfigured := false
	for _, view := range views {
		if view.ID == snapshot.DefaultProvider {
			savedDefault["provider"], savedDefault["model"] = snapshot.DefaultProvider, snapshot.DefaultModel
		}
		currentConfigured = currentConfigured || view.ID == current["provider"]
	}
	if activeID == "" && !currentConfigured {
		current = map[string]any{"provider": "", "model": ""}
	}
	return map[string]any{"revision": snapshot.Revision, "providers": views,
		"savedDefault": savedDefault, "current": current}, nil
}

func (s *Server) writeProviderSettings(w http.ResponseWriter, status int, snapshot config.ProviderSettingsSnapshot, extra map[string]any) {
	response, err := s.providerSettingsResponse(snapshot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	for key, value := range extra {
		response[key] = value
	}
	writeJSON(w, status, response)
}

func providerSettingsError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, config.ErrProviderSettingsConflict) || errors.Is(err, config.ErrProviderSettingsReadOnly) || errors.Is(err, config.ErrProviderSettingsCredentialSource) {
		status = http.StatusConflict
	}
	writeError(w, status, err.Error())
}

type providerSettingsRequest struct {
	ExpectedRevision *string                        `json:"expectedRevision"`
	Intent           string                         `json:"intent"`
	ID               string                         `json:"id"`
	DisplayName      string                         `json:"displayName"`
	CatalogProvider  string                         `json:"catalogProvider"`
	Transport        string                         `json:"transport"`
	BaseURL          string                         `json:"baseUrl"`
	Model            string                         `json:"model"`
	Models           []config.ProviderModelSettings `json:"models"`
	CredentialAction string                         `json:"credentialAction"`
	APIKey           string                         `json:"apiKey"`
	ClearCredential  bool                           `json:"clearCredential"`
}

func (s *Server) handleProviderSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	snapshot, err := s.readProviderSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provider settings unreadable")
		return
	}
	if r.Method == http.MethodGet {
		s.writeProviderSettings(w, http.StatusOK, snapshot, nil)
		return
	}
	var body providerSettingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.ID = auth.CanonicalProviderID(strings.TrimSpace(body.ID))
	if body.ID == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	if body.APIKey != "" && strings.TrimSpace(body.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "api key must not be blank")
		return
	}
	if body.ID == "openai-codex" && (body.APIKey != "" || body.ClearCredential || body.CredentialAction == "replace" || body.CredentialAction == "clear") {
		writeError(w, http.StatusBadRequest, "Codex uses OAuth sign-in and cannot accept API-key edits")
		return
	}
	expected := snapshot.Revision
	modern := body.ExpectedRevision != nil
	if modern {
		expected = *body.ExpectedRevision
	}
	if !modern && (body.Models != nil || body.CredentialAction != "" || body.Intent != "") {
		writeError(w, http.StatusBadRequest, "expectedRevision is required; reload provider settings")
		return
	}
	mutation := config.ProviderSettingsMutation{ExpectedRevision: expected, ID: body.ID}
	if r.Method == http.MethodDelete {
		mutation.Action = "delete"
		result, err := config.SaveProviderSettingsForWorkspace(mutation, s.trustProviderConfig)
		if err != nil {
			providerSettingsError(w, err)
			return
		}
		s.writeProviderSettings(w, http.StatusOK, result, map[string]any{"deleted": true, "id": body.ID})
		return
	}
	if body.Intent != "" && body.Intent != "create" && body.Intent != "update" {
		writeError(w, http.StatusBadRequest, "invalid provider save intent")
		return
	}
	credentialAction := strings.TrimSpace(body.CredentialAction)
	if credentialAction == "" {
		credentialAction = "keep"
		if body.APIKey != "" {
			credentialAction = "replace"
		}
		if body.ClearCredential {
			credentialAction = "clear"
		}
	}
	switch credentialAction {
	case "keep":
		if body.APIKey != "" || body.ClearCredential {
			writeError(w, http.StatusBadRequest, "keep cannot replace or clear credentials")
			return
		}
	case "replace":
		if strings.TrimSpace(body.APIKey) == "" || body.ClearCredential {
			writeError(w, http.StatusBadRequest, "replace requires a nonblank API key")
			return
		}
		mutation.APIKey = body.APIKey
	case "clear":
		if body.APIKey != "" {
			writeError(w, http.StatusBadRequest, "cannot set and clear a credential together")
			return
		}
		mutation.ClearCredential = true
	default:
		writeError(w, http.StatusBadRequest, "invalid credential action")
		return
	}
	var existing *config.ProviderSettingsProvider
	for i := range snapshot.Providers {
		if snapshot.Providers[i].ID == body.ID && snapshot.Providers[i].Registered {
			existing = &snapshot.Providers[i]
			break
		}
	}
	// The same snapshot supplies both the intent precondition and revision.
	// A concurrent writer after this check changes the store's CAS revision,
	// so create cannot turn into overwrite or update into silent recreation.
	if body.Intent != "" && expected != snapshot.Revision {
		providerSettingsError(w, config.ErrProviderSettingsConflict)
		return
	}
	if body.Intent == "create" && existing != nil {
		writeError(w, http.StatusConflict, "provider id already exists; edit the existing provider or choose another id")
		return
	}
	if body.Intent == "update" && existing == nil {
		writeError(w, http.StatusConflict, "provider no longer exists; reload provider settings before editing")
		return
	}
	provider := config.ProviderSettingsProvider{ID: body.ID, DisplayName: strings.TrimSpace(body.DisplayName), CatalogProvider: strings.TrimSpace(body.CatalogProvider), Transport: strings.TrimSpace(body.Transport), BaseURL: strings.TrimSpace(body.BaseURL), Model: strings.TrimSpace(body.Model), Models: body.Models}
	if existing != nil {
		provider.Kind = existing.Kind
	}
	if !modern {
		// Compatibility for the original single-model setup wizard. New forms
		// always supply a revision and explicitly choose their default action.
		if provider.Models == nil {
			provider.Models = []config.ProviderModelSettings{{ID: provider.Model, Name: provider.Model}}
		}
	}
	mutation.Action, mutation.Provider = "upsert", &provider
	result, err := config.SaveProviderSettingsForWorkspace(mutation, s.trustProviderConfig)
	if err != nil {
		providerSettingsError(w, err)
		return
	}
	status := http.StatusOK
	if existing == nil {
		status = http.StatusCreated
	}
	if !modern {
		status = http.StatusCreated
		// Older clients expect the saved provider itself rather than an envelope.
		response, err := s.providerSettingsResponse(result)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "config unreadable")
			return
		}
		for _, view := range response["providers"].([]providerView) {
			if view.ID == body.ID {
				writeJSON(w, status, view)
				return
			}
		}
	}
	s.writeProviderSettings(w, status, result, nil)
}

func (s *Server) handleProviderSettingsDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	var body struct {
		ID               string  `json:"id"`
		Model            string  `json:"model"`
		ExpectedRevision *string `json:"expectedRevision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	snapshot, err := s.readProviderSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provider settings unreadable")
		return
	}
	expected := snapshot.Revision
	if body.ExpectedRevision != nil {
		expected = *body.ExpectedRevision
	}
	result, err := config.SaveProviderSettingsForWorkspace(config.ProviderSettingsMutation{Action: "make_default", ExpectedRevision: expected, ID: auth.CanonicalProviderID(body.ID), Model: strings.TrimSpace(body.Model)}, s.trustProviderConfig)
	if err != nil {
		providerSettingsError(w, err)
		return
	}
	s.writeProviderSettings(w, http.StatusOK, result, map[string]any{"default": result.DefaultProvider, "restartRequired": false})
}

// Candidates come from an already cached public catalog. Opening settings
// never refreshes that cache or sends credentials to a model endpoint.
func (s *Server) handleProviderCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.writeProviderCatalog(w, r, catalog.NewClient(config.Home()).ReadOnlySnapshot(), "local_catalog", false)
}

// Refresh is an explicit public-directory request. It never resolves a model
// credential, discovers an authenticated endpoint, or submits a model prompt.
func (s *Server) handleProviderCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	client := catalog.NewClient(config.Home())
	if err := client.Refresh(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "Model catalog could not be refreshed. Cached entries are unchanged; check the network and retry.")
		return
	}
	s.writeProviderCatalog(w, r, client.ReadOnlySnapshot(), "refreshed_catalog", true)
}

func (s *Server) writeProviderCatalog(w http.ResponseWriter, r *http.Request, cached catalog.Catalog, source string, networkRequested bool) {
	type candidate struct {
		ID        string                         `json:"id"`
		Name      string                         `json:"name"`
		API       string                         `json:"api"`
		Transport string                         `json:"transport"`
		Models    []config.ProviderModelSettings `json:"models"`
	}
	providers := make([]candidate, 0, len(cached))
	for id, provider := range cached {
		if filter := r.URL.Query().Get("provider"); filter != "" && id != filter {
			continue
		}
		transport := catalog.TransportHint(provider.NPM)
		if transport == "" || transport == "unsupported" {
			continue
		}
		entry := candidate{ID: id, Name: provider.Name, API: provider.API, Transport: transport, Models: []config.ProviderModelSettings{}}
		for modelID, model := range provider.Models {
			configured := config.ProviderModelSettings{ID: modelID, Name: model.Name, ContextWindow: model.Limit.Context, MaxOutputTokens: model.Limit.Output}
			if len(model.Modalities.Input) > 0 {
				vision := model.SupportsImage()
				configured.SupportsVision = &vision
			}
			if model.Reasoning {
				reasoning := true
				configured.SupportsReasoning = &reasoning
			}
			entry.Models = append(entry.Models, configured)
		}
		sort.Slice(entry.Models, func(i, j int) bool { return entry.Models[i].ID < entry.Models[j].ID })
		providers = append(providers, entry)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers, "source": source, "networkRequested": networkRequested})
}

// A fresh conversation resolves the saved default at its admission boundary,
// rather than reusing the startup route. Existing session headers stay fixed.
func (s *Server) freshProviderRoute() (string, string, error) {
	if s.buildProvider == nil {
		return s.freshProviderName, s.freshModel, nil
	}
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		return "", "", errors.New("provider configuration unreadable")
	}
	id := auth.CanonicalProviderID(cfg.Provider.Default)
	for _, provider := range providerViewsForServer(s, cfg) {
		if provider.ID == id {
			return id, provider.Model, nil
		}
	}
	return "", "", errors.New("saved default provider is not configured")
}

func (s *Server) freshProviderPrompt(provider, model string) (string, []llm.SystemSection) {
	return rtpkg.RebindProviderPrompt(s.freshSystem, s.freshSystemSections, provider, model)
}

// Configuration-backed Desktop runtimes may only launch routes explicitly
// added by the user. A reduced embedder that directly supplies a Loop and no
// provider builder retains its independent provider contract.
func (s *Server) requireProviderMembership(provider string) error {
	if s.buildProvider == nil {
		return nil
	}
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		return errors.New("provider configuration unreadable")
	}
	if provider == "" {
		provider = s.freshProviderName
	}
	if !s.providerExists(cfg, provider) {
		return errors.New("provider is no longer configured; add it in Model settings or select a configured provider")
	}
	return nil
}
