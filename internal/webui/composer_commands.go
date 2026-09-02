package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// handleSessionCommand implements the transcript-level slash commands that
// must never be forwarded to the model. The TUI owns equivalent operations;
// Desktop keeps its own thin transport so the command palette can provide the
// same durable behavior without constructing a terminal REPL.
func (s *Server) handleSessionCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Command   string `json:"command"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil || !validSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, "valid sessionId is required")
		return
	}
	command := strings.ToLower(strings.TrimSpace(body.Command))
	switch command {
	case "save", "undo", "retry", "clear-history":
	default:
		writeError(w, http.StatusBadRequest, "unsupported session command")
		return
	}
	if s.loop == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	if !s.runMu.TryLock() {
		writeError(w, http.StatusConflict, "a turn is running; stop it before editing history")
		return
	}
	defer s.runMu.Unlock()

	header, history, err := s.store.Load(body.SessionID)
	if err != nil || header == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := s.activateSession(body.SessionID, header, history); err != nil {
		writeError(w, http.StatusConflict, "failed to activate session: "+err.Error())
		return
	}
	if command == "save" {
		if err := s.store.Sync(body.SessionID); err != nil {
			writeError(w, http.StatusInternalServerError, "save failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
		return
	}

	next := []llm.Message(nil)
	prefill := ""
	if command != "clear-history" {
		var ok bool
		next, prefill, ok = transcript.UndoWithPrefill(history)
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"changed": false, "messages": history, "message": "Nothing to undo"})
			return
		}
	}
	cursor := session.NewHistoryCursor(history)
	if err := s.store.ReplaceHistoryAndMark(body.SessionID, next, &cursor); err != nil {
		writeError(w, http.StatusInternalServerError, "history update failed: "+err.Error())
		return
	}
	s.loop.Restore(next)
	response := map[string]any{"changed": true, "messages": next}
	if command == "clear-history" {
		response["cleared"] = true
	} else {
		response["prefill"] = prefill
		response["retry"] = command == "retry"
	}
	writeJSON(w, http.StatusOK, response)
}

// handleGoals exposes the same durable GoalStore used by the agent tools to
// the Desktop composer. GET lists goals; POST creates one from the short
// objective entered in the command dialog.
func (s *Server) handleGoals(w http.ResponseWriter, r *http.Request) {
	store := builtin.CurrentGoalStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "goal store unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"goals": store.List("", "")})
	case http.MethodPost:
		var body struct {
			Objective   string `json:"objective"`
			Description string `json:"description"`
			Priority    string `json:"priority"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		objective := strings.TrimSpace(body.Objective)
		if objective == "" {
			writeError(w, http.StatusBadRequest, "objective is required")
			return
		}
		if len([]rune(objective)) > 240 {
			writeError(w, http.StatusBadRequest, "objective is too long")
			return
		}
		description := strings.TrimSpace(body.Description)
		if description == "" {
			description = objective
		}
		if len([]rune(description)) > 4000 {
			writeError(w, http.StatusBadRequest, "description is too long")
			return
		}
		priority := builtin.GoalPriority(body.Priority)
		switch priority {
		case "", builtin.GoalMedium:
			priority = builtin.GoalMedium
		case builtin.GoalHigh, builtin.GoalLow:
		default:
			writeError(w, http.StatusBadRequest, "priority must be high, medium, or low")
			return
		}
		goal, err := store.Create(objective, description, priority, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, goal)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleCompact performs a real manual compaction of one idle session and
// persists the replacement snapshot before publishing it to the live loop.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID    string `json:"sessionId"`
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || !validSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, "valid sessionId is required")
		return
	}
	if len([]rune(body.Instructions)) > 2000 {
		writeError(w, http.StatusBadRequest, "instructions are too long")
		return
	}
	if s.loop == nil || s.loop.Compactor == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "compactor unavailable")
		return
	}
	if !s.runMu.TryLock() {
		writeError(w, http.StatusConflict, "a turn or compaction is already running")
		return
	}
	defer s.runMu.Unlock()

	header, history, err := s.store.Load(body.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := s.activateSession(body.SessionID, header, history); err != nil {
		writeError(w, http.StatusConflict, "failed to activate session: "+err.Error())
		return
	}
	if len(history) <= 2 {
		writeJSON(w, http.StatusOK, map[string]any{"compacted": false, "before": len(history), "after": len(history), "message": "No compactable history yet"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cursor := session.NewHistoryCursor(history)
	result, err := s.loop.CompactNow(ctx, agent.CompactOptions{
		Trigger:      "manual",
		Force:        true,
		Instructions: strings.TrimSpace(body.Instructions),
		Persist: func(replacement []llm.Message) error {
			return s.store.ReplaceHistoryAndMark(body.SessionID, replacement, &cursor)
		},
		Emit: func(ev agent.Event) {
			s.hub.publish(body.SessionID, ev)
		},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "compact failed: "+err.Error())
		return
	}
	if !result.Applied {
		writeJSON(w, http.StatusOK, map[string]any{
			"compacted":    false,
			"before":       result.BeforeMessages,
			"after":        result.AfterMessages,
			"beforeTokens": result.BeforeTokens,
			"afterTokens":  result.AfterTokens,
			"message":      "History did not need compaction",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"compacted":    true,
		"before":       result.BeforeMessages,
		"after":        result.AfterMessages,
		"beforeTokens": result.BeforeTokens,
		"afterTokens":  result.AfterTokens,
	})
}

// applyPermissionMode keeps the permission gate and the Loop plan controller
// on one state transition. Desktop settings and /plan both use this path.
func (s *Server) applyPermissionMode(mode permission.Mode) error {
	if s == nil || s.loop == nil || s.loop.Gate == nil {
		return nil
	}
	if s.setPermissionMode != nil {
		return s.setPermissionMode(mode)
	}
	return s.loop.Gate.RunModeTransition(func() error {
		previous := s.loop.Gate.Mode()
		if mode == permission.ModePlan {
			if previous != permission.ModePlan {
				s.loop.SetPrePlanMode(string(previous))
			}
			s.loop.Gate.SetModeAndWait(mode)
			if committed := s.loop.Gate.Mode(); committed != mode {
				if committed != permission.ModePlan {
					s.loop.SetPrePlanMode("")
				}
				s.loop.SetPlanMode(committed == permission.ModePlan)
				return fmt.Errorf("permission mode %s was superseded by %s", mode, committed)
			}
			s.loop.SetPlanMode(true)
			return nil
		}
		if previous == permission.ModePlan {
			s.loop.SetPrePlanMode("")
		}
		s.loop.Gate.SetModeAndWait(mode)
		if committed := s.loop.Gate.Mode(); committed != mode {
			s.loop.SetPlanMode(committed == permission.ModePlan)
			if committed != permission.ModePlan {
				s.loop.SetPrePlanMode("")
			}
			return fmt.Errorf("permission mode %s was superseded by %s", mode, committed)
		}
		s.loop.SetPlanMode(false)
		return nil
	})
}
