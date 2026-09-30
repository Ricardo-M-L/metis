package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
)

func permissionFixtureDescription(t *testing.T, value string) string {
	t.Helper()
	description := computeruse.Description{
		Name: "metis-cu", Version: "permission-test", ProtocolVersion: computeruse.ProtocolVersion,
		Platform: goruntime.GOOS, Arch: goruntime.GOARCH,
		Capabilities: []string{"status", "stop", "end-turn", "serialized-input", "input-ownership"},
		Permissions:  map[string]string{"accessibility": value, "screenRecording": value},
	}
	data, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func shellPermissionQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func permissionFixtureHelper(t *testing.T, requestDescription, marker string) string {
	return permissionFixtureHelperWithProbe(t, requestDescription, marker, "notGranted")
}

func permissionFixtureHelperWithProbe(t *testing.T, requestDescription, marker, probeValue string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "metis-cu")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"  --describe) [ \"$2\" = --json ] || exit 9; printf '%s\\n' " + shellPermissionQuote(permissionFixtureDescription(t, probeValue)) + ";;\n" +
		"  --request-permission) [ \"$3\" = --json ] || exit 9; printf '%s\\n' \"$2\" > " + shellPermissionQuote(marker) + "; printf '%s\\n' " + shellPermissionQuote(requestDescription) + ";;\n" +
		"  *) exit 2;;\nesac\n"
	if requestDescription == "" {
		script = "#!/bin/sh\n[ \"$1\" = --describe ] && [ \"$2\" = --json ] || exit 2\nprintf '%s\\n' " + shellPermissionQuote(permissionFixtureDescription(t, "notGranted")) + "\n"
	}
	if err := os.WriteFile(filename, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestComputerUsePermissionRequestDoesNotTrustProbeGrant(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS permission request flow")
	}
	manager := computeruse.New(cuTestHome(t))
	marker := filepath.Join(t.TempDir(), "requested")
	_, err := manager.InstallLocal(context.Background(), permissionFixtureHelperWithProbe(t, permissionFixtureDescription(t, "notGranted"), marker, "granted"))
	if err != nil {
		t.Fatal(err)
	}
	opened := false
	status, err := (*runtime)(nil).requestComputerUsePermission(context.Background(), manager, "permissions-accessibility",
		func(context.Context, string) error { opened = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil || !opened || status.Description.Permissions["accessibility"] != "notGranted" {
		t.Fatalf("direct helper request skipped despite a granted probe: marker=%v opened=%t status=%+v", err, opened, status)
	}
}

func TestComputerUsePermissionRequestsVerifiedInstalledHelper(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS permission request flow")
	}
	for _, test := range []struct {
		name, action, kind, requested string
		wantSettings                  bool
	}{
		{"accessibility-granted", "permissions-accessibility", "accessibility", "granted", false},
		{"screen-recording-pending", "permissions-screen-recording", "screen-recording", "notGranted", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := cuTestHome(t)
			marker := filepath.Join(t.TempDir(), "request-kind")
			manager := computeruse.New(home)
			installed, err := manager.InstallLocal(context.Background(), permissionFixtureHelper(t, permissionFixtureDescription(t, test.requested), marker))
			if err != nil {
				t.Fatal(err)
			}
			var opened []string
			status, err := (*runtime)(nil).requestComputerUsePermission(context.Background(), manager, test.action,
				func(_ context.Context, action string) error { opened = append(opened, action); return nil })
			if err != nil {
				t.Fatal(err)
			}
			kind, err := os.ReadFile(marker)
			if err != nil || strings.TrimSpace(string(kind)) != test.kind {
				t.Fatalf("request kind = %q, err = %v", kind, err)
			}
			if status.Path != installed.Path || !status.Installed || status.Description == nil {
				t.Fatalf("status did not refresh verified installation: %+v", status)
			}
			key := test.kind
			if key == "screen-recording" {
				key = "screenRecording"
			}
			if status.Description.Permissions[key] != test.requested {
				t.Fatalf("request result was lost to a stale status probe: %+v", status.Description)
			}
			if (len(opened) > 0) != test.wantSettings || len(opened) > 1 {
				t.Fatalf("settings opened %v, want %t", opened, test.wantSettings)
			}
			if test.wantSettings && opened[0] != test.action {
				t.Fatalf("wrong settings pane action: %v", opened)
			}
		})
	}
}

func TestComputerUsePermissionRejectsMissingTamperedAndOldHelper(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS permission request flow")
	}
	for _, test := range []struct {
		name, kind, wantError string
	}{
		{"missing", "missing", "not installed"},
		{"tampered", "tampered", "checksum"},
		{"old", "old", "update or reinstall"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := cuTestHome(t)
			manager := computeruse.New(home)
			if test.kind != "missing" {
				requestDescription := permissionFixtureDescription(t, "notGranted")
				if test.kind == "old" {
					requestDescription = ""
				}
				installed, err := manager.InstallLocal(context.Background(), permissionFixtureHelper(t, requestDescription, filepath.Join(t.TempDir(), "marker")))
				if err != nil {
					t.Fatal(err)
				}
				if test.kind == "tampered" {
					if err := os.Chmod(installed.Path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(installed.Path, []byte("tampered"), 0500); err != nil {
						t.Fatal(err)
					}
				}
			}
			opened := false
			_, err := (*runtime)(nil).requestComputerUsePermission(context.Background(), manager, "permissions-accessibility",
				func(context.Context, string) error { opened = true; return nil })
			if err == nil || !strings.Contains(err.Error(), test.wantError) || opened {
				t.Fatalf("kind=%s err=%v opened=%t", test.kind, err, opened)
			}
		})
	}
}

func TestComputerUsePermissionRequestHonorsCancellation(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	filename := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(filename, []byte("#!/bin/sh\nexec sleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runComputerUsePermissionRequest(ctx, computeruse.Status{Path: filename, Version: "permission-test"}, "accessibility")
	if err == nil || !strings.Contains(err.Error(), "canceled") || time.Since(start) > 2*time.Second {
		t.Fatalf("request did not stop within bound: err=%v elapsed=%s", err, time.Since(start))
	}
}
