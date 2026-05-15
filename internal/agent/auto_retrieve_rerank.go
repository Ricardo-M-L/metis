package agent

// auto_retrieve_rerank.go — optional LLM-driven rerank for AutoRetrieve.
//
// claude-code's findRelevantMemories does this with a Sonnet sideQuery
// per turn (memdir/findRelevantMemories.ts:98). metis's default is
// raw BM25 — fast, deterministic, no spend — but BM25 mis-ranks when
// the query and passage share keywords yet mean different things
// (e.g. query "how to escape Python's GIL" vs a passage about a Go
// "go" channel pattern: BM25 fixates on "go" overlap).
//
// METIS_AUTO_RETRIEVE_RERANK=1 wires this in: BM25 still runs first
// (cheap candidate generation), then the K*3 raw passages get
// presented to the active provider with a one-shot rank prompt,
// and only the model-picked top-K make it into the system section.
//
// Hard 3s timeout. On any failure (timeout, parse error, provider
// returns garbage) we fall back to BM25 ordering — the model still
// gets retrieval, just without the LLM polish. The point is to
// IMPROVE ranking when it works, not to crash the loop when it
// doesn't.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

// rerankAutoRetrieve takes the BM25 candidates and asks the provider
// to pick the top k. Returns the picked subset on success; the
// candidates capped to k on any failure (so the caller never sees an
// error path — degraded ranking is acceptable, lost retrieval is not).
//
// candidates is the BM25 top K*N (N=3 by default in callers) so the
// model has slack to pick from. k is the final count to return.
func rerankAutoRetrieve(
	ctx context.Context,
	provider llm.Provider,
	query string,
	candidates []memory.Passage,
	k int,
) []memory.Passage {
	if provider == nil || len(candidates) == 0 || k <= 0 {
		return capPassages(candidates, k)
	}
	if len(candidates) <= k {
		return candidates // nothing to rerank
	}

	prompt := buildRerankPrompt(query, candidates, k)

	rerankCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req := llm.Request{
		Model:     "", // let provider use its default
		System:    rerankSystemPrompt,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: prompt}}}},
		Stream:    false,
		MaxTokens: 200, // we only need a JSON array of indices
	}

	resp, err := provider.Complete(rerankCtx, req)
	if err != nil || resp == nil {
		return capPassages(candidates, k)
	}

	body := extractText(resp)
	indices, ok := parseRerankIndices(body, len(candidates))
	if !ok || len(indices) == 0 {
		return capPassages(candidates, k)
	}
	out := make([]memory.Passage, 0, k)
	for _, idx := range indices {
		if idx < 0 || idx >= len(candidates) {
			continue
		}
		out = append(out, candidates[idx])
		if len(out) >= k {
			break
		}
	}
	if len(out) == 0 {
		return capPassages(candidates, k)
	}
	return out
}

const rerankSystemPrompt = `You rank archival memory passages by relevance to a query. Your output is parsed by code; output ONLY a JSON array of integer indices, nothing else. Pick the most relevant N passages from the candidates, in best-first order. Example output: [3, 0, 7]`

func buildRerankPrompt(query string, candidates []memory.Passage, k int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Query: %s\n\nCandidates:\n", strings.TrimSpace(query))
	for i, p := range candidates {
		// Trim each candidate to ~300 chars so the rerank prompt
		// itself stays bounded; the ranker just needs the gist to
		// score relevance, not the full body.
		body := strings.TrimSpace(p.Content)
		if len(body) > 300 {
			body = body[:297] + "..."
		}
		fmt.Fprintf(&sb, "[%d] %s\n", i, body)
	}
	fmt.Fprintf(&sb, "\nReturn the top %d most relevant indices as a JSON array, best-first. Only the JSON array, no explanation.", k)
	return sb.String()
}

// parseRerankIndices extracts a `[1, 0, 5]`-style JSON array from a
// model's response. Tolerates surrounding whitespace, code fences, or
// trailing prose — best-effort extraction since we explicitly tell
// the model to output only the array but some tuned models still
// chat-pad their replies.
func parseRerankIndices(body string, max int) ([]int, bool) {
	body = strings.TrimSpace(body)
	// Strip ``` fences if present.
	if strings.HasPrefix(body, "```") {
		// Drop everything up to the first newline (the ```json line)
		// and the trailing ```.
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			body = body[nl+1:]
		}
		body = strings.TrimSuffix(body, "```")
		body = strings.TrimSpace(body)
	}
	// Find the first `[` and last `]` — anything between should be
	// the array. Tolerates "Top picks: [1, 0, 5] (great!)".
	start := strings.IndexByte(body, '[')
	end := strings.LastIndexByte(body, ']')
	if start < 0 || end < 0 || end < start {
		return nil, false
	}
	body = body[start : end+1]

	var raw []int
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, false
	}
	out := make([]int, 0, len(raw))
	seen := make(map[int]bool, len(raw))
	for _, v := range raw {
		if v < 0 || v >= max || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, len(out) > 0
}

// extractText pulls the assistant text out of a Complete() response,
// concatenating any text blocks. Tool-use / image blocks ignored
// because the rerank reply should be plain text JSON.
func extractText(resp *llm.Response) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// capPassages returns the first k entries of in (or all of them if
// in is shorter). Used as the BM25-fallback path so a rerank failure
// degrades to plain top-k by BM25 score rather than dropping
// retrieval entirely.
func capPassages(in []memory.Passage, k int) []memory.Passage {
	if k <= 0 {
		return nil
	}
	if len(in) <= k {
		return in
	}
	return in[:k]
}
