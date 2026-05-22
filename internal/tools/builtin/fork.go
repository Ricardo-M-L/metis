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
// `restored-src/src/tools/AgentTool/forkSubagent.ts`.
//
// Placeholder tool_results (2026-05-15 fix): we DO synthesize them
// before appending the directive. The earlier comment dismissed this
// as an Anthropic prompt-cache optimization that wouldn't apply to
// OpenAI-dialect providers — that was wrong. The OpenAI / DeepSeek
// schema strictly requires every assistant `tool_calls` block to be
// followed by tool messages matching every tool_call_id, otherwise
// the API rejects with `invalid_request_error: insufficient tool
// messages following tool_calls message`. Since the parent snapshot
// always ends with the assistant turn that called Fork (the Fork
// tool_use itself!), shoving a user TEXT message at the end leaves
// that tool_use unanswered and the child's first API call fails.
// The fix is to synthesize a placeholder tool_result for every
// outstanding tool_use in the snapshot's tail before appending the
// directive — keeps the conversation valid for ALL providers.
// Anthropic API was tolerant of the broken shape so the bug only
// manifested on OpenAI-dialect providers.

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
	tools.BaseTool
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

// ShortDescription — symmetric with Agent.ShortDescription so the
// short-form tool palette makes the cold/warm distinction visible
// without requiring the model to read the long Description.
func (Fork) ShortDescription() string {
	return "Spawn a WARM child (inherits parent's full history + system + model) for synthesis/drafting that depends on this conversation's context. Returns the child's final text. Cache-friendly (shared prefix). For cold lookups use Agent. Capped at 1 nested fork by default."
}
func (Fork) Description() string {
	return `Spawn a WARM child agent that inherits the parent's full conversation history, system prompt, and model. The child sees every turn of this conversation as its starting point, then runs the directive you give it. Returns the child's final assistant text. (For self-contained tasks that don't need this conversation's context, use Agent instead — that's the cold spawn.)

Use Fork for:
  - Synthesis/drafting that depends on context: "based on everything we've decided, draft the migration script", "given the bugs we just diagnosed, write the changelog entry", "produce the final ADR from the design discussion above".
  - Heavy generation that would balloon parent context: long file rewrites, structured reports, code generation off the conversation. The child returns just the final text — intermediate reasoning stays out of the parent.
  - Cache-efficient parallel exploration of one decision: fork twice with two phrasings of the same directive, compare the answers.

Do NOT use Fork for:
  - Cold lookups ("grep for X", "read /etc/hosts") — that's Agent territory. Fork carries the entire parent prefix forward, which is wasted bytes if the child doesn't need them.
  - Tasks where the parent's conversation is irrelevant or actively distracting (the child should focus on a fresh question without the noise of the prior turns).
  - Recursive nested forks — Fork is capped at 1 level deep by default (raise [agents].max_fork_depth to 2 in ~/.metis/config.toml if you really need fork-in-fork). Each nesting layer rewrites the prompt prefix and exponentially decays the cache benefit; after one level the savings are gone.

Trade-off vs Agent:
  - Agent (cold spawn): no parent context, smaller request, model has to be re-briefed. Best for "go look up X" sub-tasks.
  - Fork (warm spawn): full parent context, prompt-cache friendly (child shares the parent's byte-identical prefix), model already knows everything. Best for "given everything above, do X" sub-tasks.

Briefing the fork: keep the directive short and direct. The child already has the conversation — repeating it just dilutes focus. State the action and any output constraints ("under 200 words", "Markdown table only").`
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
				"description": "Tool-call budget for the forked child (default 100). Bumped from 20 → 100 on 2026-05-21 alongside Agent.max_iter for parity (fork inherits context but still needs the same headroom as a fresh Agent for non-trivial implementation tasks).",
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
//
// Default 1 (2026-05-15, lowered from 2):
// Fork's value proposition is prompt-cache reuse — the child shares
// byte-identical prefix with the parent. Each nested layer rewrites
// that prefix (level-1 fork adds its own bytes the level-2 fork must
// inherit), so cache hit rate decays exponentially with depth. By
// level 2 the cache savings have largely evaporated. Matches CC's
// stricter rule (CC rejects Fork-inside-Fork outright via
// FORK_BOILERPLATE_TAG detection). Users who genuinely need a fork
// tree can raise `[agents].max_fork_depth = 2` in
// ~/.metis/config.toml — they pay the cache miss explicitly.
type forkDepthKey struct{}

const defaultMaxForkDepth = 1

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
			Output: fmt.Sprintf(
				"fork nesting limit (%d) exceeded — Fork-in-fork rewrites the prompt prefix and exponentially decays the cache benefit. Alternatives: "+
					"(1) flatten the work into the current turn; "+
					"(2) call Agent({prompt: \"...\"}) for a cold sub-agent — loses parent history continuity but works at any depth; "+
					"(3) raise [agents].max_fork_depth in ~/.metis/config.toml if fork-in-fork is genuinely needed.",
				cap,
			),
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

	// Default bumped 20 → 100 on 2026-05-21 (parity with Agent.max_iter).
	// See agent.go for the full rationale.
	maxIter := intArg(in, "max_iter", 100)
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
	// 2026-05-15 fix — placeholder tool_results before the directive.
	// See package doc-comment for why this is needed (OpenAI strict
	// pairing of tool_calls↔tool_results). The parent snapshot ALWAYS
	// ends with the assistant turn that called Fork (or sometimes a
	// batch of parallel tool_calls including Fork). Append synthetic
	// tool_results so the child's first API call sees a well-formed
	// conversation; only then append the directive.
	if placeholders := buildPlaceholderToolResults(snap.Messages); len(placeholders) > 0 {
		sub.AppendUserBlocks(placeholders)
	}
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
		//
		// Fix 1 (2026-05-15) — TextDelta NOT forwarded; same
		// reasoning as Agent.forwardSubAgentEvent. The child's text
		// becomes Fork's tool_result body that the parent UI prints
		// when Fork returns; live deltas only corrupt the parent's
		// stream.
		if parentOut != nil {
			switch ev.Kind {
			case agent.EventToolStart, agent.EventToolResult:
				forwarded := ev
				forwarded.ToolName = "fork: " + ev.ToolName
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

// buildPlaceholderToolResults inspects the inherited snapshot and, if
// its tail is an assistant message containing tool_use blocks (i.e.
// the parent's last turn was the very tool call that invoked Fork),
// returns one synthetic `tool_result` content block per tool_use_id.
// The child needs these so its first API call presents a well-formed
// conversation to the provider — OpenAI / DeepSeek strictly reject
// `tool_calls` that aren't immediately followed by matching tool
// messages.
//
// Why "(forked)" stub text rather than the real result: the parent
// hasn't received its own tool_result yet (Fork.Execute IS that
// tool_result, still in flight), so we can't reflect anything truthful
// to the child. Saying "(forked)" tells the model the call was
// short-circuited into a fork — sufficient to keep it from re-issuing
// the same tool call. claude-code does the same thing.
func buildPlaceholderToolResults(messages []llm.Message) []llm.ContentBlock {
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role != llm.RoleAssistant {
		return nil
	}
	var out []llm.ContentBlock
	for _, b := range last.Content {
		if b.Type != "tool_use" {
			continue
		}
		out = append(out, llm.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  b.ToolUseID,
			ToolResult: "(forked — original call short-circuited into a child fork; result will be reported when the child returns to the parent)",
		})
	}
	return out
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
