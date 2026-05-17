package agent

// auto_retrieve_rerank_test.go — pins the 2026-05-15 LLM-rerank path
// for AutoRetrieve. See auto_retrieve_rerank.go for design.

import (
	"context"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

// fakeRerankProvider lets us stub the Complete() reply and verify
// rerankAutoRetrieve calls Complete with the right shape.
type fakeRerankProvider struct {
	reply      string
	err        error
	gotRequest *llm.Request
	maxCtx     int
}

func (p *fakeRerankProvider) Name() string          { return "fake-rerank" }
func (p *fakeRerankProvider) MaxContextTokens() int { return p.maxCtx }
func (p *fakeRerankProvider) ModelID() string       { return "" }
func (p *fakeRerankProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.gotRequest = &req
	if p.err != nil {
		return nil, p.err
	}
	return &llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: p.reply}},
	}, nil
}
func (p *fakeRerankProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("rerank should use Complete, not Stream")
}

func makePassages(n int) []memory.Passage {
	out := make([]memory.Passage, n)
	for i := 0; i < n; i++ {
		out[i] = memory.Passage{
			ID:      "p" + string(rune('a'+i)),
			Content: "passage about topic " + string(rune('a'+i)),
		}
	}
	return out
}

func TestRerankAutoRetrieve_HappyPath(t *testing.T) {
	t.Parallel()
	candidates := makePassages(6) // pa..pf
	prov := &fakeRerankProvider{reply: "[3, 1, 5]"}
	got := rerankAutoRetrieve(context.Background(), prov, "test query", candidates, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 picks; got %d", len(got))
	}
	// Order should match the model's pick: 3, 1, 5 → pd, pb, pf.
	wantIDs := []string{"pd", "pb", "pf"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("pick[%d] = %s, want %s", i, got[i].ID, want)
		}
	}
	// Verify provider was called with a sensible request shape.
	if prov.gotRequest == nil {
		t.Fatal("provider not called")
	}
	if prov.gotRequest.MaxTokens == 0 {
		t.Error("MaxTokens should be set (small)")
	}
	if len(prov.gotRequest.Messages) == 0 {
		t.Error("provider got empty messages")
	}
}

func TestRerankAutoRetrieve_FallbackOnProviderError(t *testing.T) {
	t.Parallel()
	candidates := makePassages(6)
	prov := &fakeRerankProvider{err: errors.New("provider down")}
	got := rerankAutoRetrieve(context.Background(), prov, "q", candidates, 3)
	if len(got) != 3 {
		t.Errorf("provider error should fall back to BM25 top-K; got %d picks", len(got))
	}
	// BM25 fallback preserves input order: first 3 → pa, pb, pc.
	if got[0].ID != "pa" || got[1].ID != "pb" || got[2].ID != "pc" {
		t.Errorf("BM25 fallback order wrong: %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestRerankAutoRetrieve_FallbackOnGarbageResponse(t *testing.T) {
	t.Parallel()
	candidates := makePassages(6)
	prov := &fakeRerankProvider{reply: "I think the most relevant ones are passage A and C, etc."}
	got := rerankAutoRetrieve(context.Background(), prov, "q", candidates, 3)
	if len(got) != 3 {
		t.Errorf("garbage reply should fall back; got %d picks", len(got))
	}
	if got[0].ID != "pa" {
		t.Errorf("expected BM25 fallback (pa first); got %s", got[0].ID)
	}
}

func TestRerankAutoRetrieve_NilProvider(t *testing.T) {
	t.Parallel()
	candidates := makePassages(5)
	got := rerankAutoRetrieve(context.Background(), nil, "q", candidates, 2)
	if len(got) != 2 {
		t.Errorf("nil provider should return BM25 top-K; got %d", len(got))
	}
	if got[0].ID != "pa" || got[1].ID != "pb" {
		t.Errorf("BM25 order wrong: %v", []string{got[0].ID, got[1].ID})
	}
}

func TestRerankAutoRetrieve_NoCandidates(t *testing.T) {
	t.Parallel()
	prov := &fakeRerankProvider{reply: "[0]"}
	got := rerankAutoRetrieve(context.Background(), prov, "q", nil, 3)
	if got != nil {
		t.Errorf("empty candidates should return nil; got %v", got)
	}
}

func TestRerankAutoRetrieve_FewerCandidatesThanK(t *testing.T) {
	t.Parallel()
	candidates := makePassages(2)
	prov := &fakeRerankProvider{reply: "[1, 0]"}
	got := rerankAutoRetrieve(context.Background(), prov, "q", candidates, 5)
	// Skip rerank when len(candidates) <= k — return all of them.
	if len(got) != 2 {
		t.Errorf("with %d candidates and k=5, expected all 2 unranked; got %d", len(candidates), len(got))
	}
	if prov.gotRequest != nil {
		t.Errorf("provider should NOT be called when nothing to rerank; got request anyway")
	}
}

func TestParseRerankIndices_PlainArray(t *testing.T) {
	t.Parallel()
	got, ok := parseRerankIndices("[2, 0, 1]", 5)
	if !ok || len(got) != 3 || got[0] != 2 || got[1] != 0 || got[2] != 1 {
		t.Errorf("plain array parse wrong: ok=%v got=%v", ok, got)
	}
}

func TestParseRerankIndices_StripsCodeFence(t *testing.T) {
	t.Parallel()
	got, ok := parseRerankIndices("```json\n[3, 1]\n```", 5)
	if !ok || len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Errorf("code-fence parse wrong: ok=%v got=%v", ok, got)
	}
}

func TestParseRerankIndices_TolerantOfSurroundingProse(t *testing.T) {
	t.Parallel()
	got, ok := parseRerankIndices("Top picks: [4, 2, 0] (great matches)", 5)
	if !ok || len(got) != 3 {
		t.Errorf("prose-padded parse wrong: ok=%v got=%v", ok, got)
	}
}

func TestParseRerankIndices_DropsOutOfRange(t *testing.T) {
	t.Parallel()
	got, ok := parseRerankIndices("[2, 99, -1, 0]", 5)
	if !ok {
		t.Fatalf("expected ok=true; got false")
	}
	// 99 and -1 dropped; 2 and 0 kept (in input order).
	if len(got) != 2 || got[0] != 2 || got[1] != 0 {
		t.Errorf("range filter wrong: %v", got)
	}
}

func TestParseRerankIndices_DropsDuplicates(t *testing.T) {
	t.Parallel()
	got, _ := parseRerankIndices("[1, 1, 2, 1]", 5)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("dedup wrong: %v", got)
	}
}

func TestParseRerankIndices_RejectsNonArray(t *testing.T) {
	t.Parallel()
	if _, ok := parseRerankIndices("not json at all", 5); ok {
		t.Error("non-array should return ok=false")
	}
	if _, ok := parseRerankIndices("", 5); ok {
		t.Error("empty input should return ok=false")
	}
	if _, ok := parseRerankIndices("[]", 5); ok {
		t.Error("empty array should return ok=false")
	}
}
