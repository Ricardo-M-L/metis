// Package runtime — agent profile loader for `metis --agent=NAME`.
//
// An agent profile is a markdown file at ~/.metis/agents/<name>.md (user)
// or ./.metis/agents/<name>.md (project). Mirrors claude-code's
// .claude/agents/<name>.md format:
//
//	---
//	name: reviewer
//	description: code reviewer that only reads
//	model: claude-haiku-4-5
//	tools: Read, Glob, Grep, LS
//	disallowed_tools: Bash, Write, Edit
//	permission_mode: default
//	effort: low
//	max_turns: 20
//	initial_prompt: |
//	  Focus on the diff between origin/main and HEAD.
//	---
//	You are a meticulous code reviewer. Always start by listing files
//	that changed, then walk through them depth-first.
//
// The body becomes the system prompt; the frontmatter is parsed into
// AgentProfile. CLI applies it by:
//
//   - swapping system prompt
//   - filtering the tool registry to allowlist (or removing disallowed)
//   - overriding model + permission mode + effort + max_iter
//   - prepending initial_prompt to the first user message
package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// AgentProfile is the parsed result of loading ~/.metis/agents/<name>.md.
// All fields are optional except Name; a missing field means "inherit
// from parent runtime".
type AgentProfile struct {
	Name            string
	Description     string
	Model           string   // empty = inherit
	Tools           []string // allowlist; empty = inherit (no filter)
	DisallowedTools []string // blocklist applied after allowlist
	PermissionMode  string   // default | acceptEdits | plan | dontAsk | bypassPermissions | fullAccess; empty = inherit
	Effort          string   // low | medium | high; empty = inherit
	MaxTurns        int      // 0 = inherit
	InitialPrompt   string   // prepended to first user message
	Skills          []string // extra skills to load
	OmitClaudeMd    bool     // skip ~/.metis/system.md addendum
	SystemPrompt    string   // markdown body
	// MemorySnapshot is parsed only for backwards compatibility with legacy
	// profiles. The old agent.Store snapshot format is deprecated and no
	// production runtime restores it; durable state now flows through the
	// unified memory.Repository and provenance-aware session recall.
	MemorySnapshot string
	// Source identifies which trust layer supplied this profile. It is runtime
	// provenance, not frontmatter: project profiles may customize behavior but
	// must not silently elevate a top-level runtime to fullAccess before the
	// workspace trust gate has accepted that checkout.
	Source AgentProfileSource
}

type AgentProfileSource string

const (
	AgentProfileSourceProject AgentProfileSource = "project"
	AgentProfileSourceUser    AgentProfileSource = "user"
	AgentProfileSourceBuiltin AgentProfileSource = "builtin"
)

// AvailableAgentProfileNames returns every profile slug that the runtime can
// resolve: bundled profiles plus project and user-defined markdown profiles.
// The result is sorted and deduplicated so the generated tool schema is stable
// across requests (important for provider-side prompt caching).
func AvailableAgentProfileNames() []string {
	seen := make(map[string]struct{})
	for _, name := range BuiltinProfileNames() {
		if validAgentName(name) {
			seen[name] = struct{}{}
		}
	}
	for _, dir := range []string{
		filepath.Join(".metis", "agents"),
		filepath.Join(config.Home(), "agents"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".md")
			if validAgentName(name) {
				seen[name] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// LoadAgentProfile resolves NAME to a markdown file and parses it.
//
// Search order (G.7, 2026-05-12):
//
//  1. ./.metis/agents/<name>.md           — project override
//  2. ~/.metis/agents/<name>.md           — user override
//  3. //go:embed builtin_profiles/<name>.md — bundled default
//
// Returns nil profile + nil err when name is empty (no flag passed).
// User-written files always win over bundled — the bundled set is
// just a fallback so `metis --agent=explore` works on a fresh
// install without any user setup.
func LoadAgentProfile(name string) (*AgentProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	if !validAgentName(name) {
		return nil, fmt.Errorf("invalid agent name %q (use [a-zA-Z0-9._-])", name)
	}
	candidates := []struct {
		path   string
		source AgentProfileSource
	}{
		{path: filepath.Join(".metis", "agents", name+".md"), source: AgentProfileSourceProject},
		{path: filepath.Join(config.Home(), "agents", name+".md"), source: AgentProfileSourceUser},
	}
	for _, candidate := range candidates {
		b, err := os.ReadFile(candidate.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", candidate.path, err)
		}
		prof, err := parseAgentProfile(string(b))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", candidate.path, err)
		}
		if prof.Name == "" {
			prof.Name = name
		}
		prof.Source = candidate.source
		return prof, nil
	}
	// Fallback to bundled (G.7). Not finding a user file is the
	// common case — most users only customize a couple of profiles
	// and rely on the bundled set for the rest.
	if prof, err := LoadBuiltinProfile(name); err == nil {
		prof.Source = AgentProfileSourceBuiltin
		return prof, nil
	} else if !errors.Is(err, ErrBuiltinProfileNotFound) {
		// Bundled profile exists but parse failed — surface that.
		return nil, err
	}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.path)
	}
	return nil, fmt.Errorf("agent profile %q not found (looked in %s, then bundled)", name, strings.Join(paths, ", "))
}

