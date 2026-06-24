package client

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"
)

// TestSpawn_Handshake is an integration smoke test: it spawns the real
// `metis acp` process and verifies the initialize handshake (no LLM call,
// so it's fast and credential-free). Skipped when no metis binary is on
// PATH (e.g. a clean CI without an install step).
func TestSpawn_Handshake(t *testing.T) {
	if _, err := exec.LookPath("metis"); err != nil {
		t.Skip("metis binary not on PATH; skipping ACP handshake integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Spawn(ctx, WithStderr(io.Discard))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer c.Close()

	if c.ProtocolVersion != protocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", c.ProtocolVersion, protocolVersion)
	}
	if c.Server.Name == "" {
		t.Error("handshake returned empty Server.Name")
	}
}

// TestEventFromMap unit-tests the wire→Event decoder without a subprocess.
func TestEventFromMap(t *testing.T) {
	ev := eventFromMap(map[string]any{
		"kind":       "tool_result",
		"tool_id":    "t1",
		"tool_result": "ok",
		"tool_error": true,
	})
	if ev.Kind != "tool_result" || ev.ToolID != "t1" || ev.ToolResult != "ok" || !ev.ToolError {
		t.Errorf("bad decode: %+v", ev)
	}

	ev2 := eventFromMap(map[string]any{
		"kind":         "permission_request",
		"tool_name":    "Bash",
		"tool_id":      "t2",
		"reason":       "policy",
		"tool_input":   map[string]any{"command": "ls"},
		"input_tokens": float64(12), // JSON numbers decode as float64
	})
	if ev2.ToolName != "Bash" || ev2.Reason != "policy" || ev2.ToolInput["command"] != "ls" || ev2.InputTokens != 12 {
		t.Errorf("bad decode: %+v", ev2)
	}
}
