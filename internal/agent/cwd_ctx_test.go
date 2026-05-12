package agent

// cwd_ctx_test.go pins the context-based cwd plumbing added in
// Phase G.2 (2026-05-12). WithCwd / CwdFromContext is the only
// channel sub-agent isolation has for telling tools (Bash, future
// LS/Glob) about a per-invocation working directory; if this regresses
// the `cwd:"..."` schema field silently does nothing.

import (
	"context"
	"testing"
)

// TestWithCwd_RoundTrip — basic set + read. The simplest contract:
// what goes in comes out, no string mangling.
func TestWithCwd_RoundTrip(t *testing.T) {
	ctx := WithCwd(context.Background(), "/tmp/sub-agent-cwd")
	if got := CwdFromContext(ctx); got != "/tmp/sub-agent-cwd" {
		t.Errorf("CwdFromContext = %q, want /tmp/sub-agent-cwd", got)
	}
}

// TestWithCwd_EmptyIsNoOp — empty string MUST NOT shadow a parent
// cwd value. Otherwise a sub-sub-agent that didn't pass cwd would
// silently clobber its parent's cwd context. Callers rely on
// `WithCwd(ctx, "")` being safe to call unconditionally.
func TestWithCwd_EmptyIsNoOp(t *testing.T) {
	parent := WithCwd(context.Background(), "/tmp/parent")
	child := WithCwd(parent, "")
	if got := CwdFromContext(child); got != "/tmp/parent" {
		t.Errorf("WithCwd(ctx, \"\") should be no-op; got %q, want /tmp/parent", got)
	}
}

// TestCwdFromContext_DefaultIsEmpty — when no cwd was attached,
// readers must see "" so they fall back to os.Getwd(). If this
// returned a non-empty default the parent's cwd would leak in
// unexpected ways.
func TestCwdFromContext_DefaultIsEmpty(t *testing.T) {
	if got := CwdFromContext(context.Background()); got != "" {
		t.Errorf("CwdFromContext on bare ctx should be \"\"; got %q", got)
	}
}

// TestWithCwd_ChildOverridesParent — a sub-sub-agent that wants a
// different cwd MUST be able to shadow its parent's value. This is
// the standard context.WithValue layering — verify our wrapper
// preserves it.
func TestWithCwd_ChildOverridesParent(t *testing.T) {
	parent := WithCwd(context.Background(), "/tmp/parent")
	child := WithCwd(parent, "/tmp/child")
	if got := CwdFromContext(child); got != "/tmp/child" {
		t.Errorf("child cwd should override parent; got %q, want /tmp/child", got)
	}
	// And the parent's view must be unaffected — context immutability.
	if got := CwdFromContext(parent); got != "/tmp/parent" {
		t.Errorf("parent ctx must be unchanged; got %q", got)
	}
}
