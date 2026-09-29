package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestRosterQueuedIdentityDoesNotConsumeCLIKindAdmission(t *testing.T) {
	roster := NewRoster(1, 1)
	for i := 0; i < 3; i++ {
		teammate := &Teammate{Name: fmt.Sprintf("worker-%d", i)}
		if err := roster.RegisterQueued(teammate); err != nil {
			t.Fatal(err)
		}
		if teammate.Snapshot().Status != StatusQueued {
			t.Fatal("queued registration started execution")
		}
		if err := roster.TryStartQueued(teammate, nil); err != nil {
			t.Fatalf("Desktop scheduler admission incorrectly uses CLI kind cap: %v", err)
		}
	}
	roster.CancelAll()
}

func TestRosterQueuedIdentityCancellationRejectsLateLease(t *testing.T) {
	roster := NewRoster(1)
	teammate := &Teammate{Name: "waiting"}
	if err := roster.RegisterQueued(teammate); err != nil {
		t.Fatal(err)
	}
	teammate.RequestCancel()
	var releases atomic.Int32
	if err := roster.TryStartQueued(teammate, func() { releases.Add(1) }); !errors.Is(err, context.Canceled) {
		t.Fatalf("start cancelled queue = %v", err)
	}
	teammate.Finish(StatusKilled, "", context.Canceled, "cancelled")
	roster.UnregisterTeammate(teammate)
	if releases.Load() != 0 {
		t.Fatal("failed admission took ownership of caller's lease")
	}
	// Even a late publisher cannot strand a lease after done has closed.
	teammate.SetResourceRelease(func() { releases.Add(1) })
	if releases.Load() != 1 {
		t.Fatal("late lease was not returned")
	}
}

func TestRosterQueuedExecutionReleaseExactlyOnce(t *testing.T) {
	roster := NewRoster(1)
	teammate := &Teammate{Name: "worker"}
	if err := roster.RegisterQueued(teammate); err != nil {
		t.Fatal(err)
	}
	var releases atomic.Int32
	if err := roster.TryStartQueued(teammate, func() { releases.Add(1) }); err != nil {
		t.Fatal(err)
	}
	teammate.Finish(StatusCompleted, "done", nil, "")
	roster.UnregisterTeammate(teammate)
	roster.UnregisterTeammate(teammate)
	if releases.Load() != 1 {
		t.Fatalf("lease released %d times", releases.Load())
	}
	if err := roster.TryStartQueued(teammate, nil); !errors.Is(err, ErrRosterResetting) {
		t.Fatalf("retired queue restarted: %v", err)
	}
}

func TestRosterQueuedIdentityBound(t *testing.T) {
	roster := NewRoster(1)
	for i := 0; i < DesktopQueuedIdentityLimit; i++ {
		if err := roster.RegisterQueued(&Teammate{Name: fmt.Sprintf("worker-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := roster.RegisterQueued(&Teammate{Name: "overflow"}); !errors.Is(err, ErrQueueCapacityExceeded) {
		t.Fatalf("unbounded queue: %v", err)
	}
	if roster.Count() != DesktopQueuedIdentityLimit {
		t.Fatalf("queue size = %d", roster.Count())
	}
}
