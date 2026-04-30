package skill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeSource is the minimum implementation a plugin author would write
// to register an alternative skill registry (e.g. "fetch from internal
// HTTPS server"). Used in tests both to verify the public interface
// compiles in isolation and to document the contract.
type fakeSource struct {
	name string
	want *Skill
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Fetch(_ context.Context, _ string) (*Skill, error) {
	clone := *f.want
	clone.Source = f.name
	return &clone, nil
}

// fakeSearcher exercises the optional Searcher interface.
type fakeSearcher struct {
	*fakeSource
	hits []SearchHit
}

func (f *fakeSearcher) Search(_ context.Context, _ string, _ int) ([]SearchHit, error) {
	return f.hits, nil
}

func TestSkill_JSONRoundTrip(t *testing.T) {
	in := Skill{
		Name:        "summarize",
		Description: "Summarize the user input",
		Category:    "writing",
		Prompt:      "Summarize concisely:",
		Tools:       []string{"Read"},
		Tags:        []string{"writing", "demo"},
		Uses:        3,
		ContentHash: "abc123",
		Source:      "github:foo/bar:summarize",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Skill
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.Description != in.Description ||
		out.ContentHash != in.ContentHash || out.Source != in.Source {
		t.Errorf("round trip mismatch: %+v vs %+v", in, out)
	}
}

func TestSkill_OmitemptyFields(t *testing.T) {
	// Empty optional fields should not appear in the JSON — keeps
	// disk format clean for hand-edited skills.
	b, _ := json.Marshal(Skill{Name: "n"})
	if !strings.Contains(string(b), `"name":"n"`) {
		t.Errorf("name missing: %s", b)
	}
	if strings.Contains(string(b), "category") {
		t.Errorf("empty category should be omitted: %s", b)
	}
	if strings.Contains(string(b), "content_hash") {
		t.Errorf("empty content_hash should be omitted: %s", b)
	}
}

func TestSourceInterface_PluginCompiles(t *testing.T) {
	// Static check that fakeSource satisfies Source. If the public
	// interface drifts, this fails to compile — exactly the canary we
	// want for an SDK contract test.
	var _ Source = &fakeSource{}
}

func TestSearcherInterface_PluginCompiles(t *testing.T) {
	var _ Searcher = &fakeSearcher{}
}

func TestSource_Fetch(t *testing.T) {
	src := &fakeSource{
		name: "test",
		want: &Skill{Name: "demo", Description: "d"},
	}
	got, err := src.Fetch(context.Background(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "demo" {
		t.Errorf("expected demo skill; got %+v", got)
	}
	if got.Source != "test" {
		t.Errorf("Fetch should stamp Source; got %q", got.Source)
	}
}

func TestSearcher_Search(t *testing.T) {
	hits := []SearchHit{
		{Source: "test", Ref: "foo/bar:x", Name: "x", Description: "d"},
		{Source: "test", Ref: "foo/baz:y", Name: "y"},
	}
	sr := &fakeSearcher{
		fakeSource: &fakeSource{name: "test"},
		hits:       hits,
	}
	got, err := sr.Search(context.Background(), "anything", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	if got[0].Ref != "foo/bar:x" {
		t.Errorf("hit[0].Ref = %q", got[0].Ref)
	}
}
