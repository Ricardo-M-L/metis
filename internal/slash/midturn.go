// Package slash — mid-turn signal classification.
//
// When the user types a slash command while the agent is mid-turn (a
// previous turn's tool call is still running), the TUI needs to know
// whether the command is safe to execute right now, whether it should
// be refused with a hint, or whether it represents text the user wants
// steered into the running turn.
//
// Three classes:
//
//	MidTurnDestructive — would interrupt or invalidate the in-flight
//	                     turn (clear/new/quit/reset/undo/retry).
//	                     TUI should refuse with "press Esc to cancel
//	                     first, then …".
//	MidTurnCustom      — user-authored prompt-template command (custom
//	                     .md files). The handler already resolved the
//	                     template into prompt text via SignalCustomPrompt;
//	                     the TUI should SteerInject that text.
//	MidTurnSafe        — read-only / informational (cost/context/tools/
//	                     help/version/effort/theme/...). Safe to dispatch
//	                     immediately as a side panel without disturbing
//	                     the running turn.
package slash

// MidTurnClass classifies one Signal for mid-turn dispatch.
type MidTurnClass int

const (
	// MidTurnSafe — default. The signal opens a panel / changes a
	// session-local UI knob without touching the agent loop.
	MidTurnSafe MidTurnClass = iota
	// MidTurnDestructive — the signal would interrupt or destroy the
	// in-flight turn. TUI must refuse + hint.
	MidTurnDestructive
	// MidTurnCustom — the signal carries resolved prompt text in its
	// display. TUI should SteerInject(display) instead of executing
	// the signal handler.
	MidTurnCustom
)

// ClassifyMidTurn returns the dispatch class for a Signal. Default is
// MidTurnSafe — anything not explicitly marked destructive or custom
// is presumed safe to fire mid-turn (overlays, theme switches, etc.).
//
// If you add a new Signal, decide its mid-turn semantics here. Skipping
// the decision means the new Signal defaults to MidTurnSafe — usually
// fine, but verify the handler doesn't mutate Loop.Messages or trigger
// a new turn.
func ClassifyMidTurn(s Signal) MidTurnClass {
	switch s {
	// Destructive: would interrupt / invalidate the running turn or
	// destroy state the in-flight turn depends on.
	case SignalQuit, SignalClear, SignalNew, SignalCompact,
		SignalReload, SignalUndo, SignalRewind, SignalRetry, SignalBranch,
		SignalResume:
		return MidTurnDestructive
	// Custom: handler returned resolved prompt text in display; we
	// route that text through SteerInject instead of letting it
	// re-enter the agent loop as a separate user message.
	case SignalCustomPrompt:
		return MidTurnCustom
	// Batch is similar to custom: it rewrites the prompt to a
	// research→plan→execute contract. Mid-turn it should also be
	// refused (it expects to BE the start of a turn, not a steer).
	case SignalBatch:
		return MidTurnDestructive
	}
	return MidTurnSafe
}
