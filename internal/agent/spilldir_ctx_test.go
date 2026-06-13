package agent

import (
	"context"
	"testing"
)

// The spill dir threads to sub-agents through context the same way the
// budget tracker does (dispatch.go → builtin/agent.go,fork.go), so a
// child's oversized tool results offload instead of flooding its
// context. Lock the round-trip + the empty-dir no-op.
func TestSpillDirContextRoundTrip(t *testing.T) {
	base := context.Background()
	if got := SpillDirFromContext(base); got != "" {
		t.Fatalf("unset context should yield empty dir, got %q", got)
	}
	ctx := WithSpillDir(base, "/sessions/abc/microcompact-cache")
	if got := SpillDirFromContext(ctx); got != "/sessions/abc/microcompact-cache" {
		t.Fatalf("round-trip dir = %q", got)
	}
}

// Empty dir (spilling disabled in parent) must not be stored — a child
// reading it back gets "" and disables its own spilling, rather than
// spilling to an empty path.
func TestWithSpillDirEmptyIsNoop(t *testing.T) {
	ctx := WithSpillDir(context.Background(), "")
	if got := SpillDirFromContext(ctx); got != "" {
		t.Fatalf("empty dir should not be stored, got %q", got)
	}
}
