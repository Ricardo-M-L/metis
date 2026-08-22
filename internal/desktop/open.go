package desktop

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenPath reveals a file or directory with the platform shell. The caller
// resolves and validates the target first; no command is executed through a
// shell, so path contents cannot become command syntax.
func OpenPath(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open path: %w", err)
	}
	return nil
}
