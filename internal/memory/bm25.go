package memory

import (
	"math"
	"sort"
	"strings"
)

// BM25 is a transitional ranker for archival search. It's the standard
// recipe (k1=1.5, b=0.75) over the existing tokenize() output. We pick this
// over plain TF-IDF for two reasons:
//   1. Length normalization keeps long passages from dominating just for
//      having more total tokens.
//   2. The IDF here uses (N - df + 0.5)/(df + 0.5) smoothing so a term
//      that appears in nearly every document still has a finite weight.
//
// When archival memory grows past ~10k passages or a query needs semantic
// (not lexical) recall, swap this for an embedding index — the surrounding
// ArchivalMemory API doesn't change.

const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// BM25F field weights — Robertson 2005 ("Field-weighted BM25 for XML
// Retrieval"). Tags are typically curated keywords (the user / agent
// thought enough to attach them) so they deserve more weight than
// arbitrary words in the body. Content is the baseline. The third
// channel (label/title) is reserved — Passage.Label doesn't exist
// today but the API leaves room.
//
// Weights tuned for "tags 2x more selective than content" — this is
// the Elasticsearch default tuning that's served the industry well
// for ~15 years. If recall on tag matches feels too aggressive, drop
// weightTags to 1.5; if it feels too soft, push to 2.5.
const (
	weightTags    = 2.0
	weightContent = 1.0
)

// BM25Doc holds the precomputed term frequency table for one passage.
//
// Per-field counts (`contentTF` / `tagsTF`) feed BM25F's weighted-TF
// scoring. The legacy `tokenSet` is the *combined* TF used by the
// vanilla BM25 path (`bm25Score`); it stays populated so old callers
// (and the standalone unit tests) don't have to migrate. New callers
// should prefer `bm25fScore`.
type BM25Doc struct {
	id         string
	tokenSet   map[string]int // combined TF (content + tags merged) — legacy
	contentTF  map[string]int
	tagsTF     map[string]int
	length     int // combined length, for legacy bm25Score
	contentLen int
	tagsLen    int
}

// NewBM25Doc builds a doc with all text in one bag — use this for free-
// form text where there's no field structure (e.g. raw memory dumps).
// Equivalent to BM25 (not BM25F) since contentTF == tokenSet.
func NewBM25Doc(id, text string) *BM25Doc {
	toks := tokenize(text)
	set := make(map[string]int, len(toks))
	for _, t := range toks {
		set[t]++
	}
	return &BM25Doc{
		id:         id,
		tokenSet:   set,
		contentTF:  set,
		length:     len(toks),
		contentLen: len(toks),
	}
}

// NewBM25FDoc builds a multi-field doc: `text` is the body, `tags` is
// the (typically) shorter keyword list. Tag tokens are weighted up at
// score time, mirroring Elasticsearch's default "tags ~2× more
// influential than body" tuning.
func NewBM25FDoc(id, text string, tags []string) *BM25Doc {
	contentToks := tokenize(text)
	contentSet := make(map[string]int, len(contentToks))
	for _, t := range contentToks {
		contentSet[t]++
	}
	// Tags get tokenized as if joined — mainly so multi-word tags
	// ("auth-bug", "memory leak") don't lose their components.
	tagsText := ""
	if len(tags) > 0 {
		tagsText = " " + strings.Join(tags, " ")
	}
	tagsToks := tokenize(tagsText)
	tagsSet := make(map[string]int, len(tagsToks))
	for _, t := range tagsToks {
		tagsSet[t]++
	}
	// Combined set for the legacy bm25Score path.
	combined := make(map[string]int, len(contentSet)+len(tagsSet))
	for k, v := range contentSet {
		combined[k] += v
	}
	for k, v := range tagsSet {
		combined[k] += v
	}
	return &BM25Doc{
		id:         id,
		tokenSet:   combined,
		contentTF:  contentSet,
		tagsTF:     tagsSet,
		length:     len(contentToks) + len(tagsToks),
		contentLen: len(contentToks),
		tagsLen:    len(tagsToks),
	}
}

