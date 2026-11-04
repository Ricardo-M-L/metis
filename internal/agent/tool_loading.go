package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

// boundedToolSpecs changes only this request's projection. Registry remains the
// complete permission-filtered catalog searched by ToolSearch. The stable Direct
// prefix never gets truncated; an impossible Direct-only request is rejected by
// the adapter's preflight instead of silently losing capabilities.
func boundedToolSpecs(specs []llm.ToolSpec, reg *tools.Registry, preserve map[string]bool, recent []string, limit int) []llm.ToolSpec {
	if limit <= 0 || len(specs) <= limit {
		return specs
	}
	deferred := make(map[string]bool)
	for _, entry := range reg.ModelEntriesForCache() {
		if entry.Exposure == tools.ToolExposureDeferred {
			deferred[entry.Tool.Name()] = true
		}
	}
	specs = stripAndAppendToolSearchWithExposure(specs, deferred, preserve)
	if len(specs) <= limit {
		return specs
	}
	direct := 0
	available := make(map[string]bool)
	for _, s := range specs {
		if deferred[s.Name] {
			available[s.Name] = true
		} else {
			direct++
		}
	}
	slots := limit - direct
	keep := make(map[string]bool)
	for i := len(recent) - 1; i >= 0 && len(keep) < slots; i-- {
		if available[recent[i]] {
			keep[recent[i]] = true
		}
	}
	// Stable fallback for snapshots produced before recency tracking existed.
	for _, s := range specs {
		if len(keep) >= slots {
			break
		}
		if available[s.Name] {
			keep[s.Name] = true
		}
	}
	out := make([]llm.ToolSpec, 0, len(specs))
	for _, s := range specs {
		if !deferred[s.Name] || keep[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

func (l *Loop) boundToolSpecs(specs []llm.ToolSpec, preserve map[string]bool) []llm.ToolSpec {
	l.mu.RLock()
	p := l.Provider
	recent := append([]string(nil), l.discoveredMCPRecent...)
	l.mu.RUnlock()
	return boundedToolSpecs(specs, l.Registry, preserve, recent, pubprovider.ToolDefinitionLimit(p))
}

func touchDiscovered(recent []string, name string) []string {
	for i, n := range recent {
		if n == name {
			recent = append(recent[:i], recent[i+1:]...)
			break
		}
	}
	return append(recent, name)
}
