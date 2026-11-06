package memory

import (
	"testing"
	"time"
)

func TestRecallGuardSerializesRepositoryWriter(t *testing.T) {
	mm, err := NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.Archival().Insert(Passage{ID: "one", Content: "Nebula", SourceSessionID: "source"}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	guardDone := make(chan error, 1)
	go func() {
		guardDone <- mm.GuardRecallSnapshot(func(_, _ string, p []Passage) error {
			if len(p) != 1 {
				t.Error("missing authoritative passage")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	started := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() { close(started); writerDone <- mm.DeleteSession("source") }()
	<-started
	select {
	case err := <-writerDone:
		t.Fatalf("writer crossed active snapshot guard: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-guardDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := mm.GuardRecallSnapshot(func(_, _ string, p []Passage) error {
		if len(p) != 0 {
			t.Error("next snapshot retained revoked passage")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
