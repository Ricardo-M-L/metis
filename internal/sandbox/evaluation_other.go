//go:build !darwin && !linux

package sandbox

import "os/exec"

func wrapEvaluationPlatform(_ *exec.Cmd, _, _ string) error {
	return ErrUnsupportedPlatform
}
