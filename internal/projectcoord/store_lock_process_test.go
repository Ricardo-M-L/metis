package projectcoord

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The helpers execute the test binary as independent OS processes. In
// particular, killing the lock holder cannot run its deferred release.
func TestProjectCoordinatorLockProcessHelper(t *testing.T) {
	action := os.Getenv("METIS_COORD_LOCK_PROCESS_ACTION")
	if action == "" {
		return
	}
	store := NewStore(os.Getenv("METIS_COORD_LOCK_PROCESS_ROOT"))
	workspace := os.Getenv("METIS_COORD_LOCK_PROCESS_WORKSPACE")
	runID := os.Getenv("METIS_COORD_LOCK_PROCESS_RUN")
	if action == "hold" {
		release, err := store.lockRun(workspace, runID)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer release()
		fmt.Fprintln(os.Stdout, "locked")
		time.Sleep(time.Hour)
		return
	}
	// All contender processes must exist before the parent releases the gate.
	fmt.Fprintln(os.Stdout, "ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var result any
	switch action {
	case "claim":
		item, err := store.ClaimNext(workspace, runID, ClaimOptions{Worker: fmt.Sprintf("process-%d", os.Getpid())})
		if errors.Is(err, ErrNoReadyWork) {
			result = map[string]bool{"idle": true}
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		} else {
			result = item
		}
	case "event":
		item, err := store.EnqueueEvent(workspace, runID, EventInput{Source: "process-test", ID: "same-event", Subject: "Process event", Prompt: "Record durable work"})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		result = item
	default:
		fmt.Fprintln(os.Stderr, "unknown helper action", action)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type coordinatorLockProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr bytes.Buffer
}

func startCoordinatorLockProcess(t *testing.T, action, root, workspace, runID string) *coordinatorLockProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &coordinatorLockProcess{cmd: exec.Command(executable, "-test.run=^TestProjectCoordinatorLockProcessHelper$")}
	p.cmd.Env = append(os.Environ(), "METIS_COORD_LOCK_PROCESS_ACTION="+action, "METIS_COORD_LOCK_PROCESS_ROOT="+root, "METIS_COORD_LOCK_PROCESS_WORKSPACE="+workspace, "METIS_COORD_LOCK_PROCESS_RUN="+runID)
	p.cmd.Stderr = &p.stderr
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.stdout = bufio.NewReader(out)
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := p.stdout.ReadString('\n'); ready <- line }()
	want := "ready\n"
	if action == "hold" {
		want = "locked\n"
	}
	select {
	case line := <-ready:
		if line != want {
			_ = p.cmd.Wait()
			t.Fatalf("helper readiness = %q, want %q: %s", line, want, p.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper process did not become ready")
	}
	return p
}

func TestCoordinatorLockReleasesAfterHolderProcessKilled(t *testing.T) {
	workspace, root := t.TempDir(), filepath.Join(t.TempDir(), "state")
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Recover a killed lock holder"})
	if err != nil {
		t.Fatal(err)
	}
	holder := startCoordinatorLockProcess(t, "hold", root, run.Workspace, run.ID)
	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := holder.cmd.Wait(); err == nil {
		t.Fatal("killed helper process exited successfully")
	}
	started := time.Now()
	release, err := NewStore(root).lockRun(run.Workspace, run.ID)
	if err != nil {
		t.Fatalf("new process could not acquire the killed holder's lock after %s: %v", time.Since(started), err)
	}
	defer release()
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("killed holder's lock took %s to recover, want less than 3s", elapsed)
	}
}

func TestCoordinatorLockDoesNotStealOldLiveProcessLock(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Preserve a live old lock"})
	if err != nil {
		t.Fatal(err)
	}
	_ = startCoordinatorLockProcess(t, "hold", root, run.Workspace, run.ID)
	dir, err := store.projectDir(run.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".locks", run.ID+".lock")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	contender := NewStore(root)
	contender.lockWait = 80 * time.Millisecond
	release, err := contender.lockRun(run.Workspace, run.ID)
	if err == nil {
		release()
		t.Fatal("another live process's old lock was stolen")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("live lock contention error = %v, want timeout", err)
	}
}

func TestCoordinatorLockKeepsStablePrivateFile(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Keep one lock inode"})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.projectDir(run.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".locks", run.ID+".lock")
	var original os.FileInfo
	for i := 0; i < 3; i++ {
		release, err := store.lockRun(run.Workspace, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		release()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("lock is not a regular file: %v", info.Mode())
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("lock permissions = %o, want 600", info.Mode().Perm())
		}
		if original == nil {
			original = info
		} else if !os.SameFile(original, info) {
			t.Fatal("release/acquire replaced the lock inode")
		}
	}
}

func TestCoordinatorLockRefusesLegacyDirectoryWithoutDeletingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "old-worker")
	if err := os.WriteFile(marker, []byte("still running"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProjectCoordinatorLock(path, time.Second); err == nil || !strings.Contains(err.Error(), "stop old coordinator workers") {
		t.Fatalf("legacy lock error = %v, want actionable refusal", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "still running" {
		t.Fatalf("legacy lock contents changed: %q, %v", data, err)
	}
}

func TestCoordinatorLockRefusesSymlinkAndPreservesTarget(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "linked.lock")
	if err := os.WriteFile(target, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := acquireProjectCoordinatorLock(link, time.Second); err == nil {
		t.Fatal("symlinked lock accepted")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "preserve" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("symlink target permissions changed: %v, %v", info, err)
		}
	}
}

func runCoordinatorContenderProcesses(t *testing.T, action, root, workspace, runID string, count int) [][]byte {
	t.Helper()
	processes := make([]*coordinatorLockProcess, count)
	for i := range processes {
		processes[i] = startCoordinatorLockProcess(t, action, root, workspace, runID)
	}
	var gate sync.WaitGroup
	for _, p := range processes {
		gate.Add(1)
		go func(p *coordinatorLockProcess) { defer gate.Done(); _, _ = io.WriteString(p.stdin, "go\n") }(p)
	}
	gate.Wait()
	results := make([][]byte, count)
	for i, p := range processes {
		var err error
		results[i], err = io.ReadAll(p.stdout)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("contender process failed: %v: %s", err, p.stderr.String())
		}
	}
	return results
}

func TestSeparateProcessesClaimExactlyOneWorkItem(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Atomic independent process claims"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, idle := 0, 0
	for _, result := range runCoordinatorContenderProcesses(t, "claim", root, run.Workspace, run.ID, 12) {
		var payload struct {
			ID    string `json:"id"`
			Idle  bool   `json:"idle"`
			Lease *Lease `json:"lease"`
		}
		if err := json.Unmarshal(result, &payload); err != nil {
			t.Fatalf("helper JSON: %v: %s", err, result)
		}
		if payload.Idle {
			idle++
		} else if payload.ID == "research" && payload.Lease != nil && payload.Lease.Token != "" {
			claimed++
		} else {
			t.Fatalf("unexpected claim result: %s", result)
		}
	}
	if claimed != 1 || idle != 11 {
		t.Fatalf("separate-process claims: claimed=%d idle=%d, want 1/11", claimed, idle)
	}
	after, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item := itemByID(t, after, "research"); item.Attempts != 1 || item.Status != ItemRunning {
		t.Fatalf("durable winner = %+v", item)
	}
}

func TestSeparateProcessesDeduplicateOneExternalEvent(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Atomic independent process event ingestion"})
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, result := range runCoordinatorContenderProcesses(t, "event", root, run.Workspace, run.ID, 12) {
		var item WorkItem
		if err := json.Unmarshal(result, &item); err != nil {
			t.Fatalf("helper JSON: %v: %s", err, result)
		}
		if id == "" {
			id = item.ID
		}
		if item.ID != id || item.EventID != "same-event" {
			t.Fatalf("event contender returned a different event: %+v", item)
		}
	}
	after, err := NewStore(root).Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || len(after.Items) != len(run.Items)+2 {
		t.Fatalf("duplicate event graph: id=%q items=%d, want %d", id, len(after.Items), len(run.Items)+2)
	}
}
