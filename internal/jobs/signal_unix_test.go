//go:build !windows

package jobs

// signal_unix_test.go — pin the tree-kill behaviour added 2026-05-09.
// The interesting case is `bash -c 'sleep N & sleep N & wait'` —
// before the Setpgid + killTreeStaged work, Stop would only
// SIGTERM the bash leader and the two child sleeps would survive as
// orphans (re-parented to init, still consuming a slot). After the
// fix, the negative-pid SIGKILL hits the entire group at once.
//
// We use `pgrep -P <pid>` to count surviving descendants. Avoids
// platform-specific /proc parsing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRegistryResetAndWaitTermIgnoringHelper(t *testing.T) {
	if os.Getenv("METIS_TEST_RESET_AND_WAIT_HELPER") != "1" {
		return
	}
	ready := os.NewFile(3, "reset-and-wait-ready")
	termObserved := os.NewFile(4, "reset-and-wait-term")
	if ready == nil || termObserved == nil {
		os.Exit(2)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	if _, err := ready.Write([]byte{1}); err != nil {
		os.Exit(2)
	}
	_ = ready.Close()
	<-term
	// Observe SIGTERM but deliberately stay alive. ResetAndWait must escalate
	// to SIGKILL and join the Registry-owned cmd.Wait before it returns.
	if _, err := termObserved.Write([]byte{1}); err != nil {
		os.Exit(2)
	}
	_ = termObserved.Close()
	select {}
}

func TestRegistryLeaderExitChildIgnoresTermHelper(t *testing.T) {
	role := os.Getenv("METIS_TEST_LEADER_EXIT_CHILD_ROLE")
	if role == "" {
		return
	}
	pidPath := os.Getenv("METIS_TEST_LEADER_EXIT_CHILD_PID")
	child := exec.Command("sh", "-c", `trap '' TERM HUP; echo $$ > "$METIS_TEST_LEADER_EXIT_CHILD_PID"; while :; do sleep 1; done`)
	child.Env = append(os.Environ(), "METIS_TEST_LEADER_EXIT_CHILD_PID="+pidPath)
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	<-term
	// The leader exits cooperatively while its same-process-group child keeps
	// running and ignores SIGTERM. The Registry's delayed SIGKILL stage must
	// therefore outlive cmd.Wait and finish the process group.
	os.Exit(0)
}

func TestRegistryResetAndWaitKillsGroupAfterLeaderExitsOnTerm(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryLeaderExitChildIgnoresTermHelper$")
	cmd.Env = append(os.Environ(),
		"METIS_TEST_LEADER_EXIT_CHILD_ROLE=leader",
		"METIS_TEST_LEADER_EXIT_CHILD_PID="+pidPath,
	)
	r := NewRegistry(t.TempDir())
	if _, err := r.Spawn(SpawnArgs{Command: "leader-with-term-ignoring-child", Cmd: cmd}); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	leaderPID := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-leaderPID, syscall.SIGKILL) }()

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 || !pidAlive(childPID) {
		t.Fatalf("term-ignoring child did not become ready: pid=%d", childPID)
	}

	r.ResetAndWait(250 * time.Millisecond)
	if cmd.ProcessState == nil {
		t.Fatal("ResetAndWait returned before reaping the cooperative leader")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && pidAlive(childPID) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidAlive(childPID) {
		t.Fatalf("ResetAndWait returned while SIGTERM-ignoring child pid %d was alive", childPID)
	}
}

func TestRegistryResetAndWaitKillsChildAfterNaturalLeaderExit(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "natural-child.pid")
	cmd := exec.Command("bash", "-c", `trap '' TERM HUP; sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$METIS_TEST_NATURAL_CHILD_PID"`)
	cmd.Env = append(os.Environ(), "METIS_TEST_NATURAL_CHILD_PID="+pidPath)
	r := NewRegistry(t.TempDir())
	job, err := r.Spawn(SpawnArgs{Command: "natural-leader-exit", Cmd: cmd})
	if err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	leaderPID := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-leaderPID, syscall.SIGKILL) }()
	waitForStatus(t, r, job.ID, StatusCompleted, 3*time.Second)
	if cmd.ProcessState == nil {
		t.Fatal("precondition: leader was not reaped")
	}

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 || !pidAlive(childPID) {
		t.Fatalf("natural leader did not leave a live child: pid=%d", childPID)
	}

	r.ResetAndWait(0)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && pidAlive(childPID) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidAlive(childPID) {
		t.Fatalf("ResetAndWait returned while natural leader's child pid %d was alive", childPID)
	}
}

