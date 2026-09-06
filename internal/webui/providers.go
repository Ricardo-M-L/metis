package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

type providerView struct {
	ID                   string `json:"id"`
	Transport            string `json:"transport"`
	BaseURL              string `json:"baseUrl,omitempty"`
	Model                string `json:"model,omitempty"`
	Custom               bool   `json:"custom"`
	Default              bool   `json:"default"`
	CredentialConfigured bool   `json:"credentialConfigured"`
	CredentialKind       string `json:"credentialKind,omitempty"`
	SetupCommand         string `json:"setupCommand,omitempty"`
}

const openAICodexBaseURL = "https://chatgpt.com/backend-api/codex"

func providerProbeTarget(view providerView) (string, string, error) {
	base := strings.TrimRight(strings.TrimSpace(view.BaseURL), "/")
	transport := strings.ToLower(strings.TrimSpace(view.Transport))
	switch transport {
	case "openai", "openai_chat", "openai_responses":
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return base + "/models", "bearer", nil
	case "anthropic", "anthropic_messages":
		if base == "" {
			base = "https://api.anthropic.com"
		}
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		return base + "/models", "anthropic", nil
	case "gemini", "gemini_native":
		if base == "" {
			base = "https://generativelanguage.googleapis.com"
		}
		if !strings.HasSuffix(base, "/v1beta") {
			base += "/v1beta"
		}
		return base + "/models", "gemini", nil
	default:
		return "", "", errors.New("this provider transport has no safe metadata probe")
	}
}

func validProbeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func configuredProviderViews(cfg *config.Config) []providerView {
	if cfg == nil {
		return nil
	}
	credential := func(id string) (bool, string) {
		if id == "openai-codex" {
			return rtpkg.ProviderHasCredentials(cfg, id), "oauth"
		}
		if id == "anthropic" {
			// API-key auth wins at runtime. Only label this OAuth when the
			// runtime-aware check succeeds without an API key.
			if _, err := cfg.ResolveAPIKey(id); err == nil {
				return true, "api_key"
			}
			if rtpkg.ProviderHasCredentials(cfg, id) {
				return true, "oauth"
			}
			return false, "api_key"
		}
		kind := "api_key"
		if raw, custom := cfg.Provider.Custom[id]; custom {
			switch strings.ToLower(strings.TrimSpace(raw.Transport)) {
			case "vertex", "vertex_anthropic":
				kind = "service_account"
			case "bedrock", "bedrock_anthropic":
				kind = "aws"
			}
		}
		return rtpkg.ProviderHasCredentials(cfg, id), kind
	}
	views := []providerView{
		{ID: "anthropic", Transport: "anthropic_messages", BaseURL: cfg.Provider.Anthropic.BaseURL, Model: cfg.Provider.Anthropic.Model},
		{ID: "openai", Transport: "openai_chat", BaseURL: cfg.Provider.OpenAI.BaseURL, Model: cfg.Provider.OpenAI.Model},
		{ID: "openai-codex", Transport: "openai_codex_responses", BaseURL: openAICodexBaseURL, Model: cfg.Provider.OpenAICodex.Model, SetupCommand: "metis login openai-codex"},
		{ID: "gemini", Transport: "gemini_native", BaseURL: cfg.Provider.Gemini.BaseURL, Model: cfg.Provider.Gemini.Model},
	}
	customIDs := make([]string, 0, len(cfg.Provider.Custom))
	for id := range cfg.Provider.Custom {
		if builtInProvider(id) {
			continue
		}
		customIDs = append(customIDs, id)
	}
	sort.Strings(customIDs)
	for _, id := range customIDs {
		raw := cfg.Provider.Custom[id]
		views = append(views, providerView{
			ID: id, Transport: raw.Transport, BaseURL: raw.BaseURL, Model: raw.Model, Custom: true,
		})
	}
	defaultProvider := auth.CanonicalProviderID(cfg.Provider.Default)
	for i := range views {
		views[i].Default = views[i].ID == defaultProvider
		views[i].CredentialConfigured, views[i].CredentialKind = credential(views[i].ID)
		if views[i].SetupCommand == "" && !views[i].Custom && !views[i].CredentialConfigured {
			views[i].SetupCommand = "metis login " + views[i].ID
		}
	}
	return views
}

