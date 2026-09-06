// Package transport holds the HTTP retry / error helpers shared by
// every provider subpackage (anthropic, openai, gemini, azure, vertex,
// bedrock). Lives in its own package so the per-provider subpackages
// can share these helpers without forming a cycle through internal/llm
// root (which would happen if root re-exported them and a provider
// also imported root).
package transport

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNetwork is a message-safe classification marker. Provider error
// boundaries may expose it through errors.Is while deliberately refusing to
// unwrap a credential-bearing net/url error.
var ErrNetwork = errors.New("network transport error")

// IsNetworkError recognizes typed transport failures without requiring a
// caller to retain their potentially sensitive Error strings.
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNetwork) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Provider SDKs and custom RoundTrippers may expose a private net.Error
	// without wrapping it in net.OpError. Preserve the historical retry
	// behaviour for those typed errors while exporting only ErrNetwork across
	// credential-redaction boundaries.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// ParseRetryAfter extracts the HTTP Retry-After header (delta-seconds or an
// HTTP-date) from a response, returning 0 when absent or unparseable.
// Providers pass the result into RetryableError.After so RetryWithBackoff
// honors a server-requested cool-down on 429/503.
func ParseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// RetryableError marks an error that RetryWithBackoff should attempt
// again. HTTP-status helpers below wrap raw errors in this type when
// appropriate.
type RetryableError struct {
	Err error
	// After, when > 0, is the server-requested cool-down (the HTTP
	// Retry-After header on a 429/503). RetryWithBackoff waits at least
	// this long instead of its own exponential schedule, so we don't
	// re-fire a rate-limited request sooner than the server asked for.
	// Providers that surface a 429 should populate this from the header.
	After time.Duration
}

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// RetryExhaustedError marks a transient failure whose complete provider-level
// retry budget has already been consumed. Agent loops use this typed boundary
// to avoid wrapping a three-attempt provider retry in another whole retry
// round. Unwrap deliberately preserves errors.Is/errors.As access to both a
// RetryableError and its underlying network/status error.
type RetryExhaustedError struct {
	Err      error
	Attempts int
}

func (e *RetryExhaustedError) Error() string { return e.Err.Error() }
func (e *RetryExhaustedError) Unwrap() error { return e.Err }

// IsRetryExhausted reports whether err crossed a provider retry boundary.
func IsRetryExhausted(err error) bool {
	var exhausted *RetryExhaustedError
	return errors.As(err, &exhausted)
}

// RetryWithBackoff calls fn until it returns nil, ctx is cancelled, or
// attempts is exhausted. Ordinary transient failures back off from 500ms;
// rate limits without Retry-After back off from 5s. Both use ±20% jitter so
// a fan-out of sub-agents does not retry in lock-step.
//
// Only RetryableError or transient network errors trigger another
// attempt. 4xx (except 429) and authentication failures bypass retry —
// they're permanent and would just waste budget.
func RetryWithBackoff(ctx context.Context, attempts int, maxBackoff time.Duration, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	if maxBackoff <= 0 {
		maxBackoff = 8 * time.Second
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetry(err) {
			return err
		}
		if i == attempts-1 {
			break
		}
		backoff := time.Duration(1<<uint(i)) * 500 * time.Millisecond
		if isRateLimitError(err) {
			// An RPM bucket normally needs seconds, not the old 100/200ms
			// loop. Keep this finite: three attempts wait about 15s total,
			// while the caller's context remains the final upper bound.
			backoff = time.Duration(1<<uint(i)) * 5 * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		} else if backoff > maxBackoff {
			backoff = maxBackoff
		}
		if jitterWindow := backoff / 5; jitterWindow > 0 {
			jitter := time.Duration(rand.Int63n(int64(jitterWindow)))
			if rand.Intn(2) == 0 {
				backoff -= jitter
			} else {
				backoff += jitter
			}
		}
		// Honor a server Retry-After when the error carries one — wait AT
		// LEAST that long (no down-jitter), capped at 60s so a hostile or
		// buggy header can't park the CLI for minutes.
		var re *RetryableError
		if errors.As(err, &re) && re.After > 0 {
			backoff = re.After
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return &RetryExhaustedError{Err: lastErr, Attempts: attempts}
}

func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	// Cancellation is a caller decision, not a transport failure. In
	// particular, don't turn Esc into another outbound request.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var re *RetryableError
	if errors.As(err, &re) {
		return true
	}
	// net/http can surface a server disappearing before headers as bare EOF,
	// and a truncated response body as UnexpectedEOF. Both are safe to retry
	// at this layer because provider request bodies are replayable byte slices
	// and WebFetch only opts in for idempotent GETs.
	if IsNetworkError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// Permanent quota/balance errors must NOT be retried — they fail
	// identically on every attempt and only waste 35s+ of waiting.
	// Harness separates these via isQuotaExceededError() → QUOTA code
	// which is excluded from the default retryable set. We match the
	// same OpenAI error bodies here.
	if strings.Contains(msg, "insufficient_quota") || strings.Contains(msg, "quota_exceeded") ||
		strings.Contains(msg, "insufficient quota") || strings.Contains(msg, "quota exceeded") {
		return false
	}
	for _, code := range []string{" 429:", " 503:", " 529:", " 502:", " 504:"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	for _, transient := range []string{
		"no such host", "temporary failure in name resolution", "server misbehaving",
		"network unreachable", "network is unreachable", "connection reset by peer",
		"connection refused", "broken pipe",
	} {
		if strings.Contains(msg, transient) {
			return true
		}
	}
	return false
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, " 429:") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate_limit") ||
		strings.Contains(msg, "rpm exhausted") ||
		strings.Contains(msg, "tpm exhausted") ||
		strings.Contains(msg, "too many requests")
}

// IsRetryableStatus picks the HTTP status codes that can plausibly be
// retried. 429 (rate limit), 503 (overloaded), 529 (Anthropic-specific
// overloaded), 502/504 (gateway hiccup). 500 is intentionally
// excluded — can mean malformed request, retrying just doubles the bill.
func IsRetryableStatus(code int) bool {
	switch code {
	case 429, 502, 503, 504, 529:
		return true
	}
	return false
}

// Truncate clamps a possibly-large string for logging. Used in error
// messages where a full upstream response body would blow up the log
// without adding signal.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
