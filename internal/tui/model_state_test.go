package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// resetModelStateForTest swaps the singleton for a test-local instance
// backed by a temp dir, and restores the original on cleanup.
func resetModelStateForTest(t *testing.T) *modelState {
	t.Helper()
	tmp := t.TempDir()
	orig := globalModelState
	globalModelState = &modelState{path: filepath.Join(tmp, "model-state.json")}
	t.Cleanup(func() { globalModelState = orig })
	return globalModelState
}

func TestModelState_AddRecent(t *testing.T) {
	s := resetModelStateForTest(t)

	s.AddRecent("claude-opus-4-7")
	s.AddRecent("gpt-4o")
	s.AddRecent("claude-sonnet-4-6")

	recent := s.Recent()
	if len(recent) != 3 {
		t.Fatalf("expected 3 recent, got %d", len(recent))
	}
	if recent[0] != "claude-sonnet-4-6" {
		t.Errorf("most recent should be first, got %q", recent[0])
	}

	// Adding an existing model should move it to front.
	s.AddRecent("claude-opus-4-7")
	recent = s.Recent()
	if recent[0] != "claude-opus-4-7" {
		t.Errorf("re-added model should move to front, got %q", recent[0])
	}
	if len(recent) != 3 {
		t.Errorf("re-adding should not grow list, got %d", len(recent))
	}
}

func TestModelState_AddRecent_CapsAt10(t *testing.T) {
	s := resetModelStateForTest(t)

	for i := 0; i < 15; i++ {
		s.AddRecent("model-" + string(rune('a'+i)))
	}

	recent := s.Recent()
	if len(recent) != 10 {
		t.Fatalf("expected cap of 10, got %d", len(recent))
	}
	// Most recent should be the last one added.
	if recent[0] != "model-o" {
		t.Errorf("most recent should be model-o, got %q", recent[0])
	}
}

func TestModelState_IsRecent(t *testing.T) {
	s := resetModelStateForTest(t)

	if s.IsRecent("nonexistent") {
		t.Error("empty state should not have any recent")
	}

	s.AddRecent("claude-opus-4-7")
	if !s.IsRecent("claude-opus-4-7") {
		t.Error("added model should be recent")
	}
	if s.IsRecent("gpt-4o") {
		t.Error("non-added model should not be recent")
	}
}

func TestModelState_ToggleFavorite(t *testing.T) {
	s := resetModelStateForTest(t)

	if s.IsFavorite("claude-opus-4-7") {
		t.Error("empty state should not have favorites")
	}

	s.ToggleFavorite("claude-opus-4-7")
	if !s.IsFavorite("claude-opus-4-7") {
		t.Error("toggled-on model should be favorite")
	}

	s.ToggleFavorite("claude-opus-4-7")
	if s.IsFavorite("claude-opus-4-7") {
		t.Error("toggled-off model should not be favorite")
	}
}

func TestModelState_Persistence(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "model-state.json")

	// Write state.
	s1 := &modelState{path: path}
	s1.AddRecent("claude-opus-4-7")
	s1.ToggleFavorite("gpt-4o")

	// Read back in a new instance.
	s2 := &modelState{path: path}
	s2.load()

	if !s2.IsRecent("claude-opus-4-7") {
		t.Error("persisted recent should survive reload")
	}
	if !s2.IsFavorite("gpt-4o") {
		t.Error("persisted favorite should survive reload")
	}
}

func TestModelState_CorruptFileIgnored(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "model-state.json")
	_ = os.WriteFile(path, []byte("not json"), 0o644)

	s := &modelState{path: path}
	s.load() // should not panic

	if s.IsRecent("anything") {
		t.Error("corrupt file should yield empty state")
	}
}
