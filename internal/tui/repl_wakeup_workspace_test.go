package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestREPLWakeupWorkspaceAdmission(t *testing.T) {
	for _, equivalent := range []bool{true, false} {
		name := "different-workspace"
		if equivalent {
			name = "same-workspace-alias"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			t.Setenv("METIS_NOTIFY_CHANNEL", "off")
			root := t.TempDir()
			real, alias := filepath.Join(root, "workspace"), filepath.Join(root, "alias")
			if err := os.Mkdir(real, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, alias); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			t.Chdir(real)
			storedWorkspace := alias
			if !equivalent {
				storedWorkspace = t.TempDir()
			}
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			m.providerName = "fixture"
			if err := store.WriteHeaderFull(session.Header{ID: m.sessionID, WorkDir: storedWorkspace, Provider: "fixture", Model: m.model}); err != nil {
				t.Fatal(err)
			}
			provider := &backgroundTestProvider{requests: make(chan llm.Request, 4)}
			m.loop.Provider = provider
			svc, err := agent.NewCronService(filepath.Join(root, "cron"))
			if err != nil {
				t.Fatal(err)
			}
			m.loop.Registry.Register(builtin.NewScheduleWakeup(m.gate, svc))
			r, err := NewREPL(m.loop, nil, store, m.sessionID, false, false, m.gate, m.model, "")
			if err != nil {
				t.Fatal(err)
			}
			r.out = &bytes.Buffer{}
			r.providerName = "fixture"
			r.CronService = svc
			r.bindREPLWakeupContinuation()
			tool, _ := m.loop.Registry.Get("ScheduleWakeup")
			result, err := tool.Execute(tasks.WithSessionID(context.Background(), m.sessionID), map[string]any{"delaySeconds": 30, "prompt": "authorized continuation", "reason": "workspace admission fixture"})
			if err != nil || result.IsError == equivalent {
				t.Fatalf("equivalent=%v result=%+v err=%v", equivalent, result, err)
			}
			if !equivalent {
				if err := r.consumeREPLWakeup(context.Background()); err == nil {
					t.Fatal("different workspace admitted")
				}
				if len(svc.List()) != 0 || len(provider.requests) != 0 {
					t.Fatal("different workspace created or executed continuation")
				}
				return
			}
			job := svc.List()[0]
			if err := svc.Update(job.ID, func(j *agent.CronJob) { j.NextRun = time.Now().Add(-time.Second) }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := r.consumeREPLWakeup(ctx); err != nil {
				t.Fatalf("same workspace alias rejected during consumption: %v", err)
			}
			saved, _ := svc.Get(job.ID)
			if saved.RunCount != 1 || saved.ContinuationStatus != "succeeded" || len(provider.requests) != 1 {
				t.Fatalf("continuation did not execute once: job=%+v calls=%d", saved, len(provider.requests))
			}
		})
	}
}
