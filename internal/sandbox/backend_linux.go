//go:build linux

package sandbox

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	linuxSandboxExecutable             = "bwrap"
	linuxSandboxProfileEnvKey          = "METIS_INTERNAL_SANDBOX_PROFILE"
	linuxStdioMCPSandboxProfile        = "stdio-mcp"
	linuxStdioMCPDesktopSandboxProfile = "stdio-mcp-desktop"
)

type linuxMetisView struct {
	dir                string
	roots              []string
	restoreCwd         bool
	maskDesktopSession bool
}

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
	profile := consumeLinuxSandboxProfile(&cmd.Env)
	stdioMCP := profile == linuxStdioMCPSandboxProfile || profile == linuxStdioMCPDesktopSandboxProfile
	cwdReadOnly, err := linuxCwdNeedsReadOnlyFallback(req)
	if err != nil {
		return err
	}
	req.cwdReadOnly = cwdReadOnly
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
	if err := ensureLinuxCredentialMaskTargets(req); err != nil {
		return err
	}
	var metisView linuxMetisView
	if stdioMCP || req.credentialIsolationRequired {
		metisView, err = prepareLinuxMetisView(req)
		if err != nil {
			return err
		}
		// Only generic stdio MCP processes lose desktop/session IPC. Reusing the
		// Metis-root view for bypassPermissions must not silently change the
		// process's unrelated desktop access policy.
		metisView.maskDesktopSession = profile == linuxStdioMCPSandboxProfile
	}
	// Container control sockets are host-escape capabilities, not ordinary
	// network access, so mask them in every enabled sandbox mode (including
	// network=allow).
	req.blockedUnixSockets = linuxHostContainerSockets()
	cmd.Path = diagnostic.Executable
	cmd.Args = buildLinuxArgsWithMetisView(req, originalArgv, metisView)
	return nil
}

func buildLinuxArgs(req platformRequest, originalArgv []string) []string {
	return buildLinuxArgsWithMetisView(req, originalArgv, linuxMetisView{})
}

func buildLinuxArgsWithMetisView(req platformRequest, originalArgv []string, metisView linuxMetisView) []string {
	cwdBind := "--bind"
	if req.cwdReadOnly {
		cwdBind = "--ro-bind"
	}
	args := []string{
		"bwrap",
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		cwdBind, req.cwd, req.cwd,
		"--bind", req.tempDir, req.tempDir,
		"--ro-bind", linuxEmptyDir(req), linuxEmptyDir(req),
		"--chdir", req.cwd,
	}
	if metisView.dir != "" {
		// tempDir is writable to the server, so re-protect the view source
		// before exposing it as a mount. The view itself contains directories
		// only; host Metis contents are never copied into it.
		args = append(args, "--ro-bind", metisView.dir, metisView.dir)
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
	if metisView.dir != "" {
		for _, root := range metisView.roots {
			args = append(args, "--ro-bind", metisView.dir, root)
		}
		if metisView.restoreCwd {
			// Bubblewrap bind sources are host paths. Rebinding after the empty
			// parent mount preserves an installed plugin's declared cwd while
			// leaving sibling Metis state hidden.
			args = append(args, "--ro-bind", req.cwd, req.cwd)
			// A custom Metis root can be nested below the plugin cwd while the
			// default root is its ancestor. The cwd bind would expose that nested
			// root again, so every control root at or below cwd must be masked last.
			for _, root := range metisView.roots {
				if linuxPathWithin(req.cwd, root) {
					args = append(args, "--ro-bind", metisView.dir, root)
				}
			}
		}
	}
	// A network namespace blocks IP traffic but not path-based AF_UNIX
	// sockets. Mask the common container-engine control sockets: access to one
	// of these is effectively arbitrary host write/exec and would defeat the
	// filesystem sandbox. Abstract Unix sockets still require a future seccomp
	// layer; /sandbox status names that limitation instead of claiming more.
	for _, path := range req.blockedUnixSockets {
		args = append(args, "--ro-bind", "/dev/null", path)
	}
	if metisView.dir != "" && metisView.maskDesktopSession {
		// Generic stdio MCPs never receive host desktop/session IPC. Apply these
		// masks after every cwd and Metis-root restore so a plugin installed under
		// HOME cannot re-expose an Xauthority cookie or runtime socket. Directory
		// masks come last: binding an empty XDG runtime directory first would make
		// later per-socket bind destinations disappear and cause bwrap to fail.
		for _, path := range linuxDesktopSessionFiles() {
			args = append(args, "--ro-bind", "/dev/null", path)
		}
		for _, path := range linuxDBusSessionSocketPaths(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) {
			args = append(args, "--ro-bind", "/dev/null", path)
		}
		for _, path := range linuxDesktopSessionDirectories() {
			args = append(args, "--ro-bind", linuxEmptyDir(req), path)
		}
	}
	if req.network == NetworkBlock {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--")
	args = append(args, originalArgv...)
	return args
}

// consumeLinuxSandboxProfile removes the private Metis-to-bubblewrap control
// variable so an MCP server cannot observe or forward an implementation detail.
// Unknown values are consumed as well; only the trusted stdio-mcp value opts in.
func consumeLinuxSandboxProfile(environ *[]string) string {
	if environ == nil || len(*environ) == 0 {
		return ""
	}
	prefix := linuxSandboxProfileEnvKey + "="
	profile := ""
	sawGeneric := false
	out := (*environ)[:0]
	for _, entry := range *environ {
		if strings.HasPrefix(entry, prefix) {
			value := strings.TrimPrefix(entry, prefix)
			switch value {
			case linuxStdioMCPSandboxProfile:
				profile = linuxStdioMCPSandboxProfile
				sawGeneric = true
			case linuxStdioMCPDesktopSandboxProfile:
				if !sawGeneric {
					profile = linuxStdioMCPDesktopSandboxProfile
				}
			}
			continue
		}
		out = append(out, entry)
	}
	*environ = out
	return profile
}

func linuxDesktopSessionFiles() []string {
	candidates := []string{os.Getenv("XAUTHORITY")}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, ".Xauthority"))
	}
	return existingRegularFiles(candidates)
}

