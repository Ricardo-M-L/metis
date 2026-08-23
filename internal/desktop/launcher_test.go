package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type commandCall struct {
	name string
	args []string
}

func testLauncher(goos string) (launcher, *[]commandCall, *[]commandCall) {
	var runs, starts []commandCall
	l := launcher{
		goos:    goos,
		goarch:  "arm64",
		home:    "/home/tester",
		cwd:     "/workspace/metis",
		cliPath: "/opt/metis/bin/metis",
		stat: func(string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		},
		runCommand: func(name string, args ...string) error {
			runs = append(runs, commandCall{name: name, args: append([]string(nil), args...)})
			return nil
		},
		startCommand: func(name string, args ...string) error {
			starts = append(starts, commandCall{name: name, args: append([]string(nil), args...)})
			return nil
		},
	}
	return l, &runs, &starts
}

func TestOpenAppBuildsPlatformCommandsWithCLIOverride(t *testing.T) {
	tests := []struct {
		goos      string
		appPath   string
		wantRun   []commandCall
		wantStart []commandCall
	}{
		{
			goos: "darwin", appPath: "/Applications/Metis.app",
			wantRun: []commandCall{{name: "open", args: []string{
				"-n", "-a", "/Applications/Metis.app", "--args",
				"--workspace", "/tmp/project with spaces", "--metis-bin", "/opt/metis/bin/metis",
			}}},
		},
		{
			goos: "linux", appPath: "/usr/local/bin/metis-desktop",
			wantStart: []commandCall{{name: "/usr/local/bin/metis-desktop", args: []string{
				"--workspace", "/tmp/project with spaces", "--metis-bin", "/opt/metis/bin/metis",
			}}},
		},
		{
			goos: "windows", appPath: `C:\Program Files\Metis\Metis Desktop.exe`,
			wantRun: []commandCall{{name: "cmd", args: []string{
				"/c", "start", "", `C:\Program Files\Metis\Metis Desktop.exe`,
				"--workspace", "/tmp/project with spaces", "--metis-bin", "/opt/metis/bin/metis",
			}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			l, runs, starts := testLauncher(tc.goos)
			if err := l.openApp(tc.appPath, "/tmp/project with spaces"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*runs, tc.wantRun) {
				t.Fatalf("run commands = %#v, want %#v", *runs, tc.wantRun)
			}
			if !reflect.DeepEqual(*starts, tc.wantStart) {
				t.Fatalf("start commands = %#v, want %#v", *starts, tc.wantStart)
			}
		})
	}
}

func TestResolveLauncherCLIPathPrefersStableManagedLauncher(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, "bin", "metis")
	resolved := filepath.Join(root, "share", "metis", "versions", "0.4.28", "metis")

	got := resolveLauncherCLIPath(
		func() (string, error) { return stable, nil },
		func() (string, error) { return resolved, nil },
	)
	if got != stable {
		t.Fatalf("resolveLauncherCLIPath() = %q, want stable launcher %q", got, stable)
	}
}

func TestResolveLauncherCLIPathFallsBackToExecutable(t *testing.T) {
	resolved := filepath.Join(t.TempDir(), "metis")
	got := resolveLauncherCLIPath(
		func() (string, error) { return "", errors.New("not managed") },
		func() (string, error) { return resolved, nil },
	)
	if got != resolved {
		t.Fatalf("resolveLauncherCLIPath() = %q, want executable fallback %q", got, resolved)
	}
}

