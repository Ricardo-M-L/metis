package builtin

// subagent_jobs.go — model-facing reader tools for the sub-agent pool.
// SubAgentList / SubAgentOutput / SubAgentStop pair with the Agent tool's
// `run_in_background:true` handshake (G.1, 2026-05-12), the same way
// bash.List / bash.Output / bash.Kill pair with Bash run_in_background.
//
// Layered on top of agent.Roster: the Roster is a single source of
// truth for live sub-agents (named teammates AND anonymous
// background spawns), and these three tools just expose it to the
// model via the standard tool surface.
//
// Mirrors claude-code's TaskList / TaskOutput / TaskStop tools but
// scoped to sub-agents only (the bash job pool keeps its own
// bash.List/bash.Output/bash.Kill — having one set of tools per
// pool keeps the model's tool-pool list discoverable).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// SubAgentList — enumerate live + recently-finished sub-agents.
//
// The model uses this to discover background sub-agents it spawned
// earlier in the turn and decide whether they're worth polling. We
// include finished entries (Completed / Failed / Killed) up to a
// short retention window so the model can correlate "task I asked
// alice to do" with "yep alice finished, here's the result" without
// having to remember an agent_id verbatim.
type SubAgentList struct {
	tools.BaseTool
	gate    *permission.Gate
	roster  *agent.Roster
	history *subAgentQueryScope
}

func NewSubAgentList(gate *permission.Gate, r *agent.Roster) SubAgentList {
	return SubAgentList{gate: gate, roster: r}
}

func (s SubAgentList) WithSessionPersistence(sessionDir, parentSessionID string) SubAgentList {
	s.history = &subAgentQueryScope{sessionDir: sessionDir, parentID: parentSessionID}
	return s
}

func (SubAgentList) Name() string { return "SubAgentList" }
func (SubAgentList) Description() string {
	return "List active and saved sub-agents belonging to the current direct parent session. Returns agent_id, name, status (queued/running/completed/failed/killed), load_state and terminal stop reason, plus a result snippet. Saved producers not loaded here without a terminal receipt have status=unknown; an empty live roster does not mean history was deleted. Use optional limit/offset for pages and agent_id for exact historical queries. Desktop queued agents await execution capacity and can be cancelled with SubAgentStop."
}
func (SubAgentList) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum rows, default 50."},
			"offset": map[string]any{"type": "integer", "minimum": 0, "maximum": subAgentHistoryMaxScan, "description": "Page offset in the known parent-scoped history."},
		},
	}
}

// SubAgentList is read-only — fan it out alongside other safe tools.
func (SubAgentList) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (s SubAgentList) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := s.gate.Check(context.Background(), "SubAgentList", "")
	return mapDecision(d), src
}

func (s SubAgentList) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if s.history != nil {
		return s.executeHistorical(ctx, in)
	}
	if s.roster == nil {
		return &tools.Result{Output: "(no sub-agents — Roster wiring missing. This typically happens in headless `metis run` mode or when the loop was constructed without a Roster. Use `metis chat` for interactive sub-agent dispatch.)"}, nil
	}
	teammates := s.roster.List()
	if len(teammates) == 0 {
		return &tools.Result{Output: "(no sub-agents in flight)"}, nil
	}
	var b strings.Builder
	for _, t := range teammates {
		snap := t.Snapshot()
		dur := time.Since(snap.Started).Round(time.Second)
		if !snap.EndTime.IsZero() {
			dur = snap.EndTime.Sub(snap.Started).Round(time.Second)
		}
		fmt.Fprintf(&b, "agent_id=%s name=%s status=%s elapsed=%s background=%v",
			snap.AgentID, snap.Name, snap.Status, dur, snap.Background)
		if snap.StopHint != "" {
			fmt.Fprintf(&b, " hint=%q", snap.StopHint)
		}
		// Truncated result snippet for finished sub-agents so the
		// model can decide whether to read full output without firing
		// a second tool call.
		if !snap.Status.IsActive() && snap.Result != "" {
			snip := snap.Result
			if len(snip) > 120 {
				snip = snip[:120] + "…"
			}
			snip = strings.ReplaceAll(snip, "\n", " ⏎ ")
			fmt.Fprintf(&b, " result=%q", snip)
		}
		b.WriteByte('\n')
	}
	return &tools.Result{Output: strings.TrimSpace(b.String())}, nil
}

