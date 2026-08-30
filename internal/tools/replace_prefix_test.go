package tools

import "testing"

func TestReplacePrefixReplacesWholeNamespace(t *testing.T) {
	r := NewRegistry()
	oldA := &fakeTool{name: "mcp__srv__a"}
	oldRemoved := &fakeTool{name: "mcp__srv__removed"}
	other := &fakeTool{name: "Read"}
	r.Register(oldA)
	r.Register(oldRemoved)
	r.Register(other)

	newA := &fakeTool{name: "mcp__srv__a"}
	newB := &fakeTool{name: "mcp__srv__b"}
	r.ReplacePrefix("mcp__srv__", []Tool{newA, newB})

	gotA, ok := r.Get("mcp__srv__a")
	if !ok || gotA != newA {
		t.Fatalf("replacement a = %#v, present=%v", gotA, ok)
	}
	if _, ok := r.Get("mcp__srv__removed"); ok {
		t.Fatal("removed server tool survived namespace replacement")
	}
	if _, ok := r.Get("mcp__srv__b"); !ok {
		t.Fatal("new server tool was not installed")
	}
	if _, ok := r.Get("Read"); !ok {
		t.Fatal("unrelated tool was removed")
	}
}

func TestReplacePrefixEmptyPrefixIsNoop(t *testing.T) {
	r := NewRegistry()
	original := &fakeTool{name: "Read"}
	r.Register(original)

	r.ReplacePrefix("", []Tool{&fakeTool{name: "replacement"}})

	got, ok := r.Get("Read")
	if !ok || got != original {
		t.Fatalf("empty-prefix replacement changed registry: got %#v, present=%v", got, ok)
	}
	if _, ok := r.Get("replacement"); ok {
		t.Fatal("empty-prefix replacement installed a replacement")
	}
}

func TestReplacePrefixDuplicateReplacementUsesLastValue(t *testing.T) {
	r := NewRegistry()
	first := &fakeTool{name: "mcp__srv__a"}
	last := &fakeTool{name: "mcp__srv__a"}

	r.ReplacePrefix("mcp__srv__", []Tool{first, last})

	got, ok := r.Get("mcp__srv__a")
	if !ok || got != last {
		t.Fatalf("duplicate replacement = %#v, present=%v; want final value %#v", got, ok, last)
	}
}
