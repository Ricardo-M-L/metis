package webui

import (
	"net/http"
	"sort"
)

// MCPServerView is deliberately limited to non-credential metadata.
// Command arguments, environment, endpoint URLs, headers and errors must not
// be returned: all can contain sensitive secrets.
type MCPServerView struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	Transport string `json:"transport"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
	ToolCount int    `json:"toolCount"`
}

type MCPServersSnapshot struct {
	Servers              []MCPServerView
	PluginScanIncomplete bool
}

func (s *Server) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.mcpServersSnapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "MCP status unavailable")
		return
	}
	snapshot, err := s.mcpServersSnapshot()
	if err != nil {
		// Loading errors may contain local paths or credential-bearing TOML.
		writeError(w, http.StatusInternalServerError, "MCP status unavailable")
		return
	}
	if snapshot.Servers == nil {
		snapshot.Servers = []MCPServerView{}
	}
	sort.Slice(snapshot.Servers, func(i, j int) bool {
		if snapshot.Servers[i].Source != snapshot.Servers[j].Source {
			return snapshot.Servers[i].Source < snapshot.Servers[j].Source
		}
		return snapshot.Servers[i].Name < snapshot.Servers[j].Name
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"servers": snapshot.Servers, "statusScope": "desktop-process", "pluginScanIncomplete": snapshot.PluginScanIncomplete})
}