func spawnNaturallyCompletedJobWithLiveChild(t *testing.T, command string) (*Registry, *Job, *exec.Cmd, int) {
	t.Helper()
	pidPath := filepath.Join(t.TempDir(), "natural-child.pid")
	cmd := exec.Command("bash", "-c", `trap '' TERM HUP; sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$METIS_TEST_NATURAL_CHILD_PID"`)
	cmd.Env = append(os.Environ(), "METIS_TEST_NATURAL_CHILD_PID="+pidPath)
	r := NewRegistry(t.TempDir())
	job, err := r.Spawn(SpawnArgs{Command: command, Cmd: cmd})
	if err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	leaderPID := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		r.ResetAndWait(0)
	})
	waitForStatus(t, r, job.ID, StatusCompleted, 3*time.Second)
	if cmd.ProcessState == nil {
		t.Fatal("precondition: leader was not reaped")
	}

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 || !pidAlive(childPID) {
		t.Fatalf("natural leader did not leave a live child: pid=%d", childPID)
	}
	return r, job, cmd, childPID
}

func waitForPIDExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && pidAlive(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("pid %d remained alive after %s", pid, timeout)
	}
}

func TestRegistryStopKillsChildAfterNaturalLeaderExit(t *testing.T) {
	r, job, _, childPID := spawnNaturallyCompletedJobWithLiveChild(t, "stop-natural-child")
	if err := r.Stop(job.ID, 0); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForPIDExit(t, childPID, 3*time.Second)
	snapshot, ok := r.Get(job.ID)
	if !ok || snapshot.Status != StatusKilled {
		t.Fatalf("Stop did not mark retained tree killed: ok=%v job=%+v", ok, snapshot)
	}
}

func TestRegistryShutdownKillsChildAfterNaturalLeaderExit(t *testing.T) {
	r, _, _, childPID := spawnNaturallyCompletedJobWithLiveChild(t, "shutdown-natural-child")
	r.Shutdown(0)
	waitForPIDExit(t, childPID, 3*time.Second)
}

func TestRegistryResetAndWaitJoinsProcessRetiredByPriorReset(t *testing.T) {
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	termR, termW, err := os.Pipe()
	if err != nil {
		_ = readyW.Close()
		t.Fatal(err)
	}
	defer termR.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryResetAndWaitTermIgnoringHelper$")
	cmd.Env = append(os.Environ(), "METIS_TEST_RESET_AND_WAIT_HELPER=1")
	cmd.ExtraFiles = []*os.File{readyW, termW}
	r := NewRegistry(t.TempDir())
	if _, err := r.Spawn(SpawnArgs{Command: "retired-term-ignoring-helper", Cmd: cmd}); err != nil {
		_ = readyW.Close()
		_ = termW.Close()
		t.Fatalf("spawn helper: %v", err)
	}
	_ = readyW.Close()
	_ = termW.Close()
	pid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }()

	if err := readyR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(readyR, marker[:]); err != nil {
		t.Fatalf("wait for helper readiness: %v", err)
	}

	r.Reset(time.Hour)
	if err := termR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(termR, marker[:]); err != nil {
		t.Fatalf("ordinary Reset did not signal helper: %v", err)
	}
	if len(r.List()) != 0 {
		t.Fatal("ordinary Reset did not hide the source generation")
	}
	if !pidAlive(pid) {
		t.Fatal("precondition: long-grace helper exited before strict reset")
	}

	r.ResetAndWait(0)
	if cmd.ProcessState == nil {
		t.Fatal("ResetAndWait returned before cmd.Wait reaped the retired helper")
	}
	if pidAlive(pid) {
		t.Fatalf("ResetAndWait returned while retired helper pid %d was alive", pid)
	}
}

