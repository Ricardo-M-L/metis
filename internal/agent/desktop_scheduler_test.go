package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDesktopSchedulerHelperProcess(t *testing.T) {
	if os.Getenv("METIS_TEST_SCHED_HELPER") != "1" {
		return
	}
	dir := os.Getenv("METIS_TEST_SCHED_DIR")
	release, err := acquireDesktopExecution(context.Background(), dir, 1, 1, false, "helper")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = os.Stdin.Read(make([]byte, 1))
}

func TestDesktopSchedulerCrossProcessCrashReclaimsCapacity(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDesktopSchedulerHelperProcess$")
	cmd.Env = append(os.Environ(), "METIS_TEST_SCHED_HELPER=1", "METIS_TEST_SCHED_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan bool, 1)
	go func() { ready <- bufio.NewScanner(stdout).Scan() }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("helper did not acquire slot: %s", stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not reach acquisition barrier")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if release, err := acquireDesktopExecution(ctx, dir, 1, 1, false, "parent"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("other process exceeded total capacity: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // A crash is the behavior under test.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	release, err := acquireDesktopExecution(ctx2, dir, 1, 1, false, "parent")
	if err != nil {
		t.Fatalf("crashed process retained capacity: %v", err)
	}
	release()
	idle, err := DesktopSchedulerIdle(dir)
	if err != nil || !idle {
		t.Fatalf("crash recovery left scheduler busy: idle=%v err=%v", idle, err)
	}
}

func desktopQueuedCount(t *testing.T, dir string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := desktopSchedulerLock(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := scanDesktopQueue(dir, 0)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	return len(queue)
}

func waitDesktopQueuedCount(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if desktopQueuedCount(t, dir) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queued requests = %d, want %d", desktopQueuedCount(t, dir), want)
}

func TestDesktopSchedulerCancelledTicketRemoved(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDesktopExecution(context.Background(), dir, 1, 1, false, "root")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		release, err := acquireDesktopExecution(ctx, dir, 1, 1, true, "child")
		if release != nil {
			release()
		}
		result <- err
	}()
	waitDesktopQueuedCount(t, dir, 1)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queue = %v", err)
	}
	waitDesktopQueuedCount(t, dir, 0)
	first()
	idle, err := DesktopSchedulerIdle(dir)
	if err != nil || !idle {
		t.Fatalf("cancelled ticket kept scheduler busy: idle=%v err=%v", idle, err)
	}
}

func TestDesktopSchedulerAlternatesOwnersWithoutBreakingOwnerFIFO(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	blocker, err := acquireDesktopExecution(ctx, dir, 1, 1, false, "A")
	if err != nil {
		t.Fatal(err)
	}
	type admission struct {
		name    string
		release func()
		err     error
	}
	entered := make(chan admission, 3)
	for i, item := range []struct{ name, owner string }{{"A1", "A"}, {"A2", "A"}, {"B1", "B"}} {
		go func(name, owner string) {
			release, err := acquireDesktopExecution(ctx, dir, 1, 1, false, owner)
			entered <- admission{name: name, release: release, err: err}
		}(item.name, item.owner)
		waitDesktopQueuedCount(t, dir, i+1)
	}
	blocker()
	for _, want := range []string{"B1", "A1", "A2"} {
		select {
		case got := <-entered:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.name != want {
				got.release()
				t.Fatalf("owner fairness admitted %s, want %s", got.name, want)
			}
			got.release()
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", want, ctx.Err())
		}
	}
}

func TestDesktopSchedulerEightRootsAndSixteenSharedExecutions(t *testing.T) {
	dir := t.TempDir()
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < 8; i++ {
		release, err := acquireDesktopExecution(context.Background(), dir, 16, 8, false, fmt.Sprintf("root-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	for i := 0; i < 8; i++ {
		release, err := acquireDesktopExecution(context.Background(), dir, 16, 8, true, fmt.Sprintf("root-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if release, err := acquireDesktopExecution(ctx, dir, 16, 8, false, "extra-root"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("seventeenth execution = %v, want full pool", err)
	}
	lock, err := desktopSchedulerLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := scanDesktopSlots(dir, 16)
	lock()
	if err != nil || len(active) != 16 {
		t.Fatalf("execution peak = %d, want 16; err=%v", len(active), err)
	}
}

func TestDesktopSchedulerIdleBudgetCanBeBorrowedByChildren(t *testing.T) {
	dir := t.TempDir()
	var releases []func()
	for i := 0; i < 16; i++ {
		release, err := acquireDesktopExecution(context.Background(), dir, 16, 16, true, "sole-root")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if release, err := acquireDesktopExecution(ctx, dir, 16, 16, false, "other-root"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("other root entered full borrowed budget: %v", err)
	}
	releases[0]()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	rootRelease, err := acquireDesktopExecution(ctx2, dir, 16, 16, false, "other-root")
	if err != nil {
		t.Fatal(err)
	}
	rootRelease()
	for _, release := range releases[1:] {
		release()
	}
	// A completed run can leave advisory lock files and metadata, but none of
	// those stale files may count as an occupied permit.
	if files, err := filepath.Glob(filepath.Join(dir, "ticket-*.lock")); err != nil || len(files) != 0 {
		t.Fatalf("tickets after release = %v, err=%v", files, err)
	}
	idle, err := DesktopSchedulerIdle(dir)
	if err != nil || !idle {
		t.Fatalf("borrowed budget did not return: idle=%v err=%v", idle, err)
	}
}

func TestDesktopSchedulerOwnerSelectionSkipsCappedChildren(t *testing.T) {
	queue := []desktopRequest{
		{Sequence: 1, Owner: "A", Child: true, ChildLimit: 1},
		{Sequence: 2, Owner: "B", Child: true, ChildLimit: 1},
		{Sequence: 3, Owner: "A", Child: true, ChildLimit: 1},
	}
	active := []desktopRequest{{Owner: "A", Child: true}}
	if got := desktopNextRequest(queue, active, "B"); got != 2 {
		t.Fatalf("capped owner blocked other owner: chose %d", got)
	}
	if got := desktopNextRequest(queue, nil, "B"); got != 1 {
		t.Fatalf("per-owner FIFO broken: chose %d", got)
	}
	if got := desktopNextRequest(queue, []desktopRequest{{Owner: "A", Child: true}, {Owner: "B", Child: true}}, "A"); got != 0 {
		t.Fatalf("fully capped queue chose %d", got)
	}
}

func TestDesktopSchedulerUnconfiguredLeaseDoesNotStart(t *testing.T) {
	t.Setenv(desktopSubagentSlotDirEnv, "")
	lease, err := desktopExecutionLease(context.Background(), false)
	if err != nil || lease != nil {
		t.Fatalf("ordinary CLI lease = %v, %v", lease, err)
	}
	if strings.TrimSpace(os.Getenv(desktopSubagentSlotDirEnv)) != "" {
		t.Fatal("test unexpectedly configured Desktop scheduler")
	}
}

func TestDesktopSchedulerRotatesThreeBackloggedRoots(t *testing.T) {
	queue := []desktopRequest{
		{Sequence: 1, Owner: "A"}, {Sequence: 2, Owner: "A"},
		{Sequence: 3, Owner: "B"}, {Sequence: 4, Owner: "B"},
		{Sequence: 5, Owner: "C"}, {Sequence: 6, Owner: "C"},
	}
	last := ""
	for _, want := range []uint64{1, 3, 5, 2, 4, 6} {
		got := desktopNextRequest(queue, nil, last)
		if got != want {
			t.Fatalf("after owner %s next=%d, want %d", last, got, want)
		}
		for i, request := range queue {
			if request.Sequence == got {
				last = request.Owner
				queue = append(queue[:i], queue[i+1:]...)
				break
			}
		}
	}
}
