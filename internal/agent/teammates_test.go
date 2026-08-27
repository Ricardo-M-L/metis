package agent

// teammates_test.go locks the G.0 contract:
//   - capacity cap is enforced before any sub-agent kicks off
//   - duplicate names rejected
//   - anonymous names auto-generated + flagged
//   - List() snapshot doesn't race with concurrent Register/Unregister
//   - Lookup returns ok=false after Unregister so SendMessage can fail
//     fast instead of blocking on a dead mailbox
//
// If a refactor breaks any of these, sub-agent fork-bomb protection
// or peer-messaging delivery silently regresses.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestRoster_CapacityRejects — the G.0 fork-bomb safety net. Capacity
// 3 means the 4th Register MUST return ErrCapacityExceeded so the
// Agent tool can short-circuit with an IsError instead of spawning.
func TestRoster_CapacityRejects(t *testing.T) {
	r := NewRoster(3)
	for i := 0; i < 3; i++ {
		if err := r.Register(&Teammate{Name: "t" + string(rune('a'+i))}); err != nil {
			t.Fatalf("registration %d should succeed; got %v", i, err)
		}
	}
	err := r.Register(&Teammate{Name: "fourth"})
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("4th register should return ErrCapacityExceeded; got %v", err)
	}
	if r.Count() != 3 {
		t.Fatalf("Count after rejection should still be 3; got %d", r.Count())
	}
}

// TestRoster_CapacityZeroUnlimited — capacity <= 0 disables the cap.
// Tests pass at 50 entries; production code paths that opt out (e.g.
// /batch which is documented to need 20-30) rely on this.
func TestRoster_CapacityZeroUnlimited(t *testing.T) {
	r := NewRoster(0)
	for i := 0; i < 50; i++ {
		if err := r.Register(&Teammate{}); err != nil {
			t.Fatalf("uncapped register %d failed: %v", i, err)
		}
	}
	if r.Count() != 50 {
		t.Errorf("expected 50 teammates; got %d", r.Count())
	}
}

// TestRoster_DuplicateNamedAutoSuffixed — default behavior (2026-05-14):
// duplicate name gets auto-suffixed (alice → alice-2) instead of
// returning ErrNameInUse. Mirrors claude-code spawnMultiAgent.ts:267.
// The strict-rejection path is covered separately by
// TestRoster_DuplicateNamedRejectedWithStrictName below.
func TestRoster_DuplicateNamedAutoSuffixed(t *testing.T) {
	r := NewRoster(0)
	if err := r.Register(&Teammate{Name: "alice"}); err != nil {
		t.Fatalf("first alice should succeed; got %v", err)
	}
	tm2 := &Teammate{Name: "alice"}
	if err := r.Register(tm2); err != nil {
		t.Errorf("duplicate name should auto-suffix, not error; got %v", err)
	}
	if tm2.Name != "alice-2" {
		t.Errorf("second alice should be renamed to alice-2; got %q", tm2.Name)
	}
	tm3 := &Teammate{Name: "alice"}
	if err := r.Register(tm3); err != nil {
		t.Errorf("third alice should auto-suffix; got %v", err)
	}
	if tm3.Name != "alice-3" {
		t.Errorf("third alice should be renamed to alice-3; got %q", tm3.Name)
	}
}

// TestRoster_DuplicateNamedRejectedWithStrictName — opt-in strict
// mode for callers (slash command resume) that need exact-match
// semantics.
func TestRoster_DuplicateNamedRejectedWithStrictName(t *testing.T) {
	r := NewRoster(0)
	if err := r.Register(&Teammate{Name: "alice"}); err != nil {
		t.Fatalf("first alice should succeed; got %v", err)
	}
	err := r.Register(&Teammate{Name: "alice", StrictName: true})
	if !errors.Is(err, ErrNameInUse) {
		t.Errorf("duplicate name with StrictName=true should return ErrNameInUse; got %v", err)
	}
}

// TestRoster_AnonymousAutoNamed — empty Name gets `_anon-<hex>` and
// Anonymous flag set. UI's /agents view filters these by default so
// users see the named teammates only.
func TestRoster_AnonymousAutoNamed(t *testing.T) {
	r := NewRoster(0)
	tm := &Teammate{}
	if err := r.Register(tm); err != nil {
		t.Fatalf("anon register failed: %v", err)
	}
	if tm.Name == "" {
		t.Errorf("anonymous teammate should have auto-generated name")
	}
	if !tm.Anonymous {
		t.Errorf("anonymous teammate should have Anonymous=true; got false (name=%q)", tm.Name)
	}
}

