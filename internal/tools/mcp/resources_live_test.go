package mcp_tools

// resources_live_test.go — env-gated live check that metis reads MCP
// resources from a real @modelcontextprotocol/sdk server. Gated behind
// METIS_MCP_LIVE=1; the harness starts editors/vscode/stub_resources.mjs
// and passes its URL + token.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTPServer_Resources(t *testing.T) {
	if os.Getenv("METIS_MCP_LIVE") != "1" {
		t.Skip("set METIS_MCP_LIVE=1 with METIS_MCP_URL/METIS_MCP_TOKEN")
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
		t.Fatalf("connect: %v", err)
	}
	defer srv.Close()

	res, err := srv.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected at least one resource")
	}
	var uri string
	for _, r := range res {
		if strings.Contains(r.URI, "style") {
			uri = r.URI
		}
	}
	if uri == "" {
		uri = res[0].URI
	}
	t.Logf("resources: %d, reading %s", len(res), uri)

	rr, err := srv.ReadResource(ctx, uri)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rr.Contents) == 0 || !strings.Contains(rr.Contents[0].Text, "Use tabs") {
		t.Errorf("resource content mismatch: %+v", rr.Contents)
	}
	t.Logf("read OK: %q", rr.Contents[0].Text)
}