func TestRegistryAdoptSnapshotConcurrentReset(t *testing.T) {
	type adoptResult struct {
		job *Job
		err error
	}
	for attempt := 0; attempt < 64; attempt++ {
		r := NewRegistry(t.TempDir())
		out, _, err := r.NewDiskOutput()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", "sleep 30")
		cmd.Stdout = out.Writer()
		cmd.Stderr = out.Writer()
		ApplyProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		waitResult := make(chan error, 1)
		go func() { waitResult <- cmd.Wait() }()

		adopted := make(chan adoptResult, 1)
		go func() {
			job, err := r.Adopt(AdoptArgs{
				Command:    "adopt-reset-race",
				Cmd:        cmd,
				Output:     out,
				WaitResult: waitResult,
			})
			adopted <- adoptResult{job: job, err: err}
		}()
		resetDone := make(chan struct{})
		go func() {
			for {
				r.mu.RLock()
				published := len(r.jobs) != 0
				r.mu.RUnlock()
				if published {
					r.Reset(0)
					close(resetDone)
					return
				}
				runtime.Gosched()
			}
		}()

		result := <-adopted
		<-resetDone
		if result.err != nil {
			t.Fatalf("attempt %d: Adopt: %v", attempt, result.err)
		}
		if result.job.Status != StatusRunning || !result.job.EndTime.IsZero() || result.job.ExitCode != -1 {
			t.Fatalf("attempt %d: Adopt returned a reset-mutated snapshot: %+v", attempt, result.job)
		}
		r.ResetAndWait(0)
		if cmd.ProcessState == nil {
			t.Fatalf("attempt %d: strict cleanup did not reap adopted command", attempt)
		}
	}
}

func TestRegistryResetAndWaitJoinsPreviouslyStoppedTermIgnoringProcess(t *testing.T) {
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	termR, termW, err := os.Pipe()
	if err != nil {
		_ = readyW.Close()
		t.Fatal(err)
	}
	defer termR.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryResetAndWaitTermIgnoringHelper$")
	cmd.Env = append(os.Environ(), "METIS_TEST_RESET_AND_WAIT_HELPER=1")
	cmd.ExtraFiles = []*os.File{readyW, termW}
	r := NewRegistry(t.TempDir())
	job, err := r.Spawn(SpawnArgs{Command: "term-ignoring-helper", Cmd: cmd})
	_ = readyW.Close()
	_ = termW.Close()
	if err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	pid := cmd.Process.Pid
	if err := readyR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(readyR, marker[:]); err != nil {
		t.Fatalf("wait for helper readiness: %v", err)
	}

	// Stop is intentionally non-blocking: it marks the job Killed while its
	// long-grace staged killer and cmd.Wait are still outstanding.
	if err := r.Stop(job.ID, time.Hour); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := termR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(termR, marker[:]); err != nil {
		t.Fatalf("helper did not observe SIGTERM: %v", err)
	}
	if snapshot, ok := r.Get(job.ID); !ok || snapshot.Status != StatusKilled {
		t.Fatalf("Stop did not leave a joinable killed job: ok=%v job=%+v", ok, snapshot)
	}
	if !pidAlive(pid) {
		t.Fatal("precondition: SIGTERM-ignoring helper exited before ResetAndWait")
	}

	r.mu.RLock()
	var oldKillDone <-chan struct{}
	for stage := range r.jobs[job.ID].killStages {
		oldKillDone = stage.done
		break
	}
	r.mu.RUnlock()
	if oldKillDone == nil {
		t.Fatal("Stop did not register a tracked kill stage")
	}
	r.ResetAndWait(0)
	select {
	case <-oldKillDone:
	default:
		t.Fatal("ResetAndWait returned before the prior Stop kill stage exited")
	}
	if cmd.ProcessState == nil {
		t.Fatal("ResetAndWait returned before cmd.Wait reaped the helper")
	}
	if pidAlive(pid) {
		t.Fatalf("ResetAndWait returned while helper pid %d was alive", pid)
	}
	if _, ok := r.Get(job.ID); ok || len(r.List()) != 0 {
		t.Fatal("ResetAndWait retained a source-session job")
	}
}

