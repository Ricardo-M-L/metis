package jobs

import (
	"errors"
	"os/exec"
	"runtime"
	"sync"
)

// ProcessGroupCleaner shares one tree-kill between cancellation and cleanup
// after Wait. Create it after ApplyProcessGroup and before starting the command.
type ProcessGroupCleaner struct {
	command *exec.Cmd
	once    sync.Once
}

func NewProcessGroupCleaner(command *exec.Cmd) *ProcessGroupCleaner {
	return &ProcessGroupCleaner{command: command}
}

// Kill terminates the owned tree at most once. Cancellation must call this
// while the leader is still available for Windows taskkill /T to find its tree.
func (c *ProcessGroupCleaner) Kill() {
	if c == nil || c.command == nil || c.command.Process == nil {
		return
	}
	c.once.Do(func() { KillProcessGroup(c.command.Process) })
}

// AfterWait preserves Unix descendant cleanup after leader exit and handles
// incomplete pipe joins on Windows. A fully reaped Windows command needs no
// taskkill: its exited leader no longer identifies an active taskkill tree.
func (c *ProcessGroupCleaner) AfterWait(waitErr error) {
	if c != nil && needsProcessGroupCleanupAfterWait(c.command, waitErr, runtime.GOOS == "windows") {
		c.Kill()
	}
}

func needsProcessGroupCleanupAfterWait(command *exec.Cmd, waitErr error, windows bool) bool {
	if command == nil || command.Process == nil {
		return false
	}
	return !windows || command.ProcessState == nil || !command.ProcessState.Exited() || errors.Is(waitErr, exec.ErrWaitDelay)
}
