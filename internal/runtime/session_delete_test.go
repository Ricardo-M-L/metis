package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolatedDeleteHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", home)
	return home
}

func TestDeleteSessionHistoryAndLearnedExactOwnership(t *testing.T) {
	home := isolatedDeleteHome(t)
	for _, entry := range []HistoryEntry{
		{SessionID: "sess", Input: "private"},
		{SessionID: "sess-extra", Input: "keep"},
	} {
		if err := AppendHistory(entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []LearnedRecord{
		{SessionID: "sess", Prompt: "private"},
		{SessionID: "sess-extra", Prompt: "keep"},
	} {
		if err := AppendLearned(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteSessionHistory("sess"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSessionLearned("sess"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{HistoryJSONLPath(), filepath.Join(home, "learned.jsonl")} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "private") || !strings.Contains(string(body), "keep") {
			t.Fatalf("exact ownership filter failed for %s: %s", path, body)
		}
	}
}

func TestDeleteSessionHistoryCorruptRowFailsWithoutRewrite(t *testing.T) {
	isolatedDeleteHome(t)
	original := []byte("{not-json}\n")
	if err := os.WriteFile(HistoryJSONLPath(), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSessionHistory("sess"); err == nil {
		t.Fatal("corrupt ownership row unexpectedly succeeded")
	}
	got, err := os.ReadFile(HistoryJSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("corrupt shared log changed: %q", got)
	}
}

func TestDeleteSessionPlansUsesEmbeddedOwnership(t *testing.T) {
	home := isolatedDeleteHome(t)
	dir := filepath.Join(home, "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteCurrentPlan("sess", "private current"); err != nil {
		t.Fatal(err)
	}
	currentTemp := filepath.Join(dir, ".sess.md.interrupted.tmp")
	if err := os.WriteFile(currentTemp, []byte("private temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownedArchive := filepath.Join(dir, "sess_1.json")
	neighborArchive := filepath.Join(dir, "sess_2.json") // misleading prefix
	if err := os.WriteFile(ownedArchive, []byte(`{"session":"sess"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(neighborArchive, []byte(`{"session":"sess-extra"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSessionPlans("sess"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{CurrentPlanPath("sess"), currentTemp, ownedArchive} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("owned plan remained: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(neighborArchive); err != nil {
		t.Fatalf("neighbor plan removed: %v", err)
	}
}
