package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestRecoveryPolicyEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, seconds, attempts, backoff string
		wantErr                          bool
	}{
		{"default", "", "", "", false}, {"disabled", "0", "", "", false}, {"enabled", "600", "30", "20", false},
		{"negative", "-1", "", "", true}, {"fractional", "1.5", "", "", true}, {"bad", "secret-value", "", "", true},
		{"overflow", "9223372036854775807", "", "", true}, {"zero attempts", "600", "0", "", true},
		{"negative attempts", "600", "-1", "", true}, {"overflow attempts", "600", "2147483648", "", true},
		{"zero backoff", "600", "", "0", true}, {"disabled bad knob", "0", "bad", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METIS_RECOVERY_MAX_SECONDS", tc.seconds)
			t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", tc.attempts)
			t.Setenv("METIS_RECOVERY_MAX_BACKOFF_SECONDS", tc.backoff)
			p, err := RecoveryPolicyFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("policy=%+v err=%v", p, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-value") {
				t.Fatal("invalid raw config echoed")
			}
			if tc.name == "default" && (p.MaxDuration != time.Minute || p.MaxAttempts != 3 || p.MaxBackoff != 8*time.Second) {
				t.Fatalf("defaults=%+v", p)
			}
		})
	}
}

func TestRecoveryPolicyExplicitWindowPreservesLegacyBudgets(t *testing.T) {
	for _, tc := range []struct {
		seconds, attempts, backoff string
		want                       RecoveryPolicy
	}{
		{"0", "", "", RecoveryPolicy{0, 30, 30 * time.Second}},
		{"600", "", "", RecoveryPolicy{600 * time.Second, 30, 30 * time.Second}},
		{"600", "7", "12", RecoveryPolicy{600 * time.Second, 7, 12 * time.Second}},
		{"", "5", "2", RecoveryPolicy{time.Minute, 5, 2 * time.Second}},
	} {
		t.Run(fmt.Sprintf("seconds_%s_attempts_%s_backoff_%s", tc.seconds, tc.attempts, tc.backoff), func(t *testing.T) {
			t.Setenv("METIS_RECOVERY_MAX_SECONDS", tc.seconds)
			t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", tc.attempts)
			t.Setenv("METIS_RECOVERY_MAX_BACKOFF_SECONDS", tc.backoff)
			got, err := RecoveryPolicyFromEnv()
			if err != nil || got != tc.want {
				t.Fatalf("policy=%+v error=%v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestRecoveryDisabledPreservesThreeAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		release, err := RetryWithRecovery(context.Background(), RecoveryPolicy{}, func(ctx context.Context) error { calls++; return io.EOF })
		release()
		var exhausted *RetryExhaustedError
		if calls != 3 || !errors.As(err, &exhausted) || exhausted.Attempts != 3 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestRecoveryTypedFailuresAndPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}, true},
		{"truncated", io.ErrUnexpectedEOF, true},
		{"429", &HTTPStatusError{429, errors.New("provider 429: busy")}, true},
		{"500", &HTTPStatusError{500, errors.New("provider 500: busy")}, true},
		{"503", &HTTPStatusError{503, errors.New("provider 503: busy")}, true},
		{"400 truncated", &HTTPStatusError{400, io.ErrUnexpectedEOF}, false},
		{"401", &HTTPStatusError{401, errors.New("unauthorized")}, false},
		{"403", &HTTPStatusError{403, errors.New("forbidden")}, false},
		{"quota", &RetryableError{Err: &HTTPStatusError{429, errors.New("insufficient_quota")}}, false},
		{"untyped text", errors.New("connection refused"), false},
		{"already exhausted", &RetryExhaustedError{Err: io.EOF, Attempts: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				p := RecoveryPolicy{time.Minute, 5, time.Second}
				release, err := RetryWithRecovery(context.Background(), p, func(context.Context) error {
					calls++
					if calls == 1 {
						return tc.err
					}
					return nil
				})
				release()
				if tc.retry {
					if calls != 2 || err != nil {
						t.Fatalf("calls=%d err=%v", calls, err)
					}
				} else if calls != 1 || !errors.Is(err, tc.err) {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

func TestRecoveryWindowIncludesRequestTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()
		release, err := RetryWithRecovery(context.Background(), RecoveryPolicy{2 * time.Second, 30, time.Second}, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				time.Sleep(10 * time.Second)
				return io.EOF
			}
			<-ctx.Done()
			return ctx.Err()
		})
		release()
		if calls != 2 || time.Since(start) != 12*time.Second || !errors.Is(err, ErrRecoveryWindowExceeded) || !IsRetryExhausted(err) || errors.Is(err, context.Canceled) {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

func TestRecoveryParentCancellationWins(t *testing.T) {
	for _, inRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "request"}[inRequest], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				go func() {
					if inRequest {
						time.Sleep(600 * time.Millisecond)
					} else {
						time.Sleep(100 * time.Millisecond)
					}
					cancel()
				}()
				release, err := RetryWithRecovery(ctx, RecoveryPolicy{time.Minute, 30, time.Second}, func(attemptCtx context.Context) error {
					calls++
					if calls == 1 {
						return io.EOF
					}
					<-attemptCtx.Done()
					return io.ErrUnexpectedEOF
				})
				release()
				if !errors.Is(err, context.Canceled) || IsRetryExhausted(err) {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				want := 1
				if inRequest {
					want = 2
				}
				if calls != want {
					t.Fatalf("calls=%d want=%d", calls, want)
				}
			})
		})
	}
}

func TestRecoverySessionSharesHTTPAndStreamAttemptBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := RecoveryPolicy{time.Minute, 4, time.Second}
		session := NewRecoverySession(p)
		ctx := WithRecoverySession(context.Background(), session)
		calls := 0
		var events []RecoveryProgress
		ctx = WithRecoveryObserver(ctx, func(p RecoveryProgress) { events = append(events, p) })
		release, err := RetryWithRecovery(ctx, p, func(context.Context) error {
			calls++
			if calls < 3 {
				return io.EOF
			}
			return nil
		})
		release()
		if err != nil {
			t.Fatal(err)
		}
		if !session.RecordFailure(io.ErrUnexpectedEOF) {
			t.Fatal("stream error rejected")
		}
		if err = session.WaitRetry(ctx); err != nil {
			t.Fatal(err)
		}
		release, err = RetryWithRecovery(ctx, p, func(context.Context) error { calls++; return nil })
		release()
		if err != nil {
			t.Fatal(err)
		}
		if !session.RecordFailure(io.ErrUnexpectedEOF) {
			t.Fatal("stream error rejected")
		}
		err = session.WaitRetry(ctx)
		var exhausted *RetryExhaustedError
		if calls != 4 || !errors.As(err, &exhausted) || exhausted.Attempts != 4 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		if len(events) < 3 || events[len(events)-1].State != "exhausted" {
			t.Fatalf("progress=%+v", events)
		}
	})
}

