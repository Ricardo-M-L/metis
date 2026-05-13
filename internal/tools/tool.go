// Package tools holds the in-process Tool registry. The Tool interface
// itself + supporting types (Permission, Concurrency, Result) live in
// pkg/tool so 3rd-party plugins can compile against a stable public
// surface without depending on Metis internals.
//
// This package re-exports the public types as aliases — every existing
// caller (`tools.Tool`, `tools.PermissionAllow`, etc.) keeps working
// without churn.
package tools

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// Aliases for the public API surface in pkg/tool. Existing call sites
// keep using `tools.Tool`, `tools.PermissionAllow` etc. — these aliases
// are the same types, just reachable from this package.
type (
	Permission        = pubtool.Permission
	Concurrency       = pubtool.Concurrency
	Result            = pubtool.Result
	Tool              = pubtool.Tool
	InterruptBehavior = pubtool.InterruptBehavior
)

const (
	PermissionAsk         = pubtool.PermissionAsk
	PermissionAllow       = pubtool.PermissionAllow
	PermissionDeny        = pubtool.PermissionDeny
	ConcurrencySafe       = pubtool.ConcurrencySafe
	ConcurrencyExclusive  = pubtool.ConcurrencyExclusive
	ConcurrencyQueue      = pubtool.ConcurrencyQueue
	ConcurrencyBackground = pubtool.ConcurrencyBackground
	InterruptCancel       = pubtool.InterruptCancel
	InterruptBlock        = pubtool.InterruptBlock
)

// Re-exports for the optional capability helpers in pkg/tool. Existing
// callers can stay on `tools.IsReadOnly(t, in)` etc. without importing
// pkg/tool directly.
var (
	IsReadOnly              = pubtool.IsReadOnly
	IsDestructive           = pubtool.IsDestructive
	RequiresUserInteraction = pubtool.RequiresUserInteraction
	IsBypassImmune          = pubtool.IsBypassImmune
	GetInterruptBehavior    = pubtool.GetInterruptBehavior
	DescriptionFor          = pubtool.DescriptionFor
)

// ShortDescriptor — see pkg/tool for the rationale. Re-exported here
// for callers that already import internal/tools.
type ShortDescriptor = pubtool.ShortDescriptor

// Registry holds all tools available to the current session.
// Built-in tools register at init() time; plugins can be added later.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string // preserves registration order for deterministic listing
}

// global registry; built-in packages register themselves into it via init().
var global = &Registry{tools: make(map[string]Tool)}

// Global returns the process-wide registry.
func Global() *Registry { return global }

// NewRegistry returns a fresh Registry (used per-session).
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool. Duplicate names panic — they are programmer errors.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		panic(fmt.Sprintf("tool %q already registered", name))
	}
	r.tools[name] = t
	r.order = append(r.order, name)
}

// Replace installs t under its Name(), overwriting any prior tool with
// that name. Used for two-phase wiring where a tool is registered
// up-front with provisional config and re-registered later when more
// dependencies become available (e.g. the Skill tool needs to swap its
// loader once plugins load). Insertion order is preserved on replace —
// the tool keeps its original slot in `order`.
func (r *Registry) Replace(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if _, exists := r.tools[name]; !exists {
		// First-time install — fall through to the order-appending path.
		r.tools[name] = t
		r.order = append(r.order, name)
		return
	}
	r.tools[name] = t
}

// Get looks up a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns tools in registration order.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}

// Filter returns only tools whose name passes keep.
func (r *Registry) Filter(keep func(string) bool) []Tool {
	all := r.All()
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		if keep(t.Name()) {
			out = append(out, t)
		}
	}
	return out
}

// SortedForCache returns tools as: [built-ins sorted by name] then [MCP
// tools sorted by name]. The order is stable across MCP server churn so
// the Anthropic prompt cache breakpoint placed after the built-in
// prefix stays valid.
//
// Why this matters: Anthropic caches the request prefix; if the tools
// list changes shape (an MCP tool sorts between two built-ins, or the
// MCP set grows / shrinks across sessions), every cache entry past the
// shape change is invalidated. By keeping built-ins as a contiguous
// alphabetically-sorted prefix and MCP as a contiguous suffix, only
// the MCP region varies — the breakpoint after the last built-in still
// hits cache for the system+tools prefix.
//
// Mirrors claude-code's assembleToolPool() in tools.ts:345-367.
//
// MCP tools are identified by the `mcp__` prefix that
// internal/tools/mcp/server.go applies to every imported MCP tool.
func (r *Registry) SortedForCache() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	builtins := make([]Tool, 0, len(r.order))
	mcps := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		if strings.HasPrefix(name, "mcp__") {
			mcps = append(mcps, t)
		} else {
			builtins = append(builtins, t)
		}
	}
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name() < builtins[j].Name() })
	sort.Slice(mcps, func(i, j int) bool { return mcps[i].Name() < mcps[j].Name() })
	return append(builtins, mcps...)
}

// Restrict mutates the registry in place to keep only tools whose names
// appear in `keep`. Tools whose names are not in `keep` are dropped from
// both the lookup map and the iteration order. Used by the agent-profile
// loader to apply allowlist + blocklist filtering after the registry is
// already built. Names not present in the registry are ignored.
func (r *Registry) Restrict(keep []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wanted := make(map[string]struct{}, len(keep))
	for _, n := range keep {
		wanted[n] = struct{}{}
	}
	newOrder := r.order[:0]
	for _, n := range r.order {
		if _, ok := wanted[n]; ok {
			newOrder = append(newOrder, n)
			continue
		}
		delete(r.tools, n)
	}
	r.order = newOrder
}
