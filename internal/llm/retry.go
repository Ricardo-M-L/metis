package llm

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"strings"
	"time"
)

// RetryableError marks an error that retryWithBackoff should attempt again.
// HTTP-status helpers below wrap raw errors in this type when appropriate.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// retryWithBackoff calls fn until it returns nil, ctx is cancelled, or attempts
// is exhausted. Backoff = 2^i * 100ms, capped at maxBackoff, with ±20% jitter.
//
// Only RetryableError or transient network errors trigger another attempt.
// 4xx (except 429) and authentication failures bypass retry — they're
// permanent and would just waste budget.
func retryWithBackoff(ctx context.Context, attempts int, maxBackoff time.Duration, fn func() error) error {
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
		// ±20% jitter to avoid thundering herd if we ever ran in parallel.
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
	// Bare net errors are usually transient (DNS blip, connection reset).
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// Heuristic for HTTP responses we wrap as `error("anthropic 429: ...")`
	// in anthropic.go / openai.go. Cheap fallback when we don't have a
	// structured wrapper.
	msg := err.Error()
	for _, code := range []string{" 429:", " 503:", " 529:", " 502:", " 504:"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}
