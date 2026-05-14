package builtin

// fork.go — `Fork` tool. Spawn a child agent that **inherits the
// parent's full conversation history + system prompt + model**, then
// runs a focused directive. Returns the child's final assistant
// text to the parent.
//
// Companion to the existing `Agent` tool (agent.go), which spawns a
// COLD sub-agent (empty history). The two cover the two main
// "delegation" use cases:
//
//   - Agent (cold) — "research this URL, summarise it" — child has
//     no useful context from parent, sub-agent does the small task
//     in isolation, returns text. Token-efficient.
//   - Fork (warm) — "based on everything we've discussed, draft the
//     migration script" — child needs every nuance the parent has
//     accumulated. Cache-efficient (prompt prefix shared with parent).
//
// Mirrors claude-code-sourcemap's
// `restored-src/src/tools/AgentTool/forkSubagent.ts`. The Anthropic
// trick of placeholder tool_results for prompt-cache parity is
// out-of-scope here — metis is provider-neutral, and only Anthropic
// would benefit. We just append the directive as a fresh user
// message at the end of the inherited history.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Fork runs a child agent loop that starts from the parent's full
// conversation. Parent context (history, system prompt, model) is
// pulled from the dispatch-time ParentSnapshot — no other plumbing
// needed since dispatch.go attaches it for every tool call.
type Fork struct {
	gate     *permission.Gate
	provider llm.Provider
	registry *tools.Registry

	// MaxDepth overrides the default fork-nesting cap. 0 → use
	// defaultMaxForkDepth. Wired from config.Agents.MaxForkDepth at
	// runtime. Lower than Agent's depth because Fork carries the
	// parent's full conversation forward, doubling context per level.
	MaxDepth int
}

func (f Fork) effectiveMaxDepth() int {
	if f.MaxDepth > 0 {
		return f.MaxDepth
	}
	return defaultMaxForkDepth
}

// NewFork constructs the tool. provider+registry must be non-nil at
// runtime; nil values produce a clear error from Execute rather than
// a panic on the first invocation.
func NewFork(gate *permission.Gate, prov llm.Provider, reg *tools.Registry) Fork {
	return Fork{gate: gate, provider: prov, registry: reg}
}

func (Fork) Name() string { return "Fork" }
func (Fork) Description() string {
	return "Fork a child agent that inherits the parent's full conversation history, system prompt, and model. Best for sub-tasks that need every nuance of the current context (e.g. 'based on everything above, draft the migration script'). For isolated 'go research X' tasks use Agent instead — that one starts cold."
}
func (Fork) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"directive"},
		"properties": map[string]any{
			"directive": map[string]any{
				"type":        "string",
				"description": "The instruction the forked child should execute. Treated as a fresh user turn appended to the inherited history.",
			},
			"max_iter": map[string]any{
				"type":        "integer",
				"description": "Tool-call budget for the forked child (default 20).",
			},
		},
	}
}
func (Fork) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

func (f Fork) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := f.gate.Check(context.Background(), "Fork", strFromAny(in["directive"]))
	return mapDecision(d), src
}

// forkDepthKey caps recursion: a forked child can fork further, but
// only up to maxForkDepth nested forks. Without this cap, the LLM
// can ladder Fork→Fork→Fork until the budget cliff.
type forkDepthKey struct{}

const defaultMaxForkDepth = 2

