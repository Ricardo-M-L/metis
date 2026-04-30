// Package tool defines the public Tool contract a 3rd-party plugin
// implements. It pairs with pkg/llm (provider) as the second public
// extension point in Metis's plugin SDK.
//
// The split mirrors openclaw's `packages/plugin-sdk` and openclaude's
// `Tool.ts` interface: types here are the API surface plugins compile
// against; internal/tools holds the registry implementation that hosts
// them. As long as a 3rd party only imports pkg/tool, their plugin
// stays compatible across Metis internal refactors.
//
// To author a plugin tool:
//
//	type MyTool struct{}
//	func (MyTool) Name() string                                       { return "MyTool" }
//	func (MyTool) Description() string                                { return "..." }
//	func (MyTool) InputSchema() map[string]any                        { return map[string]any{...} }
//	func (MyTool) Concurrency() tool.Concurrency                      { return tool.ConcurrencySafe }
//	func (MyTool) CanUse(ctx context.Context, in map[string]any)
//	         (tool.Permission, string)                                { return tool.PermissionAllow, "" }
//	func (MyTool) Execute(ctx context.Context, in map[string]any)
//	         (*tool.Result, error)                                    { ... }
package tool

import "context"

// Permission is the result of a permission check on a specific invocation.
//
//   - PermissionAsk: the runtime should prompt the user before running.
//   - PermissionAllow: pre-approved (e.g. by mode=auto, or by config rule).
//   - PermissionDeny: refuse this invocation entirely.
type Permission int

const (
	PermissionAsk Permission = iota
	PermissionAllow
	PermissionDeny
)

// Concurrency declares whether the tool can run alongside other tools in
// the same streamed batch.
//
//   - ConcurrencySafe: read-only or otherwise side-effect-free — fan out
//     fearlessly. Network tools (WebFetch) belong here too: claude-code
//     / openclaude / hermes all classify their equivalents as safe.
//     Different URLs hit different domains and Go's http client pools
//     connections; rate-limit politeness lives at the HTTP layer.
//   - ConcurrencyQueue: tools that share a non-thread-safe resource and
//     must serialize *with each other* but don't need to block the safe
//     fanout. Concrete fits: a memory-write tool that mutates a single
//     index file; an MCP server pinned to one stdio connection where
//     two simultaneous calls would interleave JSON-RPC messages. Use
//     this only when there's a real shared-state hazard — for "be nice
//     to the network" use Safe and let HTTP retry handle throttling.
//   - ConcurrencyExclusive: write/exec — serialize within the batch
//     AFTER the safe + queue work completes. Bash/Edit/Write live here.
type Concurrency int

const (
	ConcurrencySafe Concurrency = iota
	ConcurrencyExclusive
	ConcurrencyQueue
)

// Result is what a tool returns to the agent loop.
//
// Output is the user-visible textual result; Display is an optional richer
// representation the TUI may pick up (e.g. truncation hints, pre-formatted
// markdown). Meta is a free-form map for tool-specific metadata that the
// agent loop / hooks may inspect.
type Result struct {
	Output  string
	IsError bool
	Display string
	Meta    map[string]any
}

// Tool is the contract every built-in and plugin tool implements.
//
// CanUse is the cheap, synchronous gate: it MUST NOT do network IO or
// expensive computation, since it runs on every invocation under the
// permission lock. Anything more expensive belongs in Execute.
//
// Concurrency takes the actual call's input so a single tool can claim
// different tiers based on what it's doing — claude-code's pattern: a
// Bash run of `ls` is Safe (read-only), `rm -rf` is Exclusive. Tools
// that don't care can ignore the argument; for those, the legacy
// `Concurrency() Concurrency` contract is equivalent.
type Tool interface {
	Name() string
	Description() string
	InputSchema() map[string]any
	Concurrency(input map[string]any) Concurrency
	CanUse(ctx context.Context, input map[string]any) (Permission, string)
	Execute(ctx context.Context, input map[string]any) (*Result, error)
}
