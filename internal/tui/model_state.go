package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// modelStateFile is the filename under ~/.metis/ that stores the
// user's recent and favorite model selections.
const modelStateFile = "model-state.json"

// ModelState is the persisted shape of the user's model preferences.
// It tracks recent selections (most recent first) and favorites.
// The current model itself is NOT persisted here — it's derived from
// the config or the most recent selection at startup.
type ModelState struct {
	Recent   []string `json:"recent"`   // model IDs, most recent first, max 10
	Favorite []string `json:"favorite"` // model IDs marked as favorite
}

// modelState manages loading/saving ModelState to ~/.metis/model-state.json.
type modelState struct {
	mu   sync.RWMutex
	data ModelState
	path string
}

var globalModelState *modelState

// getModelState returns the singleton modelState, loading it from disk
// on first call. Errors are non-fatal — a corrupt file just means
// empty recent/favorite lists.
func getModelState() *modelState {
	if globalModelState != nil {
		return globalModelState
	}
	globalModelState = &modelState{
		path: filepath.Join(config.Home(), modelStateFile),
	}
	globalModelState.load()
	return globalModelState
}

// load reads the state file from disk. Missing or corrupt files are
// silently ignored — the user just gets empty recent/favorite lists.
func (s *modelState) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // missing file is fine
	}
	_ = json.Unmarshal(data, &s.data) // corrupt file is fine too
}

// save writes the current state to disk. Errors are logged but
// non-fatal — the app keeps working with in-memory state.
func (s *modelState) save() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, data, 0o644)
}

// AddRecent records a model selection in the recent list.
// If the model is already in the list, it's moved to the front.
// The list is capped at 10 entries.
func (s *modelState) AddRecent(modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove if already present.
	for i, id := range s.data.Recent {
		if id == modelID {
			s.data.Recent = append(s.data.Recent[:i], s.data.Recent[i+1:]...)
			break
		}
	}
	// Prepend to front.
	s.data.Recent = append([]string{modelID}, s.data.Recent...)
	// Cap at 10.
	if len(s.data.Recent) > 10 {
		s.data.Recent = s.data.Recent[:10]
	}
	s.save()
}

// IsRecent checks if a model ID is in the recent list.
func (s *modelState) IsRecent(modelID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.data.Recent {
		if id == modelID {
			return true
		}
	}
	return false
}

// ToggleFavorite adds or removes a model ID from the favorite list.
func (s *modelState) ToggleFavorite(modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, id := range s.data.Favorite {
		if id == modelID {
			s.data.Favorite = append(s.data.Favorite[:i], s.data.Favorite[i+1:]...)
			s.save()
			return
		}
	}
	s.data.Favorite = append(s.data.Favorite, modelID)
	s.save()
}

// IsFavorite checks if a model ID is in the favorite list.
func (s *modelState) IsFavorite(modelID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.data.Favorite {
		if id == modelID {
			return true
		}
	}
	return false
}

// Recent returns a copy of the recent list (most recent first).
func (s *modelState) Recent() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.data.Recent))
	copy(out, s.data.Recent)
	return out
}

// Favorite returns a copy of the favorite list.
func (s *modelState) Favorite() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.data.Favorite))
	copy(out, s.data.Favorite)
	return out
}
