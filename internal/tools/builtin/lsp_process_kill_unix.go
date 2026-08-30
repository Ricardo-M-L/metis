//go:build !windows

package builtin

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Unix process groups are attached before Start through SysProcAttr, so no
// post-spawn handle is required. These no-ops keep the common lifecycle shared
// with Windows Job Objects.
type lspProcessTreeHandle struct{}

func attachLSPProcessTree(*exec.Cmd) *lspProcessTreeHandle { return nil }
func terminateLSPProcessTreeHandle(*lspProcessTreeHandle)  {}
func closeLSPProcessTreeHandle(*lspProcessTreeHandle)      {}

// Unix process-group signaling is a single non-blocking syscall. The process
// was assigned pgid==pid by jobs.ApplyProcessGroup before Start.
func killLSPProcessTreePlatform(process *os.Process, _ time.Duration) {
	if process == nil || process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
}
