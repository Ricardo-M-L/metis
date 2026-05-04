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
	"math/rand"
	"net"
	"strings"
	"time"
)

// RetryableError marks an error that RetryWithBackoff should attempt
// again. HTTP-status helpers below wrap raw errors in this type when
// appropriate.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// RetryWithBackoff calls fn until it returns nil, ctx is cancelled, or
// attempts is exhausted. Backoff = 2^i * 100ms, capped at maxBackoff,
// with ±20% jitter.
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
		backoff := time.Duration(1<<uint(i)) * 100 * time.Millisecond
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		jitter := time.Duration(rand.Int63n(int64(backoff) / 5))
		if rand.Intn(2) == 0 {
			backoff -= jitter
		} else {
			backoff += jitter
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return lastErr
}

func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	var re *RetryableError
	if errors.As(err, &re) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := err.Error()
	for _, code := range []string{" 429:", " 503:", " 529:", " 502:", " 504:"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
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
