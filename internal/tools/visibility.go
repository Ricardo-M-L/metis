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
// Patterns that match nothing are dropped silently — typing
// `--disallow-tools NoSuchTool` shouldn't crash a real session.

import "strings"

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
		// MCP global wildcard — "mcp__" or "mcp__*" mute every MCP tool.
		if p == "mcp__" || p == "mcp__*" {
			for _, n := range names {
				if strings.HasPrefix(n, "mcp__") {
					out[n] = struct{}{}
				}
			}
			continue
		}
		// MCP server prefix — "mcp__<server>" with no trailing "__" in
		// the server portion means "every tool that server exposes".
		// MCP tool names are always three segments: mcp__<server>__<tool>,
		// so a pattern lacking the third segment is unambiguously a
		// server-level pattern, never a literal tool name.
		if strings.HasPrefix(p, "mcp__") && !strings.Contains(p[len("mcp__"):], "__") {
			prefix := p + "__"
			for _, n := range names {
				if strings.HasPrefix(n, prefix) {
					out[n] = struct{}{}
				}
			}
			continue
		}
		// Exact tool name.
		for _, n := range names {
			if n == p {
				out[n] = struct{}{}
			}
		}
	}
	return out
}

// ApplyToolVisibility shrinks reg in place to the intersection of:
//   - tools currently registered
//   - tools matched by `allow` (skipped when allow is empty)
//   - tools NOT matched by `disallow`
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

	current := reg.All()
	keep := make([]string, 0, len(current))
	for _, t := range current {
		keep = append(keep, t.Name())
	}

	if len(allow) > 0 {
		allowSet := ExpandToolPatterns(reg, allow)
		filtered := keep[:0]
		for _, n := range keep {
			if _, ok := allowSet[n]; ok {
				filtered = append(filtered, n)
			}
		}
		keep = filtered
	}
	if len(disallow) > 0 {
		denySet := ExpandToolPatterns(reg, disallow)
		filtered := keep[:0]
		for _, n := range keep {
			if _, ok := denySet[n]; !ok {
				filtered = append(filtered, n)
			}
		}
		keep = filtered
	}
	reg.Restrict(keep)
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
