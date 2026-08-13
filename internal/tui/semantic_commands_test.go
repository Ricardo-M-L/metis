package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
)

func TestQuickNamesItsActualRequestShaping(t *testing.T) {
	r := &REPL{Loop: &agent.Loop{}}
	if out := cmdQuick(r, "on"); !r.Loop.FastEnabled() || !strings.Contains(out, "effort=low") || strings.Contains(out, "provider") {
		t.Fatalf("quick on = %q Fast=%v", out, r.Loop.FastEnabled())
	}
	if out := cmdQuick(r, "off"); r.Loop.FastEnabled() || !strings.Contains(out, "off") {
		t.Fatalf("quick off = %q Fast=%v", out, r.Loop.FastEnabled())
	}
}

func TestBackgroundTaskRenderingNeutralizesTerminalControls(t *testing.T) {
	job := jobs.Job{
		ID:          "bg_safe",
		Description: "build \x1b]52;c;Y2xpcA==\x07 \x9b2J done",
		Status:      jobs.StatusCompleted,
		StartTime:   time.Unix(1, 0),
		EndTime:     time.Unix(2, 0),
	}
	got := renderBackgroundTaskRow(job, job.EndTime) + "\n" + renderBackgroundTaskOutput(job,
		"first line\n\x1b[2Jsecond\x1b]52;c;Y2xpcA==\x07\x9b2J")

	for _, r := range got {
		if r != '\n' && (r <= 0x1f || (r >= 0x7f && r <= 0x9f)) {
			t.Fatalf("unsafe terminal control U+%04X reached /tasks output: %q", r, got)
		}
	}
	for _, visible := range []string{"first line\n", "second", `\x1b`, `\x07`, `\x9b`, "]52;c;Y2xpcA=="} {
		if !strings.Contains(got, visible) {
			t.Fatalf("sanitized task output lost %q: %q", visible, got)
		}
	}
}

func TestBackgroundTasksUsesRegisteredJobsOnly(t *testing.T) {
	r := &REPL{Loop: &agent.Loop{Jobs: jobs.NewRegistry(t.TempDir())}}
	out := cmdBackgroundTasks(r, "list")
	if !strings.Contains(out, "no background jobs") {
		t.Fatalf("empty tasks = %q", out)
	}
	if out := cmdBackgroundTasks(r, "stop not-real"); !strings.Contains(out, "unknown id") {
		t.Fatalf("unknown stop = %q", out)
	}
}

func TestSessionShareStatusDoesNotInventRemoteURL(t *testing.T) {
	stopBridge()
	out := cmdSessionShare(&REPL{}, "status")
	if !strings.Contains(out, "local only") || !strings.Contains(out, "/session start") {
		t.Fatalf("session status = %q", out)
	}
}
