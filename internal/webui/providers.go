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
)

type providerView struct {
	ID                   string `json:"id"`
	Transport            string `json:"transport"`
	BaseURL              string `json:"baseUrl,omitempty"`
	Model                string `json:"model,omitempty"`
	Custom               bool   `json:"custom"`
	Default              bool   `json:"default"`
	CredentialConfigured bool   `json:"credentialConfigured"`
}

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
	if err != nil || u.User != nil || u.Hostname() == "" {
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
	credential := func(id string) bool {
		_, err := cfg.ResolveAPIKey(id)
		return err == nil
	}
	views := []providerView{
		{ID: "anthropic", Transport: "anthropic_messages", BaseURL: cfg.Provider.Anthropic.BaseURL, Model: cfg.Provider.Anthropic.Model},
		{ID: "openai", Transport: "openai_chat", BaseURL: cfg.Provider.OpenAI.BaseURL, Model: cfg.Provider.OpenAI.Model},
		{ID: "gemini", Transport: "gemini_native", BaseURL: cfg.Provider.Gemini.BaseURL, Model: cfg.Provider.Gemini.Model},
	}
	customIDs := make([]string, 0, len(cfg.Provider.Custom))
	for id := range cfg.Provider.Custom {
		customIDs = append(customIDs, id)
	}
	sort.Strings(customIDs)
	for _, id := range customIDs {
		raw := cfg.Provider.Custom[id]
		views = append(views, providerView{
			ID: id, Transport: raw.Transport, BaseURL: raw.BaseURL, Model: raw.Model, Custom: true,
		})
	}
	for i := range views {
		views[i].Default = views[i].ID == cfg.Provider.Default
		views[i].CredentialConfigured = credential(views[i].ID)
	}
	return views
}

func providerExists(cfg *config.Config, id string) bool {
	if cfg == nil {
		return false
	}
	switch id {
	case "anthropic", "openai", "gemini":
		return true
	default:
		_, ok := cfg.Provider.Custom[id]
		return ok
	}
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		cfg, _, err := config.Load()
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
		if body.ClearCredential && body.APIKey != "" {
			writeError(w, http.StatusBadRequest, "cannot set and clear a credential together")
			return
		}
		spec := config.CustomProviderSpec{
			ID: strings.TrimSpace(body.ID), Transport: strings.TrimSpace(body.Transport),
			BaseURL: strings.TrimSpace(body.BaseURL), Model: strings.TrimSpace(body.Model),
		}
		if source, err := config.CustomProviderOverrideSource(spec.ID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		} else if source != "" {
			writeError(w, http.StatusConflict, "provider is controlled by "+source)
			return
		}
		if err := config.SaveUserCustomProvider(spec); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.ClearCredential {
			if err := auth.Remove(spec.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "provider saved but credential removal failed")
				return
			}
		} else if body.APIKey != "" {
			if err := auth.Set(spec.ID, body.APIKey); err != nil {
				writeError(w, http.StatusInternalServerError, "provider saved but credential write failed")
				return
			}
		}
		cfg, _, err := config.Load()
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
		if source, err := config.CustomProviderOverrideSource(id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		} else if source != "" {
			writeError(w, http.StatusConflict, "provider is controlled by "+source)
			return
		}
		if err := config.DeleteUserCustomProvider(id); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err := auth.Remove(id); err != nil {
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
	body.ID = strings.TrimSpace(body.ID)
	cfg, _, err := config.Load()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if source, err := config.ProviderDefaultOverrideSource(); err != nil {
		writeError(w, http.StatusConflict, "provider default source is unreadable")
		return
	} else if source != "" {
		writeError(w, http.StatusConflict, "provider.default is controlled by "+source)
		return
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
	body.ID = strings.TrimSpace(body.ID)
	cfg, _, err := config.Load()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if _, err := cfg.ResolveAPIKey(body.ID); err != nil && !errors.Is(err, config.ErrMissingAPIKey) {
		writeError(w, http.StatusBadRequest, "credential resolution failed")
		return
	} else if errors.Is(err, config.ErrMissingAPIKey) {
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
	body.ID = strings.TrimSpace(body.ID)
	cfg, _, err := config.Load()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	key, err := cfg.ResolveAPIKey(body.ID)
	if err != nil || strings.TrimSpace(key) == "" {
		writeError(w, http.StatusConflict, "credential is not configured")
		return
	}
	var view providerView
	for _, candidate := range configuredProviderViews(cfg) {
		if candidate.ID == body.ID {
			view = candidate
			break
		}
	}
	target, authKind, err := providerProbeTarget(view)
	if err != nil || !validProbeURL(target) {
		writeError(w, http.StatusBadRequest, "provider endpoint is not safe to probe")
		return
	}
	if authKind == "gemini" {
		u, _ := url.Parse(target)
		query := u.Query()
		query.Set("key", key)
		u.RawQuery = query.Encode()
		target = u.String()
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
