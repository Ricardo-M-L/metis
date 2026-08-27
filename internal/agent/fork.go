package agent

// fork.go — runForkedAgent for metis. Lets a caller (extractMemories,
// future autoDream / btw / promptSuggestion) spin up a sub-agent that:
//
//   1. Inherits the parent's prompt-cache key (system / tools / model /
//      message prefix unchanged) so each fork pays cache-read prices,
//      not cache-create prices.
//   2. Runs an independent query loop (no shared turn counter, no
//      Detector / Steer / Halt / Compactor — those belong to the main
//      conversation).
//   3. Has its own per-tool whitelist via CanUseToolFn — extractMemories
//      uses this to allow Read/Grep/Glob unrestricted but pin Edit/Write
//      to the memdir.
//   4. Bounds itself by maxTurns to avoid runaway burn — the extractor
//      should be 2-4 turns, an autoDream pass might be more; never
//      unbounded.
//
// Mirrors openclaude's `utils/forkedAgent.ts::runForkedAgent` minus
// JS-specific bits (Bun feature flags, REPL VM context). The Go shape
// is simpler because metis uses non-streaming Complete + a per-tool
// callback, while openclaude reuses its streaming `query` generator.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// CacheSafeParams are the cache-critical inputs that must be IDENTICAL
// to the parent's last request for prompt-cache reuse. The Anthropic
// cache key composes (model, system, tools, messages prefix) — change
// any of those and the fork pays full input price.
//
// Build via SnapshotForFork(loop) at the moment the parent finishes a
// turn; never construct manually because the canonical "system + memory
// context" string is non-trivial.
type CacheSafeParams struct {
	// System is the FINALIZED system prompt the parent used (raw
	// Loop.System + Memory.BuildContext output, concatenated). Must
	// match exactly to share cache.
	System string
	// SystemSections preserves the parent's typed prompt layout, including the
	// independent runtime-state baseline. Each snapshot owns its slice so a
	// fork cannot observe later parent mutations.
	SystemSections []llm.SystemSection

	// ToolSpecs is the parent's tool list at fork time. Order matters —
	// metis already sorts via Registry.SortedForCache(), but if the
	// parent stripped MCP schemas via lazy mode, the fork must too. We
	// snapshot what was actually sent.
	ToolSpecs []llm.ToolSpec

	// PrefixMessages is the parent's full Messages slice at fork time.
	// The fork prepends its own user prompt after this prefix. Caller
	// is responsible for trimming if it wants only N most recent
	// turns (extractMemories trims to last 30).
	PrefixMessages []llm.Message

	// Model / Effort / Provider must match the parent's.
	Model    string
	Effort   llm.Effort
	Provider llm.Provider
}