// ---------------------------------------------------------------------------

// SubAgentOutput — read the running text output for a specific sub-agent.
//
// Returns the assistant text deltas accumulated so far PLUS the
// terminal status if the sub-agent has finished. Safe to call mid-
// run; the read is a snapshot, doesn't block the sub-agent.
type SubAgentOutput struct {
	tools.BaseTool
	gate    *permission.Gate
	roster  *agent.Roster
	history *subAgentQueryScope
}

func NewSubAgentOutput(gate *permission.Gate, r *agent.Roster) SubAgentOutput {
	return SubAgentOutput{gate: gate, roster: r}
}

func (s SubAgentOutput) WithSessionPersistence(sessionDir, parentSessionID string) SubAgentOutput {
	s.history = &subAgentQueryScope{sessionDir: sessionDir, parentID: parentSessionID}
	return s
}

func (SubAgentOutput) Name() string { return "SubAgentOutput" }
func (SubAgentOutput) Description() string {
	return "Read a sub-agent's output and actual lifecycle/stop reason, including saved history after restart or live-roster eviction. Pass `agent_id` (preferred) or an unambiguous `name` within the current direct parent session. Does not resume or execute the child. A not_loaded record still exists; missing/corrupt history is reported explicitly. Safe to call mid-run."
}
func (SubAgentOutput) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent_id": map[string]any{
				"type":        "string",
				"description": "The sub-agent's agent_id (e.g. \"agt-d3a91b07\") as returned by the Agent tool's background handshake.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Alternative to agent_id — the sub-agent's name if it was started with Agent({name: \"...\"}).",
			},
		},
	}
}

func (SubAgentOutput) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (s SubAgentOutput) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := s.gate.Check(context.Background(), "SubAgentOutput", "")
	return mapDecision(d), src
}

func (s SubAgentOutput) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if s.history != nil {
		return s.executeHistorical(ctx, in)
	}
	if s.roster == nil {
		return &tools.Result{Output: "(no sub-agents — Roster wiring missing. This typically happens in headless `metis run` mode or when the loop was constructed without a Roster. Use `metis chat` for interactive sub-agent dispatch.)", IsError: true}, nil
	}
	t, err := lookupTeammate(s.roster, in)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	snap := t.Snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "agent_id=%s name=%s status=%s\n",
		snap.AgentID, snap.Name, snap.Status)
	if snap.Output != "" {
		b.WriteString("---output---\n")
		b.WriteString(snap.Output)
		if !strings.HasSuffix(snap.Output, "\n") {
			b.WriteByte('\n')
		}
	}
	if !snap.Status.IsActive() {
		fmt.Fprintf(&b, "---end (elapsed=%s)---", snap.EndTime.Sub(snap.Started).Round(time.Second))
		if snap.StopHint != "" {
			fmt.Fprintf(&b, " %s", snap.StopHint)
		}
	}
	return &tools.Result{Output: strings.TrimSpace(b.String())}, nil
}

// ---------------------------------------------------------------------------

// SubAgentStop — cancel a queued or running sub-agent.
//
// Records a cancellation request which cascades through ctx → sub-loop →
// tools as soon as the child callback is installed. The sub-agent transitions
// to StatusKilled and finishes its accumulated output snapshot for one last
// SubAgentOutput read.
type SubAgentStop struct {
	tools.BaseTool
	gate   *permission.Gate
	roster *agent.Roster
}

func NewSubAgentStop(gate *permission.Gate, r *agent.Roster) SubAgentStop {
	return SubAgentStop{gate: gate, roster: r}
}

