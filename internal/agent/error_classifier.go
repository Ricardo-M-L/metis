package agent

import "strings"

// error_classifier.go — semantic recovery routing for provider errors.
// Mirrors hermes-agent's `agent/error_classifier.py:1-100`. The agent
// loop currently catches "context too long" via tryRecoverOverflow and
// nothing else; every other error class — billing, rate-limit,
// auth-expired, server outage — turns into an indistinguishable retry
// or a user-facing crash. The classifier here gives recovery code a
// typed signal so it can pick a strategy (compress / wait / fallback /
// bail) instead of guessing.
//
// Detection is string-pattern based. Provider error envelopes vary
// wildly (Anthropic: structured JSON `{type, message}`; OpenAI: HTTP
// status + JSON; MiniMax/Anthropic-compat: status code 2013 for
// context overflow specifically; OpenRouter: prefixed messages).
// We string-match on common phrasing because reliable cross-provider
// detection requires that anyway. False positives just route to a
// safer recovery; the cost of misclassifying as "rate_limit" is one
// extra retry, not data corruption.

// ErrorClass is the categorisation of a provider error. RecoveryHint
// returns the strategy the loop should attempt.
type ErrorClass int

const (
	// ErrUnknown — couldn't classify; let the caller bubble it up.
	ErrUnknown ErrorClass = iota
	// ErrContextOverflow — input + max_tokens exceeded the model's
	// context window. Recovery: compact + retry once.
	ErrContextOverflow
	// ErrRateLimit — provider asked us to slow down. Recovery: backoff
	// + retry. Surface "rate-limited, retrying in Xs" to the user.
	ErrRateLimit
	// ErrBilling — payment / quota / org over-cap. Recovery: fail
	// hard with a CTA ("top up at provider URL") — retry won't fix it.
	ErrBilling
	// ErrAuth — token expired / revoked. Recovery: prompt user to
	// re-auth. Retry won't fix it.
	ErrAuth
	// ErrServerError — 5xx, gateway timeout, transient. Recovery:
	// backoff + retry.
	ErrServerError
	// ErrNetwork — connection refused / DNS / TLS. Recovery: backoff
	// + retry, longer delay than ErrServerError.
	ErrNetwork
	// ErrInvalidRequest — model rejected our payload (bad tool call,
	// unsupported feature). Recovery: surface to user; retry won't
	// fix without a code change.
	ErrInvalidRequest
	// ErrCancelled — user-initiated abort (ctx cancelled). Not really
	// an error per se; classifier returns this so callers can skip
	// "retry" logic for cancellations.
	ErrCancelled
)

// String returns the lowercase name of the class.
func (c ErrorClass) String() string {
	switch c {
	case ErrContextOverflow:
		return "context_overflow"
	case ErrRateLimit:
		return "rate_limit"
	case ErrBilling:
		return "billing"
	case ErrAuth:
		return "auth"
	case ErrServerError:
		return "server_error"
	case ErrNetwork:
		return "network"
	case ErrInvalidRequest:
		return "invalid_request"
	case ErrCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// RecoveryStrategy is what the loop should attempt for an error.
type RecoveryStrategy int

const (
	// RecoveryNone — bubble the error up unchanged.
	RecoveryNone RecoveryStrategy = iota
	// RecoveryRetry — re-issue the same request after a backoff.
	RecoveryRetry
	// RecoveryCompactRetry — compact history first, then retry.
	RecoveryCompactRetry
	// RecoveryFailUser — surface a user-actionable error and stop.
	RecoveryFailUser
)

// Recovery returns the recommended action for a class.
func (c ErrorClass) Recovery() RecoveryStrategy {
	switch c {
	case ErrContextOverflow:
		return RecoveryCompactRetry
	case ErrRateLimit, ErrServerError, ErrNetwork:
		return RecoveryRetry
	case ErrBilling, ErrAuth, ErrInvalidRequest:
		return RecoveryFailUser
	case ErrCancelled:
		return RecoveryNone
	default:
		return RecoveryNone
	}
}

// ClassifyError inspects an error and returns its class. nil → ErrUnknown.
//
// Implementation note: we lowercase the error message once and run all
// substring checks against that. Order matters — ErrContextOverflow
// (most specific) comes first; "rate limit" comes before generic
// "limit" patterns. Order ties prefer the more actionable
// classification.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrUnknown
	}
	msg := strings.ToLower(err.Error())

	// Cancellation — check first because user-cancel often manifests
	// as "context canceled" which would match other patterns.
	if strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "context cancelled") ||
		strings.Contains(msg, "user abort") ||
		strings.Contains(msg, "operation cancelled by user") {
		return ErrCancelled
	}

	// Context overflow — Anthropic's "prompt is too long",
	// MiniMax error code 2013, OpenAI's "exceeds context length",
	// catch-all "too many tokens".
	if strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "exceeds context length") ||
		strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "context window") ||
		strings.Contains(msg, "exceeds limit") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "code 2013") ||
		strings.Contains(msg, "\"code\":2013") {
		return ErrContextOverflow
	}

	// Rate limit — provider-side throttling.
	if strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate-limited") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "429") ||
		strings.Contains(msg, "rate_limit_exceeded") {
		return ErrRateLimit
	}

	// Billing / quota — hard fail.
	if strings.Contains(msg, "insufficient credit") ||
		strings.Contains(msg, "insufficient_credit") ||
		strings.Contains(msg, "quota") && (strings.Contains(msg, "exceeded") || strings.Contains(msg, "exhausted")) ||
		strings.Contains(msg, "billing") ||
		strings.Contains(msg, "payment required") ||
		strings.Contains(msg, "402") {
		return ErrBilling
	}

	// Auth — token / credential issues.
	if strings.Contains(msg, "401") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "invalid api key") ||
		strings.Contains(msg, "invalid_api_key") ||
		strings.Contains(msg, "expired") && strings.Contains(msg, "token") ||
		strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "auth") && strings.Contains(msg, "fail") {
		return ErrAuth
	}

	// Network — connection-level issues. Check before 5xx so a
	// connection-refused doesn't get bucketed as a server error.
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "tls handshake") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "network is unreachable") {
		return ErrNetwork
	}

	// Server error — provider 5xx.
	if strings.Contains(msg, "500") ||
		strings.Contains(msg, "502") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "504") ||
		strings.Contains(msg, "internal server error") ||
		strings.Contains(msg, "bad gateway") ||
		strings.Contains(msg, "service unavailable") ||
		strings.Contains(msg, "gateway timeout") ||
		strings.Contains(msg, "overloaded") {
		return ErrServerError
	}

	// Invalid request — 4xx that isn't auth/billing/rate.
	if strings.Contains(msg, "400") ||
		strings.Contains(msg, "invalid request") ||
		strings.Contains(msg, "validation") && strings.Contains(msg, "fail") ||
		strings.Contains(msg, "bad request") ||
		strings.Contains(msg, "schema") && strings.Contains(msg, "invalid") {
		return ErrInvalidRequest
	}

	return ErrUnknown
}

// UserFacingMessage returns a one-line, user-actionable string for
// classes whose recovery is RecoveryFailUser. For other classes
// (auto-recovered) returns "" — callers shouldn't surface anything.
func UserFacingMessage(c ErrorClass, raw error) string {
	switch c {
	case ErrBilling:
		return "billing / quota exceeded — top up at your provider's dashboard, then retry"
	case ErrAuth:
		return "auth failed — token expired or invalid; re-run `metis auth login` for this provider"
	case ErrInvalidRequest:
		if raw != nil {
			return "provider rejected the request: " + raw.Error()
		}
		return "provider rejected the request"
	}
	return ""
}
