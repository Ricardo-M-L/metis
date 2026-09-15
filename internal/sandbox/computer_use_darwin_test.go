//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func computerUseDarwinWrappedCommand(t *testing.T, enabled bool) *exec.Cmd {
	t.Helper()
	manager, err := NewManagerWithOptions(Options{Mode: "off", TempRoot: t.TempDir(), MetisHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.RequireCredentialIsolation(true); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinComputerUseOwnershipProbeHelper$")
	wrapped, release, err := manager.Acquire(cmd, Request{Cwd: t.TempDir(), ComputerUseInputOwnership: enabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if wrapped.Path != darwinSandboxExecutable || len(wrapped.Args) < 4 {
		t.Fatal("Computer Use lost its sandbox wrapper")
	}
	return wrapped
}

func TestDarwinComputerUseOwnershipPolicyIsExactAndOptIn(t *testing.T) {
	if !Available() {
		t.Skip("sandbox-exec unavailable")
	}
	generic := computerUseDarwinWrappedCommand(t, false).Args[2]
	profile := computerUseDarwinWrappedCommand(t, true).Args[2]
	for _, path := range []string{"/tmp/metis-cu-input-v1.lock", "/private/tmp/metis-cu-input-v1.lock"} {
		literal := `(literal "` + path + `")`
		if !strings.Contains(profile, literal) {
			t.Errorf("Computer Use profile cannot create ownership file: %s", path)
		}
		if strings.Contains(generic, literal) {
			t.Errorf("generic sandbox received Computer Use ownership permission: %s", path)
		}
	}
	for _, broad := range []string{
		`(allow file-write* (subpath "/tmp"))`, `(allow file-write* (subpath "/private/tmp"))`,
		`(allow file-write* (literal "/tmp/metis-cu-input-v1.lock"))`,
		`(allow file-write* (literal "/private/tmp/metis-cu-input-v1.lock"))`,
	} {
		if strings.Contains(profile, broad) {
			t.Errorf("Computer Use profile grants unnecessary writes: %s", broad)
		}
	}
	if !strings.Contains(profile, "(deny default)") || !strings.Contains(profile, `(deny file-read*`) {
		t.Fatal("Computer Use dropped its default-deny or credential boundary")
	}
}

// This probe substitutes a fixture path into the exact production rule. It
// never opens the real machine-wide input lease or starts a native MCP helper.
func TestDarwinComputerUseOwnershipFixtureKernelPolicy(t *testing.T) {
	if !Available() {
		t.Skip("sandbox-exec unavailable")
	}
	fixtureDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(fixtureDir, "input.lock")
	for _, enabled := range []bool{false, true, true} {
		cmd := computerUseDarwinWrappedCommand(t, enabled)
		profile := cmd.Args[2]
		profile = strings.ReplaceAll(profile, "/private/tmp/metis-cu-input-v1.lock", fixture)
		profile = strings.ReplaceAll(profile, "/tmp/metis-cu-input-v1.lock", fixture)
		cmd.Args[2] = profile
		cmd.Env = []string{"METIS_SANDBOX_CU_PROBE=1", "METIS_SANDBOX_CU_FIXTURE=" + fixture}
		output, err := cmd.CombinedOutput()
		if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
			t.Skip("host does not allow nested Seatbelt profiles")
		}
		if enabled && err != nil {
			t.Fatalf("Computer Use fixture create/mode/lock failed: %v\n%s", err, output)
		}
		if !enabled && (err == nil || !strings.Contains(string(output), "fixture open")) {
			t.Fatalf("generic profile did not deny the fixture open: %v\n%s", err, output)
		}
	}
}

func TestDarwinComputerUseOwnershipProbeHelper(t *testing.T) {
	if os.Getenv("METIS_SANDBOX_CU_PROBE") != "1" {
		return
	}
	fixture := os.Getenv("METIS_SANDBOX_CU_FIXTURE")
	if filepath.Base(fixture) != "input.lock" || strings.Contains(fixture, "metis-cu-input-v1.lock") {
		t.Fatal("probe requires a private fixture; real Computer Use lease is forbidden")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Open(fixture, flags|unix.O_CREAT|unix.O_EXCL, 0o644)
	if err == nil {
		if err := unix.Fchmod(fd, 0o644); err != nil {
			_ = unix.Close(fd)
			t.Fatalf("fixture chmod: %v", err)
		}
	} else if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(fixture, flags, 0)
	}
	if err != nil {
		t.Fatalf("fixture open: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("fixture flock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(fixture), "unrelated.lock"), nil, 0o600); err == nil {
		t.Fatal("ownership grant permitted an unrelated lock")
	}
	if err := os.Remove(fixture); err == nil {
		t.Fatal("ownership grant permitted unlinking the shared inode")
	}
}
