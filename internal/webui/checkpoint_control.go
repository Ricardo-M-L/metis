package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

func (s *Server) checkpointWorkspace(sessionID, workspaceID string) (string, int, error) {
	if sessionID == "" {
		return s.freshTaskWorkspace(workspaceID)
	}
	if !validSessionID(sessionID) || s.store == nil {
		return "", http.StatusBadRequest, errors.New("invalid sessionId")
	}
	header, _, err := s.store.Load(sessionID)
	if err != nil || header == nil {
		return "", http.StatusNotFound, errors.New("session not found")
	}
	if strings.TrimSpace(header.WorkDir) == "" {
		return "", http.StatusConflict, errors.New("session has no workspace")
	}
	return header.WorkDir, http.StatusOK, nil
}

func (s *Server) handleCheckpointControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID, workspaceID, command := r.URL.Query().Get("sessionId"), r.URL.Query().Get("workspaceId"), "status"
	if r.Method == http.MethodPost {
		var body struct {
			SessionID   string `json:"sessionId"`
			WorkspaceID string `json:"workspaceId"`
			Command     string `json:"command"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid checkpoint request")
			return
		}
		sessionID, workspaceID, command = body.SessionID, body.WorkspaceID, body.Command
	}
	workspace, status, err := s.checkpointWorkspace(sessionID, workspaceID)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	id := sessionID
	if id == "" {
		id = "workspace-" + workspaceIDForPath(workspace)
	}
	manager := checkpoint.NewManager(id, workspace, "")
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, manager.ControlStatus())
		return
	}
	state, err := manager.Command(r.Context(), command)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "workspace busy") {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoint": state, "message": state.String()})
}

func (s *Server) checkpointStatusForView(sessionID, workspace string) checkpoint.ControlStatus {
	if sessionID != "" {
		if path, _, err := s.checkpointWorkspace(sessionID, ""); err == nil {
			workspace = path
		} else {
			return checkpoint.ControlStatus{Enabled: false, Reason: err.Error()}
		}
	}
	return checkpoint.NewManager("desktop-status", workspace, "").ControlStatus()
}
