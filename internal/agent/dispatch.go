package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// toolSpecs builds the per-request `tools[]` array given to the LLM, by
// asking each registered tool for its name + description + input schema.
//
// Tools are emitted in cache-stable order via Registry.SortedForCache():
// built-ins sorted by name first, then MCP tools sorted by name. This
// keeps the Anthropic prompt-cache breakpoint placed after the last
// built-in valid across MCP server churn — claude-code's tools.ts:354-366.
//
// When lazy mode fires, mcp__-prefixed tools have their schemas
// stripped and a synthetic ToolSearch tool is appended. Saves
// ~10K+ tokens per iteration for MCP-heavy sessions; see lazy_tools.go
// for the trade-off discussion and the env-var match table.
//
// Mode comes from ENABLE_TOOL_SEARCH (read fresh each call so users
// can `export` to flip behavior without restarting metis — same as
// openclaude). Three branches:
//
//   - Standard → no rewrite, full schemas always sent.
//   - Always   → strip every mcp__* schema unconditionally.
//   - Auto     → strip only when the deferred MCP token estimate
//     exceeds `ContextWindow * percentage / 100`.
//
// Auto without a known ContextWindow falls back to "standard" rather
// than guessing — better to send slightly more schema than to make
// the wrong stripping decision and break a tool call.
func (l *Loop) toolSpecs() []llm.ToolSpec {
	all := l.Registry.SortedForCache()
	out := make([]llm.ToolSpec, 0, len(all))
	for _, t := range all {
		out = append(out, llm.ToolSpec{
			Name: t.Name(), Description: shortToolDesc(t.Description()), InputSchema: t.InputSchema(),
		})
	}
	mode, pct := parseEnableToolSearch(os.Getenv("ENABLE_TOOL_SEARCH"))
	switch mode {
	case LazyModeStandard:
		return out
	case LazyModeAlways:
		return stripAndAppendToolSearch(out)
	case LazyModeAuto:
		if l.ContextWindow <= 0 {
			return out
		}
		return applyLazySchemaByTokens(out, l.ContextWindow, pct)
	}
	return out
}

// shortToolDesc trims a tool's full Description() down to the
// short-form the LLM actually needs: the first paragraph, capped at
// 200 chars. Borrowed from Crush's CRUSH_SHORT_TOOL_DESCRIPTIONS
// pattern — a tool's full doc (multi-paragraph spec, examples,
// edge-case discussion) is great for `metis tools` listing but
// inflates every turn's input by ~1500 tokens across 22 tools when
// shipped verbatim. The first paragraph is enough for the model to
// pick the tool; deeper detail goes through InputSchema's parameter
// docs (which we keep intact).
//
// Boundary order (first hit wins): blank line ("\n\n"), single
// newline ("\n"), 200-char cap. We prefer paragraph splits over
// hard caps so we don't sever a half-sentence — only fall back to
// the cap when the doc is one giant run-on paragraph.
func shortToolDesc(full string) string {
	const maxLen = 200
	if i := strings.Index(full, "\n\n"); i >= 0 && i < maxLen {
		return strings.TrimSpace(full[:i])
	}
	if i := strings.Index(full, "\n"); i >= 0 && i < maxLen {
		return strings.TrimSpace(full[:i])
	}
	if len(full) > maxLen {
		return strings.TrimSpace(full[:maxLen]) + "…"
	}
	return strings.TrimSpace(full)
}

