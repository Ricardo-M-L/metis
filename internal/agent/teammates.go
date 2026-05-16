// Package agent — sub-agent Roster.
//
// Added 2026-05-12 alongside Phase G of the claude-code-parity plan
// (multi-agent expansion). The Roster is the cross-cutting registry
// that every live sub-agent (anonymous via `Agent({prompt: ...})`,
// named teammate via `Agent({name: "alice", ...})`, foreground or
// background) lands in for the duration of its run.
//
// Lives in `internal/agent/` (not `internal/runtime/`) so that
// `internal/tools/builtin/agent.go` — which already imports
// `internal/agent` — can use it without an import cycle. The runtime
// package wires the Roster as a singleton and threads it into the
// Agent + Fork tools at construction time.
//
// Two roles:
//
//  1. G.0 — concurrency cap: `Register` refuses past `Capacity`,
//     letting Agent.Execute return IsError instead of fork-bombing
//     the API.
//  2. G.3 — named teammates: `Lookup(name)` lets the SendMessage
//     tool find a peer's mailbox without going through the parent
//     agent. Anonymous sub-agents get an auto-generated name
//     (`_anon-<8hex>`) so the structure is uniform.
//
// Mailbox is a buffered channel that the sub-agent's run loop drains
// when between turns (see agent.Loop). Buffer size 16 was picked to
// match claude-code's per-teammate message-queue cap.

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// PeerMessage carries a SendMessage payload between named teammates.
// `From` is the sender's roster name (auto-generated for anonymous
// senders); `Body` is the model-authored text.
type PeerMessage struct {
	From string
	Body string
	Sent time.Time
}

// TeammateKind tags a sub-agent by orchestration shape (G.12,
// 2026-05-12). The four kinds map onto the lifecycle distinctions
// claude-code's task-router makes:
//
//   - KindAnon: anonymous one-shot Agent({prompt}) — no Roster name
//   - KindNamed: named teammate Agent({name, prompt}) addressable by
//     other teammates via MessageTeammate
//   - KindWorkflow: a hosted multi-step workflow run (placeholder
//     for future LocalWorkflowTask wiring; today it's a future-
//     proof enum value, not actively used)
//   - KindMcpMonitor: long-lived MCP-server monitor sub-agent
//     (placeholder for future MonitorMcpTask wiring)
//
// Today only KindAnon and KindNamed have concrete spawn paths in
// the Agent tool. The other two are reserved so when those features
// land, the existing `/agents list` + SubAgent* tools can group by
// kind without re-shaping the API.
type TeammateKind int

const (
	KindAnon       TeammateKind = iota // anonymous Agent({prompt})
	KindNamed                          // named Agent({name, prompt})
	KindWorkflow                       // future: hosted workflow run
	KindMcpMonitor                     // future: long-lived MCP monitor
)

func (k TeammateKind) String() string {
	switch k {
	case KindAnon:
		return "anon"
	case KindNamed:
		return "named"
	case KindWorkflow:
		return "workflow"
	case KindMcpMonitor:
		return "mcp_monitor"
	}
	return "unknown"
}

// TeammateStatus is the lifecycle stage of a sub-agent.
// Monotonic except Killed which can come from any non-terminal state.
type TeammateStatus int

const (
	// StatusRunning — sub-loop active, output growing.
	StatusRunning TeammateStatus = iota
	// StatusCompleted — sub-loop ended cleanly (end_turn / no_tool_calls).
	StatusCompleted
	// StatusFailed — sub-loop errored (timeout, panic, provider error).
	StatusFailed
	// StatusKilled — SubAgentStop or Roster.CancelAll terminated it.
	StatusKilled
)

func (s TeammateStatus) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusKilled:
		return "killed"
	}
	return "unknown"
}

