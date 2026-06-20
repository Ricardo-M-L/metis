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
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
