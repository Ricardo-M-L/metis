package mcp_tools

// roots_live_test.go — env-gated live check of the roots/list round-trip:
// the SDK stub's `whoami` tool asks the client (metis) for its workspace
// roots via a server→client roots/list request; metis must answer with
// its cwd. Validates the bidirectional request path end-to-end.
// METIS_MCP_LIVE=1 + the editors/vscode/stub_roots.mjs harness.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTPServer_RootsRoundTrip(t *testing.T) {
	if os.Getenv("METIS_MCP_LIVE") != "1" {
		t.Skip("set METIS_MCP_LIVE=1 with the roots stub harness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	headers := map[string]string{"Authorization": "Bearer " + os.Getenv("METIS_MCP_TOKEN")}
	srv, err := NewHTTPServer(ctx, "ide", os.Getenv("METIS_MCP_URL"), headers)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer srv.Close()

	for _, tl := range srv.Tools() {
		if tl.Name() != "mcp__ide__whoami" {
			continue
		}
		r, e := tl.Execute(ctx, map[string]any{})
		if e != nil {
			t.Fatal(e)
		}
		if r.IsError || !strings.Contains(r.Output, "file://") {
			t.Errorf("expected the server to receive a file:// root from metis, got %q (err=%v)", r.Output, r.IsError)
		}
		t.Logf("server saw client root: %s", r.Output)
		return
	}
	t.Fatal("whoami tool not found")
}
