package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCronRunsLifecyclePrivacyAndAdmission(t *testing.T) {
	root := t.TempDir()
	run, err := BeginCronRun(root, "job", "first", "manual")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Finish("cancelled", "", "", nil) })
	if err := run.SetSessionID("real-session"); err != nil {
		t.Fatal(err)
	}
	if running, err := CronJobRunning(root, "job"); err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	if err := WithCronJobIdle(root, "job", func() error { t.Fatal("active run was deletable"); return nil }); !errors.Is(err, ErrCronJobRunning) {
		t.Fatal(err)
	}
	if _, err := BeginCronRun(root, "job", "second", "scheduled"); !errors.Is(err, ErrCronJobRunning) {
		t.Fatalf("overlap=%v", err)
	}
	for i := 0; i < 3; i++ {
		r, err := ReadCronRun(root, "job", "first")
		if err != nil || r.Status != "running" {
			t.Fatalf("live record=%+v %v", r, err)
		}
	}
	skipped, err := ReadCronRun(root, "job", "second")
	if err != nil || skipped.Status != "skipped" || skipped.FinishedAt == nil {
		t.Fatalf("skip=%+v %v", skipped, err)
	}
	output := "API_KEY=cron-secret-sentinel\n" + strings.Repeat("已完成", 20000)
	if err := run.Finish("succeeded", "API_KEY=cron-secret-sentinel", output, nil); err != nil {
		t.Fatal(err)
	}
	r, err := ReadCronRun(root, "job", "first")
	if err != nil || r.Status != "succeeded" || r.SessionID != "real-session" || r.FinishedAt == nil {
		t.Fatalf("result=%+v %v", r, err)
	}
	if strings.Contains(r.Output+r.Summary, "cron-secret-sentinel") || len(r.Output) > CronRunOutputLimit || !strings.HasSuffix(r.Output, "[truncated]") {
		t.Fatal("output is not bounded/redacted")
	}
	if running, err := CronJobRunning(root, "job"); err != nil || running {
		t.Fatalf("finished running=%v %v", running, err)
	}
	if _, err := BeginCronRun(root, "job", "first", "manual"); err == nil {
		t.Fatal("duplicate run replaced history")
	}
	if runtime.GOOS != "windows" {
		for _, rel := range []string{"runs", "runs/job", "runs/job/first.json"} {
			st, err := os.Stat(filepath.Join(root, rel))
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm()&0o077 != 0 {
				t.Fatalf("public record: %s %v", rel, st.Mode())
			}
		}
	}
}

func TestCronRunsRejectUnsafeStorage(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"../other", "", "..", "a/b", `a\b`} {
		if _, err := BeginCronRun(root, id, "run", "manual"); err == nil {
			t.Fatalf("accepted job id %q", id)
		}
	}
	if _, err := BeginCronRun(root, "job", "../escape", "manual"); err == nil {
		t.Fatal("accepted unsafe run id")
	}
	if runtime.GOOS == "windows" {
		return
	}
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginCronRun(root, "job", "run", "manual"); err == nil {
		t.Fatal("followed runs symlink")
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatal("wrote outside storage")
	}
}

func TestCronRunsRetentionAndListBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < CronRunRetention+3; i++ {
		run, err := BeginCronRun(root, "job", fmt.Sprintf("run-%03d", i), "manual")
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Finish("succeeded", "ok", "full output", nil); err != nil {
			t.Fatal(err)
		}
	}
	records, err := ListCronRuns(root, "job", 1000)
	if err != nil || len(records) != CronRunRetention {
		t.Fatalf("count=%d err=%v", len(records), err)
	}
	if records[0].ID != "run-102" || records[0].Output != "" {
		t.Fatalf("list=%+v", records[0])
	}
	limited, err := ListCronRuns(root, "job", 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit=%d err=%v", len(limited), err)
	}
	if _, err := ReadCronRun(root, "job", "run-000"); !os.IsNotExist(err) {
		t.Fatalf("old record=%v", err)
	}
}

func TestCronRunsCrashRecoveryUsesLockOwnership(t *testing.T) {
	if os.Getenv("METIS_CRON_RUN_TEST_CHILD") == "1" {
		run, err := BeginCronRun(os.Getenv("METIS_CRON_RUN_TEST_ROOT"), "job", "crashed", "scheduled")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_ = run
		fmt.Println("ready")
		for {
			time.Sleep(time.Second)
		}
	}
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCronRunsCrashRecoveryUsesLockOwnership$")
	cmd.Env = append(os.Environ(), "METIS_CRON_RUN_TEST_CHILD=1", "METIS_CRON_RUN_TEST_ROOT="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "ready" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child failed before admission")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child admission timed out")
	}
	r, err := ReadCronRun(root, "job", "crashed")
	if err != nil || r.Status != "running" {
		t.Fatalf("live child=%+v %v", r, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	r, err = ReadCronRun(root, "job", "crashed")
	if err != nil || r.Status != "interrupted" || r.FinishedAt == nil {
		t.Fatalf("dead child=%+v %v", r, err)
	}
	next, err := BeginCronRun(root, "job", "next", "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Finish("succeeded", "", "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCronRunsInspectorDoesNotCauseFalseOverlap(t *testing.T) {
	root := t.TempDir()
	reading, release, readDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		readDone <- withRecoveredCronRuns(root, "job", func(string) error { close(reading); <-release; return nil })
	}()
	<-reading
	type result struct {
		run *CronRun
		err error
	}
	admitted := make(chan result, 1)
	go func() { run, err := BeginCronRun(root, "job", "first", "manual"); admitted <- result{run, err} }()
	// An inspector is holding both locks. Admission waits for the short
	// records transaction rather than classifying that reader as a live job.
	select {
	case got := <-admitted:
		close(release)
		if got.run != nil {
			_ = got.run.Finish("cancelled", "", "", nil)
		}
		t.Fatalf("admission escaped reader transaction: %v", got.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-admitted:
		if got.err != nil {
			t.Fatalf("reader caused false overlap: %v", got.err)
		}
		if err := got.run.Finish("succeeded", "", "", nil); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission deadlocked after reader exited")
	}
}