// Teammate is one live sub-agent's roster entry.
//
// Output is the running text-delta accumulator for SubAgentOutput. It
// grows as the sub-loop streams text and freezes when Status leaves
// Running. Kept in-memory because sub-agent output is small relative
// to bash (no `tail -f /var/log` style firehose) and the simpler model
// makes the SubAgentOutput tool a synchronous read instead of disk I/O.
type Teammate struct {
	mu sync.RWMutex // guards Status / Output / Result / EndTime / ExitErr

	// Name is what callers pass via `Agent({name: ...})`. Anonymous
	// sub-agents get an auto-generated `_anon-<8hex>` prefix so the
	// Roster doesn't need a second path.
	Name string

	// AgentID — short hex (`agt-<8hex>`) that the model uses to address
	// this sub-agent via SubAgentOutput / SubAgentStop. Returned as
	// part of the run_in_background handshake tool_result.
	AgentID string

	// Anonymous flags auto-generated names so UI can hide them from
	// the roster view (`/agents list`) by default — claude-code
	// shows only named teammates by default too.
	Anonymous bool

	// StrictName tells Roster.Register to REJECT (with ErrNameInUse)
	// instead of auto-suffixing on a name collision. Used by slash
	// commands that resume by exact name (`/agents resume alice` must
	// land on the SAME alice, not spawn a fresh alice-2 silently).
	// Default false → most spawn paths auto-suffix and stay
	// resilient to model-picked collisions.
	StrictName bool

	// Background true means this sub-agent was spawned with
	// `run_in_background:true`. UI uses this to decide whether to
	// auto-block the parent on its completion (foreground) or render
	// a non-blocking pill (background).
	Background bool

	Started time.Time

	// Mailbox accepts inbound PeerMessage from SendMessage tool calls.
	// Buffered (size 16); a full mailbox drops the message and returns
	// IsError to the sender so the model can adapt instead of blocking.
	Mailbox chan PeerMessage

	// Cancel terminates the sub-agent's context. Called by /agents kill
	// and by the Roster when parent ctx is cancelled.
	Cancel func()

	// Status / Output / Result / EndTime / ExitErr — set by the
	// sub-loop's runner goroutine; read via Snapshot() under the mutex
	// so SubAgentOutput / SubAgentList never race the streaming writer.
	Status   TeammateStatus
	Output   strings.Builder
	Result   string // final text on Completed, last partial on Killed/Failed
	EndTime  time.Time
	ExitErr  error
	StopHint string // user-facing reason ("timeout 600s", "user kill", "max_iter exhausted")
}

// IsNamed reports whether this teammate was explicitly named at
// spawn time (G.11, 2026-05-12). Anonymous spawns get an auto-
// generated `_anon-<hex>` prefix that's hidden from the default
// /agents listing — IsNamed lets `/agents list` filter cleanly
// without exposing the prefix as a stringly-typed contract.
//
// Equivalent to `!t.Anonymous`, but using a method makes call-
// sites self-documenting and gives us room to layer in more
// promotion rules later (e.g. "named iff started_at > runtime_boot
// AND name didn't start with `_`"). For now: simple negation.
func (t *Teammate) IsNamed() bool {
	if t == nil {
		return false
	}
	return !t.Anonymous
}

// AppendText is the streaming-text accumulator. Called by the
// sub-agent's event-forwarding goroutine on every EventTextDelta;
// SubAgentOutput reads what's been accumulated so far via Snapshot().
func (t *Teammate) AppendText(s string) {
	if s == "" {
		return
	}
	t.mu.Lock()
	t.Output.WriteString(s)
	t.mu.Unlock()
}

// Finish atomically transitions Status to a terminal state and
// captures the final result. Safe to call once per Teammate; the
// runner goroutine is responsible for not double-firing.
func (t *Teammate) Finish(status TeammateStatus, result string, exitErr error, hint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != StatusRunning {
		return // monotonic — first finish wins
	}
	t.Status = status
	t.Result = result
	t.ExitErr = exitErr
	t.StopHint = hint
	t.EndTime = time.Now()
}

// TeammateSnapshot is the read-only view returned by Teammate.Snapshot()
// + Roster.List(). Detaches caller from the mutex so the reader can't
// accidentally race the runner.
type TeammateSnapshot struct {
	Name       string
	AgentID    string
	Anonymous  bool
	Background bool
	Started    time.Time
	Status     TeammateStatus
	Output     string
	Result     string
	EndTime    time.Time
	ExitErr    error
	StopHint   string
}

