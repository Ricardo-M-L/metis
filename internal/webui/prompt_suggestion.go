package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/promptsuggest"
)

type promptSuggestionSnapshot struct {
	provider, model string
	exchange        promptsuggest.Exchange
	messageCount    int
}

func promptSuggestionsEnabled() bool {
	cfg, _, err := config.Load()
	return err == nil && cfg != nil && cfg.UI.PromptSuggestions
}

func (s *Server) readPromptSuggestionSnapshot(id string) (promptSuggestionSnapshot, bool) {
	if s.store == nil {
		return promptSuggestionSnapshot{}, false
	}
	lock, acquired, err := s.store.TryAcquireTurnLock(id)
	if err != nil || !acquired {
		return promptSuggestionSnapshot{}, false
	}
	defer lock.Release()
	header, history, err := s.store.Load(id)
	if err != nil || header == nil || header.ID != id {
		return promptSuggestionSnapshot{}, false
	}
	exchange, ok := promptsuggest.LatestExchange(history)
	if !ok {
		return promptSuggestionSnapshot{}, false
	}
	return promptSuggestionSnapshot{provider: header.Provider, model: header.Model, exchange: exchange, messageCount: len(history)}, true
}

func (s *Server) promptSuggestionProvider(id string, snapshot promptSuggestionSnapshot) (llm.Provider, string) {
	if s.buildProvider != nil && snapshot.provider != "" {
		s.providersMu.Lock()
		built, err := s.buildProvider(snapshot.provider, snapshot.model)
		s.providersMu.Unlock()
		if err != nil || built == nil || built.Provider == nil {
			return nil, ""
		}
		return built.Provider, built.Model
	}
	if s.loop == nil || !s.runMu.TryLock() {
		return nil, ""
	}
	defer s.runMu.Unlock()
	s.stateMu.RLock()
	matches := s.activeSessionID == id && (snapshot.provider == "" || s.activeProviderName == snapshot.provider)
	s.stateMu.RUnlock()
	if !matches {
		return nil, ""
	}
	p, model, _ := s.loop.ProviderModelSnapshot()
	if snapshot.model != "" && model != snapshot.model {
		return nil, ""
	}
	return p, model
}

func (s *Server) handlePromptSuggestion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil || !validSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, "valid sessionId is required")
		return
	}
	response := struct {
		SessionID  string `json:"sessionId"`
		Suggestion string `json:"suggestion"`
		Enabled    bool   `json:"enabled"`
	}{SessionID: body.SessionID, Enabled: promptSuggestionsEnabled()}
	defer func() { writeJSON(w, http.StatusOK, response) }()
	if !response.Enabled {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	snapshot, ok := s.readPromptSuggestionSnapshot(body.SessionID)
	if !ok {
		return
	}
	p, model := s.promptSuggestionProvider(body.SessionID, snapshot)
	if p == nil {
		return
	}
	text, err := promptsuggest.Generate(ctx, p, model, snapshot.exchange)
	if err != nil || text == "" {
		return
	}
	// Do not hold a session lock during inference: the user's next turn wins.
	current, ok := s.readPromptSuggestionSnapshot(body.SessionID)
	response.Enabled = promptSuggestionsEnabled()
	if ok && current == snapshot && response.Enabled && ctx.Err() == nil {
		response.Suggestion = text
	}
}
