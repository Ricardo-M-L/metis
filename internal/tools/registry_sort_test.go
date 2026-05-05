package tools

import (
	"context"
	"testing"

	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// fakeTool is the smallest possible Tool for registry tests.
type fakeTool struct{ name string }

func (f fakeTool) Name() string                { return f.name }
func (f fakeTool) Description() string         { return "" }
func (f fakeTool) InputSchema() map[string]any { return nil }
func (f fakeTool) Concurrency(map[string]any) pubtool.Concurrency {
	return pubtool.ConcurrencySafe
}
func (f fakeTool) CanUse(context.Context, map[string]any) (pubtool.Permission, string) {
	return pubtool.PermissionAllow, ""
}
func (f fakeTool) Execute(context.Context, map[string]any) (*pubtool.Result, error) {
	return &pubtool.Result{}, nil
}

// TestSortedForCache_BuiltinsBeforeMcp verifies the cache-stable
// ordering: built-ins sorted by name first, MCP (mcp__-prefixed)
// sorted by name after. This keeps the Anthropic prompt-cache
// breakpoint after the last built-in valid across MCP churn.
func TestSortedForCache_BuiltinsBeforeMcp(t *testing.T) {
	r := NewRegistry()
	// Register in a deliberately unsorted order, mixing MCP + built-in.
	r.Register(fakeTool{name: "Zebra"})
	r.Register(fakeTool{name: "mcp__server__alpha"})
	r.Register(fakeTool{name: "Apple"})
	r.Register(fakeTool{name: "mcp__server__zebra"})
	r.Register(fakeTool{name: "Mango"})

	got := r.SortedForCache()
	wantOrder := []string{
		"Apple", "Mango", "Zebra", // built-ins sorted
		"mcp__server__alpha", "mcp__server__zebra", // MCP sorted
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("len = %d, want %d", len(got), len(wantOrder))
	}
	for i, w := range wantOrder {
		if got[i].Name() != w {
			t.Errorf("position %d: got %q, want %q", i, got[i].Name(), w)
		}
	}
}

// TestSortedForCache_StableUnderMcpChurn verifies that adding /
// removing MCP tools doesn't shift built-in positions.
func TestSortedForCache_StableUnderMcpChurn(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "Edit"})
	r.Register(fakeTool{name: "Read"})
	r.Register(fakeTool{name: "mcp__a__one"})

	before := r.SortedForCache()

	r.Register(fakeTool{name: "mcp__b__two"})
	r.Register(fakeTool{name: "mcp__c__three"})

	after := r.SortedForCache()

	// Built-ins should be identical, sorted: Edit, Read.
	if before[0].Name() != "Edit" || before[1].Name() != "Read" {
		t.Fatalf("before: built-in order broken: %v", before)
	}
	if after[0].Name() != "Edit" || after[1].Name() != "Read" {
		t.Fatalf("after: built-in order broken: %v", after)
	}
	// And the MCP segment should remain alphabetical.
	mcpStart := 2 // first MCP after the 2 built-ins
	if after[mcpStart].Name() != "mcp__a__one" ||
		after[mcpStart+1].Name() != "mcp__b__two" ||
		after[mcpStart+2].Name() != "mcp__c__three" {
		t.Errorf("MCP not sorted alpha: %v", after[mcpStart:])
	}
}

// TestSortedForCache_HelperReExports verifies the package-level
// helper var aliases compile and call through to pkg/tool.
func TestSortedForCache_HelperReExports(t *testing.T) {
	tt := fakeTool{name: "X"}
	if IsReadOnly(tt, nil) != false {
		t.Errorf("default IsReadOnly should be false for fake tool")
	}
	if IsDestructive(tt, nil) != false {
		t.Errorf("default IsDestructive should be false")
	}
	if RequiresUserInteraction(tt) != false {
		t.Errorf("default RequiresUserInteraction should be false")
	}
	if im, _ := IsBypassImmune(tt, nil); im {
		t.Errorf("default IsBypassImmune should be false")
	}
	if GetInterruptBehavior(tt) != InterruptCancel {
		t.Errorf("default InterruptBehavior should be Cancel")
	}
}
