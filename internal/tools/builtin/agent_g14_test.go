package builtin

// agent_g14_test.go — locks Phase G.14 (2026-05-12) per-invocation
// tool filter contract on the Agent tool.
//
// Six contracts:
//
//   1. No allow/deny args — filterRegistry keeps everything (caller's
//      precondition: skip filterRegistry when both are empty).
//   2. allowed_tools narrows the sub-agent's registry.
//   3. disallowed_tools removes tools after the allow intersection.
//   4. allow + deny together — final list is allow minus deny.
//   5. stringSliceArg tolerates []any from JSON decoders + []string
//      from native callers + empty string filtering.
//   6. filterRegistry preserves parent insertion order.

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// stubTool implements tools.Tool with a configurable name. Used to
// populate a parent registry so we can inspect what the filter
// keeps.
type stubTool struct{ name string }

func (s stubTool) Name() string                                 { return s.name }
func (s stubTool) Description() string                          { return "stub " + s.name }
func (s stubTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (s stubTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }
func (s stubTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (s stubTool) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

func buildParentRegistry(t *testing.T, names ...string) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry()
	for _, n := range names {
		r.Register(stubTool{name: n})
	}
	return r
}

func registryNames(r *tools.Registry) []string {
	all := r.All()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name())
	}
	return out
}

func TestFilterRegistry_NoFiltersKeepsAll(t *testing.T) {
	t.Parallel()
	parent := buildParentRegistry(t, "Read", "Write")
	got := filterRegistry(parent, nil, nil)
	if names := registryNames(got); len(names) != 2 {
		t.Errorf("with no filters, filterRegistry should keep all (len=%d)", len(names))
	}
}

func TestFilterRegistry_AllowOnly(t *testing.T) {
	t.Parallel()
	parent := buildParentRegistry(t, "Read", "Write", "Bash", "Edit")
	got := filterRegistry(parent, []string{"Read", "Bash"}, nil)
	names := registryNames(got)
	if len(names) != 2 {
		t.Fatalf("allowlist should keep 2; got %v", names)
	}
	for _, want := range []string{"Read", "Bash"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allowlist should keep %q; got %v", want, names)
		}
	}
}

func TestFilterRegistry_DenyOnly(t *testing.T) {
	t.Parallel()
	parent := buildParentRegistry(t, "Read", "Write", "Bash")
	got := filterRegistry(parent, nil, []string{"Bash"})
	names := registryNames(got)
	if len(names) != 2 {
		t.Errorf("denylist should drop 1; got %v", names)
	}
	for _, n := range names {
		if n == "Bash" {
			t.Errorf("denylist should drop Bash; got %v", names)
		}
	}
}

func TestFilterRegistry_AllowAndDeny(t *testing.T) {
	t.Parallel()
	parent := buildParentRegistry(t, "Read", "Write", "Bash", "Edit")
	got := filterRegistry(parent, []string{"Read", "Bash", "Edit"}, []string{"Bash"})
	names := registryNames(got)
	want := map[string]bool{"Read": true, "Edit": true}
	if len(names) != 2 {
		t.Errorf("expected 2 surviving tools; got %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected survivor %q", n)
		}
	}
}

func TestFilterRegistry_OrderPreserved(t *testing.T) {
	t.Parallel()
	parent := buildParentRegistry(t, "Aaa", "Bbb", "Ccc", "Ddd")
	got := filterRegistry(parent, []string{"Ccc", "Aaa"}, nil)
	names := registryNames(got)
	// Allowlist input order doesn't matter; parent insertion order does.
	if len(names) != 2 || names[0] != "Aaa" || names[1] != "Ccc" {
		t.Errorf("order should follow parent (Aaa, Ccc); got %v", names)
	}
}

func TestStringSliceArg_HandlesNativeAndJSON(t *testing.T) {
	t.Parallel()
	// JSON-decoded array surfaces as []any.
	out := stringSliceArg(map[string]any{"k": []any{"a", "b", "c"}}, "k")
	if len(out) != 3 || out[0] != "a" || out[2] != "c" {
		t.Errorf("[]any path returned %v", out)
	}
	// Native []string also works.
	out = stringSliceArg(map[string]any{"k": []string{"x", "y"}}, "k")
	if len(out) != 2 || out[0] != "x" {
		t.Errorf("[]string path returned %v", out)
	}
	// Missing key returns nil.
	out = stringSliceArg(map[string]any{}, "k")
	if out != nil {
		t.Errorf("missing key should return nil; got %v", out)
	}
	// Empty strings are dropped.
	out = stringSliceArg(map[string]any{"k": []any{"", "a", ""}}, "k")
	if len(out) != 1 || out[0] != "a" {
		t.Errorf("empty strings should be dropped; got %v", out)
	}
}

// TestAgentExecute_AllowedToolsSchemaPath — end-to-end: spawn the
// Agent tool with allowed_tools restricting the sub-agent. Since
// helloProvider doesn't make tool calls, the only observable effect
// is that the sub-loop runs cleanly with the schema fields parsed.
func TestAgentExecute_AllowedToolsSchemaPath(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	reg.Register(stubTool{name: "Read"})
	reg.Register(stubTool{name: "Bash"})
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), reg, "m", "s").
		WithRoster(agent.NewRoster(0))
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":           "x",
		"allowed_tools":    []any{"Read"},
		"disallowed_tools": []any{"Bash"},
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("schema-path Execute should not be IsError: %s", res.Output)
	}
}
