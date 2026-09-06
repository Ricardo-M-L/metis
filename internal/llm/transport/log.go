package transport

// log.go — request-id + URL log RoundTripper (mechanism A in the
// claude-code RoundTripper layout).
//
// Two outputs:
//
//   1. X-Metis-Request-Id header on every outgoing request — gives
//      the caller a stable correlation id between client logs and
//      whatever upstream traces (Cloudflare, ALB, provider-side) the
//      response carries. Mirrors claude-code's
//      services/api/client.ts:368-388 CLIENT_REQUEST_ID_HEADER.
//
//   2. One-line debug log per request to ~/.metis/debug.log when
//      METIS_DEBUG=1 (or when *DEBUG* is otherwise enabled — env
//      check is per-request so users can toggle without restart).
//      We only log URL pathname + reqid + provider name, never body
//      or headers, to avoid leaking secrets into a long-lived file.
//
// This file is the always-on layer. dump.go is the gated heavy layer
// (full body + response). Both can stack via NewHTTPClient.

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/security"
)

// HeaderRequestID is the wire header we set. Lowercased to match Go's
// http.Header canonicalisation; the actual on-the-wire form is
// `X-Metis-Request-Id`.
const HeaderRequestID = "X-Metis-Request-Id"

// loggingTransport is the RoundTripper wrapper. inner is whatever
// http.Transport (or wrapper) the caller already had — we don't
// replace it, we layer on top.
type loggingTransport struct {
	inner    http.RoundTripper
	provider string // "anthropic" / "openai" / etc — for the log line
}

// RoundTrip injects the request-id header, dispatches, and emits one
// log line on completion (success or failure). Errors don't gate the
// log — a connect failure is at least as interesting as a 200.
func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqID := req.Header.Get(HeaderRequestID)
	if reqID == "" {
		reqID = newRequestID()
		req.Header.Set(HeaderRequestID, reqID)
	}
	// Snapshot credential-bearing header values before dispatch. A custom
	// RoundTripper may mutate req.Header, and its error can echo short opaque
	// values that generic token-shape redaction cannot identify.
	exactValues := sensitiveDumpValues(req.Header)

	start := time.Now()
	resp, err := t.inner.RoundTrip(req)
	elapsed := time.Since(start)

	if isDebugEnabled() {
		writeLog(formatLogLine(t.provider, req, resp, err, reqID, elapsed, exactValues...))
	}
	return resp, err
}

// formatLogLine renders one line. Kept tiny — this lands in
// ~/.metis/debug.log and users grep it.
func formatLogLine(provider string, req *http.Request, resp *http.Response, err error, reqID string, elapsed time.Duration, exactValues ...string) string {
	pathOnly := req.URL.Path
	if pathOnly == "" {
		pathOnly = "/"
	}
	status := "—"
	if err != nil {
		// Don't dump the full URL in the error path either — host can
		// leak proxy/gateway hostnames. Status field carries the
		// classification the user actually needs.
		// Redact before truncation: truncating first can split a credential and
		// leave an unrecognizable secret fragment in the log.
		status = "ERR:" + truncErr(security.RedactValues(err.Error(), exactValues...), 80)
	} else if resp != nil {
		status = fmt.Sprintf("%d", resp.StatusCode)
	}
	return fmt.Sprintf(
		"%s [http] %s %s %s %s elapsed=%s reqid=%s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		provider, req.Method, pathOnly, status, elapsed.Round(time.Millisecond), reqID,
	)
}

// truncErr clamps an error string to maxLen — long DNS / TLS errors
// otherwise blow up our nicely-formatted line.
func truncErr(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// newRequestID returns 8 hex chars of crypto-random — enough entropy
// to be unique within a session, short enough to read in a log.
func newRequestID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%08x", uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]))
}

// isDebugEnabled checks the per-request env state. We check on every
// request (not just at construction) so toggling METIS_DEBUG doesn't
// require a metis restart. claude-code does the same — see
// utils/debug.ts:46 for the multi-flag fallback chain.
func isDebugEnabled() bool {
	if isTruthy(os.Getenv("METIS_DEBUG")) {
		return true
	}
	if isTruthy(os.Getenv("DEBUG")) {
		return true
	}
	return false
}

// isTruthy mirrors claude-code's isEnvTruthy: anything non-empty
// other than "0" / "false" / "no" / "off" counts as on.
func isTruthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return false
	}
	switch v {
	case "0", "false", "no", "off", "n":
		return false
	}
	return true
}

var (
	logMu      sync.Mutex
	logHandle  *os.File
	logTrigger sync.Once
)

// writeLog appends line to ~/.metis/debug.log (or whatever
// METIS_DEBUG_LOG points at). Holds a mutex so concurrent provider
// requests don't interleave bytes; opens the file lazily once.
func writeLog(line string) {
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle == nil {
		path := os.Getenv("METIS_DEBUG_LOG")
		if path == "" {
			path = filepath.Join(metisHome(), "debug.log")
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			// Stay silent — debug logging shouldn't crash the agent.
			// Re-arm so a later request can retry (e.g. the dir was
			// missing for a moment, then created).
			logTrigger = sync.Once{}
			return
		}
		logHandle = f
	}
	_, _ = logHandle.WriteString(line)
}

// metisHome returns ~/.metis (or METIS_HOME). Duplicates jobs.home()
// rather than importing it — transport mustn't depend on jobs.
func metisHome() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".metis")
	}
	return "."
}

// closeLogForTest is exposed for tests that need to flush + reopen the
// log file (e.g. between sub-tests with different METIS_DEBUG_LOG
// pointers). Production code should never call it.
func closeLogForTest() {
	logMu.Lock()
	defer logMu.Unlock()
	if logHandle != nil {
		_ = logHandle.Close()
		logHandle = nil
	}
}
