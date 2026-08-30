package builtin

import (
	"os/exec"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

const (
	webBrowseProcessTreeKillLimit = 2 * time.Second
	webBrowseProcessWaitDelay     = 2 * time.Second
)

// configureWebBrowseProcess gives Chromium and every helper it spawns a
// dedicated process tree, replaces CommandContext's leader-only cancellation,
// and bounds pipe cleanup when a descendant inherits stdout or stderr.
func configureWebBrowseProcess(cmd *exec.Cmd) {
	configureWebBrowseProcessWithWaitDelay(cmd, webBrowseProcessWaitDelay)
}

func configureWebBrowseProcessWithWaitDelay(cmd *exec.Cmd, waitDelay time.Duration) {
	if cmd == nil {
		return
	}
	jobs.ApplyProcessGroup(cmd)
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			killWebBrowseProcessTree(cmd)
			return nil
		}
	}
	if waitDelay <= 0 {
		waitDelay = webBrowseProcessWaitDelay
	}
	cmd.WaitDelay = waitDelay
}

func killWebBrowseProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killWebBrowseProcessTreePlatform(cmd.Process, webBrowseProcessTreeKillLimit)
	_ = cmd.Process.Kill()
}

// runWebBrowseCommand is exec.Cmd.Run plus post-Start process-tree ownership.
// Windows attaches Chromium to a kill-on-close Job Object; Unix already owns a
// dedicated process group. Cleanup also runs after an ordinary leader exit,
// because Chromium helpers can outlive the browser process while retaining a
// captured pipe.
func runWebBrowseCommand(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	tree := attachWebBrowseProcessTree(cmd)
	err := cmd.Wait()
	if tree != nil {
		terminateWebBrowseProcessTreeHandle(tree)
		closeWebBrowseProcessTreeHandle(tree)
	} else {
		killWebBrowseProcessTree(cmd)
	}
	return err
}
