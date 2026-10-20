package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// WrapEvaluation applies the stricter boundary used for host-owned development
// fixtures. Only the workspace is writable; the test directory and fixed system
// runtimes are readable. No home directory, host /proc, or network is exposed.
// Unsupported or unavailable isolation fails closed. The caller must supply a
// minimal environment and must not pass inherited files or host IPC handles.
// The same Cmd is returned so context cancellation and stdio remain attached.
func WrapEvaluation(cmd *exec.Cmd, workspace, testsDir string) (*exec.Cmd, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, errors.New("evaluation sandbox requires a command")
	}
	if len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("evaluation sandbox cannot inherit extra file descriptors")
	}
	work, err := evaluationDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("evaluation workspace: %w", err)
	}
	tests, err := evaluationDirectory(testsDir)
	if err != nil {
		return nil, fmt.Errorf("evaluation tests: %w", err)
	}
	// Disjoint roots prevent a candidate from renaming an ancestor of the
	// immutable tests via the writable workspace on path-based backends.
	if pathWithinRoot(work, tests) || pathWithinRoot(tests, work) {
		return nil, errors.New("evaluation workspace and tests must be disjoint")
	}
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		return nil, fmt.Errorf("evaluation requires system /usr/bin/python3: %w", err)
	}
	if cmd.Env == nil {
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + work, "TMPDIR=" + work}
	}
	cmd.Dir = work
	if err := wrapEvaluationPlatform(cmd, work, tests); err != nil {
		return nil, err
	}
	return cmd, nil
}

func evaluationDirectory(path string) (string, error) {
	if path == "" {
		return "", errors.New("directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || abs == string(filepath.Separator) {
		return "", errors.New("requires an existing non-root directory")
	}
	return abs, nil
}

func evaluationArgv(cmd *exec.Cmd) []string {
	args := append([]string(nil), cmd.Args...)
	if len(args) == 0 {
		return []string{cmd.Path}
	}
	// exec.Cmd.Path is authoritative. A synthetic argv[0] must not select a
	// different executable when another process becomes the wrapper.
	args[0] = cmd.Path
	return args
}
