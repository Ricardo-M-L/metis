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
//   - ConcurrencyBackground: fire-and-forget — the dispatcher records a
//     job_id and returns immediately while the actual work runs in a
//     goroutine backed by jobs.Registry. Mirrors claude-code's
//     `run_in_background: true` semantics on AgentTool. Concrete fits:
//     `Agent({run_in_background: true})` so the parent loop can spawn
//     N sub-agents and keep working; long-running shell commands that
//     would otherwise block a turn. Added 2026-05-12 (Phase G.1).
type Concurrency int

const (
	ConcurrencySafe Concurrency = iota
	ConcurrencyExclusive
	ConcurrencyQueue
	ConcurrencyBackground
)

// Result is what a tool returns to the agent loop.
//
// Output is the user-visible textual result; Display is an optional richer
// representation the TUI may pick up (e.g. truncation hints, pre-formatted
// markdown). Meta is a free-form map for tool-specific metadata that the
// agent loop / hooks may inspect.
//
// Images, when non-empty, attaches inline image blocks to the
// tool_result the model sees on its next turn. Used by vision-aware
// tools (ViewImage, future PDF page rasterisers) so the model can
// actually SEE binary content that lives on disk. Anthropic-family
// providers render the images directly inside the tool_result block;
// OpenAI-style providers receive them as image_url content parts
// alongside the textual tool_result. Output remains the canonical
// text — it doubles as the fallback when the provider doesn't
// support vision and as a one-line summary in the TUI transcript.
type Result struct {
	Output  string
	IsError bool
	Display string
	Meta    map[string]any
	Images  []ImageAttachment
}

// ImageAttachment is one inline image returned by a tool. MediaType is
// a full IANA type ("image/png", "image/jpeg", "image/gif",
// "image/webp"). Data is the base64-encoded payload — the tool itself
// is responsible for encoding raw bytes; the dispatch layer simply
// forwards what's here into the provider-specific wire shape.
type ImageAttachment struct {
	MediaType string
	Data      string
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

	// IsEnabled reports whether the tool is currently available in the
	// running environment. Returning false hides the tool from the
	// model entirely — the registry filters it out before assembling
	// the tools[] list, so it never appears in the prompt and the
	// model can't try to call it. Called once at registration time.
	//
	// Most tools should embed BaseTool to inherit the default
	// "always enabled" implementation. Override only when the tool
	// has a real environment dependency it can detect itself
	// (e.g. LSP checking gopls on PATH).
	//
	// Mirrors claude-code's Tool.isEnabled (Tool.ts:403) — moves the
	// "is this tool present in this environment?" decision from a
	// centralized if-chain in register.go to the tool itself, so the
	// knowledge of what each tool depends on lives with the tool.
	IsEnabled() bool
}

// BaseTool provides the default IsEnabled() = true implementation as
// a zero-size embeddable. Tools that have no environment-specific
// availability check should embed it:
//
//	type Read struct {
//	    BaseTool       // inherits IsEnabled() = true for free
//	    gate  *permission.Gate
//	    state *ReadFileState
//	}
//
// Embedding adds no memory overhead (BaseTool is empty) and removes
// 35+ otherwise-identical `func (Read) IsEnabled() bool { return true }`
// stubs across builtin/. Tools with real availability checks (LSP) just
// don't embed BaseTool and write their own IsEnabled().
type BaseTool struct{}

// IsEnabled returns true — the default for tools without any
// environment-specific availability check.
func (BaseTool) IsEnabled() bool { return true }

// ShortDescriptor is an optional interface tools can implement to
// provide a curated short-form description distinct from Description().
// Used by the dispatch layer when assembling the per-turn tool list
// for sub-agents or simple-mode boots, where the full multi-paragraph
// guidance is wasted tokens.
//
// Pattern is opt-in: a tool that doesn't implement this falls back to
// the dispatch layer's automatic first-paragraph truncation. Tools
// with non-trivial Description() bodies (Bash, Edit, Write, Read,
// Agent) implement it; trivial tools (Glob, LS) don't bother — their
// Description() is already short.
//
// Mirrors claude-code's getSimplePrompt() pattern per tool
// (restored-src/src/tools/BashTool/prompt.ts:275).
type ShortDescriptor interface {
	ShortDescription() string
}

