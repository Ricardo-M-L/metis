package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
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
		root := errors.New("rate limit")
		err := RetryWithBackoff(context.Background(), 2, 10*time.Millisecond, func() error {
			calls++
			return &RetryableError{Err: root}
		})
		if err == nil {
			t.Fatal("expected error after exhaustion")
		}
		if calls != 2 {
			t.Errorf("expected 2 calls, got %d", calls)
		}
		var exhausted *RetryExhaustedError
		if !errors.As(err, &exhausted) || exhausted.Attempts != 2 {
			t.Fatalf("err = %T %v, want two-attempt RetryExhaustedError", err, err)
		}
		var retryable *RetryableError
		if !errors.As(err, &retryable) || !errors.Is(err, root) {
			t.Fatalf("exhausted marker lost wrapped causes: %v", err)
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

func TestRetry_TransientNetworkFailuresRecover(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "eof before headers", err: io.EOF},
		{name: "truncated body", err: io.ErrUnexpectedEOF},
		{name: "dns lookup", err: &net.DNSError{Err: "no such host", Name: "token.sensenova.cn"}},
		{name: "network unreachable", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}},
		{name: "connection reset", err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				err := RetryWithBackoff(context.Background(), 2, time.Millisecond, func() error {
					calls++
					if calls == 1 {
						return tc.err
					}
					return nil
				})
				if err != nil {
					t.Fatalf("retry did not recover: %v", err)
				}
				if calls != 2 {
					t.Fatalf("calls = %d, want 2", calls)
				}
			})
		})
	}
}

func TestRetry_ContextCancellationIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := RetryWithBackoff(ctx, 3, time.Millisecond, func() error {
		calls++
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("canceled context made %d outbound calls, want 0", calls)
	}
}

func TestParseRetryAfterAndRetryHonorsIt(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3"}}}
	if got := ParseRetryAfter(resp); got != 3*time.Second {
		t.Fatalf("ParseRetryAfter = %v, want 3s", got)
	}

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		err := RetryWithBackoff(context.Background(), 2, time.Millisecond, func() error {
			calls++
			if calls == 1 {
				return &RetryableError{Err: errors.New("openai 429: rpm exhausted"), After: 3 * time.Second}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("retry elapsed = %v, want exact Retry-After 3s", elapsed)
		}
	})
}
