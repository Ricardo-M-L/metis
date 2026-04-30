package tui

import (
	"testing"
	"time"
)

func TestTrimVisibleMessagesToLastUser(t *testing.T) {
	ts := time.Now()
	in := []Message{
		{Role: "info", Content: "banner", Timestamp: ts},
		{Role: "user", Content: "first", Timestamp: ts},
		{Role: "assistant", Content: "first reply", Timestamp: ts},
		{Role: "user", Content: "second", Timestamp: ts},
		{Role: "tool", Content: "Read /tmp/x", Timestamp: ts},
		{Role: "assistant", Content: "second reply", Timestamp: ts},
	}
	out := trimVisibleMessagesToLastUser(in)
	if len(out) != 3 {
		t.Fatalf("trim len = %d, want 3", len(out))
	}
	if out[1].Content != "first" {
		t.Errorf("expected first user msg preserved, got %+v", out[1])
	}
	if out[2].Content != "first reply" {
		t.Errorf("expected first assistant reply preserved, got %+v", out[2])
	}
}

func TestTrimVisibleMessagesToLastUser_NoUserNoChange(t *testing.T) {
	in := []Message{{Role: "info", Content: "banner"}}
	out := trimVisibleMessagesToLastUser(in)
	if len(out) != 1 {
		t.Errorf("trim with no user role should be identity; got len=%d", len(out))
	}
}

func TestTrimVisibleMessagesToLastUser_Empty(t *testing.T) {
	out := trimVisibleMessagesToLastUser(nil)
	if out != nil && len(out) != 0 {
		t.Errorf("trim on nil should be empty; got %v", out)
	}
}
