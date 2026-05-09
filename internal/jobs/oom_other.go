//go:build !linux

package jobs

import (
	"context"
	"os/exec"
)

// oom_other.go — no-op fallback for non-Linux platforms. macOS and
// Windows have different OOM handling (no /proc/self/oom_score_adj),
// so we just spawn the bash subprocess directly without the wrapper.
// The Linux-specific behaviour lives in oom_linux.go.

// OOMWrappedCommand returns the unwrapped bash command on non-Linux
// platforms. macOS uses Mach memory pressure events instead of an
// OOM killer; Windows has Job Objects with their own memory limits.
// Neither has an equivalent to /proc/self/oom_score_adj.
func OOMWrappedCommand(ctx context.Context, shell, cmdStr string) *exec.Cmd {
	if shell == "" {
		shell = "/bin/bash"
	}
	return exec.CommandContext(ctx, shell, "-c", cmdStr)
}
