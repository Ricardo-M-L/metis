//go:build windows

package skills

import "os/exec"

// Windows has no Unix process groups or negative-pid kill. Keep
// exec.CommandContext's default single-process cancellation; the common
// WaitDelay still bounds pipe cleanup if a descendant inherits a handle.
func configureInlineShellCancellation(_ *exec.Cmd) {}
