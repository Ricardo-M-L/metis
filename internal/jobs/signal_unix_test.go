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
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
