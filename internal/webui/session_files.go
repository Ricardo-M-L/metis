package webui

import (
	"errors"
	"net/http"

	"github.com/Ricardo-M-L/metis/internal/sessionfiles"
)

func (s *Server) handleSessionFiles(w http.ResponseWriter, r *http.Request) {
	if !s.sessionFilesRequest(w, r, false) {
		return
	}
	files, err := sessionfiles.New(s.store).List(r.URL.Query().Get("sessionId"))
	if err != nil {
		sessionFilesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleSessionFileContent(w http.ResponseWriter, r *http.Request) {
	if !s.sessionFilesRequest(w, r, true) {
		return
	}
	preview, err := sessionfiles.New(s.store).Read(r.URL.Query().Get("sessionId"), r.URL.Query().Get("id"))
	if err != nil {
		sessionFilesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) sessionFilesRequest(w http.ResponseWriter, r *http.Request, content bool) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if r.URL.Query().Get("sessionId") == "" || content && r.URL.Query().Get("id") == "" {
		writeError(w, http.StatusBadRequest, "sessionId and file id are required for a preview")
		return false
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "session files are unavailable")
		return false
	}
	return true
}

func sessionFilesError(w http.ResponseWriter, err error) {
	status := http.StatusNotFound
	if errors.Is(err, sessionfiles.ErrInvalidSession) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, sessionfiles.ErrNotText) {
		status = http.StatusUnsupportedMediaType
	}
	// Shared errors contain no raw filesystem, transcript, or credential data.
	writeError(w, status, err.Error())
}
