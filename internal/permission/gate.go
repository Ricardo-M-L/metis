// Package permission implements cascading permission rules inspired by
// Claude Code's settings precedence (CLI > local > project > user > policy).
// Modes mirror the operator-friendly defaults: ask / auto / plan / deny.
package permission

import (
	"context"
	"strings"
	"sync"
)

// Mode is the global gating posture for a session.
type Mode string

const (
	ModeAsk    Mode = "ask"    // prompt for every non-allowlisted action
	ModeAuto   Mode = "auto"   // auto-allow read-only & allowlisted; ask for the rest
	ModeBypass Mode = "bypass" // approve everything (dangerous; opt-in)
	ModePlan   Mode = "plan"   // read-only mode; no edits or shell writes
	ModeDeny   Mode = "deny"   // refuse everything that asks
)

// Decision is the final verdict for a tool call.
type Decision int

const (
	DecisionAsk Decision = iota
	DecisionAllow
	DecisionDeny
)

// Rule is a single declarative entry: tool + optional content matcher + verb.
type Rule struct {
	Tool   string // e.g. "Bash", "Edit", "*" for any
	Match  string // optional substring of stringified input
	Verb   Decision
	Source string // for diagnostics: "cli", "project", "user", ...
}

// Gate combines a Mode and a stack of Rule sources.
type Gate struct {
	mu    sync.RWMutex
	mode  Mode
	rules []Rule
	// remembered "ask once, apply forever this session"
	memoAllow map[string]bool
}

func New(mode Mode) *Gate {
	return &Gate{mode: mode, memoAllow: make(map[string]bool)}
}

func (g *Gate) Mode() Mode {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode
}

func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = m
}

// AppendRules adds rules to the bottom of the stack (lowest precedence).
// To override, push new rules later — they win on first match.
func (g *Gate) AppendRules(rules ...Rule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rules = append(g.rules, rules...)
}

// PopRules drops the trailing n rules from the stack. Used by the cron
// scheduler to install a temporary deny-list for a single firing
// (CronJob.DisabledTools, Hermes pattern) and tear it back down after
// the run, without leaking job-specific deny rules into other jobs.
//
// Best-effort: if n exceeds the current stack depth, the stack is just
// emptied. Concurrent AppendRules / PopRules from multiple goroutines
// are serialised on the same mutex.
func (g *Gate) PopRules(n int) {
	if n <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n >= len(g.rules) {
		g.rules = nil
		return
	}
	g.rules = g.rules[:len(g.rules)-n]
}

// Check evaluates a tool invocation against the current mode + rules.
// stringInput is a flattened representation used for substring matching.
func (g *Gate) Check(_ context.Context, tool, stringInput string) (Decision, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Iterate in reverse — later rules (higher precedence) win.
	for i := len(g.rules) - 1; i >= 0; i-- {
		r := g.rules[i]
		if r.Tool != "*" && r.Tool != tool {
			continue
		}
		if r.Match != "" && !strings.Contains(stringInput, r.Match) {
			continue
		}
		return r.Verb, r.Source
	}

	switch g.mode {
	case ModeBypass:
		return DecisionAllow, "mode:bypass"
	case ModeDeny:
		return DecisionDeny, "mode:deny"
	case ModePlan:
		// allow only read-only tools by name convention
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			return DecisionAllow, "mode:plan"
		default:
			return DecisionDeny, "mode:plan"
		}
	case ModeAuto:
		// Same allowlist as ModePlan — both are "reads only" stances and
		// having WebFetch in one but not the other was inconsistent.
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			return DecisionAllow, "mode:auto"
		}
	}
	return DecisionAsk, "default"
}

// Remember marks a tool+input pair as approved for the rest of the session.
// Used when the user says "always allow this kind of call".
func (g *Gate) Remember(tool, stringInput string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.memoAllow[tool+"\x00"+stringInput] = true
}

func (g *Gate) Remembered(tool, stringInput string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.memoAllow[tool+"\x00"+stringInput]
}

// Snapshot returns a copy of the current rule stack. Used by --resume to
// persist "always allow" decisions across sessions.
func (g *Gate) Snapshot() []Rule {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Rule, len(g.rules))
	copy(out, g.rules)
	return out
}
