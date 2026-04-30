package tui

// error_format.go — turn raw provider errors into readable one-liners.
//
// Anthropic / OpenAI / Gemini / MiniMax all surface their 4xx bodies as
// JSON. Dumping the raw blob to the transcript (image #62) is unreadable:
//
//	error: anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"invalid params, context window exceeds limit (2013)"},"request_id":"064170..."}
//
// claude-code's errors.ts maps each provider's failure mode to a short
// human sentence (`"API Error: Request rejected (429) · …"`) plus, when
// applicable, a recovery hint inline with the error (`"Run /login · API
// Error: …"`). We mirror that: parse the embedded JSON if any, pull the
// useful `message` out, attach a short cause-aware hint.

import (
	"encoding/json"
	"regexp"
	"strings"
)

// formatProviderError compresses a provider error message into a
// transcript-friendly one-liner. Returns the error text plus an
// optional recovery hint to be rendered as a separate dim line.
//
// Behaviour:
//   - Body looks like Anthropic JSON (has "type":"error" and a nested
//     "error.message") → strip envelope, keep just the message
//   - Body looks like OpenAI JSON (has "error":{"message":...}) → same
//   - Otherwise → use the raw error string but trim request_id-like
//     suffixes (long hex blobs) and any trailing whitespace
//
// hint is non-empty when we recognized the error class and have an
// actionable next step (auto-compaction will retry, /login required,
// etc.). Always lowercase, parenthesised in the caller.
func formatProviderError(raw string) (msg, hint string) {
	raw = strings.TrimSpace(raw)
	msg = extractInnerMessage(raw)
	if msg == "" {
		msg = stripRequestID(raw)
	}
	hint = errorRecoveryHint(msg + " " + raw)
	return msg, hint
}

// extractInnerMessage tries to pull `error.message` out of an Anthropic /
// OpenAI / Gemini-style JSON error body that's been wrapped inside
// `<provider> <status>: {...}`. Returns "" if the body isn't JSON.
func extractInnerMessage(raw string) string {
	// Find the first `{` and last `}` — providers typically prefix with
	// "anthropic 400: " or "openai 429: " before the JSON payload.
	open := strings.Index(raw, "{")
	close := strings.LastIndex(raw, "}")
	if open < 0 || close <= open {
		return ""
	}
	body := raw[open : close+1]

	// Anthropic / OpenAI shape: {"error":{"message":"...","type":"..."},"request_id":"..."}
	var anth struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &anth); err == nil && anth.Error.Message != "" {
		// Pretty: "<message> (type)" — type is helpful when the
		// message itself is generic. Skip duplication when type
		// already contained in the message.
		if anth.Error.Type != "" && !strings.Contains(strings.ToLower(anth.Error.Message), strings.ToLower(anth.Error.Type)) {
			return anth.Error.Message + " (" + anth.Error.Type + ")"
		}
		return anth.Error.Message
	}

	// Some Anthropic-compat gateways double-wrap: {"type":"error","error":{...}}.
	// First Unmarshal already captured the inner via "error" key — covered.

	// Gemini: {"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}
	// Same Unmarshal path handles it because we only look at error.message.

	// Plain: {"message":"..."}
	var flat struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &flat); err == nil && flat.Message != "" {
		return flat.Message
	}
	return ""
}

// stripRequestID drops trailing long hex blobs from a non-JSON error
// string. Anthropic-compat gateways sometimes append `… request_id=abc123`
// suffixes that just take up screen real estate.
var requestIDSuffix = regexp.MustCompile(`(?i)\s*[,;·]?\s*(request[_-]?id|x-request-id)\s*[:=]\s*[A-Za-z0-9_-]+\s*$`)

func stripRequestID(s string) string {
	return strings.TrimSpace(requestIDSuffix.ReplaceAllString(s, ""))
}

// errorRecoveryHint returns a short sentence telling the user what to
// do next, or "" if we don't recognize the error class. Matched on
// substring so it's resilient to provider-specific phrasing variants.
func errorRecoveryHint(s string) string {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "context window") ||
		strings.Contains(low, "context_length") ||
		strings.Contains(low, "exceeds limit") ||
		strings.Contains(low, "too many tokens") ||
		strings.Contains(low, "prompt is too long"):
		return "auto-compacting on next attempt — or /clear to start fresh"
	case strings.Contains(low, "rate limit") || strings.Contains(low, "rate_limit") ||
		strings.Contains(low, "429"):
		return "rate limited — wait a moment and retry"
	case strings.Contains(low, "invalid api key") || strings.Contains(low, "authentication") ||
		strings.Contains(low, "401") || strings.Contains(low, "403"):
		return "check API key in ~/.metis/config.toml or env var"
	case strings.Contains(low, "model") && (strings.Contains(low, "not found") || strings.Contains(low, "does not exist")):
		return "check model name in config; use /model to switch"
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded"):
		return "request timed out — increase timeout_seconds or retry"
	case strings.Contains(low, "connection refused") || strings.Contains(low, "no such host"):
		return "network unreachable — check base_url + connectivity"
	}
	return ""
}

// extractDedupeCount peels off the trailing "(×N)" suffix from a
// previously-deduped error row and returns N. Returns 1 (the implicit
// first occurrence) when no suffix is present so the caller can do
// `n+1` to advance the count.
var dedupeCountRE = regexp.MustCompile(`\s*\(×(\d+)\)\s*$`)

func extractDedupeCount(content string) int {
	m := dedupeCountRE.FindStringSubmatch(content)
	if len(m) < 2 {
		return 1
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	if n < 1 {
		return 1
	}
	return n
}