// SnapshotForFork captures the parent loop's CacheSafeParams under its
// mutex. Returns a copy that's safe to retain past Run's return —
// PrefixMessages is a fresh slice, ToolSpecs is recomputed, System is
// the finalized string (same composition Loop.buildRequest uses).
//
// Returns nil if loop is nil or has no Provider — both are configuration
// bugs the caller should detect, but we don't panic to keep the auto-
// memory codepath best-effort.
//
// Prefix is auto-snipped before being copied: tool_results in the
// parent's history get capped at PostCompactMaxToolResultChars when
// the parent estimate is over forkPrefixSnipThreshold. Without this,
// a long-running parent session forks against ITS full history,
// which routinely overflows MiniMax's wire cap (the user's
// 2026-05-08 report — Fork sub-agent hits 400/2013 mid-extraction).
// The compactor's snip pass uses different markers ("[snipped:")
// than the post-compact pass ("[truncated post-compact:"), so the
// idempotency guards in both Compactor.Snip and EnforcePostCompactBudget
// leave the fork's snipped slices alone on subsequent passes.
func SnapshotForFork(loop *Loop) *CacheSafeParams {
	if loop == nil {
		return nil
	}
	loop.mu.Lock()
	provider := loop.Provider
	compactor := loop.Compactor
	if provider == nil {
		loop.mu.Unlock()
		return nil
	}
	system := loop.System
	sections := make([]llm.SystemSection, 0, len(loop.SystemSections)+3)
	if len(loop.SystemSections) > 0 {
		for _, section := range loop.SystemSections {
			if section.Name != "plan_mode" {
				sections = append(sections, section)
			}
		}
		if loop.planMode && loop.PlanSystemPrompt != "" {
			sections = append(sections, llm.SystemSection{
				Name: "plan_mode", Body: loop.PlanSystemPrompt, Volatile: true,
			})
		}
		if state, ok := loop.currentRuntimeStateSectionLocked(); ok {
			sections = append(sections, llm.SystemSection{
				Name: "runtime_state", Body: state.body, Cache: true,
			})
		}
		if loop.Memory != nil {
			if memCtx := loop.Memory.BuildContext(); memCtx != "" {
				sections = append(sections, llm.SystemSection{
					Name: "memory", Body: memCtx, Volatile: true,
				})
			}
		}
	} else {
		if loop.planMode && loop.PlanSystemPrompt != "" {
			system += "\n\n" + loop.PlanSystemPrompt
		}
		if state, ok := loop.currentRuntimeStateSectionLocked(); ok {
			system += "\n\n" + state.body
		}
		if loop.Memory != nil {
			if memCtx := loop.Memory.BuildContext(); memCtx != "" {
				system += "\n\n" + memCtx
			}
		}
	}
	prefix := append([]llm.Message(nil), loop.Messages...)
	model := loop.Model
	// Effort has its own lock and may change while the parent snapshot is
	// assembled; capture one valid value for the fork.
	effort := loop.EffortValue()
	loop.mu.Unlock()

	// Pre-emptive prefix snip — applies the post-compact budget cap
	// to every tool_result in the prefix so the fork doesn't inherit
	// a 5MB grep dump from the parent. Idempotent (already-capped
	// blocks are skipped via the marker check inside
	// EnforcePostCompactBudget). Cheap when the prefix is small.
	if compactor != nil {
		prefix = compactor.EnforcePostCompactBudget(prefix)
	}

	return &CacheSafeParams{
		System:         system,
		SystemSections: append([]llm.SystemSection(nil), sections...),
		ToolSpecs:      loop.toolSpecs(), // already cache-stable order
		PrefixMessages: prefix,
		Model:          model,
		Effort:         effort,
		Provider:       provider,
	}
}

// CanUseToolFn is the per-tool gate consulted before EVERY tool dispatch
// in the fork. Returning (false, reason) yields a tool_result with
// is_error=true and the reason as content — the model sees the rejection
// and can adapt. Distinct from the main agent's permission.Gate, which
// is configurable via user policy; the fork gate is hard-coded by the
// caller (extractMemories restricts to memdir; autoDream may have its
// own rules).
//
// input is the same map[string]any the model produced — implementations
// are free to type-assert specific keys (e.g. "file_path" for Edit).
type CanUseToolFn func(ctx context.Context, toolName string, input map[string]any) (bool, string)

// AllowAll is the trivial CanUseToolFn — every tool runs with the
// parent's normal permission flow. Use ONLY when you've verified the
// fork's prompt cannot trick the model into writing outside expected
// scopes.
func AllowAll(context.Context, string, map[string]any) (bool, string) {
	return true, ""
}

// ForkedAgentParams configures one RunForkedAgent invocation.
type ForkedAgentParams struct {
	Cache CacheSafeParams

	// Prompt is the user message the fork starts with. Prepended after
	// PrefixMessages. Required.
	Prompt string

	// CanUseTool gates every tool dispatch. Required — even AllowAll
	// must be passed explicitly so callers can't forget by default.
	CanUseTool CanUseToolFn

	// Registry is where tool names are looked up. Required.
	Registry *tools.Registry

	// MaxTurns caps API round-trips. 0 → defaults to 5 (extractor's
	// well-behaved limit). Set higher for autoDream-style consolidation.
	MaxTurns int

	// MaxTokens optionally caps each response's output budget. 0 means
	// provider default. CAUTION: setting this changes thinking budget
	// on some providers (Anthropic), invalidating the parent cache —
	// only set when cache reuse isn't a goal.
	MaxTokens int

	// Hooks are emitted around the fork (SubagentStart / SubagentStop).
	// Optional — nil silences instrumentation but the fork still runs.
	Hooks *HookRegistry

	// ForkLabel is the analytics tag passed to subagent hooks
	// ("extract_memories", "auto_dream", "btw", …). Free-form.
	ForkLabel string

	// Memory lets the fork append a memory block update IF the prompt
	// asks the model to summarize into memory rather than write files.
	// Currently unused (extractMemories writes files via tools); kept
	// for future autoDream that may directly edit the in-process Core
	// block.
	Memory *memory.MemoryManager
}

