package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHeader_JSONRoundTripPreservesFields(t *testing.T) {
	in := Header{
		ID:        "session-x",
		CreatedAt: time.Date(2026, 4, 29, 9, 0, 0, 0, time.UTC),
		Model:     "claude-opus-4-7",
		System:    "you are helpful",
		WorkDir:   "/repo",
		Mode:      "auto",
		Effort:    "high",
		Preset:    "creator",
		Status:    "completed",
		Title:     "refactor sprint",
		AlwaysAllow: []SavedRule{
			{Tool: "Bash", Match: "git status", Verb: 1, Source: "user-allow"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Header
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Model != in.Model || out.Title != in.Title || out.Effort != in.Effort || out.Preset != in.Preset || out.Status != in.Status {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if len(out.AlwaysAllow) != 1 || out.AlwaysAllow[0].Tool != "Bash" {
		t.Errorf("AlwaysAllow not preserved: %+v", out.AlwaysAllow)
	}
}

func TestHeader_OmitemptySaver(t *testing.T) {
	b, _ := json.Marshal(Header{ID: "x", Model: "m"})
	for _, want := range []string{`"system"`, `"work_dir"`, `"mode"`, `"title"`} {
		if strings.Contains(string(b), want) {
			t.Errorf("empty optional field should be omitted; saw %s in %s", want, b)
		}
	}
}

func TestListEntry_ZeroValueOK(t *testing.T) {
	var e ListEntry
	if e.ID != "" || e.Model != "" || e.Title != "" || e.Bytes != 0 {
		t.Errorf("zero-value ListEntry has nonzero fields: %+v", e)
	}
}
