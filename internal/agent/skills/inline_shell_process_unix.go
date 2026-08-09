//go:build !windows

package skills

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureInlineShellCancellation gives the shell and its descendants their
// own process group, then makes context cancellation kill that whole group.
// This prevents a surviving grandchild from holding stdout/stderr pipes open
// after the inline-shell timeout.
func configureInlineShellCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return err
		}
		return nil
	}
}
