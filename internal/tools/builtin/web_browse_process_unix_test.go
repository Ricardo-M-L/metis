//go:build !windows

package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunWebBrowseCommandBoundsInheritedPipeAndKillsTree(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.CommandContext(context.Background(), "bash", "-c", fmt.Sprintf(
		"sleep 30 & child=$!; printf '%%s' \"$child\" > %q",
		pidFile,
	))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	configureWebBrowseProcessWithWaitDelay(cmd, 50*time.Millisecond)

	start := time.Now()
	err := runWebBrowseCommand(cmd)
	elapsed := time.Since(start)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("runWebBrowseCommand error = %v, want exec.ErrWaitDelay (stderr=%q)", err, stderr.String())
	}
	if elapsed > time.Second {
		t.Fatalf("inherited browser pipe kept Wait blocked for %s", elapsed)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || childPID <= 0 {
		t.Fatalf("child pid = %q, err=%v", raw, err)
	}
	assertWebBrowseProcessGone(t, childPID)
}

func TestRunWebBrowseCommandCancellationKillsTree(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf(
		"sleep 30 & child=$!; printf '%%s' \"$child\" > %q; wait",
		pidFile,
	))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	configureWebBrowseProcessWithWaitDelay(cmd, 100*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- runWebBrowseCommand(cmd) }()

	var childPID int
	for deadline := time.Now().Add(3 * time.Second); ; {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err == nil && childPID > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("browser child pid was not published: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled WebBrowse command unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("WebBrowse cancellation did not return within its bound")
	}
	assertWebBrowseProcessGone(t, childPID)
}

func assertWebBrowseProcessGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("WebBrowse descendant pid %d survived cleanup: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
