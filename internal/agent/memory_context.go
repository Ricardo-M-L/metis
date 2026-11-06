package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

// Optional capability preserves the public Repository API for plugin stores.
type memorySnapshotter interface {
	RecallSnapshot() (generation, context string, err error)
}

func recallSnapshot(repository memory.Repository) (string, string, bool) {
	if repository == nil {
		return "", "", true
	}
	if snapshotter, ok := repository.(memorySnapshotter); ok {
		generation, body, err := snapshotter.RecallSnapshot()
		return generation, body, err == nil
	}
	return "", repository.BuildContext(), true
}

var recallFence = regexp.MustCompile(`(?s)<auto-retrieve(?:\s[^>]*)?>.*?</auto-retrieve>`)

func recallBudgetLimits(target, hard int) (int, int) {
	if target <= 0 {
		target, _ = strconv.Atoi(os.Getenv("METIS_AUTO_RETRIEVE_TARGET_TOKENS"))
	}
	if hard <= 0 {
		hard, _ = strconv.Atoi(os.Getenv("METIS_AUTO_RETRIEVE_MAX_TOKENS"))
	}
	if target <= 0 {
		target = 800
	}
	if hard <= 0 {
		hard = 1500
	}
	if target > hard {
		target = hard
	}
	return target, hard
}

func relevantRecallCandidates(passages []memory.Passage, query string) []memory.Passage {
	var relevant []memory.Passage
	for _, p := range passages {
		if memory.RecallRelevant(query, p.Content) {
			relevant = append(relevant, p)
		}
	}
	return relevant
}

func normalizeRecallText(text string) string { return strings.Join(strings.Fields(text), " ") }

// Rebuild coverage from actual active messages on every turn. No lifetime seen
// set: compaction/deletion automatically makes missing bodies eligible again.
func novelRecall(passages []memory.Passage, messages []llm.Message, query string, target, hard int) []memory.Passage {
	var active []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "text" {
				active = append(active, normalizeRecallText(block.Text))
			}
		}
	}
	var picked []memory.Passage
	for _, p := range passages {
		body := normalizeRecallText(p.Content)
		if body == "" || !memory.RecallRelevant(query, p.Content) {
			continue
		}
		covered := false
		for _, text := range active {
			if strings.Contains(text, body) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		picked = append(picked, p)
		active = append(active, body)
	}
	target, hard = recallBudgetLimits(target, hard)
	var admitted []memory.Passage
	for _, passage := range picked {
		candidate := append(append([]memory.Passage(nil), admitted...), passage)
		cost := memory.RecallTokens(formatRecall(candidate, ""))
		if cost > hard || (len(admitted) > 0 && cost > target) {
			continue
		}
		admitted = candidate
		if cost >= target {
			break
		}
	}
	return admitted
}

type recallReference struct{ ID, Hash string }

func recallPassageHash(p memory.Passage) string {
	data, _ := json.Marshal([]string{p.Content, p.Scope, p.SourceSessionID})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

var recallRefs = regexp.MustCompile(` refs="([A-Za-z0-9_-]+)"`)

func formatRecall(passages []memory.Passage, generation string) string {
	var sections []string
	for _, p := range passages {
		body := memory.FormatRetrieveSection([]memory.Passage{p})
		data, _ := json.Marshal([]recallReference{{p.ID, recallPassageHash(p)}})
		body = strings.Replace(body, "<auto-retrieve>", `<auto-retrieve refs="`+base64.RawURLEncoding.EncodeToString(data)+`">`, 1)
		sections = append(sections, body)
	}
	return strings.Join(sections, "\n\n")
}

// Caller holds l.mu. A revoked or unreadable repository invalidates historical
// recall as well as provider continuation state. Legacy unversioned recalls are
// conservatively discarded once. Ordinary access metadata never changes gen.
func (l *Loop) validateMemorySnapshotLocked() {
	_, body, ok := l.memorySnapshotLocked()
	_, capable := l.Memory.(memorySnapshotter)
	authoritative := capable || l.Memory == nil
	visible := map[string]string{}
	if ok && capable && l.Memory != nil {
		passages, err := l.memoryPassagesLocked()
		if err != nil {
			ok = false
		} else {
			for _, p := range passages {
				visible[p.ID] = recallPassageHash(p)
			}
		}
	}
	before := transcript.DeepSnapshot(l.Messages)
	removed := false
	for i := range l.Messages {
		content := l.Messages[i].Content
		result := make([]llm.ContentBlock, 0, len(content))
		for _, block := range content {
			// Entire derived narratives are inseparable from their source,
			// including nested projections; never string-delete their facts.
			var derivedRemoved bool
			block, derivedRemoved = sanitizeDerivedMemory(block, body, visible, ok && authoritative)
			removed = removed || derivedRemoved
			if authoritative && l.Messages[i].Role == llm.RoleSystem && block.ProviderHint[dynamicCheckpointHint] == "checkpoint" {
				updated := sanitizeCheckpointMemory(block, body, ok)
				if updated.Text != block.Text {
					removed = true
					block = updated
					if block.Text == "" {
						continue
					}
				}
			}
			if authoritative && block.Synthetic && block.Type == "text" && strings.Contains(block.Text, "<auto-retrieve") {
				updated := recallFence.ReplaceAllStringFunc(block.Text, func(fence string) string {
					valid := ok
					match := recallRefs.FindStringSubmatch(fence)
					var refs []recallReference
					if len(match) != 2 {
						valid = false
					} else {
						data, err := base64.RawURLEncoding.DecodeString(match[1])
						if err != nil || json.Unmarshal(data, &refs) != nil || len(refs) == 0 {
							valid = false
						}
					}
					for _, ref := range refs {
						if visible[ref.ID] != ref.Hash {
							valid = false
						}
					}
					if valid {
						return fence
					}
					removed = true
					return ""
				})
				if updated != block.Text {
					block.Text = updated
					if strings.TrimSpace(updated) == "" {
						continue
					}
				}
			}
			result = append(result, block)
		}
		l.Messages[i].Content = result
	}
	if removed {
		// LastSummary is an iterative seed outside the transcript. Reset under
		// compactMu, or defer to the active transaction's release path.
		if l.compactMu.TryLock() {
			if l.Compactor != nil {
				l.Compactor.ResetCircuit()
			}
			l.compactMu.Unlock()
		} else {
			l.compactorResetPending = true
		}
		if l.memoryRevocationBefore == nil {
			l.memoryRevocationBefore = before
		}
		l.historyRevision++
		l.invalidateActiveContextLocked()
		for i := range l.Messages {
			blocks := l.Messages[i].Content[:0]
			for _, block := range l.Messages[i].Content {
				if block.Type != "provider_state" {
					blocks = append(blocks, block)
				}
			}
			l.Messages[i].Content = blocks
		}
		l.turnMemoryRecall = ""
		l.turnMemoryRecallAttached = false
	}
	if !ok {
		body = ""
		l.turnMemoryRecall = ""
	}
	// Content changes within a running tool loop must reach the next snapshot.
	if l.turnMemoryPrepared {
		l.turnMemoryContext = body
	}
}
