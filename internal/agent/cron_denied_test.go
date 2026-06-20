package agent

import (
	"testing"
)

func TestCronDeniedRoundtrip(t *testing.T) {
	root := t.TempDir()
	const job = "job-123"

	// Empty store reads clean.
	if ds, err := ListCronDenials(root, job); err != nil || len(ds) != 0 {
		t.Fatalf("empty store: got %d denials, err=%v", len(ds), err)
	}

	d1 := CronDenial{Tool: "Bash", Input: "echo hi", Reason: "unauthorized", Suggest: "Bash(echo:*)"}
	d2 := CronDenial{Tool: "Write", Input: "/tmp/x", Reason: "unauthorized", Suggest: "Write"}
	if err := RecordCronDenial(root, job, d1); err != nil {
		t.Fatal(err)
	}
	if err := RecordCronDenial(root, job, d2); err != nil {
		t.Fatal(err)
	}

	got, err := ListCronDenials(root, job)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d denials, want 2", len(got))
	}
	if got[0].Suggest != "Bash(echo:*)" || got[1].Suggest != "Write" {
		t.Errorf("order/content wrong: %+v", got)
	}
	if got[0].At.IsZero() {
		t.Errorf("RecordCronDenial should stamp At when zero")
	}

	// Clear empties it; clearing again is not an error.
	if err := ClearCronDenials(root, job); err != nil {
		t.Fatal(err)
	}
	if ds, _ := ListCronDenials(root, job); len(ds) != 0 {
		t.Errorf("after clear: got %d, want 0", len(ds))
	}
	if err := ClearCronDenials(root, job); err != nil {
		t.Errorf("clearing a missing log should be nil, got %v", err)
	}
}

// Repeated denials of the same rule collapse to one entry so a job that
// re-denies every fire doesn't grow the log without bound.
func TestRecordCronDenialDedupsBySuggest(t *testing.T) {
	root := t.TempDir()
	const job = "j"
	for i := 0; i < 5; i++ {
		_ = RecordCronDenial(root, job, CronDenial{Tool: "Bash", Input: "echo a", Suggest: "Bash(echo:*)"})
	}
	// A different rule is still recorded.
	_ = RecordCronDenial(root, job, CronDenial{Tool: "Write", Input: "/x", Suggest: "Write"})
	got, _ := ListCronDenials(root, job)
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct denials after 5 dup + 1 new, got %d", len(got))
	}
}

func TestRecordCronDenialTruncatesInput(t *testing.T) {
	root := t.TempDir()
	long := ""
	for i := 0; i < 500; i++ {
		long += "x"
	}
	if err := RecordCronDenial(root, "j", CronDenial{Tool: "Bash", Input: long}); err != nil {
		t.Fatal(err)
	}
	got, _ := ListCronDenials(root, "j")
	if len(got) != 1 || len([]rune(got[0].Input)) > 301 {
		t.Errorf("input not truncated: len=%d", len([]rune(got[0].Input)))
	}
}
