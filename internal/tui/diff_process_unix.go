//go:build !windows

package tui

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureDiffCommandCancellation isolates git and its descendants in a new
// process group. CommandContext's default cancellation kills only git itself;
// a child hook or credential helper can otherwise retain the output pipes and
// keep Cmd.Wait blocked until that child exits.
func configureDiffCommandCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
