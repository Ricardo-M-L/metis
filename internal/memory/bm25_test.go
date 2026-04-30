package memory

import (
	"strings"
	"testing"
)

func TestBM25_RanksOnTokenOverlap(t *testing.T) {
	docs := []*BM25Doc{
		NewBM25Doc("a", "the quick brown fox jumps over the lazy dog"),
		NewBM25Doc("b", "an entirely different sentence about cats"),
		NewBM25Doc("c", "another fox sighting near the river"),
	}
	got := BM25Rank("fox", docs)
	if len(got) != 2 {
		t.Fatalf("expected 2 hits (a and c), got %d", len(got))
	}
	if got[0].ID != "a" && got[0].ID != "c" {
		t.Errorf("top hit should be a or c, got %s", got[0].ID)
	}
	if got[0].Score <= got[1].Score && got[0].Score != got[1].Score {
		t.Errorf("ranking not score-desc: %+v", got)
	}
}

func TestBM25_LengthNormalization(t *testing.T) {
	// A short doc with a single hit should outrank a long doc that just
	// happens to mention the same term once. (TF-IDF without normalization
	// would tie them; BM25 favors the shorter one.)
	short := NewBM25Doc("short", "fox runs")
	long := NewBM25Doc("long",
		strings.Repeat("the quick brown dog leaps over hills and valleys ", 50)+" fox")
	got := BM25Rank("fox", []*BM25Doc{short, long})
	if len(got) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(got))
	}
	if got[0].ID != "short" {
		t.Errorf("short doc should outrank long doc; got %v", got)
	}
}

func TestBM25_EmptyQuery(t *testing.T) {
	docs := []*BM25Doc{NewBM25Doc("a", "anything")}
	if got := BM25Rank("", docs); got != nil {
		t.Errorf("empty query should yield nil, got %v", got)
	}
}

func TestBM25_NoOverlapDropsDoc(t *testing.T) {
	docs := []*BM25Doc{
		NewBM25Doc("a", "alpha beta"),
		NewBM25Doc("b", "gamma delta"),
	}
	got := BM25Rank("alpha", docs)
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("expected only doc a, got %+v", got)
	}
}

func TestBM25_RareTermBeatsCommon(t *testing.T) {
	// IDF: a term appearing in every doc carries near-zero weight; a rare
	// term carries a large weight.
	docs := []*BM25Doc{
		NewBM25Doc("a", "common common common rare"),
		NewBM25Doc("b", "common common common common"),
		NewBM25Doc("c", "common common common common"),
	}
	got := BM25Rank("rare common", docs)
	if len(got) == 0 {
		t.Fatal("expected at least one hit")
	}
	if got[0].ID != "a" {
		t.Errorf("doc with rare term should win; got %v", got)
	}
}

func TestArchivalSearch_RelevanceFindsTokenMatch(t *testing.T) {
	dir := t.TempDir()
	am, err := NewArchivalMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Three passages; only the second uses the literal query token.
	for _, c := range []string{
		"alpha beta",
		"gamma delta epsilon",
		"omicron pi rho",
	} {
		am.Insert(Passage{Content: c})
	}
	res, err := am.Search(SearchOptions{Query: "delta", SortBy: "relevance"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Content != "gamma delta epsilon" {
		t.Errorf("relevance search miss; got %+v", res)
	}
}

func TestArchivalSearch_RecentPreservesSubstringFilter(t *testing.T) {
	dir := t.TempDir()
	am, _ := NewArchivalMemory(dir)
	for _, c := range []string{"alpha", "alpha beta", "beta only"} {
		am.Insert(Passage{Content: c})
	}
	// SortBy="recent" keeps the substring filter; "beta only" + "alpha beta" pass.
	res, err := am.Search(SearchOptions{Query: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("expected 2 substring hits, got %d (%+v)", len(res), res)
	}
}

func TestArchivalSearch_TagFilter(t *testing.T) {
	dir := t.TempDir()
	am, _ := NewArchivalMemory(dir)
	am.Insert(Passage{Content: "x", Tags: []string{"red"}})
	am.Insert(Passage{Content: "y", Tags: []string{"blue"}})
	res, _ := am.Search(SearchOptions{Tags: []string{"red"}})
	if len(res) != 1 || res[0].Content != "x" {
		t.Errorf("tag filter wrong: %+v", res)
	}
}

// TestBM25F_TagBoostOutweighsBodyMatch locks in the field weighting:
// when the query term appears in tags of doc-A but only in the body of
// doc-B, doc-A should rank higher even if doc-B has more body
// occurrences. Mirrors Robertson 2005's "field weight"
// preference for curated metadata.
func TestBM25F_TagBoostOutweighsBodyMatch(t *testing.T) {
	docs := []*BM25Doc{
		// Body mentions "auth" once, NO tag.
		NewBM25FDoc("body-only", "the user fixed an auth issue last week", nil),
		// Body has "auth" once too, but ALSO tagged "auth".
		NewBM25FDoc("tagged", "the user fixed an auth issue last week", []string{"auth"}),
	}
	got := BM25FRank("auth", docs)
	if len(got) < 2 {
		t.Fatalf("expected both docs to score, got %d", len(got))
	}
	if got[0].ID != "tagged" {
		t.Errorf("BM25F should rank the tagged doc first; got order %v", got)
	}
}

// TestBM25F_BackwardCompatibleWithoutTags verifies that NewBM25Doc
// (single-field, no tags) gives the same ranking under BM25FRank as
// it would under the legacy BM25Rank. Important so existing call-
// sites that construct docs with NewBM25Doc don't silently change
// rankings when we route them through BM25FRank.
func TestBM25F_BackwardCompatibleWithoutTags(t *testing.T) {
	docsLegacy := []*BM25Doc{
		NewBM25Doc("a", "alpha beta gamma"),
		NewBM25Doc("b", "delta beta epsilon"),
		NewBM25Doc("c", "no overlap here"),
	}
	docsF := []*BM25Doc{
		NewBM25Doc("a", "alpha beta gamma"),
		NewBM25Doc("b", "delta beta epsilon"),
		NewBM25Doc("c", "no overlap here"),
	}
	legacy := BM25Rank("beta", docsLegacy)
	bm25f := BM25FRank("beta", docsF)
	if len(legacy) != len(bm25f) {
		t.Fatalf("hit count differs: legacy=%d bm25f=%d", len(legacy), len(bm25f))
	}
	for i := range legacy {
		if legacy[i].ID != bm25f[i].ID {
			t.Errorf("ranking differs at position %d: legacy=%s bm25f=%s",
				i, legacy[i].ID, bm25f[i].ID)
		}
	}
}
