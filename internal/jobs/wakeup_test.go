package jobs

import (
	"sync"
	"testing"
	"time"
)

func TestRegistryWakeupDoesNotConsumeCompletion(t *testing.T) {
	r := quickRegistry(t)
	job := spawnEcho(t, r, "host-wakeup")
	select {
	case <-r.Wakeup():
	case <-time.After(3 * time.Second):
		t.Fatal("completed process did not wake the session host")
	}
	if !r.HasPendingNotifications() {
		t.Fatal("host wake consumed the model's completion envelope")
	}
	select {
	case note := <-r.Notify():
		if note.JobID != job.ID || note.Status != StatusCompleted {
			t.Fatalf("unexpected completion: %+v", note)
		}
	default:
		t.Fatal("agent loop could not receive its completion envelope")
	}
	if r.HasPendingNotifications() {
		t.Fatal("notification remains pending after the loop consumes it")
	}
}

func TestRegistryWakeupCoalescesAndMayBeStale(t *testing.T) {
	r := NewRegistryBuffered(t.TempDir(), 4)
	for _, id := range []string{"first", "second", "third"} {
		r.publish(r.generation, Notification{JobID: id, Status: StatusCompleted})
	}
	if got := len(r.wakeup); got != 1 {
		t.Fatalf("burst produced %d wake hints, want one", got)
	}
	for i := 0; i < 3; i++ {
		<-r.Notify()
	}
	// The model can drain all envelopes while its host is busy. Receiving the
	// old edge must not require another model turn when nothing remains pending.
	<-r.Wakeup()
	if r.HasPendingNotifications() {
		t.Fatal("stale wake falsely reports pending completion")
	}
	r.publish(r.generation, Notification{JobID: "next", Status: StatusCompleted})
	select {
	case <-r.Wakeup():
	default:
		t.Fatal("later publication did not re-arm wakeup")
	}
}

func TestRegistryDroppedNotificationDoesNotWake(t *testing.T) {
	for _, capacity := range []int{0, 1} {
		r := NewRegistryBuffered(t.TempDir(), capacity)
		if capacity > 0 {
			r.publish(r.generation, Notification{JobID: "queued", Status: StatusCompleted})
			<-r.Wakeup()
		}
		r.publish(r.generation, Notification{JobID: "dropped", Status: StatusCompleted})
		select {
		case <-r.Wakeup():
			t.Fatalf("dropped notification woke host with capacity %d", capacity)
		default:
		}
		if r.HasPendingNotifications() != (capacity > 0) {
			t.Fatalf("pending state changed for dropped publication with capacity %d", capacity)
		}
	}
}

func TestRegistryResetDrainsWakeupAndRejectsOldGeneration(t *testing.T) {
	r := quickRegistry(t)
	notify, wakeup := r.Notify(), r.Wakeup()
	oldGeneration := r.generation
	r.publish(oldGeneration, Notification{JobID: "old", Status: StatusCompleted})
	r.Reset(0)
	r.publish(oldGeneration, Notification{JobID: "late-old", Status: StatusCompleted})
	if r.HasPendingNotifications() || len(r.wakeup) != 0 {
		t.Fatal("reset retained an old-generation notification or wakeup")
	}
	if r.Notify() != notify || r.Wakeup() != wakeup {
		t.Fatal("reset replaced the channels held by the active session host")
	}
	r.publish(r.generation, Notification{JobID: "new", Status: StatusCompleted})
	select {
	case <-wakeup:
	default:
		t.Fatal("destination session cannot receive fresh wakeups")
	}
	if note := <-notify; note.JobID != "new" {
		t.Fatalf("old-generation envelope crossed reset: %+v", note)
	}
}

func TestRegistryWakeupResetPublicationRace(t *testing.T) {
	r := quickRegistry(t)
	for i := 0; i < 100; i++ {
		generation := r.generation
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			r.publish(generation, Notification{JobID: "source", Status: StatusCompleted})
		}()
		go func() {
			defer group.Done()
			r.Reset(0)
		}()
		group.Wait()
		if r.HasPendingNotifications() || len(r.wakeup) != 0 {
			t.Fatalf("source-generation readiness survived concurrent reset at iteration %d", i)
		}
	}
}

func TestNilRegistryReadiness(t *testing.T) {
	var r *Registry
	if r.Wakeup() != nil || r.HasPendingNotifications() {
		t.Fatal("nil registry should disable host readiness")
	}
}
