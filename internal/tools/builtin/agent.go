package builtin

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
type Agent struct {
	gate     *permission.Gate
	provider llm.Provider
	registry *tools.Registry
	model    string
	system   string
}

// NewAgent constructs the Agent tool. Caller wires it into the registry
// after builtin.Register so the runtime's provider/model are available.
func NewAgent(gate *permission.Gate, prov llm.Provider, reg *tools.Registry, model, system string) Agent {
	return Agent{gate: gate, provider: prov, registry: reg, model: model, system: system}
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

	maxIter := intArg(in, "max_iter", 10)
	sub := agent.NewLoop(a.provider, a.registry, a.gate, agent.NewHookRegistry(), a.system, maxIter)
	sub.Model = a.model
	sub.AppendUser(prompt)

	childCtx := context.WithValue(ctx, agentDepthKey{}, depth+1)
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
				return &tools.Result{Output: ev.Err.Error(), IsError: true}, nil
			}
		}
	}
	if err := <-done; err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	out := strings.TrimSpace(output.String())
	if out == "" {
		out = fmt.Sprintf("(sub-agent finished without text output; stop_reason=%s)", stopReason)
	}
	return &tools.Result{Output: out}, nil
}