func linuxDesktopSessionDirectories() []string {
	return existingDirectories(linuxDesktopSessionDirectoryCandidates())
}

func linuxDesktopSessionDirectoryCandidates() []string {
	candidates := []string{"/tmp/.X11-unix", os.Getenv("XDG_RUNTIME_DIR")}
	candidates = append(candidates, filepath.Join("/run/user", strconv.Itoa(os.Getuid())))
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			continue
		}
		clean := filepath.Clean(candidate)
		if clean == string(filepath.Separator) {
			continue
		}
		filtered = append(filtered, clean)
	}
	return filtered
}

func linuxDBusSessionSocketPaths(address string) []string {
	var candidates []string
	for _, endpoint := range strings.Split(address, ";") {
		if !strings.HasPrefix(endpoint, "unix:") {
			continue
		}
		for _, field := range strings.Split(strings.TrimPrefix(endpoint, "unix:"), ",") {
			key, value, ok := strings.Cut(field, "=")
			if !ok || key != "path" {
				continue
			}
			if decoded, err := url.PathUnescape(value); err == nil {
				value = decoded
			}
			if filepath.IsAbs(value) && filepath.Clean(value) != string(filepath.Separator) {
				candidates = append(candidates, value)
			}
		}
	}
	return existingUnixSockets(candidates)
}

// prepareLinuxMetisView constructs a host-private directory tree used as the
// complete view of each Metis control root by stdio MCPs and by the
// bypassPermissions credential-isolation floor. Every candidate root is
// materialized before launch so credentials created after namespace setup,
// including legacy root-level stores and sidecars, remain invisible.
//
// A cwd equal to a control root cannot be restored without exposing that whole
// root, and a cwd inside .credentials cannot be restored at all. Both cases
// fail closed. A safe cwd strictly below a root receives a destination
// scaffold so buildLinuxArgsWithMetisView can restore only that subtree and
// then re-mask any nested control roots.
func prepareLinuxMetisView(req platformRequest) (linuxMetisView, error) {
	if req.metisHome == "" {
		return linuxMetisView{}, fmt.Errorf("sandbox: credential isolation requires a Metis home")
	}
	activeRoot := filepath.Clean(req.metisHome)
	for _, root := range metisControlRoots(req.home, activeRoot) {
		root = filepath.Clean(root)
		if root == string(filepath.Separator) {
			return linuxMetisView{}, fmt.Errorf("%w: refusing to use filesystem root as a Metis home", ErrUnsafeCwd)
		}
		if req.cwd == root {
			return linuxMetisView{}, fmt.Errorf("%w: cwd %q is a Metis credential root", ErrUnsafeCwd, req.cwd)
		}
		privateDir := filepath.Join(root, metisCredentialDirectoryName)
		if linuxPathWithin(privateDir, req.cwd) {
			return linuxMetisView{}, fmt.Errorf("%w: cwd %q is inside private credential directory %q", ErrUnsafeCwd, req.cwd, privateDir)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return linuxMetisView{}, fmt.Errorf("sandbox: create Metis credential boundary %q: %w", root, err)
		}
		info, err := os.Stat(root)
		if err != nil {
			return linuxMetisView{}, fmt.Errorf("sandbox: inspect Metis credential boundary %q: %w", root, err)
		}
		if !info.IsDir() {
			return linuxMetisView{}, fmt.Errorf("sandbox: Metis credential boundary %q is not a directory", root)
		}
	}

	roots := existingDirectories(metisControlRoots(req.home, activeRoot))
	if len(roots) == 0 {
		return linuxMetisView{}, fmt.Errorf("sandbox: no Metis credential root is available to isolate")
	}
	for _, root := range roots {
		if linuxPathWithin(root, req.tempDir) {
			return linuxMetisView{}, fmt.Errorf(
				"sandbox: private temp directory %q must not be inside Metis credential root %q",
				req.tempDir, root,
			)
		}
	}

	viewDir, err := os.MkdirTemp(req.tempDir, ".stdio-mcp-metis-")
	if err != nil {
		return linuxMetisView{}, fmt.Errorf("sandbox: create private Metis view: %w", err)
	}
	view := linuxMetisView{dir: viewDir, roots: roots}
	// Any root may be nested below another (for example custom METIS_HOME=$HOME
	// with the default root at $HOME/.metis). Once the outer root is covered by
	// this read-only view, a later inner mount needs its destination to exist in
	// the view rather than in the now-hidden host tree.
	for _, outer := range roots {
		for _, inner := range roots {
			rel, relErr := filepath.Rel(outer, inner)
			if relErr != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if err := os.MkdirAll(filepath.Join(viewDir, rel), 0o700); err != nil {
				_ = os.RemoveAll(viewDir)
				return linuxMetisView{}, fmt.Errorf("sandbox: scaffold nested Metis credential root: %w", err)
			}
		}
	}
	for _, root := range roots {
		rel, relErr := filepath.Rel(root, req.cwd)
		if relErr != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Join(viewDir, rel), 0o700); err != nil {
			_ = os.RemoveAll(viewDir)
			return linuxMetisView{}, fmt.Errorf("sandbox: scaffold isolated working directory: %w", err)
		}
		view.restoreCwd = true
	}
	return view, nil
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

