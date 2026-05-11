// Package permission implements cascading permission rules inspired by
// Claude Code's settings precedence (CLI > local > project > user > policy).
// Modes mirror the operator-friendly defaults: ask / auto / plan / deny.
package permission

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Mode is the global gating posture for a session.
type Mode string

const (
	ModeAsk         Mode = "ask"         // prompt for every non-allowlisted action
	ModeAcceptEdits Mode = "acceptEdits" // auto-allow reads + project-local writes; ask for shell
	ModeBypass      Mode = "bypass"      // approve everything (dangerous; opt-in)
	ModePlan        Mode = "plan"        // read-only mode; no edits or shell writes
	ModeDeny        Mode = "deny"        // refuse everything that asks

	// Removed 2026-05-11: ModeAcceptEdits. Old semantics were "auto-allow
	// read-only + safe bash, ask for writes" — a midpoint between
	// ModeAsk and ModeAcceptEdits. The name collided with claude-code's
	// ant-only `auto` (which is an LLM-classifier mode and means
	// something entirely different), confusing every user who tried to
	// compare the two. Users who want "auto-allow file edits, ask for
	// shell" should pick ModeAcceptEdits — that matches claude-code's
	// public `acceptEdits` exactly.
)

// Decision is the final verdict for a tool call.
type Decision int

const (
	DecisionAsk Decision = iota
	DecisionAllow
	DecisionDeny
)

// Rule is a single declarative entry: tool + optional content matcher + verb.
type Rule struct {
	Tool   string // e.g. "Bash", "Edit", "*" for any
	Match  string // optional substring of stringified input
	Verb   Decision
	Source string // for diagnostics: "cli", "project", "user", ...
}

// YoloVerdict is the 3-state output of a YOLO classifier (claude-code's
// utils/permissions/yoloClassifier.ts). The classifier inspects the
// proposed tool call and returns:
//   - YoloAllow: looks safe in context — proceed silently
//   - YoloSoftDeny: ambiguous — fall back to ASK (user prompt)
//   - YoloHardDeny: clearly destructive or out-of-scope — block hard
//
// Only consulted when the gate's mode is ModeBypass; users who opted
// into "approve everything" still want a sanity-check escape hatch.
type YoloVerdict int

const (
	YoloAllow YoloVerdict = iota
	YoloSoftDeny
	YoloHardDeny
)

// YoloClassifier inspects a tool invocation and returns a verdict.
// The runtime injects an LLM-backed implementation; tests can wire a
// pure-Go stub. Returning an error short-circuits to YoloAllow (fail
// open — classifier outage shouldn't block a user who already opted
// into bypass mode).
type YoloClassifier interface {
	Classify(ctx context.Context, tool, stringInput string) (YoloVerdict, string, error)
}

// StagedYoloClassifier is the optional two-stage variant. Mirrors
// claude-code's yoloClassifier.ts: stage 1 is "fast" with max_tokens=64
// and stop_sequences for an immediate yes/no decision; if it says
// block, escalate to stage 2 ("thinking") with chain-of-thought to
// reduce false positives.
//
// Cost shape: stage 1 is ~10% of stage 2's cost. Most actions get an
// instant ALLOW from stage 1 and never escalate, so the average call
// approaches stage-1 cost.
//
// A classifier that only implements YoloClassifier still works — Gate
// detects StagedYoloClassifier via type assertion and calls the
// two-stage path when available.
type StagedYoloClassifier interface {
	YoloClassifier
	// ClassifyFast must be a low-latency / low-token call. Recommended
	// max_tokens=64 + stop_sequences. Returns YoloAllow without invoking
	// stage 2; YoloHardDeny / YoloSoftDeny triggers a stage-2 confirm.
	ClassifyFast(ctx context.Context, tool, stringInput string) (YoloVerdict, string, error)

	// ClassifyThinking is the deeper review with chain-of-thought.
	// Called only when stage 1 returned a deny. If thinking returns
	// YoloAllow, the deny is reversed (stage 1 was a false positive).
	ClassifyThinking(ctx context.Context, tool, stringInput string) (YoloVerdict, string, error)
}

