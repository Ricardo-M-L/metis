package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestCronRunArgs(t *testing.T) {
	const id = "07c2dc82-93ab-4a31-9c60-1b569c898179"
	for _, args := range [][]string{{"job"}, {"job", "--run-id", id}, {"job", "--run-id=" + id}} {
		job, run, err := parseCronRunArgs(args)
		if err != nil || job != "job" || len(args) > 1 && run != id {
			t.Fatalf("args=%v got=%s %s %v", args, job, run, err)
		}
	}
	for _, args := range [][]string{nil, {"job", "--run-id"}, {"job", "--run-id", "../bad"}, {"job", "--wat"}, {"job", "--run-id", id, "--run-id", id}} {
		if _, _, err := parseCronRunArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRecordedCronStartupFailureAndBusyBookkeeping(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cron")
	svc, err := agent.NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := &agent.CronJob{ID: "job", Name: "startup", Prompt: "test", Enabled: true, Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)}}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	original := setupCronRuntime
	t.Cleanup(func() { setupCronRuntime = original })
	setupCronRuntime = func(_ context.Context, flags *cliFlags) (*runtime, error) {
		if flags.mode != string(permission.ModeDefault) || flags.cronSessionDir != filepath.Dir(root) {
			t.Fatalf("unsafe flags=%+v", flags)
		}
		return nil, errors.New("provider rejected API_KEY=secret-sentinel")
	}
	run, err := agent.BeginCronRun(root, job.ID, "held", "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	err = runRecordedCronJob(context.Background(), svc, job, "busy", "manual", nil, nil)
	if !errors.Is(err, agent.ErrCronJobRunning) {
		t.Fatalf("busy=%v", err)
	}
	current, _ := svc.Get(job.ID)
	if current.RunCount != 0 {
		t.Fatal("busy attempt advanced schedule")
	}
	_ = run.Finish("cancelled", "", "", nil)
	err = runRecordedCronJob(context.Background(), svc, job, "failed-setup", "manual", nil, nil)
	if err == nil {
		t.Fatal("setup succeeded unexpectedly")
	}
	r, err := agent.ReadCronRun(root, job.ID, "failed-setup")
	if err != nil || r.Status != "failed" || r.SessionID != "" || r.FinishedAt == nil {
		t.Fatalf("record=%+v %v", r, err)
	}
	if strings.Contains(r.Error, "secret-sentinel") {
		t.Fatal("startup error leaked credential")
	}
}

func TestExecuteCronJobSavesActualConversation(t *testing.T) {
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")
	rt, _ := newCronPermissionRuntime(t, "ReadOnlyCronLookup", map[string]any{"query": "status"}, tools.PermissionAllow, true, t.TempDir())
	store, err := session.NewStore(rt.cfg.Session.Dir)
	if err != nil {
		t.Fatal(err)
	}
	rt.store = store
	if err := store.WriteHeader(rt.sessionID, "test", "system"); err != nil {
		t.Fatal(err)
	}
	job := &agent.CronJob{ID: "saved", Prompt: "check status", Silent: true}
	root := t.TempDir()
	run, err := agent.BeginCronRun(root, job.ID, "visible-run", "manual")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Finish("cancelled", "", "", nil) })
	if err := run.SetSessionID(rt.sessionID); err != nil {
		t.Fatal(err)
	}
	if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}, run); err != nil {
		t.Fatal(err)
	}
	progress, err := agent.ReadCronRun(root, job.ID, "visible-run")
	if err != nil || progress.Status != "running" || !strings.Contains(progress.LiveText, "done") || len(progress.Activity) == 0 {
		t.Fatalf("live progress unavailable before terminal record: %+v, %v", progress, err)
	}
	_, history, err := store.Load(rt.sessionID)
	if err != nil || len(history) < 2 {
		t.Fatalf("saved=%d %v", len(history), err)
	}
	if got := cronFinalOutput(history); got != "done" {
		t.Fatalf("saved final=%q", got)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, rt.sessionID+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish("succeeded", "done", "done", nil); err != nil {
		t.Fatal(err)
	}
	final, err := agent.ReadCronRun(root, job.ID, "visible-run")
	if err != nil || final.Status != "succeeded" || final.LiveText != "" || final.Output != "done" {
		t.Fatalf("terminal result=%+v, %v", final, err)
	}
}

func TestCronFinalOutputDoesNotExposeReasoningOrPreviousTurn(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "old output"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new task"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "thinking", Text: "private reasoning"}}},
	}
	if got := cronFinalOutput(history); got != "" {
		t.Fatalf("leaked prior/reasoning=%q", got)
	}
}

func TestRecordedCronWorkspaceAndRemovedSchedule(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cron")
	svc, err := agent.NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	job := &agent.CronJob{ID: "bound", Name: "workspace", WorkDir: workspace, Prompt: "test", Enabled: true, Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)}}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	original := setupCronRuntime
	t.Cleanup(func() { setupCronRuntime = original })
	calls := 0
	setupCronRuntime = func(_ context.Context, flags *cliFlags) (*runtime, error) {
		calls++
		cwd, err := os.Getwd()
		if err != nil || cwd != workspace {
			t.Fatalf("workspace=%s %v", cwd, err)
		}
		if flags.cronSessionDir != filepath.Dir(root) {
			t.Fatal("workspace displaced durable store")
		}
		return nil, errors.New("setup fixture failure")
	}
	if err := runRecordedCronJob(context.Background(), svc, job, "bound-run", "scheduled", nil, nil); err == nil {
		t.Fatal("expected setup failure")
	}
	if cwd, _ := os.Getwd(); cwd != originalCWD {
		t.Fatalf("cwd not restored: %s", cwd)
	}
	if err := agent.WithCronJobIdle(root, job.ID, func() error { return svc.Remove(job.ID) }); err != nil {
		t.Fatal(err)
	}
	if err := runRecordedCronJob(context.Background(), svc, job, "deleted-run", "scheduled", nil, nil); err == nil {
		t.Fatal("deleted snapshot executed")
	}
	if calls != 1 {
		t.Fatalf("deleted job initialized runtime: calls=%d", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	if err := runRecordedCronJob(ctx, svc, job, "cancelled-run", "scheduled", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	r, err := agent.ReadCronRun(root, job.ID, "cancelled-run")
	if err != nil || r.Status != "cancelled" {
		t.Fatalf("cancelled record=%+v %v", r, err)
	}
}
