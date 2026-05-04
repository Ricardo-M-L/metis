// Package runtime — TCP/TLS preconnect.
//
// During cold start, metis spends 100-300ms on the first API request's
// TCP handshake + TLS negotiation before the LLM even sees the prompt.
// claude-code's utils/apiPreconnect.ts fires a HEAD/no-op fetch to the
// API origin in parallel with the rest of init so that handshake is
// warmed up by the time the first real query hits.
//
// We do the same: a goroutine that opens a TCP+TLS connection (via
// http.Head) to the provider's base URL and discards the result. The
// kernel keeps the resolved DNS + cached TLS ticket / session so the
// next dial reuses them. Failure modes (DNS down, network unreachable)
// are silent — preconnect is purely an optimization, not a health check.
package runtime

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"
)

// preconnectClient is dedicated to warmup; we keep it separate from the
// provider's real http.Client so timeouts and idle settings don't cross-
// contaminate (real provider may want long timeouts; preconnect should
// fail fast so a slow upstream doesn't pin the goroutine).
var preconnectClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		// Keep idle connections so the real client (if it shares the
		// default Go transport) can reuse them. ForceAttemptHTTP2
		// matches the typical Anthropic transport so the protocol
		// negotiation also gets cached.
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 2 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Most LLM endpoints terminate TLS 1.2+ with modern ciphers;
		// no special config needed.
		TLSClientConfig: &tls.Config{},
	},
}

// Preconnect fires a HEAD request to baseURL in a background goroutine
// to warm up the TCP+TLS connection. Non-blocking; safe to call as
// "fire and forget". Empty / invalid baseURL → no-op.
//
// Why HEAD vs GET: HEAD costs the same handshake but the server returns
// no body (or 405 Method Not Allowed, which we accept — the connection
// is still warmed up). Some Anthropic-compat gateways respond 404 to
// "/" with HEAD; that's also fine for our purposes.
func Preconnect(baseURL string) {
	if baseURL == "" {
		return
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return // bad URL → silently skip; provider will surface real error later
	}
	go func() {
		// We use the root path; some providers reject anything else
		// (e.g. /v1/messages requires POST + auth). Status code is
		// irrelevant — we only care that the TCP+TLS handshake
		// completes so the kernel caches the session.
		req, _ := http.NewRequest(http.MethodHead, u.Scheme+"://"+u.Host+"/", nil)
		resp, err := preconnectClient.Do(req)
		if err != nil {
			return // network down / DNS fail / TLS reject — silently ignore
		}
		_ = resp.Body.Close()
	}()
}
