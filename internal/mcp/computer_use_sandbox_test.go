package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestStdioSandboxInputOwnershipRequiresManagedComputerUseProfile(t *testing.T) {
	for _, profile := range []StdioSandboxProfile{StdioSandboxProfileGeneric, StdioSandboxProfileComputerUse, StdioSandboxProfileManagedComputerUse} {
		request := stdioSandboxRequest("/work/repo", profile)
		if request.Cwd != "/work/repo" || request.ComputerUseInputOwnership != (profile == StdioSandboxProfileManagedComputerUse) {
			t.Fatalf("profile %d mapped to request %+v", profile, request)
		}
		if request.ComputerUseAppGrants != (profile == StdioSandboxProfileManagedComputerUse) {
			t.Fatalf("profile %d mapped to app-grant request %+v", profile, request)
		}
		if request.MinimumMode != "" {
			t.Fatalf("profile %d changed runtime-owned sandbox policy: %+v", profile, request)
		}
	}
}

func TestManagedComputerUseLaunchRequiresVerifiedPathAndProtocol(t *testing.T) {
	verifiedPath := filepath.Join(t.TempDir(), "versions", "test", "metis-cu")
	verificationFailed := errors.New("digest or protocol verification failed")
	for _, tc := range []struct {
		name, command string
		args          []string
		resolveErr    error
		wantOK        bool
	}{
		{name: "PATH command", command: "metis-cu"},
		{name: "different executable", command: filepath.Join(t.TempDir(), "metis-cu")},
		{name: "custom arguments", command: verifiedPath, args: []string{"--override"}},
		{name: "failed verification", command: verifiedPath, resolveErr: verificationFailed},
		{name: "verified managed path", command: verifiedPath, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyManagedComputerUseLaunch(context.Background(), tc.command, tc.args, func(context.Context) (string, error) {
				return verifiedPath, tc.resolveErr
			})
			if (err == nil) != tc.wantOK {
				t.Fatalf("verification = %v, want success %v", err, tc.wantOK)
			}
			if tc.resolveErr != nil && !errors.Is(err, verificationFailed) {
				t.Fatalf("verification failure was not retained: %v", err)
			}
		})
	}
}

func TestManagedComputerUseDirectConstructorsRejectUnverifiedProfile(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for _, command := range []string{"metis-cu", filepath.Join(t.TempDir(), "metis-cu")} {
		transport, err := NewStdioTransportWithEnvAndDirAndSandboxProfile(context.Background(), command, nil, "", nil, StdioSandboxProfileManagedComputerUse)
		if transport != nil || err == nil || !strings.Contains(err.Error(), "managed Computer Use") {
			t.Fatalf("direct transport accepted unverified profile: %v, %v", transport, err)
		}
		client, err := NewStdioClientWithEnvAndDirAndSandboxProfile(context.Background(), command, nil, "", nil, StdioSandboxProfileManagedComputerUse)
		if client != nil || err == nil || !strings.Contains(err.Error(), "managed Computer Use") {
			t.Fatalf("direct client accepted unverified profile: %v, %v", client, err)
		}
	}
}

func TestManagedComputerUseProfilePreservesNarrowDesktopEnvironment(t *testing.T) {
	env := envMap(sanitizedStdioEnvForProfile([]string{
		"DISPLAY=:88", "XAUTHORITY=/tmp/cu-cookie", "DBUS_SESSION_BUS_ADDRESS=private",
	}, "", StdioSandboxProfileManagedComputerUse))
	if env["DISPLAY"] != ":88" || env["XAUTHORITY"] != "/tmp/cu-cookie" {
		t.Fatal("managed profile lost required X11 environment")
	}
	if _, ok := env["DBUS_SESSION_BUS_ADDRESS"]; ok {
		t.Fatal("managed profile broadened desktop environment access")
	}
}

// A Desktop launched from Finder may inherit "/" as its process cwd. The
// verified helper must get the runtime-owned writable directory, while an
// ordinary MCP server must still be denied a writable filesystem root.
func TestManagedComputerUseFromRootUsesPrivateSandboxCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell helper and filesystem-root guard")
	}
	privateRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metisHome := filepath.Join(privateRoot, "home", ".metis")
	t.Setenv("METIS_HOME", metisHome)
	description, err := json.Marshal(computeruse.Description{
		Name: "metis-cu", Version: "test", ProtocolVersion: computeruse.ProtocolVersion,
		Platform: runtime.GOOS, Arch: runtime.GOARCH,
		Capabilities: []string{"status", "stop", "end-turn", "serialized-input", "input-ownership"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(privateRoot, "metis-cu-fixture")
	script := "#!/bin/sh\nif [ \"$1\" = --describe ] && [ \"$2\" = --json ]; then\n" +
		"printf '%s\\n' '" + string(description) + "'\nexit 0\nfi\npwd -P\ncat >/dev/null\n"
	if err := os.WriteFile(fixture, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	installed, err := computeruse.New(metisHome).InstallLocal(context.Background(), fixture)
	if err != nil {
		t.Fatalf("install isolated verified fixture: %v", err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode: string(sandbox.ModePermissions), TempRoot: privateRoot, MetisHome: metisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close sandbox: %v", err)
		}
	})
	t.Chdir("/")
	for _, workingDir := range []string{"", "/"} {
		transport, err := NewStdioTransportWithEnvAndDirAndSandboxProfile(
			context.Background(), installed.Path, nil, workingDir,
			manager, StdioSandboxProfileManagedComputerUse,
		)
		if err != nil {
			t.Fatalf("launch verified helper from root with workingDir %q: %v", workingDir, err)
		}
		if got := transport.cmd.Dir; got != manager.TempDir() {
			_ = transport.Close()
			t.Fatalf("managed cmd.Dir = %q, want private sandbox directory %q", got, manager.TempDir())
		}
		if got := envMap(transport.cmd.Env)["PWD"]; got != manager.TempDir() {
			_ = transport.Close()
			t.Fatalf("managed PWD = %q, want private sandbox directory %q", got, manager.TempDir())
		}
		actualCwd, readErr := bufio.NewReader(transport.stdout).ReadString('\n')
		if readErr != nil || strings.TrimSpace(actualCwd) != manager.TempDir() {
			_ = transport.Close()
			t.Fatalf("helper actual cwd = %q, read error = %v; want %q", actualCwd, readErr, manager.TempDir())
		}
		if err := transport.Close(); err != nil {
			t.Fatal(err)
		}
	}
	generic, err := NewStdioTransportWithEnvAndDirAndSandboxProfile(
		context.Background(), installed.Path, nil, "/", manager, StdioSandboxProfileGeneric,
	)
	if generic != nil || !errors.Is(err, sandbox.ErrUnsafeCwd) {
		if generic != nil {
			_ = generic.Close()
		}
		t.Fatalf("ordinary MCP root working directory = (%v, %v), want ErrUnsafeCwd", generic, err)
	}
}
