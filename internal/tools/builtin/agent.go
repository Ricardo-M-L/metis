package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// anonAgentID returns the AgentID string we stamp on each Roster
// entry. Format `agt-<8hex>` keeps it short for /agents list output.
// Used in place of session-local IDs until G.4 (sub-agent resume)
// lands persistent identity.
func anonAgentID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "agt-" + hex.EncodeToString(b[:])
}

// agentDepthKey carries the nested-agent count through context so a
// sub-agent that itself spawns sub-agents can be capped before runaway
// recursion runs the user's bill into the floor.
type agentDepthKey struct{}

const maxAgentDepth = 3

// Agent is a tool that runs a focused subtask in a fresh agent loop.
// Inspired by Claude Code's LocalAgentTask: spawn an isolated message
// history, execute it to completion, and return the assistant's final text
// to the parent agent as a single tool_result.
//
// The sub-agent shares the parent's provider, tool registry, and permission
// gate. It cannot satisfy interactive permission prompts on its own, so when
// the gate returns Ask the call is denied.
//
// Roster + defaultTimeout were added 2026-05-12 (Phase G.0): the
// Roster caps concurrent sub-agents so a model can't fork-bomb the
// API, and defaultTimeout bounds wall-clock duration when the
// `timeout_seconds` input arg is missing.
type Agent struct {
	gate           *permission.Gate
	provider       llm.Provider
	registry       *tools.Registry
	roster         *agent.Roster
	model          string
	system         string
	minimalSystem  string // optional; preferred for sub-agent loops to save tokens
	defaultTimeout time.Duration
}

// NewAgent constructs the Agent tool. Caller wires it into the registry
// after builtin.Register so the runtime's provider/model are available.
//
// `system` is the full assembled prompt the parent loop runs with;
// kept as a fallback for callers that don't compute a minimal variant.
// New code should use NewAgentWithMinimal so sub-agents skip the
// parent's <project_context> + ~/.metis/system.md addendum and save
// the per-sub-agent tokens those sections would have eaten.
//
// Roster + defaultTimeout get their zero values; production callers
// should use NewAgentWithMinimal or set them via the package-level
// AttachRoster helper after construction.
func NewAgent(gate *permission.Gate, prov llm.Provider, reg *tools.Registry, model, system string) Agent {
	return Agent{gate: gate, provider: prov, registry: reg, model: model, system: system}
}

// NewAgentWithMinimal is the option-bearing variant. minimalSystem is
// what sub-agents see (mirrors openclaw's "minimal mode" sub-agent
// prompt). When empty, sub-agents fall back to `system`.
func NewAgentWithMinimal(gate *permission.Gate, prov llm.Provider, reg *tools.Registry, model, system, minimalSystem string) Agent {
	return Agent{gate: gate, provider: prov, registry: reg, model: model, system: system, minimalSystem: minimalSystem}
}

// WithRoster wires a Roster into the Agent tool and returns a new
// value. Builder-style so existing callers that ignore the cap don't
// need their signatures changed; runtime/tools.go uses it after
// constructing the shared Roster singleton.
func (a Agent) WithRoster(r *agent.Roster) Agent {
	a.roster = r
	return a
}

// WithDefaultTimeout sets the wall-clock fallback when the
// `timeout_seconds` schema field is not provided. Zero leaves the
// timeout disabled (only the agent loop's max_iter caps execution).
func (a Agent) WithDefaultTimeout(d time.Duration) Agent {
	a.defaultTimeout = d
	return a
}

func (Agent) Name() string { return "Agent" }
func (Agent) Description() string {
	return "Run a sub-agent on a focused task. Returns the sub-agent's final text. Sub-agent shares the same tools and permissions but runs in an isolated message history."
}
func (Agent) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"prompt"},
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "the focused task for the sub-agent",
			},
			"max_iter": map[string]any{
				"type":        "integer",
				"description": "tool-call budget for the sub-agent (default 10)",
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"description": "wall-clock budget for the sub-agent. Defaults to config.Agents.DefaultTimeoutSeconds (10 minutes). 0 disables timeout.",
			},
		},
	}
}
func (Agent) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

func (a Agent) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := a.gate.Check(context.Background(), "Agent", strFromAny(in["prompt"]))
	return mapDecision(d), src
}

