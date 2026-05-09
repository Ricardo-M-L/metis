// Package exitcode classifies CLI errors into shell-friendly exit codes.
//
// Background: a CI script that wraps `metis run` needs to know whether
// the failure was a usage typo (fix the args), an auth problem (fix
// the keys), a quota hit (back off), or a transient network blip
// (retry). A blanket exit-code-1 forces every wrapper to grep stderr
// for clues — fragile and locale-dependent. minimax-cli's `handler.ts`
// solved this by classifying each error class to a distinct numeric
// code; we mirror the convention so shell wrappers can `case $? in
// 3) … ;; 4) … ;; esac`.
//
// Codes are stable: do NOT renumber. Add new codes at the end.
package exitcode

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
)

// Exit codes. Numbering matches the minimax-cli scheme (`src/cli/exit.ts`)
// so users with both CLIs see consistent semantics:
//
//	0   success
//	1   GENERAL — unclassified failure
//	2   USAGE — bad args / unknown flag
//	3   AUTH — missing or invalid credentials
//	4   QUOTA — rate-limited / out of credits
//	5   TIMEOUT — wall-clock or context timeout
//	6   NETWORK — DNS / connection / transport
//	7   PERMISSION — filesystem permission, incl. EACCES
//	8   IO — disk full, read-only fs, broken pipe
//	10  CONTENT_FILTER — provider refused on safety grounds
//	130 SIGINT (Ctrl+C). UNIX convention; we don't redefine it.
//	143 SIGTERM. Same.
const (
	OK             = 0
	General        = 1
	Usage          = 2
	Auth           = 3
	Quota          = 4
	Timeout        = 5
	Network        = 6
	Permission     = 7
	IO             = 8
	ContentFilter  = 10
	SIGINT         = 130
	SIGTERM        = 143
)

// Classify maps an error to one of the codes above. Best-effort —
// nothing here is authoritative; callers who already KNOW the class
// should just use the constant directly.
//
// Error matching is layered: typed errors (errors.As) > sentinel
// matching (errors.Is) > string heuristics on err.Error(). The string
// layer is a fallback for SDK errors that don't expose typed kinds.
func Classify(err error) int {
	if err == nil {
		return OK
	}

	// Cancellation propagated from signal.NotifyContext.
	if errors.Is(err, context.Canceled) {
		return SIGINT
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}

	// Filesystem permission / not-exist / IO.
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) {
		return Permission
	}
	if errors.Is(err, syscall.ENOSPC) {
		return IO
	}
	if errors.Is(err, syscall.EPIPE) {
		return IO
	}

	// Network: net.OpError covers dial / read / write transport errors;
	// DNS lookup failures land in *net.DNSError.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return Network
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return Network
	}

	// Heuristic string layer (SDK errors / wrapped HTTP errors). Order
	// matters: keep Network ahead of ContentFilter so "connection
	// refused" doesn't false-trip on the bare "refused" token.
	low := strings.ToLower(err.Error())
	switch {
	case containsAny(low, "usage:", "unknown flag", "unknown command", "flag provided but not defined",
		"is required", "argument required", "expected argument"):
		return Usage
	case containsAny(low, "401 ", "403 ", "unauthorized", "missing api key", "invalid api key", "api key not found"):
		return Auth
	case containsAny(low, "429 ", "rate limit", "quota", "insufficient credits", "billing"):
		return Quota
	case containsAny(low, "timeout", "deadline exceeded", "context deadline"):
		return Timeout
	case containsAny(low, "no such host", "connection refused", "network is unreachable", "dial tcp", "tls:"):
		return Network
	case containsAny(low, "content_filter", "content policy", "safety", "refused on safety"):
		return ContentFilter
	}
	return General
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
