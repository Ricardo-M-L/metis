package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type desktopRewindPoint struct {
	Turn                  int    `json:"turn"`
	Prompt                string `json:"prompt"`
	HasCodeCheckpoint     bool   `json:"hasCodeCheckpoint"`
	CodeAvailable         bool   `json:"codeAvailable"`
	CodeUnavailableReason string `json:"codeUnavailableReason,omitempty"`
	LatestEdit            bool   `json:"latestEdit"`
}

type desktopRewindCheckpoint struct {
	checkpoint.ControlStatus
	Sparse bool `json:"sparse"`
}

// rewindReader is intentionally detached from the running provider/runtime.
// Inspecting or restoring B must never activate B over the currently viewed A,
// replace A's provider settings, or cancel A's background continuation.
func rewindReader(id, workspace string, history []llm.Message) *agent.Loop {
	loop := agent.NewLoop(nil, nil, nil, nil, "", 1)
	loop.Restore(history)
	loop.SetCheckpointer(checkpoint.NewManager(id, workspace, ""))
	_ = loop.LoadRewindState() // Failure is represented by unavailable code points.
	return loop
}

func desktopRewindPoints(loop *agent.Loop) []desktopRewindPoint {
	points := make([]desktopRewindPoint, 0)
	for _, point := range loop.RewindPoints() {
		points = append(points, desktopRewindPoint{Turn: point.Turn, Prompt: point.Prompt,
			HasCodeCheckpoint: point.HasCodeCheckpoint, CodeAvailable: point.HasCodeCheckpoint,
			CodeUnavailableReason: point.CodeUnavailableReason, LatestEdit: point.LatestEdit})
	}
	return points
}

// rejectRewindChild protects this restore endpoint without changing the input
// policy of unrelated Desktop routes. Child transcripts remain parent-managed.
func (s *Server) rejectRewindChild(w http.ResponseWriter, id string, header *session.Header) bool {
	parent := header.SubAgentOf
	owned := parent != ""
	if !owned {
		child, err := agent.LoadSubAgentHeader(s.store.Dir, id)
		if err == nil {
			parent, owned = child.SubAgentOf, true
		} else if !errors.Is(err, os.ErrNotExist) {
			path := filepath.Join(s.store.Dir, agent.SubAgentTranscriptDirname, id+".jsonl")
			if _, statErr := os.Lstat(path); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
				owned = true
			}
		}
	}
	if !owned && s.roster != nil {
		child, ok := s.roster.LookupByAgentID(id)
		owned = ok && child != nil
	}
	if !owned {
		s.workerTurnsMu.Lock()
		for parentID, snapshot := range s.workerSnapshots {
			for _, child := range snapshot.status.Agents {
				if child.AgentID == id {
					parent, owned = parentID, true
					break
				}
			}
			if _, ok := snapshot.unobserved[id]; ok {
				parent, owned = parentID, true
			}
			if owned {
				break
			}
		}
		s.workerTurnsMu.Unlock()
	}
	if !owned {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":     "this sub-agent is managed by its parent; rewind the parent conversation instead",
		"sessionId": id, "parentSessionId": parent, "ownership": "parent",
	})
	return true
}

func (s *Server) rewindHasActiveChildren(id, workspace string) bool {
	target := canonicalTurnWorkspace(workspace)
	var parents []string
	s.workerTurnsMu.Lock()
	for parent, snapshot := range s.workerSnapshots {
		active := snapshot.status.SubAgents > 0
		for _, child := range snapshot.status.Agents {
			active = active || subAgentStatusActive(child.Status) || child.HoldsExecutionSlot
		}
		for _, child := range snapshot.unobserved {
			active = active || subAgentStatusActive(child.Status) || child.HoldsExecutionSlot
		}
		if active {
			parents = append(parents, parent)
		}
	}
	s.workerTurnsMu.Unlock()
	for _, parent := range parents {
		if parent == id {
			return true
		}
		header, _, err := s.store.LoadHeader(parent)
		if err != nil || header == nil || header.WorkDir == "" || turnWorkspacesOverlap(target, canonicalTurnWorkspace(header.WorkDir)) {
			return true
		}
	}
	if s.roster != nil {
		for _, child := range s.roster.List() {
			if child == nil {
				continue
			}
			status := child.Snapshot()
			if status.Status != agent.StatusRunning && status.Status != agent.StatusQueued && !status.HoldsExecutionSlot {
				continue
			}
			header, err := agent.LoadSubAgentHeader(s.store.Dir, child.AgentID)
			// Legacy/unreadable live identities cannot prove a disjoint write
			// scope. Known independent projects keep their normal admission.
			if err != nil || header.SubAgentOf == id || header.WorkDir == "" || turnWorkspacesOverlap(target, canonicalTurnWorkspace(header.WorkDir)) {
				return true
			}
		}
	}
	return false
}