// IsNamed mirrors Teammate.IsNamed on the snapshot view so /agents
// list can filter without re-grabbing the live mutex.
func (s TeammateSnapshot) IsNamed() bool { return !s.Anonymous }

// Snapshot returns a thread-safe copy of the Teammate's mutable state.
// SubAgentOutput uses this to read the running buffer + status atomically.
func (t *Teammate) Snapshot() TeammateSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return TeammateSnapshot{
		Name:       t.Name,
		AgentID:    t.AgentID,
		Anonymous:  t.Anonymous,
		Background: t.Background,
		Started:    t.Started,
		Status:     t.Status,
		Output:     t.Output.String(),
		Result:     t.Result,
		EndTime:    t.EndTime,
		ExitErr:    t.ExitErr,
		StopHint:   t.StopHint,
	}
}

// Kind returns the orchestration tag for this teammate (G.12,
// 2026-05-12). Derived: named teammates → KindNamed; anonymous →
// KindAnon. Future workflows / MCP monitors set the field
// explicitly when spawning. nil-safe (returns KindAnon).
func (t *Teammate) Kind() TeammateKind {
	if t == nil {
		return KindAnon
	}
	if !t.Anonymous {
		return KindNamed
	}
	return KindAnon
}

// RosterSummary is the at-a-glance state of the live Roster — for
// status bar chips, /agents one-line, and observability hooks
// (G.16, 2026-05-12). Cheaper than List() when callers just need
// counts.
type RosterSummary struct {
	// Total active teammates across all kinds.
	Total int
	// Named is the count of teammates with a model-chosen name (not
	// auto-anonymized). What the user typically thinks of as "the
	// teammates."
	Named int
	// Anonymous count. Hidden from /agents list by default.
	Anonymous int
	// Background count — sub-agents spawned with run_in_background:
	// true. Subset of Total.
	Background int
}

