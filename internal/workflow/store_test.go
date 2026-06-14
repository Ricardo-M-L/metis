package workflow

import (
	"strings"
	"testing"
)

func TestStore_SaveLoadList(t *testing.T) {
	s := NewStore(t.TempDir())
	wf := Workflow{Name: "ci", Steps: []Step{
		{Name: "build", Command: "go build ./..."},
		{Name: "test", Command: "go test ./..."},
	}}
	if err := s.Save(wf); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("ci")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steps) != 2 || got.Steps[0].Command != "go build ./..." {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "ci" {
		t.Errorf("List = %v, want [ci]", names)
	}
}

func TestStore_RejectsBadNames(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, bad := range []string{"", "../escape", "has/slash", "has space", strings.Repeat("x", 65)} {
		if err := s.Save(Workflow{Name: bad, Steps: []Step{{Name: "a", Command: "echo x"}}}); err == nil {
			t.Errorf("Save accepted invalid name %q", bad)
		}
	}
}

func TestStore_LoadMissing(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load("nope"); err == nil || !strings.Contains(err.Error(), "no saved workflow") {
		t.Errorf("expected clean not-found error, got %v", err)
	}
}

func TestStore_RejectsEmptySteps(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save(Workflow{Name: "empty"}); err == nil {
		t.Error("Save accepted a workflow with no steps")
	}
}

func TestStore_ListEmptyDir(t *testing.T) {
	s := NewStore(t.TempDir() + "/does-not-exist-yet")
	names, err := s.List()
	if err != nil {
		t.Fatalf("List on absent dir should be clean: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected no names, got %v", names)
	}
}
