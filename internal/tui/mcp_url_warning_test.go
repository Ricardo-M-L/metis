package tui

import (
	"strings"
	"testing"
)

// TestLooksLikeURLHint_TriggersForURLs — http/https/ws-style /sse /mcp
// suffixes all fire the warning.
func TestLooksLikeURLHint_TriggersForURLs(t *testing.T) {
	for _, command := range []string{
		"https://api.sentry.dev/mcp",
		"http://localhost:8080/sse",
		"https://app.example.com/api/mcp",
		"https://gw.example.com/v1/sse",
	} {
		if hint := looksLikeURLHint(command, "test-server"); hint == "" {
			t.Errorf("expected warning for URL-shaped command %q", command)
		}
	}
}

// TestLooksLikeURLHint_QuietForRealCommands — typical stdio launch
// commands must NOT trip the warning. False positives here would
// turn a legitimate `npx some-mcp` into a noisy success message.
func TestLooksLikeURLHint_QuietForRealCommands(t *testing.T) {
	for _, command := range []string{
		"npx",
		"uvx",
		"python3",
		"/usr/local/bin/some-mcp",
		"./bin/server",
		"metis-cu",
	} {
		if hint := looksLikeURLHint(command, "test-server"); hint != "" {
			t.Errorf("false-positive warning for stdio command %q: %s", command, hint)
		}
	}
}

// TestLooksLikeURLHint_ContainsName — the hint shows the user the
// exact mcp.toml snippet to paste, with their server name filled in.
func TestLooksLikeURLHint_ContainsName(t *testing.T) {
	hint := looksLikeURLHint("https://mcp.sentry.dev/mcp", "sentry-prod")
	if !strings.Contains(hint, "name = \"sentry-prod\"") {
		t.Errorf("hint should reference the user's server name; got:\n%s", hint)
	}
	if !strings.Contains(hint, "https://mcp.sentry.dev/mcp") {
		t.Errorf("hint should include the URL the user typed; got:\n%s", hint)
	}
}