func validAgentName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// parseAgentProfile splits the YAML-ish frontmatter from the markdown
// body. We don't pull in a YAML dep — the schema is small and flat,
// so a hand parser keeps the dep tree clean and tolerates the most
// common authoring mistakes (missing quotes, extra blank lines).
func parseAgentProfile(s string) (*AgentProfile, error) {
	prof := &AgentProfile{}
	body := s
	if strings.HasPrefix(s, "---\n") || strings.HasPrefix(s, "---\r\n") {
		// Find closing fence.
		rest := s[4:]
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			fm := rest[:idx]
			// Skip past the closing fence + its newline.
			tail := rest[idx+len("\n---"):]
			tail = strings.TrimPrefix(tail, "\n")
			tail = strings.TrimPrefix(tail, "\r\n")
			if err := applyFrontmatter(fm, prof); err != nil {
				return nil, err
			}
			body = tail
		}
	}
	prof.SystemPrompt = strings.TrimSpace(body)
	return prof, nil
}

func applyFrontmatter(fm string, prof *AgentProfile) error {
	for lineno, raw := range strings.Split(fm, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			return fmt.Errorf("line %d: missing colon in %q", lineno+1, line)
		}
		key := strings.TrimSpace(trimmed[:colon])
		val := strings.TrimSpace(trimmed[colon+1:])
		val = strings.TrimSuffix(val, "\"")
		val = strings.TrimPrefix(val, "\"")
		val = strings.TrimSuffix(val, "'")
		val = strings.TrimPrefix(val, "'")

		switch strings.ToLower(key) {
		case "name":
			prof.Name = val
		case "description":
			prof.Description = val
		case "model":
			prof.Model = val
		case "tools":
			prof.Tools = splitList(val)
		case "disallowed_tools", "disallowedtools":
			prof.DisallowedTools = splitList(val)
		case "permission_mode", "permissionmode":
			prof.PermissionMode = val
		case "effort":
			prof.Effort = val
		case "max_turns", "maxturns":
			n, err := atoiSafe(val)
			if err != nil {
				return fmt.Errorf("max_turns: %w", err)
			}
			prof.MaxTurns = n
		case "initial_prompt", "initialprompt":
			prof.InitialPrompt = val
		case "skills":
			prof.Skills = splitList(val)
		case "omit_claude_md", "omitclaudemd":
			prof.OmitClaudeMd = strings.EqualFold(val, "true") || val == "1"
		case "memory_snapshot", "memorysnapshot":
			prof.MemorySnapshot = val
		}
	}
	return nil
}

func splitList(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	// Strip [ ] if present so "[a, b]" parses too.
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"'")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoiSafe(v string) (int, error) {
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an integer: %q", v)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ApplyToFlags returns a flag overlay merging the agent profile onto the
// CLI-passed flags. Profile values lose to explicit CLI overrides — if
// the user passes `--agent=reviewer --model=claude-opus-4-7`, the model
// from the CLI wins. Returned struct is a *copy* of the input flags.
//
// Used at startup to compose the final runtime configuration.
func (p *AgentProfile) MergeOnto(model, mode, effort string, maxIter int) (outModel, outMode, outEffort string, outMaxIter int) {
	outModel = model
	outMode = mode
	outEffort = effort
	outMaxIter = maxIter
	if p == nil {
		return
	}
	if outModel == "" && p.Model != "" {
		outModel = p.Model
	}
	if outMode == "" && p.PermissionMode != "" {
		outMode = p.PermissionMode
	}
	if outEffort == "" && p.Effort != "" {
		outEffort = p.Effort
	}
	if outMaxIter == 0 && p.MaxTurns > 0 {
		outMaxIter = p.MaxTurns
	}
	return
}

// FilterToolNames applies the allowlist + blocklist to a slice of
// registered tool names. Returns the surviving subset (in input order).
// Empty allowlist = inherit (all kept); blocklist always applies after.
func (p *AgentProfile) FilterToolNames(all []string) []string {
	if p == nil {
		return all
	}
	keep := all
	if len(p.Tools) > 0 {
		allow := make(map[string]struct{}, len(p.Tools))
		for _, t := range p.Tools {
			allow[t] = struct{}{}
		}
		keep = keep[:0]
		for _, n := range all {
			if _, ok := allow[n]; ok {
				keep = append(keep, n)
			}
		}
	}
	if len(p.DisallowedTools) > 0 {
		block := make(map[string]struct{}, len(p.DisallowedTools))
		for _, t := range p.DisallowedTools {
			block[t] = struct{}{}
		}
		out := keep[:0]
		for _, n := range keep {
			if _, ok := block[n]; !ok {
				out = append(out, n)
			}
		}
		keep = out
	}
	// Defensive copy so caller can't mutate the registry-backing slice.
	cp := make([]string, len(keep))
	copy(cp, keep)
	return cp
}
