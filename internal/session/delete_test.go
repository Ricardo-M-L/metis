package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteRemovesOwnedFilesAndPreservesPrefixCollisions(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const id = "sess"
	if err := store.WriteHeader(id, "model", ""); err != nil {
		t.Fatal(err)
	}

	owned := []string{
		store.timingPath(id),
		store.costPath(id),
		store.costPath(id) + ".tmp",
		store.archivePath(id),
		filepath.Join(dir, "tags", id+".txt"),
	}
	for _, path := range owned {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Snapshot(id, "named"); err != nil {
		t.Fatal(err)
	}
	owned = append(owned, filepath.Join(dir, "snapshots", id+"-named.json"))

	unrelated := []string{
		store.timingPath("sess-extra"),
		store.costPath("sess-extra"),
		store.archivePath("sess-extra"),
		filepath.Join(dir, "tags", "sess-extra.txt"),
	}
	for _, path := range unrelated {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WriteHeader("sess-extra", "model", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot("sess-extra", "named"); err != nil {
		t.Fatal(err)
	}
	unrelated = append(unrelated,
		store.path("sess-extra"),
		filepath.Join(dir, "snapshots", "sess-extra-named.json"),
	)

	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(id); err != nil {
		t.Fatalf("second Delete should be idempotent: %v", err)
	}
	for _, path := range append([]string{store.path(id)}, owned...) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("owned path still exists: %s (err=%v)", path, err)
		}
	}
	for _, path := range unrelated {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated path was removed: %s: %v", path, err)
		}
	}
}

func TestDeleteRemovesOnlyExactParentSubagents(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "subagents")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("sess", "model", ""); err != nil {
		t.Fatal(err)
	}
	writeHeader := func(name, parent string) string {
		t.Helper()
		path := filepath.Join(subdir, name)
		line := fmt.Sprintf(`{"type":"header","header":{"id":"%s","sub_agent_of":"%s"}}`+"\n", name, parent)
		if err := os.WriteFile(path, []byte(line+`{"type":"message"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	owned := writeHeader("owned.jsonl", "sess")
	other := writeHeader("other.jsonl", "sess-extra")
	malformed := filepath.Join(subdir, "malformed.jsonl")
	if err := os.WriteFile(malformed, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("sess"); err == nil {
		t.Fatal("Delete succeeded despite a subagent with unknowable ownership")
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned subagent still exists: %v", err)
	}
	for _, path := range []string{other, malformed} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated/corrupt subagent removed: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(store.path("sess")); err != nil {
		t.Fatalf("canonical transcript was removed after partial failure: %v", err)
	}
	if err := os.Remove(malformed); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("sess"); err != nil {
		t.Fatalf("retry after resolving corrupt ownership file: %v", err)
	}
}

func TestDeleteRejectsUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.jsonl")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../outside", "a/b", `a\b`, "bad\nname"} {
		if err := store.Delete(id); err == nil {
			t.Errorf("Delete(%q) unexpectedly succeeded", id)
		}
	}
	if err := (*Store)(nil).Delete("safe"); err == nil {
		t.Error("nil store Delete unexpectedly succeeded")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file changed: %v", err)
	}
}

func TestDeleteKeepsTranscriptWhenSidecarRemovalFails(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "retryable-delete"
	if err := store.WriteHeader(id, "model", ""); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory at a sidecar path makes os.Remove fail without
	// depending on platform-specific permission behavior.
	if err := os.MkdirAll(filepath.Join(store.timingPath(id), "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(id); err == nil {
		t.Fatal("Delete unexpectedly succeeded")
	}
	if _, err := os.Stat(store.path(id)); err != nil {
		t.Fatalf("canonical transcript was removed after partial failure: %v", err)
	}
}
