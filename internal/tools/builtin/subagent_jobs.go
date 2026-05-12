package builtin

// subagent_jobs.go — model-facing reader tools for the sub-agent pool.
// SubAgentList / SubAgentOutput / SubAgentStop pair with the Agent tool's
// `run_in_background:true` handshake (G.1, 2026-05-12), the same way
// BashList / BashOutput / BashKill pair with Bash run_in_background.
//
// Layered on top of agent.Roster: the Roster is a single source of
// truth for live sub-agents (named teammates AND anonymous
// background spawns), and these three tools just expose it to the
// model via the standard tool surface.
//
// Mirrors claude-code's TaskList / TaskOutput / TaskStop tools but
// scoped to sub-agents only (the bash job pool keeps its own
// BashList/BashOutput/BashKill — having one set of tools per
// pool keeps the model's tool-pool list discoverable).

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	gate   *permission.Gate
	roster *agent.Roster
}

func NewSubAgentList(gate *permission.Gate, r *agent.Roster) SubAgentList {
	return SubAgentList{gate: gate, roster: r}
}

func (SubAgentList) Name() string { return "SubAgentList" }
func (SubAgentList) Description() string {
	return "List active and recently-finished sub-agents in the current session. Returns each one's agent_id, name, status (running/completed/failed/killed), started_at, and (for completed) the final result snippet. Pairs with Agent({run_in_background: true}) — use this to discover what's still in flight."
}
func (SubAgentList) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
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

func (s SubAgentList) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	if s.roster == nil {
		return &tools.Result{Output: "(no sub-agents — Roster not wired in this session)"}, nil
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
		if snap.Status != agent.StatusRunning && snap.Result != "" {
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
	gate   *permission.Gate
	roster *agent.Roster
}

func NewSubAgentOutput(gate *permission.Gate, r *agent.Roster) SubAgentOutput {
	return SubAgentOutput{gate: gate, roster: r}
}

func (SubAgentOutput) Name() string { return "SubAgentOutput" }
func (SubAgentOutput) Description() string {
	return "Read the assistant text accumulated so far by a specific sub-agent. Pass `agent_id` (preferred) or `name`. Returns the running output + current status. Safe to call mid-run."
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

func (s SubAgentOutput) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if s.roster == nil {
		return &tools.Result{Output: "(no sub-agents — Roster not wired in this session)", IsError: true}, nil
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
	if snap.Status != agent.StatusRunning {
		fmt.Fprintf(&b, "---end (elapsed=%s)---", snap.EndTime.Sub(snap.Started).Round(time.Second))
		if snap.StopHint != "" {
			fmt.Fprintf(&b, " %s", snap.StopHint)
		}
	}
	return &tools.Result{Output: strings.TrimSpace(b.String())}, nil
}

// ---------------------------------------------------------------------------

// SubAgentStop — terminate a running sub-agent.
//
// Calls the teammate's stored Cancel func which cascades through
// ctx → sub-loop → tools. The sub-agent transitions to StatusKilled
// and finishes its accumulated output snapshot for one last
// SubAgentOutput read.
type SubAgentStop struct {
	gate   *permission.Gate
	roster *agent.Roster
}

func NewSubAgentStop(gate *permission.Gate, r *agent.Roster) SubAgentStop {
	return SubAgentStop{gate: gate, roster: r}
}

func (SubAgentStop) Name() string { return "SubAgentStop" }
func (SubAgentStop) Description() string {
	return "Terminate a running sub-agent. Pass `agent_id` (preferred) or `name`. The sub-agent's tools see context cancellation, partial output is preserved for one last SubAgentOutput read."
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
		return &tools.Result{Output: "(no sub-agents — Roster not wired in this session)", IsError: true}, nil
	}
	t, err := lookupTeammate(s.roster, in)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	snap := t.Snapshot()
	if snap.Status != agent.StatusRunning {
		return &tools.Result{Output: fmt.Sprintf("sub-agent %s already %s", snap.AgentID, snap.Status)}, nil
	}
	if t.Cancel != nil {
		t.Cancel()
	}
	return &tools.Result{Output: fmt.Sprintf("sub-agent %s (name=%s) cancellation requested", snap.AgentID, snap.Name)}, nil
}

// ---------------------------------------------------------------------------

// lookupTeammate is the shared agent_id-or-name resolver. Used by all
// three SubAgent* reader tools so the error messaging stays
// consistent regardless of which tool the model picked.
func lookupTeammate(r *agent.Roster, in map[string]any) (*agent.Teammate, error) {
	id, _ := in["agent_id"].(string)
	name, _ := in["name"].(string)
	if id == "" && name == "" {
		return nil, errors.New("either agent_id or name is required")
	}
	if id != "" {
		t, ok := r.LookupByAgentID(id)
		if !ok {
			return nil, fmt.Errorf("no sub-agent with agent_id=%s (it may have finished and been GC'd; try SubAgentList)", id)
		}
		return t, nil
	}
	t, ok := r.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("no sub-agent with name=%s (it may have finished; try SubAgentList)", name)
	}
	return t, nil
}

// AttachSubAgentTools registers the three SubAgent* tools on the
// registry. Called by runtime/tools.go after the Roster is wired so
// the model gets a complete background-sub-agent surface in one step.
//
// Roster nil disables the tools (they'd just return "no sub-agents")
// — same graceful-degrade pattern as AttachJobsRegistry.
func AttachSubAgentTools(reg *tools.Registry, gate *permission.Gate, r *agent.Roster) {
	if reg == nil || r == nil {
		return
	}
	reg.Register(NewSubAgentList(gate, r))
	reg.Register(NewSubAgentOutput(gate, r))
	reg.Register(NewSubAgentStop(gate, r))
}

// ensure sort is referenced — used below for stable ordering when we
// add filters/limits later.
var _ = sort.Strings
