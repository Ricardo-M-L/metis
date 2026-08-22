package runtime

// learning.go is metis's minimum-viable "continuous learning" log —
// at the end of every chat turn we append one JSON line summarizing
// what happened (tools used, files touched, error rate, duration).
//
// This is NOT the full agent-evolves-skills loop hermes-agent has
// (which uses an LLM to curate skills from trajectory data). It IS
// the foundation: with the log in place, future iterations can:
//   - cluster turns by intent and surface "you usually run X after Y"
//   - track which skills are over/underused and suggest pruning
//   - build a tool-affinity profile for prompt boilerplate
//
// All deterministic / structural — no extra LLM call per turn.
//
// Storage: ~/.metis/learned.jsonl, append-only, one record per turn.
// /lessons slash command surfaces the most recent N records.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var learnedMu sync.Mutex

// LearnedRecord is one turn's structural summary. Captured at the
// turn-end hook (chat surface or `metis run`'s tail).
type LearnedRecord struct {
	Timestamp  time.Time `json:"ts"`
	SessionID  string    `json:"sid"`
	Prompt     string    `json:"prompt"` // first ~200 chars of user prompt
	ToolUsed   []string  `json:"tools"`  // distinct tool names called
	FilesTouch []string  `json:"files"`  // basenames edited / written
	Duration   string    `json:"dur"`    // e.g. "1m 32s"
	Tokens     int       `json:"tokens"`
	HadErrors  bool      `json:"had_errors"` // any tool error
	Recap      string    `json:"recap"`      // structural recap line
}

// learnedPath returns the on-disk JSONL path. Honors METIS_HOME env
// override for testing / portable installs.
func learnedPath() (string, error) {
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".metis")
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(home, "learned.jsonl"), nil
}

// AppendLearned writes one record to the log. Idempotent on transient
// IO errors — caller silently degrades when the disk is unavailable
// (logging shouldn't break the chat surface).
func AppendLearned(rec LearnedRecord) error {
	learnedMu.Lock()
	defer learnedMu.Unlock()
	path, err := learnedPath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(rec)
}

// LoadLearned returns the most recent N records (newest first).
// Stops reading early once N is reached. Tail-of-file scan is O(N)
// for typical log sizes (<10MB) — no need for an index.
func LoadLearned(n int) ([]LearnedRecord, error) {
	learnedMu.Lock()
	defer learnedMu.Unlock()
	path, err := learnedPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 1024*1024) // up to 1MB per line
	var all []LearnedRecord
	for scan.Scan() {
		var r LearnedRecord
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			continue // skip malformed lines
		}
		all = append(all, r)
	}
	// Reverse-and-cap: newest first, top N.
	if len(all) > n {
		all = all[len(all)-n:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, nil
}

// SummarizeLearned formats N records into a human-readable text
// blob suitable for the /lessons slash command output.
func SummarizeLearned(n int) string {
	recs, err := LoadLearned(n)
	if err != nil {
		return "lessons: " + err.Error()
	}
	if len(recs) == 0 {
		return "lessons: no past turns logged yet (~/.metis/learned.jsonl is empty)"
	}
	var b strings.Builder
	b.WriteString("recent turns (newest first):\n")
	for _, r := range recs {
		when := r.Timestamp.Format("01/02 15:04")
		errs := ""
		if r.HadErrors {
			errs = " · ⚠ errors"
		}
		b.WriteString("  ")
		b.WriteString(when)
		b.WriteString(" · ")
		if r.Recap != "" {
			b.WriteString(r.Recap)
		} else {
			b.WriteString(truncatedPrompt(r.Prompt, 60))
		}
		b.WriteString(errs)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncatedPrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