// ForkedResult is what RunForkedAgent returns.
type ForkedResult struct {
	// NewMessages is everything the fork added on top of PrefixMessages
	// — assistant turns + tool_result user turns. The parent does NOT
	// merge these into its transcript (that's the whole point of a
	// fork); callers wanting the assistant's text use
	// LastAssistantText below.
	NewMessages []llm.Message

	// Usage accumulates across every API round-trip in the fork.
	Usage UsageTotals

	// Turns is the number of API round-trips actually made (≤ MaxTurns).
	Turns int

	// StopReason is the LAST round-trip's stop reason — usually
	// "end_turn". "max_turns" when we hit the cap with the model still
	// wanting tools.
	StopReason string

	// Duration is wall-clock from RunForkedAgent entry to return.
	Duration time.Duration
}

// UsageTotals mirrors usageTotals (private to the streaming path) but
// is exported because ForkedResult is consumed across packages. Keep
// in lock-step with streaming.go's struct.
type UsageTotals struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// LastAssistantText returns the concatenated text content of the last
// assistant message in the fork. Convenience for extractor-style forks
// that only care about the model's final summary, not the tool dance.
func (r *ForkedResult) LastAssistantText() string {
	for i := len(r.NewMessages) - 1; i >= 0; i-- {
		m := r.NewMessages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// RunForkedAgent runs a sub-agent until end_turn or maxTurns. Bounds:
//
//   - Bounded API round-trips: never exceeds MaxTurns.
//   - Bounded by ctx: cancellation propagates immediately.
//   - Bounded usage: every API call's tokens accumulate into Usage so
//     the caller can log "extract burned X tokens".
//   - Bounded errors: a single tool failure becomes a tool_result with
//     IsError=true; the agent loop continues. Provider-level errors
//     (network, 4xx) abort the fork and are returned.
//
// The fork doesn't consult the parent's permission.Gate — it has its
// own CanUseToolFn. Tools' CanUse method IS called (it's part of the
// Tool contract and may do cheap input validation), but a Permission
// of Ask is treated as Deny in the fork (you can't surface a UI prompt
// from a background extraction).
func RunForkedAgent(ctx context.Context, p ForkedAgentParams) (*ForkedResult, error) {
	if p.Prompt == "" {
		return nil, fmt.Errorf("forkedAgent: empty prompt")
	}
	if p.CanUseTool == nil {
		return nil, fmt.Errorf("forkedAgent: nil CanUseTool")
	}
	if p.Registry == nil {
		return nil, fmt.Errorf("forkedAgent: nil Registry")
	}
	if p.Cache.Provider == nil {
		return nil, fmt.Errorf("forkedAgent: nil Provider in CacheSafeParams")
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = 5
	}

	start := time.Now()
	tc := HookContext{Model: p.Cache.Model}

	if p.Hooks != nil {
		p.Hooks.EmitSubagentStart(ctx, tc, &pubhook.SubagentStart{
			Context: tc, SubagentID: p.ForkLabel, Prompt: p.Prompt, Profile: p.ForkLabel,
		})
	}

	// Build initial messages: parent prefix + new user prompt.
	msgs := make([]llm.Message, 0, len(p.Cache.PrefixMessages)+8)
	msgs = append(msgs, p.Cache.PrefixMessages...)
	msgs = append(msgs, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: p.Prompt}},
	})

	prefixLen := len(p.Cache.PrefixMessages)
	result := &ForkedResult{}

	defer func() {
		result.Duration = time.Since(start)
		if p.Hooks != nil {
			p.Hooks.EmitSubagentStop(ctx, tc, &pubhook.SubagentStop{
				Context:    tc,
				SubagentID: p.ForkLabel,
				StopReason: result.StopReason,
				Output:     result.LastAssistantText(),
			})
		}
	}()

	for turn := 0; turn < p.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			result.NewMessages = sliceFrom(msgs, prefixLen)
			result.StopReason = "ctx_cancelled"
			return result, err
		}

		req := llm.Request{
			Model:          p.Cache.Model,
			System:         p.Cache.System,
			SystemSections: append([]llm.SystemSection(nil), p.Cache.SystemSections...),
			Tools:          p.Cache.ToolSpecs,
			Messages:       append([]llm.Message(nil), msgs...),
			Effort:         p.Cache.Effort,
			MaxTokens:      p.MaxTokens,
		}
		resp, err := p.Cache.Provider.Complete(ctx, req)
		if err != nil && ClassifyError(err) == ErrContextOverflow {
			// Sub-agent's request bounced with overflow (typically
			// MiniMax's "request entity too large (2013)" — see
			// error_classifier.go). Snip every tool_result in the
			// in-flight messages to the post-compact cap and retry
			// once. Fork has no Compactor of its own, so we go directly
			// to the cheaper byte-cap pass — full LLM-summarization
			// would defeat the fork's "background, no extra LLM cost"
			// invariant.
			snipped := snipForkMessages(msgs)
			req.Messages = snipped
			resp, err = p.Cache.Provider.Complete(ctx, req)
			if err == nil {
				// Persist the trimmed view so subsequent turns don't
				// rebuild the bloated request.
				msgs = snipped
			}
		}
		if err != nil {
			result.NewMessages = sliceFrom(msgs, prefixLen)
			result.StopReason = "error"
			return result, fmt.Errorf("forkedAgent: provider: %w", err)
		}
		result.Turns++
		result.Usage.InputTokens += resp.InputTokens
		result.Usage.OutputTokens += resp.OutputTokens
		result.Usage.CacheCreationInputTokens += resp.CacheCreationInputTokens
		result.Usage.CacheReadInputTokens += resp.CacheReadInputTokens
		result.StopReason = resp.StopReason

		// Append assistant turn.
		msgs = append(msgs, llm.Message{
			Role: llm.RoleAssistant, Content: resp.Content,
		})

		toolUses := filterForkToolUses(resp.Content)
		if len(toolUses) == 0 {
			// Natural stop — model has nothing more to do.
			break
		}

		// Run each tool through the gate, collect results.
		results := make([]llm.ContentBlock, 0, len(toolUses))
		for _, tu := range toolUses {
			block := executeForkTool(ctx, p, tu)
			results = append(results, block)
		}
		msgs = append(msgs, llm.Message{
			Role: llm.RoleUser, Content: results,
		})
	}
	if result.Turns >= p.MaxTurns && result.StopReason == "tool_use" {
		result.StopReason = "max_turns"
	}
	result.NewMessages = sliceFrom(msgs, prefixLen)
	return result, nil
}

