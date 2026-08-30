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
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// InvocationPreparer is an optional capability for tools whose Execute path
// consumes a one-shot filesystem or other authorization binding. Callers that
// already applied their own permission boundary (for example a forked agent)
// may prepare that binding without re-running a gate captured by the concrete
// tool. The same invocation ID must be present in ctx for preparation and
// execution.
//
// This deliberately is not part of the public Tool interface: ordinary tools
// and third-party plugins preserve their existing CanUse/Execute lifecycle.
type InvocationPreparer interface {
	PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error
}

// Aliases for the public API surface in pkg/tool. Existing call sites
// keep using `tools.Tool`, `tools.PermissionAllow` etc. — these aliases
// are the same types, just reachable from this package.
type (
	Permission        = pubtool.Permission
	Concurrency       = pubtool.Concurrency
	Result            = pubtool.Result
	Tool              = pubtool.Tool
	BaseTool          = pubtool.BaseTool
	ToolExposure      = pubtool.ToolExposure
	ExposureAware     = pubtool.ExposureAware
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
	ToolExposureDirect    = pubtool.ToolExposureDirect
	ToolExposureDeferred  = pubtool.ToolExposureDeferred
	ToolExposureHidden    = pubtool.ToolExposureHidden
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
	CanAutoAllowInBypass    = pubtool.CanAutoAllowInBypass
	GetInterruptBehavior    = pubtool.GetInterruptBehavior
	DescriptionFor          = pubtool.DescriptionFor
	ExposureOf              = pubtool.ExposureOf
	MaxResultSizeChars      = pubtool.MaxResultSizeChars
	TimeoutMs               = pubtool.TimeoutMs
)

// Spill threshold constants — see pkg/tool for the rationale.
const (
	DefaultMaxResultSizeChars = pubtool.DefaultMaxResultSizeChars
	ResultSizeUnlimited       = pubtool.ResultSizeUnlimited
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
	// aliases maps alternative names → canonical Name() for tools that
	// implement pubtool.Aliaser. Resolved by Get() only — aliases never
	// appear in All()/SortedForCache(), so the LLM sees one name while
	// old transcripts and configs using a prior name keep resolving.
	aliases map[string]string
	// visibilityPolicies are durable, intersecting allow/deny layers. They are
	// retained so tools published after startup (plugins, MCP reconnects and IDE
	// bridges) cannot bypass a policy that was applied to the initial snapshot.
	visibilityPolicies []toolVisibilityPolicy
}

// ToolEntry is the canonical model-facing catalog record. Exposure is
// resolved once per snapshot so schema construction, ToolSearch and dispatch
// can make the same decision without inferring policy from a name prefix.
type ToolEntry struct {
	Tool     Tool
	Exposure ToolExposure
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
	if t == nil {
		panic("cannot register a nil tool")
	}
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		panic(fmt.Sprintf("tool %q already registered", name))
	}
	if !r.acceptToolLocked(t) {
		return
	}
	r.tools[name] = t
	r.order = append(r.order, name)
	r.indexAliases(t)
}

// clearAliasesOf removes every alias entry currently pointing at name.
// Caller holds mu. Run before re-indexing on Replace so a tool that
// dropped or changed its alias set doesn't leave stale entries that
// keep resolving to it — and so a freed alias can be reclaimed by
// another tool on its next (re-)index.
func (r *Registry) clearAliasesOf(name string) {
	for a, canon := range r.aliases {
		if canon == name {
			delete(r.aliases, a)
		}
	}
}

// indexAliases records t's declared aliases (if any). Caller holds mu.
// A real tool name always wins over an alias — Get checks r.tools
// first — and the first tool to claim an alias keeps it; later
// claimants are skipped rather than silently stealing resolution.
func (r *Registry) indexAliases(t Tool) {
	names := pubtool.Aliases(t)
	if len(names) == 0 {
		return
	}
	if r.aliases == nil {
		r.aliases = make(map[string]string, len(names))
	}
	for _, a := range names {
		if a == "" || a == t.Name() {
			continue
		}
		if _, taken := r.aliases[a]; taken {
			continue
		}
		r.aliases[a] = t.Name()
	}
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
	if t == nil {
		return
	}
	name := t.Name()
	if !r.acceptToolLocked(t) {
		r.removeLocked(name)
		return
	}
	if _, exists := r.tools[name]; !exists {
		// First-time install — fall through to the order-appending path.
		r.tools[name] = t
		r.order = append(r.order, name)
		r.indexAliases(t)
		return
	}
	// Re-index from a clean slate: drop the prior tool's alias claims
	// before recording the replacement's, so dropped aliases stop
	// resolving and freed ones can be reclaimed (2026-06-12 review).
	r.clearAliasesOf(name)
	r.tools[name] = t
	r.indexAliases(t)
}

// ReplacePrefix atomically replaces every tool whose canonical name starts
// with prefix. It is used when a live MCP server reconnects: the newly
// discovered namespace must replace the prior client's complete surface, not
// merely overwrite names that happen to still exist. Otherwise removed tools
// keep pointers to a closed/sticky-failed server.
func (r *Registry) ReplacePrefix(prefix string, replacements []Tool) {
	if prefix == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	wanted := make(map[string]Tool, len(replacements))
	for _, t := range replacements {
		if t == nil || !strings.HasPrefix(t.Name(), prefix) || !r.acceptToolLocked(t) {
			continue
		}
		wanted[t.Name()] = t
	}

	order := make([]string, 0, len(r.order)+len(wanted))
	seen := make(map[string]struct{}, len(wanted))
	for _, name := range r.order {
		if !strings.HasPrefix(name, prefix) {
			order = append(order, name)
			continue
		}
		r.clearAliasesOf(name)
		if t, ok := wanted[name]; ok {
			r.tools[name] = t
			r.indexAliases(t)
			order = append(order, name)
			seen[name] = struct{}{}
		} else {
			delete(r.tools, name)
		}
	}
	for _, t := range replacements {
		if t == nil || !strings.HasPrefix(t.Name(), prefix) || !r.acceptToolLocked(t) {
			continue
		}
		name := t.Name()
		if _, ok := seen[name]; ok {
			continue
		}
		replacement := wanted[name]
		r.tools[name] = replacement
		r.indexAliases(replacement)
		order = append(order, name)
		seen[name] = struct{}{}
	}
	r.order = order
}

