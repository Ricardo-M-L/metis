package agent

import (
	"encoding/json"
	"fmt"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"strings"

	"encoding/base64"
	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type memorySnapshotGuard interface {
	GuardRecallSnapshot(func(string, string, []memory.Passage) error) error
}
type guardedMemorySnapshot struct {
	generation, body string
	passages         []memory.Passage
	err              error
}

// The repository lock covers validation, historical sanitization and cloning
// the immutable request. That clone is the request's memory linearization point;
// mutations completing afterwards are visible to the following request, not a
// retroactive cancellation of already accepted provider input.
func (l *Loop) withMemoryGuardLocked(consume func()) {
	guard, ok := l.Memory.(memorySnapshotGuard)
	if !ok {
		consume()
		return
	}
	called := false
	err := guard.GuardRecallSnapshot(func(g, b string, p []memory.Passage) error {
		l.memoryGuardSnapshot = &guardedMemorySnapshot{generation: g, body: b, passages: p}
		defer func() { l.memoryGuardSnapshot = nil }()
		called = true
		consume()
		return nil
	})
	if err != nil && !called {
		l.memoryGuardSnapshot = &guardedMemorySnapshot{err: err}
		defer func() { l.memoryGuardSnapshot = nil }()
		consume()
	}
}
func (l *Loop) memorySnapshotLocked() (string, string, bool) {
	if s := l.memoryGuardSnapshot; s != nil {
		return s.generation, s.body, s.err == nil
	}
	return recallSnapshot(l.Memory)
}
func (l *Loop) memoryPassagesLocked() ([]memory.Passage, error) {
	if s := l.memoryGuardSnapshot; s != nil {
		return s.passages, s.err
	}
	return l.Memory.Search(memory.SearchOptions{SortBy: "recent"})
}
func (l *Loop) memoryCandidatesLocked(query string, k int) []memory.Passage {
	if s := l.memoryGuardSnapshot; s != nil {
		if s.err != nil {
			return nil
		}
		docs := make([]*memory.BM25Doc, 0, len(s.passages))
		byID := map[string]memory.Passage{}
		for _, p := range s.passages {
			docs = append(docs, memory.NewBM25FDoc(p.ID, p.Content, p.Tags))
			byID[p.ID] = p
		}
		var out []memory.Passage
		for _, hit := range memory.BM25FRank(query, docs) {
			if len(out) >= k {
				break
			}
			out = append(out, byID[hit.ID])
		}
		return out
	}
	return l.Memory.AutoRetrieveCandidates(query, k)
}

// Read only host-owned structural metadata; memory may legitimately contain
// arbitrary section-like strings. Legacy unstructured checkpoints fail closed.
func sanitizeCheckpointMemory(block llm.ContentBlock, current string, valid bool) llm.ContentBlock {
	var sections []llm.SystemSection
	if json.Unmarshal([]byte(block.ProviderHint[dynamicCheckpointHint+".sections"]), &sections) != nil {
		block.Text = ""
		return block
	}
	var retained []llm.SystemSection
	changed := false
	for _, section := range sections {
		keep := true
		if section.Name == "memory_index" {
			keep = valid
			for _, line := range strings.Split(section.Body, "\n") {
				if strings.TrimSpace(line) != "" && !strings.Contains(current, line) {
					keep = false
					break
				}
			}
		}
		if keep {
			retained = append(retained, section)
		} else {
			changed = true
		}
	}
	if !changed {
		return block
	}
	var dynamic strings.Builder
	for _, section := range retained {
		dynamic.WriteString("\n\n[" + section.Name + "]\n" + section.Body)
	}
	block.Text = "<dynamic_context_snapshot>\nThe following is the complete current runtime context. It supersedes earlier dynamic_context_snapshot checkpoints; absent sections are no longer active." + dynamic.String() + "\n</dynamic_context_snapshot>"
	hints := make(map[string]string, len(block.ProviderHint))
	for k, v := range block.ProviderHint {
		hints[k] = v
	}
	data, _ := json.Marshal(retained)
	hints[dynamicCheckpointHint+".sections"] = string(data)
	block.ProviderHint = hints
	return block
}

// Persist the exact post-validation history before dispatch, including dynamic
// checkpoints appended by request construction. Failure is fatal: never send a
// sanitized request while leaving a resumable revoked history as the boundary.
func (l *Loop) checkpointRequestHistory() error {
	l.mu.Lock()
	before := transcript.DeepSnapshot(l.memoryRevocationBefore)
	after := transcript.DeepSnapshot(l.Messages)
	revision := l.historyRevision
	checkpoint := l.CompactionCheckpoint
	needsReplacement := l.memoryRevocationBefore != nil
	l.mu.Unlock()
	if needsReplacement {
		if checkpoint == nil && l.HistoryCheckpoint != nil {
			return fmt.Errorf("memory revocation requires a full-history replacement checkpoint")
		}
		if checkpoint != nil {
			if err := checkpoint(before, after); err != nil {
				return fmt.Errorf("persist memory revocation: %w", err)
			}
		}
		l.mu.Lock()
		if l.historyRevision == revision {
			l.memoryRevocationBefore = nil
		}
		l.mu.Unlock()
	}
	if err := l.checkpointHistory(); err != nil {
		return fmt.Errorf("persist validated request history: %w", err)
	}
	return nil
}

// Ordered context metadata is part of durable history, not a hidden cache.
// Revocation must erase both the rendered wire delta and its full resume state.
func sanitizeContextUpdateMemory(block llm.ContentBlock, current string, visible map[string]string, valid bool) (llm.ContentBlock, bool) {
	if block.ContextUpdate == nil {
		return block, false
	}
	update := *block.ContextUpdate
	allowed := func(section provider.ContextSection) bool {
		if section.Name == "memory_index" {
			if !valid {
				return false
			}
			for _, line := range strings.Split(section.Body, "\n") {
				if strings.TrimSpace(line) != "" && !strings.Contains(current, line) {
					return false
				}
			}
		}
		if section.Name == "auto-retrieve" {
			if !valid {
				return false
			}
			for _, fence := range recallFence.FindAllString(section.Body, -1) {
				match := recallRefs.FindStringSubmatch(fence)
				if len(match) != 2 {
					return false
				}
				data, err := base64.RawURLEncoding.DecodeString(match[1])
				var refs []recallReference
				if err != nil || json.Unmarshal(data, &refs) != nil || len(refs) == 0 {
					return false
				}
				for _, ref := range refs {
					if visible[ref.ID] != ref.Hash {
						return false
					}
				}
			}
		}
		return true
	}
	removed := map[string]bool{}
	filter := func(sections []provider.ContextSection) []provider.ContextSection {
		var kept []provider.ContextSection
		for _, section := range sections {
			if allowed(section) {
				kept = append(kept, section)
			} else {
				removed[section.Name] = true
			}
		}
		return kept
	}
	update.State, update.Changed = filter(update.State), filter(update.Changed)
	if len(removed) == 0 {
		return block, false
	}
	update.Removed = append([]string(nil), update.Removed...)
	for _, name := range []string{"memory_index", "auto-retrieve"} {
		if removed[name] {
			found := false
			for _, old := range update.Removed {
				if old == name {
					found = true
				}
			}
			if !found {
				update.Removed = append(update.Removed, name)
			}
		}
	}
	block.ContextUpdate = &update
	block.Text = renderContextUpdate(&update)
	return block, true
}