// executeForkTool runs one tool_use in the fork. Returns a tool_result
// block (success or error) — never panics, never returns from the fork
// on tool failure (a memo extraction shouldn't blow up because one
// Glob errored).
func executeForkTool(ctx context.Context, p ForkedAgentParams, tu llm.ContentBlock) llm.ContentBlock {
	id := tu.ToolUseID
	name := tu.ToolName

	// 1. CanUseTool gate (caller-supplied).
	allow, reason := p.CanUseTool(ctx, name, tu.ToolInput)
	if !allow {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: id, IsError: true,
			ToolResult: fmt.Sprintf("denied by fork canUseTool: %s", reason),
		}
	}

	// 2. Registry lookup.
	t, ok := p.Registry.Get(name)
	if !ok {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: id, IsError: true,
			ToolResult: fmt.Sprintf("unknown tool %q in fork registry", name),
		}
	}

	// 3. Skip the Tool's own CanUse — that path goes through metis's
	//    main permission.Gate (yolo classifier, dangerous-pattern
	//    filter, ask/deny by mode). Forks have their own gate (the
	//    caller-supplied CanUseToolFn above) and shouldn't be cross-
	//    contaminated. extractMemories' fork gates Edit/Write to
	//    memdir; without skipping the main Gate, any "yolo immune"
	//    write rule would block the fork's whole point.
	//
	//    This mirrors openclaude: forkedAgent runs with its OWN
	//    canUseTool, not the parent's permission flow.

	// 4. Execute. Any error becomes a tool_result with is_error=true.
	res, err := t.Execute(ctx, tu.ToolInput)
	if err != nil {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: id, IsError: true,
			ToolResult: fmt.Sprintf("execute error: %v", err),
		}
	}
	if res == nil {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: id,
			ToolResult: "(no output)",
		}
	}
	return llm.ContentBlock{
		Type: "tool_result", ToolUseID: id,
		ToolResult: res.Output, IsError: res.IsError,
	}
}

