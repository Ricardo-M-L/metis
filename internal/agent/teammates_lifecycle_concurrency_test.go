package agent

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// manualDeadlineContext lets the test expire a strict join only after the
// second caller has deterministically announced itself as a queued waiter.
// A wall-clock timeout cannot prove the A -> B admission hand-off ordering.
type manualDeadlineContext struct {
	done chan struct{}
	once sync.Once
}

func newManualDeadlineContext() *manualDeadlineContext {
	return &manualDeadlineContext{done: make(chan struct{})}
}

func (*manualDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *manualDeadlineContext) Done() <-chan struct{}     { return c.done }
func (c *manualDeadlineContext) Value(any) any             { return nil }
func (c *manualDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *manualDeadlineContext) expire() { c.once.Do(func() { close(c.done) }) }

func TestRosterConcurrentStrictJoinKeepsAdmissionClosedAfterOwnerTimeout(t *testing.T) {
	roster := NewRoster(0)
	cancelled := make(chan struct{})
	releaseRunner := make(chan struct{})
	var cancelOnce sync.Once
	teammate := &Teammate{
		Name: "old-generation",
		Cancel: func() {
			cancelOnce.Do(func() { close(cancelled) })
		},
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-cancelled
		<-releaseRunner
		roster.UnregisterTeammate(teammate)
	}()

	firstCtx := newManualDeadlineContext()
	firstDone := make(chan error, 1)
	go func() { firstDone <- roster.CancelAndWait(firstCtx) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("first strict join did not cancel the old runner")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- roster.CancelAndWait(context.Background()) }()
	waitForStrictRosterWaiters(t, roster, 2)
	firstCtx.expire()
	if err := <-firstDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first CancelAndWait error = %v, want deadline exceeded", err)
	}

	// The second caller was queued before the first timed out. Admission must
	// remain closed across that ownership hand-off, and legacy non-blocking
	// reset APIs must not erase the handle the second caller still has to join.
	if err := roster.Register(&Teammate{Name: "late"}); !errors.Is(err, ErrRosterResetting) {
		t.Fatalf("Register between strict owners = %v, want %v", err, ErrRosterResetting)
	}
	roster.Reset()
	roster.CancelAll()
	if got := roster.Count(); got != 1 {
		t.Fatalf("non-blocking reset erased in-flight strict generation: count=%d, want 1", got)
	}
	select {
	case err := <-secondDone:
		t.Fatalf("second CancelAndWait returned before runner exit: %v", err)
	default:
	}

	close(releaseRunner)
	if err := <-secondDone; err != nil {
		t.Fatalf("second CancelAndWait: %v", err)
	}
	if got := roster.Count(); got != 0 {
		t.Fatalf("joined roster count = %d, want 0", got)
	}
	if err := roster.Register(&Teammate{Name: "next-generation"}); err != nil {
		t.Fatalf("Register after last strict waiter completed: %v", err)
	}
}

func TestRosterQueuedStrictJoinHonorsOwnDeadline(t *testing.T) {
	roster := NewRoster(0)
	ownerCancelled := make(chan struct{})
	releaseOwner := make(chan struct{})
	var cancelOnce sync.Once
	teammate := &Teammate{
		Name: "blocked-owner",
		Cancel: func() {
			cancelOnce.Do(func() { close(ownerCancelled) })
		},
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-ownerCancelled
		<-releaseOwner
		roster.UnregisterTeammate(teammate)
	}()

	ownerDone := make(chan error, 1)
	go func() { ownerDone <- roster.CancelAndWait(context.Background()) }()
	select {
	case <-ownerCancelled:
	case <-time.After(time.Second):
		t.Fatal("owner did not acquire strict lifecycle token")
	}

	waiterCtx := newManualDeadlineContext()
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- roster.CancelAndWait(waiterCtx) }()
	waitForStrictRosterWaiters(t, roster, 2)
	waiterCtx.expire()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued CancelAndWait error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued CancelAndWait ignored its deadline behind the owner")
	}

	// The owner is still active, so the canceled waiter must not reopen
	// admission when it removes only its own strict intent.
	if err := roster.Register(&Teammate{Name: "late"}); !errors.Is(err, ErrRosterResetting) {
		t.Fatalf("Register while owner remains active = %v, want %v", err, ErrRosterResetting)
	}
	close(releaseOwner)
	if err := <-ownerDone; err != nil {
		t.Fatalf("owner CancelAndWait: %v", err)
	}
}

