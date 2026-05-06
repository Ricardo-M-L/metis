package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// persistent.go — F16: cross-session "Yes always" approvals.
//
// Mirrors openclaw's `src/acp/persistent-bindings.ts`. Without this,
// every new chat session forgets the user's `Yes, always` answers
// from the previous one — they have to re-approve `Bash(git status)`
// every morning. With this, approvals land in
// `~/.metis/persistent-permissions.jsonl`, are loaded at gate
// construction, and survive `metis chat` exits.
//
// Format: one Approval JSON per line, append-only. Newer approvals
// for the same tool+match override older ones (loader applies in
// file order). `expires_at` (RFC3339) is honoured — entries past
// expiry are silently dropped on load.

// Approval is one persisted "Yes always" decision.
type Approval struct {
	Tool      string    `json:"tool"`
	Match     string    `json:"match,omitempty"`
	GrantedAt time.Time `json:"granted_at"`
	// ExpiresAt zero value = never expires. Future enhancement: a
	// `metis perms expire 30d` command sets a relative expiry.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// PersistentBindings is the on-disk approval store.
type PersistentBindings struct {
	path string
	mu   sync.Mutex
}

// NewPersistentBindings binds to a file at the given path. The file
// is created on first AppendApproval call; missing file is fine
// (Load returns []).
func NewPersistentBindings(path string) *PersistentBindings {
	return &PersistentBindings{path: path}
}

// Default returns the default file location for metis: ~/.metis/
// persistent-permissions.jsonl. Caller passes the metis home dir
// (we don't read $HOME directly so tests can isolate).
func Default(metisHome string) *PersistentBindings {
	return NewPersistentBindings(filepath.Join(metisHome, "persistent-permissions.jsonl"))
}

// Load reads all non-expired approvals from disk. Errors on file
// read fall through silently (returns []). Caller decides whether
// to surface the IO error.
func (p *PersistentBindings) Load() ([]Approval, error) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Approval
	now := time.Now()
	// JSONL — split on \n. Tolerate blank lines and trailing newline.
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var a Approval
		if err := json.Unmarshal(line, &a); err != nil {
			// Skip malformed lines so a single corrupt entry doesn't
			// brick the whole file.
			continue
		}
		if !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// AppendApproval adds a new approval to the file (atomic-by-line:
// one write call). Doesn't dedupe in place — old entries become
// shadowed by the newer same-key entry on next Load.
func (p *PersistentBindings) AppendApproval(a Approval) error {
	if p == nil {
		return nil
	}
	if a.GrantedAt.IsZero() {
		a.GrantedAt = time.Now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(body)
	return err
}

// LoadInto reads the persistent approvals and pushes them onto the
// gate's rule stack as DecisionAllow rules (source = "persistent").
// Convenience wrapper for the common pattern at gate boot.
func (g *Gate) LoadInto(p *PersistentBindings) error {
	if p == nil {
		return nil
	}
	approvals, err := p.Load()
	if err != nil {
		return err
	}
	rules := make([]Rule, 0, len(approvals))
	for _, a := range approvals {
		rules = append(rules, Rule{
			Tool:   a.Tool,
			Match:  a.Match,
			Verb:   DecisionAllow,
			Source: "persistent",
		})
	}
	if len(rules) > 0 {
		g.AppendRules(rules...)
	}
	return nil
}

// RememberPersistent is the cross-session sibling of Remember(): it
// both adds to the in-memory memo AND appends to disk so the next
// session boots with this approval pre-loaded.
//
// Returns the underlying disk error (rare); on error the in-memory
// memo is still set so this session continues to honour the user's
// "Yes always".
func (g *Gate) RememberPersistent(p *PersistentBindings, tool, match string) error {
	g.Remember(tool, match)
	g.AppendRules(Rule{Tool: tool, Match: match, Verb: DecisionAllow, Source: "persistent"})
	if p == nil {
		return nil
	}
	return p.AppendApproval(Approval{Tool: tool, Match: match})
}

// splitLines is a tiny helper to split JSONL by '\n' without
// pulling bufio.Scanner. Empty trailing lines drop out via the
// caller's len() check.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