// linuxCwdNeedsReadOnlyFallback reports whether a protected path is represented
// by a symlink inside a persistent cwd. Bubblewrap's bind destination follows
// symlinks, so re-binding only the resolved target would leave the writable
// directory entry replaceable; the exceptional safe fallback is a read-only
// cwd. Existing ordinary files/directories are protected by their later
// read-only bind mounts.
//
// Missing future protected paths trigger the same fallback only while the
// bypassPermissions credential-isolation floor is active. Bubblewrap cannot
// mask an absent bind destination without materializing it in the host cwd;
// making the cwd read-only is the smallest kernel-enforced fail-closed policy.
// Normal permission-mode repositories remain writable.
func linuxCwdNeedsReadOnlyFallback(req platformRequest) (bool, error) {
	if req.managerOwnedCwd {
		return false, nil
	}
	seen := make(map[string]struct{})
	for _, candidate := range linuxProtectedWriteCandidates(req) {
		candidate = filepath.Clean(candidate)
		if !linuxPathWithin(req.cwd, candidate) {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		symlinked, err := linuxPathContainsSymlink(req.cwd, candidate)
		if err != nil {
			return false, fmt.Errorf("sandbox: inspect protected path %q: %w", candidate, err)
		}
		if symlinked {
			return true, nil
		}
		if _, statErr := os.Lstat(candidate); statErr != nil {
			if os.IsNotExist(statErr) {
				if req.credentialIsolationRequired {
					return true, nil
				}
				continue
			}
			return false, fmt.Errorf("sandbox: inspect protected path %q: %w", candidate, statErr)
		}
	}
	return false, nil
}

// linuxPathContainsSymlink checks each existing component below root without
// following it. It stops at the first absent component: future paths are the
// gate-level residual described above, while dangling and intermediate
// symlinks that already exist still force the read-only fallback.
func linuxPathContainsSymlink(root, candidate string) (bool, error) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	current := filepath.Clean(root)
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return false, nil
			}
			return false, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
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
			filepath.Join(root, "llm-oauth.json"),
			filepath.Join(root, ".llm-oauth.lock"),
			filepath.Join(root, "mcp-oauth.json"),
			filepath.Join(root, ".mcp-oauth.lock"),
			filepath.Join(root, "mcp.toml"),
			filepath.Join(root, "credentials.json"),
			filepath.Join(root, "secrets.json"),
			filepath.Join(root, "config.toml"),
			filepath.Join(root, "config.local.toml"))
		files = append(files, existingCredentialSidecars(root)...)
	}
	return existingRegularFiles(files)
}

func existingCredentialSidecars(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isLegacyCredentialFilename(entry.Name()) {
			continue
		}
		files = append(files, filepath.Join(root, entry.Name()))
	}
	return files
}

func linuxProtectedReadDirectories(req platformRequest) []string {
	dirs := sensitiveHomeDirectories(req.home)
	for _, root := range metisControlRoots(req.home, req.metisHome) {
		dirs = append(dirs,
			filepath.Join(root, metisCredentialDirectoryName),
			filepath.Join(root, "ide"))
	}
	return existingDirectories(dirs)
}

// Bubblewrap can only bind over an existing mountpoint. Pre-create the fixed
// private credential directory before entering the namespace so one directory
// mask also covers final stores, lock files, and random temp files created by
// another METIS process later in the sandboxed command's lifetime.
func ensureLinuxCredentialMaskTargets(req platformRequest) error {
	for _, root := range metisControlRoots(req.home, req.metisHome) {
		dir := filepath.Join(root, metisCredentialDirectoryName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sandbox: create credential isolation directory %q: %w", dir, err)
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("sandbox: inspect credential isolation directory %q: %w", dir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("sandbox: credential isolation path %q is not a real directory", dir)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("sandbox: secure credential isolation directory %q: %w", dir, err)
		}
	}
	return nil
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
