package mcp

// End-to-end integration test exercising the full metis ↔ metis-cu
// path: spawn the metis-cu binary as a stdio MCP subprocess via
// NewServer, list its tools, call a no-arg tool (cursor_position)
// and assert the response shape.
//
// Skipped automatically when metis-cu isn't on PATH so this test
// stays informative locally and silent in CI environments where the
// MCP server isn't installed (CI for the metis repo doesn't depend
// on the metis-cu repo build artifact).

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

// TestE2E_MetisCU_Roundtrip — spawn → list tools → call cursor_position.
// Uses NewServer directly (rather than LaunchServer) because we
// don't need to graft into a tools.Registry — just verify the wire is
// real on both ends.
func TestE2E_MetisCU_Roundtrip(t *testing.T) {
	if _, err := exec.LookPath("metis-cu"); err != nil {
		t.Skip("metis-cu not in PATH; skipping (install via the metis-cu repo's `make install`)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	srv, err := mcptools.NewServer(ctx, "computer-use", "metis-cu")
	if err != nil {
		t.Fatalf("spawn metis-cu: %v", err)
	}
	defer srv.Close()

	tools := srv.Tools()
	if len(tools) < 24 {
		t.Fatalf("expected >= 24 tools from metis-cu, got %d", len(tools))
	}

	// Build a name → tool index for easy lookup. Tools are exposed
	// under their full mcp__<server>__<tool> names per Anthropic's
	// namespace.
	byName := map[string]bool{}
	for _, tool := range tools {
		byName[tool.Name()] = true
	}
	required := []string{
		"mcp__computer-use__cursor_position",
		"mcp__computer-use__screenshot",
		"mcp__computer-use__read_clipboard",
		"mcp__computer-use__list_granted_applications",
		"mcp__computer-use__left_click",
		"mcp__computer-use__type",
		"mcp__computer-use__computer_batch",
	}
	for _, n := range required {
		if !byName[n] {
			t.Errorf("missing tool: %s", n)
		}
	}

	// Find cursor_position and invoke it. Empty args is the spec'd
	// shape — empty schema accepts {} but the MCP wire will still
	// send "arguments":{} so we don't bypass that.
	for _, tool := range tools {
		if tool.Name() != "mcp__computer-use__cursor_position" {
			continue
		}
		res, err := tool.Execute(ctx, map[string]any{})
		if err != nil {
			t.Fatalf("cursor_position Execute: %v", err)
		}
		if res == nil {
			t.Fatalf("cursor_position returned nil result")
		}
		if res.IsError {
			t.Fatalf("cursor_position IsError=true; output=%q", res.Output)
		}
		// 2026-05-27: MCPTool.Execute now parses the MCP envelope and
		// surfaces the inner text part directly, no longer the
		// outer JSON-pretty-printed envelope. The cursor JSON
		// (`{"x":N,"y":N}`) thus appears UNESCAPED in res.Output.
		// Pre-fix test asserted `x\":` (escaped); now we check the
		// plain unescaped keys.
		if !strings.Contains(res.Output, `"x":`) || !strings.Contains(res.Output, `"y":`) {
			t.Errorf("cursor_position output doesn't contain {x,y} keys: %s", res.Output)
		}
		t.Logf("cursor_position OK; output:\n%s", strings.TrimSpace(res.Output))
		return
	}
	t.Fatal("cursor_position tool not found in tool list (should have caught earlier)")
}
