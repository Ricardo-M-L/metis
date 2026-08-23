package agent

// contract.go — risk-based verification enforcement at the agent-loop
// level. Runtime signals are more reliable than prompt-only thresholds, so
// the tracker combines mutation scope, implementation fan-out, observed
// validation, and high-impact commands. Two gates:
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
	contractIndependentRiskThreshold = 5
	contractMaxGateAttempts          = 2
	contractOverridePhrase           = "OVERRIDE CONTRACT:"
	contractDisableEnvVar            = "METIS_CONTRACT_DISABLE"
	contractVerifySubagentID         = "verify"
)

// contractTracker accumulates the side-effect signals one Loop run
// needs to decide whether the dispatch contract should fire. Lives
// on the Loop and is reset per Run via Loop.Reset.
type contractTracker struct {
	mainWrites           int // Write + Edit + MultiEdit tool_use counts
	agentDispatches      int // Agent tool_use counts (any subagent_type)
	implementationAgents int
	mutatedFiles         map[string]struct{}
	validationObserved   bool
	highImpactAction     bool
	verifyDispatched     bool // true iff one Agent call had subagent_type=verify
	reminderFired        bool // true iff the mid-turn reminder has already fired
	gateAttempts         int  // number of times end-of-turn gate held the loop

	// Phase B (2026-05-19): track the verify subagent's VERDICT line
	// so the end gate can refuse release on FAIL/PARTIAL/MISSING.
	// Caught on bench-iter10 where the model dispatched verify, got
	// back a FAIL verdict on parser tests, and end_turn'd anyway —
	// the pre-fix shouldGateEnd only checked "did you dispatch?",
	// not "did the verdict actually pass?". Empty string = no verify
	// result observed yet; "MISSING" = verify ran but didn't emit
	// the mandated VERDICT line; others are the literal verdict.
	lastVerifyVerdict   string
	verdictGateAttempts int // separate from gateAttempts so the verdict-gate budget doesn't share with the dispatch-gate budget
}

// observeToolUses tallies the batch the model just emitted. Counted
// at request-time so the tracker reflects intent even if a tool
// fails / is denied — the model still meant to do that work, and
// the verify obligation tracks intent not just success.
func (ct *contractTracker) observeToolUses(toolUses []llm.ContentBlock) {
	for _, tu := range toolUses {
		switch tu.ToolName {
		case "Write", "Edit", "MultiEdit", "NotebookEdit":
			ct.mainWrites++
			ct.validationObserved = false
			if path := mutationPath(tu.ToolInput); path != "" {
				if ct.mutatedFiles == nil {
					ct.mutatedFiles = make(map[string]struct{})
				}
				ct.mutatedFiles[path] = struct{}{}
			}
		case "Bash":
			command := toolInputString(tu.ToolInput, "command", "cmd")
			ct.validationObserved = ct.validationObserved || isValidationCommand(command)
			ct.highImpactAction = ct.highImpactAction || isHighImpactCommand(command)
		case "Agent":
			ct.agentDispatches++
			st, _ := tu.ToolInput["subagent_type"].(string)
			if st == contractVerifySubagentID {
				ct.verifyDispatched = true
			} else if isImplementationAgent(st) {
				ct.implementationAgents++
				ct.validationObserved = false
			}
		}
	}
}

func mutationPath(input map[string]any) string {
	return toolInputString(input, "file_path", "path", "notebook_path")
}

