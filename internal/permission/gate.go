// Package permission implements cascading permission rules inspired by
// Claude Code's settings precedence (CLI > local > project > user > policy).
// Modes include Claude Code's public permission modes plus METIS fullAccess,
// the Codex-style no-approval/no-sandbox posture.
package permission

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is the global gating posture for a session.
type Mode string

const (
	ModeDefault           Mode = "default"           // allow read-only tools; ask before state changes
	ModeAcceptEdits       Mode = "acceptEdits"       // auto-allow edits; ask for other state changes
	ModeBypassPermissions Mode = "bypassPermissions" // approve tool calls (dangerous; explicit opt-in)
	ModeFullAccess        Mode = "fullAccess"        // no approval prompts and no process sandbox
	ModePlan              Mode = "plan"              // read-only exploration until the plan is approved
	ModeDontAsk           Mode = "dontAsk"           // allow what is already allowed; deny instead of prompting

	// Deprecated source-level aliases. Persisted sessions and user config
	// from Metis <=0.2.8 used ask/bypass/deny. Keep Go callers compiling,
	// while every value written back to disk and shown in the UI is the
	// Claude Code canonical spelling.
	ModeAsk    = ModeDefault
	ModeBypass = ModeBypassPermissions
	ModeDeny   = ModeDontAsk
)

// Modes is the canonical public set. Shift+Tab intentionally uses CycleModes
// below: dontAsk and fullAccess remain explicit selections so an accidental
// keypress cannot enter either non-interactive or host-unrestricted posture.
var Modes = []Mode{
	ModeAcceptEdits,
	ModeBypassPermissions,
	ModeDefault,
	ModeDontAsk,
	ModeFullAccess,
	ModePlan,
}

// CycleModes matches Claude Code's public Shift+Tab cycle when bypass mode is
// available: default -> acceptEdits -> plan -> bypassPermissions -> default.
var CycleModes = []Mode{
	ModeDefault,
	ModeAcceptEdits,
	ModePlan,
	ModeBypassPermissions,
}

// ParseMode validates a user/session value and returns its canonical form.
// Legacy Metis spellings remain accepted as read-time aliases only.
func ParseMode(raw string) (Mode, bool) {
	switch strings.TrimSpace(raw) {
	case "default", "ask":
		return ModeDefault, true
	case "acceptEdits":
		return ModeAcceptEdits, true
	case "bypassPermissions", "bypass":
		return ModeBypassPermissions, true
	case "fullAccess", "full":
		return ModeFullAccess, true
	case "plan":
		return ModePlan, true
	case "dontAsk", "deny":
		return ModeDontAsk, true
	default:
		return "", false
	}
}

