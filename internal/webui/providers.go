package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

type providerView struct {
	ID                   string                         `json:"id"`
	Transport            string                         `json:"transport"`
	BaseURL              string                         `json:"baseUrl,omitempty"`
	Model                string                         `json:"model,omitempty"`
	Custom               bool                           `json:"custom"`
	Default              bool                           `json:"default"`
	CredentialConfigured bool                           `json:"credentialConfigured"`
	CredentialKind       string                         `json:"credentialKind,omitempty"`
	SetupCommand         string                         `json:"setupCommand,omitempty"`
	DisplayName          string                         `json:"displayName"`
	CatalogProvider      string                         `json:"catalogProvider,omitempty"`
	Models               []config.ProviderModelSettings `json:"models"`
	CredentialSource     string                         `json:"credentialSource"`
	Editable             bool                           `json:"editable"`
	ReadOnly             bool                           `json:"readOnly"`
	ReadOnlyReason       string                         `json:"readOnlyReason,omitempty"`
	Removable            bool                           `json:"removable"`
	RemoveReason         string                         `json:"removeReason,omitempty"`
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

func providerRouteViews(cfg *config.Config) []providerView {
	if cfg == nil {
		return nil
	}
	openAITransport := "openai_chat"
	if strings.EqualFold(strings.TrimSpace(cfg.Provider.OpenAI.WireProtocol), "responses") {
		openAITransport = "openai_responses"
	}
	views := []providerView{
		{ID: "anthropic", Transport: "anthropic_messages", BaseURL: cfg.Provider.Anthropic.BaseURL, Model: cfg.Provider.Anthropic.Model},
		{ID: "openai", Transport: openAITransport, BaseURL: cfg.Provider.OpenAI.BaseURL, Model: cfg.Provider.OpenAI.Model},
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
		transport := raw.Transport
		if strings.TrimSpace(transport) == "" {
			transport = "anthropic_messages"
		}
		views = append(views, providerView{
			ID: id, Transport: transport, BaseURL: raw.BaseURL, Model: raw.Model, Custom: true,
		})
	}
	// Settings, model lists and membership checks must never use auth.Load:
	// that runtime loader repairs historical credentials on disk. The
	// transaction view inspects all four stores without changing any bytes.
	for i := range views {
		views[i].CredentialKind = providerCredentialKind(cfg, views[i].ID)
	}
	_ = auth.WithProviderSettingsCredentials(func(credentials *auth.ProviderSettingsCredentials) error {
		for i := range views {
			views[i].CredentialConfigured, views[i].CredentialKind = readOnlyProviderCredential(cfg, credentials, views[i].ID)
		}
		return nil
	})
	defaultProvider := auth.CanonicalProviderID(cfg.Provider.Default)
	for i := range views {
		views[i].Default = views[i].ID == defaultProvider
		if views[i].SetupCommand == "" && !views[i].Custom && !views[i].CredentialConfigured {
			views[i].SetupCommand = "metis login " + views[i].ID
		}
	}
	return views
}

func providerCredentialKind(cfg *config.Config, id string) string {
	if id == "openai-codex" {
		return "oauth"
	}
	if raw, ok := cfg.Provider.Custom[id]; ok {
		switch strings.ToLower(strings.TrimSpace(raw.Transport)) {
		case "vertex", "vertex_anthropic":
			return "service_account"
		case "bedrock", "bedrock_anthropic":
			return "aws"
		}
	}
	return "api_key"
}

func readOnlyProviderCredential(cfg *config.Config, credentials *auth.ProviderSettingsCredentials, id string) (bool, string) {
	kind := providerCredentialKind(cfg, id)
	if id == "openai-codex" {
		return credentials.OAuthCredentialReady(id), kind
	}
	var env, inline, endpoint, transport string
	switch id {
	case "anthropic":
		raw := cfg.Provider.Anthropic
		env, inline, endpoint, transport = raw.APIKeyEnv, raw.APIKey, raw.BaseURL, "anthropic_messages"
	case "openai":
		raw := cfg.Provider.OpenAI
		env, inline, endpoint, transport = raw.APIKeyEnv, raw.APIKey, raw.BaseURL, "openai_chat"
		if strings.EqualFold(strings.TrimSpace(raw.WireProtocol), "responses") {
			transport = "openai_responses"
		}
	case "gemini":
		raw := cfg.Provider.Gemini
		env, inline, endpoint, transport = raw.APIKeyEnv, raw.APIKey, raw.BaseURL, "gemini_native"
	default:
		raw, ok := cfg.Provider.Custom[id]
		if !ok {
			return false, kind
		}
		env, inline, endpoint, transport = raw.APIKeyEnv, raw.APIKey, raw.BaseURL, raw.Transport
		if kind == "service_account" {
			path := strings.TrimSpace(raw.ServiceAccountFile)
			if path == "" {
				return false, kind
			}
			info, err := os.Stat(filepath.Clean(path))
			return err == nil && !info.IsDir(), kind
		}
		if kind == "aws" {
			access := env != "" && strings.TrimSpace(os.Getenv(env)) != ""
			if !access {
				if entry, ok := credentials.ManagedAPIKey(id); ok {
					access = strings.TrimSpace(entry.Key) != ""
				}
			}
			if !access {
				access = strings.TrimSpace(inline) != "" || strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")) != ""
			}
			secret := raw.SecretKeyEnv != "" && os.Getenv(raw.SecretKeyEnv) != "" || os.Getenv("AWS_SECRET_ACCESS_KEY") != ""
			return access && secret, kind
		}
	}
	if env != "" && os.Getenv(env) != "" || id == "gemini" && os.Getenv("GOOGLE_API_KEY") != "" {
		return true, kind
	}
	if endpoint == "" {
		switch id {
		case "anthropic":
			endpoint = "https://api.anthropic.com"
		case "openai":
			endpoint = "https://api.openai.com/v1"
		case "gemini":
			endpoint = "https://generativelanguage.googleapis.com"
		}
	}
	allowLegacy := false
	if id == "anthropic" || id == "openai" || id == "gemini" {
		official := "https://api.openai.com/v1"
		if id == "anthropic" {
			official = "https://api.anthropic.com"
		}
		if id == "gemini" {
			official = "https://generativelanguage.googleapis.com"
		}
		binding, err := auth.NormalizeEndpointBinding(id, transport, endpoint)
		target, targetErr := auth.NormalizeEndpointBinding(id, transport, official)
		allowLegacy = err == nil && targetErr == nil && binding == target
	}
	available, err := credentials.ProbeAPIKeyForEndpoint(id, transport, endpoint, allowLegacy)
	if err != nil {
		return false, kind
	} // A route mismatch never falls through to OAuth or inline credentials.
	if available || inline != "" {
		return true, kind
	}
	if id == "anthropic" && credentials.OAuthCredentialReady(id) {
		origin, err := url.Parse(endpoint)
		if err == nil && strings.EqualFold(origin.Scheme, "https") {
			host := strings.ToLower(origin.Hostname())
			if host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com") {
				return true, "oauth"
			}
		}
	}
	return false, kind
}

