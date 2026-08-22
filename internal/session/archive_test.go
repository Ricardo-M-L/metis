package session

import (
	"os"
	"testing"
)

func TestSessionArchiveRoundTripKeepsTranscript(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "session-one"
	if err := store.WriteHeader(id, "model", "system"); err != nil {
		t.Fatal(err)
	}
	transcript := store.path(id)
	before, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(id); err != nil {
		t.Fatal(err)
	}
	if !store.IsArchived(id) {
		t.Fatal("session should be archived")
	}
	after, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("archive removed transcript: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("archive modified transcript")
	}
	if err := store.Archive(id); err != nil {
		t.Fatalf("second archive should be idempotent: %v", err)
	}
	if err := store.Unarchive(id); err != nil {
		t.Fatal(err)
	}
	if store.IsArchived(id) {
		t.Fatal("session should be restored")
	}
	if err := store.Unarchive(id); err != nil {
		t.Fatalf("second unarchive should be idempotent: %v", err)
	}
}

func TestSessionArchiveRejectsMissingAndUnsafeIDs(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../escape", `..\escape`, "missing"} {
		if err := store.Archive(id); err == nil {
			t.Errorf("Archive(%q) should fail", id)
		}
	}
}
