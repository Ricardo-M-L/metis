package artifact

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreConditionalUpdateHasOneWinnerAcrossStores(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	first, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create("session-a", "Original", "<p>original</p>")
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		title string
		body  string
		item  *Manifest
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for i, store := range []*Store{first, second} {
		title := []string{"First writer", "Second writer"}[i]
		body := "<p>" + title + "</p>"
		go func() {
			<-start
			item, err := store.UpdateIfVersion("session-a", created.ID, title, body, 1)
			results <- outcome{title: title, body: body, item: item, err: err}
		}()
	}
	close(start)
	var winner outcome
	var successes, conflicts int
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			successes++
			winner = result
		case errors.Is(result.err, ErrVersionConflict):
			conflicts++
			var conflict *VersionConflictError
			if !errors.As(result.err, &conflict) || conflict.ExpectedVersion != 1 || conflict.CurrentVersion != 2 {
				t.Fatalf("conflict must describe the checked versions: %v", result.err)
			}
			if result.item != nil {
				t.Fatalf("conflict returned a manifest: %+v", result.item)
			}
		default:
			t.Fatalf("conditional update failed unexpectedly: %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	manifest, err := first.Get("session-a", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.CurrentVersion != 2 || len(manifest.Versions) != 2 || manifest.Title != winner.title {
		t.Fatalf("loser changed the winning manifest: %+v", manifest)
	}
	body, _, err := second.ReadVersion("session-a", created.ID, 0)
	if err != nil || !bytes.Contains(body, []byte(winner.title)) {
		t.Fatalf("winning content was not preserved: %q, %v", body, err)
	}
	if _, _, err := first.ReadVersion("session-a", created.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("conflicting writer created a third version: %v", err)
	}
}

func TestStoreConditionalUpdateRejectsStaleAndInvalidVersionsWithoutWriting(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create("session-a", "Original", "<p>original</p>")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("session-a", created.ID, "Newer title", "<p>newer content</p>"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int{1, 3} {
		if _, err := store.UpdateIfVersion("session-a", created.ID, "Stale title", "<p>stale content</p>", expected); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("expected=%d: got %v, want ErrVersionConflict", expected, err)
		}
	}
	for _, expected := range []int{0, -1} {
		if _, err := store.UpdateIfVersion("session-a", created.ID, "Invalid title", "<p>invalid content</p>", expected); !errors.Is(err, ErrInvalidVersion) {
			t.Fatalf("expected=%d: got %v, want ErrInvalidVersion", expected, err)
		}
	}
	manifest, err := store.Get("session-a", created.ID)
	if err != nil || manifest.CurrentVersion != 2 || len(manifest.Versions) != 2 || manifest.Title != "Newer title" {
		t.Fatalf("rejected updates changed the manifest: %+v, %v", manifest, err)
	}
	body, _, err := store.ReadVersion("session-a", created.ID, 0)
	if err != nil || !bytes.Contains(body, []byte("newer content")) {
		t.Fatalf("rejected updates changed content: %q, %v", body, err)
	}

	updated, err := store.UpdateIfVersion("session-a", created.ID, "", `<p onclick="bad()">accepted</p><script>bad()</script>`, 2)
	if err != nil || updated.CurrentVersion != 3 || updated.Title != "Newer title" {
		t.Fatalf("valid conditional update: %+v, %v", updated, err)
	}
	body, _, err = store.ReadVersion("session-a", created.ID, 0)
	if err != nil || !bytes.Contains(body, []byte("accepted")) || bytes.Contains(body, []byte("onclick")) || bytes.Contains(body, []byte("script")) {
		t.Fatalf("conditional update bypassed sanitization: %q, %v", body, err)
	}
}

func TestStoreConditionalUpdateChecksOwnershipBeforeVersion(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create("owner", "Private title", "<p>private content</p>")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("owner", created.ID, "", "<p>private newer content</p>"); err != nil {
		t.Fatal(err)
	}
	item, err := store.UpdateIfVersion("other", created.ID, "Stolen", "<p>stolen</p>", 1)
	var conflict *VersionConflictError
	if !errors.Is(err, ErrOwnerMismatch) || errors.As(err, &conflict) || item != nil {
		t.Fatalf("foreign session learned version metadata: item=%+v err=%v", item, err)
	}
	if strings.Contains(err.Error(), "Private title") || strings.Contains(err.Error(), store.Root()) || strings.Contains(err.Error(), "current") {
		t.Fatalf("foreign session error leaked private metadata: %v", err)
	}
}
