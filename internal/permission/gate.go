// Package permission implements cascading permission rules inspired by
// Claude Code's settings precedence (CLI > local > project > user > policy).
// Modes mirror the operator-friendly defaults: ask / auto / plan / deny.
package permission

import (
	"context"
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

	// readOnlyHook lets ModeAcceptEdits auto-allow ANY tool the
	// runtime tags as read-only — instead of relying on the
	// hardcoded "Read/LS/Glob/Grep/WebFetch + Edit/Write/NotebookEdit"
	// allowlist, which silently broke for tools added later
	// (SubAgentOutput, BashOutput, TaskOutput, Skill, ToolSearch,
	// LSP, MetisInfo, …). The runtime wires this up after the tool
	// registry is built; nil means "fall back to the hardcoded
	// allowlist", which keeps tests and headless paths working.
	//
	// Signature takes BOTH the tool name AND the serialized input —
	// for input-aware tools (Bash, Git) the hook needs the actual
	// argv to decide ("cat foo" auto-allows; "rm -rf" doesn't).
	// Tools with no input-sensitive read/write distinction can
	// ignore the stringInput argument.
	//
	// 2026-05-13 fix for the "acceptEdits still prompts for
	// SubAgentOutput" report: claude-code lets each tool declare
	// isReadOnly() and threads that through the permission decision;
	// metis grew the IsReadOnly capability but the gate never read
	// it.
	readOnlyHook func(toolName, stringInput string) bool

	// onModeChange — fired AFTER SetMode commits the new mode. Used by
	// the runtime to keep dependent state in sync (most importantly:
	// Loop.PlanMode must match Gate.Mode==ModePlan, otherwise users
	// who Shift+Tab into plan get every tool call denied without the
	// loop ever short-circuiting to emit a plan. 2026-05-18 fix for
	// the "plan mode deny-storm" report — see commit message for the
	// full failure mode + root cause analysis.)
	//
	// nil = no listener (default; tests + headless paths that don't
	// build a Loop are happy). Single listener by design — only the
	// runtime owns this side-channel; if more consumers ever need to
	// observe mode changes, layer a fan-out on top.
	onModeChange func(Mode)
}

func New(mode Mode) *Gate {
	return &Gate{
		mode:         mode,
		memoAllow:    make(map[string]bool),
		denialLimits: DefaultDenialLimits,
	}
}

