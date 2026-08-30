//go:build !windows

package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCodeCancellationKillsProcessTree(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	runner := NewRunCode(bypassGate())
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		res, err := runner.Execute(ctx, map[string]any{
			"language": "bash",
			"code": fmt.Sprintf(
				"sleep 30 & child=$!; printf '%%s' \"$child\" > %q; wait",
				pidFile,
			),
			"timeout": float64(30),
		})
		if err != nil {
			resultCh <- err
			return
		}
		if res == nil || !res.IsError || !strings.Contains(res.Output, "cancelled") {
			resultCh <- fmt.Errorf("unexpected cancellation result: %#v", res)
			return
		}
		resultCh <- nil
	}()

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(string(raw))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		cancel()
		t.Fatal("RunCode child pid was not published")
	}

	cancel()
	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunCode cancellation did not return within the bounded wait")
	}

	for deadline := time.Now().Add(2 * time.Second); ; {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("RunCode descendant pid %d is still alive after cancellation (probe: %v)", childPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
