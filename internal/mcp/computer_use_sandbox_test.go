package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
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
