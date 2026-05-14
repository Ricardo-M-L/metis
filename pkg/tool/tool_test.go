package tool

import (
	"context"
	"testing"
)

// fakeTool is a minimal implementation used only to assert that pkg/tool
// satisfies what an external plugin author would need: implementing the
// public interface compiles and behaves predictably without referencing
// any internal package. Embeds BaseTool to inherit the default
// IsEnabled() = true — same pattern external plugin authors should use.
type fakeTool struct {
	BaseTool
	name string
}

func (t fakeTool) Name() string                           { return t.name }
func (t fakeTool) Description() string                    { return "fake" }
func (t fakeTool) InputSchema() map[string]any            { return map[string]any{"type": "object"} }
func (t fakeTool) Concurrency(map[string]any) Concurrency { return ConcurrencySafe }
func (t fakeTool) CanUse(_ context.Context, _ map[string]any) (Permission, string) {
	return PermissionAllow, ""
}
func (t fakeTool) Execute(_ context.Context, _ map[string]any) (*Result, error) {
	return &Result{Output: "ok"}, nil
}

func TestPublicInterfaceImplements(t *testing.T) {
	// This test does double duty: it documents the minimum surface a
	// plugin author needs, AND it verifies the package compiles when
	// imported standalone (i.e. no transitive internal dep accidentally
	// bled into the public API).
	var _ Tool = fakeTool{name: "demo"}
}

func TestPermissionConstants(t *testing.T) {
	if PermissionAsk == PermissionAllow {
		t.Error("PermissionAsk and PermissionAllow must differ")
	}
	if PermissionAllow == PermissionDeny {
		t.Error("PermissionAllow and PermissionDeny must differ")
	}
}

func TestConcurrencyConstants(t *testing.T) {
	if ConcurrencySafe == ConcurrencyExclusive {
		t.Error("ConcurrencySafe and ConcurrencyExclusive must differ")
	}
}

func TestResult_ZeroValueIsUsable(t *testing.T) {
	r := &Result{}
	if r.Output != "" || r.IsError || r.Display != "" || r.Meta != nil {
		t.Errorf("zero-value Result has unexpected fields: %+v", r)
	}
}

func TestExecuteRoundTrip(t *testing.T) {
	tl := fakeTool{name: "demo"}
	res, err := tl.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.Output != "ok" {
		t.Errorf("Execute output = %q, want ok", res.Output)
	}
}
