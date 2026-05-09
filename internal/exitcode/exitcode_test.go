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
)

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
