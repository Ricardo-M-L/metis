package memdir

// decay.go — Ebbinghaus-style retention curve + sweep for auto-memory
// pruning. Borrowed from rohitg00/agentmemory's consolidation
// pipeline (see /Users/ricardo/Documents/公司学习文件/opensource-
// contributions/agentmemory/src/functions/consolidation-pipeline.ts
// lines 23-35 for the original).
//
// Why decay instead of a hard TTL: durable user facts/preferences are kept by
// policy, while short-lived project state gets a use-sensitive curve. Each
// rewrite/retrieval resets LastUsedAt and increments UseCount, so genuinely
// reused project state stays longer and one-off noise naturally fades out.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Decay parameters. Tuned so a memo untouched for ~5 months drops
// past PruneThreshold:
//
//	0.9 ^ (155/7) ≈ 0.098 < 0.1
//
// Adjust by raising DecayPeriodDays for slower forgetting or
// dropping PruneThreshold to keep things longer. Hard-coded here
// (not config) because changing them mid-run silently shifts which
// files get pruned — opt for an explicit code change.
const (
	// DecayFactor is the multiplier per DecayPeriodDays. 0.9 matches
	// agentmemory and is gentle enough that frequently-reused memos
	// stay near 1.0 indefinitely.
	DecayFactor = 0.9

	// DecayPeriodDays is the window over which one DecayFactor
	// multiplication happens. 7 days = "once a week your memory of
	// untouched facts dims by 10%".
	DecayPeriodDays = 7

	// PruneThreshold is the cutoff below which a memo is considered
	// stale enough to delete. 0.1 corresponds to ~22 weeks (~5
	// months) of zero touches given the constants above.
	PruneThreshold = 0.1

	// DefaultStrength is the strength assigned to newly-written
	// memos. 1.0 = freshly learned. Same baseline agentmemory uses.
	DefaultStrength = 1.0

	// DefaultConfidence is assigned when the extractor did not provide
	// an explicit confidence. Auto-memory facts are useful but inferred,
	// so the default is deliberately below certainty.
	DefaultConfidence = 0.8

	// ReferencePruneThreshold is lower than the project cutoff. External
	// pointers are cheap and often useful after long gaps, so they decay
	// conservatively instead of following short-lived project state.
	ReferencePruneThreshold = 0.025

	// HighConfidenceRetentionThreshold protects explicitly high-confidence
	// memories from an irreversible age-only sweep. Default inferred memories
	// use 0.8, so ordinary project/context state still expires when abandoned.
	HighConfidenceRetentionThreshold = 0.9
)

// CurrentStrength returns the decayed strength as of `now`.
//
// Semantics:
//   - Strength==0 (zero-value): treat as DefaultStrength so legacy
//     files written before the decay system existed don't get
//     retroactively pruned.
//   - LastAccessed empty / unparseable: return the recorded
//     Strength unchanged (clock isn't running yet).
//   - Otherwise: Strength × DecayFactor^(elapsed_days / period_days).
//
// Returned value is clamped to [0, 1] so a clock skew that produces
// "negative elapsed" can't bump strength above the stored value.
func (fm *Frontmatter) CurrentStrength(now time.Time) float64 {
	if fm == nil {
		return 0
	}
	base := fm.Strength
	if base == 0 {
		base = DefaultStrength
	}
	ts, ok := fm.lastActivityTime()
	if !ok {
		return clamp01(base)
	}
	elapsed := now.Sub(ts).Hours() / 24.0
	if elapsed <= 0 {
		return clamp01(base)
	}
	periods := elapsed / float64(DecayPeriodDays)
	// pow without math import (avoid pulling math just for this).
	// Compose 0.9^p iteratively for integer part, then approximate
	// fractional via linear interp — close enough for a decay heuristic.
	whole := int(periods)
	frac := periods - float64(whole)
	v := base
	for i := 0; i < whole; i++ {
		v *= DecayFactor
	}
	// Fractional period: linear blend between v and v*DecayFactor.
	v = v*(1-frac) + v*DecayFactor*frac
	return clamp01(v)
}

