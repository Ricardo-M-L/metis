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
	"github.com/Ricardo-M-L/metis/internal/jobs"
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
//
// jobsPool (added 2026-05-12, Phase G.1) hosts background sub-agents
// when `run_in_background:true`. The Tool spawns the sub-loop in a
// goroutine, registers a Job with the pool, and returns a fast
// handshake `job_id=...` tool_result; the model polls via the
// SubAgentOutput / SubAgentList / SubAgentStop reader tools (mirrors
// claude-code's TaskOutput / TaskList / TaskStop). When jobsPool is
// nil, `run_in_background` requests gracefully fall back to the
// foreground path so existing tests + minimal embeddings still work.
type Agent struct {
	gate           *permission.Gate
	provider       llm.Provider
	registry       *tools.Registry
	roster         *agent.Roster
	jobsPool       *jobs.Registry // optional; when set, run_in_background works
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

// WithJobsPool plugs in the jobs.Registry used to host background
// sub-agents when `run_in_background:true`. Without a pool the
// schema field is still accepted (claude-code parity) but it falls
// back to the foreground path so tests + minimal embeddings stay
// functional.
func (a Agent) WithJobsPool(p *jobs.Registry) Agent {
	a.jobsPool = p
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
			"run_in_background": map[string]any{
				"type":        "boolean",
				"description": "Set to true to spawn the sub-agent in the background and return a job_id immediately so the parent can continue working. Poll progress via SubAgentOutput / SubAgentList, terminate via SubAgentStop. Mirrors claude-code's AgentTool semantics. Default false (foreground).",
			},
		},
	}
}

// Concurrency is input-aware (Phase G.1, 2026-05-12): a foreground
// Agent call still serializes Exclusive after safe/queue work; a
// background call declares ConcurrencyBackground so the dispatcher
// kicks it off without gating subsequent tools on its completion.
//
// claude-code does the same thing implicitly via task lifecycle —
// the AgentTool returns immediately for run_in_background and the
// dispatcher proceeds. Doing it via Concurrency() keeps metis's
// dispatch graph honest: parallelism is declared, not implicit.
func (Agent) Concurrency(in map[string]any) tools.Concurrency {
	if b, ok := in["run_in_background"].(bool); ok && b {
		return tools.ConcurrencyBackground
	}
	return tools.ConcurrencyExclusive
}

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
	runInBackground, _ := in["run_in_background"].(bool)
	var teammate *agent.Teammate
	if a.roster != nil {
		teammate = &agent.Teammate{
			AgentID:    anonAgentID(),
			Background: runInBackground,
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
		// Background sub-agents detach from Execute's lifetime; the
		// spawned goroutine takes ownership of Unregister. Foreground
		// callers unregister here when Execute returns.
		if !runInBackground {
			defer a.roster.Unregister(teammate.Name)
		}
	}

	// Sub-loop construction is identical in both paths.
	maxIter := intArg(in, "max_iter", 10)
	subSystem := a.system
	if a.minimalSystem != "" {
		subSystem = a.minimalSystem
	}
	sub := agent.NewLoop(a.provider, a.registry, a.gate, agent.NewHookRegistry(), subSystem, maxIter)
	sub.Model = a.model
	sub.AppendUser(prompt)

	// G.0 timeout — wall-clock cap. Caller-provided `timeout_seconds`
	// wins; else config default; 0 disables.
	timeoutSec := intArg(in, "timeout_seconds", 0)
	timeout := time.Duration(timeoutSec) * time.Second
	if timeoutSec == 0 && a.defaultTimeout > 0 {
		timeout = a.defaultTimeout
	}

	// Build the sub-agent ctx with depth + (optional) timeout. For the
	// background path the goroutine owns the cancel func and keeps the
	// ctx alive past Execute's return — context.WithCancel doesn't
	// leak when the deferred goroutine runs cancel on its own exit.
	childCtx, cancel := context.WithCancel(context.WithValue(ctx, agentDepthKey{}, depth+1))
	if timeout > 0 {
		childCtx, cancel = context.WithTimeout(childCtx, timeout)
	}
	if teammate != nil {
		teammate.Cancel = cancel
	}

	parentOut := agent.EventOutFromContext(ctx)

	if runInBackground {
		return a.executeBackground(sub, childCtx, cancel, parentOut, teammate, timeout)
	}
	return a.executeForeground(sub, childCtx, cancel, parentOut, teammate, timeout)
}

// executeForeground is the pre-G.1 behavior: spawn sub-loop in a
// goroutine, drain events synchronously, return the assistant's final
// text as the tool result. The caller blocks until the sub-agent
// finishes.
func (a Agent) executeForeground(
	sub *agent.Loop,
	childCtx context.Context,
	cancel context.CancelFunc,
	parentOut chan<- agent.Event,
	teammate *agent.Teammate,
	timeout time.Duration,
) (*tools.Result, error) {
	defer cancel()

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- sub.Run(childCtx, events)
		close(events)
	}()

	var output strings.Builder
	stopReason := ""
	for ev := range events {
		forwardSubAgentEvent(parentOut, ev)
		switch ev.Kind {
		case agent.EventTextDelta:
			output.WriteString(ev.TextDelta)
			if teammate != nil {
				teammate.AppendText(ev.TextDelta)
			}
		case agent.EventPermissionRequest:
			ev.PermissionReply <- agent.PermissionDecisionDeny
		case agent.EventLoopDone:
			stopReason = ev.StopReason
		case agent.EventError:
			if ev.Err != nil {
				return wrapTimeoutErr(ev.Err, timeout), nil
			}
		}
	}
	if err := <-done; err != nil {
		return wrapTimeoutErr(err, timeout), nil
	}

	out := strings.TrimSpace(output.String())
	if out == "" {
		out = fmt.Sprintf("(sub-agent finished without text output; stop_reason=%s)", stopReason)
	}
	if teammate != nil {
		teammate.Finish(agent.StatusCompleted, out, nil, stopReason)
	}
	return &tools.Result{Output: out}, nil
}

