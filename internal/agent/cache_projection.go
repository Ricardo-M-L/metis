package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// RequestCacheProjection describes the logical request before adapter-specific
// folding, state continuation, hidden provider prefixes, and tokenization.
// Never interpret FirstChange or SharedPrefixTokens as a cache miss/hit.
type RequestCacheProjection struct {
	Source              string
	StableSystemTokens  int
	DynamicSystemTokens int
	ToolSchemaTokens    int
	HistoryTokens       int
	SharedPrefixTokens  int
	FirstChange         string
}

type cacheProjectionPart struct{ category, body string }

func requestCacheParts(req llm.Request) ([]cacheProjectionPart, RequestCacheProjection) {
	p := RequestCacheProjection{Source: "local_request_estimate"}
	parts := []cacheProjectionPart{{"model_effort", req.Model + "\x00" + string(req.Effort)}}
	add := func(category, body string) { parts = append(parts, cacheProjectionPart{category, body}) }
	if len(req.SystemSections) == 0 {
		add("stable_system", req.System)
		p.StableSystemTokens += cacheEstimate(req.System)
	} else {
		for _, s := range req.SystemSections {
			category := "stable_system"
			if s.Volatile || s.Name == "memory_index" || s.Name == "runtime_state" {
				category = "dynamic_system"
				p.DynamicSystemTokens += cacheEstimate(s.Body)
			} else {
				p.StableSystemTokens += cacheEstimate(s.Body)
			}
			add(category, s.Name+"\x00"+s.Body)
		}
	}
	for _, t := range req.Tools {
		b, _ := json.Marshal(t)
		add("tool_schema", string(b))
		p.ToolSchemaTokens += cacheEstimate(string(b))
	}
	for _, m := range req.Messages {
		// Shallow-copy each block and hash opaque payload identities before
		// serialization: base64 bytes are neither text tokens nor safe state
		// to retain in a diagnostic tracker.
		clean := m
		clean.Content = cacheProjectionBlocks(m.Content)
		b, _ := json.Marshal(clean)
		add("history", string(b))
		p.HistoryTokens += estimateActiveHistoryTokens([]llm.Message{m})
	}
	if req.ResponseFormat != nil {
		b, _ := json.Marshal(req.ResponseFormat)
		add("response_format", string(b))
	}
	return parts, p
}

func cacheProjectionBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, len(blocks))
	for i, block := range blocks {
		out[i] = block
		if block.Data != "" {
			out[i].Data = "sha256:" + sha256Hex(block.Data)
		}
		if len(block.ProviderHint) > 0 {
			out[i].ProviderHint = make(map[string]string, len(block.ProviderHint))
			for k, v := range block.ProviderHint {
				out[i].ProviderHint[k] = "sha256:" + sha256Hex(v)
			}
		}
		if len(block.ToolResultBlocks) > 0 {
			out[i].ToolResultBlocks = cacheProjectionBlocks(block.ToolResultBlocks)
		}
	}
	return out
}

// cacheEstimate intentionally uses a transparent byte heuristic, not an
// undocumented tokenizer. In particular image bytes are not image token usage.
func cacheEstimate(s string) int { return (len(s) + 3) / 4 }

func compareCacheParts(previous, current []cacheProjectionPart) (int, string) {
	if previous == nil {
		return 0, "initial"
	}
	shared := 0
	for i, part := range current {
		if i >= len(previous) {
			return shared / 4, part.category
		}
		old := previous[i]
		if old.category != part.category {
			return shared / 4, part.category
		}
		if old.body != part.body {
			n := len(old.body)
			if len(part.body) < n {
				n = len(part.body)
			}
			j := 0
			for j < n && old.body[j] == part.body[j] {
				j++
			}
			return (shared + j) / 4, part.category
		}
		shared += len(part.body)
	}
	if len(previous) > len(current) {
		return shared / 4, previous[len(current)].category
	}
	return shared / 4, "unchanged"
}

// RecordRequest preserves actual usage and adds local-only structural
// diagnostics. Raw projection content is kept only for the preceding request,
// never returned in Snapshot or introspection, and cleared at session reset.
func (r *CacheStatsRing) RecordRequest(req llm.Request, stat CacheStat) {
	if r == nil {
		return
	}
	parts, projection := requestCacheParts(req)
	r.mu.Lock()
	projection.SharedPrefixTokens, projection.FirstChange = compareCacheParts(r.previousRequest, parts)
	r.previousRequest = parts
	stat.Projection = projection
	stat.Fingerprint = FingerprintRequest(req)
	r.addLocked(stat)
	r.mu.Unlock()
}

// CacheSummary is safe for a read-only introspection tool: no prompt bodies,
// schema bodies, session IDs, or credentials are exposed.
func writeCacheSummary(b *strings.Builder, s CacheStat, turns int, rate float64) {
	fmt.Fprintf(b, "turns = %d\nactual_input_uncached = %d\nactual_cache_read = %d\nactual_cache_create = %d\nactual_output = %d\nactual_cache_read_ratio = %.4f\n", turns, s.Input, s.CacheRead, s.CacheCreate, s.Output, rate)
	p := s.Projection
	fmt.Fprintf(b, "projection_source = %s\nestimated_stable_system_tokens = %d\nestimated_dynamic_system_tokens = %d\nestimated_tool_schema_tokens = %d\nestimated_history_tokens = %d\nestimated_shared_prefix_tokens = %d\nfirst_changed_category = %s\n", p.Source, p.StableSystemTokens, p.DynamicSystemTokens, p.ToolSchemaTokens, p.HistoryTokens, p.SharedPrefixTokens, p.FirstChange)
	b.WriteString("interpretation = local logical request only; fingerprint changes do not imply a full cache miss\n")
}

func (r *CacheStatsRing) CacheSummary() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := r.buf
	if len(entries) == 0 {
		return "[cache]\nturns = 0\n"
	}
	s := entries[len(entries)-1]
	var b strings.Builder
	b.WriteString("[cache]\n")
	// Formatting lives here so the tools package does not depend on agent.
	var read, total int
	for _, entry := range entries {
		read += entry.CacheRead
		total += entry.Input + entry.CacheRead + entry.CacheCreate
	}
	var rate float64
	if total > 0 {
		rate = float64(read) / float64(total)
	}
	writeCacheSummary(&b, s, len(entries), rate)
	return b.String()
}
