//go:build !windows

package mcp

import (
	"os"
	"os/exec"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

func configureStdioProcessTree(cmd *exec.Cmd) {
	jobs.ApplyProcessGroup(cmd)
}

func terminateStdioProcessTree(process *os.Process, _ time.Duration) {
	jobs.KillProcessGroup(process)
}
