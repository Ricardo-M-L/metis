package slash

import (
	"strings"
	"testing"
)

func TestBatchPrompt_HasAllPhases(t *testing.T) {
	out := BatchPrompt("rename foo to bar everywhere")
	for _, must := range []string{
		"PHASE 1 — RESEARCH",
		"PHASE 2 — PLAN",
		"PHASE 3 — EXECUTE",
		"5–30",
		"isolation=\"worktree\"",
		"run_in_background=true",
		"PR: <url>",
		"rename foo to bar everywhere",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("BatchPrompt output missing %q", must)
		}
	}
}

func TestBatchPrompt_TrimsTask(t *testing.T) {
	out := BatchPrompt("   trim me  \n")
	if !strings.Contains(out, "Task: trim me") {
		t.Errorf("task not trimmed: %q", out)
	}
}

func TestBatchPrompt_RegisteredCommand(t *testing.T) {
	r := NewRegistry()
	RegisterAll(r, nil)
	_, ok := r.Get("batch")
	if !ok {
		t.Errorf("/batch not registered")
	}
	// Ensure handler returns SignalBatch with non-empty args.
	handled, _, sig, args := r.Parse("/batch refactor auth")
	if !handled {
		t.Errorf("/batch not parsed")
	}
	if sig != SignalBatch {
		t.Errorf("expected SignalBatch, got %v", sig)
	}
	if args != "refactor auth" {
		t.Errorf("args = %q", args)
	}

	// Empty args should yield usage hint, not SignalBatch.
	_, display, sig2, _ := r.Parse("/batch")
	if sig2 == SignalBatch {
		t.Errorf("empty /batch should not emit SignalBatch")
	}
	if !strings.Contains(display, "usage") {
		t.Errorf("expected usage hint for empty /batch, got %q", display)
	}
}
