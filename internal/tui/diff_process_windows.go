//go:build windows

package tui

import (
	"os/exec"
	"strconv"
)

// Windows has no Unix process groups. taskkill /T walks the descendant tree;
// /F ensures cancellation closes inherited output handles promptly.
func configureDiffCommandCancellation(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return nil
		}
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		// Cancellation is best-effort and idempotent. Returning nil preserves
		// the originating context cancellation as Cmd.Run's reported error.
		return nil
	}
}
