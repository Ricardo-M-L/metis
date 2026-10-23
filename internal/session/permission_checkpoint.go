package session

import (
	"errors"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// CheckpointPermissionState commits a complete permission posture before a
// worker can execute under it. The existing durable append primitive rolls
// back the header on an fsync failure; WriteHeaderFull followed by Sync would
// leave a failed elevation visible to the next process.
func (s *Store) CheckpointPermissionState(id, mode, prePlan string) error {
	parsed, ok := permission.ParseMode(mode)
	if !ok || string(parsed) != mode || !validDeleteSessionID(id) {
		return errors.New("permission checkpoint requires a session and canonical mode")
	}
	if parsed == permission.ModePlan {
		previous, ok := permission.ParseMode(prePlan)
		if !ok || previous == permission.ModePlan || string(previous) != prePlan {
			return errors.New("permission checkpoint requires explicit plan lineage")
		}
	} else if prePlan != "" {
		return errors.New("permission checkpoint has lineage outside plan mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendEntryLocked(id, Entry{Type: "header", Header: &Header{ID: id, Mode: mode, PrePlanMode: prePlan}}, true)
}
