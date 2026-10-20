//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func wrapEvaluationPlatform(cmd *exec.Cmd, workspace, tests string) error {
	if _, err := exec.LookPath(darwinSandboxExecutable); err != nil {
		return fmt.Errorf("evaluation sandbox: %w: %v", ErrDependencyMissing, err)
	}
	args := evaluationArgv(cmd)
	cmd.Path = darwinSandboxExecutable
	cmd.Args = append([]string{"sandbox-exec", "-p", buildEvaluationDarwinProfile(workspace, tests)}, args...)
	return nil
}

func buildEvaluationDarwinProfile(workspace, tests string) string {
	runtimeRoots := []string{
		"/usr/bin", "/bin", "/usr/lib", "/usr/share", "/System/Library",
		"/Library/Developer/CommandLineTools", "/Library/Apple/System/Library",
		"/System/Volumes/Preboot/Cryptexes/OS/usr/lib",
		"/System/Volumes/Preboot/Cryptexes/OS/System/Library",
		workspace, tests,
	}
	rules := []string{
		"(version 1)", "(deny default)",
		// These process queries need an explicit deny on macOS: relying on
		// deny-default alone still exposes another same-UID process's launch
		// arguments/environment through KERN_PROCARGS2. Self queries remain
		// available because the system Python launcher needs them.
		"(deny process-info* (require-not (target self)))",
		"(allow process-exec process-fork)",
		"(allow signal (target self))",
		// dyld opens the root directory during process startup. This literal
		// allows its directory entry, never arbitrary descendant file data.
		`(allow file-read* (literal "/"))`,
		`(allow file-read* (literal "/dev/null") (literal "/dev/random") (literal "/dev/urandom"))`,
		// Apple's /bin/sh consults this fixed system shell selector before
		// executing builtins. Permit only the selector (including its /var
		// alias); denying it emits a warning even when the command succeeds.
		`(allow file-read* (literal "/private/var/select/sh") (literal "/var/select/sh"))`,
		`(allow file-write* (literal "/dev/null"))`,
	}
	metadata := map[string]bool{}
	for _, root := range runtimeRoots {
		rules = append(rules, "(allow file-read* (subpath "+strconv.Quote(root)+"))")
		for dir := filepath.Dir(root); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			if !metadata[dir] {
				metadata[dir] = true
				rules = append(rules, "(allow file-read-metadata (literal "+strconv.Quote(dir)+"))")
			}
		}
	}
	// The system Python launcher consults these fixed developer-selection
	// locations. Only their metadata is necessary, including absent paths.
	for _, path := range []string{"/var", "/var/select", "/var/select/developer_dir", "/private", "/private/var", "/private/var/select", "/private/var/select/developer_dir", "/var/db/xcode_select_link", "/private/var/db/xcode_select_link"} {
		rules = append(rules, "(allow file-read-metadata (literal "+strconv.Quote(path)+"))")
	}
	rules = append(rules, "(allow file-write* (subpath "+strconv.Quote(workspace)+"))")
	return strings.Join(rules, "\n") + "\n"
}