func providerExists(cfg *config.Config, id string) bool {
	if cfg == nil {
		return false
	}
	id = auth.CanonicalProviderID(id)
	if builtInProvider(id) {
		return true
	}
	_, ok := cfg.Provider.Custom[id]
	return ok
}

func builtInProvider(id string) bool {
	switch auth.CanonicalProviderID(id) {
	case "anthropic", "openai", "openai-codex", "gemini":
		return true
	default:
		return false
	}
}

func (s *Server) loadProviderConfig() (*config.Config, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := config.ApplyProviderPolicyForWorkspace(cfg, s != nil && s.trustProviderConfig); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.loadProviderConfig()
		if err != nil || cfg == nil {
			writeError(w, http.StatusInternalServerError, "config unreadable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"providers": configuredProviderViews(cfg)})
	case http.MethodPost:
		var body struct {
			ID              string `json:"id"`
			Transport       string `json:"transport"`
			BaseURL         string `json:"baseUrl"`
			Model           string `json:"model"`
			APIKey          string `json:"apiKey"`
			ClearCredential bool   `json:"clearCredential"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if body.APIKey != "" && strings.TrimSpace(body.APIKey) == "" {
			writeError(w, http.StatusBadRequest, "api key must not be blank")
			return
		}
		if body.ClearCredential && body.APIKey != "" {
			writeError(w, http.StatusBadRequest, "cannot set and clear a credential together")
			return
		}
		spec := config.CustomProviderSpec{
			ID: strings.TrimSpace(body.ID), Transport: strings.TrimSpace(body.Transport),
			BaseURL: strings.TrimSpace(body.BaseURL), Model: strings.TrimSpace(body.Model),
		}
		if s.trustProviderConfig {
			if source, err := config.CustomProviderOverrideSource(spec.ID); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			} else if source != "" {
				writeError(w, http.StatusConflict, "provider is controlled by "+source)
				return
			}
		}
		if body.APIKey != "" {
			current, err := s.loadProviderConfig()
			if err != nil || current == nil {
				writeError(w, http.StatusInternalServerError, "config unreadable")
				return
			}
			if raw, ok := current.Provider.Custom[spec.ID]; ok {
				if source := config.CustomProviderCredentialOverrideSource(raw); source != "" {
					writeError(w, http.StatusConflict, source+" currently takes precedence; remove or unset it before saving a managed key")
					return
				}
			}
		}
		if body.APIKey == "" && !body.ClearCredential {
			storedEndpoint, present, err := auth.StoredAPIKeyEndpoint(spec.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "stored credential metadata is unreadable")
				return
			}
			if present {
				desired, bindErr := auth.NormalizeEndpointBinding(spec.ID, spec.Transport, spec.BaseURL)
				if bindErr != nil {
					writeError(w, http.StatusBadRequest, bindErr.Error())
					return
				}
				if storedEndpoint == nil {
					writeError(w, http.StatusConflict, "the existing credential is not endpoint-bound; enter the API key again or remove it")
					return
				}
				stored, bindErr := auth.NormalizeEndpointBinding(storedEndpoint.Provider, storedEndpoint.Transport, storedEndpoint.BaseURL)
				if bindErr != nil || stored != desired {
					writeError(w, http.StatusConflict, "transport or base URL changed; enter the API key again or remove it")
					return
				}
			}
		}
		if err := config.SaveUserCustomProvider(spec); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.ClearCredential {
			if err := auth.RemoveProviderCredentials(spec.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "provider saved but credential removal failed")
				return
			}
		} else if body.APIKey != "" {
			if err := auth.ActivateAPIKeyBound(spec.ID, body.APIKey, spec.Transport, spec.BaseURL); err != nil {
				writeError(w, http.StatusInternalServerError, "provider saved but credential write failed")
				return
			}
		}
		cfg, err := s.loadProviderConfig()
		if err != nil || cfg == nil {
			writeError(w, http.StatusInternalServerError, "provider saved but config reload failed")
			return
		}
		for _, view := range configuredProviderViews(cfg) {
			if view.ID == spec.ID {
				writeJSON(w, http.StatusCreated, view)
				return
			}
		}
		writeError(w, http.StatusInternalServerError, "provider save verification failed")
	case http.MethodDelete:
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
			writeError(w, http.StatusBadRequest, "provider id is required")
			return
		}
		id := strings.TrimSpace(body.ID)
		if s.trustProviderConfig {
			if source, err := config.CustomProviderOverrideSource(id); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			} else if source != "" {
				writeError(w, http.StatusConflict, "provider is controlled by "+source)
				return
			}
		}
		if err := config.DeleteUserCustomProvider(id); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err := auth.RemoveProviderCredentials(id); err != nil {
			writeError(w, http.StatusInternalServerError, "provider deleted but credential cleanup failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleProviderDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.ID = auth.CanonicalProviderID(body.ID)
	cfg, err := s.loadProviderConfig()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if s.trustProviderConfig {
		if source, err := config.ProviderDefaultOverrideSource(); err != nil {
			writeError(w, http.StatusConflict, "provider default source is unreadable")
			return
		} else if source != "" {
			writeError(w, http.StatusConflict, "provider.default is controlled by "+source)
			return
		}
	}
	if err := config.SaveUserProviderDefault(body.ID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": body.ID, "restartRequired": true})
}

func (s *Server) handleProviderValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.ID = auth.CanonicalProviderID(body.ID)
	cfg, err := s.loadProviderConfig()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	credentialConfigured := false
	for _, view := range configuredProviderViews(cfg) {
		if view.ID == body.ID {
			credentialConfigured = view.CredentialConfigured
			break
		}
	}
	if !credentialConfigured {
		writeError(w, http.StatusConflict, "credential is not configured")
		return
	}
	model := ""
	for _, view := range configuredProviderViews(cfg) {
		if view.ID == body.ID {
			model = view.Model
			break
		}
	}
	if model == "" {
		writeError(w, http.StatusBadRequest, "provider model is not configured")
		return
	}
	if s.buildProvider == nil {
		writeError(w, http.StatusServiceUnavailable, "provider construction unavailable")
		return
	}
	built, err := s.buildProvider(body.ID, model)
	if err != nil || built == nil || built.Provider == nil {
		if err == nil {
			err = errors.New("provider construction returned no provider")
		}
		writeError(w, http.StatusBadRequest, "provider validation failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "provider": body.ID, "model": built.Model,
		"note": "configuration validated locally; no model request was sent",
	})
}

// handleProviderProbe performs an explicit, no-token metadata request. It is
// separate from Validate so simply opening settings or saving a provider can
// never contact the network. Redirects are disabled to ensure credentials are
// not forwarded to a different origin.
func (s *Server) handleProviderProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || !body.Confirm {
		writeError(w, http.StatusBadRequest, "explicit network probe confirmation is required")
		return
	}
	body.ID = auth.CanonicalProviderID(body.ID)
	cfg, err := s.loadProviderConfig()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	var view providerView
	for _, candidate := range configuredProviderViews(cfg) {
		if candidate.ID == body.ID {
			view = candidate
			break
		}
	}
	if view.CredentialKind != "api_key" {
		writeError(w, http.StatusBadRequest, "this credential type does not support API-key metadata probes; use local validation")
		return
	}
	key, err := cfg.ResolveAPIKey(body.ID)
	if err != nil || strings.TrimSpace(key) == "" {
		writeError(w, http.StatusConflict, "credential is not configured")
		return
	}
	target, authKind, err := providerProbeTarget(view)
	if err != nil || !validProbeURL(target) {
		writeError(w, http.StatusBadRequest, "provider endpoint is not safe to probe")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "provider probe request is invalid")
		return
	}
	req.Header.Set("Accept", "application/json")
	switch authKind {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+key)
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", key)
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirect refused")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider metadata endpoint is unreachable")
		return
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		writeError(w, http.StatusBadGateway, "provider metadata probe returned "+resp.Status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reachable": true, "provider": body.ID, "status": resp.StatusCode,
		"note": "metadata endpoint reached; no prompt or model generation request was sent",
	})
}