func TestFindExistingAppPathOverrideAndLocalBuild(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		override string
		want     string
		fileMode os.FileMode
	}{
		{name: "mac override", goos: "darwin", override: "/custom/Metis.app", want: "/custom/Metis.app", fileMode: os.ModeDir},
		{name: "linux override", goos: "linux", override: "/custom/metis-desktop", want: "/custom/metis-desktop", fileMode: 0o755},
		{name: "windows override", goos: "windows", override: `C:\custom\metis.exe`, want: `C:\custom\metis.exe`, fileMode: 0o644},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, _, _ := testLauncher(tc.goos)
			l.appOverride = tc.override
			l.stat = statOnly(tc.want, tc.fileMode)
			if got := l.findExistingAppPath(); got != tc.want {
				t.Fatalf("findExistingAppPath() = %q, want %q", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		goos string
		name string
		mode os.FileMode
	}{
		{goos: "darwin", name: "metis-desktop.app", mode: os.ModeDir},
		{goos: "linux", name: "metis-desktop", mode: 0o755},
		{goos: "windows", name: "metis-desktop.exe", mode: 0o644},
	} {
		t.Run(tc.goos+" local build", func(t *testing.T) {
			l, _, _ := testLauncher(tc.goos)
			local := filepath.Join(l.cwd, "metis-desktop", "build", "bin", tc.name)
			l.stat = statOnly(local, tc.mode)
			if got := l.findExistingAppPath(); got != local {
				t.Fatalf("findExistingAppPath() = %q, want local build %q", got, local)
			}
		})
	}
}

func TestIsInstalledRequiresCorrectArtifactType(t *testing.T) {
	for _, tc := range []struct {
		name string
		goos string
		mode os.FileMode
		want bool
	}{
		{name: "mac bundle", goos: "darwin", mode: os.ModeDir, want: true},
		{name: "mac plain file", goos: "darwin", mode: 0o755, want: false},
		{name: "linux executable", goos: "linux", mode: 0o755, want: true},
		{name: "linux non executable", goos: "linux", mode: 0o644, want: false},
		{name: "windows exe", goos: "windows", mode: 0o644, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _, _ := testLauncher(tc.goos)
			l.appOverride = "/override/app"
			l.stat = statOnly(l.appOverride, tc.mode)
			if got := l.isInstalled(); got != tc.want {
				t.Fatalf("isInstalled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLaunchAppReportsMissingUnsupportedAndStartFailure(t *testing.T) {
	l, _, _ := testLauncher("linux")
	if err := l.launchApp("/tmp/project"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing app error = %v", err)
	}

	l.goos = "plan9"
	if err := l.launchApp("/tmp/project"); err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("unsupported platform error = %v", err)
	}

	l, _, _ = testLauncher("linux")
	l.appOverride = "/custom/metis-desktop"
	l.stat = statOnly(l.appOverride, 0o755)
	want := errors.New("start failed")
	l.startCommand = func(string, ...string) error { return want }
	if err := l.launchApp("/tmp/project"); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestDownloadURLUsesExistingReleasePage(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		l, _, _ := testLauncher(goos)
		for _, goarch := range []string{"arm64", "amd64"} {
			l.goarch = goarch
			if got := l.downloadURL(); got != metisReleasesURL {
				t.Fatalf("%s/%s URL = %q, want %q", goos, goarch, got, metisReleasesURL)
			}
		}
	}

	l, _, _ := testLauncher("plan9")
	if got := l.downloadURL(); got != "" {
		t.Fatalf("unsupported URL = %q, want empty", got)
	}
}

func statOnly(path string, mode os.FileMode) func(string) (os.FileInfo, error) {
	return func(got string) (os.FileInfo, error) {
		if got != path {
			return nil, os.ErrNotExist
		}
		return fakeFileInfo{name: filepath.Base(path), mode: mode}, nil
	}
}

type fakeFileInfo struct {
	name string
	mode os.FileMode
}

func (f fakeFileInfo) Name() string      { return f.name }
func (fakeFileInfo) Size() int64         { return 0 }
func (f fakeFileInfo) Mode() os.FileMode { return f.mode }
func (fakeFileInfo) ModTime() time.Time  { return time.Time{} }
func (f fakeFileInfo) IsDir() bool       { return f.mode.IsDir() }
func (fakeFileInfo) Sys() any            { return nil }
