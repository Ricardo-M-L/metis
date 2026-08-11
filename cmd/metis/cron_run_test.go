package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestAdvanceManualCronRunPersistsBookkeeping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
		if err != nil {
			t.Fatal(err)
		}
		job := &agent.CronJob{
			ID: "manual-job", Name: "Manual", Prompt: "check", Enabled: true,
			Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
			Repeat:   2,
		}
		if err := svc.Create(job); err != nil {
			t.Fatal(err)
		}
		before := job.NextRun
		// Advance the synthetic wall clock so the assertion does not depend on
		// the host OS clock tick (consecutive Windows reads may be identical).
		time.Sleep(2 * time.Millisecond)
		got, err := advanceManualCronRun(svc, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RunCount != 1 || got.LastRun.IsZero() || !got.NextRun.After(before) || !got.Enabled {
			t.Fatalf("bookkeeping after first run = %+v (previous next %v)", got, before)
		}
		got, err = advanceManualCronRun(svc, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RunCount != 2 || got.Enabled {
			t.Fatalf("repeat limit after second run = %+v", got)
		}
	})
}

func TestAdvanceManualCronRunRejectsMissingJob(t *testing.T) {
	svc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := advanceManualCronRun(svc, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing job error = %v", err)
	}
}

func TestReportCronFireErrorIsVisible(t *testing.T) {
	want := errors.New("provider unavailable")
	var out bytes.Buffer
	if got := reportCronFireError(&out, &agent.CronJob{ID: "job-7"}, want); !errors.Is(got, want) {
		t.Fatalf("returned error = %v", got)
	}
	if text := out.String(); !strings.Contains(text, "[cron] job job-7 failed: provider unavailable") {
		t.Fatalf("stderr = %q", text)
	}
	out.Reset()
	if got := reportCronFireError(&out, nil, nil); got != nil || out.Len() != 0 {
		t.Fatalf("success emitted output: %q, %v", out.String(), got)
	}
}
