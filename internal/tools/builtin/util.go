package builtin

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func mapDecision(d permission.Decision) tools.Permission {
	switch d {
	case permission.DecisionAllow:
		return tools.PermissionAllow
	case permission.DecisionDeny:
		return tools.PermissionDeny
	default:
		return tools.PermissionAsk
	}
}

func strFromAny(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return fmt.Sprintf("%v", v)
	}
}

func intArg(in map[string]any, key string, def int) int {
	if v, ok := in[key]; ok {
		switch x := v.(type) {
		case int:
			return x
		case int64:
			return int(x)
		case float64:
			return int(x)
		}
	}
	return def
}

// stringSliceArg extracts a []string from a tool-input map. Tolerates
// the two ways JSON decoders surface arrays — []string (when the
// caller built the map natively) and []any (from json.Unmarshal). Any
// other shape returns an empty slice rather than panicking, so a
// malformed input degrades to "no filter" instead of crashing the
// dispatcher.
func stringSliceArg(in map[string]any, key string) []string {
	v, ok := in[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func intStr(i int) string { return fmt.Sprintf("%d", i) }

// filterRegistry returns a NEW tools.Registry holding only the tools
// from `src` that pass the allow + deny filters (G.14, 2026-05-12).
//
// Semantics:
//   - len(allow)==0 + len(deny)==0 → caller checks before calling, but
//     this returns an empty Registry to keep the contract simple.
//   - len(allow)>0  → allowlist intersected with src's tools.
//   - len(deny)>0   → blocklist applied AFTER allowlist (intersection).
//
// Tools register into the new Registry in their original order via
// src.All(), preserving cache shape. The Tool values themselves are
// shared by pointer — there's no deep copy because Tool is a stateful
// interface and aliasing the same Tool into two registries is
// expected (the gate / provider / etc don't change between them).
func filterRegistry(src *tools.Registry, allow, deny []string) *tools.Registry {
	allowSet := make(map[string]struct{}, len(allow))
	for _, n := range allow {
		allowSet[n] = struct{}{}
	}
	denySet := make(map[string]struct{}, len(deny))
	for _, n := range deny {
		denySet[n] = struct{}{}
	}
	dst := tools.NewRegistry()
	for _, t := range src.All() {
		name := t.Name()
		if len(allowSet) > 0 {
			if _, ok := allowSet[name]; !ok {
				continue
			}
		}
		if _, ok := denySet[name]; ok {
			continue
		}
		dst.Register(t)
	}
	return dst
}

// copyToolRegistry returns a new registry with the same tool values and order.
// Stateful tool implementations are intentionally shared until a caller
// explicitly replaces one; Agent uses this before rebinding a child's Bash job
// tools so the process-wide parent registry is never mutated.
func copyToolRegistry(src *tools.Registry) *tools.Registry {
	dst := tools.NewRegistry()
	if src == nil {
		return dst
	}
	for _, tool := range src.All() {
		dst.Register(tool)
	}
	return dst
}

// intersectAllow combines two allowlists with intersection semantics:
// only names present in BOTH lists survive. Either list being empty
// is treated as "no restriction from this side" — the other list
// passes through unchanged. Q1 (2026-05-15) uses this to combine the
// profile's allowed_tools with the per-invocation allowed_tools so
// the sub-agent always sees the strictest result.
func intersectAllow(a, b []string) []string {
	switch {
	case len(a) == 0 && len(b) == 0:
		return nil
	case len(a) == 0:
		return append([]string(nil), b...)
	case len(b) == 0:
		return append([]string(nil), a...)
	}
	set := make(map[string]struct{}, len(a))
	for _, n := range a {
		set[n] = struct{}{}
	}
	out := make([]string, 0, len(b))
	seen := make(map[string]struct{}, len(b))
	for _, n := range b {
		if _, dup := seen[n]; dup {
			continue
		}
		if _, ok := set[n]; ok {
			out = append(out, n)
			seen[n] = struct{}{}
		}
	}
	return out
}

// unionStrings concatenates two slices, deduplicating. Q1 (2026-05-15)
// uses this to combine the profile's disallowed_tools with the
// per-invocation disallowed_tools — a blocked tool stays blocked no
// matter which side asked for it.
func unionStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, n := range a {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range b {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func bytesString(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
