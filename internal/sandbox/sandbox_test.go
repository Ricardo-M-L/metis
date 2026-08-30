package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestParseModeStrict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  Mode
		ok    bool
	}{
		{input: "", want: ModeOff, ok: true},
		{input: "off", want: ModeOff, ok: true},
		{input: "permissions", want: ModePermissions, ok: true},
		{input: "auto-allow", want: ModeAutoAllow, ok: true},
		{input: "on"},
		{input: "auto"},
		{input: "OFF"},
		{input: " off "},
		{input: "premissions"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseMode(test.input)
			if test.ok {
				if err != nil || got != test.want {
					t.Fatalf("ParseMode(%q) = (%q, %v), want (%q, nil)", test.input, got, err, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalidMode) {
				t.Fatalf("ParseMode(%q) error = %v, want ErrInvalidMode", test.input, err)
			}
		})
	}
}

func TestManagerRuntimeOverrideAndAutoAllow(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	assertState := func(want State) {
		t.Helper()
		if got := m.State(); got != want {
			t.Fatalf("State() = %+v, want %+v", got, want)
		}
	}
	assertState(State{Configured: ModePermissions, Effective: ModePermissions})

	if err := m.SetRuntimeMode("auto-allow"); err != nil {
		t.Fatal(err)
	}
	assertState(State{
		Configured:         ModePermissions,
		RuntimeOverride:    ModeAutoAllow,
		HasRuntimeOverride: true,
		Effective:          ModeAutoAllow,
		AutoAllow:          true,
	})

	// Empty is ParseMode's explicit off value, not an implicit clear.
	if err := m.SetRuntimeMode(""); err != nil {
		t.Fatal(err)
	}
	assertState(State{
		Configured:         ModePermissions,
		RuntimeOverride:    ModeOff,
		HasRuntimeOverride: true,
		Effective:          ModeOff,
	})

	m.ClearRuntimeMode()
	assertState(State{Configured: ModePermissions, Effective: ModePermissions})
	if err := m.SetConfiguredMode("auto"); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("SetConfiguredMode(auto) error = %v, want ErrInvalidMode", err)
	}
}

func TestCredentialIsolationFloorRestoresUserSandboxSelection(t *testing.T) {
	if !Available() {
		t.Skipf("sandbox unavailable: %v", Doctor().Err)
	}
	m, err := NewManagerWithOptions(Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.RequireCredentialIsolation(true); err != nil {
		t.Fatal(err)
	}
	state := m.State()
	if !state.CredentialIsolationRequired || state.Effective != ModePermissions {
		t.Fatalf("isolation floor state = %+v, want required permissions", state)
	}

	// A user runtime override remains recorded, but cannot lower the bypass
	// credential boundary while the floor is active.
	if err := m.SetRuntimeMode("off"); err != nil {
		t.Fatal(err)
	}
	if got := m.State(); got.RuntimeOverride != ModeOff || got.Effective != ModePermissions {
		t.Fatalf("runtime off bypassed isolation floor: %+v", got)
	}

	if err := m.RequireCredentialIsolation(false); err != nil {
		t.Fatal(err)
	}
	state = m.State()
	if state.CredentialIsolationRequired || state.Effective != ModeOff || !state.HasRuntimeOverride {
		t.Fatalf("removing floor did not restore user override: %+v", state)
	}
}

func TestManagerDefaultNetworkPolicy(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{
		Mode:     "permissions",
		Network:  NetworkBlock,
		TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if got := m.NetworkPolicy(); got != NetworkBlock {
		t.Fatalf("NetworkPolicy() = %q, want %q", got, NetworkBlock)
	}
}

func TestMetisControlRootsIncludesCustomAndDefault(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custom-metis")
	got := metisControlRoots(home, custom)
	want := map[string]bool{
		filepath.Clean(custom):                        false,
		filepath.Join(filepath.Clean(home), ".metis"): false,
	}
	for _, root := range got {
		if _, ok := want[root]; ok {
			want[root] = true
		}
	}
	for root, found := range want {
		if !found {
			t.Errorf("metisControlRoots omitted %q: %v", root, got)
		}
	}
}

func TestManagersDoNotShareRuntimeOverrides(t *testing.T) {
	t.Parallel()
	m1, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		_ = m1.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m1.Close(); _ = m2.Close() })
	if err := m1.SetRuntimeMode("auto-allow"); err != nil {
		t.Fatal(err)
	}
	if got := m2.State(); got.HasRuntimeOverride || got.Effective != ModePermissions {
		t.Fatalf("second manager inherited first override: %+v", got)
	}
}

