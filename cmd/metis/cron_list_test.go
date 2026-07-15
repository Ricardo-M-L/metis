package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestParseCronListOptions(t *testing.T) {
	opts, err := parseCronListOptions([]string{"--json"})
	if err != nil || !opts.jsonOutput {
		t.Fatalf("parseCronListOptions() = %+v, %v", opts, err)
	}
	if _, err := parseCronListOptions([]string{"--unknown"}); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("unknown option error = %v", err)
	}
}

func TestWriteCronListJSONIsStableAndComplete(t *testing.T) {
	base := time.Date(2026, 7, 15, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	jobs := []*agent.CronJob{
		{
			ID: "later", Name: "Later", Prompt: "report", Enabled: true,
			Schedule:  agent.CronSchedule{Kind: "cron", CronExpr: "0 9 * * *", TZ: "Asia/Shanghai"},
			CreatedAt: base, LastRun: base.Add(time.Hour), NextRun: base.Add(3 * time.Hour),
			RunCount: 2, Repeat: 5, Silent: true, SessionMode: agent.SessionModePersistent,
			AllowTools: []string{"Read"}, DisabledTools: []string{"Agent"},
		},
		{
			ID: "sooner", Name: "Sooner", Prompt: "check", Enabled: true, Paused: true,
			Schedule: agent.CronSchedule{Kind: "every", EveryMs: 300000},
			NextRun:  base.Add(2 * time.Hour),
		},
		{ID: "session-only", Ephemeral: true, Enabled: true},
	}

	var out bytes.Buffer
	if err := writeCronList(&out, jobs, cronListOptions{jsonOutput: true}); err != nil {
		t.Fatal(err)
	}
	var records []cronListRecord
	if err := json.Unmarshal(out.Bytes(), &records); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(records) != 2 || records[0].ID != "sooner" || records[1].ID != "later" {
		t.Fatalf("records = %+v", records)
	}
	if records[0].SessionMode != agent.SessionModeIsolated {
		t.Fatalf("legacy session mode = %q", records[0].SessionMode)
	}
	got := records[1]
	if got.Schedule.CronExpr != "0 9 * * *" || got.NextRun != "2026-07-15T03:00:00Z" || got.LastRun != "2026-07-15T01:00:00Z" {
		t.Fatalf("timestamps/schedule = %+v", got)
	}
	if got.RunCount != 2 || got.Repeat != 5 || !got.Silent || len(got.AllowTools) != 1 || len(got.DisabledTools) != 1 {
		t.Fatalf("metadata = %+v", got)
	}
}

func TestWriteCronListJSONEmptyUsesArray(t *testing.T) {
	var out bytes.Buffer
	if err := writeCronList(&out, nil, cronListOptions{jsonOutput: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("empty JSON = %q, want []", got)
	}
}

func TestWriteCronListHumanEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := writeCronList(&out, nil, cronListOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "(no cron jobs)" {
		t.Fatalf("empty list = %q", got)
	}
}
