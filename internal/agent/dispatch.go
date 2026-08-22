package agent

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/spill"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
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

// ToolSpecsSnapshot returns the exact []ToolSpec that would be sent on
// the next turn, given current LazyMode + discovered-tools state.
// Exposed for the TUI's /context surface (render_info.go::renderContext)
// so the "Loaded / Available" MCP split reflects the actual prompt
// rather than re-implementing the strip logic. Pure read — no state
// mutation; safe to call from any goroutine.
func (l *Loop) ToolSpecsSnapshot() []llm.ToolSpec {
	return l.toolSpecs()
}

func (l *Loop) toolSpecs() []llm.ToolSpec {
	all := l.Registry.SortedForCache()
	out := make([]llm.ToolSpec, 0, len(all))
	for _, t := range all {
		desc := descriptionForTool(t, l.ShortToolDescriptions)
		out = append(out, llm.ToolSpec{
			Name: t.Name(), Description: desc, InputSchema: t.InputSchema(),
		})
	}
	mode, pct := parseEnableToolSearch(os.Getenv("ENABLE_TOOL_SEARCH"))
	// preserve = MCP tools whose schemas the model has already fetched
	// in this session (P6, cross-compaction stability). Those keep
	// their schemas even when we're stripping the rest, so a post-
	// compaction turn doesn't force a redundant ToolSearch call for a
	// tool the model already used.
	preserve := l.snapshotDiscoveredMCP()
	switch mode {
	case LazyModeStandard:
		return out
	case LazyModeAlways:
		return stripAndAppendToolSearchWithPreserve(out, preserve)
	case LazyModeAuto:
		if l.ContextWindow <= 0 {
			return out
		}
		return applyLazySchemaByTokensWithPreserve(out, l.ContextWindow, pct, preserve)
	}
	return out
}