// CanonicalMode normalizes known values and safely falls back to default for
// corrupt/unknown persisted state. CLI and slash-command entry points should
// call ParseMode first so typos are reported instead of silently falling back.
func CanonicalMode(raw string) Mode {
	if mode, ok := ParseMode(raw); ok {
		return mode
	}
	return ModeDefault
}

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
	// modeTransitionMu serializes complete runtime-owned transitions (Gate,
	// sandbox posture, and plan controller). Direct SetMode calls remain
	// non-blocking behind an in-flight listener; they supersede the requested
	// mode and are observed by the transition owner after the listener drain.
	modeTransitionMu sync.RWMutex
	// modeTransitionWriters closes the declaration-to-Lock race for TryRLock:
	// once a writer announces intent, a new tool batch must not barge in even
	// if it reaches TryRLock just before the writer goroutine calls Lock.
	modeTransitionWriters atomic.Int64
	// transitionHolds keeps Check fail-closed across work that lives outside
	// this package, such as applying the matching sandbox posture. The
	// notification bit separately covers plain SetMode/ResetSessionState calls
	// until every coalesced listener callback has finished.
	transitionHolds      int
	modeNotifyTransition bool
	// transitionEpoch invalidates dispatcher work prepared under an older
	// permission posture. A current transition bit alone is insufficient: a
	// mode change can start and finish while a slow hook or CanUse call is
	// running, leaving that call with a stale ALLOW from the previous mode.
	transitionEpoch uint64
	// nextScopedRuleID gives each temporary rule set an identity that can be
	// removed precisely. A simple AppendRules + PopRules pair is unsafe across
	// an agent turn: an interactive "always allow" may be appended while the
	// turn is running, and PopRules would then remove the user's new rule rather
	// than the command-scoped one.
	nextScopedRuleID uint64

	// modeNotifyMu serializes delivery of mode changes without holding mu.
	// SetMode can be called concurrently by the UI and an in-flight plan tool;
	// invoking captured callbacks directly allowed an older callback to land
	// after a newer one and leave Loop.planMode out of sync with Gate.mode.
	// The small pending-drain state also permits a listener to call SetMode
	// re-entrantly: the nested update is coalesced and delivered after the
	// current callback returns instead of deadlocking.
	modeNotifyMu      sync.Mutex
	modeNotifyRunning bool
	modeNotifyPending bool
	modeNotifySettled chan struct{}
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

	// pathScopeHook answers whether a concrete filesystem target belongs to
	// the launch cwd or an explicitly added directory. Only callers using
	// CheckPath opt into this boundary; Bash intentionally stays out because
	// reliably extracting every shell-touched path requires shell parsing.
	// nil preserves the historical unrestricted behavior for embedders and
	// tests that construct a Gate without the CLI runtime.
	pathScopeHook func(path string) bool

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
		mode:         CanonicalMode(string(mode)),
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

// SetPathScopeHook installs the runtime's filesystem-scope resolver. The hook
// is evaluated outside the Gate lock so it may safely maintain its own dynamic
// /add-dir state. Passing nil disables scope enforcement.
func (g *Gate) SetPathScopeHook(fn func(path string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pathScopeHook = fn
}

func (g *Gate) pathInScope(path string) (enforced, inScope bool) {
	g.mu.RLock()
	fn := g.pathScopeHook
	g.mu.RUnlock()
	if fn == nil || strings.TrimSpace(path) == "" {
		return false, true
	}
	return true, fn(path)
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

// ResetDenials clears all denial state at an explicit session boundary. The
// canonical `/clear` command and its `/new` and `/reset` aliases all start a
// fresh session, whose activation calls this method.
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
		mode:          g.mode,
		rules:         rulesCopy,
		memoAllow:     make(map[string]bool),
		classifier:    g.classifier,
		readOnlyHook:  g.readOnlyHook,
		pathScopeHook: g.pathScopeHook,
		denialLimits:  g.denialLimits,
		// Counters + breakers start fresh by design.
	}
}

func (g *Gate) SetMode(m Mode) {
	m = CanonicalMode(string(m))
	g.commitModeChange(func() {
		g.mode = m
	})
}

// SetModeAndWait commits m and waits until the ordered listener drain has
// settled. It is intended for the runtime transition coordinator, which must
// not apply a fallback sandbox posture based on a mode that a concurrent or
// re-entrant listener already superseded. Ordinary callers should use SetMode:
// waiting from inside a mode listener would wait on the listener itself.
func (g *Gate) SetModeAndWait(m Mode) {
	if g == nil {
		return
	}
	m = CanonicalMode(string(m))
	settled := g.commitModeChange(func() {
		g.mode = m
	})
	<-settled
}

// RunModeTransition serializes a complete Gate-owned permission transition
// and keeps tool checks fail-closed until fn has also reconciled external
// state such as the process sandbox. SetMode remains independently usable and
// may supersede fn's requested mode; fn must verify Gate.Mode after waiting for
// its listener drain. RunModeTransition is intentionally not re-entrant: code
// already running inside fn must use the raw SetMode/SetModeAndWait primitives
// instead of trying to acquire the write-side coordinator again.
func (g *Gate) RunModeTransition(fn func() error) error {
	if g == nil {
		if fn == nil {
			return nil
		}
		return fn()
	}
	g.modeTransitionWriters.Add(1)
	g.modeTransitionMu.Lock()
	g.modeTransitionWriters.Add(-1)
	defer g.modeTransitionMu.Unlock()

	g.mu.Lock()
	g.transitionHolds++
	g.transitionEpoch++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.transitionHolds--
		g.mu.Unlock()
	}()
	if fn == nil {
		return nil
	}
	return fn()
}

