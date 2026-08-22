package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DeleteSessionHistory removes only history.jsonl rows with an exact session
// id match. The whole read/filter/replace transaction shares historyMu with
// AppendHistory, so an in-process append cannot be lost during the rewrite.
// Malformed rows are retained and reported: their ownership is unknowable.
func DeleteSessionHistory(sessionID string) error {
	if sessionID == "" {
		return errors.New("delete history: empty session id")
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	return filterSessionJSONL(HistoryJSONLPath(), sessionID, func(line []byte) (string, error) {
		var entry HistoryEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return "", err
		}
		return entry.SessionID, nil
	})
}

// DeleteSessionLearned removes only learned.jsonl rows with an exact session
// id match. As with history, corrupt rows fail closed rather than being
// silently dropped or guessed to belong to the target session.
func DeleteSessionLearned(sessionID string) error {
	if sessionID == "" {
		return errors.New("delete learned log: empty session id")
	}
	path, err := learnedPath()
	if err != nil {
		return err
	}
	learnedMu.Lock()
	defer learnedMu.Unlock()
	return filterSessionJSONL(path, sessionID, func(line []byte) (string, error) {
		var entry LearnedRecord
		if err := json.Unmarshal(line, &entry); err != nil {
			return "", err
		}
		return entry.SessionID, nil
	})
}

// filterSessionJSONL validates every non-blank row before changing the file.
// This two-phase shape matters: if even one row has unknown ownership, the
// original file stays byte-for-byte intact and the caller receives an error.
func filterSessionJSONL(path, sessionID string, owner func([]byte) (string, error)) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular JSONL store %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	kept := make([][]byte, 0, len(lines))
	removed := false
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			kept = append(kept, line)
			continue
		}
		ownedBy, err := owner(trimmed)
		if err != nil {
			return fmt.Errorf("inspect %s line %d: %w", filepath.Base(path), i+1, err)
		}
		if ownedBy == sessionID {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return nil
	}
	return atomicReplacePrivateFile(path, bytes.Join(kept, nil))
}

func atomicReplacePrivateFile(path string, body []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	closed := false
	defer func() {
		if !closed {
			closeErr := tmp.Close()
			err = errors.Join(err, closeErr)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	return replaceCurrentPlanFile(tmpPath, path)
}

// DeleteSessionPlans removes the exact current plan (and its interrupted
// writer temps) plus archived plans whose embedded SessionID exactly matches.
// Filename prefixes are only used to report an unreadable candidate; they are
// never sufficient evidence for deletion.
func DeleteSessionPlans(sessionID string) error {
	if sessionID == "" {
		return errors.New("delete plans: empty session id")
	}
	plansMu.Lock()
	defer plansMu.Unlock()

	dir := PlansDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete plans: %w", err)
	}

	var joined error
	remove := func(label, path string) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			joined = errors.Join(joined, fmt.Errorf("delete %s: %w", label, err))
		}
	}

	current := CurrentPlanPath(sessionID)
	currentBase := filepath.Base(current)
	// CurrentPlanPath sanitizes historical/imported ids. Only remove this
	// filename when that mapping is injective for the supplied id; otherwise a
	// different id could share the same current-plan path.
	currentOwned := currentBase == sessionID+".md"
	if currentOwned {
		remove("current plan", current)
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		if entry.Type()&os.ModeSymlink != 0 {
			if strings.HasPrefix(name, sessionID+"_") || (currentOwned && strings.HasPrefix(name, "."+currentBase+".") && strings.HasSuffix(name, ".tmp")) {
				joined = errors.Join(joined, fmt.Errorf("inspect plan %q: refusing symlink", name))
			}
			continue
		}
		if entry.IsDir() {
			continue
		}
		if currentOwned && strings.HasPrefix(name, "."+currentBase+".") && strings.HasSuffix(name, ".tmp") {
			remove("current plan temp", path)
			continue
		}
		if filepath.Ext(name) != ".json" {
			continue
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			if strings.HasPrefix(name, sessionID+"_") {
				joined = errors.Join(joined, fmt.Errorf("inspect archived plan %q: %w", name, readErr))
			}
			continue
		}
		var archived ArchivedPlan
		if decodeErr := json.Unmarshal(body, &archived); decodeErr != nil {
			if strings.HasPrefix(name, sessionID+"_") {
				joined = errors.Join(joined, fmt.Errorf("inspect archived plan %q: %w", name, decodeErr))
			}
			continue
		}
		if archived.SessionID == sessionID {
			remove("archived plan", path)
		}
	}

	if !currentOwned {
		if _, err := os.Lstat(current); err == nil {
			joined = errors.Join(joined, fmt.Errorf("current plan path for session %q is ambiguous; retained", sessionID))
		} else if !errors.Is(err, fs.ErrNotExist) {
			joined = errors.Join(joined, fmt.Errorf("inspect current plan: %w", err))
		}
	}
	return joined
}
