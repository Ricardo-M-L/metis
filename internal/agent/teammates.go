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

// Roster is the live-sub-agent registry. Thread-safe.
type Roster struct {
	mu        sync.RWMutex
	teammates map[string]*Teammate
	capacity  int
}

// NewRoster creates a Roster with the given concurrency cap.
// capacity <= 0 disables the cap (effectively unlimited). The default
// for production is config.Agents.MaxConcurrentSubAgents (5).
func NewRoster(capacity int) *Roster {
	return &Roster{
		teammates: make(map[string]*Teammate),
		capacity:  capacity,
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
// the capacity cap is hit; ErrNameInUse when t.Name is already taken.
// If t.Name is empty, an anonymous name is auto-generated and t.Name
// is overwritten in place.
func (r *Roster) Register(t *Teammate) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.capacity > 0 && len(r.teammates) >= r.capacity {
		return ErrCapacityExceeded
	}

	if t.Name == "" {
		t.Name = anonName()
		t.Anonymous = true
	}
	if _, ok := r.teammates[t.Name]; ok {
		if t.Anonymous {
			// Vanishingly rare 64-bit collision; regenerate once.
			t.Name = anonName()
		} else {
			return fmt.Errorf("%w: %q", ErrNameInUse, t.Name)
		}
	}
	if t.Mailbox == nil {
		t.Mailbox = make(chan PeerMessage, 16)
	}
	if t.Started.IsZero() {
		t.Started = time.Now()
	}
	r.teammates[t.Name] = t
	return nil
}

// Unregister removes a teammate by name. Safe to call even when the
// name isn't present (e.g., a Register that returned an error leaves
// no entry, but the caller still defers Unregister).
func (r *Roster) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.teammates, name)
}

// Count returns the number of currently-registered teammates.
// Used by Agent.Execute to check the cap before kicking off a new
// sub-agent. Cheap read-lock.
func (r *Roster) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.teammates)
}

// Capacity returns the configured cap (0 = unlimited).
func (r *Roster) Capacity() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capacity
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
func (r *Roster) LookupByAgentID(id string) (*Teammate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.teammates {
		if t.AgentID == id {
			return t, true
		}
	}
	return nil, false
}

// List returns a snapshot of all teammates sorted by Started (oldest
// first). Snapshot copies the pointers — callers MUST treat the
// returned Teammates as read-only.
func (r *Roster) List() []*Teammate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Teammate, 0, len(r.teammates))
	for _, t := range r.teammates {
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
