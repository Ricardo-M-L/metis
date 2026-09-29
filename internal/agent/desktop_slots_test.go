package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopSubagentSlotsBoundAcrossAcquires(t *testing.T) {
	t.Setenv(desktopSubagentSlotDirEnv, t.TempDir())
	t.Setenv(desktopSubagentSlotsEnv, "1")
	first, err := AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = AcquireDesktopSubagentSlot(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire = %v, want deadline exceeded", err)
	}
	first()
	second, err := AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second()
}

func TestDesktopTeammatePublishesQueuedIdentityBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(desktopSubagentSlotDirEnv, dir)
	t.Setenv(desktopSubagentSlotsEnv, "1")
	t.Setenv(desktopSubagentCapEnv, "1")
	roster := NewRoster(1, 1)
	teammate := &Teammate{Name: "worker"}
	if err := RegisterDesktopTeammate(context.Background(), roster, teammate, false); err != nil {
		t.Fatal(err)
	}
	if teammate.Snapshot().Status != StatusQueued {
		t.Fatalf("registered status = %s, want queued", teammate.Snapshot().Status)
	}
	idle, err := DesktopSchedulerIdle(dir)
	if err != nil || !idle {
		t.Fatalf("unstarted identity occupied execution budget: idle=%v err=%v", idle, err)
	}
	if _, err := AcquireDesktopTeammateExecution(context.Background(), roster, teammate); err != nil {
		t.Fatal(err)
	}
	if teammate.Snapshot().Status != StatusRunning {
		t.Fatalf("admitted status = %s, want running", teammate.Snapshot().Status)
	}
	roster.UnregisterTeammate(teammate)
	idle, err = DesktopSchedulerIdle(dir)
	if err != nil || !idle {
		t.Fatalf("unregistered identity left an execution slot: idle=%v err=%v", idle, err)
	}
}

func TestDesktopSubagentPerRootSharedCapacity(t *testing.T) {
	t.Setenv(desktopSubagentSlotDirEnv, t.TempDir())
	t.Setenv(desktopSubagentSlotsEnv, "8")
	t.Setenv(desktopSubagentCapEnv, "4")
	for i := 0; i < 4; i++ {
		release, err := AcquireDesktopSubagentSlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	release, err := AcquireDesktopSubagentSlot(ctx)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fifth child = %v, want deadline exceeded", err)
	}
}

func TestDesktopTeammateEightRequestsShareFourExecutionPermits(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			t.Setenv(desktopSubagentSlotDirEnv, t.TempDir())
			t.Setenv(desktopSubagentSlotsEnv, "8")
			t.Setenv(desktopSubagentCapEnv, "4")
			roster := NewRoster(20, 40)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			teammates := make([]*Teammate, 8)
			for i := range teammates {
				teammate := &Teammate{Name: fmt.Sprintf("worker-%d", i), Anonymous: mixed && i%2 == 0}
				if err := RegisterDesktopTeammate(ctx, roster, teammate, false); err != nil {
					t.Fatal(err)
				}
				if teammate.Snapshot().Status != StatusQueued {
					t.Fatalf("worker %d was not queued", i)
				}
				teammates[i] = teammate
			}
			admitted := make(chan struct{}, 8)
			results := make(chan error, 8)
			finish := make(chan struct{})
			var active, peak atomic.Int32
			for _, teammate := range teammates {
				go func(teammate *Teammate) {
					_, err := AcquireDesktopTeammateExecution(ctx, roster, teammate)
					if err != nil {
						roster.UnregisterTeammate(teammate)
						results <- err
						return
					}
					n := active.Add(1)
					for old := peak.Load(); n > old; old = peak.Load() {
						if peak.CompareAndSwap(old, n) {
							break
						}
					}
					admitted <- struct{}{}
					select {
					case <-finish:
					case <-ctx.Done():
					}
					active.Add(-1)
					roster.UnregisterTeammate(teammate)
					results <- nil
				}(teammate)
			}
			for i := 0; i < 4; i++ {
				select {
				case <-admitted:
				case <-ctx.Done():
					t.Fatal("four children never acquired execution permits")
				}
			}
			select {
			case <-admitted:
				t.Error("fifth child executed before a permit was released")
			case <-time.After(100 * time.Millisecond):
			}
			close(finish)
			for i := 0; i < len(teammates); i++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if peak.Load() != 4 || roster.Count() != 0 {
				t.Fatalf("peak=%d live=%d", peak.Load(), roster.Count())
			}
		})
	}
}

