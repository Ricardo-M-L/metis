//go:build linux

package sandbox

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLinuxStdioMCPDesktopIPCProfiles(t *testing.T) {
	installFakeBubblewrap(t)
	home := t.TempDir()
	xauthority := filepath.Join(t.TempDir(), "Xauthority")
	if err := os.WriteFile(xauthority, []byte("cookie"), 0o600); err != nil {
		t.Fatal(err)
	}
	xdgRuntime := t.TempDir()
	busPath := filepath.Join(t.TempDir(), "session-bus.sock")
	listener, err := net.Listen("unix", busPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("HOME", home)
	t.Setenv("XAUTHORITY", xauthority)
	t.Setenv("XDG_RUNTIME_DIR", xdgRuntime)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+busPath+",guid=test")

	manager, err := NewManagerWithOptions(Options{
		Mode: string(ModeOff), TempRoot: t.TempDir(), MetisHome: filepath.Join(home, ".metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	wrap := func(profile string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("/bin/true")
		cmd.Env = []string{"KEEP=value", linuxSandboxProfileEnvKey + "=" + profile}
		wrapped, wrapErr := manager.Wrap(cmd, Request{Cwd: t.TempDir(), MinimumMode: ModePermissions})
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		if !reflect.DeepEqual(wrapped.Env, []string{"KEEP=value"}) {
			t.Fatalf("profile %q leaked internal marker: %v", profile, wrapped.Env)
		}
		return wrapped
	}

	generic := wrap(linuxStdioMCPSandboxProfile)
	for _, sequence := range [][]string{
		{"--ro-bind", "/dev/null", xauthority},
		{"--ro-bind", linuxEmptyDir(platformRequest{tempDir: manager.TempDir()}), xdgRuntime},
		{"--ro-bind", "/dev/null", busPath},
	} {
		if !containsArgSequence(generic.Args, sequence) {
			t.Errorf("generic stdio profile missing desktop IPC mask %v: %v", sequence, generic.Args)
		}
	}
	if !containsString(linuxDesktopSessionDirectoryCandidates(), "/tmp/.X11-unix") {
		t.Fatal("generic desktop masks no longer include /tmp/.X11-unix")
	}

	desktop := wrap(linuxStdioMCPDesktopSandboxProfile)
	for _, forbidden := range []string{xauthority, xdgRuntime, busPath} {
		if containsMountDestination(desktop.Args, forbidden) {
			t.Errorf("Computer Use profile masked required desktop path %q: %v", forbidden, desktop.Args)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsMountDestination(args []string, destination string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+2] == destination {
			return true
		}
	}
	return false
}

func TestLinuxBubblewrapArgv(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sh", "-c", "printf ok")
	original := append([]string(nil), cmd.Args...)
	req := platformRequest{
		mode: ModePermissions, cwd: "/work/repo", tempDir: "/tmp/metis-sandbox-one", network: NetworkBlock,
		blockedUnixSockets: []string{"/run/docker.sock"},
	}
	// Test argv construction without depending on bwrap being installed in the
	// cross-compile environment.
	args := buildLinuxArgs(req, original)
	want := []string{
		"bwrap", "--die-with-parent", "--new-session", "--unshare-pid",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev",
		"--bind", "/work/repo", "/work/repo",
		"--bind", "/tmp/metis-sandbox-one", "/tmp/metis-sandbox-one",
		"--ro-bind", "/tmp/metis-sandbox-one/.empty-credentials", "/tmp/metis-sandbox-one/.empty-credentials",
		"--chdir", "/work/repo", "--ro-bind", "/dev/null", "/run/docker.sock", "--unshare-net", "--",
		"/bin/sh", "-c", "printf ok",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("bubblewrap argv:\n got %v\nwant %v", args, want)
	}
}

func TestLinuxBubblewrapReprotectsControlFiles(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	paths := []string{
		filepath.Join(cwd, ".git", "config"),
		filepath.Join(cwd, ".git", "hooks", "pre-commit"),
		filepath.Join(home, ".metis", "auth.json"),
		filepath.Join(home, ".metis", "mcp.toml"),
		filepath.Join(home, ".metis", "config.local.toml"),
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".aws", "credentials"),
		filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"),
		filepath.Join(home, ".netrc"),
		filepath.Join(cwd, ".gitmodules"),
		filepath.Join(cwd, ".mcp.json"),
		filepath.Join(cwd, ".vscode", "settings.json"),
		filepath.Join(home, ".zshrc"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := platformRequest{cwd: cwd, tempDir: t.TempDir(), home: home, metisHome: filepath.Join(home, ".metis")}
	args := buildLinuxArgs(req, []string{"/bin/true"})
	for _, sequence := range [][]string{
		{"--ro-bind", filepath.Join(cwd, ".git", "config"), filepath.Join(cwd, ".git", "config")},
		{"--ro-bind", filepath.Join(cwd, ".git", "hooks"), filepath.Join(cwd, ".git", "hooks")},
		{"--ro-bind", filepath.Join(home, ".metis"), filepath.Join(home, ".metis")},
		{"--ro-bind", "/dev/null", filepath.Join(home, ".metis", "auth.json")},
		{"--ro-bind", "/dev/null", filepath.Join(home, ".metis", "mcp.toml")},
		{"--ro-bind", "/dev/null", filepath.Join(home, ".metis", "config.local.toml")},
		{"--ro-bind", "/dev/null", filepath.Join(home, ".netrc")},
		{"--ro-bind", filepath.Join(req.tempDir, ".empty-credentials"), filepath.Join(home, ".ssh")},
		{"--ro-bind", filepath.Join(req.tempDir, ".empty-credentials"), filepath.Join(home, ".aws")},
		{"--ro-bind", filepath.Join(req.tempDir, ".empty-credentials"), filepath.Join(home, ".config", "gcloud")},
		{"--ro-bind", filepath.Join(cwd, ".gitmodules"), filepath.Join(cwd, ".gitmodules")},
		{"--ro-bind", filepath.Join(cwd, ".mcp.json"), filepath.Join(cwd, ".mcp.json")},
		{"--ro-bind", filepath.Join(cwd, ".vscode"), filepath.Join(cwd, ".vscode")},
		{"--ro-bind", filepath.Join(home, ".zshrc"), filepath.Join(home, ".zshrc")},
	} {
		if !containsArgSequence(args, sequence) {
			t.Fatalf("bubblewrap argv missing %v: %v", sequence, args)
		}
	}
}

func TestLinuxStdioMCPProfileMasksMetisRootAndRestoresOnlyNestedCwd(t *testing.T) {
	installFakeBubblewrap(t)
	metisHome := filepath.Join(t.TempDir(), "custom-metis")
	pluginDir := filepath.Join(metisHome, "plugins", "fixture")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: metisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	cmd := exec.Command("/bin/true")
	cmd.Env = []string{
		"KEEP=value",
		linuxSandboxProfileEnvKey + "=" + linuxStdioMCPSandboxProfile,
	}
	wrapper, err := manager.Wrap(cmd, Request{Cwd: pluginDir, MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wrapper.Env, []string{"KEEP=value"}) {
		t.Fatalf("wrapped env = %v, want internal profile consumed", wrapper.Env)
	}

	maskIndex := -1
	restoreIndex := -1
	for i := 0; i+2 < len(wrapper.Args); i++ {
		if wrapper.Args[i] != "--ro-bind" {
			continue
		}
		if wrapper.Args[i+2] == metisHome && strings.HasPrefix(wrapper.Args[i+1], manager.TempDir()+string(filepath.Separator)+".stdio-mcp-metis-") {
			maskIndex = i
		}
		if wrapper.Args[i+1] == pluginDir && wrapper.Args[i+2] == pluginDir {
			restoreIndex = i
		}
	}
	if maskIndex < 0 {
		t.Fatalf("wrapped argv has no directory-level Metis mask: %v", wrapper.Args)
	}
	if restoreIndex <= maskIndex {
		t.Fatalf("plugin cwd was not rebound after Metis root mask: mask=%d restore=%d argv=%v", maskIndex, restoreIndex, wrapper.Args)
	}
	for _, name := range []string{"auth.json", "mcp-oauth.json"} {
		if _, err := os.Lstat(filepath.Join(metisHome, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sandbox materialized credential leaf %s: %v", name, err)
		}
	}
}

func TestLinuxStdioMCPProfileNeverRestoresWholeMetisRoot(t *testing.T) {
	installFakeBubblewrap(t)
	metisHome := filepath.Join(t.TempDir(), "custom-metis")
	if err := os.MkdirAll(metisHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: metisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	cmd := exec.Command("/bin/true")
	cmd.Env = []string{linuxSandboxProfileEnvKey + "=" + linuxStdioMCPSandboxProfile}
	wrapper, err := manager.Wrap(cmd, Request{Cwd: metisHome, MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}

	maskIndex := -1
	for i := 0; i+2 < len(wrapper.Args); i++ {
		if wrapper.Args[i] == "--ro-bind" && wrapper.Args[i+2] == metisHome && strings.HasPrefix(wrapper.Args[i+1], manager.TempDir()+string(filepath.Separator)+".stdio-mcp-metis-") {
			maskIndex = i
			break
		}
	}
	if maskIndex < 0 {
		t.Fatalf("wrapped argv has no directory-level Metis mask: %v", wrapper.Args)
	}
	for i := maskIndex + 1; i+2 < len(wrapper.Args); i++ {
		if wrapper.Args[i] == "--ro-bind" && wrapper.Args[i+1] == metisHome && wrapper.Args[i+2] == metisHome {
			t.Fatalf("cwd equal to Metis root was rebound after the mask: %v", wrapper.Args)
		}
	}
}

func TestLinuxStdioMCPProfileRemasksNestedRootAfterCwdRestore(t *testing.T) {
	installFakeBubblewrap(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultMetisHome := filepath.Join(home, ".metis")
	pluginDir := filepath.Join(defaultMetisHome, "plugins", "fixture")
	customMetisHome := filepath.Join(pluginDir, "state")
	if err := os.MkdirAll(customMetisHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: customMetisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	cmd := exec.Command("/bin/true")
	cmd.Env = []string{linuxSandboxProfileEnvKey + "=" + linuxStdioMCPSandboxProfile}
	wrapper, err := manager.Wrap(cmd, Request{Cwd: pluginDir, MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}

	restoreIndex := -1
	lastNestedMaskIndex := -1
	for i := 0; i+2 < len(wrapper.Args); i++ {
		if wrapper.Args[i] != "--ro-bind" {
			continue
		}
		if wrapper.Args[i+1] == pluginDir && wrapper.Args[i+2] == pluginDir {
			restoreIndex = i
		}
		if strings.HasPrefix(wrapper.Args[i+1], manager.TempDir()+string(filepath.Separator)+".stdio-mcp-metis-") && wrapper.Args[i+2] == customMetisHome {
			lastNestedMaskIndex = i
		}
	}
	if restoreIndex < 0 {
		t.Fatalf("plugin cwd was not restored: %v", wrapper.Args)
	}
	if lastNestedMaskIndex <= restoreIndex {
		t.Fatalf("nested Metis root was not re-masked after cwd restore: restore=%d mask=%d argv=%v", restoreIndex, lastNestedMaskIndex, wrapper.Args)
	}
}

func TestLinuxStdioMCPProfileEndsMaskedWhenCwdEqualsNestedRoot(t *testing.T) {
	installFakeBubblewrap(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultMetisHome := filepath.Join(home, ".metis")
	customMetisHome := filepath.Join(defaultMetisHome, "profile")
	if err := os.MkdirAll(customMetisHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: customMetisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	cmd := exec.Command("/bin/true")
	cmd.Env = []string{linuxSandboxProfileEnvKey + "=" + linuxStdioMCPSandboxProfile}
	wrapper, err := manager.Wrap(cmd, Request{Cwd: customMetisHome, MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}

	lastSource := ""
	for i := 0; i+2 < len(wrapper.Args); i++ {
		if wrapper.Args[i] == "--ro-bind" && wrapper.Args[i+2] == customMetisHome {
			lastSource = wrapper.Args[i+1]
		}
	}
	if !strings.HasPrefix(lastSource, manager.TempDir()+string(filepath.Separator)+".stdio-mcp-metis-") {
		t.Fatalf("last mount on cwd/control root comes from %q, want private Metis view; argv=%v", lastSource, wrapper.Args)
	}
}

func TestLinuxStdioMCPProfileMaterializesAndMasksMissingControlRoots(t *testing.T) {
	installFakeBubblewrap(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultMetisHome := filepath.Join(home, ".metis")
	metisHome := filepath.Join(t.TempDir(), "future-metis-home")
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: metisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	for _, root := range []string{metisHome, defaultMetisHome} {
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("precondition: Metis root %q exists before MCP launch: %v", root, err)
		}
	}

	cmd := exec.Command("/bin/true")
	cmd.Env = []string{linuxSandboxProfileEnvKey + "=" + linuxStdioMCPSandboxProfile}
	wrapper, err := manager.Wrap(cmd, Request{Cwd: t.TempDir(), MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{metisHome, defaultMetisHome} {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			t.Fatalf("Metis root %q was not materialized as a directory: mode=%v error=%v", root, info, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("materialized Metis root %q permissions = %04o, want 0700", root, got)
		}

		masked := false
		for i := 0; i+2 < len(wrapper.Args); i++ {
			if wrapper.Args[i] == "--ro-bind" && wrapper.Args[i+2] == root && strings.HasPrefix(wrapper.Args[i+1], manager.TempDir()+string(filepath.Separator)+".stdio-mcp-metis-") {
				masked = true
				break
			}
		}
		if !masked {
			t.Fatalf("new Metis root %q was not covered by a directory-level mask: %v", root, wrapper.Args)
		}
	}
}

func TestLinuxStdioMCPProfileScaffoldsNestedRootDestinations(t *testing.T) {
	installFakeBubblewrap(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	cmd := exec.Command("/bin/true")
	cmd.Env = []string{linuxSandboxProfileEnvKey + "=" + linuxStdioMCPSandboxProfile}
	wrapper, err := manager.Wrap(cmd, Request{Cwd: t.TempDir(), MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}

	viewDir := ""
	for i := 0; i+2 < len(wrapper.Args); i++ {
		if wrapper.Args[i] == "--ro-bind" && wrapper.Args[i+2] == home && strings.HasPrefix(wrapper.Args[i+1], manager.TempDir()+string(filepath.Separator)+".stdio-mcp-metis-") {
			viewDir = wrapper.Args[i+1]
			break
		}
	}
	if viewDir == "" {
		t.Fatalf("custom root has no private view mount: %v", wrapper.Args)
	}
	info, err := os.Stat(filepath.Join(viewDir, ".metis"))
	if err != nil || !info.IsDir() {
		t.Fatalf("nested default-root destination is not scaffolded in private view: mode=%v error=%v", info, err)
	}
}

func containsArgSequence(args, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestLinuxMissingBubblewrapFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	diagnostic := Doctor()
	if diagnostic.Available || !errors.Is(diagnostic.Err, ErrDependencyMissing) {
		t.Fatalf("Doctor() = %+v, want missing dependency", diagnostic)
	}
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	_, err = m.Wrap(exec.Command("/bin/true"), Request{Cwd: t.TempDir()})
	if !errors.Is(err, ErrDependencyMissing) {
		t.Fatalf("Wrap error = %v, want ErrDependencyMissing", err)
	}
}

func TestLinuxWrapKeepsPersistentCwdWritableForMissingProtectedPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(string) string
	}{
		{name: "mcp config", target: func(cwd string) string { return filepath.Join(cwd, ".mcp.json") }},
		{name: "project commands", target: func(cwd string) string { return filepath.Join(cwd, ".metis", "commands") }},
		{name: "git hooks", target: func(cwd string) string { return filepath.Join(cwd, ".git", "hooks") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeBubblewrap(t)
			cwd := t.TempDir()
			target := tc.target(cwd)
			req := platformRequest{cwd: cwd}
			materializeLinuxProtectedWriteCandidates(t, req, target)

			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("precondition: target %q must be absent, stat error = %v", target, err)
			}

			manager, err := NewManagerWithOptions(Options{
				Mode:      string(ModePermissions),
				TempRoot:  t.TempDir(),
				MetisHome: filepath.Join(t.TempDir(), "global-metis"),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })

			cmd := exec.Command("/bin/true")
			wrapped, err := manager.Wrap(cmd, Request{Cwd: cwd})
			if err != nil {
				t.Fatalf("Wrap failed instead of safely degrading persistent cwd to read-only: %v", err)
			}
			if wrapped == nil {
				t.Fatal("Wrap returned a nil command")
			}
			if !containsArgSequence(wrapped.Args, []string{"--bind", cwd, cwd}) {
				t.Fatalf("wrapped argv does not keep ordinary persistent cwd writable: %v", wrapped.Args)
			}
			if containsArgSequence(wrapped.Args, []string{"--ro-bind", cwd, cwd}) {
				t.Fatalf("missing future control path made the entire cwd read-only: %v", wrapped.Args)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Wrap created protected host path %q, stat error = %v", target, err)
			}
		})
	}
}

func TestLinuxCredentialIsolationMakesMissingProtectedCwdReadOnly(t *testing.T) {
	installFakeBubblewrap(t)
	cwd := t.TempDir()
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.RequireCredentialIsolation(true); err != nil {
		t.Fatal(err)
	}

	wrapper, err := manager.Wrap(exec.Command("/bin/true"), Request{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgSequence(wrapper.Args, []string{"--ro-bind", cwd, cwd}) {
		t.Fatalf("missing protected path did not fail closed to a read-only cwd: %v", wrapper.Args)
	}
	if _, err := os.Lstat(filepath.Join(cwd, ".mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Wrap materialized a protected host path: %v", err)
	}
}

func TestLinuxWrapKeepsManagerOwnedEphemeralCwdWritableWhenProtectedPathsAreMissing(t *testing.T) {
	installFakeBubblewrap(t)
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	cwd, err := os.MkdirTemp(manager.TempDir(), "runcode-")
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(cwd, ".mcp.json")
	wrapped, err := manager.Wrap(exec.Command("/bin/true"), Request{Cwd: cwd})
	if err != nil {
		t.Fatalf("Wrap rejected manager-owned ephemeral cwd: %v", err)
	}
	if !containsArgSequence(wrapped.Args, []string{"--bind", cwd, cwd}) {
		t.Fatalf("wrapped argv does not keep manager-owned cwd writable: %v", wrapped.Args)
	}
	if containsArgSequence(wrapped.Args, []string{"--ro-bind", cwd, cwd}) {
		t.Fatalf("wrapped argv unexpectedly makes manager-owned cwd read-only: %v", wrapped.Args)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Wrap created protected path in manager temp cwd %q: %v", missing, err)
	}
}

func TestLinuxManagerTempSymlinkResolvesToWritablePersistentTarget(t *testing.T) {
	installFakeBubblewrap(t)
	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	persistent := t.TempDir()
	alias := filepath.Join(manager.TempDir(), "workspace-link")
	if err := os.Symlink(persistent, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	wrapper, err := manager.Wrap(exec.Command("/bin/true"), Request{Cwd: alias})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgSequence(wrapper.Args, []string{"--bind", persistent, persistent}) {
		t.Fatalf("ordinary persistent target did not remain writable: %v", wrapper.Args)
	}
}

func TestLinuxProtectedSymlinkMakesPersistentCwdReadOnly(t *testing.T) {
	installFakeBubblewrap(t)
	cwd := t.TempDir()
	protected := filepath.Join(cwd, ".mcp.json")
	materializeLinuxProtectedWriteCandidates(t, platformRequest{cwd: cwd}, protected)
	target := filepath.Join(t.TempDir(), "outside-mcp.json")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, protected); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	wrapper, err := manager.Wrap(exec.Command("/bin/true"), Request{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgSequence(wrapper.Args, []string{"--ro-bind", cwd, cwd}) {
		t.Fatalf("protected symlink did not make persistent cwd read-only: %v", wrapper.Args)
	}
}

func TestLinuxProtectedIntermediateSymlinkMakesPersistentCwdReadOnly(t *testing.T) {
	installFakeBubblewrap(t)
	cwd := t.TempDir()
	metisTarget := t.TempDir()
	if err := os.Symlink(metisTarget, filepath.Join(cwd, ".metis")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	wrapper, err := manager.Wrap(exec.Command("/bin/true"), Request{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgSequence(wrapper.Args, []string{"--ro-bind", cwd, cwd}) {
		t.Fatalf("protected intermediate symlink did not make persistent cwd read-only: %v", wrapper.Args)
	}
}

func TestLinuxWrapKeepsCwdWritableWhenProtectedPathsExist(t *testing.T) {
	installFakeBubblewrap(t)
	cwd := t.TempDir()
	materializeLinuxProtectedWriteCandidates(t, platformRequest{cwd: cwd}, "")

	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	wrapped, err := manager.Wrap(exec.Command("/bin/true"), Request{Cwd: cwd})
	if err != nil {
		t.Fatalf("Wrap failed with complete protected-path scaffold: %v", err)
	}
	if !containsArgSequence(wrapped.Args, []string{"--bind", cwd, cwd}) {
		t.Fatalf("wrapped argv no longer grants cwd writes: %v", wrapped.Args)
	}
}

func TestLinuxMissingProtectedPathSandboxE2E(t *testing.T) {
	diagnostic := Doctor()
	if !diagnostic.Available {
		t.Skipf("bubblewrap unavailable: %v", diagnostic.Err)
	}
	probe := exec.Command(diagnostic.Executable,
		"--die-with-parent", "--ro-bind", "/", "/", "--", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create an unprivileged sandbox here: %v", err)
	}

	manager, err := NewManagerWithOptions(Options{
		Mode:      string(ModePermissions),
		TempRoot:  t.TempDir(),
		MetisHome: filepath.Join(t.TempDir(), "global-metis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	persistent := t.TempDir()
	if err := os.WriteFile(filepath.Join(persistent, "notes.txt"), []byte("readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCmd, err := manager.Wrap(
		exec.Command("/bin/sh", "-c", `test "$(cat notes.txt)" = readable && touch normal.txt`),
		Request{Cwd: persistent},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCmd.Run(); err != nil {
		t.Fatalf("ordinary persistent cwd was not writable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(persistent, "normal.txt")); err != nil {
		t.Fatalf("normal file was not created in ordinary repo: %v", err)
	}

	protected := filepath.Join(persistent, ".metis", "config.toml")
	if err := os.MkdirAll(filepath.Dir(protected), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectedCmd, err := manager.Wrap(
		exec.Command("/bin/sh", "-c", "printf changed > .metis/config.toml"),
		Request{Cwd: persistent},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := protectedCmd.Run(); err == nil {
		t.Fatal("sandbox allowed modification of an existing protected control file")
	}
	if got, err := os.ReadFile(protected); err != nil || string(got) != "original" {
		t.Fatalf("protected file changed to %q, error=%v", got, err)
	}

	symlinked := t.TempDir()
	symlinkPath := filepath.Join(symlinked, ".mcp.json")
	materializeLinuxProtectedWriteCandidates(t, platformRequest{cwd: symlinked}, symlinkPath)
	symlinkTarget := filepath.Join(t.TempDir(), "outside-mcp.json")
	if err := os.WriteFile(symlinkTarget, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, symlinkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	symlinkCmd, err := manager.Wrap(
		exec.Command("/bin/sh", "-c", "rm .mcp.json && printf malicious > .mcp.json"),
		Request{Cwd: symlinked},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := symlinkCmd.Run(); err == nil {
		t.Fatal("sandbox allowed replacement of a protected symlink")
	}
	info, err := os.Lstat(symlinkPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("protected symlink was replaced: mode=%v error=%v", info, err)
	}

	ephemeral, err := os.MkdirTemp(manager.TempDir(), "runcode-")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(ephemeral, "scratch.txt")
	ephemeralCmd, err := manager.Wrap(
		exec.Command("/bin/sh", "-c", "printf ok > scratch.txt"),
		Request{Cwd: ephemeral},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ephemeralCmd.Run(); err != nil {
		t.Fatalf("manager-owned ephemeral cwd was not writable: %v", err)
	}
	if got, err := os.ReadFile(scratch); err != nil || string(got) != "ok" {
		t.Fatalf("ephemeral write = %q, %v; want ok", got, err)
	}
}

func installFakeBubblewrap(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	path := filepath.Join(binDir, linuxSandboxExecutable)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
}

func materializeLinuxProtectedWriteCandidates(t *testing.T, req platformRequest, skip string) {
	t.Helper()
	directories := map[string]bool{
		filepath.Join(req.cwd, ".git", "hooks"):      true,
		filepath.Join(req.cwd, ".metis", "agents"):   true,
		filepath.Join(req.cwd, ".metis", "commands"): true,
		filepath.Join(req.cwd, ".metis", "skills"):   true,
		filepath.Join(req.cwd, ".vscode"):            true,
		filepath.Join(req.cwd, ".idea"):              true,
	}
	for _, candidate := range linuxProtectedWriteCandidates(req) {
		candidate = filepath.Clean(candidate)
		if !linuxPathWithin(req.cwd, candidate) || candidate == filepath.Clean(skip) {
			continue
		}
		if directories[candidate] {
			if err := os.MkdirAll(candidate, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(candidate, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