func TestRecoveryHeadersDoNotResetFailureWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := RecoveryPolicy{time.Second, 30, time.Second}
		s := NewRecoverySession(p)
		ctx := WithRecoverySession(context.Background(), s)
		calls := 0
		release, err := RetryWithRecovery(ctx, p, func(context.Context) error {
			calls++
			if calls == 1 {
				return io.EOF
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		release()
		if !s.RecordFailure(io.ErrUnexpectedEOF) {
			t.Fatal("failed to record")
		}
		err = s.WaitRetry(ctx)
		if calls != 2 || !errors.Is(err, ErrRecoveryWindowExceeded) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestRecoveryBackoffSaturatesAndRetryAfterCapped(t *testing.T) {
	if got := recoveryBackoff(2147483647, time.Second, io.EOF); got != time.Second {
		t.Fatalf("overflow backoff=%v", got)
	}
	if got := recoveryBackoff(0, 30*time.Second, &RetryableError{Err: errors.New("provider 429: busy"), After: 24 * time.Hour}); got != 30*time.Second {
		t.Fatalf("retry-after=%v", got)
	}
}

func TestRecoveryNestedProviderOwnershipFailsWithoutDeadlock(t *testing.T) {
	p := RecoveryPolicy{time.Minute, 3, time.Second}
	ctx := WithRecoverySession(context.Background(), NewRecoverySession(p))
	innerCalls := 0
	release, err := RetryWithRecovery(ctx, p, func(attemptCtx context.Context) error {
		innerRelease, innerErr := RetryWithRecovery(attemptCtx, p, func(context.Context) error { innerCalls++; return nil })
		innerRelease()
		return innerErr
	})
	release()
	if err == nil || !strings.Contains(err.Error(), "ManagesRecoverySession") || innerCalls != 0 {
		t.Fatalf("calls=%d err=%v", innerCalls, err)
	}
}
