// Package turnprogress carries content-free, observed turn startup progress.
// The session owner assigns request identities and sequence numbers. Worker
// observers only report fixed boundaries and durations on their local clock.
package turnprogress

import (
	"context"
	"sync"
	"time"
)

type Phase string

const (
	PhaseReceived        Phase = "received"
	PhaseSessionLock     Phase = "session_lock"
	PhaseQueuedGlobal    Phase = "queued_global"
	PhaseQueuedWorkspace Phase = "queued_workspace"
	PhaseWorkerStart     Phase = "worker_start"
	PhaseRuntimePrepare  Phase = "runtime_prepare"
	PhaseMCPWait         Phase = "mcp_wait"
	PhaseRequestPrepare  Phase = "request_prepare"
	PhaseRequestSlot     Phase = "request_slot"
	PhaseRateCooldown    Phase = "rate_cooldown"
	PhaseRetryBackoff    Phase = "retry_backoff"
	PhaseRequestWrite    Phase = "request_write"
	PhaseWaitingUpstream Phase = "waiting_upstream"
	PhaseThinking        Phase = "thinking"
	PhaseGenerating      Phase = "generating"
	PhaseTool            Phase = "tool"
	PhasePermission      Phase = "permission"
	PhaseUserInput       Phase = "user_input"
	PhaseFinalizing      Phase = "finalizing"
	PhaseCompleted       Phase = "completed"
	PhaseError           Phase = "error"
	PhaseStopped         Phase = "stopped"
)

func (p Phase) Valid() bool {
	switch p {
	case PhaseReceived, PhaseSessionLock, PhaseQueuedGlobal, PhaseQueuedWorkspace,
		PhaseWorkerStart, PhaseRuntimePrepare, PhaseMCPWait, PhaseRequestPrepare,
		PhaseRequestSlot, PhaseRateCooldown, PhaseRetryBackoff, PhaseRequestWrite,
		PhaseWaitingUpstream, PhaseThinking, PhaseGenerating, PhaseTool,
		PhasePermission, PhaseUserInput, PhaseFinalizing, PhaseCompleted, PhaseError, PhaseStopped:
		return true
	}
	return false
}

func (p Phase) Terminal() bool { return p == PhaseCompleted || p == PhaseError || p == PhaseStopped }

// Milestone disambiguates an observed boundary within a phase. In particular,
// receiving a response byte does not imply receiving reasoning or answer text.
type Milestone string

const (
	MilestoneWorkerStarted  Milestone = "worker_started"
	MilestoneRuntimeReady   Milestone = "runtime_ready"
	MilestoneMCPReady       Milestone = "mcp_ready"
	MilestoneRequestWritten Milestone = "request_written"
	MilestoneFirstByte      Milestone = "first_byte"
	MilestoneFirstThinking  Milestone = "first_thinking"
	MilestoneFirstText      Milestone = "first_text"
)

func (m Milestone) Valid() bool {
	return m == "" || m == MilestoneWorkerStarted || m == MilestoneRuntimeReady || m == MilestoneMCPReady || m == MilestoneRequestWritten || m == MilestoneFirstByte || m == MilestoneFirstThinking || m == MilestoneFirstText
}

// Update is a worker observation, never an authoritative session snapshot.
// ElapsedMS uses the worker observer's monotonic clock. nil means unknown;
// observed zero milliseconds is preserved. No prompt, URL or error is carried.
type Update struct {
	Phase        Phase     `json:"phase"`
	Milestone    Milestone `json:"milestone,omitempty"`
	ElapsedMS    *int64    `json:"elapsedMs,omitempty"`
	ProviderCall int       `json:"providerCall,omitempty"`
	Attempt      int       `json:"attempt,omitempty"`
}

func (u Update) Valid() bool {
	return u.Phase.Valid() && u.Milestone.Valid() && (u.ElapsedMS == nil || *u.ElapsedMS >= 0) && u.ProviderCall >= 0 && u.Attempt >= 0
}

const SourceServerObserved = "server_observed"

// Snapshot is generated only by the parent session owner. ServerElapsedMS and
// PhaseElapsedMS use that parent's monotonic clock. ElapsedMS, when supplied,
// remains a separate worker-local observation and must not be subtracted from
// a server duration. Sequence is monotonically increasing per RequestID.
type Snapshot struct {
	SessionID       string `json:"sessionId,omitempty"`
	RequestID       string `json:"requestId"`
	Sequence        uint64 `json:"sequence"`
	Phase           Phase  `json:"phase"`
	ServerElapsedMS *int64 `json:"serverElapsedMs"`
	PhaseElapsedMS  *int64 `json:"phaseElapsedMs"`
	ElapsedMS       *int64 `json:"elapsedMs,omitempty"`
	Source          string `json:"source"`
	Terminal        bool   `json:"terminal,omitempty"`
}

type Observer func(Update)
type observerKey struct{}
type observerState struct {
	mu       sync.Mutex
	observer Observer
	started  time.Time
	last     Update
	hasLast  bool
}

// WithObserver starts an independent local observation clock. Callbacks must
// only enqueue or update local state: persistence and IPC flushing belong to
// their owner, never the decoded delta path. Callback panics are isolated.
func WithObserver(ctx context.Context, observer Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, &observerState{observer: observer, started: time.Now()})
}

func observerFrom(ctx context.Context) *observerState {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(observerKey{}).(*observerState)
	return s
}

func HasObserver(ctx context.Context) bool {
	s := observerFrom(ctx)
	return s != nil && s.observer != nil
}

func Emit(ctx context.Context, phase Phase) { Observe(ctx, Update{Phase: phase}) }

// Observe accepts only fixed labels and nonnegative numbers. Consecutive
// identical phase/call/attempt/milestone boundaries are coalesced. The callback
// receives its own elapsed value so retaining an Update never shares pointers.
func Observe(ctx context.Context, update Update) {
	s := observerFrom(ctx)
	if s == nil || s.observer == nil || !update.Valid() {
		return
	}
	s.mu.Lock()
	if s.hasLast && s.last.Phase == update.Phase && s.last.Milestone == update.Milestone && s.last.ProviderCall == update.ProviderCall && s.last.Attempt == update.Attempt {
		s.mu.Unlock()
		return
	}
	if update.ElapsedMS == nil {
		elapsed := time.Since(s.started).Milliseconds()
		update.ElapsedMS = &elapsed
	} else {
		elapsed := *update.ElapsedMS
		update.ElapsedMS = &elapsed
	}
	s.last, s.hasLast = update, true
	s.mu.Unlock()
	defer func() { _ = recover() }()
	s.observer(update)
}