// Summary returns a snapshot of teammate counts grouped by shape.
// Cheap read-lock; safe to call from the status-bar render path.
func (r *Roster) Summary() RosterSummary {
	if r == nil {
		return RosterSummary{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := RosterSummary{}
	for _, t := range r.teammates {
		out.Total++
		if t.Anonymous {
			out.Anonymous++
		} else {
			out.Named++
		}
		if t.Background {
			out.Background++
		}
	}
	return out
}

// Roster is the live-sub-agent registry. Thread-safe.
//
// Fix 2 (2026-05-15) — `recentlyFinished` LRU buffer. Pre-fix,
// Unregister hard-deleted the teammate the instant the sub-agent's
// loop returned. A parent that called Agent(run_in_background:true)
// and then polled SubAgentOutput would hit "no sub-agent with
// agent_id=..." for any sub-agent that finished between two polls,
// losing its output. The buffer keeps the last RosterFinishedKeep
// finished teammates around so polling parents can still read them.
// LRU bound keeps memory bounded across long sessions.
type Roster struct {
	mu        sync.RWMutex
	teammates map[string]*Teammate
	// recentlyFinished is the keep-around list of teammates whose
	// sub-loop has exited but whose Output/Status the parent may
	// still want to read. Append-only with size cap; oldest evicted
	// when over RosterFinishedKeep. Stored separately from teammates
	// so cap-counted Count() / Summary() report only live work.
	recentlyFinished []*Teammate
	capNamed         int
	capAnon          int
}

// RosterFinishedKeep is the max number of finished teammates the
// Roster preserves for SubAgentOutput / SubAgentList polling. 50 is
// generous enough to survive a 5-parallel-spawn-poll cycle ~10 times
// (covering most coordinator patterns) without unbounded memory
// growth.
const RosterFinishedKeep = 50

// NewRoster creates a Roster with split caps for named teammates and
// anonymous sub-agents. Either <= 0 disables that side's cap.
// Production defaults are config.Agents.MaxConcurrentNamed (20) +
// MaxConcurrentAnon (40).
//
// Two caps instead of one: named teammates are "team roster slots"
// (addressable, persistent, shown in /agents list) while anonymous
// sub-agents are "research helpers" (one-shot, hidden from list).
// Mixing them in one pool meant 5 anon explorations filled the budget
// and locked out the next named reviewer spawn — exactly the wrong
// incentive. Split caps let the model spawn explore helpers freely
// without starving the named teammate pool.
//
// Backwards-compat: pre-2026-05-14 callers used `NewRoster(cap)` for
// a combined cap. Single-int callers route through the variadic
// shim below — first arg is `capNamed`, optional second is `capAnon`.
// One-arg form `NewRoster(N)` mirrors the old semantics: both pools
// get N (or unlimited when N == 0).
func NewRoster(capNamed int, capAnon ...int) *Roster {
	anon := capNamed
	if len(capAnon) > 0 {
		anon = capAnon[0]
	}
	return &Roster{
		teammates: make(map[string]*Teammate),
		capNamed:  capNamed,
		capAnon:   anon,
	}
}

// ErrCapacityExceeded is returned by Register when the Roster is full.
// Callers (Agent.Execute) wrap it as a tool IsError so the model can
// retry later or scope down.
var ErrCapacityExceeded = errors.New("sub-agent capacity exceeded")

// ErrNameInUse is returned when Register encounters a duplicate Name.
// Two named teammates can't co-exist with the same name — pick another
// or wait for the prior one to finish.
var ErrNameInUse = errors.New("teammate name already in use")

// Register places t in the Roster. Returns ErrCapacityExceeded when
// the capacity cap is hit. If t.Name is empty an anonymous name is
// auto-generated and t.Name overwritten in place.
//
// Name-collision policy (mirrors claude-code spawnMultiAgent.ts:267):
// when t.Name conflicts with a live teammate, Register auto-suffixes
// with -2, -3, ... up to 99 and overwrites t.Name in place. Callers
// that want STRICT rejection on conflict should set t.StrictName
// before calling — that path still returns ErrNameInUse.
//
// Rationale: the model picks names semantically ("reviewer",
// "explorer"); making it reason about uniqueness is wasted reasoning.
// Auto-suffix follows the principle that names are display labels,
// not identity. ErrNameInUse stays for the rare caller that needs
// exact-match semantics (e.g., a slash command resuming a specific
// named teammate by name — auto-suffix there would silently spawn a
// new agent instead of resuming, which is wrong).
func (r *Roster) Register(t *Teammate) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Determine the kind FIRST so the cap check below picks the
	// right side. Empty-name → anon; explicit non-empty name → named
	// (unless caller already set Anonymous=true for some custom flow).
	if t.Name == "" {
		t.Name = anonName()
		t.Anonymous = true
	}

	// Cap check by kind. Two pools, two budgets.
	var inKind int
	for _, existing := range r.teammates {
		if existing.Anonymous == t.Anonymous {
			inKind++
		}
	}
	var cap int
	if t.Anonymous {
		cap = r.capAnon
	} else {
		cap = r.capNamed
	}
	if cap > 0 && inKind >= cap {
		return ErrCapacityExceeded
	}
	if _, ok := r.teammates[t.Name]; ok {
		switch {
		case t.Anonymous:
			// Vanishingly rare 64-bit collision; regenerate once.
			t.Name = anonName()
		case t.StrictName:
			return fmt.Errorf("%w: %q", ErrNameInUse, t.Name)
		default:
			// Auto-suffix: alice → alice-2 → alice-3 → ... → alice-99.
			// 99 is generous; if a single Roster has 99 collisions on
			// the same base name the user has bigger problems than
			// naming.
			base := t.Name
			found := false
			for i := 2; i <= 99; i++ {
				candidate := fmt.Sprintf("%s-%d", base, i)
				if _, taken := r.teammates[candidate]; !taken {
					t.Name = candidate
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: %q (99 suffixes exhausted)", ErrNameInUse, base)
			}
		}
	}
	// 2026-05-16: Only named teammates get a Mailbox. Anonymous
	// sub-agents are part of the Sub-Agent paradigm (claude-code 架构图
	// image 3 "硬性约束: 子智能体不能相互通信") — denying them a
	// mailbox at type level beats the previous description-only refusal
	// in MessageTeammate, which a clever model could try to bypass.
	// MessageTeammate's Execute now sees nil Mailbox and rejects.
	if !t.Anonymous && t.Mailbox == nil {
		t.Mailbox = make(chan PeerMessage, 16)
	}
	if t.Started.IsZero() {
		t.Started = time.Now()
	}
	r.teammates[t.Name] = t
	return nil
}

// Unregister moves a teammate from the live map to recentlyFinished
// (Fix 2 / 2026-05-15). Safe to call even when the name isn't
// present (a Register that returned an error leaves no entry, but
// the caller still defers Unregister). The teammate stays
// discoverable via LookupByAgentID and shows up in List() until
// evicted from the LRU.
func (r *Roster) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.teammates[name]
	if !ok {
		return
	}
	delete(r.teammates, name)
	r.recentlyFinished = append(r.recentlyFinished, t)
	// LRU evict: drop the oldest entries when over cap.
	if over := len(r.recentlyFinished) - RosterFinishedKeep; over > 0 {
		r.recentlyFinished = r.recentlyFinished[over:]
	}
}

// Count returns the number of currently-registered teammates.
// Used by Agent.Execute to check the cap before kicking off a new
// sub-agent. Cheap read-lock.
func (r *Roster) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.teammates)
}