// computeIDF builds the IDF table for the corpus.
func computeIDF(docs []*BM25Doc) map[string]float64 {
	df := make(map[string]int)
	for _, d := range docs {
		for term := range d.tokenSet {
			df[term]++
		}
	}
	n := float64(len(docs))
	idf := make(map[string]float64, len(df))
	for term, freq := range df {
		idf[term] = math.Log(1 + (n-float64(freq)+0.5)/(float64(freq)+0.5))
	}
	return idf
}

func avgDocLen(docs []*BM25Doc) float64 {
	if len(docs) == 0 {
		return 0
	}
	total := 0
	for _, d := range docs {
		total += d.length
	}
	return float64(total) / float64(len(docs))
}

// bm25Score returns the BM25 score for a tokenized query against one doc.
func bm25Score(query []string, doc *BM25Doc, idf map[string]float64, avgdl float64) float64 {
	if doc.length == 0 || avgdl == 0 {
		return 0
	}
	var score float64
	dl := float64(doc.length)
	for _, q := range query {
		f := float64(doc.tokenSet[q])
		if f == 0 {
			continue
		}
		score += idf[q] * (f * (bm25K1 + 1)) /
			(f + bm25K1*(1-bm25B+bm25B*dl/avgdl))
	}
	return score
}

// BM25Result is one ranked hit. Score is the raw BM25 score; comparable
// only within the same call.
type BM25Result struct {
	ID    string
	Score float64
}

// bm25fScore is the field-weighted variant. The standard derivation
// (Robertson 2005) computes a virtual weighted TF:
//
//	tf_weighted(t, d) = sum over fields f of  weight_f * tf(t, f, d)
//
// Then plugs that into the classic BM25 formula along with a virtual
// weighted document length. We give tags ~2× the importance of
// content, matching the Elasticsearch default. Length normalization
// uses avgdl_content as the reference (tags are short by design and
// would otherwise dominate the b·dl/avgdl penalty).
func bm25fScore(query []string, doc *BM25Doc, idf map[string]float64, avgdl float64) float64 {
	if doc.contentLen == 0 && doc.tagsLen == 0 || avgdl == 0 {
		return 0
	}
	// Virtual length = sum of weighted field lengths.
	dl := weightContent*float64(doc.contentLen) + weightTags*float64(doc.tagsLen)
	var score float64
	for _, q := range query {
		tfWeighted := weightContent*float64(doc.contentTF[q]) + weightTags*float64(doc.tagsTF[q])
		if tfWeighted == 0 {
			continue
		}
		score += idf[q] * (tfWeighted * (bm25K1 + 1)) /
			(tfWeighted + bm25K1*(1-bm25B+bm25B*dl/avgdl))
	}
	return score
}

// BM25FRank is the field-weighted entry point. Same return shape as
// BM25Rank — drop-in replacement when callers have multi-field docs
// constructed via NewBM25FDoc. With single-field docs (NewBM25Doc),
// behavior is mathematically identical to BM25Rank because tagsTF /
// tagsLen are zero, so the weighted sums collapse to plain content.
func BM25FRank(query string, docs []*BM25Doc) []BM25Result {
	qtokens := tokenize(query)
	if len(qtokens) == 0 || len(docs) == 0 {
		return nil
	}
	idf := computeIDF(docs)
	// Weighted average doc length so length normalization matches the
	// score function's virtual-length view of each doc.
	if len(docs) == 0 {
		return nil
	}
	var totalWeighted float64
	for _, d := range docs {
		totalWeighted += weightContent*float64(d.contentLen) + weightTags*float64(d.tagsLen)
	}
	avgdl := totalWeighted / float64(len(docs))
	out := make([]BM25Result, 0, len(docs))
	for _, d := range docs {
		if s := bm25fScore(qtokens, d, idf, avgdl); s > 0 {
			out = append(out, BM25Result{ID: d.id, Score: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// BM25Rank scores every doc against query and returns hits in score-desc
// order. Docs with zero score (no overlap) are dropped.
func BM25Rank(query string, docs []*BM25Doc) []BM25Result {
	qtokens := tokenize(query)
	if len(qtokens) == 0 || len(docs) == 0 {
		return nil
	}
	idf := computeIDF(docs)
	avgdl := avgDocLen(docs)

	out := make([]BM25Result, 0, len(docs))
	for _, d := range docs {
		if s := bm25Score(qtokens, d, idf, avgdl); s > 0 {
			out = append(out, BM25Result{ID: d.id, Score: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