func (SubAgentStop) Name() string { return "SubAgentStop" }
func (SubAgentStop) Description() string {
	return "Cancel a queued or running sub-agent. Pass `agent_id` (preferred) or `name`. Cancellation interrupts execution-capacity waits and running tools; partial output is preserved for one last SubAgentOutput read."
}
func (SubAgentStop) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent_id": map[string]any{
				"type":        "string",
				"description": "The sub-agent's agent_id (e.g. \"agt-d3a91b07\").",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Alternative to agent_id — the sub-agent's name.",
			},
		},
	}
}

// Stop is destructive — keep it Exclusive so it doesn't race a
// concurrent SubAgentOutput on the same teammate.
func (SubAgentStop) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}

func (s SubAgentStop) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := s.gate.Check(context.Background(), "SubAgentStop", "")
	return mapDecision(d), src
}

func (s SubAgentStop) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if s.roster == nil {
		return &tools.Result{Output: "(no sub-agents — Roster wiring missing. This typically happens in headless `metis run` mode or when the loop was constructed without a Roster. Use `metis chat` for interactive sub-agent dispatch.)", IsError: true}, nil
	}
	t, err := lookupTeammate(s.roster, in)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	snap := t.Snapshot()
	if !snap.Status.IsActive() {
		return &tools.Result{Output: fmt.Sprintf("sub-agent %s already %s", snap.AgentID, snap.Status)}, nil
	}
	// RequestCancel is deliberately safe even while Agent.Execute is still
	// constructing the child context. A direct read of t.Cancel would race its
	// publication and could lose a stop request in that short setup window.
	t.RequestCancel()
	return &tools.Result{Output: fmt.Sprintf("sub-agent %s (name=%s) cancellation requested", snap.AgentID, snap.Name)}, nil
}

// ---------------------------------------------------------------------------

// lookupTeammate resolves reader/stop targets from live or recently finished
// generations. Peer-message delivery deliberately uses live-only Lookup.
func lookupTeammate(r *agent.Roster, in map[string]any) (*agent.Teammate, error) {
	id, _ := in["agent_id"].(string)
	name, _ := in["name"].(string)
	if id == "" && name == "" {
		return nil, errors.New("either agent_id or name is required")
	}
	if id != "" {
		t, ok := r.LookupByAgentID(id)
		if !ok {
			return nil, fmt.Errorf("no sub-agent with agent_id=%s in this process; use parent-scoped saved-history queries when persistence is configured", id)
		}
		return t, nil
	}
	t, ok := r.Lookup(name)
	if !ok {
		t, _, ok = r.LookupRecentlyFinished(name)
		if !ok {
			return nil, fmt.Errorf("no sub-agent with name=%s in this process; use its stable agent_id for parent-scoped saved-history queries", name)
		}
	}
	return t, nil
}

// AttachSubAgentTools registers the three SubAgent* tools on the
// registry. Called by runtime/tools.go after the Roster is wired so
// the model gets a complete background-sub-agent surface in one step.
//
// Roster nil disables the tools (they'd just return "no sub-agents")
// — same graceful-degrade pattern as AttachJobsRegistry.
func AttachSubAgentTools(reg *tools.Registry, gate *permission.Gate, r *agent.Roster, scopes ...SubAgentQueryScope) {
	if reg == nil || (r == nil && len(scopes) == 0) {
		return
	}
	list, output := NewSubAgentList(gate, r), NewSubAgentOutput(gate, r)
	if len(scopes) > 0 {
		scope := scopes[len(scopes)-1]
		list = list.WithSessionPersistence(scope.SessionDir, scope.ParentSessionID)
		output = output.WithSessionPersistence(scope.SessionDir, scope.ParentSessionID)
	}
	reg.Register(list)
	reg.Register(output)
	if r != nil {
		reg.Register(NewSubAgentStop(gate, r))
	}
}
