package skills

// overlap.go — deterministic redundancy detection for agent-created skills.
//
// The dreaming SkillSynth only ever *creates* skills, so over time the
// library accumulates near-duplicates ("git-commit-helper" and
// "commit-with-message" describing the same procedure). hermes-agent's
// curator runs an LLM "consolidation / umbrella-building" pass to merge
// them. Letting an LLM scan the whole library every dream is expensive and
// over-eager, so metis gates that pass: this file finds *candidate*
// overlap clusters cheaply and deterministically (Jaccard similarity over
// tokenized name+description), and only when a cluster exists does the
// dream fork get a consolidation instruction. No overlaps → no aux-model
// cost, no behavior change.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OverlapThreshold is the Jaccard score above which two skills are
// considered redundant candidates. 0.5 = at least half the combined
// name+description vocabulary is shared — high enough to avoid flagging
// merely topical neighbors, low enough to catch reworded duplicates.
const OverlapThreshold = 0.5

// overlapStopwords are dropped before similarity so boilerplate
// description words ("the", "a", "skill", "use") don't inflate scores.
var overlapStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "to": true, "of": true, "for": true,
	"and": true, "or": true, "in": true, "on": true, "with": true, "by": true,
	"skill": true, "use": true, "used": true, "when": true, "this": true,
	"that": true, "is": true, "it": true, "you": true, "your": true,
}

// DetectOverlaps returns clusters of agent-created skill names that look
// redundant. Each returned slice has >= 2 names (sorted); the slice of
// clusters is sorted by first member for determinism. Only flat *.md
// skills directly under dir are considered (same eligibility as the
// curator). Errors collapse to an empty result — overlap detection is
// advisory and must never block a dream.
func DetectOverlaps(dir string) [][]string {
	if dir == "" {
		return nil
	}
	type sk struct {
		name   string
		tokens map[string]bool
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	usage := NewUsageStore(dir)
	var skills []sk
	for _, e := range ents {
		if isJunkFilename(e.Name()) {
			continue
		}
		base, ok := skillFileExt(e.Name())
		if !ok {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		// Provenance gate: consolidation may only nominate agent-created
		// skills. A user's hand-written flat .md (no agent-synth usage
		// record) must never be clustered and archived by the dream — the
		// flat-file shape alone is not proof of agent authorship.
		if !usage.IsAgentCreated(base) {
			continue
		}
		loaded, lerr := Load(filepath.Join(dir, e.Name()))
		if lerr != nil || loaded == nil {
			continue
		}
		toks := tokenizeForOverlap(loaded.Name + " " + loaded.Description + " " + loaded.WhenToUse)
		if len(toks) == 0 {
			continue
		}
		skills = append(skills, sk{name: base, tokens: toks})
	}
	if len(skills) < 2 {
		return nil
	}

	// Union-find over pairs whose Jaccard >= threshold.
	parent := make([]int, len(skills))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := 0; i < len(skills); i++ {
		for j := i + 1; j < len(skills); j++ {
			if jaccard(skills[i].tokens, skills[j].tokens) >= OverlapThreshold {
				parent[find(i)] = find(j)
			}
		}
	}
	groups := map[int][]string{}
	for i := range skills {
		root := find(i)
		groups[root] = append(groups[root], skills[i].name)
	}
	var out [][]string
	for _, names := range groups {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		out = append(out, names)
	}
	sort.Slice(out, func(a, b int) bool { return out[a][0] < out[b][0] })
	return out
}

// tokenizeForOverlap lowercases and tokenizes: ASCII alnum runs become
// word tokens (>=2 chars, stopwords dropped); CJK runs become unigrams +
// adjacent bigrams (no spaces between Han characters, so a per-rune +
// bigram scheme is the standard cheap approach — same idea as the memory
// package's BM25 tokenizer). Without the CJK branch a Chinese-named skill
// produced an empty token set and could never be flagged as a duplicate.
func tokenizeForOverlap(s string) map[string]bool {
	out := map[string]bool{}
	runes := []rune(strings.ToLower(s))
	var word []rune
	flush := func() {
		if len(word) >= 2 {
			w := string(word)
			if !overlapStopwords[w] {
				out[w] = true
			}
		}
		word = word[:0]
	}
	var prevCJK rune
	for _, r := range runes {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			word = append(word, r)
			prevCJK = 0
		case isCJK(r):
			flush()
			out[string(r)] = true // unigram
			if prevCJK != 0 {
				out[string([]rune{prevCJK, r})] = true // bigram
			}
			prevCJK = r
		default:
			flush()
			prevCJK = 0
		}
	}
	flush()
	return out
}

// isCJK reports whether r is in a common CJK ideograph / kana range —
// enough to tokenize Chinese/Japanese skill names; not exhaustive.
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // Extension A
		(r >= 0x3040 && r <= 0x30FF) // Hiragana + Katakana
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
