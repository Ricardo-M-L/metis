//go:build !linux

package jobs

import (
	"context"
	"testing"
)

func TestOOMWrappedCommand_NonLinuxPassesThrough(t *testing.T) {
	cmd := OOMWrappedCommand(context.Background(), "/bin/bash", "echo hi")
	// On non-Linux platforms we don't wrap with sh — bash directly.
	if cmd.Path != "/bin/bash" {
		t.Errorf("non-Linux should exec the supplied shell directly, got %q", cmd.Path)
	}
	if len(cmd.Args) != 3 {
		t.Fatalf("expected exactly 3 args (shell, -c, cmd), got %v", cmd.Args)
	}
	if cmd.Args[1] != "-c" || cmd.Args[2] != "echo hi" {
		t.Errorf("args should be [shell -c cmd], got %v", cmd.Args)
	}
}

func TestOOMWrappedCommand_NonLinuxDefaultsToBashWhenShellEmpty(t *testing.T) {
	cmd := OOMWrappedCommand(context.Background(), "", "true")
	if cmd.Path != "/bin/bash" {
		t.Errorf("empty shell should default to /bin/bash, got %q", cmd.Path)
	}
}
