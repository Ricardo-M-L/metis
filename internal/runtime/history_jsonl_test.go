package runtime

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAppendHistory_RoundTrip(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := AppendHistory(HistoryEntry{
		SessionID: "s-1", Input: "hello world", Source: "tui",
	}); err != nil {
		t.Fatal(err)
	}
	if err := AppendHistory(HistoryEntry{
		SessionID: "s-2", Input: "second prompt", Source: "repl",
	}); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(HistoryJSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var got []HistoryEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e HistoryEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Input != "hello world" || got[1].Input != "second prompt" {
		t.Errorf("entries don't match: %+v", got)
	}
	if got[0].Source != "tui" || got[1].Source != "repl" {
		t.Errorf("source not preserved: %+v", got)
	}
	if got[0].Timestamp.IsZero() {
		t.Error("timestamp should be auto-stamped")
	}
}

func TestAppendHistory_EmptyInputDropped(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := AppendHistory(HistoryEntry{Input: ""}); err != nil {
		t.Errorf("empty input should be no-op, not error; got %v", err)
	}
	if _, err := os.Stat(HistoryJSONLPath()); err == nil {
		t.Error("file should not be created for empty input")
	}
}

func TestAppendHistory_FilePerm0600(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	_ = AppendHistory(HistoryEntry{Input: "x"})
	st, err := os.Stat(HistoryJSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("history.jsonl perm = %o, want 600 (input may carry secrets)", mode)
	}
}

func TestAppendHistory_OneLinePerEntry(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for i := 0; i < 5; i++ {
		_ = AppendHistory(HistoryEntry{SessionID: "s", Input: "line"})
	}
	b, _ := os.ReadFile(HistoryJSONLPath())
	if n := strings.Count(string(b), "\n"); n != 5 {
		t.Errorf("expected 5 newlines, got %d (output: %s)", n, b)
	}
}