func TestRegistryResetAndWaitFencesSpawnAndAdoptUntilJoin(t *testing.T) {
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	termR, termW, err := os.Pipe()
	if err != nil {
		_ = readyW.Close()
		t.Fatal(err)
	}
	defer termR.Close()

	r := NewRegistry(t.TempDir())
	oldOut, _, err := r.NewDiskOutput()
	if err != nil {
		t.Fatal(err)
	}
	oldCmd := exec.Command(os.Args[0], "-test.run=^TestRegistryResetAndWaitTermIgnoringHelper$")
	oldCmd.Env = append(os.Environ(), "METIS_TEST_RESET_AND_WAIT_HELPER=1")
	oldCmd.ExtraFiles = []*os.File{readyW, termW}
	oldCmd.Stdout = oldOut.Writer()
	oldCmd.Stderr = oldOut.Writer()
	ApplyProcessGroup(oldCmd)
	if err := oldCmd.Start(); err != nil {
		_ = oldOut.Close()
		t.Fatal(err)
	}
	_ = readyW.Close()
	_ = termW.Close()
	waitResult := make(chan error, 1)
	if _, err := r.Adopt(AdoptArgs{
		Command:    "old-term-ignoring-helper",
		Cmd:        oldCmd,
		Output:     oldOut,
		WaitResult: waitResult,
	}); err != nil {
		KillProcessGroup(oldCmd.Process)
		_ = oldCmd.Wait()
		_ = oldOut.Close()
		t.Fatal(err)
	}
	oldReleased := false
	defer func() {
		if !oldReleased {
			KillProcessGroup(oldCmd.Process)
			waitResult <- oldCmd.Wait()
		}
	}()
	if err := readyR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(readyR, marker[:]); err != nil {
		t.Fatalf("wait for old helper readiness: %v", err)
	}

	resetDone := make(chan struct{})
	go func() {
		r.ResetAndWait(time.Hour)
		close(resetDone)
	}()
	if err := termR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(termR, marker[:]); err != nil {
		t.Fatalf("reset did not reach the old process: %v", err)
	}

	rejectedSpawn := exec.Command("sh", "-c", "exit 0")
	if _, err := r.Spawn(SpawnArgs{Command: "must-not-start", Cmd: rejectedSpawn}); !errors.Is(err, ErrRegistryResetting) {
		t.Fatalf("Spawn during ResetAndWait error = %v, want %v", err, ErrRegistryResetting)
	}
	if rejectedSpawn.Process != nil {
		t.Fatal("Spawn started a process before rejecting reset admission")
	}

	rejectedOut, _, err := r.NewDiskOutput()
	if err != nil {
		t.Fatal(err)
	}
	rejectedAdopt := exec.Command("sh", "-c", "sleep 30")
	rejectedAdopt.Stdout = rejectedOut.Writer()
	rejectedAdopt.Stderr = rejectedOut.Writer()
	ApplyProcessGroup(rejectedAdopt)
	if err := rejectedAdopt.Start(); err != nil {
		_ = rejectedOut.Close()
		t.Fatal(err)
	}
	if _, err := r.Adopt(AdoptArgs{Command: "caller-owned", Cmd: rejectedAdopt, Output: rejectedOut}); !errors.Is(err, ErrRegistryResetting) {
		KillProcessGroup(rejectedAdopt.Process)
		_ = rejectedAdopt.Wait()
		_ = rejectedOut.Close()
		t.Fatalf("Adopt during ResetAndWait error = %v, want %v", err, ErrRegistryResetting)
	}
	KillProcessGroup(rejectedAdopt.Process)
	_ = rejectedAdopt.Wait()
	_ = rejectedOut.Close()

	KillProcessGroup(oldCmd.Process)
	waitResult <- oldCmd.Wait()
	oldReleased = true
	select {
	case <-resetDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ResetAndWait did not finish after the old cmd.Wait completed")
	}

	fresh := exec.Command("sh", "-c", "echo fresh")
	freshJob, err := r.Spawn(SpawnArgs{Command: "fresh", Cmd: fresh})
	if err != nil {
		t.Fatalf("Spawn remained fenced after ResetAndWait: %v", err)
	}
	waitForStatus(t, r, freshJob.ID, StatusCompleted, 3*time.Second)
}

