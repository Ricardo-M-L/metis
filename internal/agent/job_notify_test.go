package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

func TestFormatJobNotificationsRedactsCommandCredentials(t *testing.T) {
	got := formatJobNotifications([]jobs.Notification{{
		JobID:  "bg_secret",
		Status: jobs.StatusCompleted,
		Command: `curl -H 'Authorization: Bearer background-super-secret' ` +
			`https://example.test?api_key=second-super-secret`,
	}})
	for _, secret := range []string{"background-super-secret", "second-super-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("formatted notification leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("formatted notification did not retain a redaction marker: %s", got)
	}
}

func TestFormatAwaitedJobNotificationIncludesRedactedOutput(t *testing.T) {
	got := formatJobNotificationsWithOutputs([]jobs.Notification{{
		JobID:    "bg_waited",
		Status:   jobs.StatusCompleted,
		ExitCode: 0,
		Elapsed:  3 * time.Second,
		Command:  "sleep 3; print marker",
	}}, map[string]string{
		"bg_waited": "WAIT_MARKER\nAuthorization: Bearer output-super-secret",
	})
	if !strings.Contains(got, `<job_output job_id="bg_waited">`) ||
		!strings.Contains(got, "WAIT_MARKER") {
		t.Fatalf("awaited output missing from notification: %s", got)
	}
	if strings.Contains(got, "output-super-secret") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("awaited output was not redacted: %s", got)
	}
	if !strings.Contains(got, "do not call BashOutput") {
		t.Fatalf("notification did not explain that output is already available: %s", got)
	}
}

func TestReadAwaitedJobOutputsFromCompletedRegistryJob(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := pool.Spawn(jobs.SpawnArgs{
		Command: "printf REAL_WAIT_OUTPUT",
		Cmd:     exec.CommandContext(ctx, "sh", "-c", "printf REAL_WAIT_OUTPUT"),
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	select {
	case notification := <-pool.Notify():
		if notification.JobID != job.ID || notification.Status != jobs.StatusCompleted {
			t.Fatalf("unexpected notification: %#v", notification)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test background job")
	}

	loop := &Loop{Jobs: pool}
	outputs := loop.readAwaitedJobOutputs(map[string]struct{}{job.ID: {}})
	if got := outputs[job.ID]; got != "REAL_WAIT_OUTPUT" {
		t.Fatalf("captured output = %q, want REAL_WAIT_OUTPUT", got)
	}
}
