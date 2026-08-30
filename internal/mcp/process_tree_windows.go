//go:build windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func configureStdioProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func terminateStdioProcessTree(process *os.Process, limit time.Duration) {
	if process == nil || process.Pid <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	killer := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(process.Pid))
	killer.WaitDelay = limit
	_ = killer.Run()
	// If taskkill itself failed or timed out, still terminate the leader. This
	// cannot guarantee escaped descendants, but pipe closure in Close remains
	// bounded and prevents the Metis runtime from hanging indefinitely.
	_ = process.Kill()
}