// runClassifier executes the classifier with two-stage escalation when
// supported. Caller holds g.mu.Lock(). Returns the same shape as a
// single-stage Classify call.
func (g *Gate) runClassifier(ctx context.Context, tool, stringInput string) (YoloVerdict, string, error) {
	if staged, ok := g.classifier.(StagedYoloClassifier); ok {
		v, src, err := staged.ClassifyFast(ctx, tool, stringInput)
		if err != nil {
			return v, src, err
		}
		if v == YoloAllow {
			return v, src, nil
		}
		// Stage 1 leaned deny → confirm with stage 2. False-positive
		// reversal (thinking says allow) wins; the system prompt tells
		// stage 2 to think hard before reversing.
		v2, src2, err := staged.ClassifyThinking(ctx, tool, stringInput)
		if err != nil {
			// Stage 2 failed. Trust stage 1 — better safe than open.
			return v, src + "+stage2_err", nil
		}
		if v2 == YoloAllow {
			return YoloAllow, "thinking_reversed:" + src + "→allow", nil
		}
		return v2, "thinking:" + src2, nil
	}
	// Single-stage classifier path — original behaviour.
	return g.classifier.Classify(ctx, tool, stringInput)
}

// DenialLimits caps how many times an automated subsystem (rule set,
// classifier) can deny in a row before the gate forces ASK regardless
// of mode. Mirrors claude-code denialTracking.ts:13-15.
//
// MaxConsecutive: after N back-to-back DENIES, the gate degrades the
// next would-be-DENY decision to ASK. Resets on any successful ALLOW.
//
// MaxTotal: after N total DENIES in a session, same degradation. The
// total counter never resets — a session that has been hitting walls
// all night should fall back to humans even if the streak was broken
// by an occasional allow.
type DenialLimits struct {
	MaxConsecutive int
	MaxTotal       int
}

// DefaultDenialLimits matches claude-code DENIAL_LIMITS.
var DefaultDenialLimits = DenialLimits{MaxConsecutive: 3, MaxTotal: 20}

// classifierFailClosedFor is how long the gate stops calling a failing
// classifier and falls back to a safe default. 30 minutes mirrors
// claude-code's CLASSIFIER_FAIL_CLOSED_REFRESH_MS — short enough that a
// transient outage self-heals within one short session, long enough
// that we don't keep hammering a dead endpoint every tool call.
const classifierFailClosedFor = 30 * time.Minute

// Gate combines a Mode and a stack of Rule sources.
type Gate struct {
	mu    sync.RWMutex
	mode  Mode
	rules []Rule
	// remembered "ask once, apply forever this session"
	memoAllow map[string]bool

	// classifier optionally inspects bypass-mode calls. nil = no
	// extra classification; bypass mode short-circuits to allow.
	classifier YoloClassifier

	// denialLimits + denial counters: mid-session circuit breaker.
	// When the rule set or classifier denies too many times in a
	// row (or in total), Check downgrades the next would-be DENY to
	// ASK so a human pulls the agent out of an automated rut.
	denialLimits        DenialLimits
	consecutiveDenials  int
	totalDenials        int
	denialFallbackUntil time.Time // if non-zero & in future, force ASK regardless

	// classifierFailUntil tracks Yolo classifier outage. When the
	// classifier returns an error (transport, 5xx, timeout), Bypass
	// mode falls open (we trust user's bypass posture) but stops
	// calling the classifier for 30 minutes — otherwise every tool
	// call burns a doomed retry. Mirrors CLASSIFIER_FAIL_CLOSED_*.
	classifierFailUntil time.Time
}

func New(mode Mode) *Gate {
	return &Gate{
		mode:         mode,
		memoAllow:    make(map[string]bool),
		denialLimits: DefaultDenialLimits,
	}
}

// SetDenialLimits overrides the default denial-tracking thresholds.
// Set MaxConsecutive=0 or MaxTotal=0 to disable that half of the
// breaker.
func (g *Gate) SetDenialLimits(d DenialLimits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.denialLimits = d
}

// recordDenial bumps both denial counters and trips the breaker if
// either limit is reached. Caller must hold g.mu.Lock().
func (g *Gate) recordDenial() {
	g.consecutiveDenials++
	g.totalDenials++
	lim := g.denialLimits
	if (lim.MaxConsecutive > 0 && g.consecutiveDenials >= lim.MaxConsecutive) ||
		(lim.MaxTotal > 0 && g.totalDenials >= lim.MaxTotal) {
		// Degrade for one tool decision. Cleared on next successful
		// allow OR when a human override resets via ResetDenials.
		g.denialFallbackUntil = time.Now().Add(time.Hour)
	}
}

