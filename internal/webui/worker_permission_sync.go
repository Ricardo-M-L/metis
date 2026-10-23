package webui

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func (s *Server) markWorkerPermissionOwner(id string) {
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	s.markWorkerPermissionOwnerLocked(id)
}

func (s *Server) markWorkerPermissionOwnerLocked(id string) {
	if s.workerPermissionOwned == nil {
		s.workerPermissionOwned = make(map[string]bool)
	}
	s.workerPermissionOwned[id] = true
}

func (s *Server) acceptWorkerStatus(id string, owner *isolatedActiveTurn, status desktopipc.Status) {
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	if s.workerTurns[id] != owner {
		return
	}
	if p := status.Permission; p != nil && (!p.Valid() || p.SessionID != id) {
		return
	}
	owner.permission = nil
	if status.Permission != nil {
		copy := *status.Permission
		owner.permission = &copy
	}
	s.setWorkerSnapshotLocked(id, status)
}

func (s *Server) liveWorkerPermission(id string) (*desktopipc.PermissionState, bool) {
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	owner := s.workerTurns[id]
	if owner == nil {
		return nil, false
	}
	if owner.permission == nil {
		return nil, true
	}
	copy := *owner.permission
	return &copy, true
}

// A detached worker advances the durable header while the selected parent
// Loop may still belong to the same session with its old gate. Suppress stale
// parent saves until an explicit activation/settings boundary has adopted the
// durable posture. Running workers retain ownership even after navigation.
func (s *Server) syncWorkerPermissionState(id string) error {
	if id == "" || s.loop == nil || s.loop.Gate == nil {
		return nil
	}
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	if !s.workerPermissionOwned[id] {
		return nil
	}
	var hdr *session.Header
	if owner := s.workerTurns[id]; owner != nil {
		if owner.permission == nil {
			hdr = &session.Header{Mode: string(permission.ModePlan), PrePlanMode: string(permission.ModeDefault)}
		} else {
			hdr = &session.Header{Mode: owner.permission.Mode, PrePlanMode: owner.permission.PrePlanMode}
		}
	} else {
		var err error
		hdr, _, err = s.store.LoadHeader(id)
		if err != nil || hdr == nil {
			return fmt.Errorf("load worker permission state: %w", err)
		}
	}
	mode, prePlan, _, err := rtpkg.ValidateRestoredPermissionStateFromSource(hdr.Mode, hdr.PrePlanMode, s.freshPermissionMode, s.trustSessionPermissions)
	if err != nil {
		return err
	}
	if s.preflightPermissionMode != nil {
		if err := s.preflightPermissionMode(mode, prePlan); err != nil {
			return err
		}
	} else if err := rtpkg.PreflightRestoredPermissionState(nil, mode, prePlan); err != nil {
		return err
	}
	if err := rtpkg.RestorePermissionModeState(rtpkg.PermissionModeState{Mode: mode, PrePlanMode: prePlan}, s.applyPermissionMode); err != nil {
		return err
	}
	if s.workerTurns[id] == nil {
		delete(s.workerPermissionOwned, id)
	}
	return nil
}

func (s *Server) permissionStateFromHeader(hdr *session.Header) (permission.Mode, string, error) {
	s.stateMu.RLock()
	fallback := s.freshPermissionMode
	s.stateMu.RUnlock()
	mode, prePlan, _, err := rtpkg.ValidateRestoredPermissionStateFromSource(hdr.Mode, hdr.PrePlanMode, fallback, s.trustSessionPermissions)
	return mode, prePlan, err
}

func (s *Server) addPermissionStatus(payload map[string]any, id string) {
	payload["permissionSessionId"] = id
	payload["permissionModeAvailable"] = false
	if id == "" {
		s.stateMu.RLock()
		mode := s.freshPermissionMode
		s.stateMu.RUnlock()
		if _, ok := permission.ParseMode(string(mode)); ok {
			payload["permissionMode"], payload["permissionModeAvailable"] = string(mode), true
		}
		return
	}
	if s.store == nil {
		return
	}
	var hdr *session.Header
	if live, active := s.liveWorkerPermission(id); active {
		if live == nil {
			return
		}
		hdr = &session.Header{Mode: live.Mode, PrePlanMode: live.PrePlanMode}
	} else {
		var err error
		hdr, _, err = s.store.LoadHeader(id)
		if err != nil || hdr == nil {
			return
		}
	}
	mode, prePlan, err := s.permissionStateFromHeader(hdr)
	if err != nil {
		return
	}
	payload["permissionMode"], payload["permissionPrePlanMode"], payload["permissionModeAvailable"] = string(mode), prePlan, true
}

// Explicit user permission changes cross their own fsync boundary. Ordinary
// metadata persistence stays unchanged, and a failed fsync rolls this complete
// posture back before saveSettings restores the live gate.
func (s *Server) persistActiveSessionPermissionState() error {
	s.stateMu.RLock()
	id := s.activeSessionID
	s.stateMu.RUnlock()
	if id == "" || s.store == nil || s.loop == nil || s.loop.Gate == nil {
		return nil
	}
	state, err := rtpkg.CapturePermissionModeState(s.loop.Gate, s.loop)
	if err != nil {
		return err
	}
	return s.store.CheckpointPermissionState(id, string(state.Mode), state.PrePlanMode)
}
