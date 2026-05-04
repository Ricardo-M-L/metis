package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestLoop_MaybeCompact_SnipFiresAtSoftThreshold — proves the snip tier
// runs at 70% threshold (vs full Compact at 85%) and emits the EventInfo
// "context snipped" exactly once. This is the missing live verification
// for #67: unit tests proved Snip() works in isolation, this proves the
// Loop layer wires it correctly.
func TestLoop_MaybeCompact_SnipFiresAtSoftThreshold(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})
	l := &Loop{Compactor: c}

	// Build a convo dominated by a fat tool_result that's BELOW the
	// 85% Compact threshold but ABOVE the 70% Snip threshold. With
	// effectiveInputCap=1000, snip fires at ~700 tokens, full at ~850.
	// One 4000-char tool_result is ~1000 tokens — well into Snip range.
	l.Messages = []llm.Message{
		msg(llm.RoleUser, "system seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", strings.Repeat("a", 4000)),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}
	if !c.ShouldSnip(l.Messages) {
		t.Fatalf("precondition: convo at est=%d should trigger ShouldSnip (cap=%d, thresh=%v)",
			estimateTokens(l.Messages), c.effectiveInputCap(), c.SnipThreshold)
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	snipNotices := 0
	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "context snipped") {
			snipNotices++
		}
	}
	if snipNotices != 1 {
		t.Errorf("expected exactly 1 'context snipped' notice; got %d", snipNotices)
	}
	// The fat tool_result should now be truncated.
	got := l.Messages[2].Content[0].ToolResult
	if len(got) >= 4000 {
		t.Errorf("tool_result was not snipped; len=%d (want < 4000)", len(got))
	}
	if !strings.Contains(got, "[snipped:") {
		t.Errorf("snipped result missing marker; got tail: %q", got[len(got)-60:])
	}
}

// TestLoop_MaybeCompact_NoSnipNoticeWhenNothingToSnip — when ShouldSnip
// returns true but Snip doesn't actually shorten anything (e.g. all
// tool_results already short), no event should fire. Otherwise the user
// gets phantom "snipped" notices for no-op cleanups.
func TestLoop_MaybeCompact_NoSnipNoticeWhenNothingToSnip(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.SnipMaxToolResultChars = 800
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})
	l := &Loop{Compactor: c}

	// Convo crosses 70% threshold via TEXT bulk, not tool_results.
	// Snip has nothing oversized to truncate → no message changes →
	// no notice should emit.
	l.Messages = []llm.Message{
		msg(llm.RoleUser, "seed"),
		msg(llm.RoleUser, strings.Repeat("x", 1500)),
		msg(llm.RoleAssistant, strings.Repeat("y", 1500)),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}
	if !c.ShouldSnip(l.Messages) {
		t.Fatalf("precondition: convo should trigger ShouldSnip")
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	close(out)

	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "context snipped") {
			t.Errorf("phantom 'snipped' notice fired for no-op pass; ev: %q", ev.Info)
		}
	}
}