// configuredProviderViews projects actual saved membership, rather than the
// four transport presets supplied by Config defaults. Callers with a trusted
// workspace use the Server variant below.
func configuredProviderViews(cfg *config.Config) []providerView {
	snapshot, err := config.ReadProviderSettingsForWorkspace(false)
	if err != nil {
		return nil
	}
	return registeredProviderViews(cfg, snapshot)
}

func (s *Server) configuredProviderViews(cfg *config.Config) ([]providerView, error) {
	snapshot, err := s.readProviderSettings()
	if err != nil {
		return nil, err
	}
	return registeredProviderViews(cfg, snapshot), nil
}

func providerExists(cfg *config.Config, id string) bool {
	id = auth.CanonicalProviderID(id)
	for _, view := range configuredProviderViews(cfg) {
		if view.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) providerExists(cfg *config.Config, id string) bool {
	views, err := s.configuredProviderViews(cfg)
	if err != nil {
		return false
	}
	id = auth.CanonicalProviderID(id)
	for _, view := range views {
		if view.ID == id {
			return true
		}
	}
	return false
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
	s.handleProviderSettings(w, r)
}

func (s *Server) handleProviderDefault(w http.ResponseWriter, r *http.Request) {
	s.handleProviderSettingsDefault(w, r)
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
	if err != nil || !s.providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	credentialConfigured := false
	for _, view := range providerViewsForServer(s, cfg) {
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
	for _, view := range providerViewsForServer(s, cfg) {
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
	if err != nil || !s.providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	var view providerView
	for _, candidate := range providerViewsForServer(s, cfg) {
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, "provider metadata probe returned "+resp.Status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reachable": true, "provider": body.ID, "status": resp.StatusCode,
		"note": "metadata endpoint reached; no prompt or model generation request was sent",
	})
}