func TestDesktopQueuedTeammateCancellationClearsTicket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(desktopSubagentSlotDirEnv, dir)
	t.Setenv(desktopSubagentSlotsEnv, "1")
	t.Setenv(desktopSubagentCapEnv, "1")
	roster := NewRoster(1, 1)
	first, second := &Teammate{Name: "first"}, &Teammate{Name: "second"}
	for _, teammate := range []*Teammate{first, second} {
		if err := RegisterDesktopTeammate(context.Background(), roster, teammate, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AcquireDesktopTeammateExecution(context.Background(), roster, first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := AcquireDesktopTeammateExecution(ctx, roster, second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued execution cancellation = %v", err)
	}
	roster.UnregisterTeammate(second)
	roster.UnregisterTeammate(first)
	idle, err := DesktopSchedulerIdle(dir)
	if err != nil || !idle {
		t.Fatalf("cancelled queue retained a ticket: idle=%v err=%v", idle, err)
	}
}

func TestDesktopNestedExecutionCanYieldParentPermit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(desktopSubagentSlotDirEnv, dir)
	t.Setenv(desktopSubagentSlotsEnv, "1")
	t.Setenv(desktopSubagentCapEnv, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	parentCtx, closeParent, err := enterDesktopLoop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeParent()
	roster := NewRoster(1, 1)
	child := &Teammate{Name: "nested"}
	if err := RegisterDesktopTeammate(parentCtx, roster, child, true); err != nil {
		t.Fatal(err)
	}
	if child.Snapshot().Status != StatusQueued {
		t.Fatal("nested creation waited for parent execution permit")
	}
	result := make(chan error, 1)
	go func() {
		_, err := AcquireDesktopTeammateExecution(parentCtx, roster, child)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("child executed before parent yielded: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	resume := YieldDesktopExecution(parentCtx)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	roster.UnregisterTeammate(child)
	if err := resume(parentCtx); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopCLIWithoutSlotDirRetainsRosterCapacity(t *testing.T) {
	t.Setenv(desktopSubagentSlotDirEnv, "")
	roster := NewRoster(1, 1)
	first := &Teammate{Name: "first"}
	if err := RegisterDesktopTeammate(context.Background(), roster, first, false); err != nil {
		t.Fatal(err)
	}
	defer roster.UnregisterTeammate(first)
	if err := RegisterDesktopTeammate(context.Background(), roster, &Teammate{Name: "second"}, false); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("ordinary CLI capacity = %v, want ErrCapacityExceeded", err)
	}
}

func TestDesktopInProcessContextQueuesChildAndPreservesOwner(t *testing.T) {
	t.Setenv(desktopSubagentSlotDirEnv, "")
	dir := t.TempDir()
	config := DesktopExecutionConfig{SlotDir: dir, TotalAgentSlots: 1, SubagentsPerRoot: 1, Owner: "image-session:turn-1"}
	base := WithDesktopExecutionConfig(context.Background(), config)
	parentCtx, closeParent, err := enterDesktopLoop(base)
	if err != nil {
		t.Fatal(err)
	}
	defer closeParent()
	if got := desktopLeaseFromContext(parentCtx).owner; got != config.Owner {
		t.Fatalf("root owner = %q, want %q", got, config.Owner)
	}
	roster := NewRoster(1, 1)
	child := &Teammate{Name: "image-child"}
	// A detached background child retains both the config and the root owner.
	childBase := context.WithoutCancel(parentCtx)
	if got, ok := DesktopExecutionConfigFromContext(childBase); !ok || got != config {
		t.Fatalf("detached context config = %+v, found=%v", got, ok)
	}
	if err := RegisterDesktopTeammate(childBase, roster, child, false); err != nil {
		t.Fatal(err)
	}
	if child.Snapshot().Status != StatusQueued {
		t.Fatalf("context-only registration = %s, want queued", child.Snapshot().Status)
	}
	ctx, cancel := context.WithTimeout(childBase, 2*time.Second)
	defer cancel()
	result := make(chan struct {
		ctx context.Context
		err error
	}, 1)
	go func() {
		childCtx, err := AcquireDesktopTeammateExecution(ctx, roster, child)
		result <- struct {
			ctx context.Context
			err error
		}{childCtx, err}
	}()
	select {
	case got := <-result:
		t.Fatalf("child ran while root held the only slot: %v", got.err)
	case <-time.After(50 * time.Millisecond):
	}
	resume := YieldDesktopExecution(parentCtx)
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if owner := desktopLeaseFromContext(got.ctx).owner; owner != config.Owner {
		t.Fatalf("child owner = %q, want %q", owner, config.Owner)
	}
	roster.UnregisterTeammate(child)
	if err := resume(parentCtx); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopInProcessContextOverridesWorkerEnvironment(t *testing.T) {
	envDir, contextDir := t.TempDir(), t.TempDir()
	t.Setenv(desktopSubagentSlotDirEnv, envDir)
	t.Setenv(desktopSubagentSlotsEnv, "1")
	t.Setenv(desktopSubagentCapEnv, "1")
	config := DesktopExecutionConfig{SlotDir: contextDir, TotalAgentSlots: 2, SubagentsPerRoot: 2, Owner: "embedded-turn"}
	ctx := WithDesktopExecutionConfig(context.Background(), config)
	rootCtx, closeRoot, err := enterDesktopLoop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRoot()
	roster := NewRoster(1, 1)
	child := &Teammate{Name: "child"}
	if err := RegisterDesktopTeammate(rootCtx, roster, child, false); err != nil {
		t.Fatal(err)
	}
	childCtx, err := AcquireDesktopTeammateExecution(rootCtx, roster, child)
	if err != nil {
		t.Fatalf("context total=2 should admit child alongside root: %v", err)
	}
	if owner := desktopLeaseFromContext(childCtx).owner; owner != config.Owner {
		t.Fatalf("child owner = %q", owner)
	}
	if idle, err := DesktopSchedulerIdle(contextDir); err != nil || idle {
		t.Fatalf("context scheduler should be active: idle=%v err=%v", idle, err)
	}
	if idle, err := DesktopSchedulerIdle(envDir); err != nil || !idle {
		t.Fatalf("environment scheduler should be unused: idle=%v err=%v", idle, err)
	}
	roster.UnregisterTeammate(child)
}

func TestDesktopInvalidInProcessConfigDoesNotFallBackToEnvironment(t *testing.T) {
	t.Setenv(desktopSubagentSlotDirEnv, t.TempDir())
	t.Setenv(desktopSubagentSlotsEnv, "2")
	ctx := WithDesktopExecutionConfig(context.Background(), DesktopExecutionConfig{
		TotalAgentSlots: 2, SubagentsPerRoot: 1, Owner: "invalid-image-turn",
	})
	_, closeLease, err := enterDesktopLoop(ctx)
	if closeLease != nil {
		closeLease()
	}
	if err == nil {
		t.Fatal("invalid context config silently fell back to worker environment")
	}
}
