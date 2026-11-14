//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func configureMemoryDistillationProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