// recordAllow resets the consecutive streak. Caller must hold lock.
// Total never resets — claude-code intent: total counter is "session
// has been pathologically deny-heavy", which doesn't go away just
// because we got lucky once.
func (g *Gate) recordAllow() {
	g.consecutiveDenials = 0
	if !g.denialFallbackUntil.IsZero() && time.Now().After(g.denialFallbackUntil) {
		g.denialFallbackUntil = time.Time{}
	}
}

// ResetDenials clears all denial state. Wired to a `/reset` slash
// command so users can explicitly tell the gate "I know what I'm
// doing, stop nagging."
func (g *Gate) ResetDenials() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consecutiveDenials = 0
	g.totalDenials = 0
	g.denialFallbackUntil = time.Time{}
}

// DenialState exposes counters for UI.
func (g *Gate) DenialState() (consecutive, total int, fallbackActive bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.consecutiveDenials, g.totalDenials,
		!g.denialFallbackUntil.IsZero() && time.Now().Before(g.denialFallbackUntil)
}

// markClassifierFailed opens the 30-minute fail-closed window. Caller
// holds g.mu.Lock().
func (g *Gate) markClassifierFailed() {
	g.classifierFailUntil = time.Now().Add(classifierFailClosedFor)
}

// classifierUsable reports whether the classifier is currently trusted.
// Caller holds at least RLock.
func (g *Gate) classifierUsable() bool {
	if g.classifier == nil {
		return false
	}
	if g.classifierFailUntil.IsZero() {
		return true
	}
	return time.Now().After(g.classifierFailUntil)
}

// SetClassifier installs (or removes, on nil) a YoloClassifier.
// Thread-safe; callable mid-session via /yolo on or similar.
func (g *Gate) SetClassifier(c YoloClassifier) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.classifier = c
}

func (g *Gate) Mode() Mode {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode
}

func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = m
}

// AppendRules adds rules to the bottom of the stack (lowest precedence).
// To override, push new rules later — they win on first match.
func (g *Gate) AppendRules(rules ...Rule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rules = append(g.rules, rules...)
}

// PopRules drops the trailing n rules from the stack. Used by the cron
// scheduler to install a temporary deny-list for a single firing
// (CronJob.DisabledTools, Hermes pattern) and tear it back down after
// the run, without leaking job-specific deny rules into other jobs.
//
// Best-effort: if n exceeds the current stack depth, the stack is just
// emptied. Concurrent AppendRules / PopRules from multiple goroutines
// are serialised on the same mutex.
func (g *Gate) PopRules(n int) {
	if n <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n >= len(g.rules) {
		g.rules = nil
		return
	}
	g.rules = g.rules[:len(g.rules)-n]
}

