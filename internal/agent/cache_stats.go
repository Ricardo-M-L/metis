package agent

// cache_stats.go — minimal ring-buffer "what's my cache hit rate
// looking like, and what changed between turns?" tracker.
//
// Source of inspiration: openclaude's cacheMetrics.ts (697 lines) +
// promptCacheBreakDetection.ts. We do a tiny version: the per-turn
// {input, cache_create, cache_read, output} tuple plus a fingerprint
// hash so the model can self-diagnose "I just blew my cache, why?".
//
// Surfacing:
//   - metis_info dumps the latest entries under [cache] (separate
//     section)
//   - the agent loop appends one CacheStat per turn after a
//     successful Provider.Stream
//
// What it deliberately doesn't do:
//   - render dashboards (that's `metis stats` HTML; SUMMARY #18)
//   - track cross-session aggregates (a follow-up; needs a sidecar
//     file under ~/.metis/cache_stats.jsonl)
//   - second-guess the provider's own usage numbers — we record
//     what the provider returned

import (
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// CacheStat is one turn's worth of usage + the fingerprint of the
// inputs that determined whether the prefix cache could be reused.
type CacheStat struct {
	// Turn is the loop's turn index when this stat was recorded.
	Turn int
	// Input / Output are model.Response token counts.
	Input  int
	Output int
	// CacheCreate is `cache_creation_input_tokens` (Anthropic) /
	// equivalent on other providers — billed full price.
	CacheCreate int
	// CacheRead is the cached prefix billed at ~10% (Anthropic) /
	// `cached_tokens` field on OpenAI-flavoured providers.
	CacheRead int
	// Fingerprint is sha256(model | system | tool_names | effort).
	// Comparing fingerprints across turns identifies which input
	// changed when the cache breaks unexpectedly.
	Fingerprint string
}

// HitRate returns CacheRead / (CacheRead + CacheCreate + Input).
// 0 when nothing was billed at all (e.g. provider returned empty
// usage).
func (s CacheStat) HitRate() float64 {
	denom := s.CacheRead + s.CacheCreate + s.Input
	if denom <= 0 {
		return 0
	}
	return float64(s.CacheRead) / float64(denom)
}

// CacheStatsRing is a fixed-size ring buffer of recent CacheStats.
// Cap defaults to 100 — large enough to see patterns, small enough
// not to inflate memory in long-lived sessions.
type CacheStatsRing struct {
	mu  sync.RWMutex
	buf []CacheStat
	cap int
}

// NewCacheStatsRing constructs a ring with the given cap. Pass 0 to
// use the default (100).
func NewCacheStatsRing(cap int) *CacheStatsRing {
	if cap <= 0 {
		cap = 100
	}
	return &CacheStatsRing{cap: cap}
}

// Add appends a new stat, evicting the oldest when at capacity.
func (r *CacheStatsRing) Add(s CacheStat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) >= r.cap {
		// Drop the oldest. A single-shift on a 100-element slice is
		// effectively free — we don't bother with the head/tail
		// indices a more sophisticated ring would use.
		r.buf = append(r.buf[1:], s)
		return
	}
	r.buf = append(r.buf, s)
}

// Reset drops stats from the previous top-level session.
func (r *CacheStatsRing) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = nil
}

// Snapshot returns a defensive copy of the current ring contents.
func (r *CacheStatsRing) Snapshot() []CacheStat {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CacheStat, len(r.buf))
	copy(out, r.buf)
	return out
}

// HitRate returns the aggregated cache_read / total ratio across all
// recorded turns. Useful for the metis_info summary line.
func (r *CacheStatsRing) HitRate() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var read, total int
	for _, s := range r.buf {
		read += s.CacheRead
		total += s.CacheRead + s.CacheCreate + s.Input
	}
	if total == 0 {
		return 0
	}
	return float64(read) / float64(total)
}

// LastBreak compares the most recent stat's fingerprint to the one
// before it. Returns ("", "") when there's no break or when fewer
// than 2 entries exist; otherwise returns (oldFP, newFP) so callers
// can log "cache broke at turn N (was X, now Y)".
//
// Note: we return only the fingerprints — comparing them is
// caller's job. We deliberately don't try to identify "which field
// changed" because the inputs to the hash are short and the caller
// likely already has the model/system/tools at hand to diff
// directly.
func (r *CacheStatsRing) LastBreak() (oldFP, newFP string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.buf) < 2 {
		return "", ""
	}
	prev := r.buf[len(r.buf)-2].Fingerprint
	cur := r.buf[len(r.buf)-1].Fingerprint
	if prev != cur {
		return prev, cur
	}
	return "", ""
}

// FingerprintFor builds the hash key used by CacheStat. Inputs:
//
//	model — Loop.Model (the resolved id)
//	system — the assembled system prompt at the time of the call
//	tools — the tool spec list ([]llm.ToolSpec); only Name participates
//	effort — Loop.EffortValue()
//
// Output: hex-encoded sha256, first 12 chars (full hash is overkill
// for prefix-cache invalidation analysis).
func FingerprintFor(model, system string, tools []llm.ToolSpec, effort llm.Effort) string {
	h := sha256.New()
	fmt.Fprintln(h, "model:", model)
	fmt.Fprintln(h, "system_len:", len(system))
	// Use a content hash of system rather than the body — system is
	// likely large (KBs), and the fingerprint just needs identity.
	fmt.Fprintln(h, "system_hash:", sha256Hex(system))
	for _, t := range tools {
		fmt.Fprintln(h, "tool:", t.Name)
	}
	fmt.Fprintln(h, "effort:", effort)
	sum := h.Sum(nil)
	return fmt.Sprintf("%x", sum[:6])
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:6])
}
