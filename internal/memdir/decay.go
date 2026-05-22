package memdir

// decay.go — Ebbinghaus-style retention curve + sweep for auto-memory
// pruning. Borrowed from rohitg00/agentmemory's consolidation
// pipeline (see /Users/ricardo/Documents/公司学习文件/opensource-
// contributions/agentmemory/src/functions/consolidation-pipeline.ts
// lines 23-35 for the original).
//
// Why decay instead of a hard TTL: auto-memory's extractor keeps
// rewriting load-bearing facts (each rewrite resets LastAccessed),
// so genuinely useful memos stay fresh indefinitely. A noise memo
// (one-off detail the extractor happened to pull out) stops getting
// touched and naturally fades out. A TTL would either be too short
// (kill useful facts) or too long (keep noise around forever).

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	if fm.LastAccessed == "" {
		return clamp01(base)
	}
	ts, err := time.Parse(time.RFC3339, fm.LastAccessed)
	if err != nil {
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

// MarkAccessed sets LastAccessed=now and Strength=DefaultStrength.
// Use this on every memdir write so the freshness clock resets when
// the extractor rewrites a memo. Caller is responsible for
// persisting via RenderFile.
func (fm *Frontmatter) MarkAccessed(now time.Time) {
	if fm == nil {
		return
	}
	fm.Strength = DefaultStrength
	fm.LastAccessed = now.UTC().Format(time.RFC3339)
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
		if fm.CurrentStrength(now) >= PruneThreshold {
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