func toolInputString(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isImplementationAgent(subagentType string) bool {
	switch strings.ToLower(strings.TrimSpace(subagentType)) {
	case "", "general", "implement", "implementation", "worker":
		return true
	default:
		return false
	}
}

func isValidationCommand(command string) bool {
	lower := strings.ToLower(command)
	for _, marker := range []string{
		"go test", "go vet", "cargo test", "pytest", "npm test",
		"npm run test", "pnpm test", "yarn test", "bun test",
		"mvn test", "gradle test", "./gradlew test", "make test",
		"golangci-lint", "npm run lint", "pnpm lint", "yarn lint",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isHighImpactCommand(command string) bool {
	lower := strings.ToLower(command)
	for _, marker := range []string{
		"rm -rf", "git push", "gh pr create", "npm publish",
		"goreleaser", "drop table", "drop database", "truncate table",
		"terraform apply", "kubectl apply", "helm upgrade",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// observeToolResults pairs the just-completed tool_results with
// their originating toolUses, extracts the VERDICT line from any
// verify-subagent result, and stashes it on the tracker for the
// end-gate to consult. Phase B (2026-05-19) — see lastVerifyVerdict
// field comment for the bench-iter10 case this catches.
//
// VERDICT extraction:
//   - Match the LAST `VERDICT: PASS|FAIL|PARTIAL` line in the body
//     (subagent profile mandates it on its own line at the end).
//   - If verify dispatched but body contains no VERDICT line, record
//     "MISSING" so the gate can hold for the same reason ("verify
//     didn't follow protocol → not safe to release").
//   - Only set when subagent_type was exactly "verify"; other
//     subagent types are out of scope for this gate.
func (ct *contractTracker) observeToolResults(toolUses, results []llm.ContentBlock) {
	for i, tu := range toolUses {
		if tu.Type != "tool_use" || tu.ToolName != "Agent" {
			continue
		}
		st, _ := tu.ToolInput["subagent_type"].(string)
		if st != contractVerifySubagentID {
			continue
		}
		if i >= len(results) {
			continue
		}
		body := results[i].ToolResult
		ct.lastVerifyVerdict = extractVerdict(body)
	}
}

// extractVerdict scans subagent body for the mandated VERDICT line.
// Returns "PASS", "FAIL", "PARTIAL", or "MISSING" (no VERDICT line
// found). The match is case-sensitive on "VERDICT:" per profile;
// the verdict word itself is also case-sensitive — the profile
// specifies upper-case and the verifier subagent must honor it.
//
// Takes the LAST match in case the subagent paraphrased earlier
// (the truthful verdict is always the final summary line).
func extractVerdict(body string) string {
	const marker = "VERDICT:"
	lastIdx := strings.LastIndex(body, marker)
	if lastIdx < 0 {
		return "MISSING"
	}
	tail := body[lastIdx+len(marker):]
	// Trim leading whitespace; the verdict word is the first
	// non-whitespace token after "VERDICT:".
	tail = strings.TrimLeft(tail, " \t")
	for _, want := range []string{"PASS", "FAIL", "PARTIAL"} {
		if strings.HasPrefix(tail, want) {
			return want
		}
	}
	return "MISSING"
}

// contractDisabled returns true when METIS_CONTRACT_DISABLE=1 is
// set in env. Read each call so test rigs can flip mid-run.
func contractDisabled() bool {
	return os.Getenv(contractDisableEnvVar) == "1"
}

// riskScore combines mutation volume, distinct-file scope, implementation
// fan-out, observed validation, and high-impact external actions. Runtime owns
// this policy so the base prompt only needs to state the invariant.
func (ct *contractTracker) riskScore() int {
	if ct.highImpactAction {
		return contractIndependentRiskThreshold
	}
	score := minInt(ct.mainWrites, 3)
	score += minInt(len(ct.mutatedFiles), 3)
	score += minInt(ct.implementationAgents*2, 6)
	if ct.validationObserved && score > 0 {
		score--
	}
	return score
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// thresholdMet reports whether this run warrants independent verification.
func (ct *contractTracker) thresholdMet() bool {
	return ct.riskScore() >= contractIndependentRiskThreshold
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
			"CONTRACT REMINDER (heads-up). Runtime risk is now %d/%d "+
			"(%d mutation(s) across %d file(s), %d implementation agent(s), "+
			"validation observed: %t, high-impact action: %t). Non-trivial "+
			"implementation MUST end with a verify sub-agent that returns "+
			"`VERDICT: PASS`. Plan your remaining moves so the work ends "+
			"with:\n"+
			"    Agent({subagent_type: \"verify\", prompt: \"<what to check "+
			"+ the VERDICT line you expect back>\"})\n"+
			"Running `go build` / `npm test` / etc. yourself does NOT "+
			"substitute. Only the verifier issues a verdict.\n"+
			"</system-reminder>",
		ct.riskScore(), contractIndependentRiskThreshold,
		ct.mainWrites, len(ct.mutatedFiles), ct.implementationAgents,
		ct.validationObserved, ct.highImpactAction,
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
	if !ct.thresholdMet() {
		return ""
	}
	// Override applies to BOTH the dispatch-gate (no verify yet) and
	// the verdict-gate (verify ran but didn't pass) — it's an escape
	// hatch for the whole contract.
	if strings.Contains(assistantText, contractOverridePhrase) {
		return ""
	}

	// Phase B: verdict gate. If verify was dispatched and we observed
	// a verdict, only PASS releases. FAIL / PARTIAL / MISSING all
	// hold — model must address findings and re-verify. Separate
	// attempt budget so a model that keeps getting FAIL doesn't burn
	// its dispatch-gate budget too.
	if ct.verifyDispatched && ct.lastVerifyVerdict != "" && ct.lastVerifyVerdict != "PASS" {
		if ct.verdictGateAttempts >= contractMaxGateAttempts {
			return ""
		}
		ct.verdictGateAttempts++
		return fmt.Sprintf(
			"<system-reminder>\n"+
				"VERDICT GATE — HALT (attempt %d of %d). Your verify "+
				"subagent returned VERDICT: %s. Per the contract, end "+
				"is not allowed until VERDICT: PASS. Pick one:\n\n"+
				"  (a) Address the verifier's findings (read its report, "+
				"fix the failing tests / missing pieces), then RE-DISPATCH "+
				"Agent({subagent_type: \"verify\", ...}) and wait for PASS.\n\n"+
				"  (b) Override by writing this exact phrase as a line in "+
				"your next reply:\n"+
				"          %s <one-line reason why a non-PASS verdict is OK here>\n"+
				"      (Logged for audit. Genuine cases: the verifier was "+
				"wrong about the scope, or PARTIAL is the intended end "+
				"state for this task. Don't override just because fixing "+
				"is hard.)\n\n"+
				"After attempt %d/%d, the loop releases with a warning so "+
				"we don't burn tokens infinitely.\n"+
				"</system-reminder>",
			ct.verdictGateAttempts, contractMaxGateAttempts,
			ct.lastVerifyVerdict,
			contractOverridePhrase,
			ct.verdictGateAttempts, contractMaxGateAttempts,
		)
	}

	// Original dispatch gate: verify never dispatched.
	if ct.verifyDispatched {
		return ""
	}
	if ct.gateAttempts >= contractMaxGateAttempts {
		return ""
	}
	ct.gateAttempts++
	return fmt.Sprintf(
		"<system-reminder>\n"+
			"CONTRACT GATE — HALT (attempt %d of %d). Runtime risk is %d/%d "+
			"after %d mutation(s) across %d file(s), %d implementation "+
			"agent(s), validation observed=%t, high-impact action=%t. You "+
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
		ct.riskScore(), contractIndependentRiskThreshold,
		ct.mainWrites, len(ct.mutatedFiles), ct.implementationAgents,
		ct.validationObserved, ct.highImpactAction,
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
