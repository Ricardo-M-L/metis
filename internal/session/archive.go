package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sessionArchiveDir = ".archive"

// ArchiveInfo is recoverable session-lifecycle metadata. The transcript
// remains in its original JSONL file; archiving only changes whether normal
// resume catalogs include it.
type ArchiveInfo struct {
	SessionID  string    `json:"session_id"`
	ArchivedAt time.Time `json:"archived_at"`
}

func validArchiveSessionID(id string) bool {
	return strings.TrimSpace(id) != "" && id != "." && id != ".." &&
		filepath.Base(id) == id && !strings.ContainsAny(id, `/\`)
}

func (s *Store) archivePath(id string) string {
	return filepath.Join(s.Dir, sessionArchiveDir, id+".json")
}

// Archive hides a session from default resume lists without moving or
// deleting its transcript. Repeating the operation is idempotent.
func (s *Store) Archive(id string) error {
	if s == nil || !validArchiveSessionID(id) {
		return errors.New("archive session: invalid id")
	}
	if _, _, err := s.LoadHeader(id); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	dir := filepath.Join(s.Dir, sessionArchiveDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	if _, err := os.Stat(s.archivePath(id)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("archive session: %w", err)
	}
	data, err := json.Marshal(ArchiveInfo{SessionID: id, ArchivedAt: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".archive-*.tmp")
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("archive session: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("archive session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("archive session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	if err := os.Rename(tmpName, s.archivePath(id)); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	return nil
}

// Unarchive restores a session to normal resume lists. It is idempotent.
func (s *Store) Unarchive(id string) error {
	if s == nil || !validArchiveSessionID(id) {
		return errors.New("unarchive session: invalid id")
	}
	if err := os.Remove(s.archivePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unarchive session: %w", err)
	}
	return nil
}

// IsArchived reports whether a session has recoverable archive metadata.
func (s *Store) IsArchived(id string) bool {
	if s == nil || !validArchiveSessionID(id) {
		return false
	}
	_, err := os.Stat(s.archivePath(id))
	return err == nil
}
