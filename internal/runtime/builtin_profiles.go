package runtime

// builtin_profiles.go — bundled agent profiles (G.7, 2026-05-12).
//
// Mirrors internal/agent/skills/embedded.go: the markdown files under
// builtin_profiles/ are embedded into the binary at build time so
// `Agent({name: "explore"})` works out of the box on a fresh metis
// install, without the user having to mkdir ~/.metis/agents and drop
// markdown there.
//
// Nine profiles ship:
//
//   - explore         — fast read-only code search
//   - plan            — implementation planning (no code edits)
//   - verify          — test runner + result parsing
//   - general         — catch-all with full toolset
//   - go-reviewer     — metis-specific: Go diff review by severity
//   - mcp-debugger    — metis-specific: MCP server diagnostics
//   - creator         — implementation-focused end-to-end work
//   - coordinator     — delegation and orchestration
//   - teammate        — long-running coordinated team member
//
// User overrides win: if ~/.metis/agents/explore.md or
// ./.metis/agents/explore.md exists, that one is used instead of the
// bundled version. This keeps the bundled set as a sensible default
// without locking the user out of customization.
//
// Lookup order (resolved in LoadAgentProfile):
//
//   1. ./.metis/agents/<name>.md           (project override)
//   2. ~/.metis/agents/<name>.md           (user override)
//   3. //go:embed builtin_profiles/<name>.md (bundled fallback)

import (
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

//go:embed builtin_profiles/*.md
var bundledProfileFS embed.FS

// BuiltinProfileNames returns the slugs of every profile shipped in
// the binary. Used by /agents list and tests; the slice is sorted for
// stable output.
func BuiltinProfileNames() []string {
	entries, err := bundledProfileFS.ReadDir("builtin_profiles")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".md"))
	}
	// Deterministic order — embed.FS already walks alphabetically
	// but be defensive in case Go's FS contract changes.
	stringsSort(out)
	return out
}

// LoadBuiltinProfile parses a bundled profile by name. Returns
// (nil, ErrBuiltinProfileNotFound) when the slug isn't part of the
// shipped set so LoadAgentProfile can distinguish "no bundled match,
// keep searching" from "tried to parse but the markdown is broken"
// (which IS a hard error).
func LoadBuiltinProfile(name string) (*AgentProfile, error) {
	if name == "" {
		return nil, ErrBuiltinProfileNotFound
	}
	path := filepath.Join("builtin_profiles", name+".md")
	b, err := bundledProfileFS.ReadFile(path)
	if err != nil {
		return nil, ErrBuiltinProfileNotFound
	}
	prof, err := parseAgentProfile(string(b))
	if err != nil {
		return nil, fmt.Errorf("parse bundled profile %s: %w", name, err)
	}
	if prof.Name == "" {
		prof.Name = name
	}
	return prof, nil
}

// ErrBuiltinProfileNotFound is the sentinel returned by
// LoadBuiltinProfile when the requested slug isn't in the
// //go:embed set. Distinguishes from a parse error so the caller
// can fall through to its next lookup path.
var ErrBuiltinProfileNotFound = errors.New("builtin agent profile not found")

// stringsSort sorts in place. Tiny inline replacement to avoid
// pulling sort into this otherwise-light file.
func stringsSort(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j-1] > ss[j]; j-- {
			ss[j-1], ss[j] = ss[j], ss[j-1]
		}
	}
}
