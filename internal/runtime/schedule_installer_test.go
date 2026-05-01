package runtime

import (
	"strings"
	"testing"
)

func TestValidScheduleID(t *testing.T) {
	good := []string{"a", "loop-1", "job_alpha.42", "X"}
	for _, s := range good {
		if !validScheduleID(s) {
			t.Errorf("validScheduleID(%q) should be true", s)
		}
	}
	bad := []string{"", "with space", "a/b", strings.Repeat("a", 65)}
	for _, s := range bad {
		if validScheduleID(s) {
			t.Errorf("validScheduleID(%q) should be false", s)
		}
	}
}

func TestCronToCalendarInterval(t *testing.T) {
	out, err := cronToCalendarInterval("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<key>Hour</key>") || !strings.Contains(out, "<integer>9</integer>") {
		t.Errorf("missing hour=9 in:\n%s", out)
	}
	if !strings.Contains(out, "<key>Minute</key>") || !strings.Contains(out, "<integer>0</integer>") {
		t.Errorf("missing minute=0")
	}
	// "*" fields should NOT emit a key.
	if strings.Contains(out, "<key>Day</key>") {
		t.Errorf("Day should be absent for `*` field:\n%s", out)
	}
}

func TestCronToCalendarInterval_StarSlashN(t *testing.T) {
	out, err := cronToCalendarInterval("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<key>Minute</key>") || !strings.Contains(out, "<integer>15</integer>") {
		t.Errorf("`*/15` should approximate to minute=15:\n%s", out)
	}
}

func TestCronToCalendarInterval_RejectsBadShape(t *testing.T) {
	if _, err := cronToCalendarInterval("0 9"); err == nil {
		t.Errorf("3-field cron should error")
	}
}

func TestCronToOnCalendar(t *testing.T) {
	out, err := cronToOnCalendar("30 14 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "14:30:00") {
		t.Errorf("expected 14:30:00 in OnCalendar, got %q", out)
	}
}

func TestValidateScheduleSpec(t *testing.T) {
	good := HostScheduleSpec{
		ID: "x", MetisBinary: "/usr/local/bin/metis", JobID: "j1", Cron: "0 9 * * *",
	}
	if err := validateScheduleSpec(good); err != nil {
		t.Errorf("good spec rejected: %v", err)
	}
	bad := []HostScheduleSpec{
		{}, // all empty
		{ID: "x", MetisBinary: "metis", JobID: "j1", Cron: "0 9 * * *"}, // not abs
		{ID: "x", MetisBinary: "/path", JobID: "", Cron: "0 9 * * *"},   // empty job
	}
	for i, s := range bad {
		if err := validateScheduleSpec(s); err == nil {
			t.Errorf("bad spec #%d should error", i)
		}
	}
}
