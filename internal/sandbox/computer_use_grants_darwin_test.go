//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func computerUseAppGrantsFixtureCommand(t *testing.T, fixtureHome string, enabled bool, homeCwd ...bool) (*exec.Cmd, error) {
	t.Helper()
	canonical := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinComputerUseAppGrantsProbeHelper$")
	cmd.Env = []string{"METIS_SANDBOX_CU_GRANTS_FIXTURE=" + fixtureHome}
	cwd := canonical(t.TempDir())
	if len(homeCwd) != 0 && homeCwd[0] {
		cwd = fixtureHome
		cmd.Env = append(cmd.Env, "METIS_SANDBOX_CU_GRANTS_HOME_CWD=1")
	}
	err := wrapPlatform(cmd, platformRequest{
		mode: ModePermissions, cwd: cwd, tempDir: canonical(t.TempDir()),
		home: fixtureHome, metisHome: filepath.Join(fixtureHome, ".metis"),
		computerUseAppGrants: enabled, network: NetworkBlock,
	})
	return cmd, err
}

func TestDarwinComputerUseAppGrantsFixtureKernelPolicy(t *testing.T) {
	if !Available() {
		t.Skip("sandbox-exec unavailable")
	}
	fixtureHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// All content is synthetic. Never read real app grants or credentials.
	if err := os.Mkdir(filepath.Join(fixtureHome, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureHome, ".metis", "auth.json"), []byte("fixture credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ enabled, homeCwd bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		cmd, err := computerUseAppGrantsFixtureCommand(t, fixtureHome, tc.enabled, tc.homeCwd)
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.CombinedOutput()
		if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
			t.Skip("host does not allow nested Seatbelt profiles")
		}
		if tc.enabled && err != nil {
			t.Fatalf("managed app-grant persistence failed: %v\n%s", err, output)
		}
		if !tc.enabled && (err == nil || !strings.Contains(string(output), "mkdir granted dir")) {
			t.Fatalf("generic profile did not deny creating app-grant state: %v\n%s", err, output)
		}
	}
}

