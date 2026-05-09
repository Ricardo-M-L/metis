package tui

import (
	"os/exec"
	"strings"
	"sync"
	"time"
)

// statusline_extras.go — F12 helpers for the bottom-row status bar:
// cached git branch (no shell-out per frame) + best-effort token-cost
// estimate from the in-memory model catalog.

// gitBranchCache snapshots the current branch every gitBranchTTL so
// renderStatusBar (called every animation frame) doesn't fork
// `git rev-parse` on each tick. Stale cache is fine — the user feels
// no difference if the branch in the status bar lags by 5 seconds.
var (
	gitBranchMu        sync.Mutex
	gitBranchCached    string
	gitBranchCheckedAt time.Time
)

const gitBranchTTL = 5 * time.Second

// cachedGitBranch returns the active branch name, or "" when cwd
// isn't a git repo. TTL-cached so callers can invoke from a hot
// render path.
func cachedGitBranch() string {
	gitBranchMu.Lock()
	defer gitBranchMu.Unlock()
	if time.Since(gitBranchCheckedAt) < gitBranchTTL {
		return gitBranchCached
	}
	gitBranchCheckedAt = time.Now()
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		gitBranchCached = ""
		return ""
	}
	gitBranchCached = strings.TrimSpace(string(out))
	return gitBranchCached
}

// estimateCost returns a best-effort dollar estimate for the
// session's accumulated token usage on the current model. Returns 0
// when the model id isn't in the in-memory catalog (unknown / custom
// gateway / models.dev-not-fetched) — the caller hides the $ field
// rather than show a fake $0.0000.
//
// Calculation:
//
//	input tokens × input_price_per_mtok / 1e6
//
// + output tokens × output_price_per_mtok / 1e6
// + cache_read tokens × cache_read_price_per_mtok / 1e6
// + cache_create tokens × cache_create_price_per_mtok / 1e6
//
// We intentionally use SESSION CUMULATIVE counts, not last-call —
// the right side already shows context-window usage; the $ figure is
// "what have I spent so far this session" the way claude-code's
// /cost answers.
func estimateCost(m *Model) float64 {
	if m == nil || m.loop == nil || m.loop.Provider == nil {
		return 0
	}
	// Pricing currently lives in catalog packages outside the TUI;
	// to avoid an import cycle and keep this lightweight, we expose
	// the price lookup through Provider.PricePerMTok if implemented.
	// When not implemented (no catalog hit), return 0 silently.
	type pricer interface {
		PricePerMTok() (in, out, cacheRead, cacheCreate float64, ok bool)
	}
	prov, ok := m.loop.Provider.(pricer)
	if !ok {
		return 0
	}
	in, out, cacheRead, cacheCreate, hit := prov.PricePerMTok()
	if !hit {
		return 0
	}
	t := &m.totalTokens
	const M = 1_000_000.0
	return float64(t.in)*in/M +
		float64(t.out)*out/M +
		float64(t.cacheRead)*cacheRead/M +
		float64(t.cacheCreate)*cacheCreate/M
}