// TryAcquireToolDispatchLease admits one complete dispatcher batch on the
// read side of the permission-transition barrier. RunModeTransition owns the
// write side. TryRLock is deliberate: if a transition is active or merely
// queued behind an older batch, a new batch fails closed immediately instead
// of reader-barging ahead of the waiting writer.
//
// The caller must hold the returned lease through schema, hooks, CanUse, ASK,
// Execute, and PostToolUse. A nil Gate is admitted for reduced embedders.
func (g *Gate) TryAcquireToolDispatchLease() (release func(), allowed bool, reason string) {
	if g == nil {
		return func() {}, true, ""
	}
	if g.modeTransitionWriters.Load() > 0 {
		return nil, false, "mode:transition"
	}
	if !g.modeTransitionMu.TryRLock() {
		return nil, false, "mode:transition"
	}
	if g.modeTransitionWriters.Load() > 0 {
		g.modeTransitionMu.RUnlock()
		return nil, false, "mode:transition"
	}
	g.mu.RLock()
	transitioning := g.modeNotifyTransition || g.transitionHolds > 0
	g.mu.RUnlock()
	if transitioning {
		g.modeTransitionMu.RUnlock()
		return nil, false, "mode:transition"
	}
	return g.modeTransitionMu.RUnlock, true, ""
}

// commitModeChange atomically marks the Gate transitional and mutates its mode
// state under g.mu, then joins or starts the ordered callback drain. Registering
// the commit under modeNotifyMu closes the old race where a new mode could land
// after the drainer decided it was idle but before it cleared the fail-closed
// marker.
func (g *Gate) commitModeChange(commit func()) <-chan struct{} {
	g.modeNotifyMu.Lock()
	g.mu.Lock()
	g.modeNotifyTransition = true
	g.transitionEpoch++
	commit()
	g.mu.Unlock()
	if g.modeNotifyRunning {
		g.modeNotifyPending = true
		settled := g.modeNotifySettled
		g.modeNotifyMu.Unlock()
		return settled
	}
	g.modeNotifyRunning = true
	g.modeNotifySettled = make(chan struct{})
	settled := g.modeNotifySettled
	g.modeNotifyMu.Unlock()

	g.drainModeChanges()
	return settled
}

// drainModeChanges delivers the latest committed mode in commit order. Calls
// made while another callback is running are coalesced; every sequential call
// still fires synchronously, while concurrent/re-entrant updates return
// promptly and are guaranteed to finish with the listener observing the final
// Gate.mode. The transition guard is cleared only while modeNotifyMu excludes
// a new unregistered commit.
func (g *Gate) drainModeChanges() {
	// A runtime listener is trusted composition code, but a panic must not leave
	// every future tool dispatch denied and every SetModeAndWait blocked forever.
	// Restore the notifier invariants, wake all callers joined to this drain, and
	// then re-panic so the caller's normal crash boundary still observes the
	// original failure. Keep the admission marker fail-closed: the listener may
	// have panicked before it reconciled an external sandbox, so admitting tools
	// against the newly committed Gate mode could expose a torn posture. A later
	// successful transition notifies the listener again and clears the marker.
	defer func() {
		if recovered := recover(); recovered != nil {
			g.modeNotifyMu.Lock()
			g.modeNotifyPending = false
			g.modeNotifyRunning = false
			settled := g.modeNotifySettled
			g.modeNotifySettled = nil
			if settled != nil {
				close(settled)
			}
			g.modeNotifyMu.Unlock()
			panic(recovered)
		}
	}()
	for {
		g.mu.RLock()
		mode := g.mode
		listener := g.onModeChange
		g.mu.RUnlock()
		if listener != nil {
			listener(mode)
		}

		g.modeNotifyMu.Lock()
		if g.modeNotifyPending {
			g.modeNotifyPending = false
			g.modeNotifyMu.Unlock()
			continue
		}
		g.mu.Lock()
		g.modeNotifyTransition = false
		g.mu.Unlock()
		g.modeNotifyRunning = false
		settled := g.modeNotifySettled
		g.modeNotifySettled = nil
		close(settled)
		g.modeNotifyMu.Unlock()
		return
	}
}

