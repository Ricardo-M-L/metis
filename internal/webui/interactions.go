package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/google/uuid"
)

const pendingInteractionLimit = 32
const pendingInteractionTimeout = 120 * time.Second

// publishInteraction gives each interactive request a session owner and a
// bounded lifetime. Only its presentation copy enters snapshots or replay;
// authorization inputs and reply channels stay with the running agent.
func (s *Server) publishInteraction(ctx context.Context, sessionID string, ev agent.Event) bool {
	switch ev.Kind {
	case agent.EventPermissionRequest:
		if ev.PermissionReply == nil {
			return true
		}
		id := uuid.NewString()
		pending := &permissionPending{
			reply: ev.PermissionReply, tool: ev.PermissionTool, session: sessionID,
			event: ev.PresentationCopy(), done: make(chan struct{}),
		}
		s.permMu.Lock()
		if ctx.Err() != nil || len(s.pendingPerms) >= pendingInteractionLimit {
			s.permMu.Unlock()
			replyPermission(pending, agent.PermissionDecisionDeny)
			return true
		}
		if s.pendingPerms == nil {
			s.pendingPerms = make(map[string]*permissionPending)
		}
		s.pendingPerms[id] = pending
		// Publish under the queue lock so a cancellation cannot publish its
		// resolution before the corresponding card has entered the event hub.
		s.hub.publish(sessionID, pending.event, map[string]any{"permId": id})
		s.permMu.Unlock()
		go s.waitInteraction(ctx, pending.done, func() { s.timeoutPermission(id) })
		return true
	case agent.EventAskUser:
		if ev.AskUserReply == nil {
			return true
		}
		id := uuid.NewString()
		pending := &askPending{
			reply: ev.AskUserReply, session: sessionID,
			event: ev.PresentationCopy(), done: make(chan struct{}),
		}
		s.askMu.Lock()
		if ctx.Err() != nil || len(s.pendingAsks) >= pendingInteractionLimit {
			s.askMu.Unlock()
			replyAsk(pending, "")
			return true
		}
		if s.pendingAsks == nil {
			s.pendingAsks = make(map[string]*askPending)
		}
		s.pendingAsks[id] = pending
		s.hub.publish(sessionID, pending.event, map[string]any{"askId": id})
		s.askMu.Unlock()
		go s.waitInteraction(ctx, pending.done, func() { s.timeoutAsk(id) })
		return true
	default:
		return false
	}
}

func (s *Server) waitInteraction(ctx context.Context, done <-chan struct{}, expire func()) {
	timer := time.NewTimer(pendingInteractionTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		expire()
	case <-timer.C:
		expire()
	}
}

func finishInteraction(done chan struct{}) {
	if done != nil {
		close(done)
	}
}

func replyPermission(pending *permissionPending, decision agent.PermissionDecision) {
	select {
	case pending.reply <- decision:
	default:
	}
}

func replyAsk(pending *askPending, answer string) {
	select {
	case pending.reply <- answer:
	default:
	}
}

func (s *Server) interactionResolved(sessionID, field, id string) {
	s.hub.publish(sessionID, agent.Event{Kind: agent.EventInfo}, map[string]any{
		"kind": "interaction_resolved", field: id,
	})
}

type permissionSnapshot struct {
	ID      string `json:"permId"`
	Session string `json:"session"`
	Tool    string `json:"tool"`
	Input   string `json:"input"`
	Reason  string `json:"reason"`
}

type askSnapshot struct {
	ID            string   `json:"askId"`
	Session       string   `json:"session"`
	Question      string   `json:"question"`
	Options       []string `json:"options"`
	AllowFreeform bool     `json:"allowFreeform"`
}

