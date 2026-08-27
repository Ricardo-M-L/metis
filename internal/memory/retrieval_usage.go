package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

// MarkRetrieved persists reuse metadata for only the passages that were
// actually attached to a model request. AutoRetrieveCandidates deliberately
// stays side-effect-free so rerank candidates and prompt-size previews are not
// mistaken for genuine memory use.
func (mm *MemoryManager) MarkRetrieved(passages []Passage) error {
	if mm == nil || len(passages) == 0 {
		return nil
	}
	mm.usageMu.Lock()
	defer mm.usageMu.Unlock()
	return withRepositoryLock(mm.root, func() error {
		return mm.markRetrievedLocked(passages)
	})
}

func (mm *MemoryManager) markRetrievedLocked(passages []Passage) error {

	archiveIDs := map[string]struct{}{}
	topicIDs := map[string]struct{}{}
	for _, passage := range passages {
		id := strings.TrimSpace(passage.ID)
		if id == "" {
			continue
		}
		if isTopicPassage(passage) {
			topicIDs[id] = struct{}{}
		} else {
			archiveIDs[id] = struct{}{}
		}
	}
	now := time.Now().UTC()
	var errs []error
	if len(archiveIDs) > 0 && mm.archival != nil {
		errs = append(errs, mm.archival.markRetrieved(archiveIDs, now))
	}
	if len(topicIDs) > 0 {
		errs = append(errs, markTopicPassagesRetrieved(mm.root, topicIDs, now))
	}
	mm.Invalidate()
	return errors.Join(errs...)
}

func isTopicPassage(passage Passage) bool {
	if strings.HasPrefix(passage.ID, "topic-") {
		for _, tag := range passage.Tags {
			if tag == "topic" {
				return true
			}
		}
	}
	return false
}

func (am *ArchivalMemory) markRetrieved(ids map[string]struct{}, now time.Time) error {
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
	stamp := now.Format(time.RFC3339Nano)
	changed := false
	var output strings.Builder
	refreshedIndex := make(map[string]Passage)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		passage, parseErr := parsePassageLine(trimmed)
		if parseErr != nil {
			output.WriteString(line)
			output.WriteByte('\n')
			continue
		}
		if _, ok := ids[passage.ID]; ok {
			passage.LastUsedAt = stamp
			passage.UseCount++
			encoded, marshalErr := json.Marshal(passage)
			if marshalErr != nil {
				return marshalErr
			}
			line = string(encoded)
			changed = true
		}
		refreshedIndex[passage.ID] = passage
		output.WriteString(line)
		output.WriteByte('\n')
	}
	if !changed {
		am.index = refreshedIndex
		return nil
	}
	if err := atomicWriteFile(path, output.String(), 0o600); err != nil {
		return err
	}
	am.index = refreshedIndex
	return am.saveIndex()
}

func markTopicPassagesRetrieved(root string, ids map[string]struct{}, now time.Time) error {
	remaining := make(map[string]struct{}, len(ids))
	for id := range ids {
		remaining[id] = struct{}{}
	}
	var errs []error
	for _, path := range topicPaths(root, false) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		id := topicPassageID(rel)
		if _, ok := remaining[id]; !ok {
			continue
		}
		delete(remaining, id)
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		frontmatter, body, err := memdir.ParseFile(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := frontmatter.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("mark retrieved %s: %w", rel, err))
			continue
		}
		frontmatter.MarkAccessed(now)
		rendered, err := memdir.RenderFile(frontmatter, string(body))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := atomicWriteFile(path, string(rendered), 0o600); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
