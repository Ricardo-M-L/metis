//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
)

func wrapEvaluationPlatform(cmd *exec.Cmd, workspace, tests string) error {
	path, err := exec.LookPath(linuxSandboxExecutable)
	if err != nil {
		return fmt.Errorf("evaluation sandbox: %w: %v", ErrDependencyMissing, err)
	}
	args := buildEvaluationLinuxArgs(workspace, tests, evaluationArgv(cmd))
	cmd.Path = path
	cmd.Args = args
	return nil
}

func buildEvaluationLinuxArgs(workspace, tests string, command []string) []string {
	args := []string{
		"bwrap", "--die-with-parent", "--new-session", "--unshare-all",
		"--cap-drop", "ALL", "--tmpfs", "/", "--dev", "/dev",
	}
	// Begin with an empty root, rather than masking selected host secrets.
	// No /proc is mounted: a fixture cannot read the parent's environment or
	// use a host process's root/cwd/file-descriptor links to escape the view.
	for _, path := range []string{"/usr/bin", "/bin", "/usr/lib", "/usr/lib64", "/lib", "/lib64", "/usr/share"} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args, "--ro-bind", tests, tests, "--bind", workspace, workspace,
		"--remount-ro", "/", "--chdir", workspace, "--")
	return append(args, command...)
}
