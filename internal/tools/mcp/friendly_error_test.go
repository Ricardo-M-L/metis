package mcp_tools

import (
	"errors"
	"strings"
	"testing"
)

// TestFriendlyMCPError_TransportFailureHasActionableGuidance — 2026-05-26
// regression for session 41040bea: a dead cu MCP subprocess used to
// surface as `mcp stdio write: write |1: broken pipe`, indistinguishable
// to the model from a malformed call. The model burned 20+ turns
// retrying. friendlyMCPError must now produce text that (a) names the
// dead server, (b) tells the model not to retry blindly, (c) points
// at the recovery command.
func TestFriendlyMCPError_TransportFailureHasActionableGuidance(t *testing.T) {
	cases := []string{
		"mcp stdio write: write |1: broken pipe",
		"transport closed",
		"read tcp 127.0.0.1:1234->127.0.0.1:5678: use of closed network connection",
		"unexpected EOF",
		"dial tcp 127.0.0.1:9999: connect: connection refused",
		"file already closed",
	}
	for _, raw := range cases {
		got := friendlyMCPError("computer-use", errors.New(raw))
		if !strings.Contains(got, "computer-use") {
			t.Errorf("transport msg %q lost the server name; got %q", raw, got)
		}
		if !strings.Contains(got, "no longer reachable") {
			t.Errorf("transport msg %q missing the structured marker; got %q", raw, got)
		}
		if !strings.Contains(got, "Do not retry") {
			t.Errorf("transport msg %q should tell the model not to retry; got %q", raw, got)
		}
		if !strings.Contains(got, raw) {
			t.Errorf("transport msg %q lost the raw cause; got %q", raw, got)
		}
	}
}

// TestFriendlyMCPError_CuServerSuggestsCuRestart — cu is the only
// server with a dedicated slash command (`/cu`). For every other MCP
// server we suggest the generic `/mcp restart <name>`. Verifies both
// branches so a future rename of /cu can't silently leak through.
func TestFriendlyMCPError_CuServerSuggestsCuRestart(t *testing.T) {
	got := friendlyMCPError("computer-use", errors.New("broken pipe"))
	if !strings.Contains(got, "/cu disable then /cu enable") {
		t.Errorf("cu server should recommend the /cu restart loop; got %q", got)
	}

	got = friendlyMCPError("firecrawl", errors.New("broken pipe"))
	if !strings.Contains(got, "/mcp restart firecrawl") {
		t.Errorf("non-cu server should fall back to generic /mcp restart; got %q", got)
	}
	if strings.Contains(got, "/cu") {
		t.Errorf("non-cu server should NOT mention the /cu command; got %q", got)
	}
}

// TestFriendlyMCPError_NonTransportPassthrough — application-level
// errors (real tool failures, schema mismatches, server-side validation)
// must NOT be reframed as transport failures. The model needs to act on
// those normally; wrapping them in "subprocess crashed" guidance would
// derail recovery for a perfectly-alive server.
func TestFriendlyMCPError_NonTransportPassthrough(t *testing.T) {
	cases := []string{
		"tier \"click\" on app \"iTerm2\" does not permit \"full\" operations",
		"invalid params: missing required field: name",
		"tool list_windows is not implemented yet",
		"tool error: HTTP 429 rate limit exceeded",
	}
	for _, raw := range cases {
		got := friendlyMCPError("computer-use", errors.New(raw))
		if got != raw {
			t.Errorf("non-transport msg %q must pass through unchanged; got %q", raw, got)
		}
	}
}
