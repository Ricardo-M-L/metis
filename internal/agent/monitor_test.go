package agent

// monitor_test.go — covers the per-line pattern watcher: pattern
// matching fires events, non-matching lines are ignored, the rate
// limiter mutes after maxStrikes, and Stop unwires cleanly.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// writeAppend appends a line + newline to the file used by the
// MonitorRegistry's tail. Mirrors how jobs.DiskOutput's Writer would
// behave from the spawned process side.
func writeAppend(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// recvWithin pulls one event off the channel or fails the test if
// nothing arrives within d. Timeout-bounded so a test bug doesn't hang
// the suite.
func recvWithin(t *testing.T, ch <-chan MonitorEvent, d time.Duration) MonitorEvent {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(d):
		t.Fatalf("expected MonitorEvent within %s, got none", d)
		return MonitorEvent{}
	}
}

// noEventWithin asserts the channel stays silent for d. Used to verify
// non-matching lines and the rate-limit mute path.
func noEventWithin(t *testing.T, ch <-chan MonitorEvent, d time.Duration) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("expected NO event within %s, got %+v", d, e)
	case <-time.After(d):
		// success — no event arrived
	}
}

func TestMonitor_PatternMatchFiresEvent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "job.out")
	if err := os.WriteFile(out, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewMonitorRegistry(16)
	r.pollInterval = 30 * time.Millisecond
	defer r.StopAll()

	r.Watch("bg_test1", out, "test watch", []*regexp.Regexp{regexp.MustCompile(`ERROR`)})
	writeAppend(t, out, "INFO: starting up")
	writeAppend(t, out, "ERROR: something bad")
	writeAppend(t, out, "DEBUG: continuing")

	e := recvWithin(t, r.Events(), 1*time.Second)
	if e.JobID != "bg_test1" {
		t.Errorf("event.JobID = %q, want bg_test1", e.JobID)
	}
	if e.Match != "ERROR: something bad" {
		t.Errorf("event.Match = %q, want %q", e.Match, "ERROR: something bad")
	}
	if e.Description != "test watch" {
		t.Errorf("event.Description = %q, want %q", e.Description, "test watch")
	}
}

func TestMonitor_NonMatchingLinesSilent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "job.out")
	_ = os.WriteFile(out, []byte{}, 0o600)
	r := NewMonitorRegistry(16)
	r.pollInterval = 30 * time.Millisecond
	defer r.StopAll()

	r.Watch("bg_silent", out, "noop", []*regexp.Regexp{regexp.MustCompile(`^FATAL`)})
	writeAppend(t, out, "INFO: nothing fatal here")
	writeAppend(t, out, "WARN: also fine")

	noEventWithin(t, r.Events(), 200*time.Millisecond)
}

func TestMonitor_RateLimitMutesAfterStrikes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "job.out")
	_ = os.WriteFile(out, []byte{}, 0o600)
	r := NewMonitorRegistry(16)
	// Crank knobs to fast-test scale: 50ms poll, 200ms rate window,
	// 2 strike cap. Logical equivalent of the production 1s/15s/3
	// without sleeping the suite.
	r.pollInterval = 30 * time.Millisecond
	r.rateWindow = 200 * time.Millisecond
	r.maxStrikes = 2
	defer r.StopAll()

	r.Watch("bg_flood", out, "flood", []*regexp.Regexp{regexp.MustCompile(`MATCH`)})
	// Burst 5 matches. Expected: first emits (lastEmit empty); next 2
	// strike-counted (strikes 1, 2); 4th trips the breaker; 5th
	// silent.
	for i := 0; i < 5; i++ {
		writeAppend(t, out, "MATCH burst")
	}

	// Drain whatever fires before the breaker trips. Should be 1 (first)
	// + 2 (strikes 1+2) = at most 3 events; once tripped, no more fire.
	deadline := time.After(500 * time.Millisecond)
	count := 0
collect:
	for {
		select {
		case <-r.Events():
			count++
		case <-deadline:
			break collect
		}
	}
	if count == 0 {
		t.Errorf("expected at least 1 event in burst; got 0")
	}
	if count > 3 {
		t.Errorf("rate limiter should have muted after maxStrikes=2; got %d events", count)
	}
}

func TestMonitor_StopHaltsWatcher(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "job.out")
	_ = os.WriteFile(out, []byte{}, 0o600)
	r := NewMonitorRegistry(16)
	r.pollInterval = 30 * time.Millisecond

	r.Watch("bg_stop", out, "halt-me", []*regexp.Regexp{regexp.MustCompile(`HIT`)})
	r.Stop("bg_stop")
	// Even if a match lands after Stop, no event should fire.
	writeAppend(t, out, "HIT this should NOT emit")
	noEventWithin(t, r.Events(), 200*time.Millisecond)
}

func TestMonitor_DuplicateWatchReplacesPrevious(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "job.out")
	_ = os.WriteFile(out, []byte{}, 0o600)
	r := NewMonitorRegistry(16)
	r.pollInterval = 30 * time.Millisecond
	defer r.StopAll()

	r.Watch("bg_dup", out, "first", []*regexp.Regexp{regexp.MustCompile(`AAA`)})
	r.Watch("bg_dup", out, "second", []*regexp.Regexp{regexp.MustCompile(`BBB`)})
	writeAppend(t, out, "AAA should NOT match the replaced watch")
	writeAppend(t, out, "BBB matches the new one")

	e := recvWithin(t, r.Events(), 500*time.Millisecond)
	if e.Description != "second" {
		t.Errorf("after replace, watch description = %q, want %q", e.Description, "second")
	}
}

func TestMonitor_EmptyArgsAreNoop(t *testing.T) {
	r := NewMonitorRegistry(16)
	defer r.StopAll()
	// All three should silently no-op without panicking.
	r.Watch("", "/tmp/x", "no jobid", []*regexp.Regexp{regexp.MustCompile(".+")})
	r.Watch("bg", "", "no path", []*regexp.Regexp{regexp.MustCompile(".+")})
	r.Watch("bg", "/tmp/x", "no patterns", nil)
}
