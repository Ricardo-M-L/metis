package tools

// visibility.go — global tool-pool filtering (allow / disallow) with
// MCP server-prefix matching. Mirrors claude-code's
// filterToolsByDenyRules (restored-src/src/tools.ts:262) + the
// allowedTools/disallowedTools CLI flags (main.tsx:988), adapted for
// metis's Registry model.
//
// The two callers today:
//   1. cmd/metis/main.go::setupRuntime — applies config + CLI flag
//      values once after MCP launch, so MCP-supplied tools are also
//      subject to user filtering.
//   2. (future) per-turn dispatch — if we ever want runtime toggles.
//
// Pattern grammar:
//   "Bash"               exact match on registered tool name
//   "mcp__office-word"   matches every mcp__office-word__* tool (server-level mute)
//   "mcp__"              matches every mcp__* tool (MCP wildcard)
//   "mcp__*"             alias of "mcp__"
//
// Patterns that match nothing in the current snapshot remain installed: a
// later plugin or MCP reconnect with that name must still be filtered.

import "strings"

type toolVisibilityPolicy struct {
	allow    []string
	disallow []string
}

// toolNameMatchesPattern evaluates the visibility grammar without consulting
// the current registry. This is what makes policies durable for tools that are
// registered later by plugins, IDE bridges or an MCP reconnect.
func toolNameMatchesPattern(name, raw string) bool {
	p := strings.TrimSpace(raw)
	if p == "" {
		return false
	}
	if p == "mcp__" || p == "mcp__*" {
		return strings.HasPrefix(name, "mcp__")
	}
	if strings.HasPrefix(p, "mcp__") && !strings.Contains(p[len("mcp__"):], "__") {
		return strings.HasPrefix(name, p+"__")
	}
	return name == p
}

func matchesAnyToolPattern(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if toolNameMatchesPattern(name, pattern) {
			return true
		}
	}
	return false
}

// permitsToolNameLocked applies every installed layer as an intersection.
// Caller holds Registry.mu.
func (r *Registry) permitsToolNameLocked(name string) bool {
	for _, policy := range r.visibilityPolicies {
		if len(policy.allow) > 0 && !matchesAnyToolPattern(name, policy.allow) {
			return false
		}
		if matchesAnyToolPattern(name, policy.disallow) {
			return false
		}
	}
	return true
}

// ExpandToolPatterns resolves user-supplied patterns against the
// current registry into the concrete set of registered tool names they
// match. Returns a set (map[name]struct{}) for O(1) membership testing
// downstream. The registry's All() snapshot is taken once.
//
// Exposed (capital E) so tests in other packages and future callers
// (e.g. /tools status output) can reuse the same matcher.
func ExpandToolPatterns(reg *Registry, patterns []string) map[string]struct{} {
	out := make(map[string]struct{}, len(patterns))
	if reg == nil || len(patterns) == 0 {
		return out
	}
	all := reg.All()
	names := make([]string, 0, len(all))
	for _, t := range all {
		names = append(names, t.Name())
	}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		for _, n := range names {
			if toolNameMatchesPattern(n, p) {
				out[n] = struct{}{}
			}
		}
	}
	return out
}

// ApplyToolVisibility installs a durable policy and shrinks the current
// registry to the intersection of:
//   - tools matched by `allow` (skipped when allow is empty)
//   - tools NOT matched by `disallow`
//
// Future Register/Replace/ReplacePrefix calls pass through the same policy.
//
// Both `allow` and `disallow` use the ExpandToolPatterns grammar. No-op
// when both are empty.
//
// Order matters: allow narrows first, then disallow subtracts. This
// matches user mental model — "give me Read+Edit+Bash, minus Bash"
// yields {Read, Edit} as expected.
func ApplyToolVisibility(reg *Registry, allow, disallow []string) {
	if reg == nil {
		return
	}
	if len(allow) == 0 && len(disallow) == 0 {
		return
	}

	policy := toolVisibilityPolicy{
		allow:    append([]string(nil), allow...),
		disallow: append([]string(nil), disallow...),
	}
	reg.mu.Lock()
	reg.visibilityPolicies = append(reg.visibilityPolicies, policy)
	order := reg.order[:0]
	for _, name := range reg.order {
		if reg.permitsToolNameLocked(name) {
			order = append(order, name)
			continue
		}
		reg.clearAliasesOf(name)
		delete(reg.tools, name)
	}
	reg.order = order
	reg.mu.Unlock()
}

// SplitCSV splits a comma- or whitespace-separated list of tool
// patterns into a clean slice. Empty input → empty slice. Used by both
// the CLI flag parser and config loader so the two share the same
// quirks (trim, drop empties, tolerate trailing comma).
//
// Mirrors claude-code's `--tools "A,B" / --tools "A B"` dual syntax;
// users on either muscle memory get the same result.
func SplitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
