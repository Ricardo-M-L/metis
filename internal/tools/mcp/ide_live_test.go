package mcp_tools

// ide_live_test.go — env-gated live check that metis's Streamable-HTTP
// MCP client interoperates with a server built on the official
// @modelcontextprotocol/sdk (the same transport the VS Code IDE
// extension uses). The harness starts a standalone SDK stub server and
// passes its URL + token here.
//
// Gated behind METIS_MCP_LIVE=1 so normal `go test ./...` never needs a
// running server. Run via the cross-lang harness.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTPServer_SDKInterop(t *testing.T) {
	if os.Getenv("METIS_MCP_LIVE") != "1" {
		t.Skip("set METIS_MCP_LIVE=1 and provide METIS_MCP_URL/METIS_MCP_TOKEN")
	}
	url := os.Getenv("METIS_MCP_URL")
	token := os.Getenv("METIS_MCP_TOKEN")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var headers map[string]string
	if token != "" {
		headers = map[string]string{"Authorization": "Bearer " + token}
	}
	srv, err := NewHTTPServer(ctx, "ide", url, headers)
	if err != nil {
		t.Fatalf("connect to SDK stub: %v", err)
	}
	defer srv.Close()

	tools := srv.Tools()
	if len(tools) == 0 {
		t.Fatal("expected tools from the SDK server, got none")
	}
	var echo, sel bool
	for _, tl := range tools {
		switch tl.Name() {
		case "mcp__ide__echo":
			echo = true
		case "mcp__ide__getCurrentSelection":
			sel = true
		}
	}
	if !echo || !sel {
		t.Fatalf("missing expected tools (echo=%v sel=%v); got %d tools", echo, sel, len(tools))
	}

	// Call echo and verify the round-trip through the real SDK transport.
	for _, tl := range tools {
		if tl.Name() == "mcp__ide__echo" {
			res, err := tl.Execute(ctx, map[string]any{"msg": "ping"})
			if err != nil {
				t.Fatalf("echo execute: %v", err)
			}
			if !strings.Contains(res.Output, "echo:ping") {
				t.Errorf("echo output = %q, want it to contain echo:ping", res.Output)
			}
			t.Logf("echo round-trip OK: %q", res.Output)
		}
	}
}
