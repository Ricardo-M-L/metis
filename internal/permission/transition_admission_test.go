package permission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestResetSessionStateAndWaitJoinsInFlightListenerDrain(t *testing.T) {
	g := New(ModeFullAccess)
	planEntered := make(chan struct{})
	releasePlan := make(chan struct{})
	g.SetModeChangeListener(func(mode Mode) {
		if mode == ModePlan {
			close(planEntered)
			<-releasePlan
		}
	})

	planDone := make(chan struct{})
	go func() {
		g.SetMode(ModePlan)
		close(planDone)
	}()
	select {
	case <-planEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("plan listener did not start")
	}

	resetDone := make(chan struct{})
	go func() {
		g.ResetSessionStateAndWait(ModeBypassPermissions, nil, nil, nil)
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("waiting reset returned before the in-flight listener drain settled")
	case <-time.After(50 * time.Millisecond):
	}

	close(releasePlan)
	select {
	case <-planDone:
	case <-time.After(2 * time.Second):
		t.Fatal("plan mode change did not finish")
	}
	select {
	case <-resetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting reset did not finish after listener drain settled")
	}
	if got := g.Mode(); got != ModeBypassPermissions {
		t.Fatalf("mode after reset = %q, want %q", got, ModeBypassPermissions)
	}
}

func TestResetSessionStateAndWaitAllowsListenerReentrantSetMode(t *testing.T) {
	g := New(ModeDefault)
	var once sync.Once
	g.SetModeChangeListener(func(mode Mode) {
		if mode == ModeFullAccess {
			once.Do(func() { g.SetMode(ModeBypassPermissions) })
		}
	})

	done := make(chan struct{})
	go func() {
		g.ResetSessionStateAndWait(ModeFullAccess, nil, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting reset deadlocked on listener-reentrant SetMode")
	}
	if got := g.Mode(); got != ModeBypassPermissions {
		t.Fatalf("listener-reentrant final mode = %q, want %q", got, ModeBypassPermissions)
	}
}

func TestResetSessionStateAndWaitFinalizesLineageBeforeQueuedSnapshot(t *testing.T) {
	g := New(ModeDefault)
	prePlan := ""
	g.SetModeChangeListener(func(mode Mode) {
		if mode == ModePlan {
			g.SetMode(ModeDontAsk)
		}
	})

	afterSettledEntered := make(chan struct{})
	releaseAfterSettled := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		g.ResetSessionStateAndWait(ModePlan, nil, func() {
			prePlan = string(ModeDefault)
		}, func() {
			close(afterSettledEntered)
			<-releaseAfterSettled
			if g.Mode() != ModePlan {
				prePlan = ""
			}
		})
		close(resetDone)
	}()
	select {
	case <-afterSettledEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("session reset did not reach its after-settled finalizer")
	}
	if got := g.Mode(); got != ModeDontAsk {
		t.Fatalf("listener downgrade mode = %q, want %q", got, ModeDontAsk)
	}

	type snapshot struct {
		mode    Mode
		prePlan string
	}
	snapshotDone := make(chan snapshot, 1)
	go func() {
		var got snapshot
		_ = g.RunModeTransition(func() error {
			got = snapshot{mode: g.Mode(), prePlan: prePlan}
			return nil
		})
		snapshotDone <- got
	}()
	deadline := time.After(2 * time.Second)
	for g.modeTransitionWriters.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("snapshot writer did not queue behind session reset")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	close(releaseAfterSettled)
	select {
	case <-resetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session reset did not finish")
	}
	select {
	case got := <-snapshotDone:
		if got.mode != ModeDontAsk || got.prePlan != "" {
			t.Fatalf("queued snapshot observed torn state: mode=%q prePlan=%q", got.mode, got.prePlan)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued snapshot did not finish")
	}
}

func TestResetSessionStateAndWaitSerializesWithRuntimeTransition(t *testing.T) {
	g := New(ModeFullAccess)
	transitionEntered := make(chan struct{})
	releaseTransition := make(chan struct{})
	transitionDone := make(chan struct{})
	go func() {
		_ = g.RunModeTransition(func() error {
			close(transitionEntered)
			<-releaseTransition
			g.SetModeAndWait(ModePlan)
			return nil
		})
		close(transitionDone)
	}()
	select {
	case <-transitionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime transition did not start")
	}

	resetDone := make(chan struct{})
	go func() {
		g.ResetSessionStateAndWait(ModeDefault, nil, nil, nil)
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("session reset bypassed the in-flight runtime transition")
	case <-time.After(50 * time.Millisecond):
	}
	if got := g.Mode(); got != ModeFullAccess {
		t.Fatalf("waiting reset mutated mode early: %q", got)
	}

	close(releaseTransition)
	select {
	case <-transitionDone:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime transition did not finish")
	}
	select {
	case <-resetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("serialized session reset did not finish")
	}
	if got := g.Mode(); got != ModeDefault {
		t.Fatalf("final reset mode = %q, want %q", got, ModeDefault)
	}
}

func TestToolDispatchAdmissionRejectsTransitionAndInvalidatesOldEpoch(t *testing.T) {
	g := New(ModeFullAccess)
	epoch, allowed, reason := g.ToolDispatchAdmission()
	if !allowed || reason != "" {
		t.Fatalf("initial admission = (%d, %v, %q), want allowed", epoch, allowed, reason)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	g.SetModeChangeListener(func(mode Mode) {
		if mode == ModeDefault {
			close(entered)
			<-release
		}
	})
	done := make(chan struct{})
	go func() {
		g.SetMode(ModeDefault)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("mode listener did not start")
	}

	duringEpoch, allowed, reason := g.ToolDispatchAdmission()
	if allowed || reason != "mode:transition" {
		t.Fatalf("admission during listener = (%d, %v, %q), want transition denial", duringEpoch, allowed, reason)
	}
	if decision, source := g.Check(context.Background(), "Bash", "echo ok"); decision != DecisionDeny || source != "mode:transition" {
		t.Fatalf("Gate.Check during listener = %v (%q), want transition denial", decision, source)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mode transition did not settle")
	}
	afterEpoch, allowed, reason := g.ToolDispatchAdmission()
	if !allowed || reason != "" {
		t.Fatalf("admission after listener = (%d, %v, %q), want allowed", afterEpoch, allowed, reason)
	}
	if afterEpoch == epoch {
		t.Fatalf("mode transition did not invalidate old dispatch epoch %d", epoch)
	}
}

func TestModeListenerPanicFailsClosedUntilSuccessfulTransition(t *testing.T) {
	g := New(ModeDefault)
	g.SetModeChangeListener(func(Mode) { panic("listener exploded") })

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("SetMode did not propagate listener panic")
			}
		}()
		g.SetMode(ModePlan)
	}()

	if _, allowed, reason := g.ToolDispatchAdmission(); allowed || reason != "mode:transition" {
		t.Fatalf("admission after listener panic = allowed=%v reason=%q, want recoverable fail-closed", allowed, reason)
	}
	if decision, source := g.Check(context.Background(), "Read", "/tmp/value"); decision != DecisionDeny || source != "mode:transition" {
		t.Fatalf("Gate.Check after listener panic = %v (%q), want transition denial", decision, source)
	}

	observed := make(chan Mode, 1)
	g.SetModeChangeListener(func(mode Mode) { observed <- mode })
	done := make(chan struct{})
	go func() {
		g.SetModeAndWait(ModeDontAsk)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SetModeAndWait remained blocked after prior listener panic")
	}
	select {
	case got := <-observed:
		if got != ModeDontAsk {
			t.Fatalf("replacement listener observed %q, want %q", got, ModeDontAsk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement listener was not invoked")
	}
	if _, allowed, reason := g.ToolDispatchAdmission(); !allowed || reason != "" {
		t.Fatalf("successful replacement transition did not restore admission: allowed=%v reason=%q", allowed, reason)
	}
}

func TestRunModeTransitionFailurePathsReleaseCoordinator(t *testing.T) {
	g := New(ModeDefault)
	wantErr := errors.New("transition failed")
	if got := g.RunModeTransition(func() error { return wantErr }); !errors.Is(got, wantErr) {
		t.Fatalf("RunModeTransition error = %v, want %v", got, wantErr)
	}
	if release, allowed, reason := g.TryAcquireToolDispatchLease(); !allowed || reason != "" {
		t.Fatalf("error path leaked coordinator: allowed=%v reason=%q", allowed, reason)
	} else {
		release()
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("RunModeTransition did not propagate callback panic")
			}
		}()
		_ = g.RunModeTransition(func() error { panic("transition panic") })
	}()
	if release, allowed, reason := g.TryAcquireToolDispatchLease(); !allowed || reason != "" {
		t.Fatalf("panic path leaked coordinator: allowed=%v reason=%q", allowed, reason)
	} else {
		release()
	}
}

