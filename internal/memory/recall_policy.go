package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// RecallSnapshot is an authoritative view using the repository's existing scope,
// tombstone and security filters. Its generation excludes access counters and
// timestamps: recording a genuine use must not revoke a cacheable prefix.
func (mm *MemoryManager) RecallSnapshot() (generation, body string, err error) {
	err = mm.GuardRecallSnapshot(func(g, b string, _ []Passage) error { generation, body = g, b; return nil })
	return
}

// GuardRecallSnapshot serializes an authoritative snapshot and its consumer
// against repository API writers (including other processes). The callback is
// the linearization point: later writes apply to the next request snapshot.
// It must not call back into repository APIs or wait for network I/O.
func (mm *MemoryManager) GuardRecallSnapshot(consume func(string, string, []Passage) error) error {
	if mm == nil {
		return consume("", "", nil)
	}
	return withRepositoryLock(mm.root, func() error {
		generation, body, passages, err := mm.recallSnapshotLocked()
		if err != nil {
			return err
		}
		return consume(generation, body, passages)
	})
}

func (mm *MemoryManager) recallSnapshotLocked() (string, string, []Passage, error) {
	if mm.core != nil {
		mm.core.mu.Lock()
		err := mm.core.reloadAuthoritativeLocked(true)
		mm.core.mu.Unlock()
		if err != nil {
			return "", "", nil, err
		}
	}
	var passages []Passage
	if mm.archival != nil {
		archived, err := mm.archival.Search(SearchOptions{SortBy: "recent"})
		if err != nil {
			return "", "", nil, err
		}
		for _, p := range archived {
			if mm.passageVisible(p) {
				passages = append(passages, p)
			}
		}
	}
	topics, err := loadTopicDocuments(mm.root)
	if err != nil {
		return "", "", nil, err
	}
	for _, topic := range topics {
		if mm.passageVisible(topic.passage) {
			passages = append(passages, topic.passage)
		}
	}
	sort.Slice(passages, func(i, j int) bool { return passages[i].ID < passages[j].ID })
	index, err := mm.compactTopicIndex()
	if err != nil {
		return "", "", nil, err
	}
	core := ""
	if mm.core != nil {
		core = mm.core.Render()
	}
	body := composeMemoryContext(core, index)
	h := sha256.New()
	paths := []string{mm.root, filepath.Join(mm.root, "archival"), filepath.Join(mm.root, "archival", "passages.jsonl")}
	for _, topic := range topics {
		paths = append(paths, filepath.Join(mm.root, topic.file))
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", nil, err
		}
		fmt.Fprintf(h, "%s|%o\n", path, info.Mode())
	}
	for _, p := range passages {
		data, _ := json.Marshal([]string{p.ID, p.Content, p.Scope, p.SourceSessionID})
		h.Write(data)
	}
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil)), body, passages, nil
}

// RecallRelevant requires lexical evidence rather than a language-specific
// greeting blacklist. In CJK a lone common character is not evidence; adjacent
// pairs carry meaning. Single Latin terms can still be precise identifiers.
func RecallRelevant(query, content string) bool {
	q := recallTerms(query)
	if len(q) == 0 {
		return false
	}
	c := recallTerms(content)
	matches := 0
	for term := range q {
		if c[term] {
			matches++
		}
	}
	if matches == 0 {
		return false
	}
	// Long instructions matching just one generic word are weak evidence.
	return len(q) < 5 || matches >= 2 || float64(matches)/float64(len(q)) >= .25
}

func recallTerms(s string) map[string]bool {
	out := map[string]bool{}
	for _, token := range tokenize(s) {
		runes := []rune(token)
		if len(runes) == 1 && isCJKRune(runes[0]) {
			continue
		}
		out[token] = true
	}
	return out
}

// RecallTokens deliberately estimates conservatively for mixed CJK/Latin text.
// This is a local admission estimate, not a provider tokenizer.
func RecallTokens(s string) int {
	n := 0
	ascii := 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			n++
		}
	}
	return n + (ascii+2)/3
}

// BudgetRecall admits complete passages up to target; a single complete passage
// may exceed target but never hard. It never cuts XML fences or a fact mid-body.
func BudgetRecall(passages []Passage, target, hard int) []Passage {
	if target <= 0 {
		target = 800
	}
	if hard <= 0 {
		hard = 1500
	}
	if target > hard {
		target = hard
	}
	var picked []Passage
	for _, p := range passages {
		if strings.TrimSpace(p.Content) == "" || !utf8.ValidString(p.Content) {
			continue
		}
		candidate := append(append([]Passage(nil), picked...), p)
		cost := RecallTokens(FormatRetrieveSection(candidate)) + 32 // generation attribute
		if cost > hard {
			continue
		}
		if len(picked) > 0 && cost > target {
			continue
		}
		picked = candidate
		if cost >= target {
			break
		}
	}
	return picked
}
