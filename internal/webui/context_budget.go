package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

type providerContextBudgetRequest struct {
	Model            string `json:"model"`
	Window           int    `json:"window"`
	ExpectedRouteKey string `json:"expectedRouteKey"`
}

// Only the server may derive an endpoint/account identity. An opaque identity
// from the preceding GET fences a save against a changed provider route.
func (s *Server) handleProviderContextBudget(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/providers/"), "/")
	if len(parts) != 2 || parts[1] != "context-budget" || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := auth.CanonicalProviderID(parts[0])
	cfg, err := s.loadProviderConfig()
	if err != nil || !providerExists(cfg, id) {
		writeError(w, http.StatusBadRequest, "provider is not configured")
		return
	}
	body := providerContextBudgetRequest{Model: r.URL.Query().Get("model")}
	if r.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || body.Window < 0 || body.Window > 100_000_000 {
			writeError(w, http.StatusBadRequest, "invalid context budget")
			return
		}
	}
	body.Model = strings.TrimSpace(body.Model)
	if body.Model == "" || len(body.Model) > 256 || strings.ContainsAny(body.Model, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	route, err := rtpkg.ProviderContextBudgetRoute(cfg, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "provider route is unavailable")
		return
	}
	key, err := config.ContextBudgetRouteKey(route, body.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid provider or model route")
		return
	}
	if r.Method == http.MethodPost {
		if body.ExpectedRouteKey == "" || body.ExpectedRouteKey != key {
			writeError(w, http.StatusConflict, "provider route changed; reload context settings and try again")
			return
		}
		if err := config.SetContextBudget(route, body.Model, body.Window); err != nil {
			writeError(w, http.StatusBadRequest, "could not save context budget")
			return
		}
	}
	requested, configured, err := config.LookupContextBudget(route, body.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read context budget")
		return
	}
	// A successful save does not interrupt an in-flight worker or mutate its
	// request binding. New tasks construct a provider using the saved budget.
	response := map[string]any{"providerId": id, "model": body.Model, "routeKey": key,
		"requestedWindow": requested, "configured": configured, "applied": false,
		"appliesOn": "next_task", "capabilities": llm.ContextCapabilities{}}
	build := s.buildProvider
	if build == nil {
		build = func(provider, model string) (*rtpkg.ProviderBuild, error) {
			return rtpkg.BuildProviderWithoutPreconnect(cfg, provider, model)
		}
	}
	built, err := build(id, body.Model)
	if err == nil && built != nil && built.Provider != nil {
		if metadata, ok := built.Provider.(llm.ContextCapabilitiesProvider); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			refreshErr := metadata.RefreshContextCapabilities(ctx)
			cancel()
			if errors.Is(refreshErr, context.Canceled) && r.Context().Err() != nil {
				return
			}
			capabilities := metadata.ContextCapabilities()
			if (capabilities.RouteKey != "" || capabilities.ProviderID != "" || capabilities.Model != "") && (capabilities.RouteKey != key || capabilities.ProviderID != id || capabilities.Model != body.Model) {
				writeError(w, http.StatusConflict, "provider route changed; reload context settings and try again")
				return
			}
			response["capabilities"] = capabilities
		} else {
			response["capabilities"] = llm.ContextCapabilities{Window: built.Provider.MaxContextTokens(), Source: "provider_fallback"}
		}
	} else {
		response["capabilityWarning"] = "provider capabilities are unavailable; saved budget applies when the provider is configured"
	}
	writeJSON(w, http.StatusOK, response)
}