func (f Fork) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	directive, _ := in["directive"].(string)
	if strings.TrimSpace(directive) == "" {
		return nil, errors.New("directive is required")
	}
	if f.provider == nil || f.registry == nil {
		return nil, errors.New("Fork tool not fully wired (missing provider/registry)")
	}

	depth, _ := ctx.Value(forkDepthKey{}).(int)
	if cap := f.effectiveMaxDepth(); depth >= cap {
		return &tools.Result{
			Output:  fmt.Sprintf("fork nesting limit (%d) exceeded — flatten the work into the current turn, or raise [agents].max_fork_depth in ~/.metis/config.toml", cap),
			IsError: true,
		}, nil
	}

	snap := agent.ParentSnapshotFromContext(ctx)
	if snap == nil {
		return &tools.Result{
			Output:  "Fork: no parent context available (called outside a live agent loop)",
			IsError: true,
		}, nil
	}
	if len(snap.Messages) == 0 {
		return &tools.Result{
			Output:  "Fork: parent context is empty — use Agent for cold-start sub-tasks",
			IsError: true,
		}, nil
	}

	maxIter := intArg(in, "max_iter", 20)
	// G.13 (2026-05-12) — Fork is the warm-context spawn path.
	// Prepend the fork preamble so the child knows it can see the
	// parent's full history above and shouldn't re-explore.
	subSystem := agent.BuildSubPrompt(agent.SubPromptInputs{
		Mode: agent.SubPromptFork,
		Base: snap.System,
	})
	sub := agent.NewLoop(f.provider, f.registry, f.gate, agent.NewHookRegistry(), subSystem, maxIter)
	if snap.Model != "" {
		sub.Model = snap.Model
	}
	// Forks also get short-form tool descriptions — they inherit the
	// parent's snap.System (already heavy with conversation context)
	// and don't need the parent's full tool prompts repeated. Phase
	// C.1 / 2026-05-14.
	sub.ShortToolDescriptions = true
	// Restore copies the slice defensively; any further mutation in
	// the parent doesn't affect the child's view.
	sub.Restore(snap.Messages)
	// Append the directive as a fresh user message — claude-code
	// wraps it in <fork-boilerplate> reminding the child it's a
	// worker. We ship a minimal one-liner equivalent so the model
	// doesn't drift back into "let me ask a clarifying question"
	// mid-fork.
	sub.AppendUser(buildForkDirective(directive))

	childCtx := context.WithValue(ctx, forkDepthKey{}, depth+1)
	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- sub.Run(childCtx, events)
		close(events)
	}()

	parentOut := agent.EventOutFromContext(ctx)
	var output strings.Builder
	stopReason := ""
	for ev := range events {
		// Forward selected sub-events upstream so the user sees
		// "Fork · Reading X" instead of a dead spinner.
		if parentOut != nil {
			switch ev.Kind {
			case agent.EventToolStart, agent.EventToolResult, agent.EventTextDelta:
				forwarded := ev
				if ev.Kind == agent.EventToolStart || ev.Kind == agent.EventToolResult {
					forwarded.ToolName = "fork: " + ev.ToolName
				}
				select {
				case parentOut <- forwarded:
				default:
				}
			}
		}
		switch ev.Kind {
		case agent.EventTextDelta:
			output.WriteString(ev.TextDelta)
		case agent.EventPermissionRequest:
			// Same policy as Agent: deny interactive prompts,
			// child gets the denial as a tool_result and decides
			// how to recover.
			ev.PermissionReply <- agent.PermissionDecisionDeny
		case agent.EventLoopDone:
			stopReason = ev.StopReason
		case agent.EventError:
			if ev.Err != nil {
				return &tools.Result{Output: ev.Err.Error(), IsError: true}, nil
			}
		}
	}
	if err := <-done; err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	out := strings.TrimSpace(output.String())
	if out == "" {
		out = fmt.Sprintf("(forked child finished without text output; stop_reason=%s)", stopReason)
	}
	return &tools.Result{Output: out}, nil
}

// buildForkDirective wraps the user's directive with a short
// boilerplate reminding the child it's a forked worker — analogous
// to claude-code's <fork-boilerplate> block but stripped down. The
// goal: keep the child focused on the directive and resist its
// system prompt's "default to clarifying questions" behaviour.
func buildForkDirective(directive string) string {
	return `[fork] You are a forked worker continuing the conversation above. ` +
		`Execute the directive directly with the tools you have. Do not ask ` +
		`clarifying questions. Do not propose alternatives. Report concisely ` +
		`once the work is done.

Directive: ` + directive
}
