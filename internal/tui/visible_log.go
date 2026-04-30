package tui

// visible_log.go holds tiny helpers for the in-memory chat log that the
// bubbletea Model renders. The "visible log" is distinct from the agent's
// underlying llm.Message history: it includes "info" / "permission" /
// "error" rows that have no place in the LLM transcript.

// trimVisibleMessagesToLastUser drops everything from the most recent
// user-role entry to the end of the slice, returning the trimmed prefix.
//
// /undo uses this to keep the on-screen chat consistent with the loop's
// rolled-back history. Returns the input unchanged if no user-role entry
// is found (e.g. the chat is empty or only has an info banner).
func trimVisibleMessagesToLastUser(msgs []Message) []Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return append([]Message(nil), msgs[:i]...)
		}
	}
	return msgs
}
