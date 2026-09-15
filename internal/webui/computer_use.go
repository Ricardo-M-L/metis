package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const computerUseRequestLimit = 4096

// handleComputerUse exposes only named actions. In particular, status never
// installs a helper, and an HTTP caller cannot provide a local executable.
func (s *Server) handleComputerUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.computerUse == nil {
		writeError(w, http.StatusServiceUnavailable, "Computer Use is unavailable in this runtime")
		return
	}
	action := "status"
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, computerUseRequestLimit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "Computer Use request is too large")
			} else {
				writeError(w, http.StatusBadRequest, "invalid Computer Use request")
			}
			return
		}
		action, err = decodeComputerUseAction(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	status, err := s.computerUse(r.Context(), action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func decodeComputerUseAction(body []byte) (string, error) {
	invalid := errors.New("expected one Computer Use action field")
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') || !decoder.More() {
		return "", invalid
	}
	key, err := decoder.Token()
	if err != nil || key != "action" {
		return "", invalid
	}
	var action string
	if err := decoder.Decode(&action); err != nil || decoder.More() {
		return "", invalid
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return "", invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", invalid
	}
	switch action {
	case "install", "enable", "stop", "disable", "permissions-accessibility", "permissions-screen-recording":
		return action, nil
	default:
		return "", errors.New("unsupported Computer Use action")
	}
}
