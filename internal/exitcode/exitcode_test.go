package exitcode

// Pin every exit-code branch so a future "let's reorganize errors"
// refactor can't silently change what shell wrappers see. Codes are
// stable in API terms — adding new codes is fine, repurposing
// existing ones is not.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

type testOAuthStatusError struct {
	status int
}

func (e *testOAuthStatusError) Error() string        { return "oauth endpoint failed" }
func (e *testOAuthStatusError) OAuthStatusCode() int { return e.status }

func TestClassify_Nil(t *testing.T) {
	if got := Classify(nil); got != OK {
		t.Errorf("Classify(nil) = %d, want %d", got, OK)
	}
}

func TestClassify_ContextCanceled(t *testing.T) {
	if got := Classify(context.Canceled); got != SIGINT {
		t.Errorf("ctx canceled = %d, want %d", got, SIGINT)
	}
}

func TestClassify_ContextDeadline(t *testing.T) {
	if got := Classify(context.DeadlineExceeded); got != Timeout {
		t.Errorf("ctx deadline = %d, want %d", got, Timeout)
	}
}

func TestClassify_FilesystemPerm(t *testing.T) {
	if got := Classify(os.ErrPermission); got != Permission {
		t.Errorf("ErrPermission = %d, want %d", got, Permission)
	}
	if got := Classify(syscall.EACCES); got != Permission {
		t.Errorf("EACCES = %d, want %d", got, Permission)
	}
}

func TestClassify_DiskFull(t *testing.T) {
	if got := Classify(syscall.ENOSPC); got != IO {
		t.Errorf("ENOSPC = %d, want %d", got, IO)
	}
}

func TestClassify_BrokenPipe(t *testing.T) {
	if got := Classify(syscall.EPIPE); got != IO {
		t.Errorf("EPIPE = %d, want %d", got, IO)
	}
}

func TestClassify_DNSError(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "x.invalid"}
	if got := Classify(dns); got != Network {
		t.Errorf("DNSError = %d, want %d", got, Network)
	}
}

func TestClassify_NetOpError(t *testing.T) {
	op := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if got := Classify(op); got != Network {
		t.Errorf("net.OpError = %d, want %d", got, Network)
	}
}

func TestClassify_RedactedNetworkMarker(t *testing.T) {
	wrapper := fmt.Errorf("provider request failed: %w", transport.ErrNetwork)
	if got := Classify(wrapper); got != Network {
		t.Errorf("redacted network marker = %d, want %d", got, Network)
	}
}