func TestManagerConcurrentModeAccess(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				switch (worker + i) % 4 {
				case 0:
					_ = m.SetConfiguredMode("permissions")
				case 1:
					_ = m.SetRuntimeMode("auto-allow")
				case 2:
					m.ClearRuntimeMode()
				case 3:
					_ = m.State()
					_ = m.AutoAllow()
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestManagerOwnsPrivateTempDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m1, err := NewManagerWithOptions(Options{TempRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := NewManagerWithOptions(Options{TempRoot: root})
	if err != nil {
		_ = m1.Close()
		t.Fatal(err)
	}
	if m1.TempDir() == m2.TempDir() {
		t.Fatalf("managers shared temp directory %q", m1.TempDir())
	}
	first := m1.TempDir()
	if err := m1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private temp still exists after Close: %v", err)
	}
	if err := m1.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCloseWaitsForActiveProcessLease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m, err := NewManagerWithOptions(Options{TempRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	tempDir := m.TempDir()
	wrapped, release, err := m.Acquire(exec.Command("ignored"), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped == nil || release == nil {
		t.Fatal("Acquire returned an incomplete lease")
	}

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before the active process lease was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := os.Stat(tempDir); err != nil {
		t.Fatalf("manager temp directory disappeared while leased: %v", err)
	}
	if _, secondRelease, err := m.Acquire(exec.Command("ignored"), Request{}); !errors.Is(err, ErrManagerClosed) || secondRelease != nil {
		t.Fatalf("Acquire during Close = (release=%v, err=%v), want nil ErrManagerClosed", secondRelease != nil, err)
	}

	release()
	release() // idempotent: callers may converge on multiple cleanup paths.
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after releasing the process lease")
	}
	if _, err := os.Stat(tempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manager temp directory still exists after lease drain: %v", err)
	}
}

func TestPathWithinRootUsesPathBoundaries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "manager")
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{name: "root", path: root, want: true},
		{name: "descendant", path: filepath.Join(root, "runcode", "work"), want: true},
		{name: "prefix sibling", path: root + "-other", want: false},
		{name: "parent", path: filepath.Dir(root), want: false},
		{name: "empty root", path: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathWithinRoot(root, tc.path); got != tc.want {
				t.Fatalf("pathWithinRoot(%q, %q) = %v, want %v", root, tc.path, got, tc.want)
			}
		})
	}
	if pathWithinRoot("", root) {
		t.Fatal("empty root unexpectedly owns a path")
	}
}

func TestWrapOffPassesThroughEveryCommandField(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	stdin := bytes.NewBufferString("input")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", "echo hi")
	cmd.Args[0] = "custom-argv-zero"
	cmd.Env = []string{"A=1", "B=two"}
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	originalArgs := append([]string(nil), cmd.Args...)
	originalEnv := append([]string(nil), cmd.Env...)
	originalPath := cmd.Path

	got, err := m.Wrap(cmd, Request{Cwd: dir, Network: "not-validated-when-off"})
	if err != nil {
		t.Fatal(err)
	}
	if got != cmd {
		t.Fatal("Wrap returned a different *exec.Cmd")
	}
	if cmd.Path != originalPath || !reflect.DeepEqual(cmd.Args, originalArgs) || !reflect.DeepEqual(cmd.Env, originalEnv) {
		t.Fatalf("off mode changed command: path=%q args=%v env=%v", cmd.Path, cmd.Args, cmd.Env)
	}
	if cmd.Dir != dir || cmd.Stdin != stdin || cmd.Stdout != stdout || cmd.Stderr != stderr {
		t.Fatal("off mode changed dir or stdio")
	}
}

func TestRequestMinimumModeActivatesAvailableSandbox(t *testing.T) {
	if !Available() {
		t.Skipf("sandbox unavailable: %v", Doctor().Err)
	}
	m, err := NewManagerWithOptions(Options{Mode: string(ModeOff), TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	cwd := t.TempDir()
	cmd := exec.Command("/bin/true")
	originalPath := cmd.Path
	wrapped, err := m.Wrap(cmd, Request{Cwd: cwd, MinimumMode: ModePermissions})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Path == originalPath {
		t.Fatalf("minimum permissions request left command unsandboxed: %q", wrapped.Path)
	}
}

func TestRequestRejectsInvalidMinimumMode(t *testing.T) {
	m, err := NewManagerWithOptions(Options{Mode: string(ModeOff), TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	_, err = m.Wrap(exec.Command("ignored"), Request{MinimumMode: Mode("invalid")})
	if !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("invalid minimum mode error = %v, want ErrInvalidMode", err)
	}
}

func TestWrapRejectsFilesystemRootAsWritableCwd(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	_, err = m.Wrap(exec.Command("/bin/true"), Request{Cwd: string(filepath.Separator)})
	if !errors.Is(err, ErrUnsafeCwd) {
		t.Fatalf("Wrap cwd=/ error = %v, want ErrUnsafeCwd", err)
	}
}

func TestClosedManagerFailsClosed(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wrap(exec.Command("/bin/true"), Request{Cwd: t.TempDir()}); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Wrap after Close error = %v, want ErrManagerClosed", err)
	}
}

func TestDoctorAndAvailableAgree(t *testing.T) {
	t.Parallel()
	diagnostic := Doctor()
	if diagnostic.Platform == "" || diagnostic.Backend == "" {
		t.Fatalf("incomplete diagnostic: %+v", diagnostic)
	}
	if Available() != diagnostic.Available {
		t.Fatalf("Available() = %v, Doctor().Available = %v", Available(), diagnostic.Available)
	}
	if diagnostic.Available {
		if !diagnostic.Supported || diagnostic.Executable == "" || diagnostic.Err != nil {
			t.Fatalf("inconsistent available diagnostic: %+v", diagnostic)
		}
	} else if diagnostic.Err == nil {
		t.Fatalf("unavailable diagnostic lacks reason: %+v", diagnostic)
	}
}

func TestWrapRejectsUnknownNetworkPolicyWhenEnabled(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	_, err = m.Wrap(exec.Command("/bin/true"), Request{Cwd: t.TempDir(), Network: "proxy-maybe"})
	if !errors.Is(err, ErrInvalidNetworkPolicy) {
		t.Fatalf("Wrap invalid network error = %v, want ErrInvalidNetworkPolicy", err)
	}
}
