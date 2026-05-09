package transport

// httpclient.go — public constructors that wrap a vanilla
// http.Client with the logging + dump RoundTrippers. Provider New()
// functions call NewHTTPClient(timeout, providerName) instead of
// `&http.Client{Timeout: timeout}` to opt their requests into the
// debug surface.
//
// Layering (outermost first):
//
//   loggingTransport  ← always there; cheap, env-gated emit
//     dumpTransport   ← always there; bypasses fast when METIS_DUMP_PROMPTS off
//       innerRT       ← the original http.DefaultTransport (or whatever
//                        the caller passed in; tests inject a stub)
//
// Why two layers instead of one fat one: the loggingTransport's
// debug.log line is useful even with dumps off (1k lines/day,
// trivial cost), and turning dumps on shouldn't gate request-id
// injection. Splitting also keeps each file under 250 lines.

import (
	"net/http"
	"sync"
	"time"
)

// SessionIDFn is how we plumb the session id into a long-lived
// http.Client. The caller-supplied closure is read on every
// RoundTrip so a single client can outlive many session-ids
// (e.g. /new mid-process). Pass nil for "default" naming.
type SessionIDFn func() string

// NewHTTPClient returns an http.Client whose Transport is
// loggingTransport(dumpTransport(http.DefaultTransport)). Used by
// every provider's New() to install the debug stack uniformly.
//
//   timeout    — maps to http.Client.Timeout (per-request total).
//   provider   — short tag included in every log line and dump entry
//                ("anthropic", "openai", "gemini", ...).
//
// The active session id is resolved at request time via the
// process-global GlobalSessionID. Callers don't pass it because
// providers are constructed before `runtime` knows the session id
// (resume vs new branches both decide later).
func NewHTTPClient(timeout time.Duration, provider string) *http.Client {
	rt := WrapRoundTripper(http.DefaultTransport, provider, GlobalSessionID)
	return &http.Client{
		Transport: rt,
		Timeout:   timeout,
	}
}

// activeSessionID is the process-global session tag used by the
// dump-prompts file router. SetSessionID is called once per session
// bootstrap from runtime/sessionboot. Default empty → "default".
var (
	sessionMu sync.RWMutex
	sessionID string
)

// SetSessionID is wired from runtime once a session is open. Safe to
// call repeatedly (e.g. /new mid-process flips to a fresh id).
func SetSessionID(id string) {
	sessionMu.Lock()
	sessionID = id
	sessionMu.Unlock()
}

// GlobalSessionID is the SessionIDFn used by NewHTTPClient. Closures
// over the package-global so providers don't need to thread an id
// through their constructors.
func GlobalSessionID() string {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessionID
}

// WrapRoundTripper layers logging + dump on an arbitrary base
// RoundTripper. Useful when a caller already has a custom
// http.Transport (e.g. proxy-aware Vertex auth) — they pass it in
// instead of going through DefaultTransport.
//
// sessIDFn is queried at every request-time inside dumpTransport so
// /new mid-process picks up the new id without rebuilding the
// transport stack.
func WrapRoundTripper(base http.RoundTripper, provider string, sessIDFn SessionIDFn) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	dump := &dumpTransport{
		inner:    base,
		provider: provider,
		sessIDFn: sessIDFn,
	}
	log := &loggingTransport{
		inner:    dump,
		provider: provider,
	}
	return log
}
