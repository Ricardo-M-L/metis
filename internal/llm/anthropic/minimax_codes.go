package anthropic

// minimax_codes.go translates the bare MiniMax business-error codes
// embedded in upstream HTTP error bodies into actionable, user-facing
// hints. Without this layer the user sees `anthropic 400: {"type":
// "error","error":{"message":"...(1028)"}}` and has no idea whether
// to top up, retry, or open a support ticket.
//
// Codes catalog: per `https://api.minimaxi.com/v1/text/chatcompletion`
// docs and observed responses against the Anthropic-shim endpoint.
// Numbers come from the `(NNNN)` suffix MiniMax appends to the
// human-readable message in the response body. Keep this table
// CONSERVATIVE — only add codes we've actually seen or are clearly
// documented; a wrong friendly hint is worse than no hint.

import (
	"fmt"
	"regexp"
)

// minimaxCodeRegexp captures the "(NNNN)" suffix MiniMax glues onto
// every business-level error message. We match on the response body
// text we already embed in the error.
var minimaxCodeRegexp = regexp.MustCompile(`\((\d{4})\)`)

// minimaxHints maps a known business-error code to a human-friendly
// rephrasing. The hint is appended to the existing error message, so
// downstream exitcode.Classify (string heuristics on err.Error())
// still finds its keywords — e.g. "rate limit" makes Quota fire.
var minimaxHints = map[string]string{
	// Auth / config
	"1004": "auth: invalid API key (MiniMax 1004)",
	"1008": "auth: account suspended or unverified (MiniMax 1008)",
	// Quota / billing
	"1028": "quota: insufficient credits — top up at minimaxi.com/account (MiniMax 1028)",
	"1030": "quota: rate limit exceeded — back off and retry (MiniMax 1030)",
	// Content filter
	"1002": "content_filter: input rejected by safety review (MiniMax 1002)",
	"1039": "content_filter: output blocked by safety review (MiniMax 1039)",
	// Plan / capability
	"2061": "plan: this model is not enabled on your account (MiniMax 2061 — Speech requires Plus, Video requires Max)",
	// Tool calling — the famous one we already bandage in stream.go
	"2013": "invalid tool arguments JSON (MiniMax 2013) — usually a model that emitted bad args; metis auto-retries with a reminder",
	// Context window
	"2026": "context window exceeded (MiniMax 2026) — try /compact or shorter input",
}

// extractMinimaxHint scans `body` (or any error message) for the
// trailing `(NNNN)` token and returns the matching hint. Empty
// string when there's no match or the code isn't in our table —
// callers MUST treat this as advisory, not authoritative.
func extractMinimaxHint(body string) string {
	if body == "" {
		return ""
	}
	m := minimaxCodeRegexp.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return minimaxHints[m[1]]
}

// wrapWithMinimaxHint is the convenience wrapper used by the Stream /
// Send paths. If the response body carries a known MiniMax code, the
// returned error reads `<original>: <hint>` so log lines stay
// greppable for the original code AND the user gets the friendly
// rephrasing.
func wrapWithMinimaxHint(err error, body string) error {
	if err == nil {
		return nil
	}
	hint := extractMinimaxHint(body)
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w (%s)", err, hint)
}
