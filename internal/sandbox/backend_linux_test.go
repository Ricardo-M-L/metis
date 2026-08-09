//go:build linux

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

func TestLinuxWrapFailsClosedForMissingProtectedCwdPaths(t *testing.T) {
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
			if wrapped != nil {
				t.Fatalf("Wrap returned command on fail-closed path: %+v", wrapped)
			}
			if !errors.Is(err, ErrProtectedPathMissing) {
				t.Fatalf("Wrap error = %v, want ErrProtectedPathMissing", err)
			}
			if !strings.Contains(err.Error(), target) {
				t.Fatalf("Wrap error %q does not identify missing target %q", err, target)
			}
			if cmd.Path != "/bin/true" {
				t.Fatalf("Wrap mutated command before failing: path = %q", cmd.Path)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Wrap created protected host path %q, stat error = %v", target, err)
			}
		})
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
