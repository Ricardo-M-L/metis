//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"os/exec"
	"runtime"
)

func doctorPlatform() Diagnostic {
	return Diagnostic{
		Platform:  runtime.GOOS,
		Backend:   "none",
		Supported: false,
		Available: false,
		Err:       fmt.Errorf("%w: %s has no Metis OS sandbox backend", ErrUnsupportedPlatform, runtime.GOOS),
	}
}

func wrapPlatform(_ *exec.Cmd, _ platformRequest) error {
	return doctorPlatform().Err
}
