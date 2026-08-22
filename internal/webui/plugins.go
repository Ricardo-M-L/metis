package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/pluginmarket"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

type pluginView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Version         string   `json:"version,omitempty"`
	Description     string   `json:"description,omitempty"`
	Source          string   `json:"source"`
	Loaded          bool     `json:"loaded"`
	Capabilities    []string `json:"capabilities,omitempty"`
	Error           string   `json:"error,omitempty"`
	RestartRequired bool     `json:"restartRequired"`
}

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	loaded := map[string]bool{}
	if s.plugins != nil {
		for _, plugin := range s.plugins.All() {
			if plugin != nil {
				loaded[plugin.Manifest.Name] = true
			}
		}
	}
	dir := rtpkg.PluginsDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "plugin directory unreadable")
		return
	}
	views := make([]pluginView, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "marketplaces" {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "plugin.toml")
		var manifest pubplugin.Manifest
		if _, err := toml.DecodeFile(path, &manifest); err != nil {
			views = append(views, pluginView{ID: entry.Name(), Name: entry.Name(), Source: path, Error: "manifest unreadable", RestartRequired: true})
			continue
		}
		caps := make([]string, 0, 3)
		if manifest.MCPServer != nil {
			caps = append(caps, "MCP tools")
		}
		if len(manifest.Skills) > 0 {
			caps = append(caps, "Skills")
		}
		if len(manifest.Hooks) > 0 {
			caps = append(caps, "Hooks")
		}
		name := manifest.Name
		if name == "" {
			name = entry.Name()
		}
		views = append(views, pluginView{
			ID: entry.Name(), Name: name, Version: manifest.Version, Description: manifest.Description,
			Source: path, Loaded: loaded[name], Capabilities: caps, RestartRequired: !loaded[name],
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins": views, "directory": dir,
		"note": "Installed plugin manifests are read without starting subprocesses or exposing environment values. Catalog changes apply after Desktop restart.",
	})
}

func (s *Server) pluginMarketplace() *pluginmarket.Manager {
	if s.pluginMarket == nil {
		s.pluginMarket = pluginmarket.NewManager()
	}
	return s.pluginMarket
}

func (s *Server) handlePluginCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.pluginMarketplace().Catalog())
}

func (s *Server) handlePluginCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Marketplaces []string `json:"marketplaces"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	results, catalog := s.pluginMarketplace().Sync(ctx, body.Marketplaces)
	status := http.StatusOK
	for _, result := range results {
		if result.Error != "" {
			status = http.StatusBadGateway
			break
		}
	}
	writeJSON(w, status, map[string]any{"results": results, "catalog": catalog})
}

func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Name        string `json:"name"`
		Marketplace string `json:"marketplace"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := s.pluginMarketplace().Install(ctx, body.Name, body.Marketplace)
	if err != nil {
		writePluginActionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handlePluginRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	result, err := s.pluginMarketplace().Remove(body.ID)
	if err != nil {
		writePluginActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writePluginActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pluginmarket.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid plugin or marketplace name")
	case errors.Is(err, pluginmarket.ErrNotFound):
		writeError(w, http.StatusNotFound, "plugin not found in this marketplace")
	case errors.Is(err, pluginmarket.ErrAlreadyInstalled):
		writeError(w, http.StatusConflict, "plugin is already installed")
	case errors.Is(err, pluginmarket.ErrNotInstallable):
		writeError(w, http.StatusUnprocessableEntity, "this plugin source is not installable yet")
	default:
		writeError(w, http.StatusInternalServerError, "plugin operation failed: "+err.Error())
	}
}