// handleInteractions restores cards when their session is selected or its SSE
// stream reconnects. A snapshot never includes another session's pending work.
func (s *Server) handleInteractions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId is required")
		return
	}
	permissions := make([]permissionSnapshot, 0)
	asks := make([]askSnapshot, 0)
	s.permMu.Lock()
	for id, pending := range s.pendingPerms {
		if pending.session != sessionID {
			continue
		}
		input := pending.event.PermissionInput
		if input == nil {
			input = pending.event.ToolInput
		}
		encoded, _ := json.Marshal(input)
		tool := pending.tool
		if tool == "" {
			tool = pending.event.ToolName
		}
		permissions = append(permissions, permissionSnapshot{
			ID: id, Session: pending.session, Tool: tool,
			Input: truncateSSE(string(encoded), 300), Reason: pending.event.PermissionReason,
		})
	}
	s.permMu.Unlock()
	s.askMu.Lock()
	for id, pending := range s.pendingAsks {
		if pending.session != sessionID {
			continue
		}
		asks = append(asks, askSnapshot{
			ID: id, Session: pending.session, Question: pending.event.AskUserQuestion,
			Options:       append([]string{}, pending.event.AskUserOptions...),
			AllowFreeform: pending.event.AskUserAllowFreeform,
		})
	}
	s.askMu.Unlock()
	sort.Slice(permissions, func(i, j int) bool { return permissions[i].ID < permissions[j].ID })
	sort.Slice(asks, func(i, j int) bool { return asks[i].ID < asks[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"permissions": permissions, "asks": asks})
}

// handlePermission resolves a card only after checking its optional session
// owner. Missing sessionId remains compatible with older Desktop clients.
func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
		Approve   bool   `json:"approve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.permMu.Lock()
	pending, ok := s.pendingPerms[body.ID]
	if ok && body.SessionID != "" && pending.session != body.SessionID {
		s.permMu.Unlock()
		writeError(w, http.StatusConflict, "permission request belongs to another session")
		return
	}
	if ok {
		delete(s.pendingPerms, body.ID)
		finishInteraction(pending.done)
	}
	s.permMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "permission request expired or already resolved")
		return
	}
	decision := agent.PermissionDecisionDeny
	if body.Approve {
		decision = agent.PermissionDecisionAllow
	}
	s.interactionResolved(pending.session, "permId", body.ID)
	replyPermission(pending, decision)
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true})
}

func (s *Server) timeoutPermission(id string) {
	s.permMu.Lock()
	pending, ok := s.pendingPerms[id]
	if ok {
		delete(s.pendingPerms, id)
		finishInteraction(pending.done)
	}
	s.permMu.Unlock()
	if ok {
		s.interactionResolved(pending.session, "permId", id)
		replyPermission(pending, agent.PermissionDecisionDeny)
	}
}

func (s *Server) takePendingAsk(id string) (*askPending, bool) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	pending, ok := s.pendingAsks[id]
	if ok {
		delete(s.pendingAsks, id)
		finishInteraction(pending.done)
	}
	return pending, ok
}

func (s *Server) timeoutAsk(id string) {
	if pending, ok := s.takePendingAsk(id); ok {
		s.interactionResolved(pending.session, "askId", id)
		replyAsk(pending, "")
	}
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
		Answer    string `json:"answer"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.askMu.Lock()
	pending, ok := s.pendingAsks[body.ID]
	if ok && body.SessionID != "" && pending.session != body.SessionID {
		s.askMu.Unlock()
		writeError(w, http.StatusConflict, "question belongs to another session")
		return
	}
	if ok {
		delete(s.pendingAsks, body.ID)
		finishInteraction(pending.done)
	}
	s.askMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "question expired or already answered")
		return
	}
	s.interactionResolved(pending.session, "askId", body.ID)
	replyAsk(pending, body.Answer)
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true})
}

// cancelSessionInteractions releases just one turn's waits. An empty owner is
// reserved for whole-server shutdown, including legacy unowned pending cards.
func (s *Server) cancelSessionInteractions(sessionID string) {
	permissions := make(map[string]*permissionPending)
	s.permMu.Lock()
	for id, pending := range s.pendingPerms {
		if sessionID == "" || pending.session == sessionID {
			permissions[id] = pending
			delete(s.pendingPerms, id)
			finishInteraction(pending.done)
		}
	}
	s.permMu.Unlock()
	for id, pending := range permissions {
		s.interactionResolved(pending.session, "permId", id)
		replyPermission(pending, agent.PermissionDecisionDeny)
	}

	asks := make(map[string]*askPending)
	s.askMu.Lock()
	for id, pending := range s.pendingAsks {
		if sessionID == "" || pending.session == sessionID {
			asks[id] = pending
			delete(s.pendingAsks, id)
			finishInteraction(pending.done)
		}
	}
	s.askMu.Unlock()
	for id, pending := range asks {
		s.interactionResolved(pending.session, "askId", id)
		replyAsk(pending, "")
	}
}

func (s *Server) cancelPendingInteractions() {
	s.cancelSessionInteractions("")
}
