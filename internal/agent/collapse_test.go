package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestCollapseMiddle_FoldsEarlyMessages — basic contract: messages from
// ProtectFirst .. ProtectFirst+CollapseFoldWindow get replaced with a
// single summary boundary; tail stays intact byte-for-byte.
func TestCollapseMiddle_FoldsEarlyMessages(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.CollapseFoldWindow = 5
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	msgs := []llm.Message{
		msg(llm.RoleUser, "system seed"),
		// fold window — these 5 should be summarized
		msg(llm.RoleUser, "early-1"),
		msg(llm.RoleAssistant, "early-2"),
		msg(llm.RoleUser, "early-3"),
		msg(llm.RoleAssistant, "early-4"),
		msg(llm.RoleUser, "early-5"),
		// kept tail
		msg(llm.RoleAssistant, "later-1"),
		msg(llm.RoleUser, "later-2"),
		msg(llm.RoleAssistant, "later-3"),
	}
	out, err := c.CollapseMiddle(context.Background(), msgs)
	if err != nil {
		t.Fatalf("collapse err: %v", err)
	}

	// Expect: [system seed, summary boundary, later-1, later-2, later-3]
	// = 5 messages (or 6 if synthetic user-ack inserted).
	if len(out) > 6 || len(out) < 5 {
		t.Fatalf("expected 5 or 6 messages after collapse; got %d", len(out))
	}
	// First message preserved.
	if out[0].Content[0].Text != "system seed" {
		t.Errorf("ProtectFirst preserved: got %q", out[0].Content[0].Text)
	}
	// Boundary contains the summary marker.
	bodyText := out[1].Content[0].Text
	if !strings.Contains(bodyText, "Early conversation collapsed") {
		t.Errorf("boundary should mention 'collapsed'; got %q", bodyText)
	}
	if !strings.Contains(bodyText, "MOCK_SUMMARY") {
		t.Errorf("boundary should embed the summarizer's text; got %q", bodyText)
	}
	// Tail preserved byte-for-byte.
	tail := out[len(out)-3:]
	if tail[0].Content[0].Text != "later-1" || tail[1].Content[0].Text != "later-2" || tail[2].Content[0].Text != "later-3" {
		t.Errorf("tail mutated; got %+v", tail)
	}
}

// TestCollapseMiddle_NoOpWhenTooSmall — convo shorter than
// ProtectFirst+CollapseFoldWindow+ProtectLast+1 returns unchanged.
func TestCollapseMiddle_NoOpWhenTooSmall(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.CollapseFoldWindow = 5
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	short := []llm.Message{
		msg(llm.RoleUser, "a"),
		msg(llm.RoleAssistant, "b"),
		msg(llm.RoleUser, "c"),
	}
	out, err := c.CollapseMiddle(context.Background(), short)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != len(short) {
		t.Errorf("short convo should be no-op; got %d != %d", len(out), len(short))
	}
}

// TestCollapseMiddle_BreakerHonored — when the breaker is open, collapse
// short-circuits without calling the summarizer.
func TestCollapseMiddle_BreakerHonored(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.CollapseFoldWindow = 5
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})
	c.consecutiveFailures = MaxConsecutiveCompactFailures

	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		msg(llm.RoleUser, "1"), msg(llm.RoleAssistant, "2"),
		msg(llm.RoleUser, "3"), msg(llm.RoleAssistant, "4"),
		msg(llm.RoleUser, "5"),
		msg(llm.RoleAssistant, "tail-1"),
		msg(llm.RoleUser, "tail-2"),
		msg(llm.RoleAssistant, "tail-3"),
	}
	out, err := c.CollapseMiddle(context.Background(), msgs)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != len(msgs) {
		t.Errorf("breaker-tripped path should be no-op; got %d != %d", len(out), len(msgs))
	}
}