// executeBatch runs every tool_use in toolUses, returning the matching
// tool_result blocks. Three phases:
//
//	Phase 0  — per-tool pre-checks: PreToolUse hook + permission CanUse.
//	            Tools whose CanUse returns ASK get queued for batch
//	            confirmation rather than prompting one-at-a-time.
//	Phase 0b — batch ASK confirmation. Each EventPermissionRequest
//	            carries PermissionPending = remaining-asks-in-batch so
//	            the TUI can render "1 of N" rather than the user
//	            facing N independent prompts.
//	Phase 1+ — execute survivors by concurrency tier:
//	             Safe      — fan out in parallel
//	             Queue     — FIFO concurrent with safe fanout
//	             Exclusive — serialized after safe+queue
//
// The phased pattern preserves the "writes happen after reads in the
// same turn" invariant without per-tool dependency analysis. Queue is
// metis-original: useful for rate-limited APIs (WebFetch, an MCP server
// pinned to one connection) that don't need full exclusivity but
// shouldn't run in parallel with each other.
func (l *Loop) executeBatch(ctx context.Context, toolUses []llm.ContentBlock, out chan<- Event, tc HookContext) ([]llm.ContentBlock, error) {
	results := make([]llm.ContentBlock, len(toolUses))
	type job struct {
		idx   int
		blk   llm.ContentBlock
		t     tools.Tool
		early *llm.ContentBlock // set when pre-check decided result
		ready bool              // true once pre-checks all pass
	}
	jobs := make([]*job, len(toolUses))

	// Phase 0a: per-tool pre-checks. ToolSearch and unknown tools are
	// resolved inline (no permission flow needed). Others run their
	// PreToolUse hook + CanUse here so we know which ones need ASK
	// before launching any goroutine.
	asks := make([]*job, 0)
	for i, b := range toolUses {
		// Synthetic ToolSearch (lazy-MCP-schema feature, Task #72) is
		// not in the Registry — handle it inline.
		if b.ToolName == "ToolSearch" {
			results[i] = handleToolSearch(l, b)
			emit(ctx, out, Event{
				Kind: EventToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName,
				ToolResult: &ToolResult{Output: results[i].ToolResult, IsError: results[i].IsError},
			})
			continue
		}
		t, ok := l.Registry.Get(b.ToolName)
		if !ok {
			results[i] = llm.ContentBlock{
				Type: "tool_result", ToolUseID: b.ToolUseID,
				ToolResult: fmt.Sprintf("error: unknown tool %q", b.ToolName), IsError: true,
			}
			emit(ctx, out, Event{
				Kind: EventToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName,
				ToolResult: &ToolResult{Output: "unknown tool", IsError: true},
			})
			continue
		}

		j := &job{idx: i, blk: b, t: t}
		jobs[i] = j

		emit(ctx, out, Event{
			Kind: EventToolStart, ToolUseID: b.ToolUseID,
			ToolName: b.ToolName, ToolInput: b.ToolInput,
		})

		// PreToolUse hook can short-circuit (Output), rewrite input
		// (ModifiedInput), or halt the entire turn (Halt). Halt is
		// claude-code parity: subprocess hook returns
		// `{"decision":"halt"}` or exits with code 49, telling the
		// agent loop to stop after the current tool batch — useful
		// for "veto chain" hooks (the model wandered into a forbidden
		// path; abort the turn rather than just denying one tool).
		if l.Hooks != nil {
			mod := l.Hooks.EmitPreToolUse(ctx, tc, &PreToolUseHook{
				Context: tc, Tool: b.ToolName, Input: b.ToolInput,
			})
			if mod != nil {
				if mod.Halt {
					reason := mod.HaltReason
					if reason == "" {
						reason = "halted by PreToolUse hook"
					}
					l.haltTurn(reason)
				}
				if mod.Output != nil {
					blkOut := llm.ContentBlock{
						Type: "tool_result", ToolUseID: b.ToolUseID,
						ToolResult: mod.Output.Content, IsError: mod.Output.IsError,
					}
					emit(ctx, out, Event{
						Kind: EventToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName,
						ToolResult: &ToolResult{Output: mod.Output.Content, IsError: mod.Output.IsError},
					})
					j.early = &blkOut
					continue
				}
				if mod.ModifiedInput != nil {
					j.blk.ToolInput = mod.ModifiedInput
				}
			}
		}

		// Permission decision.
		perm, _ := t.CanUse(ctx, j.blk.ToolInput)
		if perm == tools.PermissionDeny {
			blkOut := llm.ContentBlock{
				Type: "tool_result", ToolUseID: b.ToolUseID,
				ToolResult: "denied by permission policy", IsError: true,
			}
			emit(ctx, out, Event{
				Kind: EventToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName,
				ToolResult: &ToolResult{Output: "denied", IsError: true},
			})
			j.early = &blkOut
			continue
		}
		if perm == tools.PermissionAsk {
			asks = append(asks, j)
			continue
		}
		j.ready = true
	}

	// Phase 0b: ASK in batch. Each event reports PermissionPending so
	// the TUI knows "you have N more decisions queued behind this one"
	// — the model emitted them as a batch and we keep them grouped
	// rather than letting Execute interleave between asks.
	for ai, j := range asks {
		remaining := len(asks) - ai - 1
		ar := l.askPermissionPending(ctx, j.blk, out, remaining)
		if !ar.proceed {
			j.early = ar.earlyReturn
			continue
		}
		j.ready = true
	}

	// Phase 1: classify ready jobs by concurrency tier and execute.
	var safeJobs, queueJobs, exclJobs []*job
	for _, j := range jobs {
		if j == nil || j.early != nil || !j.ready {
			continue
		}
		switch j.t.Concurrency(j.blk.ToolInput) {
		case tools.ConcurrencySafe:
			safeJobs = append(safeJobs, j)
		case tools.ConcurrencyQueue:
			queueJobs = append(queueJobs, j)
		default:
			exclJobs = append(exclJobs, j)
		}
	}

	// Phase 1a: fan-out safe tools.
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, j := range safeJobs {
		wg.Add(1)
		go func(j *job) {
			defer wg.Done()
			blk := l.runExecute(ctx, j.t, j.blk, out, tc)
			mu.Lock()
			results[j.idx] = blk
			mu.Unlock()
		}(j)
	}

	// Phase 1b (concurrent with 1a): drain the queue jobs FIFO from a
	// single goroutine. Queue tools run alongside the safe fanout but
	// never concurrently with each other — the right shape for rate-
	// limited remote APIs.
	if len(queueJobs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, j := range queueJobs {
				blk := l.runExecute(ctx, j.t, j.blk, out, tc)
				mu.Lock()
				results[j.idx] = blk
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Phase 2: serialize exclusive tools. Order preserved by insertion.
	for _, j := range exclJobs {
		results[j.idx] = l.runExecute(ctx, j.t, j.blk, out, tc)
	}

	// Fill in early-decided results (hook short-circuit / deny / ask-
	// denied) from their job entries.
	for i, j := range jobs {
		if j == nil {
			continue
		}
		if j.early != nil {
			results[i] = *j.early
		}
	}
	return results, nil
}

// runExecute runs the post-permission portion of a single tool_use:
// tool.Execute → PostToolUse hook → emit ToolResult event. It assumes
// PreToolUse + permission have already passed in executeBatch's
// pre-decision phase.
//
// Returns the matching tool_result block. All error paths produce a
// well-formed block so the LLM never sees a missing tool_result.
func (l *Loop) runExecute(ctx context.Context, t tools.Tool, blk llm.ContentBlock, out chan<- Event, tc HookContext) llm.ContentBlock {
	// Tag ctx with the parent's event out-channel so sub-tools (Agent)
	// can forward intermediate events for live UI updates.
	toolCtx := WithEventOut(ctx, out)
	// And the parent's conversation snapshot so the Fork tool can
	// spawn a child loop that inherits parent history+system. Pure
	// read pattern — Fork copies what it needs and never mutates back.
	l.mu.RLock()
	snap := ParentSnapshot{
		Messages: l.Messages,
		System:   l.System,
		Model:    l.Model,
	}
	l.mu.RUnlock()
	toolCtx = WithParentSnapshot(toolCtx, snap)

	// Honor InterruptBlock: tools that declare InterruptBlock want to
	// finish their current invocation even if the parent ctx gets
	// cancelled mid-call (Bash running `make install`, Edit mid-write,
	// SendMessage mid-flight). Detach the cancel signal — values from
	// ctx still flow through.
	//
	// Caveat: a malicious / runaway InterruptBlock tool could ignore
	// shutdown forever. Mitigation lives at the layer that issues the
	// cancel: the TUI's double-Ctrl+C path can hard-kill via the loop
	// owner. This detach only protects against the FIRST Ctrl+C.
	if tools.GetInterruptBehavior(t) == tools.InterruptBlock {
		toolCtx = context.WithoutCancel(toolCtx)
	}
	res, err := t.Execute(toolCtx, blk.ToolInput)
	if l.Detector != nil {
		l.Detector.Record(blk.ToolName, blk.ToolInput)
	}
	if l.Hooks != nil {
		var output string
		var isErr bool
		if err != nil {
			output, isErr = err.Error(), true
		} else if res != nil {
			output, isErr = res.Output, res.IsError
		}
		l.Hooks.EmitPostToolUse(ctx, tc, &PostToolUseHook{
			Context: tc, Tool: blk.ToolName, Input: blk.ToolInput,
			Output: output, IsError: isErr,
		})
		// Distinct PostToolUseFailure firing on tool errors so observers
		// can subscribe to "only failures" without filtering by IsError
		// inside every PostToolUse handler. Mirrors claude-code's split.
		if isErr {
			l.Hooks.EmitPostToolUseFailure(ctx, tc, &pubhook.PostToolUseFailure{
				Context: tc, Tool: blk.ToolName, Input: blk.ToolInput,
				Output: output, Error: output, Attempt: 1,
			})
		}
	}

	if err != nil {
		s := err.Error()
		emit(ctx, out, Event{
			Kind: EventToolResult, ToolUseID: blk.ToolUseID, ToolName: blk.ToolName,
			ToolResult: &ToolResult{Output: s, IsError: true},
		})
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: blk.ToolUseID,
			ToolResult: s, IsError: true,
		}
	}
	emit(ctx, out, Event{
		Kind: EventToolResult, ToolUseID: blk.ToolUseID, ToolName: blk.ToolName,
		ToolResult: &ToolResult{Output: res.Output, IsError: res.IsError},
	})
	return llm.ContentBlock{
		Type: "tool_result", ToolUseID: blk.ToolUseID,
		ToolResult: res.Output, IsError: res.IsError,
	}
}
