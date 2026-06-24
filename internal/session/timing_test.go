package session

import (
	"testing"
	"time"
)

func TestTimingRecorder_RoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewSessionID()
	rec := store.NewTimingRecorder(id)
	rec.Record("Read", 12*time.Millisecond, false)
	rec.Record("Bash", 1500*time.Millisecond, false)
	rec.Record("Edit", 30*time.Millisecond, true)

	steps, err := store.ReadTiming(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(steps))
	}
	if steps[0].Tool != "Read" || steps[0].ElapsedMS != 12 {
		t.Errorf("step0 = %+v", steps[0])
	}
	if steps[1].Tool != "Bash" || steps[1].ElapsedMS != 1500 {
		t.Errorf("step1 = %+v", steps[1])
	}
	if !steps[2].IsError {
		t.Errorf("step2 should be flagged is_error: %+v", steps[2])
	}
	if steps[2].TS.IsZero() {
		t.Error("step should carry a timestamp")
	}
}

func TestReadTiming_NoSidecarIsEmpty(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	steps, err := store.ReadTiming("never-ran")
	if err != nil {
		t.Errorf("missing sidecar should be (nil,nil), got err=%v", err)
	}
	if len(steps) != 0 {
		t.Errorf("want no steps, got %d", len(steps))
	}
}

func TestNilTimingRecorder_IsNoop(t *testing.T) {
	var rec *TimingRecorder // the Loop's TimingSink may be a nil recorder's method
	rec.Record("X", time.Second, false)
	// must not panic
}
