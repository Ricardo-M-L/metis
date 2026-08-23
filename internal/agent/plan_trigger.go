package agent

// plan_trigger.go — runtime scope-check safety net for broad rewrite, port,
// translation, and migration requests. It prevents an agent from silently
// choosing a reduced implementation when parity was expected. Runtime owns
// detection; the base prompt only states the invariant.

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

// planTriggerReminder is intentionally short and decision-oriented. Explicit
// full-parity or scoped requests can proceed without a redundant question;
// ambiguous scope must be surfaced before implementation.
const planTriggerReminder = `<system-reminder>
PLAN-THEN-ASK trigger detected in your last user message
("rewrite / port / translate / migrate" intent).

Before implementation, inspect enough representative source to establish the
true scope. Determine whether the request requires full parity, a stated
subset, or staged delivery. If the user's request and repository already make
that choice explicit, record it and proceed. Otherwise enter plan mode and ask
a focused scope question with the materially different options and tradeoffs.
Never silently substitute an MVP for a full rewrite or port.
</system-reminder>`
