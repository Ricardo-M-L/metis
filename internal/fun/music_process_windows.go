//go:build windows

package fun

import (
	"os"
	"os/exec"
	"syscall"
)

func configureDetachedPlayer(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// Windows does not expose POSIX SIGTERM/SIGKILL semantics through os.Process;
// Kill is the supported termination primitive for both graceful and forced
// player shutdown paths.
func terminatePlayer(proc *os.Process) error { return proc.Kill() }

func killPlayer(proc *os.Process) error { return proc.Kill() }