// executeBackground (G.1, 2026-05-12) returns immediately with a
// handshake `{agent_id, name}` tool_result while the actual sub-loop
// runs in a detached goroutine. The model polls SubAgentOutput /
// SubAgentList to read progress and SubAgentStop to terminate.
//
// The goroutine owns the Teammate lifecycle: it appends text deltas
// to teammate.Output as they stream, then calls teammate.Finish on
// exit (Completed / Failed / Killed). The Roster.Unregister is
// likewise deferred inside the goroutine so background sub-agents
// stay visible in /agents list until they actually finish.
func (a Agent) executeBackground(
	sub *agent.Loop,
	childCtx context.Context,
	cancel context.CancelFunc,
	parentOut chan<- agent.Event,
	teammate *agent.Teammate,
	timeout time.Duration,
) (*tools.Result, error) {
	if teammate == nil {
		// No Roster wired — graceful fallback to foreground so callers
		// that opt into run_in_background on a Roster-less embedding
		// still get a useful result (just synchronously).
		return a.executeForeground(sub, childCtx, cancel, parentOut, nil, timeout)
	}

	go func() {
		defer cancel()
		defer func() {
			if a.roster != nil {
				a.roster.Unregister(teammate.Name)
			}
		}()
		// panic recovery — a background sub-agent that panics should
		// land in StatusFailed with a captured error string, not crash
		// the parent process.
		defer func() {
			if r := recover(); r != nil {
				teammate.Finish(agent.StatusFailed, "", fmt.Errorf("panic: %v", r), "panic")
			}
		}()

		events := make(chan agent.Event, 64)
		done := make(chan error, 1)
		go func() {
			done <- sub.Run(childCtx, events)
			close(events)
		}()

		stopReason := ""
		for ev := range events {
			forwardSubAgentEvent(parentOut, ev)
			switch ev.Kind {
			case agent.EventTextDelta:
				teammate.AppendText(ev.TextDelta)
			case agent.EventPermissionRequest:
				ev.PermissionReply <- agent.PermissionDecisionDeny
			case agent.EventLoopDone:
				stopReason = ev.StopReason
			case agent.EventError:
				if ev.Err != nil {
					status := agent.StatusFailed
					hint := stopReason
					if errors.Is(ev.Err, context.DeadlineExceeded) && timeout > 0 {
						hint = fmt.Sprintf("timeout %s", timeout)
					} else if errors.Is(ev.Err, context.Canceled) {
						status = agent.StatusKilled
						hint = "cancelled"
					}
					teammate.Finish(status, teammateSnapshotOutput(teammate), ev.Err, hint)
					return
				}
			}
		}
		err := <-done
		final := teammateSnapshotOutput(teammate)
		switch {
		case err == nil:
			teammate.Finish(agent.StatusCompleted, final, nil, stopReason)
		case errors.Is(err, context.DeadlineExceeded) && timeout > 0:
			teammate.Finish(agent.StatusFailed, final, err, fmt.Sprintf("timeout %s", timeout))
		case errors.Is(err, context.Canceled):
			teammate.Finish(agent.StatusKilled, final, err, "cancelled")
		default:
			teammate.Finish(agent.StatusFailed, final, err, stopReason)
		}
	}()

	return &tools.Result{
		Output: fmt.Sprintf(
			"sub-agent spawned in background (agent_id=%s, name=%s). Poll progress with SubAgentOutput, list active sub-agents with SubAgentList, terminate with SubAgentStop.",
			teammate.AgentID, teammate.Name,
		),
		Meta: map[string]any{
			"agent_id":   teammate.AgentID,
			"name":       teammate.Name,
			"background": true,
		},
	}, nil
}

// forwardSubAgentEvent forwards selected sub-agent events to the
// parent's UI channel for live progress rendering. Shared by both
// foreground and background paths so the UX is identical regardless
// of how the sub-agent was spawned.
func forwardSubAgentEvent(parentOut chan<- agent.Event, ev agent.Event) {
	if parentOut == nil {
		return
	}
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

// teammateSnapshotOutput is a tiny convenience for the background
// runner: capture whatever's accumulated in the teammate so far when
// we hit an error / cancellation, so callers reading
// SubAgentOutput after a failure still see partial progress.
func teammateSnapshotOutput(t *agent.Teammate) string {
	return strings.TrimSpace(t.Snapshot().Output)
}

// wrapTimeoutErr converts a sub-loop error into a user-readable
// tool_result. Pulled out of executeForeground so both
// EventError + `<-done` arms can share the message — without this,
// "timed out after 30s" only appeared on one path.
func wrapTimeoutErr(err error, timeout time.Duration) *tools.Result {
	if errors.Is(err, context.DeadlineExceeded) && timeout > 0 {
		return &tools.Result{
			Output: fmt.Sprintf(
				"sub-agent timed out after %s. Re-spawn with `timeout_seconds: <larger>` if the task legitimately needs more wall-clock budget; otherwise scope down the prompt.",
				timeout,
			),
			IsError: true,
		}
	}
	return &tools.Result{Output: err.Error(), IsError: true}
}
