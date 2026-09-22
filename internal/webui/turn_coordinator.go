package webui

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
)

// TurnCoordinator admits top-level Desktop turns without letting unrelated
// sessions share a mutable runtime or two agents write the same checkout at
// once.  A lease is held for the whole turn, including model/tool work.
//
// The coordinator deliberately owns only admission.  Runtime isolation,
// cancellation and persistence remain the responsibility of the turn runner.
// Keeping this small makes the invariant testable independently from Wails,
// SSE and providers.
type TurnCoordinator struct {
	mu          sync.Mutex
	maxParallel int
	running     int
	workspaces  map[string]struct{}
	waiters     []*turnWaiter
	closed      bool
}

type turnWaiter struct {
	workspace string
	ready     chan struct{}
	granted   bool
}

// TurnLease is the exclusive ownership token returned by Acquire. Release is
// idempotent so cancellation and normal completion can safely race in callers.
type TurnLease struct {
	coordinator *TurnCoordinator
	workspace   string
	once        sync.Once
}

// NewTurnCoordinator creates a bounded scheduler. Desktop's high-performance
// profile uses six root turns; callers may supply a lower bound for tests or a
// constrained host.
func NewTurnCoordinator(maxParallel int) *TurnCoordinator {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	return &TurnCoordinator{
		maxParallel: maxParallel,
		workspaces:  make(map[string]struct{}),
	}
}

// Acquire waits until both a global slot and an exclusive workspace writer
// lease are available. Queued turns from other workspaces are allowed to pass
// a blocked same-workspace turn, so one busy repository cannot idle the whole
// Desktop.
func (c *TurnCoordinator) Acquire(ctx context.Context, workspace string) (*TurnLease, error) {
	if c == nil {
		return nil, errors.New("turn coordinator is unavailable")
	}
	workspace = canonicalTurnWorkspace(workspace)
	w := &turnWaiter{workspace: workspace, ready: make(chan struct{})}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("turn coordinator is closed")
	}
	c.waiters = append(c.waiters, w)
	c.dispatchLocked()
	c.mu.Unlock()

	select {
	case <-w.ready:
		c.mu.Lock()
		granted := w.granted
		c.mu.Unlock()
		if !granted {
			return nil, errors.New("turn coordinator is closed")
		}
		return &TurnLease{coordinator: c, workspace: workspace}, nil
	case <-ctx.Done():
		c.mu.Lock()
		if !w.granted {
			c.removeWaiterLocked(w)
			c.dispatchLocked()
			c.mu.Unlock()
			return nil, ctx.Err()
		}
		// A lease can be granted at the same instant as caller cancellation.
		// Return and immediately release it so accounting never leaks a slot.
		c.mu.Unlock()
		lease := &TurnLease{coordinator: c, workspace: workspace}
		lease.Release()
		return nil, ctx.Err()
	}
}

func (l *TurnLease) Release() {
	if l == nil || l.coordinator == nil {
		return
	}
	l.once.Do(func() {
		c := l.coordinator
		c.mu.Lock()
		if c.running > 0 {
			c.running--
		}
		delete(c.workspaces, l.workspace)
		c.dispatchLocked()
		c.mu.Unlock()
	})
}

// Close wakes queued callers. Active leases remain valid and release normally.
func (c *TurnCoordinator) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, w := range c.waiters {
		close(w.ready)
	}
	c.waiters = nil
	c.mu.Unlock()
}

func (c *TurnCoordinator) dispatchLocked() {
	if c.closed {
		return
	}
	for c.running < c.maxParallel {
		index := -1
		for i, w := range c.waiters {
			if _, busy := c.workspaces[w.workspace]; !busy {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		w := c.waiters[index]
		copy(c.waiters[index:], c.waiters[index+1:])
		c.waiters[len(c.waiters)-1] = nil
		c.waiters = c.waiters[:len(c.waiters)-1]
		c.running++
		c.workspaces[w.workspace] = struct{}{}
		w.granted = true
		close(w.ready)
	}
}

func (c *TurnCoordinator) removeWaiterLocked(target *turnWaiter) {
	for i, w := range c.waiters {
		if w != target {
			continue
		}
		copy(c.waiters[i:], c.waiters[i+1:])
		c.waiters[len(c.waiters)-1] = nil
		c.waiters = c.waiters[:len(c.waiters)-1]
		return
	}
}

func canonicalTurnWorkspace(workspace string) string {
	if workspace == "" {
		return "<unknown-workspace>"
	}
	if absolute, err := filepath.Abs(workspace); err == nil {
		workspace = absolute
	}
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	return filepath.Clean(workspace)
}
