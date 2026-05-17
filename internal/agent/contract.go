package agent

// contract.go — dispatch-contract enforcement at the agent-loop
// level. Closes the loophole that broke claude-code-go Round-4
// (2026-05-17): the model finished 13 Go files of implementation,
// then end_turn'd without ever calling Agent(subagent_type="verify").
// Tool-layer nudges in TaskUpdate/TodoWrite were bypassed because
// the model only created 1 large in_progress task and never closed
// 3+, so the keyword-based nudge trigger never fired.
//
// claude-code-sourcemap has the same blind spot (verified by
// Explore agent 2026-05-17): its enforcement is also tool-layer
// nudge + prompt MUST clause. No loop-level gate. metis goes one
// step beyond on this axis.
//
// The contract here counts side-effect-producing tool calls
// directly instead of trusting Task* tool usage. Two gates:
//
//   (1) mid-turn one-time SystemReminder when threshold first
//       crosses — heads-up: "you're doing substantial work; plan
//       to verify before you end."
//
//   (2) end-of-turn gate (up to 2 attempts) that holds the loop
//       and forces another turn if the model tries to end without
//       dispatching verify. Escape hatch: prefix the reply with
//       `OVERRIDE CONTRACT: <reason>` to release immediately; the
//       override is logged so the user sees it in the event stream.
//
// Disabled when METIS_CONTRACT_DISABLE=1 — useful for tests and
// one-off workflows where the contract gets in the way.

