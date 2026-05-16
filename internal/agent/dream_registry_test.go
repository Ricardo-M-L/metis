package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestBuildDreamRegistry_AddsSkillSynthWithoutPollutingParent — the
// core scoping invariant: SkillSynth must end up in the dream
// registry, but the parent registry the main agent uses must NOT
// contain it. Without this, a regular user-turn would see SkillSynth
// in its tool schema and might try to call it.
func TestBuildDreamRegistry_AddsSkillSynthWithoutPollutingParent(t *testing.T) {
	parent := tools.NewRegistry()
	parent.Register(&stubTool{name: "Read"})

	dream := buildDreamRegistry(parent, t.TempDir(), nil)
	if dream == nil {
		t.Fatalf("buildDreamRegistry returned nil")
	}
	if _, ok := dream.Get("SkillSynth"); !ok {
		t.Errorf("dream registry missing SkillSynth")
	}
	if _, ok := dream.Get("Read"); !ok {
		t.Errorf("dream registry should inherit parent tools (Read missing)")
	}
	if _, leaked := parent.Get("SkillSynth"); leaked {
		t.Errorf("SkillSynth leaked into parent registry — main agent would see it")
	}
}

// TestBuildDreamRegistry_NilParentPassesThrough — defensive: a nil
// parent (test scaffold without a fully-wired Loop) must not panic.
func TestBuildDreamRegistry_NilParentPassesThrough(t *testing.T) {
	got := buildDreamRegistry(nil, t.TempDir(), nil)
	if got != nil {
		t.Errorf("nil parent should return nil, got non-nil")
	}
}

// TestBuildDreamRegistry_EmptySkillsDirReturnsParent — when the user
// skills dir can't be resolved (UserHomeDir failure), we fall back to
// the parent registry rather than blow up. The memory consolidation
// can still complete; only the skill-synthesis branch is lost.
func TestBuildDreamRegistry_EmptySkillsDirReturnsParent(t *testing.T) {
	parent := tools.NewRegistry()
	parent.Register(&stubTool{name: "Read"})

	dream := buildDreamRegistry(parent, "", nil)
	if dream != parent {
		t.Errorf("empty skillsDir should return parent unchanged")
	}
	if _, ok := dream.Get("SkillSynth"); ok {
		t.Errorf("SkillSynth must not appear when skillsDir is empty")
	}
}

// stubTool is the minimum Tool impl needed by the registry tests.
// Embeds BaseTool to inherit IsEnabled() = true.
type stubTool struct {
	tools.BaseTool
	name string
}

func (s *stubTool) Name() string                                 { return s.name }
func (s *stubTool) Description() string                          { return "stub" }
func (s *stubTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (s *stubTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (s *stubTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (s *stubTool) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "stub"}, nil
}
