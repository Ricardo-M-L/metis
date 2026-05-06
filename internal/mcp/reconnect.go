package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// reconnect.go — F15: thin wrapper around Client that re-establishes
// the underlying transport when it dies (stdio subprocess crash, HTTP
// SSE stream drop, server restart). Mirrors what claude-code's
// `mcpClients` map does internally — every CallTool that errors with
// "transport closed" gets a fresh dial before bubbling the error up.
//
// Design choices:
//   - Exponential backoff (1s → 2s → 4s → 8s, capped at 30s).
//   - Caller pattern: factory func creates a fresh Client. We retain
//     the factory so reconnection doesn't require knowing whether
//     the original Client was stdio or HTTP-SSE.
//   - Best-effort: if reconnect fails after maxReconnectAttempts the
//     wrapper returns the underlying error. The caller (the MCP
//     tool that wraps a metis Tool) will then fail-soft per the
//     existing tool error path.

// ReconnectingClient wraps Client with auto-restart on transport
// failure. Methods (ListTools, CallTool) try the inner client; on
// transport-level errors, dial a fresh inner client and retry once.
//
// Not goroutine-safe across reconnects; each call serialises through
// the mutex. MCP servers are typically low-throughput (a handful of
// tool calls per turn), so the contention cost is negligible.
type ReconnectingClient struct {
	mu      sync.Mutex
	current *Client
	factory func(ctx context.Context) (*Client, error)
	ctx     context.Context

	attempts int       // consecutive failed reconnect tries
	lastTry  time.Time // backoff timestamp
}

const (
	maxReconnectAttempts = 5
	reconnectBaseDelay   = 1 * time.Second
	reconnectMaxDelay    = 30 * time.Second
)

// NewReconnectingClient takes a factory that produces a fresh Client
// on demand. The factory is called once up front; subsequent calls
// happen on transport failure. The factory MUST be idempotent and
// re-callable (e.g. it can re-spawn the stdio subprocess or re-dial
// the HTTP endpoint).
//
// Returns an error when the FIRST dial fails — the caller decides
// whether to register the MCP server at all if it can't even start.
func NewReconnectingClient(ctx context.Context, factory func(ctx context.Context) (*Client, error)) (*ReconnectingClient, error) {
	c, err := factory(ctx)
	if err != nil {
		return nil, err
	}
	return &ReconnectingClient{
		current: c,
		factory: factory,
		ctx:     ctx,
	}, nil
}

// CallTool proxies to the inner client; on transport-level errors,
// reconnects and retries once. Application-level tool errors (e.g.
// the tool said "no") are returned unchanged — only transport faults
// trigger a reconnect.
func (rc *ReconnectingClient) CallTool(ctx context.Context, name string, args map[string]interface{}) ([]byte, error) {
	rc.mu.Lock()
	cli := rc.current
	rc.mu.Unlock()
	out, err := cli.CallTool(ctx, name, args)
	if err == nil {
		return out, nil
	}
	if !isTransportError(err) {
		return out, err
	}
	// Transport died — try once to reconnect + retry.
	if rerr := rc.reconnect(ctx); rerr != nil {
		return nil, fmt.Errorf("CallTool: transport down and reconnect failed: %w (original: %v)", rerr, err)
	}
	rc.mu.Lock()
	cli = rc.current
	rc.mu.Unlock()
	return cli.CallTool(ctx, name, args)
}

// ListTools — same retry shape as CallTool.
func (rc *ReconnectingClient) ListTools(ctx context.Context) ([]Tool, error) {
	rc.mu.Lock()
	cli := rc.current
	rc.mu.Unlock()
	tools, err := cli.ListTools(ctx)
	if err == nil {
		return tools, nil
	}
	if !isTransportError(err) {
		return tools, err
	}
	if rerr := rc.reconnect(ctx); rerr != nil {
		return nil, fmt.Errorf("ListTools: transport down and reconnect failed: %w (original: %v)", rerr, err)
	}
	rc.mu.Lock()
	cli = rc.current
	rc.mu.Unlock()
	return cli.ListTools(ctx)
}

// Close shuts down the inner client and prevents further reconnects.
func (rc *ReconnectingClient) Close() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.current == nil {
		return nil
	}
	err := rc.current.Close()
	rc.current = nil
	return err
}

// reconnect closes the current client and dials a fresh one with
// exponential backoff. Caller-facing serialisation is via the mutex.
func (rc *ReconnectingClient) reconnect(ctx context.Context) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.attempts >= maxReconnectAttempts {
		return fmt.Errorf("max reconnect attempts (%d) reached", maxReconnectAttempts)
	}
	// Backoff against the previous attempt's wallclock so a flapping
	// server doesn't get hammered.
	delay := reconnectBaseDelay << rc.attempts
	if delay > reconnectMaxDelay {
		delay = reconnectMaxDelay
	}
	if elapsed := time.Since(rc.lastTry); rc.attempts > 0 && elapsed < delay {
		select {
		case <-time.After(delay - elapsed):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	rc.lastTry = time.Now()
	rc.attempts++

	if rc.current != nil {
		_ = rc.current.Close()
		rc.current = nil
	}
	c, err := rc.factory(ctx)
	if err != nil {
		return err
	}
	rc.current = c
	rc.attempts = 0 // success — reset for next failure
	return nil
}

// isTransportError reports whether an error indicates the underlying
// connection died (vs. an application-level tool error). String
// match — providers don't expose typed errors.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, needle := range []string{
		"transport closed",
		"connection refused",
		"broken pipe",
		"EOF",
		"i/o timeout",
		"context canceled",
		"connection reset",
		"use of closed",
	} {
		if contains(msg, needle) {
			return true
		}
	}
	return false
}

// contains is a stdlib-equivalent so we avoid an import.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