func TestDarwinComputerUseAppGrantsRejectsUnsafeState(t *testing.T) {
	for _, state := range []string{"directory symlink", "granted symlink", "temporary symlink", "dangling symlink", "granted hardlink", "directory file", "granted directory"} {
		t.Run(state, func(t *testing.T) {
			fixtureHome, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(fixtureHome, ".metis-cu")
			path := filepath.Join(dir, "granted.json")
			target := filepath.Join(t.TempDir(), "fixture-target")
			if err := os.WriteFile(target, []byte("unchanged fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if state != "directory symlink" && state != "directory file" {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "directory symlink":
				err = os.Symlink(filepath.Dir(target), dir)
			case "granted symlink":
				err = os.Symlink(target, path)
			case "temporary symlink":
				err = os.Symlink(target, path+".tmp")
			case "dangling symlink":
				err = os.Symlink(target+".missing", path)
			case "granted hardlink":
				err = os.Link(target, path)
			case "directory file":
				err = os.WriteFile(dir, nil, 0o600)
			case "granted directory":
				err = os.Mkdir(path, 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			profile, err := computerUseDarwinAppGrantsProfile(fixtureHome, true)
			if err == nil || profile != "" || !strings.Contains(err.Error(), "unsafe Computer Use app grants path") {
				t.Fatalf("unsafe state was accepted: %v, %s", err, profile)
			}
			if actual, err := os.ReadFile(target); err != nil || string(actual) != "unchanged fixture" {
				t.Fatalf("validation modified fixture target: %q, %v", actual, err)
			}
		})
	}
}

func TestDarwinComputerUseAppGrantsLateSymlinkKernelPolicy(t *testing.T) {
	if !Available() {
		t.Skip("sandbox-exec unavailable")
	}
	for _, name := range []string{".metis-cu", ".metis-cu/granted.json", ".metis-cu/granted.json.tmp"} {
		t.Run(name, func(t *testing.T) {
			fixtureHome, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if name != ".metis-cu" {
				if err := os.Mkdir(filepath.Join(fixtureHome, ".metis-cu"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd, err := computerUseAppGrantsFixtureCommand(t, fixtureHome, true)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "fixture-target")
			if err := os.WriteFile(target, []byte("unchanged fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			linkTarget := target
			if name == ".metis-cu" {
				linkTarget = filepath.Dir(target)
			}
			if err := os.Symlink(linkTarget, filepath.Join(fixtureHome, name)); err != nil {
				t.Fatal(err)
			}
			output, runErr := cmd.CombinedOutput()
			if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
				t.Skip("host does not allow nested Seatbelt profiles")
			}
			if runErr == nil || (!strings.Contains(string(output), "write granted tmp") && !strings.Contains(string(output), "rename granted")) {
				t.Fatalf("late symlink was not denied: %v\n%s", runErr, output)
			}
			if actual, err := os.ReadFile(target); err != nil || string(actual) != "unchanged fixture" {
				t.Fatalf("late symlink modified its target: %q, %v", actual, err)
			}
		})
	}
}

func TestDarwinComputerUseAppGrantsDirectoryMutationKernelPolicy(t *testing.T) {
	if !Available() {
		t.Skip("sandbox-exec unavailable")
	}
	for _, action := range []string{"rename", "remove", "chmod", "times", "xattr"} {
		t.Run(action, func(t *testing.T) {
			fixtureHome, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := computerUseAppGrantsFixtureCommand(t, fixtureHome, true, true)
			if err != nil {
				t.Fatal(err)
			}
			cmd.Env = append(cmd.Env, "METIS_SANDBOX_CU_GRANTS_DIR_ACTION="+action)
			output, runErr := cmd.CombinedOutput()
			if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
				t.Skip("host does not allow nested Seatbelt profiles")
			}
			if runErr != nil {
				t.Fatalf("directory mutation boundary failed: %v\n%s", runErr, output)
			}
			info, err := os.Lstat(filepath.Join(fixtureHome, ".metis-cu"))
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
				t.Fatalf("fixture directory changed: %v, %v", info, err)
			}
		})
	}
}

func TestDarwinComputerUseAppGrantsProbeHelper(t *testing.T) {
	fixtureHome := os.Getenv("METIS_SANDBOX_CU_GRANTS_FIXTURE")
	if fixtureHome == "" {
		return
	}
	// Mirror metis-cu saveGranted with an explicit isolated fixture, without
	// changing HOME, launching the helper, or calling any desktop API.
	path := filepath.Join(fixtureHome, ".metis-cu", "granted.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir granted dir: %v", err)
	}
	if action := os.Getenv("METIS_SANDBOX_CU_GRANTS_DIR_ACTION"); action != "" {
		var err error
		dir := filepath.Dir(path)
		switch action {
		case "rename":
			err = os.Rename(dir, filepath.Join(fixtureHome, "moved-grants"))
		case "remove":
			err = os.Remove(dir)
		case "chmod":
			err = os.Chmod(dir, 0o700)
		case "times":
			err = os.Chtimes(dir, time.Unix(1, 0), time.Unix(1, 0))
		case "xattr":
			err = unix.Setxattr(dir, "com.metis.fixture", []byte("fixture"), 0)
		default:
			t.Fatalf("unexpected fixture action %q", action)
		}
		if err == nil {
			t.Fatalf("app-grant permission allowed directory %s", action)
		}
		return
	}
	for _, data := range []string{`{"Acceptance Fixture":"full"}`, `{}`} {
		if err := os.WriteFile(path+".tmp", []byte(data), 0o644); err != nil {
			t.Fatalf("write granted tmp: %v", err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatalf("rename granted: %v", err)
		}
		if actual, err := os.ReadFile(path); err != nil || string(actual) != data {
			t.Fatalf("read persisted grant: %q, %v", actual, err)
		}
	}
	deniedPaths := []string{
		filepath.Join(fixtureHome, ".metis-cu", "config.toml"),
		filepath.Join(fixtureHome, ".metis-cu", "granted.json.other"),
		filepath.Join(fixtureHome, ".metis", "auth.json"),
	}
	if os.Getenv("METIS_SANDBOX_CU_GRANTS_HOME_CWD") != "1" {
		deniedPaths = append(deniedPaths, filepath.Join(fixtureHome, "unrelated.json"))
	}
	for _, denied := range deniedPaths {
		if err := os.WriteFile(denied, []byte("must not write"), 0o600); err == nil {
			t.Fatalf("app-grant permission allowed unrelated write: %s", denied)
		}
	}
	if _, err := os.ReadFile(filepath.Join(fixtureHome, ".metis", "auth.json")); err == nil {
		t.Fatal("app-grant permission exposed credentials")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixtureHome, "unrelated.json"), path); err == nil {
		t.Fatal("app-grant permission allowed creating a symlink")
	}
}