// ResetSessionState crosses a top-level chat-session boundary without
// rebuilding the Gate. Process-scoped wiring (managed/config/CLI/persistent
// rules, classifier, read-only hook, mode listener and denial limits) stays in
// place, while state granted or accumulated by the previous interactive
// session is discarded.
//
// resumedRules must contain only rules loaded from the destination session.
// Callers should tag them with a "session:" source so a later switch can
// discard them again. The replacement is committed under one lock, preventing
// a concurrent permission check from observing a half-cleared rule stack.
func (g *Gate) ResetSessionState(mode Mode, resumedRules []Rule) {
	_ = g.resetSessionState(mode, resumedRules)
}

// ResetSessionStateAndWait performs the same atomic session-state replacement
// as ResetSessionState and additionally waits for the shared ordered listener
// drain to settle. Session activation boundaries should use this form before
// exposing the destination session to tool dispatch. beforeCommit runs under
// the same transition coordinator immediately before
// the Gate commit; callers use it to install plan lineage that the mode
// listener needs while reconciling a restored Plan posture. afterSettled runs
// under that same coordinator after the complete listener drain has settled,
// but before the write-side transition lease is released. This lets callers
// repair controller state after a listener fail-closed downgrade without
// exposing a torn Gate/lineage snapshot to a queued persistence writer.
//
// Ordinary callers retain ResetSessionState's compatibility behavior: a reset
// that joins an already running listener drain returns promptly. Like
// SetModeAndWait, this method must not be called from inside the mode listener
// itself.
func (g *Gate) ResetSessionStateAndWait(mode Mode, resumedRules []Rule, beforeCommit, afterSettled func()) {
	if g == nil {
		return
	}
	_ = g.RunModeTransition(func() error {
		if beforeCommit != nil {
			beforeCommit()
		}
		settled := g.resetSessionState(mode, resumedRules)
		if settled != nil {
			<-settled
		}
		if afterSettled != nil {
			afterSettled()
		}
		return nil
	})
}

func (g *Gate) resetSessionState(mode Mode, resumedRules []Rule) <-chan struct{} {
	if g == nil {
		return nil
	}
	mode = CanonicalMode(string(mode))
	return g.commitModeChange(func() {
		kept := g.rules[:0]
		for _, rule := range g.rules {
			if isSessionRuleSource(rule.Source) {
				continue
			}
			kept = append(kept, rule)
		}
		// Detach from the old backing array before appending destination rules.
		// Besides making the ownership boundary explicit, this prevents a caller's
		// later mutation of resumedRules from changing the live Gate.
		g.rules = append([]Rule(nil), kept...)
		g.rules = append(g.rules, resumedRules...)
		g.mode = mode
		g.memoAllow = make(map[string]bool)
		g.consecutiveDenials = 0
		g.totalDenials = 0
		g.denialFallbackUntil = time.Time{}
	})
}

