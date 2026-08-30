package builtin

import (
	"bytes"
	"os/exec"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

const (
	lspProcessTreeKillLimit = 2 * time.Second
	lspProcessWaitLimit     = 2 * time.Second
)

// configureLSPProcess gives an LSP server and every process it spawns a
// dedicated process group. CommandContext otherwise kills only the immediate
// child, which can leave compiler helpers holding the stdio pipes open.
func configureLSPProcess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	jobs.ApplyProcessGroup(cmd)
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			killLSPProcessTree(cmd)
			return nil
		}
	}
	// Bound os/exec's own pipe cleanup if a descendant escapes the process
	// group or inherits a pipe handle on a platform without Unix groups.
	cmd.WaitDelay = lspProcessWaitLimit
}

// killLSPProcessTree invokes the platform tree-kill primitive with a hard
// bound, then always attempts a direct leader kill as a final fallback.
func killLSPProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killLSPProcessTreePlatform(cmd.Process, lspProcessTreeKillLimit)
	_ = cmd.Process.Kill()
}

func waitForLSPProcess(done <-chan error, waitLimit time.Duration) bool {
	if waitLimit <= 0 {
		return false
	}
	timer := time.NewTimer(waitLimit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// runLSPCombinedOutput is exec.Cmd.CombinedOutput plus post-Start process-tree
// attachment. On Windows this assigns gopls to a kill-on-close Job Object;
// stdlib CombinedOutput provides no hook between Start and Wait to do that.
func runLSPCombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	tree := attachLSPProcessTree(cmd)
	err := cmd.Wait()
	if tree != nil {
		// The leader may have exited while compiler helpers remain alive. Job
		// termination before handle close deterministically drains descendants.
		terminateLSPProcessTreeHandle(tree)
		closeLSPProcessTreeHandle(tree)
	}
	return output.Bytes(), err
}