func (s *Server) adoptRewindOutcome(id string, reader *agent.Loop, history []llm.Message) {
	s.cancelMu.Lock()
	if s.backgroundSession == id {
		s.stopBackgroundContinuationLocked()
	}
	s.cancelMu.Unlock()
	s.stateMu.RLock()
	active := s.activeSessionID == id
	s.stateMu.RUnlock()
	if active && s.loop != nil {
		s.contextMu.Lock()
		s.loop.Restore(history)
		s.loop.SetCheckpointer(reader.Checkpointer)
		_ = s.loop.LoadRewindState()
		s.contextMu.Unlock()
	}
}

// The revision binds a panel to the complete transcript, workspace, controls
// and exact rewind journal. A stale browser tab cannot reuse turn numbers from
// a different history lifetime. Invalid metadata remains a stable unavailable
// state, without following symlinks or reading unbounded sidecar contents.
func desktopRewindRevision(id, workspace string, history []llm.Message, manager *checkpoint.Manager) string {
	hash := sha256.New()
	_ = json.NewEncoder(hash).Encode(struct {
		Session   string
		Workspace string
		History   []llm.Message
		Control   checkpoint.ControlStatus
	}{id, canonicalTurnWorkspace(workspace), history, manager.ControlStatus()})
	path := manager.RewindMetadataPath()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		_, _ = io.WriteString(hash, "rewind metadata unavailable")
	} else if file, openErr := os.Open(path); openErr == nil {
		actual, statErr := file.Stat()
		if statErr == nil && os.SameFile(info, actual) {
			_, _ = io.Copy(hash, io.LimitReader(file, (8<<20)+1))
		}
		_ = file.Close()
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "session store unavailable")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Turn      int    `json:"turn"`
		Mode      string `json:"mode"`
		Revision  string `json:"revision"`
	}
	body.SessionID = r.URL.Query().Get("sessionId")
	if r.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid rewind request")
			return
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid rewind request")
			return
		}
	}
	if !validSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, "valid sessionId is required")
		return
	}
	header, history, err := s.store.Load(body.SessionID)
	if err != nil || header == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if s.rejectRewindChild(w, body.SessionID, header) {
		return
	}
	if strings.TrimSpace(header.WorkDir) == "" {
		writeError(w, http.StatusConflict, "session has no workspace")
		return
	}
	if r.Method == http.MethodGet {
		reader := rewindReader(body.SessionID, header.WorkDir, history)
		points := desktopRewindPoints(reader)
		if s.rewindHasActiveChildren(body.SessionID, header.WorkDir) {
			for index := range points {
				points[index].CodeAvailable = false
				points[index].CodeUnavailableReason = "子代理仍在运行，请停止后再恢复代码"
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": body.SessionID,
			"revision": desktopRewindRevision(body.SessionID, header.WorkDir, history, reader.Checkpointer),
			"points":   points, "checkpoint": desktopRewindCheckpoint{reader.Checkpointer.ControlStatus(), reader.Checkpointer.Sparse()}})
		return
	}
	scope := agent.RewindConversation
	switch body.Mode {
	case "conversation":
	case "code":
		scope = agent.RewindCode
	case "both":
		scope = agent.RewindCodeAndConversation
	default:
		writeError(w, http.StatusBadRequest, "mode must be conversation, code, or both")
		return
	}
	if body.Turn < 1 || len(body.Revision) != sha256.Size*2 {
		writeError(w, http.StatusBadRequest, "turn and revision from the rewind panel are required")
		return
	}
	turnLock, acquired, err := s.store.TryAcquireTurnLock(body.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session lock failed: "+err.Error())
		return
	}
	if !acquired {
		writeError(w, http.StatusConflict, "a turn is running; stop it before rewinding")
		return
	}
	defer turnLock.Release()
	if !s.runMu.TryLock() {
		writeError(w, http.StatusConflict, "a turn is running; stop it before rewinding")
		return
	}
	defer s.runMu.Unlock()
	var lease *TurnLease
	if scope&agent.RewindCode != 0 {
		lease, acquired = s.turnCoordinator.TryAcquireRewind(header.WorkDir)
	} else {
		lease, acquired = s.turnCoordinator.TryAcquire(header.WorkDir)
	}
	if !acquired {
		writeError(w, http.StatusConflict, "workspace is busy; stop its running turn before rewinding")
		return
	}
	defer lease.Release()
	// The cross-process lock and workspace admission own this fresh load.
	header, history, err = s.store.Load(body.SessionID)
	if err != nil || header == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if s.rejectRewindChild(w, body.SessionID, header) {
		return
	}
	if canonicalTurnWorkspace(header.WorkDir) != lease.workspace {
		writeError(w, http.StatusConflict, "session workspace changed; reopen the rewind panel")
		return
	}
	reader := rewindReader(body.SessionID, header.WorkDir, history)
	if scope&agent.RewindCode != 0 && s.rewindHasActiveChildren(body.SessionID, header.WorkDir) {
		writeError(w, http.StatusConflict, "子代理仍在运行，请停止后再恢复代码")
		return
	}
	if body.Revision != desktopRewindRevision(body.SessionID, header.WorkDir, history, reader.Checkpointer) {
		writeError(w, http.StatusConflict, "conversation or checkpoints changed; reopen the rewind panel")
		return
	}
	var selected *desktopRewindPoint
	for _, point := range desktopRewindPoints(reader) {
		if point.Turn == body.Turn {
			copy := point
			selected = &copy
			break
		}
	}
	if selected == nil {
		writeError(w, http.StatusBadRequest, "selected conversation point no longer exists")
		return
	}
	if scope&agent.RewindCode != 0 && !selected.CodeAvailable {
		reason := selected.CodeUnavailableReason
		if reason == "" {
			reason = "code checkpoint is unavailable; conversation-only rewind remains available"
		}
		writeError(w, http.StatusConflict, reason)
		return
	}
	// Code-only restore also changes the stopped task's world state: an old
	// resume token must not replay a continuation against rolled-back files.
	if scope == agent.RewindCode {
		if err := s.store.InvalidateDesktopResume(body.SessionID); err != nil {
			writeError(w, http.StatusInternalServerError, "resume invalidation failed: "+err.Error())
			return
		}
	}
	cursor := session.NewHistoryCursor(history)
	persistedHistory := history
	originalReaderHistory := reader.History()
	result, err := reader.RewindToTurnWithPersist(body.Turn, scope, func(next []llm.Message) error {
		// Restore repairs legacy orphaned tool results in memory. If a file
		// failure asks us to compensate, restore the exact original durable
		// history rather than accidentally committing that unrelated repair.
		if reflect.DeepEqual(next, originalReaderHistory) {
			next = history
		}
		if err := s.store.InvalidateDesktopResume(body.SessionID); err != nil {
			return err
		}
		// Reuse the durable replacement transaction: failed fsync truncates the
		// uncommitted history_replace, rather than leaving Load ahead of files.
		if err := s.store.CheckpointCompaction(body.SessionID, persistedHistory, next, &cursor); err != nil {
			return err
		}
		persistedHistory = next
		return nil
	})
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, agent.ErrInvalidRewindPoint) || errors.Is(err, agent.ErrInvalidRewindScope) {
			status = http.StatusBadRequest
		}
		_, actual, loadErr := s.store.Load(body.SessionID)
		partial := result.CodeRestored || result.ConversationRestored || (loadErr == nil && !reflect.DeepEqual(history, actual))
		if partial {
			if loadErr == nil {
				s.adoptRewindOutcome(body.SessionID, reader, actual)
			}
			writeJSON(w, status, map[string]any{"sessionId": body.SessionID, "error": "文件或对话已恢复，但恢复记录未完整保存，请刷新并检查当前状态：" + err.Error(), "partial": true, "reloadRequired": true, "codeRestored": result.CodeRestored, "conversationRestored": result.ConversationRestored})
		} else {
			writeError(w, status, "rewind failed: "+err.Error())
		}
		return
	}
	s.adoptRewindOutcome(body.SessionID, reader, reader.History())
	prefill := ""
	if result.ConversationRestored {
		prefill = result.Prompt
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": body.SessionID, "changed": true, "mode": body.Mode,
		"messages": reader.History(), "prefill": prefill, "result": map[string]any{
			"label": result.Label, "codeRestored": result.CodeRestored, "conversationRestored": result.ConversationRestored, "turnsUndone": result.TurnsUndone}})
}