// ToolDispatchAdmission returns a stable permission-transition epoch and
// whether a dispatcher may currently enter an untrusted tool boundary. A
// dispatcher must capture the epoch before consulting tool schema or hooks,
// then require the same epoch before CanUse and Execute. This rejects both an
// active transition and an ALLOW prepared under a transition that started and
// settled while an earlier boundary was running.
//
// The nil receiver is intentionally admitted for reduced embedders that do not
// install a Gate.
func (g *Gate) ToolDispatchAdmission() (epoch uint64, allowed bool, reason string) {
	if g == nil {
		return 0, true, ""
	}
	if g.modeTransitionWriters.Load() > 0 {
		g.mu.RLock()
		epoch = g.transitionEpoch
		g.mu.RUnlock()
		return epoch, false, "mode:transition"
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.modeNotifyTransition || g.transitionHolds > 0 {
		return g.transitionEpoch, false, "mode:transition"
	}
	return g.transitionEpoch, true, ""
}

// isSessionRuleSource identifies grants whose lifetime is one interactive
// chat. "interactive" is used by /allow and the permission prompt; resumed
// rules are normalized to "session:*" at the persistence boundary.
func isSessionRuleSource(source string) bool {
	return source == "interactive" || strings.HasPrefix(source, "session:")
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

// PushScopedRules installs rules for a bounded operation and returns an
// idempotent cleanup function that removes exactly those rules, even if other
// rules are appended in the meantime. The caller-provided Source is replaced
// with a unique session-scoped source so managed-policy and CLI rules retain
// their higher authority.
//
// This is used by custom-command `allowed-tools`: the entries are temporary
// pre-approvals for one Loop.Run, not persistent permission changes.
func (g *Gate) PushScopedRules(rules ...Rule) func() {
	if g == nil || len(rules) == 0 {
		return func() {}
	}

	g.mu.Lock()
	g.nextScopedRuleID++
	source := "session:scoped-command:" + strconv.FormatUint(g.nextScopedRuleID, 10)
	for i := range rules {
		rules[i].Source = source
	}
	g.rules = append(g.rules, rules...)
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			kept := g.rules[:0]
			for _, rule := range g.rules {
				if rule.Source != source {
					kept = append(kept, rule)
				}
			}
			g.rules = kept
			g.mu.Unlock()
		})
	}
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
//  1. Plan overrides user rules, but read-only candidates still pass the
//     bypass-immune safety/secret checks before they are auto-allowed.
//  2. Explicit ASK/DENY rules win. An explicit ALLOW remains subject to the
//     bypass-immune safety checks below.
//  3. Safety-check writes (.git/, .ssh/, ~/.bashrc, ...) and credential
//     reads are bypass-immune. Interactive modes ASK; bypassPermissions
//     silently DENIES because that mode must never open an approval UI.
//  4. Denial-fallback breaker: if the gate has been denying repeatedly
//     (consecutive ≥3 OR total ≥20), force ASK so a human breaks the
//     auto-deny loop.
//  5. Explicit ALLOW rules and mode-default fallthrough.
func (g *Gate) Check(ctx context.Context, tool, stringInput string) (Decision, string) {
	return g.check(ctx, tool, stringInput, false)
}

// CheckPath is Check plus the cwd/--add-dir boundary for one concrete target.
// Declarative rules retain their normal precedence, so a deliberate user
// allow can grant an outside path. ModePlan remains stronger for mutations:
// its Edit/Write/NotebookEdit denial cannot be overridden by a scope grant.
func (g *Gate) CheckPath(ctx context.Context, tool, stringInput, path string) (Decision, string) {
	enforced, inScope := g.pathInScope(path)
	outOfScope := enforced && !inScope
	if tool == "Grep" {
		if secret, covered := metisSecretCoveredByRoot(path); covered {
			stringInput += "\n" + secret
		}
	}
	// Resolve the nearest existing ancestor before the secret-path check. The
	// leaf itself may not exist yet, but Read/Grep and future file creation still
	// follow a symlinked parent at execution time.
	if abs, resolved, ok := resolvePathThroughExistingParents(path); ok && resolved != abs {
		stringInput += "\n" + resolved
		// Scope must be checked against the path the OS will actually touch.
		// A workspace-local symlink to an outside directory is otherwise an
		// implicit escape from cwd/--add-dir for both reads and writes.
		if resolvedEnforced, resolvedInScope := g.pathInScope(resolved); resolvedEnforced && !resolvedInScope {
			outOfScope = true
		}
		if tool == "Grep" {
			if secret, covered := metisSecretCoveredByRoot(resolved); covered {
				stringInput += "\n" + secret
			}
		}
	}
	return g.check(ctx, tool, stringInput, outOfScope)
}

