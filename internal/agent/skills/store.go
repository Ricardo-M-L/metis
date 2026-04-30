// Package skills holds Metis's local skill catalog and the marketplace
// adapters that fetch skills from external sources. Split out from the
// previously monolithic `internal/agent` package so the agent loop doesn't
// drag in HTTP clients / GitHub URLs / search ranking logic on every import.
//
// Layout mirrors the openclaw / hermes-agent split between *store* (on-disk
// list/get/save/delete) and *marketplace* (fetch + verify + search). New
// sources (HTTP, registry servers, etc.) live alongside marketplace.go.
package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// Skill is an alias for the public manifest type so existing in-tree
// callers keep using `skills.Skill` without churn. Plugin authors should
// import `pkg/skill` directly instead.
type Skill = pubskill.Skill

// Store is the local on-disk catalog of installed skills. Skills are
// individual JSON files under Root.
type Store struct {
	Root string
}

// NewStore returns a Store rooted at dir, creating the directory if missing.
// Any error here is silently swallowed because callers (TUI panel listings,
// skill-install commands) prefer "show empty list" over "blow up startup".
func NewStore(dir string) *Store {
	_ = os.MkdirAll(dir, 0o755)
	return &Store{Root: dir}
}

func (s *Store) List() ([]Skill, error) {
	ents, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Root, e.Name()))
		if err != nil {
			continue
		}
		var sk Skill
		if err := json.Unmarshal(b, &sk); err != nil {
			continue
		}
		out = append(out, sk)
	}
	return out, nil
}

func (s *Store) Get(name string) (*Skill, error) {
	b, err := os.ReadFile(filepath.Join(s.Root, sanitize(name)+".json"))
	if err != nil {
		return nil, err
	}
	var sk Skill
	if err := json.Unmarshal(b, &sk); err != nil {
		return nil, err
	}
	return &sk, nil
}

func (s *Store) Save(sk *Skill) error {
	b, err := json.MarshalIndent(sk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Root, sanitize(sk.Name)+".json"), b, 0o644)
}

func (s *Store) Delete(name string) error {
	return os.Remove(filepath.Join(s.Root, sanitize(name)+".json"))
}

// RenderForPrompt formats every installed skill as a markdown bullet list
// for injection into the system prompt. Empty store → empty string + nil
// (so the caller can `if s != ""` to decide whether to splice it in).
func (s *Store) RenderForPrompt() (string, error) {
	skills, err := s.List()
	if err != nil || len(skills) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("## Available Skills\n\n")
	for _, sk := range skills {
		b.WriteString("- **")
		b.WriteString(sk.Name)
		b.WriteString("**")
		if sk.Description != "" {
			b.WriteString(": ")
			b.WriteString(sk.Description)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nUse the `Skill` tool to invoke a skill by name.\n")
	return b.String(), nil
}

func (s *Store) IncrementUse(name string) error {
	sk, err := s.Get(name)
	if err != nil {
		return err
	}
	sk.Uses++
	return s.Save(sk)
}

// sanitize strips path-traversal-y characters so a malicious skill name
// can't escape Store.Root.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' || r == 0 {
			return -1
		}
		return r
	}, name)
}
