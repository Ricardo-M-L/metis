// Package timebudget implements opt-in wall-clock budgets shared by entrypoints.
package timebudget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// FromEnv returns zero for an unset, empty, or zero budget. Invalid values
// fail closed rather than silently disabling an operator's requested limit.
func FromEnv(name string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("invalid %s: expected non-negative integer seconds within time.Duration range (0 disables the budget)", name)
	}
	return time.Duration(n) * time.Second, nil
}

// WithEnv keeps caller cancellation/deadlines even when the local budget is
// unlimited. Descendants must not detach this invocation-wide deadline.
func WithEnv(parent context.Context, name string) (context.Context, context.CancelFunc, error) {
	budget, err := FromEnv(name)
	if err != nil {
		return nil, nil, err
	}
	if budget == 0 {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, nil
	}
	cause := fmt.Errorf("%s wall-clock budget reached (%s): %w", name, budget, context.DeadlineExceeded)
	ctx, cancel := context.WithTimeoutCause(parent, budget, cause)
	return ctx, cancel, nil
}

// CauseError preserves a configured deadline's useful reason and sentinel,
// without replacing unrelated provider/tool failures with a coincident timeout.
func CauseError(ctx context.Context, err error) error {
	if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
			return errors.Join(cause, err)
		}
	}
	return err
}
