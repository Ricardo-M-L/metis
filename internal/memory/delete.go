package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

// DeleteSession forgets only memory records explicitly attributed to the
// supplied session. Shared Core blocks and unattributed durable memories are
// intentionally preserved.
func (mm *MemoryManager) DeleteSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if mm == nil || sessionID == "" {
		return nil
	}
	if err := validatePersistedMetadata(sessionID); err != nil {
		return err
	}
	mm.usageMu.Lock()
	defer mm.usageMu.Unlock()
	return withRepositoryLock(mm.root, func() error {
		// Persist the deletion boundary before touching any tier. Even if one
		// corrupt file makes cleanup incomplete, a late writer cannot resurrect
		// data for this source session.
		if err := markSessionDeletedLocked(mm.root, sessionID); err != nil {
			return err
		}
		var errs []error
		if mm.recall != nil {
			errs = append(errs, mm.recall.deleteSessionLocked(sessionID))
		}
		if mm.daily != nil {
			errs = append(errs, mm.daily.deleteSessionLocked(sessionID))
		}
		if mm.archival != nil {
			errs = append(errs, mm.archival.deleteBySourceSessionLocked(sessionID))
		}
		errs = append(errs, deleteTopicFilesBySessionLocked(mm.root, sessionID))
		mm.Invalidate()
		return errors.Join(errs...)
	})
}

func (rm *RecallMemory) DeleteSession(sessionID string) error {
	if rm == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if err := validatePersistedMetadata(sessionID); err != nil {
		return err
	}
	repositoryRoot := repositoryRootForTier(rm.root, "recall")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := markSessionDeletedLocked(repositoryRoot, strings.TrimSpace(sessionID)); err != nil {
			return err
		}
		return rm.deleteSessionLocked(strings.TrimSpace(sessionID))
	})
}

func (rm *RecallMemory) deleteSessionLocked(sessionID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if err := rm.reloadMessagesLocked(true); err != nil {
		return err
	}
	oldMessages := append([]Message(nil), rm.messages...)
	oldSessions := append([]Session(nil), rm.sessions...)
	filtered := rm.messages[:0]
	for _, message := range rm.messages {
		if message.SessionID != sessionID {
			filtered = append(filtered, message)
		}
	}
	rm.messages = filtered
	sessions := rm.sessions[:0]
	for _, session := range rm.sessions {
		if session.ID != sessionID {
			sessions = append(sessions, session)
		}
	}
	rm.sessions = sessions
	if err := rm.saveMessages(); err != nil {
		rm.messages = oldMessages
		rm.sessions = oldSessions
		return err
	}
	if err := rm.saveSessions(); err != nil {
		rm.messages = oldMessages
		rm.sessions = oldSessions
		return errors.Join(err, rm.saveMessages())
	}
	return nil
}

func (ds *DailyStore) DeleteSession(sessionID string) error {
	if ds == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if err := validatePersistedMetadata(sessionID); err != nil {
		return err
	}
	repositoryRoot := repositoryRootForTier(ds.root, "daily")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := markSessionDeletedLocked(repositoryRoot, strings.TrimSpace(sessionID)); err != nil {
			return err
		}
		return ds.deleteSessionLocked(strings.TrimSpace(sessionID))
	})
}

func (ds *DailyStore) deleteSessionLocked(sessionID string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	entries, err := os.ReadDir(ds.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(ds.root, entry.Name())
		note, err := parseDailyNoteFile(path, entry)
		if err != nil {
			errs = append(errs, fmt.Errorf("parse daily note %s: %w", entry.Name(), err))
			continue
		}
		if note.SessionID == sessionID {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (am *ArchivalMemory) DeleteBySourceSession(sessionID string) error {
	if am == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if err := validatePersistedMetadata(sessionID); err != nil {
		return err
	}
	repositoryRoot := repositoryRootForTier(am.root, "archival")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := markSessionDeletedLocked(repositoryRoot, strings.TrimSpace(sessionID)); err != nil {
			return err
		}
		return am.deleteBySourceSessionLocked(strings.TrimSpace(sessionID))
	})
}

func (am *ArchivalMemory) deleteBySourceSessionLocked(sessionID string) error {
	am.mu.Lock()
	defer am.mu.Unlock()
	path := filepath.Join(am.root, "passages.jsonl")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keptLines := make([]string, 0)
	kept := make([]Passage, 0)
	removed := false
	var errs []error
	for lineNumber, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, err := parsePassageLine(line)
		if err != nil {
			// Preserve corrupt bytes, but fail closed: deletion must not report
			// success when ownership could not be determined.
			keptLines = append(keptLines, line)
			errs = append(errs, fmt.Errorf("parse archival passage line %d: %w", lineNumber+1, err))
			continue
		}
		if p.SourceSessionID == sessionID {
			removed = true
			continue
		}
		kept = append(kept, p)
		keptLines = append(keptLines, line)
	}
	if !removed {
		return errors.Join(errs...)
	}
	var body strings.Builder
	am.index = make(map[string]Passage, len(kept))
	for _, line := range keptLines {
		body.WriteString(line)
		body.WriteByte('\n')
	}
	for _, p := range kept {
		am.index[p.ID] = p
	}
	if err := atomicWriteFile(path, body.String(), 0o600); err != nil {
		return errors.Join(append(errs, err)...)
	}
	if err := am.saveIndex(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func deleteTopicFilesBySessionLocked(root, sessionID string) error {
	var errs []error
	reserved := map[string]struct{}{
		FileMemMemory: {}, FileMemUser: {}, FileMemSystem: {}, FileMemWorking: {}, FileMemSummary: {},
	}
	for _, dir := range []string{root, filepath.Join(root, "topics")} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changed := false
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == memdir.ENTRYPOINT_NAME || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			if dir == root {
				if _, ok := reserved[entry.Name()]; ok {
					continue
				}
			}
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// Root can contain unrelated Markdown documents. Only a file that
			// advertises topic frontmatter is owned by the repository; once it
			// does, parse failures are deletion failures rather than silent skips.
			if !strings.HasPrefix(strings.TrimSpace(string(raw)), "---") {
				continue
			}
			fm, _, err := memdir.ParseFile(raw)
			if err == nil && fm != nil {
				err = fm.Validate()
			}
			if err != nil || fm == nil {
				if err == nil {
					err = errors.New("missing topic frontmatter")
				}
				errs = append(errs, fmt.Errorf("parse topic %s: %w", path, err))
				continue
			}
			if fm.OriginSessionID == sessionID {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				} else {
					changed = true
				}
			}
		}
		if changed {
			files, err := memdir.ScanMemoryFiles(context.Background(), dir)
			if err == nil {
				err = memdir.WriteIndex(dir, files)
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
