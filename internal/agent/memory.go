package agent

// Package memory implements the auto-memory and skill-nudge system.
// Inspired by Hermes' AIAgent memory nudge (every N turns) and skill nudge
// (every M iterations when skill_manage tool was used). Tracks when to
// suggest memory updates and skill creation based on agent activity patterns.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store persists memory entries to JSONL under a root directory.
type Store struct {
	Root string
}

// NewStore returns a Store that persists under dir.
func NewStore(dir string) *Store {
	os.MkdirAll(dir, 0o755)
	return &Store{Root: dir}
}

// Entry is one memory item.
type Entry struct {
	Type      string   `json:"type"` // "fact" | "preference" | "context"
	Key       string   `json:"key"`
	Value     string   `json:"value"`
	Source    string   `json:"source,omitempty"` // which tool / turn produced it
	CreatedAt string   `json:"created_at"`
	Tags      []string `json:"tags,omitempty"`
}

// Fact records a factual finding.
type Fact struct {
	Content string `json:"content"`
	Tags    []string
}

// Preference records a user preference.
type Preference struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Save persists an entry to the store.
func (s *Store) Save(e Entry) error {
	f, err := os.OpenFile(filepath.Join(s.Root, e.Type+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	e.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	return json.NewEncoder(f).Encode(e)
}

// Query searches memory entries by type/tag/key substring. Returns newest first.
func (s *Store) Query(typ, keySubstr string, limit int) ([]Entry, error) {
	var entries []Entry
	if typ == "" {
		types := []string{"fact", "preference", "context"}
		for _, t := range types {
			ents, err := s.loadFile(filepath.Join(s.Root, t+".jsonl"), keySubstr, limit)
			if err != nil {
				continue
			}
			entries = append(entries, ents...)
		}
	} else {
		var err error
		entries, err = s.loadFile(filepath.Join(s.Root, typ+".jsonl"), keySubstr, limit)
		if err != nil {
			return nil, err
		}
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func (s *Store) loadFile(path, keySubstr string, limit int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []Entry
	d := json.NewDecoder(f)
	for d.More() {
		var e Entry
		if err := d.Decode(&e); err != nil {
			continue
		}
		if keySubstr != "" && !strings.Contains(strings.ToLower(e.Key+e.Value), strings.ToLower(keySubstr)) {
			continue
		}
		entries = append(entries, e)
	}
	// newest first
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// Render returns a compact memory summary suitable for injection into system prompt.
func (s *Store) Render(facts, prefs, ctx int) (string, error) {
	var parts []string
	if facts > 0 {
		ents, _ := s.Query("fact", "", facts)
		if len(ents) > 0 {
			var lines []string
			for _, e := range ents {
				lines = append(lines, "- "+e.Value)
			}
			parts = append(parts, "## Facts\n"+strings.Join(lines, "\n"))
		}
	}
	if prefs > 0 {
		ents, _ := s.Query("preference", "", prefs)
		if len(ents) > 0 {
			var lines []string
			for _, e := range ents {
				lines = append(lines, "- "+e.Key+": "+e.Value)
			}
			parts = append(parts, "## Preferences\n"+strings.Join(lines, "\n"))
		}
	}
	if ctx > 0 {
		ents, _ := s.Query("context", "", ctx)
		if len(ents) > 0 {
			var lines []string
			for _, e := range ents {
				lines = append(lines, "- "+e.Value)
			}
			parts = append(parts, "## Recent Context\n"+strings.Join(lines, "\n"))
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "## Memory\n" + strings.Join(parts, "\n\n"), nil
}
