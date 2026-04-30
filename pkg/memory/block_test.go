package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBlock_JSONRoundTrip(t *testing.T) {
	in := Block{
		ID:        "b-1",
		Label:     "working",
		Content:   "remember this",
		MaxChars:  4096,
		CreatedAt: "2026-04-29T08:00:00Z",
		UpdatedAt: "2026-04-29T08:30:00Z",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Block
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip: got %+v, want %+v", out, in)
	}
}

func TestBlock_FieldNamesStable(t *testing.T) {
	// Lock the JSON field names so a future internal refactor can't
	// silently change the on-disk format and break 3rd-party plugins
	// that read memory files directly.
	b := Block{ID: "x", Label: "y", Content: "z", MaxChars: 1, CreatedAt: "a", UpdatedAt: "b"}
	bs, _ := json.Marshal(b)
	for _, want := range []string{`"id"`, `"label"`, `"content"`, `"max_chars"`, `"created_at"`, `"updated_at"`} {
		if !strings.Contains(string(bs), want) {
			t.Errorf("missing field %s in marshaled Block: %s", want, bs)
		}
	}
}
