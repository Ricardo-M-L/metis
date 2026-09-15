package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
)

func TestCU_ManagedActions(t *testing.T) {
	for _, test := range []struct{ args, action string }{
		{"", "status"}, {" status ", "status"}, {"install", "install"},
		{"enable", "enable"}, {"on", "enable"}, {"disable", "disable"},
		{"off", "disable"}, {"stop", "stop"},
		{"permissions-accessibility", "permissions-accessibility"},
		{"permissions-screen-recording", "permissions-screen-recording"},
	} {
		t.Run(test.args, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &REPL{ctx: ctx, ComputerUse: func(gotCtx context.Context, action string) (computeruse.Status, error) {
				calls++
				if gotCtx != ctx || action != test.action {
					t.Fatalf("callback got context=%v action=%q, want shared context and %q", gotCtx, action, test.action)
				}
				return computeruse.Status{Installed: true, Enabled: true, Running: true}, nil
			}}
			if got := cmdCU(r, test.args); got != "cu: installed; enabled; running" {
				t.Fatalf("unexpected status: %q", got)
			}
			if calls != 1 {
				t.Fatalf("callback calls = %d, want 1", calls)
			}
		})
	}
}

func TestCU_NoManagerDoesNotAdoptUnmanagedBinaryOrChangeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	binDir := filepath.Join(home, "bin")
	configDir := filepath.Join(home, ".metis")
	for _, dir := range []string{binDir, configDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	name := "metis-cu"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	// An unmanaged executable exists on PATH, but no command may launch or
	// persist it. Its invalid body also prevents a real GUI process in tests.
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("unmanaged fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	configPath := filepath.Join(configDir, "mcp.toml")
	before := []byte("[[servers]]\nname = \"computer-use\"\ncommand = \"metis-cu\"\n")
	if err := os.WriteFile(configPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, repl := range []*REPL{nil, {}} {
		for _, action := range []string{"", "status", "install", "enable", "disable", "stop", "permissions-accessibility", "permissions-screen-recording"} {
			if got := cmdCU(repl, action); !strings.Contains(got, "unavailable") {
				t.Errorf("%q without manager: %q", action, got)
			}
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("unmanaged config changed: %q, %v", after, err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected configuration files: %v, %v", entries, err)
	}
}

func TestCU_RejectsExtraArgumentsWithoutCallback(t *testing.T) {
	r := &REPL{ComputerUse: func(context.Context, string) (computeruse.Status, error) {
		t.Fatal("invalid command reached manager")
		return computeruse.Status{}, nil
	}}
	for _, args := range []string{"enable /tmp/metis-cu", "install --from /tmp/metis-cu", "status extra", "stop all", "explode", "help", "--help"} {
		if got := cmdCU(r, args); !strings.Contains(got, cuUsage) {
			t.Errorf("%q should show usage, got %q", args, got)
		}
	}
}

func TestCU_CallbackErrorAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &REPL{ctx: ctx, ComputerUse: func(got context.Context, _ string) (computeruse.Status, error) {
		return computeruse.Status{Running: true}, got.Err()
	}}
	if got := cmdCU(r, "enable"); got != "cu: context canceled" {
		t.Fatalf("cancellation was lost or success reported: %q", got)
	}
	r = &REPL{ComputerUse: func(ctx context.Context, _ string) (computeruse.Status, error) {
		if ctx == nil {
			t.Fatal("callback received nil context")
		}
		return computeruse.Status{}, errors.New("component checksum mismatch")
	}}
	if got := cmdCU(r, "install"); got != "cu: component checksum mismatch" {
		t.Fatalf("manager error not surfaced: %q", got)
	}
}

func TestCU_StatusDistinguishesReadinessAndPermissions(t *testing.T) {
	status := computeruse.Status{
		Installed: true, Enabled: true, Running: false,
		Path: "/managed/metis-cu", Version: "1.2.3", Source: "local",
		Message: "Screen Recording permission is required",
		Description: &computeruse.Description{Permissions: map[string]string{
			"screen_recording": "denied", "accessibility": "granted",
		}},
	}
	got := formatComputerUseStatus(status)
	for _, expected := range []string{"cu: installed; enabled; stopped", "version: 1.2.3", "source: local", "binary: /managed/metis-cu", "accessibility: granted", "screen_recording: denied", status.Message} {
		if !strings.Contains(got, expected) {
			t.Errorf("status missing %q: %q", expected, got)
		}
	}
	if strings.Index(got, "accessibility:") > strings.Index(got, "screen_recording:") {
		t.Fatalf("permission order is not deterministic: %q", got)
	}
	if got := formatComputerUseStatus(computeruse.Status{}); got != "cu: not installed; disabled; stopped" {
		t.Fatalf("empty state: %q", got)
	}
}

func TestCU_TUIBridgeUsesSharedManager(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Model{ctx: ctx, ext: ExternalHooks{ComputerUse: func(got context.Context, action string) (computeruse.Status, error) {
		calls++
		if got != ctx || action != "stop" {
			t.Fatalf("bridge callback context/action mismatch: %v %q", got, action)
		}
		return computeruse.Status{Installed: true}, nil
	}}}
	if got := cmdCU(m.asREPL(), "stop"); got != "cu: installed; disabled; stopped" || calls != 1 {
		t.Fatalf("bridge did not delegate: %q calls=%d", got, calls)
	}
}

func TestCU_CommandCatalogShowsManagedActions(t *testing.T) {
	command := BuildREPLCommands().Get("cu")
	if command == nil || command.Handler == nil {
		t.Fatal("Computer Use command is not registered")
	}
	for _, action := range []string{"status", "install", "enable", "disable", "stop", "permissions-accessibility", "permissions-screen-recording"} {
		if !strings.Contains(command.Description, action) {
			t.Errorf("Computer Use help missing %q: %q", action, command.Description)
		}
	}
}
