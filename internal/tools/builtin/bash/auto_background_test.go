package bash

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAutoBackgroundPromotionHasSingleWaitOwner(t *testing.T) {
	oldThreshold := AutoBackgroundThreshold
	AutoBackgroundThreshold = 25 * time.Millisecond
	t.Cleanup(func() { AutoBackgroundThreshold = oldThreshold })

	pool := jobs.NewRegistry(t.TempDir())
	b := Bash{gate: permission.New(permission.ModeBypass), Jobs: pool}
	res, err := b.Execute(context.Background(), map[string]any{
		"command":     `printf 'promotion-start\n'; sleep 0.20; printf 'promotion-done\n'`,
		"description": "exercise automatic background promotion",
		"timeout_ms":  float64(2_000),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || res.IsError || !strings.Contains(res.Output, "moved to background") {
		t.Fatalf("promotion result = %+v", res)
	}

	listed := pool.List()
	if len(listed) != 1 {
		t.Fatalf("jobs after promotion = %+v, want one", listed)
	}
	id := listed[0].ID

	deadline := time.Now().Add(3 * time.Second)
	var final jobs.Job
	for time.Now().Before(deadline) {
		var ok bool
		final, ok = pool.Get(id)
		if ok && final.Status != jobs.StatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Status != jobs.StatusCompleted || final.ExitCode != 0 {
		t.Fatalf("promoted job terminal state = %s exit=%d; double Wait usually reports failed/-1",
			final.Status, final.ExitCode)
	}
	body, err := jobs.ReadJobOutput(final.OutputPath, 0)
	if err != nil {
		t.Fatalf("ReadJobOutput: %v", err)
	}
	if !strings.Contains(body, "promotion-start") || !strings.Contains(body, "promotion-done") {
		t.Fatalf("promoted output incomplete: %q", body)
	}
}

func TestAutoBackgroundAdoptRejectedByStrictResetCleansOrphan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell to hold the foreground command")
	}

	oldThreshold := AutoBackgroundThreshold
	AutoBackgroundThreshold = 20 * time.Millisecond
	t.Cleanup(func() { AutoBackgroundThreshold = oldThreshold })

	root := t.TempDir()
	pool := jobs.NewRegistry(root)

	// Keep ResetAndWait inside its strict admission window with a real adopted
	// command whose sole Wait result is deliberately withheld. The process may
	// already have exited, but Registry cannot finish the lifecycle join until
	// its caller-owned Wait result is delivered.
	oldOut, _, err := pool.NewDiskOutput()
	if err != nil {
		t.Fatal(err)
	}
	oldCmd := exec.Command("/bin/sh", "-c", "exit 0")
	jobs.ApplyProcessGroup(oldCmd)
	if err := oldCmd.Start(); err != nil {
		_ = oldOut.Close()
		t.Fatal(err)
	}
	heldWait := make(chan error, 1)
	if _, err := pool.Adopt(jobs.AdoptArgs{
		Command:    "hold strict reset join",
		Cmd:        oldCmd,
		Output:     oldOut,
		WaitResult: heldWait,
	}); err != nil {
		jobs.KillProcessGroup(oldCmd.Process)
		_ = oldCmd.Wait()
		_ = oldOut.Close()
		t.Fatal(err)
	}

	var releaseOld sync.Once
	releaseReset := func() {
		releaseOld.Do(func() {
			jobs.KillProcessGroup(oldCmd.Process)
			heldWait <- oldCmd.Wait()
		})
	}
	defer releaseReset()

	resetDone := make(chan struct{})
	go func() {
		pool.ResetAndWait(0)
		close(resetDone)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for len(pool.List()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if listed := pool.List(); len(listed) != 0 {
		t.Fatalf("strict reset did not detach its source generation: %+v", listed)
	}
	probe := exec.Command("/bin/sh", "-c", "exit 99")
	if _, err := pool.Spawn(jobs.SpawnArgs{Command: "reset admission probe", Cmd: probe}); !errors.Is(err, jobs.ErrRegistryResetting) {
		t.Fatalf("Spawn during strict reset = %v, want %v", err, jobs.ErrRegistryResetting)
	}
	if probe.Process != nil {
		t.Fatal("strict reset admission probe unexpectedly started")
	}

	jobDir := filepath.Join(root, "jobs")
	filesBefore := dirNames(t, jobDir)
	fdsBefore, haveFDCount := openFDCount()

	started := filepath.Join(root, "foreground-started")
	release := filepath.Join(root, "foreground-release")
	defer func() { _ = os.WriteFile(release, []byte("cleanup"), 0o600) }()
	command := "printf 'reset-fallback-start\\n'; : > " + shellQuoteForTest(started) +
		"; while [ ! -f " + shellQuoteForTest(release) +
		" ]; do sleep 0.01; done; printf 'reset-fallback-done\\n'"
	type executeResult struct {
		result *tools.Result
		err    error
	}
	executeDone := make(chan executeResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		result, err := (Bash{gate: permission.New(permission.ModeBypass), Jobs: pool}).Execute(ctx, map[string]any{
			"command":     command,
			"description": "exercise rejected automatic promotion",
			"timeout_ms":  float64(3_000),
		})
		executeDone <- executeResult{result: result, err: err}
	}()

	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat foreground marker: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground command did not start")
		}
		time.Sleep(time.Millisecond)
	}

	// The timer must get enough time to attempt promotion. A successful Adopt
	// would return a background job immediately; rejection must instead retain
	// foreground ownership and keep waiting for the command to finish.
	select {
	case got := <-executeDone:
		t.Fatalf("Execute returned before foreground release; strict Adopt was not rejected: result=%+v err=%v", got.result, got.err)
	case <-time.After(10 * AutoBackgroundThreshold):
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got executeResult
	select {
	case got = <-executeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not return after releasing the foreground command")
	}
	if got.err != nil {
		t.Fatalf("Execute: %v", got.err)
	}
	if got.result == nil || got.result.IsError {
		t.Fatalf("foreground fallback result = %+v", got.result)
	}
	if strings.Contains(got.result.Output, "moved to background") ||
		!strings.Contains(got.result.Output, "reset-fallback-start") ||
		!strings.Contains(got.result.Output, "reset-fallback-done") {
		t.Fatalf("foreground fallback output = %q", got.result.Output)
	}
	if listed := pool.List(); len(listed) != 0 {
		t.Fatalf("rejected promotion published a job: %+v", listed)
	}

	if filesAfter := dirNames(t, jobDir); !slices.Equal(filesAfter, filesBefore) {
		t.Fatalf("rejected promotion leaked an output file: before=%v after=%v", filesBefore, filesAfter)
	}
	if haveFDCount {
		deadline = time.Now().Add(time.Second)
		for {
			fdsAfter, ok := openFDCount()
			if !ok || fdsAfter <= fdsBefore {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("rejected promotion leaked an open file descriptor: before=%d after=%d", fdsBefore, fdsAfter)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	releaseReset()
	select {
	case <-resetDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ResetAndWait did not finish after its held Wait result was released")
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func openFDCount() (int, bool) {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return len(entries), true
		}
	}
	return 0, false
}
