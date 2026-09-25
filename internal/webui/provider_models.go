package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

const maxProviderModelListingBytes = 2 << 20

type discoveredProviderModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type providerModelsRequest struct {
	ID              string `json:"id"`
	Transport       string `json:"transport"`
	BaseURL         string `json:"baseUrl"`
	APIKey          string `json:"apiKey"`
	ClearCredential bool   `json:"clearCredential"`
}

// handleProviderModels offers candidates for a saved provider or an unsaved
// form. Discovery never edits config. A stored key is reused for a draft only
// when the draft still names the same provider, transport, and endpoint.
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body providerModelsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.ID = auth.CanonicalProviderID(strings.TrimSpace(body.ID))
	body.Transport = strings.TrimSpace(body.Transport)
	body.BaseURL = strings.TrimSpace(body.BaseURL)
	if body.APIKey != "" && (strings.TrimSpace(body.APIKey) == "" || strings.ContainsAny(body.APIKey, "\r\n\x00")) {
		writeError(w, http.StatusBadRequest, "invalid API key")
		return
	}
	if body.ID == "openai-codex" {
		if body.Transport != "" || body.BaseURL != "" || body.APIKey != "" {
			writeError(w, http.StatusBadRequest, "the Codex OAuth model catalog cannot use a draft endpoint")
			return
		}
		cfg, err := s.loadProviderConfig()
		if err != nil || cfg == nil || !providerExists(cfg, body.ID) {
			writeError(w, http.StatusInternalServerError, "config unreadable")
			return
		}
		if !configuredProviderCredential(cfg, body.ID) {
			writeError(w, http.StatusConflict, "Codex OAuth is not connected")
			return
		}
		models := make([]discoveredProviderModel, 0, len(openai.CodexModels()))
		for _, model := range openai.CodexModels() {
			models = append(models, discoveredProviderModel{ID: model.ID, Name: model.Name})
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models, "source": "local_catalog", "provider": body.ID})
		return
	}
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	var saved providerView
	for _, candidate := range configuredProviderViews(cfg) {
		if candidate.ID == body.ID {
			saved = candidate
			break
		}
	}
	draft := body.Transport != "" || body.BaseURL != ""
	if draft && (body.Transport == "" || body.BaseURL == "") {
		writeError(w, http.StatusBadRequest, "transport and base URL are both required")
		return
	}
	view := saved
	if draft {
		view = providerView{ID: body.ID, Transport: body.Transport, BaseURL: body.BaseURL}
	} else if view.ID == "" {
		writeError(w, http.StatusBadRequest, "provider or draft endpoint is required")
		return
	}
	if saved.ID != "" && saved.CredentialKind != "api_key" && body.APIKey == "" {
		writeError(w, http.StatusBadRequest, "this credential type has no supported model listing")
		return
	}
	target, authKind, err := providerProbeTarget(view)
	if err != nil || !validProbeURL(target) {
		writeError(w, http.StatusBadRequest, "provider endpoint is not safe to query")
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" && !body.ClearCredential && saved.ID != "" && sameProviderModelEndpoint(saved, view) {
		if resolved, err := cfg.ResolveAPIKey(saved.ID); err == nil {
			key = strings.TrimSpace(resolved)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	models, err := queryProviderModels(ctx, target, authKind, view.Transport, key)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "source": "provider", "provider": body.ID})
}

func configuredProviderCredential(cfg *config.Config, id string) bool {
	for _, view := range configuredProviderViews(cfg) {
		if view.ID == id {
			return view.CredentialConfigured
		}
	}
	return false
}

func sameProviderModelEndpoint(a, b providerView) bool {
	if a.ID == "" || a.ID != b.ID {
		return false
	}
	left, err := auth.NormalizeEndpointBinding(a.ID, a.Transport, a.BaseURL)
	if err != nil {
		return false
	}
	right, err := auth.NormalizeEndpointBinding(b.ID, b.Transport, b.BaseURL)
	return err == nil && left == right
}

