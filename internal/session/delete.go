package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Delete removes the transcript and every sidecar owned directly by id.
// It is idempotent: deleting an already absent session succeeds. Callers are
// responsible for ensuring the session is not active before invoking it.
func (s *Store) Delete(id string) error {
	if s == nil || !validDeleteSessionID(id) {
		return errors.New("delete session: invalid id")
	}

	var joined error
	remove := func(label, path string) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, fmt.Errorf("delete session %s: %w", label, err))
		}
	}

	remove("timing", s.timingPath(id))
	remove("message metrics", s.messageMetricsPath(id))
	remove("cost", s.costPath(id))
	remove("cost temp", s.costPath(id)+".tmp")
	remove("archive marker", s.archivePath(id))
	remove("tag", filepath.Join(s.Dir, "tags", id+".txt"))

	if err := s.deleteNamedSnapshots(id); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := s.deleteSubagentTranscripts(id); err != nil {
		joined = errors.Join(joined, err)
	}
	if joined != nil {
		// Keep the canonical transcript discoverable when a sidecar could
		// not be removed, so callers can retry instead of leaving private
		// remnants behind an apparently successful deletion.
		return joined
	}
	remove("transcript", s.path(id))
	return joined
}

func validDeleteSessionID(id string) bool {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *Store) deleteNamedSnapshots(id string) error {
	dir := filepath.Join(s.Dir, "snapshots")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete session snapshots: %w", err)
	}

	prefix := id + "-"
	var joined error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		snapshotName := name[len(prefix) : len(name)-len(".json")]
		belongs, readErr := snapshotBelongsTo(filepath.Join(dir, name), id, snapshotName)
		if readErr != nil {
			// This filename is a candidate for id, but a corrupt payload has
			// no trustworthy ownership boundary. Retain it and fail the
			// deletion so the canonical transcript remains visible/retryable.
			joined = errors.Join(joined, fmt.Errorf("inspect session snapshot %q: %w", name, readErr))
			continue
		}
		if !belongs {
			// IDs can share prefixes ("sess" and "sess-extra"). The
			// embedded header is the authoritative ownership boundary.
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			joined = errors.Join(joined, fmt.Errorf("delete session snapshot %q: %w", name, err))
		}
	}
	return joined
}

func snapshotBelongsTo(path, sessionID, snapshotName string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var payload struct {
		Header *Header `json:"header"`
	}
	if err := json.NewDecoder(f).Decode(&payload); err != nil {
		return false, err
	}
	return payload.Header != nil && payload.Header.ID == sessionID+"-snapshot-"+snapshotName, nil
}

// deleteSubagentTranscripts removes only files whose first JSONL record is a
// valid header with an exact SubAgentOf match. Corrupt and unrelated files are
// deliberately retained; ordinary ForkedFrom branches live outside this
// directory and are never cascaded.
func (s *Store) deleteSubagentTranscripts(id string) error {
	dir := filepath.Join(s.Dir, "subagents")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete session subagents: %w", err)
	}

	var joined error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		matches, readErr := subagentBelongsTo(path, id)
		if readErr != nil {
			// A malformed transcript has no trustworthy ownership boundary.
			// Keep it rather than risking deletion of another session's data,
			// and report a partial failure instead of claiming all private
			// remnants were removed.
			joined = errors.Join(joined, fmt.Errorf("inspect session subagent %q: %w", entry.Name(), readErr))
			continue
		}
		if !matches {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, fmt.Errorf("delete session subagent %q: %w", entry.Name(), err))
		}
	}
	return joined
}

func subagentBelongsTo(path, parentID string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024), 256*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, err
		}
		return false, errors.New("empty subagent transcript")
	}
	var first struct {
		Type   string  `json:"type"`
		Header *Header `json:"header"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &first); err != nil {
		return false, err
	}
	return first.Type == "header" && first.Header != nil && first.Header.SubAgentOf == parentID, nil
}