// DescriptionFor returns the description string a Tool would expose at
// the LLM boundary given a "use short form" hint. Falls back to
// Description() when the tool doesn't implement ShortDescriptor — the
// caller (dispatch.go) is responsible for any further truncation.
func DescriptionFor(t Tool, short bool) string {
	if short {
		if sd, ok := t.(ShortDescriptor); ok {
			return sd.ShortDescription()
		}
	}
	return t.Description()
}

// InterruptBehavior controls what happens when the user submits a new
// message while a tool is mid-execution. Mirrors claude-code's Tool.ts:416.
//
//   - InterruptCancel: stop the tool and discard its result. Right for
//     pure-read tools (Read, Grep, WebFetch) where a half-finished
//     answer is just wasted bytes.
//   - InterruptBlock: keep running; the new user message waits in the
//     queue. Right for tools whose half-state is worse than full state
//     (Bash running `make install`, an in-flight Edit that already
//     wrote bytes).
type InterruptBehavior int

const (
	InterruptCancel InterruptBehavior = iota
	InterruptBlock
)

// ReadOnlyAware lets a tool self-report per-input whether the call has
// no observable side effects. The runtime reads this when deciding
// whether a tool_result block can be Snipped (lossy truncation in
// context compaction) safely — read outputs can lose detail, write
// outputs can't because future turns may need the full diff.
//
// Default when not implemented: false (conservative).
type ReadOnlyAware interface {
	IsReadOnly(input map[string]any) bool
}

// Destructive lets a tool self-report per-input whether the call is
// irreversible (rm, DROP, send-message, overwrite-file). The TUI uses
// this to color the ASK prompt; the gate uses it to upgrade ASK to
// stricter confirmation.
//
// Default when not implemented: false.
type Destructive interface {
	IsDestructive(input map[string]any) bool
}

// RequiresUserInteractive lets a tool declare it MUST get human
// involvement to function — e.g. AskUser, an OAuth-flow trigger. Mode
// bypass cannot satisfy these, since "user wants to skip prompts" is
// orthogonal to "tool needs the user to type something". Mirrors
// Tool.ts:435 requiresUserInteraction.
type RequiresUserInteractive interface {
	RequiresUserInteraction() bool
}

// BypassImmuneAware lets a tool declare per-input that its Deny/Ask
// decision must NOT be upgraded to Allow by mode=bypass. Used for
// safety-check paths (.git/config, .ssh/, ~/.bashrc) that should
// always prompt regardless of bypass posture. Returns reason for UI.
//
// Mirrors claude-code's safetyCheck flow in permissions.ts:1144-1152
// and 1252-1260.
type BypassImmuneAware interface {
	IsBypassImmune(input map[string]any) (immune bool, reason string)
}

// Interruptible lets a tool override the default InterruptCancel.
// Most tools don't need this — `Bash`, long-running task tools that
// hold real-world side effects override it.
type Interruptible interface {
	InterruptBehavior() InterruptBehavior
}

// IsReadOnly returns whether t reports the input as read-only.
// Default false (assume side effects) when t does not implement
// ReadOnlyAware.
func IsReadOnly(t Tool, input map[string]any) bool {
	if r, ok := t.(ReadOnlyAware); ok {
		return r.IsReadOnly(input)
	}
	return false
}

// IsDestructive returns whether t reports the input as irreversible.
// Default false.
func IsDestructive(t Tool, input map[string]any) bool {
	if d, ok := t.(Destructive); ok {
		return d.IsDestructive(input)
	}
	return false
}

// RequiresUserInteraction returns whether t needs human input.
// Default false.
func RequiresUserInteraction(t Tool) bool {
	if r, ok := t.(RequiresUserInteractive); ok {
		return r.RequiresUserInteraction()
	}
	return false
}

// IsBypassImmune returns whether t's CanUse decision on the given
// input should resist mode=bypass override.
//
// Default (false, "") — bypass mode can override.
func IsBypassImmune(t Tool, input map[string]any) (bool, string) {
	if b, ok := t.(BypassImmuneAware); ok {
		return b.IsBypassImmune(input)
	}
	return false, ""
}

// GetInterruptBehavior returns t's interrupt policy. Default is
// InterruptCancel.
func GetInterruptBehavior(t Tool) InterruptBehavior {
	if i, ok := t.(Interruptible); ok {
		return i.InterruptBehavior()
	}
	return InterruptCancel
}