func TestRosterStrictJoinChecksCanceledContextAfterTokenAcquire(t *testing.T) {
	roster := NewRoster(0)
	var cancelCalls atomic.Int32
	teammate := &Teammate{Name: "preserved", Cancel: func() { cancelCalls.Add(1) }}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}

	ctx := newManualDeadlineContext()
	ctx.expire()
	if err := roster.CancelAndWait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CancelAndWait error = %v, want deadline exceeded", err)
	}
	if got := cancelCalls.Load(); got != 0 {
		t.Fatalf("already-canceled strict caller signaled generation %d times", got)
	}
	if err := roster.Register(&Teammate{Name: "admission-restored"}); err != nil {
		t.Fatalf("canceled strict caller leaked admission barrier: %v", err)
	}

	// Prove the ownership token was returned on either select branch.
	roster.UnregisterTeammate(teammate)
	if next, ok := roster.Lookup("admission-restored"); ok {
		roster.UnregisterTeammate(next)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := roster.CancelAndWait(cleanupCtx); err != nil {
		t.Fatalf("next strict owner could not acquire returned token: %v", err)
	}
}

func TestRosterCancelAndWaitLatchesRequestUntilCancelIsPublished(t *testing.T) {
	roster := NewRoster(0)
	teammate := &Teammate{Name: "constructing-runner"}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}

	joinDone := make(chan error, 1)
	go func() { joinDone <- roster.CancelAndWait(context.Background()) }()
	waitForStrictRosterWaiters(t, roster, 1)
	select {
	case err := <-joinDone:
		t.Fatalf("strict join returned before the constructing runner published cancellation: %v", err)
	default:
	}

	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	teammate.SetCancel(func() {
		cancelOnce.Do(func() { close(cancelled) })
	})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("latched lifecycle cancellation did not fire when SetCancel published")
	}
	roster.UnregisterTeammate(teammate)
	if err := <-joinDone; err != nil {
		t.Fatalf("CancelAndWait: %v", err)
	}
}

func TestTeammateSetCancelAndRequestCancelAreRaceSafe(t *testing.T) {
	for i := 0; i < 100; i++ {
		teammate := &Teammate{}
		start := make(chan struct{})
		var calls atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			teammate.RequestCancel()
		}()
		go func() {
			defer wg.Done()
			<-start
			teammate.SetCancel(func() { calls.Add(1) })
		}()
		close(start)
		wg.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("iteration %d cancellation calls = %d, want exactly 1", i, got)
		}
	}
}

func TestRosterStrictJoinIncludesRunnerRetiredByConcurrentReset(t *testing.T) {
	roster := NewRoster(0)
	resetClearedLive := make(chan struct{})
	allowResetReturn := make(chan struct{})
	releaseRunner := make(chan struct{})
	var firstCancel sync.Once
	teammate := &Teammate{
		Name: "retired-before-strict",
		Cancel: func() {
			firstCancel.Do(func() {
				// cancelAndMaybeForget clears the public map before signaling.
				close(resetClearedLive)
				<-allowResetReturn
			})
		},
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-releaseRunner
		roster.UnregisterTeammate(teammate)
	}()

	resetDone := make(chan struct{})
	go func() {
		roster.Reset()
		close(resetDone)
	}()
	select {
	case <-resetClearedLive:
	case <-time.After(time.Second):
		t.Fatal("Reset did not reach its post-clear cancellation edge")
	}
	if got := roster.Count(); got != 0 {
		t.Fatalf("legacy Reset live count = %d, want 0", got)
	}

	strictDone := make(chan error, 1)
	go func() { strictDone <- roster.CancelAndWait(context.Background()) }()
	waitForStrictRosterWaiters(t, roster, 1)
	close(allowResetReturn)
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("Reset did not release lifecycle ownership")
	}
	select {
	case err := <-strictDone:
		t.Fatalf("strict join forgot retired runner: %v", err)
	default:
	}

	close(releaseRunner)
	if err := <-strictDone; err != nil {
		t.Fatalf("CancelAndWait: %v", err)
	}
	roster.mu.RLock()
	draining := len(roster.draining)
	roster.mu.RUnlock()
	if draining != 0 {
		t.Fatalf("joined roster retained %d retired runners", draining)
	}
}

func waitForStrictRosterWaiters(t *testing.T, roster *Roster, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		roster.mu.RLock()
		got := roster.strictResetters
		roster.mu.RUnlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	roster.mu.RLock()
	got := roster.strictResetters
	roster.mu.RUnlock()
	t.Fatalf("strict waiter count = %d, want %d", got, want)
}