// filterForkToolUses returns just the tool_use blocks from a content
// list. Mirrors filterToolUses in streaming.go but local so we don't
// take a dep on private helpers. Single-pass, allocation-light.
func filterForkToolUses(content []llm.ContentBlock) []llm.ContentBlock {
	var out []llm.ContentBlock
	for _, b := range content {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

func sliceFrom(msgs []llm.Message, from int) []llm.Message {
	if from < 0 || from >= len(msgs) {
		return nil
	}
	out := make([]llm.Message, len(msgs)-from)
	copy(out, msgs[from:])
	return out
}

// inflightForks tracks how many forks are currently running, exposed
// via ForkInflight(). Used by tests and the future /memory show
// command to display "auto-memory extracting…" status.
var (
	inflightMu sync.Mutex
	inflight   int
)

// ForkInflight returns the count of in-flight forks. Snapshot — may
// race; informational only.
func ForkInflight() int {
	inflightMu.Lock()
	defer inflightMu.Unlock()
	return inflight
}

func incInflight() {
	inflightMu.Lock()
	inflight++
	inflightMu.Unlock()
}

func decInflight() {
	inflightMu.Lock()
	inflight--
	inflightMu.Unlock()
}

// snipForkMessages caps every tool_result in the fork's in-flight
// message slice at PostCompactMaxToolResultChars. Same contract as
// Compactor.EnforcePostCompactBudget but operates without a Compactor
// instance — fork can't borrow the parent's because the parent might
// be mid-iteration. Returns the input slice unchanged when no
// tool_result needed clamping.
func snipForkMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	mutated := false
	for i := range out {
		newContent := make([]llm.ContentBlock, len(out[i].Content))
		copy(newContent, out[i].Content)
		rowChanged := false
		for bi := range newContent {
			b := &newContent[bi]
			if b.Type != "tool_result" {
				continue
			}
			if len(b.ToolResult) <= PostCompactMaxToolResultChars {
				continue
			}
			if strings.Contains(b.ToolResult, "[truncated post-compact:") ||
				strings.Contains(b.ToolResult, "[snipped:") {
				continue
			}
			head := b.ToolResult[:PostCompactMaxToolResultChars]
			// Don't cut mid-rune: back up to a UTF-8 boundary so the snipped
			// result isn't invalid encoding that a strict gateway (the
			// MiniMax overflow path this recovers) would reject.
			for len(head) > 0 && !utf8.RuneStart(head[len(head)-1]) {
				head = head[:len(head)-1]
			}
			omitted := len(b.ToolResult) - len(head)
			b.ToolResult = head + fmt.Sprintf("\n[truncated post-compact: %d chars omitted]", omitted)
			rowChanged = true
		}
		if rowChanged {
			out[i].Content = newContent
			mutated = true
		}
	}
	if !mutated {
		return messages
	}
	return out
}

// RunForkedAgentInstrumented wraps RunForkedAgent with the inflight
// counter increment / decrement. Use this when the fork is fired in
// the background (extractMemories does this); the un-instrumented
// version is for synchronous callers (tests).
func RunForkedAgentInstrumented(ctx context.Context, p ForkedAgentParams) (*ForkedResult, error) {
	incInflight()
	defer decInflight()
	return RunForkedAgent(ctx, p)
}
