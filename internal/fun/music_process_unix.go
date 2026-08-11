//go:build !windows

package fun

import (
	"os"
	"os/exec"
	"syscall"
)

func configureDetachedPlayer(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminatePlayer(proc *os.Process) error { return proc.Signal(syscall.SIGTERM) }

func killPlayer(proc *os.Process) error { return proc.Signal(syscall.SIGKILL) }