// Get looks up a tool by name, falling back to declared aliases
// (pubtool.Aliaser) so renamed tools keep resolving for old
// transcripts, configs and models that learned the prior name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.tools[name]; ok {
		return t, ok
	}
	if canonical, ok := r.aliases[name]; ok {
		t, ok := r.tools[canonical]
		return t, ok
	}
	return nil, false
}

// GetModelEntry resolves aliases and returns only tools a model is allowed to
// know about. Hidden tools remain reachable through Get for internal runtime
// composition, but guessed model calls and ToolSearch cannot cross this gate.
func (r *Registry) GetModelEntry(name string) (ToolEntry, bool) {
	if r == nil {
		return ToolEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.lookupLocked(name)
	if !ok || t == nil || !t.IsEnabled() {
		return ToolEntry{}, false
	}
	exposure := effectiveExposure(t)
	if exposure == ToolExposureHidden {
		return ToolEntry{}, false
	}
	return ToolEntry{Tool: t, Exposure: exposure}, true
}

// GetForModel is the compact form used by model-originated execution paths.
func (r *Registry) GetForModel(name string) (Tool, bool) {
	entry, ok := r.GetModelEntry(name)
	if !ok {
		return nil, false
	}
	return entry.Tool, true
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

// ModelEntriesForCache returns Direct tools first and Deferred tools second,
// each segment sorted by canonical name. Hidden/disabled tools are omitted.
// The stable Direct prefix is the prompt-cache anchor; deferred churn cannot
// reorder it.
func (r *Registry) ModelEntriesForCache() []ToolEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	direct := make([]ToolEntry, 0, len(r.order))
	deferred := make([]ToolEntry, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		if t == nil || !t.IsEnabled() {
			continue
		}
		entry := ToolEntry{Tool: t, Exposure: effectiveExposure(t)}
		switch entry.Exposure {
		case ToolExposureHidden:
			continue
		case ToolExposureDeferred:
			deferred = append(deferred, entry)
		default:
			direct = append(direct, entry)
		}
	}
	sort.Slice(direct, func(i, j int) bool { return direct[i].Tool.Name() < direct[j].Tool.Name() })
	sort.Slice(deferred, func(i, j int) bool { return deferred[i].Tool.Name() < deferred[j].Tool.Name() })
	return append(direct, deferred...)
}

// ModelToolsForCache is the Tool-only compatibility projection.
func (r *Registry) ModelToolsForCache() []Tool {
	entries := r.ModelEntriesForCache()
	out := make([]Tool, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Tool)
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

// SortedForCache is the legacy Tool-only form of ModelToolsForCache. It
// returns the model-visible Direct segment followed by Deferred, with each
// segment sorted by canonical name. Hidden and disabled tools are omitted.
//
// Why this matters: Anthropic caches the request prefix; if the tools
// list changes shape, every cache entry past the shape change is invalidated.
// The explicit exposure segments keep stable Direct capabilities contiguous
// while Deferred plugins/MCP tools can grow, shrink or hydrate independently.
//
// Mirrors claude-code's assembleToolPool() in tools.ts:345-367.
//
// Legacy mcp__ tools are treated as Deferred until they adopt ExposureAware.
func (r *Registry) SortedForCache() []Tool {
	return r.ModelToolsForCache()
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
		r.clearAliasesOf(n)
		delete(r.tools, n)
	}
	r.order = newOrder
}

func (r *Registry) lookupLocked(name string) (Tool, bool) {
	if t, ok := r.tools[name]; ok {
		return t, true
	}
	canonical, ok := r.aliases[name]
	if !ok {
		return nil, false
	}
	t, ok := r.tools[canonical]
	return t, ok
}

func (r *Registry) acceptToolLocked(t Tool) bool {
	return t != nil && t.IsEnabled() && r.permitsToolNameLocked(t.Name())
}

func (r *Registry) removeLocked(name string) {
	if _, ok := r.tools[name]; !ok {
		return
	}
	r.clearAliasesOf(name)
	delete(r.tools, name)
	order := r.order[:0]
	for _, current := range r.order {
		if current != name {
			order = append(order, current)
		}
	}
	r.order = order
}

// effectiveExposure keeps old MCP/plugin tools deferred during the migration
// even if they predate ExposureAware. New tools should implement the optional
// interface explicitly; all other legacy tools remain Direct.
func effectiveExposure(t Tool) ToolExposure {
	if t == nil {
		return ToolExposureHidden
	}
	if _, explicit := t.(pubtool.ExposureAware); explicit {
		return pubtool.ExposureOf(t)
	}
	if strings.HasPrefix(t.Name(), "mcp__") {
		return ToolExposureDeferred
	}
	return ToolExposureDirect
}

// EffectiveExposure is the internal compatibility resolver wrappers should
// forward. Unlike pkg/tool.ExposureOf it includes the temporary mcp__ fallback
// for third-party tools compiled before ExposureAware existed.
func EffectiveExposure(t Tool) ToolExposure { return effectiveExposure(t) }
