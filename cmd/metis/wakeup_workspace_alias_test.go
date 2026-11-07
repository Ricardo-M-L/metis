package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// The persisted header may use a symlink (including macOS /var), while
// os.Getwd returns another spelling of the same authorized workspace.
func TestHeadlessWakeupAcceptsCanonicalEquivalentRestoredWorkspace(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "workspace")
	alias := filepath.Join(root, "workspace-alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(real)
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{ID: "original", WorkDir: alias, Provider: "fixture", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	svc, err := agent.NewCronService(filepath.Join(root, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	rt := &runtime{store: store, cronSvc: svc, gate: gate, registry: tools.NewRegistry(), sessionID: "original", providerName: "fixture", model: "fixture"}
	rt.registry.Register(builtin.NewScheduleWakeup(gate, svc))
	rt.bindWakeupContinuation()
	tool, _ := rt.registry.Get("ScheduleWakeup")
	result, err := tool.Execute(tasks.WithSessionID(context.Background(), "original"), map[string]any{"delaySeconds": 30, "prompt": "fixture continuation", "reason": "same authorized workspace"})
	if err != nil || result.IsError {
		t.Fatalf("same canonical workspace rejected: result=%+v err=%v", result, err)
	}
	job := svc.List()[0]
	if err := svc.Update(job.ID, func(j *agent.CronJob) { j.NextRun = time.Now().Add(-time.Second) }); err != nil {
		t.Fatal(err)
	}
	run, err := awaitHeadlessWakeup(context.Background(), rt)
	if err != nil || run == nil {
		t.Fatalf("same canonical workspace not admitted: run=%+v err=%v", run, err)
	}
	if err := run.Finish("cancelled", "fixture admission only", "", context.Canceled); err != nil {
		t.Fatal(err)
	}
}
