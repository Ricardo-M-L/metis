package memory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MarkRetrieved records only passages actually attached to a model request.
// Candidate expansion and previews remain side-effect free.
func (mm *MemoryManager) MarkRetrieved(passages []Passage) error {
	return mm.MarkRetrievedEvent(generateID(), passages)
}

// MarkRetrievedEvent is an optional capability for request-scoped idempotency.
// Retrying the same event and selection never increments a counter twice.
func (mm *MemoryManager) MarkRetrievedEvent(eventID string, passages []Passage) error {
	if mm == nil || len(passages) == 0 {
		return nil
	}
	if strings.TrimSpace(eventID) == "" {
		return errors.New("memory: empty usage event id")
	}
	if err := validatePersistedMetadata(eventID); err != nil {
		return err
	}
	mm.usageMu.Lock()
	defer mm.usageMu.Unlock()
	return withRepositoryLock(mm.root, func() error { return mm.markRetrievedEventLocked(eventID, passages) })
}

func (mm *MemoryManager) markRetrievedLocked(passages []Passage) error {
	return mm.markRetrievedEventLocked(generateID(), passages)
}

// Caller holds the repository lock. The authoritative sources establish
// existence; ledger entries can never recreate a removed passage.
func (mm *MemoryManager) markRetrievedEventLocked(eventID string, passages []Passage) error {
	wanted := make(map[string][]Passage, len(passages))
	needArchive, needTopics := false, false
	for _, p := range passages {
		if strings.TrimSpace(p.ID) == "" {
			continue
		}
		wanted[p.ID] = append(wanted[p.ID], p)
		if isTopicPassage(p) {
			needTopics = true
		} else {
			needArchive = true
			if strings.HasPrefix(p.ID, "topic-") {
				needTopics = true
			}
		}
	}
	var current []Passage
	if needArchive && mm.archival != nil {
		mm.archival.mu.Lock()
		archived, err := mm.archival.readPassagesLocked(false)
		mm.archival.mu.Unlock()
		if err != nil {
			return err
		}
		current = append(current, archived...)
	}
	if needTopics {
		topics, err := mm.loadTopicDocumentsCached()
		if err != nil {
			return err
		}
		for _, doc := range topics {
			current = append(current, doc.passage)
		}
	}
	entries := make([]usageEntry, 0, len(wanted))
	seen := make(map[string]bool, len(wanted))
	for _, p := range current {
		receipts, requested := wanted[p.ID]
		if !requested {
			continue
		}
		for _, receipt := range receipts {
			if receipt.CreatedAt != "" && receipt.CreatedAt != p.CreatedAt || receipt.SourceSessionID != "" && receipt.SourceSessionID != p.SourceSessionID || receipt.Scope != "" && receipt.Scope != p.Scope {
				return fmt.Errorf("memory: retrieved passage %s identity changed", p.ID)
			}
		}
		if !mm.passageVisible(p) {
			return fmt.Errorf("memory: retrieved passage %s is outside this workspace", p.ID)
		}
		deleted, err := sessionDeletedLocked(mm.root, p.SourceSessionID)
		if err != nil {
			return err
		}
		if deleted {
			continue
		}
		key := passageUsageKey(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, usageEntry{Key: key})
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return mm.usage.appendLocked(usageEvent{Version: 1, ID: eventID, At: time.Now().UTC().Format(time.RFC3339Nano), Entries: entries})
}

func (mm *MemoryManager) decorateUsage(passages []Passage) error {
	if mm == nil {
		return nil
	}
	return mm.usage.decorate(passages)
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
