package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrResumeUnavailable = errors.New("stopped turn resume token is unavailable or stale")

// TurnLock serializes Desktop turns for one session across server processes.
// The parent owns it through worker exit; the worker must not acquire it again.
// Keep lock files stable: unlinking one while held can create a second lock inode.
type TurnLock struct{ file *os.File }

// TryAcquireTurnLock keeps idle-only Desktop history edits from waiting behind
// or modifying a running worker owned by another Desktop server process.
func (s *Store) TryAcquireTurnLock(id string) (*TurnLock, bool, error) {
	if s == nil || !validDeleteSessionID(id) {
		return nil, false, errors.New("session turn lock: invalid id")
	}
	file, err := openSessionTurnLock(filepath.Join(s.Dir, id+".turn.lock"))
	if err != nil {
		return nil, false, fmt.Errorf("session turn lock: %w", err)
	}
	locked, err := trySessionTurnLock(file)
	if err != nil || !locked {
		_ = file.Close()
		return nil, false, err
	}
	return &TurnLock{file: file}, true, nil
}

func (s *Store) AcquireTurnLock(ctx context.Context, id string) (*TurnLock, error) {
	if s == nil || !validDeleteSessionID(id) {
		return nil, errors.New("session turn lock: invalid id")
	}
	file, err := openSessionTurnLock(filepath.Join(s.Dir, id+".turn.lock"))
	if err != nil {
		return nil, fmt.Errorf("session turn lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		locked, err := trySessionTurnLock(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("session turn lock: %w", err)
		}
		if locked {
			return &TurnLock{file: file}, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *TurnLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockSessionTurn(l.file)
	_ = l.file.Close()
	l.file = nil
}

// StartDesktopTurn must be called with the session TurnLock held, after any
// workspace queue wait. Reloading here makes a queued double-click observe the
// previous claim instead of trusting its pre-queue snapshot.
func (s *Store) StartDesktopTurn(id, token string, resume bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hdr, _, err := s.LoadHeader(id)
	if err != nil {
		return err
	}
	if resume && (hdr.Status != "stopped" || token == "" || hdr.ResumeToken == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(hdr.ResumeToken)) != 1) {
		return ErrResumeUnavailable
	}
	return s.appendEntryLocked(id, Entry{Type: "header", Header: &Header{
		ID: id, Status: "running", ClearResumeToken: true,
	}}, true)
}

// InvalidateDesktopResume clears a Desktop checkpoint when another entry point
// supplies new input, while preserving that entry point's existing CLI status
// semantics. Parent-owned workers may call it again after admission safely.
func (s *Store) InvalidateDesktopResume(id string) error {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hdr, _, err := s.LoadHeader(id)
	if err != nil {
		return err
	}
	if hdr.ResumeToken == "" {
		return nil
	}
	return s.appendEntryLocked(id, Entry{Type: "header", Header: &Header{ID: id, ClearResumeToken: true}}, true)
}

// FinishDesktopTurn grants a new token only after a graceful stop crossed its
// history fsync boundary. A force-killed worker or failed checkpoint is never
// eligible, even when its last persisted status happens to be stopped.
func (s *Store) FinishDesktopTurn(id, status string, checkpointed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hdr := &Header{ID: id, Status: status, ClearResumeToken: true}
	if status == "stopped" && checkpointed {
		_, history, err := s.Load(id)
		if err != nil {
			return err
		}
		if len(history) == 0 {
			return errors.New("stopped turn has no checkpoint history")
		}
		if err := validateCheckpointToolPairs(history); err != nil {
			return err
		}
		var token [24]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		hdr.ResumeToken = hex.EncodeToString(token[:])
		hdr.ClearResumeToken = false
	}
	err := s.appendEntryLocked(id, Entry{Type: "header", Header: hdr}, true)
	if err != nil && hdr.ResumeToken != "" {
		// An fsync error can occur after append made the new token visible.
		// Retract that grant before returning failure so live readers cannot
		// mistake a failed durability boundary for a resumable checkpoint.
		clearErr := s.appendEntryLocked(id, Entry{Type: "header", Header: &Header{ID: id, Status: "failed", ClearResumeToken: true}}, true)
		return errors.Join(err, clearErr)
	}
	return err
}

func DesktopResumeAvailable(hdr *Header) bool {
	return hdr != nil && hdr.Status == "stopped" && hdr.ResumeToken != ""
}