// Capacity returns the COMBINED cap (named + anon) for backward
// compat with callers / docs that talk about a single number.
// 0 here can only happen if both sides are unlimited.
func (r *Roster) Capacity() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.capNamed <= 0 && r.capAnon <= 0 {
		return 0
	}
	return r.capNamed + r.capAnon
}

// CapacityNamed returns the configured cap on named teammates.
// 0 = unlimited.
func (r *Roster) CapacityNamed() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capNamed
}

// CapacityAnon returns the configured cap on anonymous sub-agents.
// 0 = unlimited.
func (r *Roster) CapacityAnon() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capAnon
}

// Lookup returns the named teammate, if any. The (Teammate, ok)
// pattern is used by SendMessage to refuse delivery when the target
// has finished or never existed.
func (r *Roster) Lookup(name string) (*Teammate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.teammates[name]
	return t, ok
}

// LookupByAgentID finds a teammate by their AgentID (the `agt-<8hex>`
// handle returned to the model in run_in_background's tool_result).
// Used by SubAgentOutput / SubAgentStop which receive the agent_id
// rather than the name. Linear scan because the typical Roster size
// is < 10 — a separate index isn't worth the maintenance.
//
// Fix 2 (2026-05-15) — also scans recentlyFinished so a parent that
// polls just after a sub-agent completes still finds it. Live
// teammates are checked first since cap-bounded recentlyFinished
// could in theory contain a stale entry with the same agent_id
// (e.g., resume from snapshot); the live entry always wins.
func (r *Roster) LookupByAgentID(id string) (*Teammate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.teammates {
		if t.AgentID == id {
			return t, true
		}
	}
	for _, t := range r.recentlyFinished {
		if t.AgentID == id {
			return t, true
		}
	}
	return nil, false
}

// List returns a snapshot of all teammates sorted by Started (oldest
// first). Snapshot copies the pointers — callers MUST treat the
// returned Teammates as read-only.
//
// Fix 2 (2026-05-15) — recentlyFinished teammates are included so
// SubAgentList shows them with their Completed/Failed/Killed status
// instead of disappearing the moment they finish. Live teammates
// first (their Status is still Running), then finished.
func (r *Roster) List() []*Teammate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Teammate, 0, len(r.teammates)+len(r.recentlyFinished))
	for _, t := range r.teammates {
		out = append(out, t)
	}
	for _, t := range r.recentlyFinished {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Started.Before(out[j].Started)
	})
	return out
}

// CancelAll fires every teammate's Cancel func and clears the
// registry. Used during shutdown so orphan sub-agents don't keep
// burning tokens past parent termination.
func (r *Roster) CancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.teammates {
		if t.Cancel != nil {
			t.Cancel()
		}
	}
	r.teammates = make(map[string]*Teammate)
}

// anonName generates an auto-name for sub-agents that didn't pass
// `name`. Format `_anon-<8hex>` keeps it visually distinct from
// user-chosen names (which must start with a letter per the loader)
// while staying compact in /agents output.
func anonName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "_anon-" + hex.EncodeToString(b[:])
}
