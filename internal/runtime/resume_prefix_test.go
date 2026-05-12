package runtime

// resume_prefix_test.go — `--resume <prefix>` ergonomics. The picker
// prints truncated 12-char prefixes; the CLI flag must accept those
// same prefixes (and any unambiguous prefix length) instead of forcing
// the user to paste the full UUID. Mirrors `gh issue view <num>` /
// `git log <short-sha>` UX.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/session"
)

// seedSession writes a minimal valid header into store/<id>.jsonl so
// List() picks it up and Load() can find it later.
func seedSession(t *testing.T, store *session.Store, id string) {
	t.Helper()
	if err := store.WriteHeaderFull(session.Header{ID: id, Model: "test-m"}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func newTempStore(t *testing.T) *session.Store {
	t.Helper()
	dir := t.TempDir()
	return &session.Store{Dir: dir}
}

func TestResolveSessionID_FullIDFastPath(t *testing.T) {
	store := newTempStore(t)
	id := "0f8244a9-1671-437d-8c48-e913a1933be9"
	seedSession(t, store, id)
	got, err := ResolveSessionID(store, id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != id {
		t.Errorf("want %q, got %q", id, got)
	}
}

func TestResolveSessionID_UniquePrefix(t *testing.T) {
	store := newTempStore(t)
	full := "0f8244a9-1671-437d-8c48-e913a1933be9"
	seedSession(t, store, full)
	// Also seed an unrelated session to prove the prefix discriminates.
	seedSession(t, store, "aaaa1111-2222-3333-4444-555566667777")

	// The 12-char prefix the picker prints.
	got, err := ResolveSessionID(store, "0f8244a9-167")
	if err != nil {
		t.Fatalf("expected unique prefix resolution; got error: %v", err)
	}
	if got != full {
		t.Errorf("want %q, got %q", full, got)
	}
}

func TestResolveSessionID_AmbiguousPrefixErrorsOut(t *testing.T) {
	store := newTempStore(t)
	a := "0f8244a9-1671-437d-8c48-e913a1933be9"
	b := "0f8244a9-9999-aaaa-bbbb-cccccccccccc"
	seedSession(t, store, a)
	seedSession(t, store, b)
	_, err := ResolveSessionID(store, "0f8244a9-")
	if err == nil {
		t.Fatal("expected ambiguity error; got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguity; got %q", err)
	}
	// Should list both candidates so the user can disambiguate.
	if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("error should list both matches; got %q", err)
	}
}

func TestResolveSessionID_NoMatchErrors(t *testing.T) {
	store := newTempStore(t)
	seedSession(t, store, "aaaa1111-2222-3333-4444-555566667777")
	_, err := ResolveSessionID(store, "deadbeef")
	if err == nil {
		t.Fatal("expected not-found error; got nil")
	}
	if !strings.Contains(err.Error(), "no session matches") {
		t.Errorf("error should describe miss; got %q", err)
	}
}

func TestResolveSessionID_EmptyInputErrors(t *testing.T) {
	store := newTempStore(t)
	_, err := ResolveSessionID(store, "")
	if err == nil {
		t.Fatal("empty input should error")
	}
	_, err = ResolveSessionID(store, "   ")
	if err == nil {
		t.Fatal("whitespace-only input should error")
	}
}

func TestResolveSessionID_FullIDPreferredOverPrefixCollision(t *testing.T) {
	// A literal id `abc` exists, AND there's also `abc12345-…`. Passing
	// the literal `abc` must hit the fast-path (existing file) rather
	// than fall into the prefix scan and complain about collisions.
	store := newTempStore(t)
	seedSession(t, store, "abc")
	seedSession(t, store, "abc12345-2222-3333-4444-555566667777")
	got, err := ResolveSessionID(store, "abc")
	if err != nil {
		t.Fatalf("expected fast-path; got error: %v", err)
	}
	if got != "abc" {
		t.Errorf("fast-path must short-circuit prefix scan; got %q", got)
	}
}

// Sanity that store.Dir is what we think it is — the test fixture
// relies on writing files under it.
func TestSeedSessionLandsOnDisk(t *testing.T) {
	store := newTempStore(t)
	seedSession(t, store, "abc-xyz")
	_, err := os.Stat(filepath.Join(store.Dir, "abc-xyz.jsonl"))
	if err != nil {
		t.Fatalf("seed file missing: %v", err)
	}
}