// TestRoster_UnregisterClearsLookup — after Unregister, Lookup must
// report not-found so SendMessage refuses delivery to a dead mailbox.
func TestRoster_UnregisterClearsLookup(t *testing.T) {
	r := NewRoster(0)
	tm := &Teammate{Name: "bob"}
	_ = r.Register(tm)
	if _, ok := r.Lookup("bob"); !ok {
		t.Fatalf("Lookup bob right after Register should succeed")
	}
	r.Unregister("bob")
	if _, ok := r.Lookup("bob"); ok {
		t.Errorf("Lookup bob after Unregister must return false; got found")
	}
}

// TestRoster_MailboxAutoCreated — Register without setting Mailbox
// should populate a buffered chan so SendMessage can do
// `<- t.Mailbox` without a nil-channel panic. Buffer size 16 is
// the documented contract; verify by filling without blocking.
func TestRoster_MailboxAutoCreated(t *testing.T) {
	r := NewRoster(0)
	tm := &Teammate{Name: "carol"}
	_ = r.Register(tm)
	if tm.Mailbox == nil {
		t.Fatalf("Register must auto-create Mailbox")
	}
	for i := 0; i < 16; i++ {
		select {
		case tm.Mailbox <- PeerMessage{From: "test", Body: "msg"}:
		default:
			t.Fatalf("Mailbox should accept 16 messages without blocking; rejected at %d", i)
		}
	}
}

// TestRoster_ListSortedByStarted — /agents list order MUST be
// chronological for the UI to read naturally (oldest sub-agent first,
// like a process table).
func TestRoster_ListSortedByStarted(t *testing.T) {
	r := NewRoster(0)
	// Register out of order; List should re-sort by Started.
	_ = r.Register(&Teammate{Name: "a"})
	_ = r.Register(&Teammate{Name: "b"})
	_ = r.Register(&Teammate{Name: "c"})
	got := r.List()
	if len(got) != 3 {
		t.Fatalf("List should return 3; got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Started.Before(got[i-1].Started) {
			t.Errorf("List[%d] (%s, %v) older than List[%d] (%s, %v)",
				i, got[i].Name, got[i].Started, i-1, got[i-1].Name, got[i-1].Started)
		}
	}
}

// TestRoster_CancelAllFires — shutdown path. Every teammate's Cancel
// must run, and the roster ends empty so subsequent /agents list
// doesn't leak references.
func TestRoster_CancelAllFires(t *testing.T) {
	r := NewRoster(0)
	var cancelled sync.Map
	for _, n := range []string{"x", "y", "z"} {
		name := n
		_ = r.Register(&Teammate{
			Name:   name,
			Cancel: func() { cancelled.Store(name, true) },
		})
	}
	r.CancelAll()
	for _, n := range []string{"x", "y", "z"} {
		if _, ok := cancelled.Load(n); !ok {
			t.Errorf("CancelAll did not fire %s's Cancel func", n)
		}
	}
	if r.Count() != 0 {
		t.Errorf("CancelAll should clear the registry; Count=%d", r.Count())
	}
}

// TestRoster_ConcurrentSafe — concurrent Register/Unregister/Count
// must not race. Run with `go test -race` to verify the mutex is
// covering all maps and slices.
func TestRoster_ConcurrentSafe(t *testing.T) {
	r := NewRoster(0)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		i := i
		go func() {
			defer wg.Done()
			_ = r.Register(&Teammate{Name: anonName()})
			_ = i
		}()
		go func() {
			defer wg.Done()
			_ = r.Count()
			_ = r.List()
		}()
	}
	wg.Wait()
	if r.Count() != 100 {
		t.Errorf("100 concurrent registers should yield Count=100; got %d", r.Count())
	}
}

func TestRosterCancelAndWaitJoinsRunner(t *testing.T) {
	r := NewRoster(2)
	canceled := make(chan struct{})
	release := make(chan struct{})
	teammate := &Teammate{Name: "background"}
	teammate.Cancel = func() {
		select {
		case <-canceled:
		default:
			close(canceled)
		}
	}
	if err := r.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-canceled
		<-release
		r.UnregisterTeammate(teammate)
	}()

	done := make(chan error, 1)
	go func() { done <- r.CancelAndWait(context.Background()) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("roster did not cancel the runner")
	}
	select {
	case err := <-done:
		t.Fatalf("CancelAndWait returned before runner exit: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := r.Register(&Teammate{Name: "late"}); !errors.Is(err, ErrRosterResetting) {
		t.Fatalf("register during join error = %v, want %v", err, ErrRosterResetting)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("CancelAndWait: %v", err)
	}
	if r.Count() != 0 || len(r.List()) != 0 {
		t.Fatalf("joined roster not empty: count=%d list=%+v", r.Count(), r.List())
	}
	if err := r.Register(&Teammate{Name: "next-session"}); err != nil {
		t.Fatalf("roster not reusable after join: %v", err)
	}
}
