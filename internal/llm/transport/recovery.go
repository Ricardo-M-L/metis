package transport

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RecoveryPolicy is an explicit, opt-in replacement for the normal three
// provider attempts, not an additional outer retry loop. MaxAttempts includes
// the initial request. The window starts when its first transient failure is
// observed and includes all later waits, credential refreshes and HTTP work.
type RecoveryPolicy struct {
	MaxDuration time.Duration
	MaxAttempts int
	MaxBackoff  time.Duration
}

func (p RecoveryPolicy) Enabled() bool { return p.MaxDuration > 0 }

// RecoveryPolicyFromEnv validates configuration without echoing raw values.
// Disabled mode retains the historical three-attempt policy. Optional knobs
// are still validated when present so configuration errors are never hidden.
func RecoveryPolicyFromEnv() (RecoveryPolicy, error) {
	p := RecoveryPolicy{MaxAttempts: 30, MaxBackoff: 30 * time.Second}
	for _, field := range []struct {
		name      string
		dst       *time.Duration
		allowZero bool
	}{
		{"METIS_RECOVERY_MAX_SECONDS", &p.MaxDuration, true},
		{"METIS_RECOVERY_MAX_BACKOFF_SECONDS", &p.MaxBackoff, false},
	} {
		raw := strings.TrimSpace(os.Getenv(field.name))
		if raw == "" {
			continue
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || n > uint64(math.MaxInt64/int64(time.Second)) || (n == 0 && !field.allowZero) {
			return RecoveryPolicy{}, fmt.Errorf("%s must be a %s integer number of seconds within time.Duration range", field.name, map[bool]string{true: "non-negative", false: "positive"}[field.allowZero])
		}
		*field.dst = time.Duration(n) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv("METIS_RECOVERY_MAX_ATTEMPTS")); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 31)
		if err != nil || n == 0 {
			return RecoveryPolicy{}, errors.New("METIS_RECOVERY_MAX_ATTEMPTS must be a positive integer within int32 range")
		}
		p.MaxAttempts = int(n)
	}
	return p, nil
}

// ErrRecoveryWindowExceeded is a message-safe terminal classification. It is
// wrapped in RetryExhaustedError so an agent cannot multiply this budget.
var ErrRecoveryWindowExceeded = errors.New("network recovery window exhausted")

// HTTPStatusError retains typed status even if reading its body also failed.
// Long recovery must not turn an authoritative 401/400 into a retryable EOF.
type HTTPStatusError struct {
	StatusCode int
	Err        error
}

func (e *HTTPStatusError) Error() string { return e.Err.Error() }
func (e *HTTPStatusError) Unwrap() error { return e.Err }

// RecoveryProgress is safe to log: no payload, URL, credential or error text.
// Attempt counts calls into the recovery function (individual HTTP requests
// for Responses; Stream calls for legacy providers with internal SDK retries).
// Next waits precede Attempt+1. The callback runs synchronously and must not block.
type RecoveryProgress struct {
	State                     string
	Attempt, MaxAttempts      int
	Elapsed, Remaining, Delay time.Duration
}
type recoveryObserverKey struct{}

func WithRecoveryObserver(ctx context.Context, fn func(RecoveryProgress)) context.Context {
	return context.WithValue(ctx, recoveryObserverKey{}, fn)
}

// RetryWithRecovery returns a release function for the successful attempt.
// Streaming callers MUST attach it to Close; non-stream callers defer it.
// Success stops the recovery timer without canceling the live stream. Failed
// attempts are canceled here. Parent cancellation always takes precedence.
// A fn must honor its context (net/http does); we deliberately never detach a
// goroutine that could keep issuing requests beyond the recovery deadline.
func RetryWithRecovery(ctx context.Context, p RecoveryPolicy, fn func(context.Context) error) (release func(), err error) {
	if session := RecoverySessionFromContext(ctx); session != nil {
		return session.run(ctx, fn)
	}
	if p.MaxDuration < 0 {
		return func() {}, errors.New("invalid recovery policy: duration must be non-negative")
	}
	if !p.Enabled() {
		return func() {}, RetryWithBackoff(ctx, 3, 0, func() error { return fn(ctx) })
	}
	return NewRecoverySession(p).run(ctx, fn)
}

// RecoverySession belongs to ONE logical model response, including dial
// attempts and interrupted streams. Headers alone never reset its counters.
// Discard it only after a complete response or a terminal error. Do not share
// it with other model requests or subagents. Methods serialize mutations;
// observers must not call back into the session.
type RecoverySession struct {
	mu                 sync.Mutex
	policy             RecoveryPolicy
	started            time.Time
	attempts, failures int
	lastErr            error
}
type recoverySessionKey struct{}
type activeRecoverySessionKey struct{}

func NewRecoverySession(p RecoveryPolicy) *RecoverySession { return &RecoverySession{policy: p} }
func WithRecoverySession(ctx context.Context, session *RecoverySession) context.Context {
	return context.WithValue(ctx, recoverySessionKey{}, session)
}
func RecoverySessionFromContext(ctx context.Context) *RecoverySession {
	session, _ := ctx.Value(recoverySessionKey{}).(*RecoverySession)
	return session
}
func RecoveryPolicyForContext(ctx context.Context) (RecoveryPolicy, error) {
	if s := RecoverySessionFromContext(ctx); s != nil {
		return s.policy, nil
	}
	return RecoveryPolicyFromEnv()
}

