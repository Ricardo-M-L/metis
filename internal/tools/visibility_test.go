package tools

// visibility_test.go pins the global tool-pool filter (allow/disallow
// with MCP server-prefix matching) introduced 2026-05-14. The matcher
// has three pattern shapes (exact, "mcp__server" prefix, "mcp__" full
// wildcard) and must compose allow-then-disallow correctly.

import (
	"reflect"
	"sort"
	"testing"
)

// Re-uses the package-local fakeTool defined in registry_sort_test.go
// (same package, same _test build target). Visibility tests only need
// Name(); the other Tool methods are stubbed there already.

func newRegWith(names ...string) *Registry {
	r := NewRegistry()
	for _, n := range names {
		r.Register(fakeTool{name: n})
	}
	return r
}

func registryNames(r *Registry) []string {
	all := r.All()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name())
	}
	sort.Strings(out)
	return out
}

func TestExpandToolPatterns_Exact(t *testing.T) {
	r := newRegWith("Bash", "Read", "Edit")
	got := ExpandToolPatterns(r, []string{"Bash", "Read"})
	want := map[string]struct{}{"Bash": {}, "Read": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exact match: got %v want %v", got, want)
	}
}

func TestExpandToolPatterns_MCPServerPrefix(t *testing.T) {
	r := newRegWith("Bash", "mcp__office-word__create_document", "mcp__office-word__add_paragraph", "mcp__playwright__navigate")
	got := ExpandToolPatterns(r, []string{"mcp__office-word"})
	want := map[string]struct{}{
		"mcp__office-word__create_document": {},
		"mcp__office-word__add_paragraph":   {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("server prefix: got %v want %v", got, want)
	}
}

func TestExpandToolPatterns_MCPWildcard(t *testing.T) {
	r := newRegWith("Bash", "mcp__a__one", "mcp__b__two", "Read")
	for _, wildcard := range []string{"mcp__", "mcp__*"} {
		got := ExpandToolPatterns(r, []string{wildcard})
		want := map[string]struct{}{
			"mcp__a__one": {},
			"mcp__b__two": {},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("wildcard %q: got %v want %v", wildcard, got, want)
		}
	}
}

func TestExpandToolPatterns_UnknownPatternsDropped(t *testing.T) {
	r := newRegWith("Bash", "Read")
	got := ExpandToolPatterns(r, []string{"NonExistent", "mcp__ghost", "Bash"})
	want := map[string]struct{}{"Bash": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unknowns: got %v want %v", got, want)
	}
}

func TestExpandToolPatterns_EmptyInputs(t *testing.T) {
	r := newRegWith("Bash", "Read")
	if got := ExpandToolPatterns(r, nil); len(got) != 0 {
		t.Errorf("nil patterns: got %v want empty", got)
	}
	if got := ExpandToolPatterns(r, []string{"", "  "}); len(got) != 0 {
		t.Errorf("blank patterns: got %v want empty", got)
	}
	if got := ExpandToolPatterns(nil, []string{"Bash"}); len(got) != 0 {
		t.Errorf("nil registry: got %v want empty", got)
	}
}

func TestApplyToolVisibility_NoOpWhenBothEmpty(t *testing.T) {
	r := newRegWith("Bash", "Read", "Edit")
	ApplyToolVisibility(r, nil, nil)
	if got := registryNames(r); !reflect.DeepEqual(got, []string{"Bash", "Edit", "Read"}) {
		t.Errorf("registry must be untouched; got %v", got)
	}
}

func TestApplyToolVisibility_AllowlistShrinks(t *testing.T) {
	r := newRegWith("Bash", "Read", "Edit", "WebFetch")
	ApplyToolVisibility(r, []string{"Read", "Edit"}, nil)
	if got := registryNames(r); !reflect.DeepEqual(got, []string{"Edit", "Read"}) {
		t.Errorf("allowlist: got %v want [Edit Read]", got)
	}
}

func TestApplyToolVisibility_DisallowSubtracts(t *testing.T) {
	r := newRegWith("Bash", "Read", "Edit", "WebFetch")
	ApplyToolVisibility(r, nil, []string{"WebFetch"})
	if got := registryNames(r); !reflect.DeepEqual(got, []string{"Bash", "Edit", "Read"}) {
		t.Errorf("disallow: got %v", got)
	}
}

func TestApplyToolVisibility_AllowThenDisallow(t *testing.T) {
	r := newRegWith("Bash", "Read", "Edit", "WebFetch")
	ApplyToolVisibility(r, []string{"Read", "Edit", "Bash"}, []string{"Bash"})
	if got := registryNames(r); !reflect.DeepEqual(got, []string{"Edit", "Read"}) {
		t.Errorf("allow then disallow: got %v want [Edit Read]", got)
	}
}

func TestApplyToolVisibility_MCPServerWipe(t *testing.T) {
	r := newRegWith("Bash", "Read", "mcp__office-word__create_document", "mcp__office-word__add_paragraph", "mcp__playwright__navigate")
	ApplyToolVisibility(r, nil, []string{"mcp__office-word"})
	got := registryNames(r)
	want := []string{"Bash", "Read", "mcp__playwright__navigate"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mcp server wipe: got %v want %v", got, want)
	}
}

func TestApplyToolVisibility_MCPWildcardWipesAllMCP(t *testing.T) {
	r := newRegWith("Bash", "mcp__a__one", "mcp__b__two", "mcp__c__three")
	ApplyToolVisibility(r, nil, []string{"mcp__"})
	got := registryNames(r)
	if !reflect.DeepEqual(got, []string{"Bash"}) {
		t.Errorf("mcp wildcard: got %v want [Bash]", got)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Bash", []string{"Bash"}},
		{"Bash,Read,Edit", []string{"Bash", "Read", "Edit"}},
		{"Bash Read Edit", []string{"Bash", "Read", "Edit"}}, // claude-code accepts space-separated too
		{"Bash, Read , Edit ", []string{"Bash", "Read", "Edit"}},
		{",,Bash,,", []string{"Bash"}},
		{"mcp__office-word, Bash", []string{"mcp__office-word", "Bash"}},
	}
	for _, c := range cases {
		got := SplitCSV(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitCSV(%q) = %v want %v", c.in, got, c.want)
		}
	}
}
