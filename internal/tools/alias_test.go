package tools

import (
	"context"
	"testing"
)

type aliasedTool struct {
	BaseTool
	name    string
	aliases []string
}

func (a aliasedTool) Name() string                                { return a.name }
func (a aliasedTool) Description() string                         { return "aliased stub" }
func (a aliasedTool) InputSchema() map[string]any                 { return map[string]any{"type": "object"} }
func (a aliasedTool) Concurrency(map[string]any) Concurrency      { return ConcurrencySafe }
func (a aliasedTool) Aliases() []string                           { return a.aliases }
func (a aliasedTool) CanUse(context.Context, map[string]any) (Permission, string) {
	return PermissionAllow, ""
}
func (a aliasedTool) Execute(context.Context, map[string]any) (*Result, error) {
	return &Result{Output: "ok"}, nil
}

func TestGetResolvesAliases(t *testing.T) {
	r := NewRegistry()
	r.Register(aliasedTool{name: "NewName", aliases: []string{"OldName", "LegacyName"}})

	if _, ok := r.Get("NewName"); !ok {
		t.Fatal("canonical name must resolve")
	}
	got, ok := r.Get("OldName")
	if !ok {
		t.Fatal("alias OldName must resolve")
	}
	if got.Name() != "NewName" {
		t.Fatalf("alias resolved to %q, want NewName", got.Name())
	}
	if _, ok := r.Get("LegacyName"); !ok {
		t.Fatal("alias LegacyName must resolve")
	}
	if _, ok := r.Get("NoSuchName"); ok {
		t.Fatal("unknown name must not resolve")
	}
}

// A real tool name shadows any alias claiming the same string.
func TestRealNameWinsOverAlias(t *testing.T) {
	r := NewRegistry()
	r.Register(aliasedTool{name: "Thief", aliases: []string{"Victim"}})
	r.Register(aliasedTool{name: "Victim"})

	got, ok := r.Get("Victim")
	if !ok {
		t.Fatal("Victim must resolve")
	}
	if got.Name() != "Victim" {
		t.Fatalf("Get(Victim) resolved to %q via alias; real name must win", got.Name())
	}
}

// Aliases never leak into the LLM-facing listings.
func TestAliasesAbsentFromAll(t *testing.T) {
	r := NewRegistry()
	r.Register(aliasedTool{name: "Canonical", aliases: []string{"Shadow"}})
	for _, tl := range r.All() {
		if tl.Name() == "Shadow" {
			t.Fatal("alias appeared in All()")
		}
	}
	if n := len(r.All()); n != 1 {
		t.Fatalf("All() len = %d, want 1", n)
	}
}

// Replace with a version that DROPPED an alias must stop the stale
// alias from resolving (2026-06-12 review: indexAliases was add-only).
func TestReplaceDropsStaleAlias(t *testing.T) {
	r := NewRegistry()
	r.Register(aliasedTool{name: "Foo", aliases: []string{"Bar"}})
	if _, ok := r.Get("Bar"); !ok {
		t.Fatal("alias Bar should resolve after Register")
	}
	r.Replace(aliasedTool{name: "Foo", aliases: nil}) // dropped Bar
	if _, ok := r.Get("Bar"); ok {
		t.Error("alias Bar must stop resolving after Replace dropped it")
	}
}

// Freeing an alias via Replace lets another tool claim it.
func TestReplaceFreesAliasForReclaim(t *testing.T) {
	r := NewRegistry()
	r.Register(aliasedTool{name: "Foo", aliases: []string{"X"}})
	r.Register(aliasedTool{name: "Baz", aliases: []string{"X"}}) // X taken by Foo
	r.Replace(aliasedTool{name: "Foo", aliases: nil})            // Foo frees X
	r.Replace(aliasedTool{name: "Baz", aliases: []string{"X"}})  // Baz re-indexes, claims X
	got, ok := r.Get("X")
	if !ok {
		t.Fatal("X should resolve after Foo freed it")
	}
	if got.Name() != "Baz" {
		t.Errorf("X resolved to %q, want Baz", got.Name())
	}
}
