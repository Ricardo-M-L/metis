//go:build darwin

package bash

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplySandboxWrap_OffPassesThrough — mode=off must return the
// *exec.Cmd byte-for-byte unchanged so the legacy bash path keeps
// working exactly as before this commit.
func TestApplySandboxWrap_OffPassesThrough(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "echo hi")
	got, err := applySandboxWrap(context.Background(), cmd, SandboxModeOff, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Path != "/bin/sh" {
		t.Errorf("off mode altered Path: %q", got.Path)
	}
	if len(got.Args) != 3 || got.Args[0] != "/bin/sh" {
		t.Errorf("off mode altered Args: %v", got.Args)
	}
}

// TestApplySandboxWrap_PermissionsWrapsWithSeatbelt — permissions
// mode should produce a cmd whose Path is sandbox-exec, with the
// original argv re-issued after a "-p <profile>" pair.
func TestApplySandboxWrap_PermissionsWrapsWithSeatbelt(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "echo hi")
	wrapped, err := applySandboxWrap(context.Background(), cmd, SandboxModePermissions, t.TempDir())
	if err != nil {
		t.Fatalf("wrap err: %v", err)
	}
	if wrapped.Path != "/usr/bin/sandbox-exec" {
		t.Errorf("wrapped Path = %q, want /usr/bin/sandbox-exec", wrapped.Path)
	}
	if len(wrapped.Args) < 5 {
		t.Fatalf("wrapped Args too short: %v", wrapped.Args)
	}
	if wrapped.Args[0] != "sandbox-exec" {
		t.Errorf("wrapped Args[0] = %q, want sandbox-exec", wrapped.Args[0])
	}
	if wrapped.Args[1] != "-p" {
		t.Errorf("wrapped Args[1] = %q, want -p", wrapped.Args[1])
	}
	// Re-issued original argv lives at the tail.
	if !strings.HasSuffix(strings.Join(wrapped.Args, " "), "/bin/sh -c echo hi") {
		t.Errorf("wrapped Args tail lost original argv: %v", wrapped.Args)
	}
}

// TestSandboxProfile_AllowsCwdWritesBlocksElsewhere — end-to-end
// integration test: build the profile for a specific cwd, run a
// command through sandbox-exec, and verify that
//   - writing INSIDE the cwd succeeds
//   - writing to /private/etc fails (kernel-level EPERM)
//
// Pins the actual security guarantee, not just the wrapper shape.
// Skipped when sandbox-exec is unavailable (corp-policy / etc.).
func TestSandboxProfile_AllowsCwdWritesBlocksElsewhere(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	dir := t.TempDir()
	profile := buildSandboxProfile(dir)

	// Allowed write: inside the cwd subtree.
	allowed := filepath.Join(dir, "allowed.txt")
	cmd := exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c",
		"echo ok > "+allowed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cwd write blocked unexpectedly: %v\noutput=%s", err, out)
	}
	if data, err := os.ReadFile(allowed); err != nil || string(data) != "ok\n" {
		t.Errorf("allowed file missing/wrong content: data=%q err=%v", data, err)
	}

	// Blocked write: outside the cwd. We try /private/etc which has
	// no chance of being on the allowlist for any user. Even root
	// would be EPERM'd here because Seatbelt outranks DAC.
	cmd = exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c",
		"echo nope > /private/etc/metis-sandbox-canary 2>&1")
	out, _ := cmd.CombinedOutput()
	// We expect a failure mentioning permission / sandbox / operation not permitted.
	low := strings.ToLower(string(out))
	if !strings.Contains(low, "not permitted") && !strings.Contains(low, "sandbox") && !strings.Contains(low, "permission") {
		t.Errorf("expected sandbox-level denial; got: %s", out)
	}
	if _, err := os.Stat("/private/etc/metis-sandbox-canary"); err == nil {
		// Try to clean up if it somehow got through; this would mean
		// the sandbox failed open.
		_ = os.Remove("/private/etc/metis-sandbox-canary")
		t.Error("sandbox FAILED OPEN: /private/etc/metis-sandbox-canary was written despite the profile")
	}

	// Allowed read: arbitrary system files (the profile permits
	// file-read* everywhere). Sanity check that we didn't accidentally
	// lock down reads.
	cmd = exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c", "cat /etc/hosts >/dev/null")
	if err := cmd.Run(); err != nil {
		t.Errorf("global read was blocked; profile is too restrictive: %v", err)
	}
}

// TestSandboxProfile_ContainsExpectedAllows — schema-level guard
// against accidental removal of one of the allow clauses the
// profile relies on (process-exec is the load-bearing one; without
// it the wrapped binary can't even start). This catches "I
// refactored buildSandboxProfile and lost a clause" regressions
// before they reach the integration test which is slower.
func TestSandboxProfile_ContainsExpectedAllows(t *testing.T) {
	profile := buildSandboxProfile(t.TempDir())
	mustContain := []string{
		"(deny default)",
		"(allow process-exec)",
		"(allow process-fork)",
		"(allow file-read*)",
		"(allow network*)",
		"(allow mach-lookup)",
	}
	for _, want := range mustContain {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing expected clause %q;\nfull profile:\n%s", want, profile)
		}
	}
}
