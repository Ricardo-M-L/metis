package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func installProbeGitFixture(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX controlled Git process fixture")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func probeFixtureQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func waitProbeMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Git fixture did not reach the blocking command")
}

func TestProbeContextCancellationRejectsIncompleteGitObservation(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(fmt.Sprintf("later_command_%t", later), func(t *testing.T) {
			workspace, marker := t.TempDir(), filepath.Join(t.TempDir(), "entered")
			body := ""
			if later {
				body += "case \"$*\" in *--show-toplevel*) printf '%s\\n' " + probeFixtureQuote(workspace) + "; exit 0;; esac\n"
			}
			body += "printf '%s\\n' \"$$\" > " + probeFixtureQuote(marker) + "\nexec /bin/sleep 60\n"
			installProbeGitFixture(t, body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := ProbeContext(ctx, workspace); done <- err }()
			waitProbeMarker(t, marker)
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled observation became a profile: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled Probe did not join its Git process")
			}
			if time.Since(start) > time.Second {
				t.Fatal("Git cancellation exceeded the pipe-drain allowance")
			}
		})
	}
}

func TestProbeBackgroundHasOneOverallDeadline(t *testing.T) {
	workspace, marker := t.TempDir(), filepath.Join(t.TempDir(), "entered")
	installProbeGitFixture(t, "printf '%s\\n' \"$$\" > "+probeFixtureQuote(marker)+"\nexec /bin/sleep 60\n")
	start := time.Now()
	_, err := Probe(workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed out Git became a non-Git profile: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("overall Probe deadline elapsed=%s", elapsed)
	}
}

func TestProbeContextPipeDrainIsBoundedAndDescendantStopped(t *testing.T) {
	workspace, marker := t.TempDir(), filepath.Join(t.TempDir(), "descendant")
	installProbeGitFixture(t, "/bin/sleep 60 &\nprintf '%s\\n' \"$!\" > "+probeFixtureQuote(marker)+"\nexit 0\n")
	start := time.Now()
	_, err := ProbeContext(context.Background(), workspace)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("unjoined Git output became a non-Git profile: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("inherited stdout delayed Probe beyond its drain limit")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		state := strings.TrimSpace(string(output))
		if state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Probe-owned descendant %d survived", pid)
}
