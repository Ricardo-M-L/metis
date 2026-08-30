//go:build !windows

package mcp

import (
	"context"
	"testing"
)

func TestStdioTransportStartsInDedicatedProcessGroup(t *testing.T) {
	transport, err := NewStdioTransport(context.Background(), "/bin/sh", "-c", "read _")
	if err != nil {
		t.Fatal(err)
	}
	if transport.cmd == nil || transport.cmd.SysProcAttr == nil || !transport.cmd.SysProcAttr.Setpgid {
		_ = transport.Close()
		t.Fatalf("stdio MCP child lacks a dedicated process group: %#v", transport.cmd)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
}
