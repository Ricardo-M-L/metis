//go:build !darwin

package bash

// sandbox_other.go — non-macOS shim for the Seatbelt-based sandbox
// modes. Linux landlock + Windows job-objects are different enough
// (and big enough) that they live in their own future commits;
// right now anything other than `off` rejects the spawn rather than
// silently running unsandboxed (which would betray the user's
// `/sandbox` selection).

import (
	"context"
	"fmt"
	"os/exec"
)

func sandboxAvailable() bool { return false }

func applySandboxWrap(_ context.Context, cmd *exec.Cmd, mode string, _ string) (*exec.Cmd, error) {
	mode = NormalizeSandboxMode(mode)
	if mode == SandboxModeOff {
		return cmd, nil
	}
	return nil, fmt.Errorf("sandbox.bash.mode=%q is only supported on macOS today (Linux landlock / Windows job-objects pending)", mode)
}
