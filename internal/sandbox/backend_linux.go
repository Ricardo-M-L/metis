//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const linuxSandboxExecutable = "bwrap"

func doctorPlatform() Diagnostic {
	d := Diagnostic{
		Platform:  runtime.GOOS,
		Backend:   "bubblewrap",
		Supported: true,
	}
	path, err := exec.LookPath(linuxSandboxExecutable)
	if err != nil {
		d.Executable = linuxSandboxExecutable
		d.Err = fmt.Errorf("%w: %s is required when sandbox mode is on: %v", ErrDependencyMissing, linuxSandboxExecutable, err)
		return d
	}
	d.Available = true
	d.Executable = path
	return d
}

func wrapPlatform(cmd *exec.Cmd, req platformRequest) error {
	diagnostic := doctorPlatform()
	if !diagnostic.Available {
		return diagnostic.Err
	}
	if err := validateLinuxProtectedWritePaths(req); err != nil {
		return err
	}
	originalArgv := append([]string(nil), cmd.Args...)
	if len(originalArgv) == 0 {
		originalArgv = []string{cmd.Path}
	}
	// One immutable empty directory is rebound over credential directories.
	// It lives inside the manager-owned temp root, then buildLinuxArgs
	// re-protects the source itself read-only before using it as a mount.
	if err := os.MkdirAll(linuxEmptyDir(req), 0o500); err != nil {
		return fmt.Errorf("sandbox: create bubblewrap credential mask: %w", err)
	}
	// Container control sockets are host-escape capabilities, not ordinary
	// network access, so mask them in every enabled sandbox mode (including
	// network=allow).
	req.blockedUnixSockets = linuxHostContainerSockets()
	cmd.Path = diagnostic.Executable
	cmd.Args = buildLinuxArgs(req, originalArgv)
	return nil
}

func buildLinuxArgs(req platformRequest, originalArgv []string) []string {
	args := []string{
		"bwrap",
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", req.cwd, req.cwd,
		"--bind", req.tempDir, req.tempDir,
		"--ro-bind", linuxEmptyDir(req), linuxEmptyDir(req),
		"--chdir", req.cwd,
	}
	// Re-apply read-only mounts after the writable cwd bind. This keeps
	// auto-loaded control surfaces immutable even when Metis starts from the
	// user's home directory or a repository root.
	for _, path := range linuxProtectedWritePaths(req) {
		args = append(args, "--ro-bind", path, path)
	}
	// Hide credential files behind /dev/null. The root filesystem is read-only,
	// but read access would still expose keys to a model-authored command.
	for _, path := range linuxProtectedReadFiles(req) {
		args = append(args, "--ro-bind", "/dev/null", path)
	}
	for _, path := range linuxProtectedReadDirectories(req) {
		args = append(args, "--ro-bind", linuxEmptyDir(req), path)
	}
	// A network namespace blocks IP traffic but not path-based AF_UNIX
	// sockets. Mask the common container-engine control sockets: access to one
	// of these is effectively arbitrary host write/exec and would defeat the
	// filesystem sandbox. Abstract Unix sockets still require a future seccomp
	// layer; /sandbox status names that limitation instead of claiming more.
	for _, path := range req.blockedUnixSockets {
		args = append(args, "--ro-bind", "/dev/null", path)
	}
	if req.network == NetworkBlock {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--")
	args = append(args, originalArgv...)
	return args
}

func linuxProtectedWritePaths(req platformRequest) []string {
	return existingPaths(linuxProtectedWriteCandidates(req))
}

func linuxProtectedWriteCandidates(req platformRequest) []string {
	candidates := []string{
		filepath.Join(req.cwd, ".git", "config"),
		filepath.Join(req.cwd, ".git", "hooks"),
		filepath.Join(req.cwd, ".metis", "agents"),
		filepath.Join(req.cwd, ".metis", "commands"),
		filepath.Join(req.cwd, ".metis", "skills"),
		filepath.Join(req.cwd, ".metis", "config.toml"),
		filepath.Join(req.cwd, ".metis", "config.local.toml"),
	}
	candidates = append(candidates, dangerousWriteFiles(req.cwd)...)
	candidates = append(candidates, dangerousWriteDirectories(req.cwd)...)
	candidates = append(candidates, metisControlRoots(req.home, req.metisHome)...)
	if req.home != "" {
		candidates = append(candidates, sensitiveHomeDirectories(req.home)...)
		candidates = append(candidates, sensitiveHomeFiles(req.home)...)
		candidates = append(candidates, dangerousWriteFiles(req.home)...)
		candidates = append(candidates, dangerousWriteDirectories(req.home)...)
	}
	return candidates
}

// validateLinuxProtectedWritePaths fails closed when a path that must remain
// immutable is missing beneath the writable cwd. Bubblewrap creates missing
// bind destinations before mounting them. Because cwd is already a writable
// host bind, trying to mask such a path would create the mount point in the
// host worktree. Silently omitting it instead would let the sandbox create an
// auto-loaded control file or directory.
func validateLinuxProtectedWritePaths(req platformRequest) error {
	gitDir := filepath.Join(req.cwd, ".git")
	gitDirExists := false
	if info, err := os.Stat(gitDir); err == nil {
		gitDirExists = info.IsDir()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: inspect Git metadata directory %q: %w", gitDir, err)
	}

	seen := make(map[string]struct{})
	for _, candidate := range linuxProtectedWriteCandidates(req) {
		candidate = filepath.Clean(candidate)
		if !linuxPathWithin(req.cwd, candidate) {
			continue
		}
		if (candidate == filepath.Join(gitDir, "config") || candidate == filepath.Join(gitDir, "hooks")) && !gitDirExists {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}

		if _, err := os.Stat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf(
					"%w: %q is inside writable cwd %q; bubblewrap cannot protect a missing bind destination without creating it on the host",
					ErrProtectedPathMissing, candidate, req.cwd,
				)
			}
			return fmt.Errorf("sandbox: inspect protected path %q: %w", candidate, err)
		}
	}
	return nil
}

func linuxPathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func linuxProtectedReadFiles(req platformRequest) []string {
	if req.metisHome == "" && req.home == "" {
		return nil
	}
	var files []string
	if req.home != "" {
		files = append(files, sensitiveHomeFiles(req.home)...)
	}
	for _, root := range metisControlRoots(req.home, req.metisHome) {
		files = append(files,
			filepath.Join(root, "auth.json"),
			filepath.Join(root, "mcp-oauth.json"),
			filepath.Join(root, "credentials.json"),
			filepath.Join(root, "secrets.json"),
			filepath.Join(root, "config.toml"))
	}
	return existingRegularFiles(files)
}

func linuxProtectedReadDirectories(req platformRequest) []string {
	dirs := sensitiveHomeDirectories(req.home)
	for _, root := range metisControlRoots(req.home, req.metisHome) {
		dirs = append(dirs, filepath.Join(root, "ide"))
	}
	return existingDirectories(dirs)
}

func linuxEmptyDir(req platformRequest) string {
	return filepath.Join(req.tempDir, ".empty-credentials")
}

func linuxHostContainerSockets() []string {
	candidates := []string{
		"/run/docker.sock",
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
		"/run/containerd/containerd.sock",
		"/var/run/containerd/containerd.sock",
		"/run/crio/crio.sock",
		"/var/run/crio/crio.sock",
		"/run/k3s/containerd/containerd.sock",
	}
	userRuntime := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		userRuntime = xdg
	}
	candidates = append(candidates,
		filepath.Join(userRuntime, "docker.sock"),
		filepath.Join(userRuntime, "podman", "podman.sock"),
		filepath.Join(userRuntime, "containerd", "containerd.sock"))
	return existingUnixSockets(candidates)
}

func existingPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		clean, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		clean = filepath.Clean(clean)
		if _, ok := seen[clean]; ok {
			continue
		}
		if _, err := os.Stat(clean); err != nil {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func existingRegularFiles(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() {
			out = append(out, filepath.Clean(resolved))
		}
	}
	return out
}

func existingDirectories(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}

func existingUnixSockets(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		info, err := os.Stat(resolved)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}