import (
	"fmt"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const (
	contractWriteThreshold   = 5
	contractAgentThreshold   = 2
	contractMaxGateAttempts  = 2
	contractOverridePhrase   = "OVERRIDE CONTRACT:"
	contractDisableEnvVar    = "METIS_CONTRACT_DISABLE"
	contractVerifySubagentID = "verify"
)

// contractTracker accumulates the side-effect signals one Loop run
// needs to decide whether the dispatch contract should fire. Lives
// on the Loop and is reset per Run via Loop.Reset.
type contractTracker struct {
	mainWrites       int  // Write + Edit + MultiEdit tool_use counts
	agentDispatches  int  // Agent tool_use counts (any subagent_type)
	verifyDispatched bool // true iff one Agent call had subagent_type=verify
	reminderFired    bool // true iff the mid-turn reminder has already fired
	gateAttempts     int  // number of times end-of-turn gate held the loop
}

// observeToolUses tallies the batch the model just emitted. Counted
// at request-time so the tracker reflects intent even if a tool
// fails / is denied — the model still meant to do that work, and
// the verify obligation tracks intent not just success.
func (ct *contractTracker) observeToolUses(toolUses []llm.ContentBlock) {
	for _, tu := range toolUses {
		switch tu.ToolName {
		case "Write", "Edit", "MultiEdit":
			ct.mainWrites++
		case "Agent":
			ct.agentDispatches++
			if st, ok := tu.ToolInput["subagent_type"].(string); ok && st == contractVerifySubagentID {
				ct.verifyDispatched = true
			}
		}
	}
}

// contractDisabled returns true when METIS_CONTRACT_DISABLE=1 is
// set in env. Read each call so test rigs can flip mid-run.
func contractDisabled() bool {
	return os.Getenv(contractDisableEnvVar) == "1"
}

// thresholdMet — substantial work happened. Either ≥5 direct file
// writes (model chose to single-thread) OR ≥2 Agent dispatches
// (model chose to delegate). Both cases warrant a verify pass.
func (ct *contractTracker) thresholdMet() bool {
	return ct.mainWrites >= contractWriteThreshold ||
		ct.agentDispatches >= contractAgentThreshold
}

// shouldFireMidTurnReminder returns the reminder body when the
// threshold was just crossed for the first time, the model hasn't
// already dispatched verify, and we haven't fired before. Empty
// otherwise. Marks reminderFired on a non-empty return so the
// caller doesn't need to track that bit separately.
func (ct *contractTracker) shouldFireMidTurnReminder() string {
	if contractDisabled() {
		return ""
	}
	if !ct.thresholdMet() || ct.verifyDispatched || ct.reminderFired {
		return ""
	}
	ct.reminderFired = true
	return fmt.Sprintf(
		"<system-reminder>\n"+
			"CONTRACT REMINDER (heads-up). You've now dispatched %d Agent "+
			"sub-agent(s) and made %d direct file mutation(s). Per the "+
			"dispatch contract in your base prompt, non-trivial "+
			"implementation MUST end with a verify sub-agent that returns "+
			"`VERDICT: PASS`. Plan your remaining moves so the work ends "+
			"with:\n"+
			"    Agent({subagent_type: \"verify\", prompt: \"<what to check "+
			"+ the VERDICT line you expect back>\"})\n"+
			"Running `go build` / `npm test` / etc. yourself does NOT "+
			"substitute. Only the verifier issues a verdict.\n"+
			"</system-reminder>",
		ct.agentDispatches, ct.mainWrites,
	)
}

// shouldGateEnd runs when the model emits an assistant message with
// stop_reason != tool_use (it's trying to end the turn). Returns
// non-empty body when the gate should HOLD the loop — caller
// injects the body as a user message and continues iterating
// instead of returning. Returns empty when the gate decides the
// model is allowed to end.
//
// Gate releases when ANY of:
//   - threshold not met (small task; nothing to verify)
//   - verify already dispatched
//   - we've already gated MaxGateAttempts times (don't infinite-loop)
//   - assistantText contains the OVERRIDE CONTRACT: escape phrase
//   - env override disables the contract entirely
//
// Increments gateAttempts on a non-empty return so the cap holds.
func (ct *contractTracker) shouldGateEnd(assistantText string) string {
	if contractDisabled() {
		return ""
	}
	if !ct.thresholdMet() || ct.verifyDispatched {
		return ""
	}
	if ct.gateAttempts >= contractMaxGateAttempts {
		return ""
	}
	if strings.Contains(assistantText, contractOverridePhrase) {
		return ""
	}
	ct.gateAttempts++
	return fmt.Sprintf(
		"<system-reminder>\n"+
			"CONTRACT GATE — HALT (attempt %d of %d). You made %d file "+
			"mutation(s) / dispatched %d Agent sub-agent(s) this run, then "+
			"tried to end the turn without spawning a verifier. Per the "+
			"contract, end is not allowed yet. Pick one:\n\n"+
			"  (a) Spawn the verifier now and wait for VERDICT: PASS:\n"+
			"      Agent({subagent_type: \"verify\",\n"+
			"             prompt: \"<what to check + expected VERDICT line>\"})\n\n"+
			"  (b) Override the contract by writing this exact phrase as a "+
			"line in your next reply:\n"+
			"          %s <one-line reason>\n"+
			"      (Logged for audit. Use only when verification genuinely "+
			"does not apply — pure refactors, dry runs, documentation-only "+
			"changes, etc.)\n\n"+
			"After this attempt %d/%d, if you still try to end without "+
			"(a) or (b), the loop releases with a warning so we don't burn "+
			"tokens infinitely.\n"+
			"</system-reminder>",
		ct.gateAttempts, contractMaxGateAttempts,
		ct.mainWrites, ct.agentDispatches,
		contractOverridePhrase,
		ct.gateAttempts, contractMaxGateAttempts,
	)
}

// wasOverridden reports whether the model used the escape phrase
// in its assistant text. Caller uses this to log a one-line event
// so the user sees "model overrode contract because X" in the
// transcript rather than a silent release.
func (ct *contractTracker) wasOverridden(assistantText string) bool {
	return strings.Contains(assistantText, contractOverridePhrase)
}

// reset clears all counters. Called from Loop.Reset so a /clear or
// session restart begins with a clean contract state. Done as a
// pointer-method so the caller doesn't accidentally copy a fresh
// zero value onto the Loop and lose any other state we add later.
func (ct *contractTracker) reset() {
	*ct = contractTracker{}
}

// assistantText joins all text blocks in an assistant message into
// one string. Used by the end-of-turn gate to look for the
// OVERRIDE CONTRACT: escape phrase. Tool-use blocks contribute
// nothing here; only the model's user-facing prose counts.
func assistantText(content []llm.ContentBlock) string {
	var b strings.Builder
	for _, blk := range content {
		if blk.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