func TestClassify_TypedOAuthStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   int
	}{
		{status: 401, want: Auth},
		{status: 403, want: Auth},
		{status: 429, want: Quota},
	} {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			err := fmt.Errorf("login failed: %w", &testOAuthStatusError{status: tc.status})
			if got := Classify(err); got != tc.want {
				t.Errorf("OAuth status %d = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassify_HeuristicMatches(t *testing.T) {
	cases := []struct {
		err  string
		want int
	}{
		{"unknown flag: --foo", Usage},
		{"flag provided but not defined: -r", Usage},
		{"run: prompt is required", Usage}, // metis run empty-prompt error
		{"argument required for --output", Usage},
		{"401 unauthorized", Auth},
		{"missing api key for openai", Auth},
		{"invalid api key", Auth},
		{"429 too many requests", Quota},
		{"rate limit exceeded", Quota},
		{"insufficient credits", Quota},
		{"request timeout after 30s", Timeout},
		{"context deadline exceeded", Timeout},
		{"response blocked by content_filter", ContentFilter},
		{"refused on safety grounds", ContentFilter},
		{"dial tcp: no such host", Network},
		{"connection refused", Network},
		{"tls: handshake failed", Network},
		{"some random failure", General},
	}
	for _, c := range cases {
		t.Run(c.err, func(t *testing.T) {
			err := errors.New(c.err)
			if got := Classify(err); got != c.want {
				t.Errorf("Classify(%q) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestClassify_TypedBeatsHeuristic — when an error chain contains both
// a typed sentinel and a heuristic-matchable string, the typed match
// must win. (`fmt.Errorf("403 forbidden: %w", os.ErrPermission)` is
// a permission-denied filesystem op that *also* mentions 403, and the
// classification needs to be PERMISSION, not AUTH.)
func TestClassify_TypedBeatsHeuristic(t *testing.T) {
	wrapped := fmt.Errorf("403 forbidden: %w", os.ErrPermission)
	if got := Classify(wrapped); got != Permission {
		t.Errorf("typed perm error wrapped with 403 prefix should be Permission; got %d", got)
	}
}

// TestIncompleteError_Classify — the agent layer constructs an
// *IncompleteError when a loop abort path (max_iterations,
// diminishing_returns, loop_detected) fires. Classify must map any
// of these onto the Incomplete exit code (11), regardless of the
// detail text — which may contain heuristic-matching tokens like
// "rate limit" or "deadline" since the model's last activity could
// have been waiting on those before the abort tripped. The typed
// branch fires before the string layer to prevent that misroute.
func TestIncompleteError_Classify(t *testing.T) {
	cases := []struct {
		name string
		err  *IncompleteError
	}{
		{"diminishing_returns", &IncompleteError{Reason: "diminishing_returns", Detail: "3 iters past 75% budget with <256 bytes useful output"}},
		{"max_iterations", &IncompleteError{Reason: "max_iterations", Detail: "budget exhausted (50 iters, 3 grace used)"}},
		{"loop_detected", &IncompleteError{Reason: "loop_detected", Detail: "same tool call+result repeated within window"}},
		// Bare detail with heuristic-matchable tokens — typed branch
		// must still win, NOT route to Quota/Timeout.
		{"detail_with_rate_limit_text", &IncompleteError{Reason: "diminishing_returns", Detail: "model was retrying after 429 rate limit when abort fired"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.err); got != Incomplete {
				t.Errorf("Classify(%v) = %d, want %d (Incomplete)", c.err, got, Incomplete)
			}
		})
	}
}

// TestIncompleteError_Wrapped — must work through fmt.Errorf("%w")
// wrapping so the cmdRun layer can decorate the error with context
// before returning it.
func TestIncompleteError_Wrapped(t *testing.T) {
	ie := &IncompleteError{Reason: "max_iterations", Detail: "50 iters"}
	wrapped := fmt.Errorf("run aborted: %w", ie)
	if got := Classify(wrapped); got != Incomplete {
		t.Errorf("wrapped IncompleteError = %d, want %d", got, Incomplete)
	}
	// And errors.As round-trips.
	var unwrapped *IncompleteError
	if !errors.As(wrapped, &unwrapped) {
		t.Fatal("errors.As did not unwrap IncompleteError")
	}
	if unwrapped.Reason != "max_iterations" {
		t.Errorf("unwrapped reason = %q, want max_iterations", unwrapped.Reason)
	}
}

// TestIncompleteError_NilSafe — defensive: Error() on a nil receiver
// must not panic. cmdRun shouldn't ever return a nil pointer of this
// type, but defensive Error() implementations are cheap.
func TestIncompleteError_NilSafe(t *testing.T) {
	var ie *IncompleteError
	_ = ie.Error() // must not panic
}

// A worker can emit a valid incomplete terminal frame and subsequently fail
// while saving its checkpoint or closing the IPC bridge. That independent
// failure must not be hidden behind the earlier terminal outcome.
func TestClassify_JoinedTerminalOutcomePreservesFailure(t *testing.T) {
	incomplete := &IncompleteError{Reason: "max_iterations", Detail: "last activity mentioned 429 rate limit and timeout"}
	filter := &ContentFilterError{Reason: "content_filter"}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"incomplete_only", errors.Join(incomplete), Incomplete},
		{"incomplete_wrapped", fmt.Errorf("run aborted: %w", incomplete), Incomplete},
		{"joined_incomplete_only", errors.Join(incomplete, fmt.Errorf("another outcome: %w", incomplete)), Incomplete},
		{"filter_only", errors.Join(filter), ContentFilter},
		{"mixed_terminal_outcomes", errors.Join(filter, incomplete), Incomplete},
		{"disk_full", errors.Join(incomplete, fmt.Errorf("save checkpoint: %w", syscall.ENOSPC)), IO},
		{"broken_bridge", errors.Join(incomplete, fmt.Errorf("report checkpoint: %w", syscall.EPIPE)), IO},
		{"permission", errors.Join(incomplete, fmt.Errorf("save checkpoint: %w", os.ErrPermission)), Permission},
		{"opaque_storage_failure", errors.Join(incomplete, errors.New("save checkpoint: backend unavailable")), General},
		{"opaque_storage_failure_first", errors.Join(errors.New("save checkpoint: backend unavailable"), incomplete), General},
		{"canceled", errors.Join(incomplete, context.Canceled), SIGINT},
		{"deadline", errors.Join(incomplete, context.DeadlineExceeded), Timeout},
		{"network", errors.Join(incomplete, transport.ErrNetwork), Network},
		{"oauth", errors.Join(incomplete, &testOAuthStatusError{status: 401}), Auth},
		{"quota", errors.Join(incomplete, errors.New("HTTP 429 rate limit")), Quota},
		{"filter_with_storage_failure", errors.Join(filter, errors.New("save checkpoint: backend unavailable")), General},
		{"filter_with_disk_full", errors.Join(filter, syscall.ENOSPC), IO},
		{"storage_mentions_safety", errors.Join(incomplete, errors.New("save safety database failed")), General},
		{"filter_storage_mentions_safety", errors.Join(filter, errors.New("save safety database failed")), General},
		{"wrapped_join", fmt.Errorf("worker failed: %w", errors.Join(incomplete, errors.New("save checkpoint: backend unavailable"))), General},
		{"typed_network_wrapper", &net.OpError{Op: "read", Net: "tcp", Err: errors.Join(incomplete, errors.New("opaque failure"))}, Network},
		{"quota_wrapper", fmt.Errorf("HTTP 429 rate limit: %w", errors.Join(incomplete, errors.New("opaque failure"))), Quota},
		{"nested_join", errors.Join(incomplete, fmt.Errorf("cleanup failed: %w", errors.Join(filter, syscall.EPIPE))), IO},
		{"multiple_wrap", fmt.Errorf("terminal: %w; storage: %w", incomplete, syscall.ENOSPC), IO},
		{"ordinary_join_keeps_typed_priority", errors.Join(errors.New("HTTP 401 unauthorized"), os.ErrPermission), Permission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Fatalf("Classify(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
