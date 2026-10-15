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

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
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
//	11  INCOMPLETE — loop aborted before the task was confirmed done
//	    (budget exhausted / diminishing returns / repetitive-loop
//	    detector). The model returned NO error, but the run shouldn't
//	    be treated as success. See IncompleteError below.
//	130 SIGINT (Ctrl+C). UNIX convention; we don't redefine it.
//	143 SIGTERM. Same.
const (
	OK            = 0
	General       = 1
	Usage         = 2
	Auth          = 3
	Quota         = 4
	Timeout       = 5
	Network       = 6
	Permission    = 7
	IO            = 8
	ContentFilter = 10
	Incomplete    = 11
	SIGINT        = 130
	SIGTERM       = 143
)

// IncompleteError signals a loop that exited a non-success abort path
// (the model didn't error, but the agent stopped before confirming the
// task was done — e.g., budget exhausted, repetitive-loop detector
// tripped, diminishing-returns abort). Carries a machine-readable
// Reason (matches loop.go's stopReason strings) and a human-readable
// Detail (the same text the agent emitted as EventInfo at abort).
//
// Treating this as a distinct exit code (11) lets CI scripts tell
// "agent stopped early" apart from "agent errored" (general/1) and
// "agent succeeded" (0). Prior behavior was to swallow these aborts
// as exit 0, which fooled scripts into thinking the task was done.
type IncompleteError struct {
	Reason string // machine-readable, e.g. "diminishing_returns", "max_iterations", "loop_detected"
	Detail string // human-readable; usually the abort's EventInfo text
}

func (e *IncompleteError) Error() string {
	if e == nil {
		return "incomplete: <nil>"
	}
	if e.Detail != "" {
		return "incomplete (" + e.Reason + "): " + e.Detail
	}
	return "incomplete: " + e.Reason
}

// ContentFilterError identifies a provider's terminal refusal separately from
// independent worker failures whose messages happen to mention safety.
type ContentFilterError struct {
	Reason string
}

func (e *ContentFilterError) Error() string {
	if e == nil {
		return "content_filter: <nil>"
	}
	return "content_filter: provider stopped generation (" + e.Reason + ")"
}

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

	// A terminal outcome and a later checkpoint/IPC failure are independent
	// branches. Keep the real failure's typed classification, but exclude the
	// terminal branch's descriptive text from heuristic matching.
	summary := summarizeTerminalBranches(err)
	if summary.joined {
		if !summary.failure && summary.terminal != OK {
			return summary.terminal
		}
		code := classifyFailure(err, summary.text)
		if summary.terminal != OK && code == ContentFilter {
			// Only explicit terminal types may suppress independent failures.
			// "save safety database failed" is not a provider refusal.
			return General
		}
		return code
	}

	// Typed IncompleteError takes precedence — the agent layer
	// constructs this directly when a loop abort path was hit, so we
	// must respect it before any string heuristics that might also
	// match its message text.
	var ie *IncompleteError
	if errors.As(err, &ie) {
		return Incomplete
	}
	var filtered *ContentFilterError
	if errors.As(err, &filtered) {
		return ContentFilter
	}
	return classifyFailure(err, err.Error())
}

type terminalBranchSummary struct {
	text     string
	terminal int
	failure  bool
	joined   bool
}

func summarizeTerminalBranches(err error) terminalBranchSummary {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		summary := terminalBranchSummary{joined: true}
		text := err.Error()
		var failures []string
		for _, child := range multi.Unwrap() {
			if child == nil {
				continue
			}
			part := summarizeTerminalBranches(child)
			summary.failure = summary.failure || part.failure
			if part.terminal > summary.terminal {
				summary.terminal = part.terminal
			}
			if part.failure {
				failures = append(failures, part.text)
			}
			// Standard joins and fmt wrappers include the complete child text.
			// If a custom wrapper doesn't, use only known failure branch text.
			if strings.Contains(text, child.Error()) {
				text = strings.Replace(text, child.Error(), part.text, 1)
			} else {
				text = ""
			}
		}
		if text == "" {
			text = strings.Join(failures, "\n")
		}
		summary.text = text
		return summary
	}
	if single, ok := err.(interface{ Unwrap() error }); ok && single.Unwrap() != nil {
		child := single.Unwrap()
		summary := summarizeTerminalBranches(child)
		if summary.joined {
			if summary.failure && strings.Contains(err.Error(), child.Error()) {
				summary.text = strings.Replace(err.Error(), child.Error(), summary.text, 1)
			}
			return summary
		}
	}
	var incomplete *IncompleteError
	if errors.As(err, &incomplete) {
		return terminalBranchSummary{terminal: Incomplete}
	}
	var filtered *ContentFilterError
	if errors.As(err, &filtered) {
		return terminalBranchSummary{terminal: ContentFilter}
	}
	return terminalBranchSummary{text: err.Error(), failure: true}
}

func classifyFailure(err error, message string) int {

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
	if errors.Is(err, transport.ErrNetwork) {
		return Network
	}

	var oauthStatus interface{ OAuthStatusCode() int }
	if errors.As(err, &oauthStatus) {
		switch oauthStatus.OAuthStatusCode() {
		case 401, 403:
			return Auth
		case 429:
			return Quota
		}
	}

	// Heuristic string layer (SDK errors / wrapped HTTP errors). Order
	// matters: keep Network ahead of ContentFilter so "connection
	// refused" doesn't false-trip on the bare "refused" token.
	low := strings.ToLower(message)
	switch {
	case containsAny(low, "usage:", "unknown flag", "unknown command", "flag provided but not defined",
		"is required", "argument required", "expected argument"):
		return Usage
	case containsAny(low, "401 ", "403 ", "http 401", "http 403", "unauthorized", "missing api key", "invalid api key", "api key not found"):
		return Auth
	case containsAny(low, "429 ", "rate limit", "quota", "insufficient credits", "billing"):
		return Quota
	case containsAny(low, "timeout", "deadline exceeded", "context deadline"):
		return Timeout
	case containsAny(low, "no such host", "connection refused", "connection reset", "broken pipe", "network is unreachable", "dial tcp", "read tcp", "tls:"):
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
