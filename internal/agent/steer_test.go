package agent

import (
	"strings"
	"sync"
	"testing"
)

// TestSteerInject_BuffersAndDrainsOnce — basic queue contract: text in,
// text out, then empty. Multiple SteerInject calls accumulate joined
// with "\n".
func TestSteerInject_BuffersAndDrainsOnce(t *testing.T) {
	l := &Loop{}
	l.SteerInject("first hint")
	l.SteerInject("second hint")
	got := l.drainSteer()
	want := "first hint\nsecond hint"
	if got != want {
		t.Errorf("drainSteer mismatch:\n got %q\nwant %q", got, want)
	}
	if again := l.drainSteer(); again != "" {
		t.Errorf("second drain should be empty; got %q", again)
	}
}

// TestSteerInject_DropsWhitespace — empty / whitespace-only steers are
// no-ops so the mid-turn UI doesn't pollute the agent context with
// blank "[user steer mid-turn] " markers.
func TestSteerInject_DropsWhitespace(t *testing.T) {
	l := &Loop{}
	l.SteerInject("")
	l.SteerInject("   ")
	l.SteerInject("\n\t  ")
	if got := l.drainSteer(); got != "" {
		t.Errorf("whitespace-only steers should drop; got %q", got)
	}
}

// TestSteerInject_RaceSafe — SteerInject is callable from any goroutine
// (the chat surface is on a different goroutine than the agent loop).
// Race-free if mu is held correctly. Run under -race.
func TestSteerInject_RaceSafe(t *testing.T) {
	l := &Loop{}
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.SteerInject("hint-" + string(rune('a'+i%26)))
		}(i)
	}
	wg.Wait()
	got := l.drainSteer()
	// All n inserts should have landed; we don't assert order (concurrent
	// inserts have no order guarantee), only count.
	parts := strings.Split(got, "\n")
	if len(parts) != n {
		t.Errorf("expected %d steer entries; got %d", n, len(parts))
	}
}
