package agent

// plan_trigger.go — runtime safety net for the "rewrite / port /
// translate / migrate a project" intent. Backs up the
// 08_interaction_modes.md "Mandatory plan-then-ask" section with a
// runtime detector: when a user message contains a trigger phrase
// AND the agent hasn't yet entered plan mode this session, we
// prepend a `<system-reminder>` block that names the required
// sequence (Glob survey → EnterPlanMode → AskUser(3 options) →
// ExitPlanMode → dispatch).
//
// Why runtime instead of pure prompt: some models (notably
// minimax-m2.7, session 5d9a38e5 repro 2026-05-21) reliably skip
// mid-prompt instructions when the system prompt is long. The
// trigger detector is model-independent — pure string match on the
// user's text — so it fires regardless of which provider is loaded.
// False-positive rate is bounded by the small trigger-phrase list
// (only project-level intents, not e.g. "refactor this function").

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// planTriggerPhrases matches user-input intents that REQUIRE
// EnterPlanMode + AskUser before any sub-agent dispatch. Kept short
// and project-scoped on purpose: "refactor this function" should
// NOT trigger; "refactor the whole module" should. Order doesn't
// matter (first match wins, all match types route to the same
// reminder body).
//
// Add new phrases sparingly — each one is a runtime intercept the
// user can't easily turn off mid-session. Match is case-insensitive
// + substring (no word-boundary check) so "重写" inside "完全重写整个项目"
// fires, and "rewrite" inside "the rewrite tool" also fires (the
// latter is a false positive we accept — typing "the rewrite tool"
// in metis is uncommon enough to not be worth distinguishing).
var planTriggerPhrases = []string{
	// English — project-level intents only.
	"rewrite",
	"port to ",
	"port from ",
	"port the ",
	"translate to ",
	"translate from ",
	"refactor the whole",
	"refactor the entire",
	"refactor entire",
	"migrate to ",
	"migrate from ",
	"convert the project",
	"convert the codebase",
	"reimplement",
	// Chinese — same scope.
	"重写",
	"转写",
	"迁移",
	"改写成",
	"改写为",
	"用 go 写",
	"用 rust 写",
	"用 python 写",
	"用 typescript 写",
	"用 java 写",
	"换成 go",
	"换成 rust",
	"换成 python",
	"全部翻译",
}

// shouldInjectPlanTrigger reports whether to inject the reminder.
// True when the text contains a trigger phrase AND the loop hasn't
// yet seen an EnterPlanMode call in its message history.
//
// `alreadyEntered` is the caller's pre-computed flag (cheaper than
// re-scanning Messages from inside the lock); detectPlanModeEntered
// is the helper that derives it from a Messages slice.
func shouldInjectPlanTrigger(text string, alreadyEntered bool) bool {
	if alreadyEntered {
		return false
	}
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, p := range planTriggerPhrases {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// detectPlanModeEntered scans a Messages slice for any assistant
// tool_use call with name=="EnterPlanMode". One sighting → the
// flow has already started; we don't need to remind again.
//
// Cheap O(N) scan acceptable here — only runs once per user turn
// at AppendUser time, not per assistant message.
func detectPlanModeEntered(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ToolName == "EnterPlanMode" {
				return true
			}
		}
	}
	return false
}

// planTriggerReminder is the actual `<system-reminder>` body. Kept
// short (LLM attention drops fast on long reminders) and specific
// (names the 4 steps + cites the prompt section so the model can
// reconcile with the system prompt it already saw).
const planTriggerReminder = `<system-reminder>
PLAN-THEN-ASK trigger detected in your last user message
("rewrite / port / translate / migrate" intent).

Per 08_interaction_modes.md the FIRST action MUST be:

  1. Glob + Read 5-10 key source files to estimate true scope.
  2. EnterPlanMode → write a 3-option plan to plan file:
       Option A — 1:1 full port (every source file → equivalent target)
       Option B — MVP core (list which features kept vs dropped)
       Option C — incremental (port phase 1, evaluate, then continue)
     Include rough file count + iter-budget estimate per option.
  3. AskUser({question, options: [A, B, C], allow_freeform: true}).
  4. After the user picks → ExitPlanMode → dispatch implementer agents.

DO NOT skip steps 2-3. Silently choosing MVP scope wastes ~1 hour
of the user's time discovering a partial implementation 60 minutes
in (session 5d9a38e5 / image #50 repro). If iter budget seems
insufficient for Option A, surface that AS PART OF the plan;
do NOT silently default to Option B.
</system-reminder>`
