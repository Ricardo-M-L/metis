package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// toolSpecs builds the per-request `tools[]` array given to the LLM, by
// asking each registered tool for its name + description + input schema.
func (l *Loop) toolSpecs() []llm.ToolSpec {
	all := l.Registry.All()
	out := make([]llm.ToolSpec, 0, len(all))
	for _, t := range all {
		out = append(out, llm.ToolSpec{
			Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema(),
		})
	}
	return out
}

// executeBatch runs every tool_use in toolUses, returning the matching
// tool_result blocks. Concurrency tiers:
//
//	Safe       — fan out in parallel, no constraints
//	Queue      — FIFO among themselves, runs *concurrently with* the
//	             safe parallel fanout (a single-slot worker pool)
//	Exclusive  — serialize AFTER the safe + queue work completes
//
// The phased pattern preserves the "writes happen after reads in the
// same turn" invariant without per-tool dependency analysis. Queue is
// the new tier — useful for rate-limited APIs (WebFetch, an MCP server
// pinned to one connection) that don't need full exclusivity but
// shouldn't run in parallel with each other.
func (l *Loop) executeBatch(ctx context.Context, toolUses []llm.ContentBlock, out chan<- Event, tc HookContext) ([]llm.ContentBlock, error) {
	results := make([]llm.ContentBlock, len(toolUses))
	type job struct {
		idx int
		blk llm.ContentBlock
		t   tools.Tool
	}
	var safeJobs, queueJobs, exclJobs []job
	for i, b := range toolUses {
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
		j := job{idx: i, blk: b, t: t}
		// Input-dependent concurrency: Bash (and similar) decides
		// based on its argv whether the call is read-only. Tools that
		// don't care just ignore the argument.
		switch t.Concurrency(b.ToolInput) {
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
		go func(j job) {
			defer wg.Done()
			blk := l.runOne(ctx, j.t, j.blk, out, tc)
			mu.Lock()
			results[j.idx] = blk
			mu.Unlock()
		}(j)
	}

	// Phase 1b (concurrent with 1a): drain the queue jobs FIFO from a
	// single goroutine. This means queue tools run alongside the safe
	// fanout but never concurrently with each other — the right shape
	// for rate-limited remote APIs.
	if len(queueJobs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, j := range queueJobs {
				blk := l.runOne(ctx, j.t, j.blk, out, tc)
				mu.Lock()
				results[j.idx] = blk
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Phase 2: serialize exclusive tools. Order preserved by insertion.
	for _, j := range exclJobs {
		results[j.idx] = l.runOne(ctx, j.t, j.blk, out, tc)
	}
	return results, nil
}

// runOne executes a single tool_use block end-to-end: PreToolUse hook →
// permission check (with optional ask flow) → tool.Execute → PostToolUse
// hook → emit result event. Returns the matching tool_result block.
//
// All early-return paths (deny / ask-deny / hook-intercept) produce a
// well-formed tool_result block so the LLM always sees a consistent
// response shape per tool_use it emitted.
func (l *Loop) runOne(ctx context.Context, t tools.Tool, blk llm.ContentBlock, out chan<- Event, tc HookContext) llm.ContentBlock {
	emit(ctx, out, Event{
		Kind: EventToolStart, ToolUseID: blk.ToolUseID,
		ToolName: blk.ToolName, ToolInput: blk.ToolInput,
	})

	// PreToolUse hook — can short-circuit (Output) or rewrite input.
	if l.Hooks != nil {
		mod := l.Hooks.EmitPreToolUse(ctx, tc, &PreToolUseHook{
			Context: tc, Tool: blk.ToolName, Input: blk.ToolInput,
		})
		if mod != nil {
			if mod.Output != nil {
				b := llm.ContentBlock{
					Type: "tool_result", ToolUseID: blk.ToolUseID,
					ToolResult: mod.Output.Content, IsError: mod.Output.IsError,
				}
				emit(ctx, out, Event{
					Kind: EventToolResult, ToolUseID: blk.ToolUseID, ToolName: blk.ToolName,
					ToolResult: &ToolResult{Output: mod.Output.Content, IsError: mod.Output.IsError},
				})
				return b
			}
			if mod.ModifiedInput != nil {
				blk.ToolInput = mod.ModifiedInput
			}
		}
	}

	// Permission check.
	perm, _ := t.CanUse(ctx, blk.ToolInput)
	if perm == tools.PermissionDeny {
		b := llm.ContentBlock{
			Type: "tool_result", ToolUseID: blk.ToolUseID,
			ToolResult: "denied by permission policy", IsError: true,
		}
		emit(ctx, out, Event{
			Kind: EventToolResult, ToolUseID: blk.ToolUseID, ToolName: blk.ToolName,
			ToolResult: &ToolResult{Output: "denied", IsError: true},
		})
		return b
	}
	if perm == tools.PermissionAsk {
		ar := l.askPermission(ctx, blk, out)
		if !ar.proceed {
			return *ar.earlyReturn
		}
	}

	// Tag ctx with the parent's event out-channel so sub-tools (Agent)
	// can forward intermediate events for live UI updates. Without
	// this, the user sees the Agent pill spin for minutes with no
	// progress until the sub-loop returns final text.
	toolCtx := WithEventOut(ctx, out)
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