func TestQueuedModeTransitionWritersPreventReaderBarge(t *testing.T) {
	g := New(ModeDefault)
	releaseReader, allowed, reason := g.TryAcquireToolDispatchLease()
	if !allowed {
		t.Fatalf("initial reader denied: %q", reason)
	}

	const writers = 2
	done := make(chan struct{}, writers)
	for range writers {
		go func() {
			_ = g.RunModeTransition(nil)
			done <- struct{}{}
		}()
	}
	deadline := time.After(2 * time.Second)
	for g.modeTransitionWriters.Load() < writers {
		select {
		case <-deadline:
			t.Fatalf("queued writers = %d, want %d", g.modeTransitionWriters.Load(), writers)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if release, allowed, reason := g.TryAcquireToolDispatchLease(); allowed || reason != "mode:transition" {
		if release != nil {
			release()
		}
		t.Fatalf("reader barged ahead of queued writers: allowed=%v reason=%q", allowed, reason)
	}

	releaseReader()
	for range writers {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("queued transition writer did not finish")
		}
	}
	if release, allowed, reason := g.TryAcquireToolDispatchLease(); !allowed || reason != "" {
		t.Fatalf("coordinator remained closed after queued writers: allowed=%v reason=%q", allowed, reason)
	} else {
		release()
	}
}
