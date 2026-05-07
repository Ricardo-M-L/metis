package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestTimeoutDefaults — when no env var is set, each layer falls back
// to the documented default. Regression: a typo in
// `defaultRequestTimeout` etc would change behavior across the codebase
// silently.
func TestTimeoutDefaults(t *testing.T) {
	t.Setenv("MCP_CONNECT_TIMEOUT", "")
	t.Setenv("MCP_REQUEST_TIMEOUT", "")
	t.Setenv("MCP_TOOL_TIMEOUT", "")
	if got := ConnectTimeout(); got != 30*time.Second {
		t.Errorf("connect default = %v; want 30s", got)
	}
	if got := RequestTimeout(); got != 60*time.Second {
		t.Errorf("request default = %v; want 60s", got)
	}
	if got := ToolTimeout(); got < time.Hour {
		t.Errorf("tool default should be effectively infinite (≥1h); got %v", got)
	}
}

// TestTimeoutEnvOverride — env var with valid Go duration syntax wins.
func TestTimeoutEnvOverride(t *testing.T) {
	t.Setenv("MCP_CONNECT_TIMEOUT", "5s")
	t.Setenv("MCP_REQUEST_TIMEOUT", "2m")
	t.Setenv("MCP_TOOL_TIMEOUT", "1h")
	if got := ConnectTimeout(); got != 5*time.Second {
		t.Errorf("connect override = %v; want 5s", got)
	}
	if got := RequestTimeout(); got != 2*time.Minute {
		t.Errorf("request override = %v; want 2m", got)
	}
	if got := ToolTimeout(); got != time.Hour {
		t.Errorf("tool override = %v; want 1h", got)
	}
}

// TestTimeoutEnvInvalidFallsBack — garbage in env stays out of the
// flow: bad syntax falls back to default rather than panicking or
// producing a 0 timeout (which would instantly cancel every RPC).
func TestTimeoutEnvInvalidFallsBack(t *testing.T) {
	t.Setenv("MCP_CONNECT_TIMEOUT", "not-a-duration")
	t.Setenv("MCP_REQUEST_TIMEOUT", "-5s")
	if got := ConnectTimeout(); got != 30*time.Second {
		t.Errorf("invalid syntax should fall back; got %v", got)
	}
	if got := RequestTimeout(); got != 60*time.Second {
		t.Errorf("negative duration should fall back; got %v", got)
	}
}

// TestBoundedBuffer_Cap — writes past cap silently drop, but the
// String() output marks the elision so users see what happened.
func TestBoundedBuffer_Cap(t *testing.T) {
	b := newBoundedBuffer(10)
	n, err := b.Write([]byte("hello world this is more than ten bytes"))
	if err != nil {
		t.Fatalf("Write should never error; got %v", err)
	}
	if n != len("hello world this is more than ten bytes") {
		t.Errorf("Write should report full length to keep io.Copy draining; got %d", n)
	}
	out := b.String()
	if !strings.HasPrefix(out, "hello worl") {
		t.Errorf("first 10 bytes should survive; got %q", out)
	}
	if !strings.Contains(out, "elided") {
		t.Errorf("truncation suffix missing; got %q", out)
	}
}

// TestBoundedBuffer_NoTruncationMarkerWhenUnderCap — under-cap content
// returns clean (no elision suffix).
func TestBoundedBuffer_NoTruncationMarkerWhenUnderCap(t *testing.T) {
	b := newBoundedBuffer(100)
	b.Write([]byte("short"))
	if got := b.String(); got != "short" {
		t.Errorf("unexpected suffix on under-cap content; got %q", got)
	}
}
