package builtin

import (
	"context"
	"sync"
	"time"

	toolapi "github.com/Ricardo-M-L/metis/internal/tools"
)

// A permission binding belongs to one dispatcher-generated invocation ID and
// remains valid until that exact invocation consumes it. Wall-clock expiry is
// unsafe here: batch dispatch records every CanUse result before presenting
// sequential ASK dialogs, so a legitimate later approval can easily outlive a
// short TTL. Denied/cancelled calls leave unreachable IDs behind; cap those
// records instead of expiring active approvals.
const maxPendingInvocationBindings = 4096

type timedInvocationBinding[T any] struct {
	value   T
	created time.Time
}

// invocationAuthorizer is a one-shot, per-call hand-off from CanUse to
// Execute. Bindings are never selected by path or FIFO order: only the exact
// dispatcher-generated invocation ID can consume them, and consumption
// deletes the entry atomically.
type invocationAuthorizer[T any] struct {
	mu      sync.Mutex
	pending map[string]timedInvocationBinding[T]
}

func newInvocationAuthorizer[T any]() *invocationAuthorizer[T] {
	return &invocationAuthorizer[T]{pending: make(map[string]timedInvocationBinding[T])}
}

func (a *invocationAuthorizer[T]) record(ctx context.Context, value T) {
	if a == nil {
		return
	}
	id := toolapi.InvocationIDFromContext(ctx)
	if id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.pending[id]; !exists && len(a.pending) >= maxPendingInvocationBindings {
		a.evictOldestLocked()
	}
	a.pending[id] = timedInvocationBinding[T]{value: value, created: time.Now()}
}

// consume returns (value, hasInvocationID, found). hasInvocationID lets the
// caller distinguish direct legacy Execute (which may perform an immediate
// gate recheck) from a dispatched Execute whose missing binding must fail
// closed rather than borrowing another call's approval.
func (a *invocationAuthorizer[T]) consume(ctx context.Context) (value T, hasInvocationID, found bool) {
	id := toolapi.InvocationIDFromContext(ctx)
	if id == "" {
		return value, false, false
	}
	if a == nil {
		return value, true, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	binding, ok := a.pending[id]
	if !ok {
		return value, true, false
	}
	delete(a.pending, id)
	return binding.value, true, true
}

func (a *invocationAuthorizer[T]) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, binding := range a.pending {
		if oldestID == "" || binding.created.Before(oldest) || (binding.created.Equal(oldest) && id < oldestID) {
			oldestID = id
			oldest = binding.created
		}
	}
	if oldestID != "" {
		delete(a.pending, oldestID)
	}
}
