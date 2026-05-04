package transport

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestRetry_StopsAfterFirstSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		err := RetryWithBackoff(context.Background(), 3, 10*time.Millisecond, func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("expected 1 call, got %d", calls)
		}
	})
}

func TestRetry_RetriesUntilSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		err := RetryWithBackoff(context.Background(), 3, 10*time.Millisecond, func() error {
			calls++
			if calls < 3 {
				return &RetryableError{Err: errors.New("rate limit")}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 3 {
			t.Errorf("expected 3 calls, got %d", calls)
		}
	})
}

func TestRetry_GivesUpAfterAttemptsExhausted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		err := RetryWithBackoff(context.Background(), 2, 10*time.Millisecond, func() error {
			calls++
			return &RetryableError{Err: errors.New("rate limit")}
		})
		if err == nil {
			t.Fatal("expected error after exhaustion")
		}
		if calls != 2 {
			t.Errorf("expected 2 calls, got %d", calls)
		}
	})
}

func TestRetry_NonRetryableErrorBypassesLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		err := RetryWithBackoff(context.Background(), 3, 10*time.Millisecond, func() error {
			calls++
			return errors.New("anthropic 401: invalid api key")
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if calls != 1 {
			t.Errorf("non-retryable error should not be retried; got %d calls", calls)
		}
	})
}

func TestRetry_StatusCodeHeuristic(t *testing.T) {
	for _, code := range []int{429, 502, 503, 504, 529} {
		synctest.Test(t, func(t *testing.T) {
			var calls int
			RetryWithBackoff(context.Background(), 2, 5*time.Millisecond, func() error {
				calls++
				return fmt.Errorf("provider %d: overloaded", code)
			})
			if calls != 2 {
				t.Errorf("status %d should be retryable, got %d calls", code, calls)
			}
		})
	}
}

func TestRetry_CtxCancelStopsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls int
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		err := RetryWithBackoff(ctx, 10, 200*time.Millisecond, func() error {
			calls++
			return &RetryableError{Err: errors.New("rate limit")}
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected ctx.Canceled, got %v", err)
		}
		if calls > 3 {
			t.Errorf("ctx cancel should stop loop fast, got %d calls", calls)
		}
	})
}

func TestRetry_InvalidStatusNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		RetryWithBackoff(context.Background(), 3, 5*time.Millisecond, func() error {
			calls++
			return fmt.Errorf("openai 400: bad request")
		})
		if calls != 1 {
			t.Errorf("400 should not be retried, got %d calls", calls)
		}
	})
}
