package runtime

import (
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memory"
)

type bindingLegacyRepository struct{ memory.Repository }

func TestWorkspaceBindingGuardPinsWorkspaceAndForwardsAuthority(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.Archival().Insert(memory.Passage{ID: "one", Content: "safe workspace fact"}); err != nil {
		t.Fatal(err)
	}
	binding := &WorkspaceMemoryBinding{current: mm}
	called := false
	sentinel := errors.New("consumer failure")
	err = binding.GuardRecallSnapshot(func(g, b string, p []memory.Passage) error {
		called = true
		if g == "" || len(p) != 1 {
			t.Fatal("authority snapshot missing")
		}
		if binding.mu.TryLock() {
			binding.mu.Unlock()
			t.Fatal("workspace can switch during accepted snapshot")
		}
		return sentinel
	})
	if !called || !errors.Is(err, sentinel) {
		t.Fatal("guard consumer/error not forwarded")
	}
	g, body, err := binding.RecallSnapshot()
	if err != nil || g == "" {
		t.Fatalf("snapshot %q %q %v", g, body, err)
	}
	for _, repo := range []memory.Repository{nil, bindingLegacyRepository{mm}} {
		binding.current = repo
		called = false
		err = binding.GuardRecallSnapshot(func(string, string, []memory.Passage) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("unavailable authority manufactured a snapshot")
		}
	}
}
