package webui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTurnCoordinatorRunsDistinctWorkspacesInParallel(t *testing.T) {
	c := NewTurnCoordinator(2)
	first, err := c.Acquire(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	ready := make(chan *TurnLease, 1)
	errs := make(chan error, 1)
	go func() {
		lease, err := c.Acquire(context.Background(), t.TempDir())
		if err != nil {
			errs <- err
			return
		}
		ready <- lease
	}()
	select {
	case err := <-errs:
		t.Fatal(err)
	case second := <-ready:
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("independent workspace was queued behind a running turn")
	}
}

func TestTurnCoordinatorResizeAdmitsQueuedWorkspace(t *testing.T) {
	c := NewTurnCoordinator(1)
	first, err := c.Acquire(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	ready := make(chan *TurnLease, 1)
	errs := make(chan error, 1)
	go func() {
		lease, err := c.Acquire(context.Background(), t.TempDir())
		if err != nil {
			errs <- err
			return
		}
		ready <- lease
	}()
	select {
	case lease := <-ready:
		lease.Release()
		t.Fatal("queued workspace ran before the scheduler was resized")
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(50 * time.Millisecond):
	}

	c.SetMaxParallel(2)
	if got := c.MaxParallel(); got != 2 {
		t.Fatalf("max parallel = %d, want 2", got)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case lease := <-ready:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("queued workspace did not start after resize")
	}
}

func TestTurnCoordinatorResizeWhenIdleKeepsRunningBudget(t *testing.T) {
	c := NewTurnCoordinator(2)
	first, err := c.Acquire(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	applied := false
	if c.ResizeWhenIdle(4, func() { applied = true }) {
		t.Fatal("resize should defer while a workspace lease is active")
	}
	if applied || c.MaxParallel() != 2 {
		t.Fatalf("running resize changed scheduler: applied=%v max=%d", applied, c.MaxParallel())
	}
	first.Release()
	if !c.ResizeWhenIdle(4, func() { applied = true }) {
		t.Fatal("resize should apply after the active lease releases")
	}
	if !applied || c.MaxParallel() != 4 {
		t.Fatalf("idle resize did not apply: applied=%v max=%d", applied, c.MaxParallel())
	}
}

func TestTurnCoordinatorSerializesSameWorkspace(t *testing.T) {
	c := NewTurnCoordinator(6)
	workspace := t.TempDir()
	first, err := c.Acquire(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	ready := make(chan *TurnLease, 1)
	go func() {
		lease, err := c.Acquire(context.Background(), workspace)
		if err == nil {
			ready <- lease
		}
	}()
	select {
	case unexpected := <-ready:
		unexpected.Release()
		t.Fatal("same workspace received a second writer lease")
	case <-time.After(50 * time.Millisecond):
	}
	first.Release()
	select {
	case second := <-ready:
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("waiting same-workspace turn was not admitted after release")
	}
}

func TestTurnCoordinatorDoesNotHeadOfLineBlockOtherWorkspaces(t *testing.T) {
	c := NewTurnCoordinator(2)
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	first, err := c.Acquire(context.Background(), workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	var group sync.WaitGroup
	group.Add(1)
	blocked := make(chan struct{})
	go func() {
		defer group.Done()
		lease, err := c.Acquire(context.Background(), workspaceA)
		if err == nil {
			lease.Release()
		}
		close(blocked)
	}()
	time.Sleep(20 * time.Millisecond)
	leaseB, err := c.Acquire(context.Background(), workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	leaseB.Release()
	select {
	case <-blocked:
		t.Fatal("same-workspace waiter ran before original lease released")
	default:
	}
	first.Release()
	group.Wait()
}

func TestTurnCoordinatorCancellationRemovesWaiter(t *testing.T) {
	c := NewTurnCoordinator(1)
	workspace := t.TempDir()
	first, err := c.Acquire(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Acquire(ctx, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire error = %v, want deadline exceeded", err)
	}
	first.Release()
	lease, err := c.Acquire(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}