// MarkAccessed records a retrieval/reuse without claiming the content was
// edited. LastAccessed is maintained as a compatibility alias for older
// readers; new retention decisions prefer LastUsedAt.
func (fm *Frontmatter) MarkAccessed(now time.Time) {
	if fm == nil {
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	fm.Strength = DefaultStrength
	fm.LastAccessed = stamp
	fm.LastUsedAt = stamp
	fm.UseCount++
	if fm.Confidence <= 0 {
		fm.Confidence = DefaultConfidence
	} else {
		fm.Confidence = clamp01(fm.Confidence)
	}
}

// MarkUpdated records a content rewrite and its accompanying reuse. New
// memos and edits both flow through this method during post-processing.
func (fm *Frontmatter) MarkUpdated(now time.Time) {
	if fm == nil {
		return
	}
	fm.MarkAccessed(now)
	fm.UpdatedAt = now.UTC().Format(time.RFC3339)
}

// ShouldPrune applies the type-aware retention policy:
//   - user facts and feedback/preferences are durable by default;
//   - project/context state uses the legacy strength curve, reinforced by reuse;
//   - references use a lower threshold and therefore decay conservatively;
//   - unknown legacy types retain the prior uniform policy.
func (fm *Frontmatter) ShouldPrune(now time.Time) bool {
	if fm == nil {
		return false
	}
	// Pinned is intentionally an extension field: Frontmatter preserves
	// unknown YAML keys so existing `pinned: true` memories do not need a
	// migration. High-confidence memories are likewise protected because a
	// decay sweep is destructive and cannot re-derive a forgotten fact.
	if fm.retentionPinned() || fm.Confidence >= HighConfidenceRetentionThreshold {
		return false
	}
	switch fm.Type {
	case TypeUser, TypeFeedback:
		return false
	case TypeReference:
		// References are cheap pointers and may be used seasonally. Keep
		// them for at least a year before even considering decay.
		if last, ok := fm.lastActivityTime(); ok && now.Sub(last) < 365*24*time.Hour {
			return false
		}
	}
	strength := fm.CurrentStrength(now)
	// Each reuse contributes 4%, capped at 80%. This is enough for a
	// repeatedly consulted project decision to outlive a one-off note while
	// still allowing genuinely abandoned state to expire eventually.
	uses := fm.UseCount
	if uses < 0 {
		uses = 0
	}
	if uses > 20 {
		uses = 20
	}
	strength *= 1 + float64(uses)*0.04
	threshold := PruneThreshold
	if fm.Type == TypeReference {
		threshold = ReferencePruneThreshold
	}
	return strength < threshold
}

// lastActivityTime returns the newest valid retrieval/access/update stamp.
// Picking the newest (rather than blindly preferring LastUsedAt) prevents a
// recently rewritten memory with older legacy usage metadata from being
// deleted on its first sweep.
func (fm *Frontmatter) lastActivityTime() (time.Time, bool) {
	if fm == nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, stamp := range []string{fm.LastUsedAt, fm.LastAccessed, fm.UpdatedAt} {
		if stamp == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
			if newest.IsZero() || parsed.After(newest) {
				newest = parsed
			}
		}
	}
	return newest, !newest.IsZero()
}

func (fm *Frontmatter) retentionPinned() bool {
	if fm == nil || fm.Extra == nil {
		return false
	}
	raw, ok := fm.Extra["pinned"]
	if !ok {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

// SweepResult summarises a DecayAndPrune pass. Used by the dream
// agent / diag command to print a concise "kept N, pruned M" line
// instead of a per-file log.
type SweepResult struct {
	Kept   int      // files whose strength is still above threshold
	Pruned []string // absolute paths of files deleted
	Errors []error  // non-fatal per-file errors (read/parse/delete)
}

// DecayAndPrune walks `root`, computes each memo's CurrentStrength
// against `now`, and deletes anything that fell below
// PruneThreshold. Returns a result summary; never an error for
// per-file issues (those land in result.Errors) — only directory-
// level problems surface as the outer error.
//
// Intended call sites:
//   - At session start (cheap; touches each file once)
//   - From the dream agent's wrap-up phase (after a fresh batch of
//     extractions, prune the now-stale predecessors)
//   - `metis dream sweep` (future CLI command)
//
// Skips MEMORY.md and the core.d / archival / recall / daily
// subtrees — those have their own lifecycle and aren't part of the
// extractor-generated memo set.
func DecayAndPrune(ctx context.Context, root string, now time.Time) (SweepResult, error) {
	if root == "" {
		return SweepResult{}, errors.New("memdir: DecayAndPrune: empty root")
	}
	files, err := ScanMemoryFiles(ctx, root)
	if err != nil {
		return SweepResult{}, fmt.Errorf("scan: %w", err)
	}
	res := SweepResult{}
	for i := range files {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		// Frontmatter is a value field on MemoryFile; take its
		// address so CurrentStrength's nil guard still works for
		// any future caller that DOES pass a nil ptr.
		fm := &files[i].Frontmatter
		if !fm.ShouldPrune(now) {
			res.Kept++
			continue
		}
		if err := os.Remove(files[i].Path); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("remove %s: %w", files[i].Path, err))
			continue
		}
		res.Pruned = append(res.Pruned, files[i].Path)
	}
	return res, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