// RecordFailure records an interrupted stream in the same window that owns
// its HTTP attempts. False means a permanent/exhausted error: do not retry.
func (s *RecoverySession) RecordFailure(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordFailure(err)
}
func (s *RecoverySession) recordFailure(err error) bool {
	if !shouldRecover(err) {
		return false
	}
	s.lastErr = err
	s.failures++
	if s.started.IsZero() {
		s.started = time.Now()
	}
	return true
}
func (s *RecoverySession) WaitRetry(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitRetry(ctx)
}
func (s *RecoverySession) progress(ctx context.Context, state string, delay time.Duration) {
	elapsed := time.Duration(0)
	if !s.started.IsZero() {
		elapsed = time.Since(s.started)
	}
	event := RecoveryProgress{state, s.attempts, s.policy.MaxAttempts, elapsed, max(time.Duration(0), s.policy.MaxDuration-elapsed), delay}
	if observe, _ := ctx.Value(recoveryObserverKey{}).(func(RecoveryProgress)); observe != nil {
		observe(event)
	}
	if isDebugEnabled() {
		writeLog(fmt.Sprintf("%s [recovery] state=%s attempt=%d max_attempts=%d elapsed=%s remaining=%s delay=%s\n", time.Now().UTC().Format(time.RFC3339Nano), state, event.Attempt, event.MaxAttempts, event.Elapsed, event.Remaining, delay))
	}
}
func (s *RecoverySession) exhausted(ctx context.Context, window bool) error {
	err := s.lastErr
	if window {
		err = errors.Join(err, ErrRecoveryWindowExceeded)
	}
	if err == nil {
		err = errors.New("network recovery attempt budget exhausted")
	}
	s.progress(ctx, "exhausted", 0)
	return &RetryExhaustedError{Err: err, Attempts: s.attempts}
}
func (s *RecoverySession) check(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !s.started.IsZero() && time.Since(s.started) >= s.policy.MaxDuration {
		return s.exhausted(ctx, true)
	}
	if s.attempts >= s.policy.MaxAttempts {
		return s.exhausted(ctx, false)
	}
	return nil
}
func (s *RecoverySession) waitRetry(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if s.started.IsZero() {
		return errors.New("cannot wait for recovery before a transient failure")
	}
	delay := min(recoveryBackoff(s.failures-1, s.policy.MaxBackoff, s.lastErr), s.policy.MaxDuration-time.Since(s.started))
	s.progress(ctx, "waiting", delay)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
	}
	return s.check(ctx)
}
func (s *RecoverySession) run(ctx context.Context, fn func(context.Context) error) (func(), error) {
	if active, _ := ctx.Value(activeRecoverySessionKey{}).(*RecoverySession); active == s {
		// A custom wrapper that drops the optional ownership marker must
		// fail safely instead of recursively locking the same session.
		return func() {}, errors.New("nested network recovery for one request: provider wrapper must forward ManagesRecoverySession")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	noop := func() {}
	if !s.policy.Enabled() || s.policy.MaxAttempts < 1 || s.policy.MaxBackoff <= 0 {
		return noop, errors.New("invalid enabled recovery policy")
	}
	for {
		if err := s.check(ctx); err != nil {
			return noop, err
		}
		s.attempts++
		attemptCtx, cancel := context.WithCancelCause(ctx)
		var timer *time.Timer
		var timerDone chan struct{}
		if !s.started.IsZero() {
			timerDone = make(chan struct{})
			timer = time.AfterFunc(s.policy.MaxDuration-time.Since(s.started), func() { cancel(ErrRecoveryWindowExceeded); close(timerDone) })
		}
		err := fn(context.WithValue(attemptCtx, activeRecoverySessionKey{}, s))
		if timer != nil && !timer.Stop() {
			<-timerDone
		}
		if ctx.Err() != nil {
			cancel(nil)
			return noop, ctx.Err()
		}
		if errors.Is(context.Cause(attemptCtx), ErrRecoveryWindowExceeded) || (!s.started.IsZero() && time.Since(s.started) >= s.policy.MaxDuration) {
			cancel(nil)
			// Our private deadline cancels the attempt context, not the user
			// task. Keep the preceding transient cause instead of exporting
			// context.Canceled and misclassifying budget expiry as user cancel.
			if err != nil && !errors.Is(err, context.Canceled) {
				s.lastErr = err
			}
			return noop, s.exhausted(ctx, true)
		}
		if err == nil {
			if !s.started.IsZero() {
				s.progress(ctx, "headers_recovered", 0)
			}
			return func() { cancel(nil) }, nil
		}
		cancel(nil)
		if !s.recordFailure(err) {
			s.lastErr = err
			return noop, err
		}
		if err := s.waitRetry(ctx); err != nil {
			return noop, err
		}
	}
}

func shouldRecover(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsRetryExhausted(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, quota := range []string{"insufficient_quota", "quota_exceeded", "insufficient quota", "quota exceeded"} {
		if strings.Contains(msg, quota) {
			return false
		}
	}
	var status *HTTPStatusError
	if errors.As(err, &status) && status.StatusCode >= 400 {
		return status.StatusCode == 429 || status.StatusCode >= 500 && status.StatusCode <= 599
	}
	var retryable *RetryableError
	return errors.As(err, &retryable) || IsNetworkError(err)
}

func recoveryBackoff(failures int, cap time.Duration, err error) time.Duration {
	base := 500 * time.Millisecond
	if isRateLimitError(err) {
		base = 5 * time.Second
	}
	// Saturating multiplication: a large explicit attempt budget cannot wrap
	// the duration and accidentally create a tight retry loop.
	for i := 0; i < failures && base < cap; i++ {
		if base > cap/2 {
			base = cap
		} else {
			base *= 2
		}
	}
	base = min(base, cap)
	var retryable *RetryableError
	if errors.As(err, &retryable) && retryable.After > 0 {
		base = min(retryable.After, cap)
	}
	return base
}