// countDescendants returns how many direct children of pid are still
// alive. Walks one level — sufficient because our test command is
// `bash -c '... & ... & wait'` so all sleep PIDs are direct children
// of the bash leader.
func countDescendants(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", fmt.Sprint(pid)).Output()
	if err != nil {
		// pgrep exits 1 when no matches — treat as 0.
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

// pidAlive returns true when `kill -0 pid` succeeds.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return exec.Command("kill", "-0", fmt.Sprint(pid)).Run() == nil
}

func TestKillTreeStaged_KillsBashChildren(t *testing.T) {
	// Spawn `bash -c 'sleep 30 & sleep 30 & wait'` directly via
	// exec — this mirrors what jobs.Spawn does to its caller's cmd.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", "sleep 30 & sleep 30 & wait")
	ApplyProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	leader := cmd.Process.Pid

	// Reap the process in a goroutine so the leader doesn't linger as
	// a zombie after kill (which would falsely fail the alive check —
	// `kill -0` reports zombies as alive).
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()

	// Give bash a moment to fork the two sleeps. 200ms is generous;
	// macOS on a busy CI box has been seen to need ~100ms.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countDescendants(t, leader) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := countDescendants(t, leader)
	if got < 2 {
		t.Fatalf("expected ≥2 sleep children before kill; got %d", got)
	}

	// Kill the group. 200ms grace is enough for SIGTERM to settle
	// before SIGKILL escalation; sleep doesn't trap signals so the
	// SIGTERM stage already kills it.
	done := make(chan struct{})
	killTreeStaged(cmd.Process, 200*time.Millisecond, done)
	<-done

	// Wait for the leader to be reaped. `kill -0` on a zombie returns
	// success on macOS, so we wait for cmd.Wait() (which calls wait4
	// and clears the zombie) before checking pidAlive.
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("leader didn't exit within 3s after killTreeStaged: leader=%d descendants=%d",
			leader, countDescendants(t, leader))
	}
	if pidAlive(leader) {
		t.Errorf("after Wait: leader %d still alive", leader)
	}
	if got := countDescendants(t, leader); got != 0 {
		t.Errorf("after Wait: descendants of %d = %d (want 0)", leader, got)
	}
}

func TestApplyProcessGroup_SetsSetpgid(t *testing.T) {
	cmd := exec.Command("true")
	ApplyProcessGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("Setpgid should be true; got SysProcAttr=%+v", cmd.SysProcAttr)
	}
	// Idempotent — second call must not flip back.
	ApplyProcessGroup(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Errorf("second ApplyProcessGroup flipped Setpgid back to false")
	}
}

func TestRegistryStop_TreeKillsEntireGroup(t *testing.T) {
	// End-to-end: register a sleeping bash with two sub-sleeps, stop
	// it via Registry.Stop, verify both sub-sleeps are gone.
	r := NewRegistry(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "-c", "sleep 30 & sleep 30 & wait")
	j, err := r.Spawn(SpawnArgs{
		Command: "sleep 30 & sleep 30 & wait",
		Cmd:     cmd,
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	leader := cmd.Process.Pid

	// Wait for fork-out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countDescendants(t, leader) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if countDescendants(t, leader) < 2 {
		t.Fatalf("pre-stop: bash didn't spawn 2 children")
	}

	if err := r.Stop(j.ID, 200*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait for Registry's wait goroutine to reap the leader (it
	// transitions Status to StatusKilled). Then check descendants.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := r.Get(j.ID)
		if ok && got.Status == StatusKilled && countDescendants(t, leader) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := r.Get(j.ID)
	t.Fatalf("after Registry.Stop: status=%s descendants=%d",
		got.Status, countDescendants(t, leader))
}