// resolvePathThroughExistingParents resolves symlinks as far as the filesystem
// permits, then reattaches any missing suffix. filepath.EvalSymlinks requires
// the full path to exist and therefore cannot by itself protect a missing
// credential leaf under a symlinked METIS_HOME alias.
func resolvePathThroughExistingParents(path string) (abs, resolved string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", false
	}
	cursor := filepath.Clean(abs)
	var missing []string
	for {
		if existing, err := filepath.EvalSymlinks(cursor); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				existing = filepath.Join(existing, missing[i])
			}
			return abs, filepath.Clean(existing), true
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return abs, abs, true
		}
		missing = append(missing, filepath.Base(cursor))
		cursor = parent
	}
}

func (g *Gate) check(_ context.Context, tool, stringInput string, outOfScope bool) (decision Decision, source string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.modeNotifyTransition || g.transitionHolds > 0 {
		return DecisionDeny, "mode:transition"
	}
	// Both unattended postures guarantee that no decision escapes as ASK.
	// bypassPermissions converts secret/safety prompts to DENY; fullAccess skips
	// those implicit boundaries below but still honors an explicit ask/deny rule
	// as a silent refusal.
	defer func() {
		if (g.mode == ModeBypassPermissions || g.mode == ModeFullAccess) && decision == DecisionAsk {
			decision = DecisionDeny
		}
	}()

	// Plan is applied BEFORE rules so leftover allow rules cannot punch
	// a state-changing hole through the read-only planning boundary.
	switch g.mode {
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
		//       leave, or query MetisInfo to introspect. plan mode became "blanket
		//       deny with five exceptions" — not actually plannable.
		//
		// New policy (in order):
		//   1. Plan-mode meta tools always pass (EnterPlanMode is a
		//      no-op when already in plan; ExitPlanMode is the only
		//      way out — denying it would be a trap).
		//   2. The runtime's readOnlyHook decides tools with no side
		//      effects (queries the registry's IsReadOnly). This covers
		//      MetisInfo, Agent, read-only output/query tools, Skill, LSP,
		//      WebFetch, etc. TodoWrite and Fork intentionally remain denied.
		//   3. Before allowing a read-only candidate, apply the same
		//      bypass-immune path and secret-read checks used by other modes.
		//   4. A fallback allowlist supports headless/test paths with no hook.
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
		allowSource := ""
		if g.callReadOnlyHookLocked(tool, stringInput) {
			allowSource = "mode:plan:readonly"
		} else {
			switch tool {
			case "Read", "LS", "Glob", "Grep", "WebFetch":
				allowSource = "mode:plan:fallback"
			}
		}
		if allowSource != "" {
			// A read-only Bash command can still expose a bypass-immune path
			// (for example `cat ~/.ssh/config`). Likewise, Read can leak a
			// private key. Plan mode must not skip those checks merely because
			// its hard boundary is evaluated before normal rules.
			if isBypassImmuneSafetyAttempt(tool, stringInput) {
				return DecisionAsk, "safety_check:bypass_immune"
			}
			if isSecretReadAttempt(tool, stringInput) {
				return DecisionAsk, "secret_read:bypass_immune"
			}
			if outOfScope {
				// Plan ignores ordinary rules as an execution boundary, but an
				// explicit matching ALLOW may expand where its read-only tools
				// explore. A higher-authority matching DENY still wins here.
				if idx, ok := g.bestMatchingRuleLocked(tool, stringInput); !ok || g.rules[idx].Verb != DecisionAllow {
					return DecisionAsk, "scope:outside"
				}
			}
			g.recordAllow()
			return DecisionAllow, allowSource
		}

		g.recordDenial()
		return DecisionDeny, "mode:plan"
	}

	// Denial-fallback circuit breaker. Once tripped, force ASK so a
	// human pulls us out — but DON'T early-return for ALLOW outcomes
	// (rules below may still allow legitimately, breaker is about
	// denials only). We check it after rules below by capturing the
	// rule decision and downgrading DENY → ASK if breaker is hot.
	breakerActive := !g.denialFallbackUntil.IsZero() && time.Now().Before(g.denialFallbackUntil)

	// Resolve explicit ASK/DENY before implicit safety-path allowances. This is
	// especially important in bypassPermissions: classifying a sensitive-path
	// Bash command as read-only must not erase a deliberate policy/user rule.
	// Explicit ALLOW is intentionally deferred until after the bypass-immune
	// safety checks, so it cannot authorize a write to ~/.ssh or ~/.claude.
	bestIdx, matched := g.bestMatchingRuleLocked(tool, stringInput)
	if matched && g.rules[bestIdx].Verb != DecisionAllow {
		r := g.rules[bestIdx]
		decision, source := g.applyBreaker(r.Verb, r.Source, breakerActive)
		if g.mode == ModeDontAsk && decision == DecisionAsk {
			g.recordDenial()
			return DecisionDeny, "mode:dontAsk:" + source
		}
		return decision, source
	}

	// fullAccess is the Codex-style combination of approval=never and an
	// unrestricted process sandbox. It deliberately skips METIS's implicit
	// protected-path, credential-read, workspace-scope, dangerous-pattern, and
	// classifier checks. Explicit policy/user rules above and tool/hook errors
	// still apply; this mode changes authorization, not argument validity.
	if g.mode == ModeFullAccess {
		g.recordAllow()
		return DecisionAllow, "mode:fullAccess"
	}

	// Safety-check paths: bypass-immune via path pattern. Applies only
	// to tools that touch the filesystem / shell. In interactive modes,
	// writing to .ssh/ or .git/config gets a human in the loop; bypass
	// converts the resulting ASK to a silent DENY. Unlike
	// the old path-substring-only check, a Bash command that merely reads
	// a sensitive agent directory can pass in bypass mode; Claude Code's
	// Bash path validation makes the same read/write operation distinction.
	safetyPathHit := isBypassImmuneSafetyAttempt(tool, stringInput)
	readOnlyBypassBash := g.mode == ModeBypassPermissions && tool == "Bash" && IsReadOnlyBashSafetyOperation(expandKnownCredentialRoots(stringInput))
	if safetyPathHit && !readOnlyBypassBash {
		// Don't recordDenial here — safety ASK is informational, not
		// a "rule denied" signal. Repeated touches of .git/ shouldn't
		// trip the denial breaker.
		if g.mode == ModeDontAsk {
			g.recordDenial()
			return DecisionDeny, "mode:dontAsk:safety_check"
		}
		return DecisionAsk, "safety_check:bypass_immune"
	}
	// Reading a credential file leaks the secret into the model context /
	// transcript / provider request. Read of a secret path (~/.ssh/id_*,
	// ~/.aws/credentials, …) is gated to ASK in interactive modes and a
	// silent DENY in bypass, where read-only tools are otherwise auto-allowed.
	if isSecretReadAttempt(tool, stringInput) {
		if g.mode == ModeDontAsk {
			g.recordDenial()
			return DecisionDeny, "mode:dontAsk:secret_read"
		}
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
	if matched {
		r := g.rules[bestIdx]
		decision, source := g.applyBreaker(r.Verb, r.Source, breakerActive)
		if g.mode == ModeDontAsk && decision == DecisionAsk {
			g.recordDenial()
			return DecisionDeny, "mode:dontAsk:" + source
		}
		return decision, source
	}

	// Scope is a permission boundary, not a hard blacklist: ordinary modes
	// ask the user before touching a path outside cwd/--add-dir, dontAsk turns
	// that prompt into a denial, and the explicitly dangerous bypass mode keeps
	// its documented unrestricted behavior. Matching declarative rules were
	// resolved above, so a deliberate allow/deny still takes precedence.
	if outOfScope {
		switch g.mode {
		case ModeBypassPermissions:
			// Continue into the bypass classifier/dangerous-pattern checks.
		case ModeDontAsk:
			g.recordDenial()
			return DecisionDeny, "mode:dontAsk:scope"
		default:
			return DecisionAsk, "scope:outside"
		}
	}

	switch g.mode {
	case ModeBypassPermissions:
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
					// 2026-07-26: collapsed to ALLOW in bypass mode.
					// claude-code's auto-mode classifier has only two
					// terminal verdicts — shouldBlock:true (deny) and
					// shouldBlock:false (allow). There is no third
					// "ambiguous → prompt the human" state; ambiguous
					// falls into shouldBlock:false. metis used to ship
					// a YoloSoftDeny → DecisionAsk branch, which meant
					// bypassPermissions mode STILL popped a permission
					// dialog every time the classifier wasn't sure —
					// exactly the UX claude-code's bypassPermissions
					// avoids (user feedback: bypass should not prompt).
					// Hard deny stays hard; soft deny joins the allow path.
					_ = src
				}
			}
		}
		g.recordAllow()
		return DecisionAllow, "mode:bypassPermissions"
	case ModeDefault:
		// Auto-allow read-only operations even in default mode — without
		// this, the user has to confirm every Read / Grep / Glob, which
		// is enormous friction for legitimate exploration and matches
		// what claude-code does in its "default" mode (read-only tools
		// have implicit allowlist via isReadOnly). The default semantic
		// is "ask for anything that could change state", not "ask for
		// literally every tool". Writes / Bash / Edit / Memory.add etc.
		// still fall through to DecisionAsk (the default at the bottom).
		// 2026-05-18 fix: pre this, `metis run --mode default 'read X'` in
		// headless mode auto-denied the Read, surprising users.
		if g.callReadOnlyHookLocked(tool, stringInput) {
			g.recordAllow()
			return DecisionAllow, "mode:default:readonly"
		}
		// Hardcoded fallback for test paths without a hook.
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			g.recordAllow()
			return DecisionAllow, "mode:default:readonly_fallback"
		}
	case ModeAcceptEdits:
		// Auto-allow read-only AND project-local writes/edits. Bash
		// still falls through to ASK so commands aren't auto-run.
		// This sits between default (asks for writes) and
		// bypassPermissions (allows ordinary state changes).

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
	case ModeDontAsk:
		// Claude Code's dontAsk is not a blanket deny. It preserves all
		// normal implicit/explicit allows (especially read-only tools), but
		// converts anything that would open an approval prompt into DENY.
		if g.callReadOnlyHookLocked(tool, stringInput) {
			g.recordAllow()
			return DecisionAllow, "mode:dontAsk:readonly"
		}
		switch tool {
		case "Read", "LS", "Glob", "Grep", "WebFetch":
			g.recordAllow()
			return DecisionAllow, "mode:dontAsk:readonly_fallback"
		}
		g.recordDenial()
		return DecisionDeny, "mode:dontAsk"
	}
	return DecisionAsk, "default"
}

// bestMatchingRuleLocked resolves authority first and recency second. Caller
// must hold g.mu. Keeping this in one helper ensures plan's narrow scope-grant
// exception and the ordinary rule path cannot drift apart.
func (g *Gate) bestMatchingRuleLocked(tool, stringInput string) (int, bool) {
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
				break
			}
		}
	}
	return bestIdx, bestIdx >= 0
}

// applyBreaker downgrades DENY → ASK when the denial-tracking breaker
// is hot. The check-level bypass invariant converts that ASK back to a
// silent DENY, so an unattended session can never be forced into a modal.
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