func (a Agent) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	prompt, _ := in["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if a.provider == nil || a.registry == nil {
		return nil, errors.New("Agent tool not fully wired (missing provider/registry)")
	}
	depth, _ := ctx.Value(agentDepthKey{}).(int)
	if depth >= maxAgentDepth {
		return &tools.Result{
			Output:  fmt.Sprintf("agent nesting limit (%d) exceeded", maxAgentDepth),
			IsError: true,
		}, nil
	}

	// G.0 cap — refuse before constructing the sub-loop so the API
	// burn stays bounded. The cap reads from the Roster's Capacity
	// (set at runtime construction from config.Agents.MaxConcurrentSubAgents).
	// When no Roster is attached we skip the check, matching the
	// pre-G.0 behavior used by unit tests that construct Agent directly.
	var teammate *agent.Teammate
	if a.roster != nil {
		teammate = &agent.Teammate{
			AgentID: anonAgentID(),
		}
		if err := a.roster.Register(teammate); err != nil {
			if errors.Is(err, agent.ErrCapacityExceeded) {
				cap := a.roster.Capacity()
				return &tools.Result{
					Output: fmt.Sprintf(
						"sub-agent capacity exceeded (%d/%d in flight). Wait for a teammate to finish or raise config.Agents.MaxConcurrentSubAgents.",
						a.roster.Count(), cap,
					),
					IsError: true,
				}, nil
			}
			return &tools.Result{Output: err.Error(), IsError: true}, nil
		}
		defer a.roster.Unregister(teammate.Name)
	}

	maxIter := intArg(in, "max_iter", 10)
	subSystem := a.system
	if a.minimalSystem != "" {
		subSystem = a.minimalSystem
	}
	sub := agent.NewLoop(a.provider, a.registry, a.gate, agent.NewHookRegistry(), subSystem, maxIter)
	sub.Model = a.model
	sub.AppendUser(prompt)

	// G.0 timeout — wall-clock cap that bounds runaway sub-agents
	// when the agent-loop's max_iter alone isn't enough (e.g., a
	// single Bash tool call hanging on a network read). Caller-
	// provided `timeout_seconds` wins; otherwise the Agent tool's
	// defaultTimeout (from config); 0 disables.
	timeoutSec := intArg(in, "timeout_seconds", 0)
	timeout := time.Duration(timeoutSec) * time.Second
	if timeoutSec == 0 && a.defaultTimeout > 0 {
		timeout = a.defaultTimeout
	}
	childCtx, cancel := context.WithCancel(context.WithValue(ctx, agentDepthKey{}, depth+1))
	defer cancel()
	if timeout > 0 {
		childCtx, cancel = context.WithTimeout(childCtx, timeout)
		defer cancel()
	}
	if teammate != nil {
		teammate.Cancel = cancel
	}

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- sub.Run(childCtx, events)
		close(events)
	}()

	// parentOut is the chat surface's event channel, attached via
	// context. We forward sub-tool start/end events so the user
	// sees real-time progress ("Agent · Reading foo.go", "Agent ·
	// Bash...") instead of a silent spinner.
	parentOut := agent.EventOutFromContext(ctx)
	var output strings.Builder
	stopReason := ""
	for ev := range events {
		// Forward selected events upstream so the parent UI can
		// render live progress. We tag the ToolName with "sub: "
		// to distinguish from parent's own tool calls.
		if parentOut != nil {
			switch ev.Kind {
			case agent.EventToolStart, agent.EventToolResult, agent.EventTextDelta:
				forwarded := ev
				if ev.Kind == agent.EventToolStart || ev.Kind == agent.EventToolResult {
					forwarded.ToolName = "sub: " + ev.ToolName
				}
				select {
				case parentOut <- forwarded:
				default:
					// parent buffer full — drop silently rather than block
				}
			}
		}
		switch ev.Kind {
		case agent.EventTextDelta:
			output.WriteString(ev.TextDelta)
		case agent.EventPermissionRequest:
			// No UI to satisfy interactive prompts — deny and let the sub-agent
			// decide whether to recover. The denial reaches it as a tool_result.
			ev.PermissionReply <- agent.PermissionDecisionDeny
		case agent.EventLoopDone:
			stopReason = ev.StopReason
		case agent.EventError:
			if ev.Err != nil {
				// G.0 — timeout path also reaches here via Loop.Run
				// emitting EventError(ctx.Err()) when its ctx tripped.
				// Mirror the `<-done` arm's user-facing message so the
				// model gets the same "retry with larger budget" hint
				// regardless of which observation point fired first.
				if errors.Is(ev.Err, context.DeadlineExceeded) && timeout > 0 {
					return &tools.Result{
						Output: fmt.Sprintf(
							"sub-agent timed out after %s. Re-spawn with `timeout_seconds: <larger>` if the task legitimately needs more wall-clock budget; otherwise scope down the prompt.",
							timeout,
						),
						IsError: true,
					}, nil
				}
				return &tools.Result{Output: ev.Err.Error(), IsError: true}, nil
			}
		}
	}
	if err := <-done; err != nil {
		// Surface the wall-clock timeout (G.0) distinctly so the
		// parent model can decide whether to retry with a larger
		// budget vs. scope the request down.
		if errors.Is(err, context.DeadlineExceeded) && timeout > 0 {
			return &tools.Result{
				Output: fmt.Sprintf(
					"sub-agent timed out after %s. Re-spawn with `timeout_seconds: <larger>` if the task legitimately needs more wall-clock budget; otherwise scope down the prompt.",
					timeout,
				),
				IsError: true,
			}, nil
		}
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	out := strings.TrimSpace(output.String())
	if out == "" {
		out = fmt.Sprintf("(sub-agent finished without text output; stop_reason=%s)", stopReason)
	}
	return &tools.Result{Output: out}, nil
}
