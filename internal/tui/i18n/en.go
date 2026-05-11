package i18n

// en is the canonical locale. New keys are added here first; all
// other locales must mirror this set (parity test enforces it).
var en = map[string]string{
	// Input box
	"input.placeholder": "type a message · /commands · alt+enter newline",
	"input.bash_mode":   "bash mode — `!cmd` runs without LLM",
	"input.send_hint":   "Enter send · Alt+Enter newline",

	// Permission modes. `auto` was removed 2026-05-11 (collided with
	// claude-code's ant-only LLM-classifier mode of the same name).
	"mode.ask":         "ask",
	"mode.acceptEdits": "acceptEdits",
	"mode.bypass":      "bypass",
	"mode.plan":        "plan",
	"mode.deny":        "deny",
	"mode.cycle_hint":  "shift+tab to cycle",

	// Permission prompt
	"perm.allow":        "Yes",
	"perm.allow_always": "Yes, always",
	"perm.deny":         "No",
	"perm.cancel":       "Cancel turn",
	"perm.q.action":     "Allow this action?",
	"perm.q.tool":       "Allow this %s command?", // %s = tool name

	// Status bar / footer
	"footer.tokens":  "tokens",
	"footer.context": "context",
	"footer.session": "session",
	"footer.branch":  "branch",
	"footer.cost":    "cost",

	// Common verbs / phrases
	"verb.cogitating": "Cogitating",
	"verb.thinking":   "Thinking",
	"verb.processing": "Processing",
	"verb.compiling":  "Compiling",
	"verb.brewing":    "Brewing",
	"verb.simmering":  "Simmering",

	// Errors / info rows
	"err.network":           "network error",
	"err.rate_limit":        "rate-limited — please retry",
	"err.context_full":      "context window full — compacting",
	"info.compacted":        "context compacted",
	"info.snipped":          "tool output snipped",
	"info.copied":           "copied to clipboard",
	"info.dialog_dismissed": "dialog dismissed",

	// Search / palette
	"search.no_match":  "no match",
	"search.next":      "next",
	"search.prev":      "prev",
	"palette.no_match": "no command matches",

	// Update notice
	"update.available": "metis %s available — run `metis update`",
}