func queryProviderModels(ctx context.Context, target, authKind, transport, key string) ([]discoveredProviderModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("invalid provider model-list request")
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		switch authKind {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+key)
		case "anthropic":
			req.Header.Set("x-api-key", key)
		case "gemini":
			req.Header.Set("x-goog-api-key", key)
		}
	}
	if authKind == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	transportClient := http.DefaultTransport.(*http.Transport).Clone()
	if parsed, err := url.Parse(target); err == nil {
		host := strings.ToLower(parsed.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			transportClient.Proxy = nil
		}
	}
	defer transportClient.CloseIdleConnections()
	client := &http.Client{
		Timeout:       12 * time.Second,
		Transport:     transportClient,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider model-list endpoint is unreachable or redirected")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider model-list endpoint returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderModelListingBytes+1))
	if err != nil {
		return nil, errors.New("failed to read provider model list")
	}
	if len(data) > maxProviderModelListingBytes {
		return nil, errors.New("provider model list exceeds the size limit")
	}
	return parseProviderModels(data, transport)
}

func parseProviderModels(data []byte, transport string) ([]discoveredProviderModel, error) {
	var listing map[string]json.RawMessage
	if err := json.Unmarshal(data, &listing); err != nil || listing == nil {
		return nil, errors.New("provider did not return a JSON model list")
	}
	var entries []json.RawMessage
	var keys []string
	if raw, ok := listing["data"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return nil, errors.New("provider model list has an invalid data array")
		}
	} else if raw, ok := listing["models"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			var byID map[string]json.RawMessage
			if err := json.Unmarshal(raw, &byID); err != nil || byID == nil {
				return nil, errors.New("provider model list has an invalid models collection")
			}
			keys = make([]string, 0, len(byID))
			for id := range byID {
				keys = append(keys, id)
			}
			sort.Strings(keys)
			entries = make([]json.RawMessage, len(keys))
			for i, id := range keys {
				entries[i] = byID[id]
			}
		}
	} else {
		return nil, errors.New("provider response has no model list")
	}
	models := make([]discoveredProviderModel, 0, min(len(entries), 1000))
	seen := make(map[string]struct{})
	for i, raw := range entries {
		if len(models) >= 1000 {
			break
		}
		var row struct {
			ID                         string   `json:"id"`
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			DisplayNameSnake           string   `json:"display_name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		}
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		id := strings.TrimSpace(row.ID)
		if i < len(keys) {
			id = keys[i]
		}
		if strings.EqualFold(transport, "gemini_native") {
			if id == "" {
				id = strings.TrimSpace(row.Name)
			}
			if len(row.SupportedGenerationMethods) > 0 {
				canGenerate := false
				for _, method := range row.SupportedGenerationMethods {
					canGenerate = canGenerate || method == "generateContent"
				}
				if !canGenerate {
					continue
				}
			}
			id = strings.TrimPrefix(id, "models/")
		}
		if id == "" || len(id) > 256 || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(row.DisplayName)
		if name == "" {
			name = strings.TrimSpace(row.DisplayNameSnake)
		}
		if name == "" {
			name = strings.TrimSpace(row.Name)
		}
		if name == "" || len(name) > 256 {
			name = id
		}
		models = append(models, discoveredProviderModel{ID: id, Name: name})
	}
	return models, nil
}

func (s *Server) handleProviderModelSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.ID = auth.CanonicalProviderID(strings.TrimSpace(body.ID))
	body.Model = strings.TrimSpace(body.Model)
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	cfg, err := s.loadProviderConfig()
	if err != nil || !providerExists(cfg, body.ID) {
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if s.trustProviderConfig {
		var source string
		if _, custom := cfg.Provider.Custom[body.ID]; custom && !builtInProvider(body.ID) {
			source, err = config.CustomProviderOverrideSource(body.ID)
		} else {
			source, err = config.ProviderModelOverrideSource(body.ID)
		}
		if err != nil {
			writeError(w, http.StatusConflict, "provider model source is unreadable")
			return
		}
		if source != "" {
			writeError(w, http.StatusConflict, "provider model is controlled by "+source)
			return
		}
	}
	if err := config.SaveUserProviderModel(body.ID, body.Model); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": body.ID, "model": body.Model, "restartRequired": true})
}