// descriptionForTool picks the right description for a tool given the
// current loop's "short tool descriptions" hint:
//
//   - short && tool implements tools.ShortDescriptor → use that
//     verbatim. Tools opt in by defining ShortDescription() on their
//     receiver; metis's core 5 (Bash/Edit/Write/Read/Agent) do.
//   - short && tool does NOT implement it → fall back to first-
//     paragraph truncation of Description() (same as legacy behavior).
//   - !short → first-paragraph truncation of Description(), preserving
//     the pre-Phase-C behavior for main-loop boots.
//
// The hand-curated ShortDescription is preferred over arbitrary
// first-paragraph truncation because (a) it can omit the "context-
// pulling" preamble paragraphs that exist for human readers but
// confuse the LLM, and (b) it can squeeze a "what / when / NOT" hint
// trio into the 200-char budget that automatic truncation would only
// catch the "what" of.
func descriptionForTool(t tools.Tool, short bool) string {
	if short {
		if sd, ok := t.(tools.ShortDescriptor); ok {
			return sd.ShortDescription()
		}
	}
	return shortToolDesc(t.Description())
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
		idx       int
		blk       llm.ContentBlock
		t         tools.Tool
		early     *llm.ContentBlock // set when pre-check decided result
		ready     bool              // true once pre-checks all pass
		startedAt time.Time         // set when EventToolStart is emitted; used to populate Event.Elapsed on result
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
			tsStart := time.Now()
			results[i] = handleToolSearch(l, b)
			emit(ctx, out, Event{
				Kind: EventToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName,
				ToolResult: &ToolResult{Output: results[i].ToolResult, IsError: results[i].IsError},
				Elapsed:    time.Since(tsStart),
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
			hookStart := time.Now()
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
						Elapsed:    time.Since(hookStart),
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
		//
		// The reason string from CanUse used to be discarded with `_`,
		// which meant a deny showed up as opaque "denied" everywhere:
		// the model saw "denied by permission policy" (which doesn't
		// say WHY) and the TUI / non-interactive log saw bare "denied"
		// (which doesn't even hint where to look). Caught during the
		// 2026-05-18 bench6 review — 1–3 mystery denies per run that
		// no one could debug without source-diving. Surface the reason
		// in BOTH outbound channels so future denies are debuggable.
		perm, reason := t.CanUse(ctx, j.blk.ToolInput)
		if perm == tools.PermissionDeny {
			modelMsg := "denied by permission policy"
			tuiMsg := "denied"
			if reason != "" {
				modelMsg = "denied by permission policy: " + reason
				tuiMsg = "denied: " + reason
			}
			blkOut := llm.ContentBlock{
				Type: "tool_result", ToolUseID: b.ToolUseID,
				ToolResult: modelMsg, IsError: true,
			}
			emit(ctx, out, Event{
				Kind: EventToolResult, ToolUseID: b.ToolUseID, ToolName: b.ToolName,
				ToolResult: &ToolResult{Output: tuiMsg, IsError: true},
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
	var safeJobs, queueJobs, exclJobs, bgJobs []*job
	for _, j := range jobs {
		if j == nil || j.early != nil || !j.ready {
			continue
		}
		switch j.t.Concurrency(j.blk.ToolInput) {
		case tools.ConcurrencySafe:
			safeJobs = append(safeJobs, j)
		case tools.ConcurrencyQueue:
			queueJobs = append(queueJobs, j)
		case tools.ConcurrencyBackground:
			bgJobs = append(bgJobs, j)
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

	// Phase 1c (G.1, 2026-05-12): fire-and-forget background tools.
	// Tools that declare ConcurrencyBackground (e.g. `Agent` when
	// `run_in_background:true`) get their Execute() invoked in a
	// detached goroutine; the dispatcher synthesizes a placeholder
	// tool_result immediately so the model can keep streaming without
	// waiting for completion.
	//
	// The Tool itself is responsible for returning a fast handshake
	// result (job_id) inside its goroutine and posting the final
	// outcome through the jobs.Registry notification channel — same
	// pattern Bash uses for run_in_background commands. We don't
	// touch results[] here because the Tool already wrote a synchronous
	// handshake result; see internal/tools/builtin/agent.go::Execute
	// for the background branch.
	//
	// Why not just include in safeJobs: a background tool's Execute is
	// "instant" (it spawns + returns), but its concurrency semantics
	// are distinct from safe — running 5 background sub-agents IS a
	// legitimate parallel side effect that the model is explicitly
	// asking for. Keeping them in their own tier makes the dispatch
	// graph readable and lets future scheduling policy hook in here
	// (e.g., rate-limiting background spawns).
	for _, j := range bgJobs {
		results[j.idx] = l.runExecute(ctx, j.t, j.blk, out, tc)
	}

	wg.Wait()

	// Phase 2: serialize exclusive tools. Order preserved by insertion.
	//
	// Ctx-cancel short-circuit (2026-05-18): if the parent ctx already
	// died (user Ctrl+C between safe-fanout and exclusives, or a
	// background spawn raced the cancel), skip the remaining
	// exclusives and synthesize a cancellation tool_result so the
	// batch stays API-valid. Without this, each exclusive's Execute
	// runs and returns its own ctx.Canceled — wasted work, plus the
	// model sees N identical generic-cancel errors instead of one
	// clear "batch interrupted" signal.
	if err := ctx.Err(); err != nil {
		for _, j := range exclJobs {
			emit(ctx, out, Event{
				Kind: EventToolResult, ToolUseID: j.blk.ToolUseID, ToolName: j.blk.ToolName,
				ToolResult: &ToolResult{Output: "skipped: turn interrupted before this tool ran", IsError: true},
			})
			results[j.idx] = llm.ContentBlock{
				Type: "tool_result", ToolUseID: j.blk.ToolUseID,
				ToolResult: "skipped: turn interrupted before this tool ran (" + err.Error() + ")", IsError: true,
			}
		}
	} else {
		for _, j := range exclJobs {
			results[j.idx] = l.runExecute(ctx, j.t, j.blk, out, tc)
		}
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

// safeToolExecute runs a tool's Execute with panic recovery. Tools run
// inside the dispatcher's safe-fanout goroutine (executeBatch Phase 1a),
// where an UNrecovered panic crashes the entire metis process — taking
// every other in-flight sub-agent and the user's whole session down with
// it. Recovering here turns ANY tool fault (a buggy builtin, an MCP server
// wrapper, a sub-agent loop) into an ordinary error result the model can
// see and work around. This is the process-wide crash-isolation backstop;
// it's why metis doesn't need to spawn each sub-agent as its own OS
// process to get fault containment. The Agent/Fork tools also recover
// internally, but this guard covers the long tail of every other tool.
// spillDir returns the directory for ingestion-time result spilling.
// SpillDir is wired independently from the Compactor's microcompact
// cache (runtime/agent_loop.go): the two share the Read-recovery
// idiom but NOT a kill switch — METIS_MICROCOMPACT=0 must not
// silently disable spill too (2026-06-11 review finding). Empty =
// spilling disabled.
func (l *Loop) spillDir() string {
	return l.SpillDir
}

func safeToolExecute(ctx context.Context, t tools.Tool, in map[string]any) (res *tools.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = nil
			err = fmt.Errorf("tool %q crashed (recovered panic): %v", t.Name(), r)
			// Full stack only under METIS_DEBUG so the TUI alt-screen
			// stays clean in normal runs (stderr leaks corrupt it).
			if os.Getenv("METIS_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "metis: recovered tool panic in %s: %v\n%s\n", t.Name(), r, debug.Stack())
			}
		}
	}()
	return t.Execute(ctx, in)
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
	// Shared USD budget: sub-agent builders (Agent / Fork) pull this so
	// child spend draws down the parent's pool. nil when no cap is set.
	toolCtx = WithBudget(toolCtx, l.Budget)
	// Shared spill dir: sub-agents offload their own oversized tool
	// results to the same session store instead of flooding the child
	// context. Empty when spilling is disabled.
	toolCtx = WithSpillDir(toolCtx, l.SpillDir)
	// Plan-mode controller: lets EnterPlanMode / ExitPlanMode flip
	// loop.PlanMode mid-turn. Empty interface check is unnecessary —
	// *Loop always satisfies PlanController. Tools that don't care
	// (most of them) simply never pull this key from context.
	toolCtx = WithPlanController(toolCtx, l)
	// Parent tool_use_id: only consumed by the Agent tool's sub-event
	// forwarder, which stamps it on each forwarded child event so the
	// TUI can attribute progress to the right SubAgentInfo pill when
	// multiple sub-agents run in parallel. Other tools ignore it.
	toolCtx = WithParentToolUseID(toolCtx, blk.ToolUseID)

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
	// 2026-05-23: measure wall-clock around Execute so EventToolResult
	// carries the authoritative duration. forwardSubAgentEvent
	// preserves Event.Elapsed via shallow copy, so the parent TUI
	// reads the child loop's measurement instead of computing an
	// inter-event delta that hits 0ms on fast Reads (image #54).
	// Unified-rewind hook: snapshot the working tree before the first
	// file-mutating tool of this turn, so /rewind can restore files +
	// conversation together. Best-effort, once per turn, no-op when
	// checkpointing is off. See checkpoint_hook.go.
	l.snapPreEdit(blk.ToolName, blk.ToolInput)

	execStart := time.Now()
	// Cooperative per-call timeout (DSH tool-call-timeout-policy parity):
	// a tool that declares TimeoutMs() arms a deadline; if that deadline
	// wins, the result becomes a structured TOOL_TIMEOUT error instead of
	// whatever the tool returned. Arm AFTER the InterruptBlock detach so
	// the budget survives context.WithoutCancel (that strips the parent's
	// cancel, not the tool's own deadline).
	var res *tools.Result
	var err error
	if timeoutMs := tools.TimeoutMs(t); timeoutMs > 0 {
		var cancel context.CancelFunc
		toolCtx, cancel = context.WithTimeout(toolCtx, time.Duration(timeoutMs)*time.Millisecond)
		res, err = safeToolExecute(toolCtx, t, blk.ToolInput)
		cancel()
		if err == nil && toolCtx.Err() == context.DeadlineExceeded {
			res = &tools.Result{Output: fmt.Sprintf("Error: tool call timed out after %dms", timeoutMs), IsError: true}
		}
	} else {
		res, err = safeToolExecute(toolCtx, t, blk.ToolInput)
	}
	l.recordCheckpointMutation(blk.ToolName, blk.ToolInput, res, err)
	execElapsed := time.Since(execStart)
	// Persist per-step timing to the session sidecar (best-effort). This is
	// the single chokepoint every path's tool calls pass through, so one
	// hook covers TUI / headless / cron / sub-agents.
	if l.TimingSink != nil {
		l.TimingSink(blk.ToolName, execElapsed, err != nil || (res != nil && res.IsError))
	}
	if l.Detector != nil {
		l.Detector.Record(blk.ToolName, blk.ToolInput)
	}
	var hookContext string // PostToolUseContext additionalContext, appended below
	if l.Hooks != nil {
		var output string
		var isErr bool
		if err != nil {
			output, isErr = err.Error(), true
		} else if res != nil {
			output, isErr = res.Output, res.IsError
		}
		post := &PostToolUseHook{
			Context: tc, Tool: blk.ToolName, Input: blk.ToolInput,
			Output: output, IsError: isErr,
		}
		l.Hooks.EmitPostToolUse(ctx, tc, post)
		// Feedback-capable variant: handlers can return
		// AdditionalContext (lint diagnostics, format fixes) that gets
		// appended to the tool_result as a <system-reminder> so the
		// MODEL sees it next turn. Mirrors claude-code's PostToolUse
		// additionalContext response field.
		hookContext = l.Hooks.EmitPostToolUseContext(ctx, tc, post)
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
			Elapsed:    execElapsed,
		})
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: blk.ToolUseID,
			ToolResult: s, IsError: true,
		}
	}
	if res == nil {
		// A tool returned (nil, nil) — valid by the (*tools.Result, error)
		// signature but a bug (mock tools / a misbehaving MCP wrapper do it).
		// Without this guard the res.Output deref below panics inside the
		// fan-out goroutine, which has NO recover (safeToolExecute's recover
		// only covers Execute), crashing the whole process. fork.go guards
		// the same case; do it here too.
		res = &tools.Result{Output: "tool returned no result", IsError: true}
	}
	emit(ctx, out, Event{
		Kind: EventToolResult, ToolUseID: blk.ToolUseID, ToolName: blk.ToolName,
		ToolResult: &ToolResult{Output: res.Output, IsError: res.IsError},
		Elapsed:    execElapsed,
	})

	// Ingestion-time spill (claude-code's maxResultSizeChars, Tool.ts:456):
	// an oversized tool_result never enters the context wholesale — the
	// full content is persisted under the microcompact cache dir and the
	// model gets a preview + path it can Read back. Complements
	// Microcompact, which only offloads retroactively once the context
	// passes the snip threshold; by then a 500 KB dump has already
	// burned a turn of window. The TUI event above already carried the
	// full output, so on-screen display is unaffected. METIS_SPILL=0
	// disables (same spirit as the METIS_MICROCOMPACT kill switch).
	resultBody := res.Output
	if limit := tools.MaxResultSizeChars(t); limit > 0 && len(resultBody) > limit {
		if dir := l.spillDir(); dir != "" {
			if sr, serr := spill.Store(dir, blk.ToolUseID, resultBody); serr == nil {
				resultBody = sr.Stub()
			}
		}
	}
	// PostToolUseContext hook feedback — appended AFTER the spill check
	// so fresh diagnostics never get offloaded with the bulk output.
	if hookContext != "" {
		resultBody = resultBody + wrapAsSystemReminder(hookContext)
	}
	// Anti-pattern nudge: when the model uses Bash for something a
	// dedicated tool would do better (cat → Read, find → Glob, etc.),
	// append a one-line <system-reminder> to the tool_result. The
	// command still ran (no refusal); the nudge teaches the model to
	// pick the right tool next turn. See bash_redirects.go for the
	// detector logic.
	if blk.ToolName == "Bash" && !res.IsError {
		if cmd, _ := blk.ToolInput["command"].(string); cmd != "" {
			if hint := bashRedirect(cmd); hint != "" {
				resultBody = resultBody + wrapAsSystemReminder(hint)
			}
		}
	}
	// Skill-manifest nudge: when Read returns a SKILL.md, append a
	// reminder that the body is a procedure to follow (not a doc to
	// summarise). See skill_redirects.go for the detector.
	if blk.ToolName == "Read" && !res.IsError {
		if path, _ := blk.ToolInput["path"].(string); path != "" {
			if hint := skillReadHint(path, res.Output); hint != "" {
				resultBody = resultBody + wrapAsSystemReminder(hint)
			}
		}
	}
	// Vision-aware tools (ViewImage today, future PDF page rasterisers)
	// return base64-encoded images in res.Images. Promote them to a
	// multi-part tool_result body so vision-capable providers can
	// natively show the bytes as an image content block. The text
	// `resultBody` becomes the first text part (one-line summary like
	// "ViewImage: /abs/path.png (123 KiB, image/png)"); images follow.
	//
	// Vision gate: skip the fan-out entirely when the configured
	// provider isn't vision-capable (e.g. deepseek-v4-pro on
	// /chat/completions). The previous behaviour pushed image blocks
	// to the adapter regardless; non-vision OpenAI-compat providers
	// then returned 400 "unknown variant image_url" and burned the
	// whole turn on a cryptic error. With the gate, the model
	// receives the textual summary alone plus a one-line
	// system-reminder explaining the image was dropped, so it can
	// still describe e.g. file size / MIME and either retry on a
	// vision provider or fall back to other tools. The TUI-paste
	// path already has the equivalent strip in
	// internal/tui/keybind_submit.go::splitOffImageBlocks; this is
	// the tool-result twin.
	if len(res.Images) > 0 {
		visionCapability := pubprovider.ProviderVisionCapability(l.Provider)
		if visionCapability == pubprovider.VisionUnsupported {
			resultBody = resultBody + wrapAsSystemReminder(
				fmt.Sprintf("%d image attachment(s) dropped — current provider doesn't support vision input. To actually see the image, switch to a vision-capable model (e.g. claude-*, gpt-4o, minimax-m*, glm-5*, kimi-k2*, deepseek-vl*).", len(res.Images)),
			)
		} else {
			blocks := make([]llm.ContentBlock, 0, 1+len(res.Images))
			if resultBody != "" {
				blocks = append(blocks, llm.ContentBlock{Type: "text", Text: resultBody})
			}
			for _, img := range res.Images {
				blocks = append(blocks, llm.ContentBlock{
					Type: "image", MediaType: img.MediaType, Data: img.Data,
				})
			}
			return llm.ContentBlock{
				Type: "tool_result", ToolUseID: blk.ToolUseID,
				ToolResult: resultBody, IsError: res.IsError,
				ToolResultBlocks: blocks,
			}
		}
	}
	return llm.ContentBlock{
		Type: "tool_result", ToolUseID: blk.ToolUseID,
		ToolResult: resultBody, IsError: res.IsError,
	}
}