// Check evaluates a tool invocation against the current mode + rules.
// stringInput is a flattened representation used for substring matching.
//
// Precedence (mirrors claude-code permissions.ts:1158-1320):
//  1. Hard modes (Plan / Deny) override everything: user-safety stances
//     that mustn't be defeated by a leftover "Yes always" rule.
//  2. Safety-check paths (.git/, .ssh/, ~/.bashrc, ...) → ASK even in
//     Bypass mode. These are bypass-immune by virtue of the path: the
//     model writing to ~/.bashrc is always worth a human glance.
//  3. Denial-fallback breaker: if the gate has been denying repeatedly
//     (consecutive ≥3 OR total ≥20), force ASK so a human breaks the
//     auto-deny loop.
//  4. Declarative rules win (user set them on purpose).
//  5. Mode-default fallthrough.
func (g *Gate) Check(_ context.Context, tool, stringInput string) (Decision, string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Hard modes — applied BEFORE rules so leftover allow rules can't
	// punch a hole through plan/deny.
	switch g.mode {
	case ModeDeny:
		g.recordDenial()
		return DecisionDeny, "mode:deny"
	case ModePlan:
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			g.recordAllow()
			return DecisionAllow, "mode:plan"
		default:
			g.recordDenial()
			return DecisionDeny, "mode:plan"
		}
	}

	// Denial-fallback circuit breaker. Once tripped, force ASK so a
	// human pulls us out — but DON'T early-return for ALLOW outcomes
	// (rules below may still allow legitimately, breaker is about
	// denials only). We check it after rules below by capturing the
	// rule decision and downgrading DENY → ASK if breaker is hot.
	breakerActive := !g.denialFallbackUntil.IsZero() && time.Now().Before(g.denialFallbackUntil)

	// Safety-check paths: bypass-immune via path pattern. Applies only
	// to tools that touch the filesystem / shell. Even in mode=Bypass,
	// writing to .ssh/ or .git/config gets a human in the loop.
	if isFileTouchingTool(tool) && matchesSafetyPath(stringInput) {
		// Don't recordDenial here — safety ASK is informational, not
		// a "rule denied" signal. Repeated touches of .git/ shouldn't
		// trip the denial breaker.
		return DecisionAsk, "safety_check:bypass_immune"
	}

	// Iterate in reverse — later rules (higher precedence) win.
	for i := len(g.rules) - 1; i >= 0; i-- {
		r := g.rules[i]
		if r.Tool != "*" && r.Tool != tool {
			continue
		}
		if r.Match != "" && !strings.Contains(stringInput, r.Match) {
			continue
		}
		return g.applyBreaker(r.Verb, r.Source, breakerActive)
	}

	switch g.mode {
	case ModeBypass:
		// Pre-filter: DangerousPatterns hard-blacklist runs BEFORE
		// the LLM classifier. These shapes ("rm -rf /", "fork bomb",
		// "kill -9 -1", etc.) have no defensible reason for an agent
		// to ever propose them; we fail-CLOSED here even in bypass
		// mode — claude-code-sourcemap and openclaude both do the
		// same. See dangerous_patterns.go for the full list.
		if hit := CheckDangerousPattern(stringInput); hit != nil {
			return g.applyBreaker(DecisionDeny, "yolo:dangerous_pattern:"+hit.Reason, breakerActive)
		}
		// YOLO classifier (claude-code parity): give the classifier a
		// chance to soft- or hard-deny even though the user opted into
		// bypass.
		//
		// Fail-closed cache: if the classifier has errored recently,
		// skip it for 30 minutes — otherwise every tool call burns a
		// doomed retry. We still fail-OPEN on the error itself
		// (returning ALLOW) because the user explicitly chose bypass;
		// classifier outage shouldn't lock them out, just stop
		// hammering the endpoint.
		if g.classifierUsable() {
			v, src, err := g.runClassifier(context.Background(), tool, stringInput)
			if err != nil {
				g.markClassifierFailed()
			} else {
				switch v {
				case YoloHardDeny:
					return g.applyBreaker(DecisionDeny, "yolo:hard_deny:"+src, breakerActive)
				case YoloSoftDeny:
					return DecisionAsk, "yolo:soft_deny:" + src
				}
			}
		}
		g.recordAllow()
		return DecisionAllow, "mode:bypass"
	case ModeAcceptEdits:
		// Auto-allow read-only AND project-local writes/edits. Bash
		// still falls through to ASK so commands aren't auto-run.
		// This sits between Auto (asks for any write) and Bypass
		// (allows anything). claude-code's acceptEdits.
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			g.recordAllow()
			return DecisionAllow, "mode:acceptEdits"
		case "Edit", "Write", "NotebookEdit":
			// Local-write fast path. Path-level safetyCheck above
			// already filtered out .git / .ssh / etc. before
			// reaching here. Outside-project paths still get a
			// chance to hit the safetyCheck list (which we did),
			// so we trust this layer.
			g.recordAllow()
			return DecisionAllow, "mode:acceptEdits"
		}
	}
	return DecisionAsk, "default"
}

// applyBreaker downgrades DENY → ASK when the denial-tracking breaker
// is hot. Idempotent for ALLOW / ASK.
func (g *Gate) applyBreaker(verb Decision, src string, breakerActive bool) (Decision, string) {
	switch verb {
	case DecisionDeny:
		if breakerActive {
			return DecisionAsk, "denial_breaker:" + src
		}
		g.recordDenial()
		return DecisionDeny, src
	case DecisionAllow:
		g.recordAllow()
	}
	return verb, src
}

// Remember marks a tool+input pair as approved for the rest of the session.
// Used when the user says "always allow this kind of call".
func (g *Gate) Remember(tool, stringInput string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.memoAllow[tool+"\x00"+stringInput] = true
}

func (g *Gate) Remembered(tool, stringInput string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.memoAllow[tool+"\x00"+stringInput]
}

// Snapshot returns a copy of the current rule stack. Used by --resume to
// persist "always allow" decisions across sessions.
func (g *Gate) Snapshot() []Rule {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Rule, len(g.rules))
	copy(out, g.rules)
	return out
}