// SetReadOnlyHook installs the runtime's "is tool X read-only?"
// resolver. Called once after the tool registry is built; nil clears
// the hook back to the legacy hardcoded-allowlist behaviour.
//
// Resolution rule: a hook returning true makes ModeAcceptEdits
// auto-allow the call. The hardcoded allowlist still runs as a
// fallback when the hook returns false (or is nil), so existing
// tests that don't wire a registry keep working.
//
// The hook receives the SAME stringInput Check was given (Bash → cmd,
// Git → args, Edit → path, etc.) so input-aware tools can refuse
// auto-allow for dangerous invocations.
func (g *Gate) SetReadOnlyHook(fn func(toolName, stringInput string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readOnlyHook = fn
}

// IsReadOnly is a public, lock-managing wrapper around the runtime's
// readOnlyHook. Returns false when no hook is wired (test paths).
// Used by callers outside the permission package (notably the agent
// Loop) to partition tool batches in plan mode: read-only ones still
// execute (so the model can read code while planning); side-effect
// ones get collected as a plan for user review.
func (g *Gate) IsReadOnly(tool, stringInput string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.callReadOnlyHookLocked(tool, stringInput)
}

// callReadOnlyHookLocked invokes the runtime "is tool X read-only?"
// resolver and returns its verdict, or false when no hook is wired.
// Caller MUST already hold g.mu (Lock or RLock) — Check is the only
// caller and it Lock()s on entry. We deliberately don't grab another
// RLock here: sync.RWMutex is non-reentrant, and the previous
// implementation deadlocked when Check held Lock and tried to RLock
// itself (2026-05-13 test-suite hang).
func (g *Gate) callReadOnlyHookLocked(tool, stringInput string) bool {
	fn := g.readOnlyHook
	if fn == nil {
		return false
	}
	// The hook is user code (registry lookup in the live runtime).
	// Drop the lock for the call so the hook is free to do its own
	// locking without re-entering ours, then take it back. Check's
	// `defer g.mu.Unlock()` keeps the contract.
	g.mu.Unlock()
	defer g.mu.Lock()
	return fn(tool, stringInput)
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

// Clone returns a fresh Gate carrying a SNAPSHOT of this gate's
// state — mode, rules, denial limits, classifier — but with its own
// mutex. Used by sub-agents (G.9, 2026-05-12) so a child loop can
// temporarily flip into `auto` while the parent stays `ask`, without
// the change leaking back into the parent's gate or contending on
// the parent's lock. The clone's memoAllow / denial counters are
// FRESH — each sub-agent starts with a clean "ask once, remember
// for the session" memo, and its denial streak doesn't bleed into
// the parent's circuit breaker state.
//
// The classifier is shared (pointer) — it's a stateless inspector,
// so cloning is unnecessary.
//
// Mirrors claude-code's PermissionGate.fork() pattern.
func (g *Gate) Clone() *Gate {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	rulesCopy := make([]Rule, len(g.rules))
	copy(rulesCopy, g.rules)
	return &Gate{
		mode:         g.mode,
		rules:        rulesCopy,
		memoAllow:    make(map[string]bool),
		classifier:   g.classifier,
		denialLimits: g.denialLimits,
		// Counters + breakers start fresh by design.
	}
}

func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	g.mode = m
	listener := g.onModeChange
	g.mu.Unlock()
	// Fire AFTER releasing the lock so the listener is free to call
	// back into Gate (or any other locked subsystem) without deadlock.
	if listener != nil {
		listener(m)
	}
}

// SetModeChangeListener wires a callback that fires every time SetMode
// is invoked, regardless of whether the mode actually changed (cheaper
// to fire than to diff, and listeners can no-op idempotently). Used by
// the runtime to sync Loop.PlanMode with Gate.Mode==ModePlan.
//
// Replaces any prior listener; pass nil to clear.
func (g *Gate) SetModeChangeListener(fn func(Mode)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onModeChange = fn
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
		// Plan-mode allowlist policy (2026-05-18 expanded):
		//
		// The original allowlist (Read/LS/Glob/Grep/WebFetch) was way
		// too narrow — every other tool got DENY, which created two
		// failure modes:
		//   (a) Loop and Gate go out of sync: Shift+Tab into plan
		//       only flipped Gate.Mode but left Loop.PlanMode=false,
		//       so the loop never short-circuited to emitPlan. Every
		//       tool the model emitted hit Gate and got denied,
		//       trapping it in a deny-storm.
		//   (b) Even with Loop.PlanMode=true, the model couldn't use
		//       AskUser to ask the human, couldn't ExitPlanMode to
		//       leave, couldn't TodoWrite to plan, couldn't query
		//       MetisInfo to introspect. plan mode became "blanket
		//       deny with five exceptions" — not actually plannable.
		//
		// New policy (in order):
		//   1. Plan-mode meta tools always pass (EnterPlanMode is a
		//      no-op when already in plan; ExitPlanMode is the only
		//      way out — denying it would be a trap).
		//   2. The runtime's readOnlyHook decides any tool with no
		//      side effects (queries the registry's IsReadOnly). This
		//      auto-covers AskUser / MetisInfo / Todo* / SubAgent* /
		//      BashOutput / TaskOutput / Skill / LSP / WebFetch / etc.
		//      — same hook ModeAcceptEdits already uses.
		//   3. Fallback legacy allowlist for the headless / test paths
		//      where no hook is wired.
		switch tool {
		case "EnterPlanMode", "ExitPlanMode":
			g.recordAllow()
			return DecisionAllow, "mode:plan:meta"
		case "AskUser":
			// AskUser is the model's only structured channel to ask
			// the human a question. Denying it in plan mode strands
			// the model: it can't propose a plan, can't ask for
			// clarification, just stares at a wall of denies. We
			// deliberately don't mark AskUser as IsReadOnly (it has
			// a side effect — the user's answer flows back into the
			// turn) so acceptEdits doesn't silently bypass it, but
			// plan mode explicitly whitelists it.
			g.recordAllow()
			return DecisionAllow, "mode:plan:askuser"
		}
		if g.callReadOnlyHookLocked(tool, stringInput) {
			g.recordAllow()
			return DecisionAllow, "mode:plan:readonly"
		}
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			g.recordAllow()
			return DecisionAllow, "mode:plan:fallback"
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
	// Reading a credential file leaks the secret into the model context /
	// transcript / provider request. Read of a secret path (~/.ssh/id_*,
	// ~/.aws/credentials, …) is gated to ASK even in ask / acceptEdits /
	// bypass modes, where read-only tools are otherwise auto-allowed below.
	if isSecretReadAttempt(tool, stringInput) {
		return DecisionAsk, "secret_read:bypass_immune"
	}

	// Rule resolution: authority first (sourceRank — policy > cli >
	// interactive > config > persistent), recency second. Among rules
	// of equal authority the LAST appended wins. Scan BACK-to-front
	// with a strict `>`: the first rule seen at a given rank is the
	// latest-appended at that rank, so it wins its tie — equivalent to
	// the historical reverse-iteration semantics. Scanning backward
	// also lets a policy-rank match short-circuit: policy is the
	// ceiling and the first one found here is the latest-appended
	// policy rule, so nothing earlier can outrank or out-recency it.
	// Match grammar: prefix (`git push:*`), glob (`/etc/**`), or legacy
	// substring — see rulematch.go.
	bestIdx, bestRank := -1, -1
	for i := len(g.rules) - 1; i >= 0; i-- {
		r := g.rules[i]
		if r.Tool != "*" && r.Tool != tool {
			continue
		}
		if !MatchesRuleContent(r.Match, stringInput) {
			continue
		}
		if rank := sourceRank(r.Source); rank > bestRank {
			bestRank, bestIdx = rank, i
			if rank == rankPolicy {
				break // ceiling reached at the latest policy rule
			}
		}
	}
	if bestIdx >= 0 {
		r := g.rules[bestIdx]
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
	case ModeAsk:
		// Auto-allow read-only operations even in ask mode — without
		// this, the user has to confirm every Read / Grep / Glob, which
		// is enormous friction for legitimate exploration and matches
		// what claude-code does in its "default" mode (read-only tools
		// have implicit allowlist via isReadOnly). The "ask" semantic
		// is "ask for anything that could change state", not "ask for
		// literally every tool". Writes / Bash / Edit / Memory.add etc.
		// still fall through to DecisionAsk (the default at the bottom).
		// 2026-05-18 fix: pre this, `metis run --mode ask 'read X'` in
		// headless mode auto-denied the Read, surprising users.
		if g.callReadOnlyHookLocked(tool, stringInput) {
			g.recordAllow()
			return DecisionAllow, "mode:ask:readonly"
		}
		// Hardcoded fallback for test paths without a hook.
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			g.recordAllow()
			return DecisionAllow, "mode:ask:readonly_fallback"
		}
	case ModeAcceptEdits:
		// Auto-allow read-only AND project-local writes/edits. Bash
		// still falls through to ASK so commands aren't auto-run.
		// This sits between Auto (asks for any write) and Bypass
		// (allows anything). claude-code's acceptEdits.

		// Registry-driven path (preferred): the runtime hook receives
		// (tool, stringInput) and answers "auto-allow this call?".
		// For metadata-only tools (SubAgentOutput / BashOutput /
		// TaskOutput / Skill / LSP / MetisInfo / ToolSearch / etc.)
		// the hook reads tool.IsReadOnly and returns true.
		//
		// For input-aware tools (Bash, Git) the hook parses the
		// stringInput — `Bash {cmd: "git status"}` auto-allows via
		// permission.IsSafeReadOnlyBash; `Bash {cmd: "rm -rf"}`
		// returns false and falls through to ASK. No hardcoded
		// Bash-skip needed here — the safety lives in the hook
		// implementation, which is closer to the policy it enforces.
		if g.callReadOnlyHookLocked(tool, stringInput) {
			g.recordAllow()
			return DecisionAllow, "mode:acceptEdits:readonly"
		}

		// Legacy hardcoded allowlist — kept as a fallback for tests
		// and headless paths that build a Gate without wiring the
		// registry hook. New tools should rely on the hook above.
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
