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
