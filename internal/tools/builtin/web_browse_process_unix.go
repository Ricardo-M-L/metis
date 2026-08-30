//go:build !windows

package builtin

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

type webBrowseProcessTreeHandle struct{}

func attachWebBrowseProcessTree(*exec.Cmd) *webBrowseProcessTreeHandle { return nil }
func terminateWebBrowseProcessTreeHandle(*webBrowseProcessTreeHandle)  {}
func closeWebBrowseProcessTreeHandle(*webBrowseProcessTreeHandle)      {}

func killWebBrowseProcessTreePlatform(process *os.Process, _ time.Duration) {
	if process == nil || process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
}
