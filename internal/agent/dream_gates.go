package agent

// dream_gates.go — the three-stage gate that decides whether
// AutoMemoryExtractor.OnLoopEnd should actually fire a dream this
// turn. Each gate is intentionally cheap-first so the hot path on a
// chatty user (turn every few seconds, no dream pending) stays at
// O(one mtime stat) and never hits the filesystem twice.
//
// Mirrors claude-code's services/autoDream/autoDream.ts:127-171:
//
//   1. Time gate — has it been ≥ METIS_DREAM_INTERVAL_HOURS since the
//      last successful dream? Reads .dream-lock mtime, no other I/O.
//   2. Scan throttle — even if (1) passes, only re-scan sessions every
//      ~10 min. Protects a watched directory with hundreds of session
//      files from a per-turn ReadDir storm.
//   3. Session gate — count distinct session JSONLs that have been
//      touched since the last dream. < METIS_DREAM_MIN_SESSIONS means
//      "not enough new material" to bother summarizing.
//
// The acquire-and-rollback dance lives in OnLoopEnd, not here — these
// helpers only answer "should we even try?".

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultDreamIntervalHours is the floor on time between dreams. 12 h
// (raised from the original 6 h on 2026-05-16). The thinking:
//   - 6 h on a heavy-use day fires the LLM fork 2-4 times, which
//     accumulates token cost AND rewrites the prompt cache 2-4 times.
//   - 12 h fires at most twice on the busiest day, once on a normal
//     day, and zero times when the 3-session gate doesn't accumulate.
//   - claude-code uses 24 h because it serves multi-day cloud
//     workflows; metis is a single-user local CLI where users want
//     "today's facts" reflected by tomorrow morning, not in three
//     days. 12 h is the middle.
// Override per host via METIS_DREAM_INTERVAL_HOURS — set 0 to disable.
const DefaultDreamIntervalHours = 12.0

// DefaultDreamMinSessions is the minimum number of *distinct* session
// files that must have been touched since the last successful dream.
// 3 is a guess at "enough variety to be worth summarizing"; lower than
// claude-code's 5 because metis is a thinner single-user client and
// users rarely keep 5+ parallel windows open. Override via
// METIS_DREAM_MIN_SESSIONS.
const DefaultDreamMinSessions = 3

// DreamScanThrottle is the cooldown between session-directory scans.
// We re-check the time gate on every LoopEnd (mtime stat is free), but
// the session count requires a directory walk — at 10 min the cost is
// amortized to ~once-per-conversation rather than once-per-turn.
const DreamScanThrottle = 10 * time.Minute

// dreamIntervalHours reads METIS_DREAM_INTERVAL_HOURS or returns the
// default. Setting it to 0 (or any non-positive value) is the
// canonical way to disable dreaming entirely — every gate will refuse.
func dreamIntervalHours() float64 {
	if raw := os.Getenv("METIS_DREAM_INTERVAL_HOURS"); raw != "" {
		if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			return v
		}
	}
	return DefaultDreamIntervalHours
}

// dreamMinSessions reads METIS_DREAM_MIN_SESSIONS or returns the
// default. Values < 1 are clamped to 1 (gating on zero sessions would
// fire every turn, defeating the gate).
func dreamMinSessions() int {
	if raw := os.Getenv("METIS_DREAM_MIN_SESSIONS"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			if v < 1 {
				return 1
			}
			return v
		}
	}
	return DefaultDreamMinSessions
}

// DreamGateDecision is the structured outcome of one gate evaluation.
// Returning a value (not just a bool) lets the extractor surface a
// reason to /dream status without re-running the gate logic.
type DreamGateDecision struct {
	Fire    bool   // true → caller should attempt to acquire the lock
	Reason  string // short tag for telemetry ("disabled" / "too-soon" / "throttled" / "thin" / "ok")
	Scanned bool   // true when gate 3 actually walked sessionsDir — caller bumps lastScanAt accordingly
}

// shouldFireDream evaluates the three gates against the current
// memory and session state. Caller passes:
//
//	lastSuccessAt — DreamLock.LastSuccessAt() value
//	lastScanAt    — the extractor's cached last-scan timestamp (in-mem)
//	sessionsDir   — typically ~/.metis/sessions
//	now           — time.Now (parameterized so tests can pin it)
//
// The function does no I/O when the time gate fails (the cheap case);
// only walks sessionsDir when both upstream gates pass.
func shouldFireDream(lastSuccessAt, lastScanAt time.Time, sessionsDir string, now time.Time) DreamGateDecision {
	hours := dreamIntervalHours()
	if hours <= 0 {
		return DreamGateDecision{Fire: false, Reason: "disabled"}
	}

	// Gate 1 — time. lastSuccessAt being zero (never dreamed) is
	// treated as "infinitely long ago", so a fresh install dreams as
	// soon as enough sessions accumulate.
	if !lastSuccessAt.IsZero() {
		if now.Sub(lastSuccessAt) < time.Duration(hours*float64(time.Hour)) {
			return DreamGateDecision{Fire: false, Reason: "too-soon"}
		}
	}

	// Gate 2 — scan throttle. Without this every LoopEnd that passes
	// gate 1 would re-walk ~/.metis/sessions, which on a heavy user
	// is a few hundred dirents. lastScanAt being zero means "we've
	// never scanned" — let it through so the very first dream-eligible
	// turn can fire without an extra 10 min wait.
	if !lastScanAt.IsZero() && now.Sub(lastScanAt) < DreamScanThrottle {
		return DreamGateDecision{Fire: false, Reason: "throttled"}
	}

	// Gate 3 — session count. Walks sessionsDir; this is the only
	// I/O-heavy gate. We're deliberately permissive on errors: a
	// missing/unreadable sessions dir means metis hasn't recorded any
	// sessions yet (brand-new install), and there's nothing to dream
	// about anyway.
	count, err := countSessionsTouchedSince(sessionsDir, lastSuccessAt)
	if err != nil {
		return DreamGateDecision{Fire: false, Reason: "scan-error", Scanned: true}
	}
	if count < dreamMinSessions() {
		return DreamGateDecision{Fire: false, Reason: "thin", Scanned: true}
	}
	return DreamGateDecision{Fire: true, Reason: "ok", Scanned: true}
}

// countSessionsTouchedSince walks sessionsDir and returns the number
// of `*.jsonl` files whose mtime is strictly after `since`. Passing a
// zero `since` counts every session file — the value a fresh-install
// dream uses to confirm "yes, there's history to consolidate".
//
// We intentionally do NOT exclude the current session: there's no
// stable handle to "my session" at this layer (the Loop doesn't carry
// SessionID), and over-counting by 1 only ever helps dreaming fire
// slightly earlier than the strict definition would suggest — which
// for a 3-session threshold means the user typically sees their first
// dream after working in 3 sessions including the current one. Good
// enough; matches user intuition more than excluding would.
func countSessionsTouchedSince(sessionsDir string, since time.Time) (int, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if since.IsZero() || info.ModTime().After(since) {
			count++
		}
	}
	return count, nil
}

// defaultSessionsDir returns ~/.metis/sessions. Used by callers that
// don't already have a sessions root threaded through their state.
// Returns empty on UserHomeDir failure — callers must treat empty as
// "skip the session gate".
func defaultSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".metis", "sessions")
}
