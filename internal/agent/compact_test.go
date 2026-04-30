package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// fakeSummarizer returns "MOCK_SUMMARY" for any call. Implements the
// streaming path now that compact.go switched off Complete — the Stream
// shim feeds two events (a text_delta and a message_stop) so the
// streaming consumer in compact.go assembles the same string the old
// Complete path used to return.
type fakeSummarizer struct {
	calls   int
	lastReq llm.Request
}

func (f *fakeSummarizer) Name() string          { return "fake" }
func (f *fakeSummarizer) MaxContextTokens() int { return 100000 }
func (f *fakeSummarizer) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	f.calls++
	f.lastReq = req
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "MOCK_SUMMARY"},
		{Type: "message_stop"},
	}}, nil
}
func (f *fakeSummarizer) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("compact_test: Complete should not be called — compaction streams now")
}

// fakeStream is a tiny llm.StreamReader that walks a fixed slice of
// events. Returns io.EOF after the last event so consumeStream knows
// the stream is done.
type fakeStream struct {
	events []llm.StreamEvent
	idx    int
}

func (s *fakeStream) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}
func (s *fakeStream) Close() error { return nil }

// --- helpers ---------------------------------------------------------------

func msg(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func toolUseMsg(id, name string) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "calling tool"},
			{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: map[string]any{}},
		},
	}
}

func toolResultMsg(id, output string) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: id, ToolResult: output},
		},
	}
}

func newCompactorFor(p llm.Provider) *Compactor {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	return NewCompactor(cfg, "test-model", 1000, p)
}

// --- tests -----------------------------------------------------------------

func TestCompact_NoOpWhenSmall(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)

	msgs := []llm.Message{
		msg(llm.RoleSystem, "system"),
		msg(llm.RoleUser, "u1"),
		msg(llm.RoleAssistant, "a1"),
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected no-op, got %d messages (was %d)", len(out), len(msgs))
	}
	if p.calls != 0 {
		t.Errorf("summarizer should not be called for short conversations")
	}
}

func TestCompact_ProducesBoundaryWithSummary(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)

	// 1 system + 6 mid + 3 tail = 10 messages, well over the threshold
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "old user "+istr(i)))
		msgs = append(msgs, msg(llm.RoleAssistant, "old asst "+istr(i)))
	}
	for i := 0; i < 3; i++ {
		msgs = append(msgs, msg(llm.RoleAssistant, "recent "+istr(i)))
	}

	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Errorf("expected 1 summarize call, got %d", p.calls)
	}
	// Expect: 1 (first) + 1 (boundary asst) + 1 (synthetic user ack —
	// keepLast[0] is assistant) + 3 (last) = 6.
	//
	// Boundary is RoleAssistant (not RoleSystem) — bug #10 2026-04-30:
	// MiniMax + strict Anthropic reject mid-array system role with
	// error 2013, so the boundary is rendered narratively as if the
	// assistant said "I summarized our earlier conversation."
	if len(out) != 6 {
		t.Fatalf("expected 6 messages after compact, got %d", len(out))
	}
	if out[1].Role != llm.RoleAssistant {
		t.Errorf("boundary should be assistant role (mid-array system rejected by APIs), got %q", out[1].Role)
	}
	if len(out[1].Content) == 0 || !strings.Contains(out[1].Content[0].Text, "MOCK_SUMMARY") {
		t.Errorf("boundary missing summary, got: %v", out[1].Content)
	}
	// out[2] must be the synthetic user ack since out[1] (boundary)
	// is asst and keepLast[0] is also asst — strict alternation.
	if out[2].Role != llm.RoleUser {
		t.Errorf("synthetic user ack expected at out[2] (boundary asst → keepLast asst), got %q", out[2].Role)
	}
}

// TestCompact_BoundaryIsNeverSystem locks in bug #10 fix: the boundary
// message MUST NOT use RoleSystem because MiniMax (and strict
// Anthropic) reject mid-array system role with error 2013. RoleAssistant
// is the only safe choice that doesn't break user/assistant alternation
// for the typical user-first keepLast.
func TestCompact_BoundaryIsNeverSystem(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 5; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "u"+istr(i)))
		msgs = append(msgs, msg(llm.RoleAssistant, "a"+istr(i)))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range out {
		// keepFirst[0] is allowed to be system (the original prompt
		// passes through), but no SUBSEQUENT message can be.
		if i == 0 {
			continue
		}
		if m.Role == llm.RoleSystem {
			t.Errorf("compacted output[%d] has RoleSystem (would trigger MiniMax error 2013): %+v", i, m)
		}
	}
}

func TestCompact_ToolPairProtectsResult(t *testing.T) {
	// Build: [sys, u1, a1, asst[tool_use X], user[tool_result X], a-recent, a-recent, a-recent]
	// ProtectFirst=1, ProtectLast=3 → cut = 8-3 = 5, which lands ON the user[tool_result X].
	// Without protection we'd keep [a-recent x3] starting with an orphan tool_result is impossible
	// because the orphan would be inside keepLast — we actually want the assistant tool_use
	// to be the one preserved.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{
		msg(llm.RoleSystem, "sys"),
		msg(llm.RoleUser, "u1"),
		msg(llm.RoleAssistant, "a1"),
		toolUseMsg("call-1", "Read"),
		toolResultMsg("call-1", "file contents"),
		msg(llm.RoleAssistant, "recent1"),
		msg(llm.RoleAssistant, "recent2"),
		msg(llm.RoleAssistant, "recent3"),
	}
	// raw cut = 8 - 3 = 5 → messages[5] is "recent1", no tool_result, no adjustment.
	// But let's use a case where cut DOES land on a tool_result:
	c.ProtectLast = 4
	// Now cut = 8 - 4 = 4 → messages[4] is the tool_result, must back up to 3.
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	// After adjustment: keepFirst=1 + boundary=1(asst) + ack=1(user, since
	// keepLast[0] is the tool_use which is asst-role) + keepLast=5 = 8.
	if len(out) != 1+1+1+5 {
		t.Fatalf("expected 8 messages after pair-aware compact, got %d", len(out))
	}
	// out[3] starts the kept tail (after keepFirst + boundary + ack);
	// must be the tool_use, not an orphan tool_result.
	tail := out[3]
	hasToolUse := false
	for _, b := range tail.Content {
		if b.Type == "tool_use" {
			hasToolUse = true
			break
		}
	}
	if !hasToolUse {
		t.Errorf("kept tail should start with the tool_use (matching the preserved tool_result), got: %+v", tail)
	}
}

func TestCompact_NoOpWhenAdjustmentSwallowsMiddle(t *testing.T) {
	// Construct a conversation where every message after the first is a tool_result
	// (pathological case). adjustCutForToolPairs will walk all the way back to
	// ProtectFirst and Compact should bail out without summarizing.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, toolResultMsg("call-"+istr(i), "ignored"))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected no-op when adjustment swallows middle, got len=%d (orig=%d)", len(out), len(msgs))
	}
	if p.calls != 0 {
		t.Errorf("summarizer should not be called when no compaction is possible")
	}
}

func TestCompact_KeepLastSliceCorrect(t *testing.T) {
	// Regression: original code used messages[ProtectLast:] which is wrong when
	// len != 2*ProtectLast. Build a 12-message conversation; verify ProtectLast=5
	// keeps exactly the last 5 messages (not 7).
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 5
	c := NewCompactor(cfg, "m", 1000, p)

	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 11; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "msg "+istr(i)))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	// keepFirst (1) + boundary (1) + keepLast (5) = 7
	if len(out) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(out))
	}
	// Last kept message must be the original last one ("msg 10").
	last := out[len(out)-1]
	if last.Content[0].Text != "msg 10" {
		t.Errorf("last message wrong: %q", last.Content[0].Text)
	}
}

func TestAdjustCutForToolPairs_DoesNotMoveOnNormalMessage(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "u"),
		msg(llm.RoleAssistant, "a"),
		msg(llm.RoleUser, "u2"),
	}
	if got := adjustCutForToolPairs(msgs, 2); got != 2 {
		t.Errorf("cut moved unexpectedly: %d", got)
	}
}

func TestAdjustCutForToolPairs_BacksUpOverToolResult(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "u"),
		toolUseMsg("x", "Read"),
		toolResultMsg("x", "data"),
	}
	if got := adjustCutForToolPairs(msgs, 2); got != 1 {
		t.Errorf("cut should back up to 1, got %d", got)
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, strings.Repeat("a", 400)),
		msg(llm.RoleAssistant, strings.Repeat("b", 400)),
	}
	got := estimateTokens(msgs)
	// Per message: 4 (role) + per block: 8 (envelope) + 100 (text/4) = 112.
	// Two messages → 224.
	if got != 224 {
		t.Errorf("estimateTokens = %d, want 224", got)
	}
}

// TestEstimateTokens_ToolHeavy locks in that tool inputs and tool_results
// are no longer invisible to the estimator — the previous body-only
// estimator missed the bulk of token consumption in tool-heavy turns,
// which let auto-compaction silently undercount and the request
// overflow the provider's context window mid-stream.
func TestEstimateTokens_ToolHeavy(t *testing.T) {
	msgs := []llm.Message{
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{{
				Type:      "tool_use",
				ToolName:  "bash",
				ToolUseID: "tu_1",
				ToolInput: map[string]any{"command": strings.Repeat("x", 400)},
			}},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type:       "tool_result",
				ToolUseID:  "tu_1",
				ToolResult: strings.Repeat("y", 800),
			}},
		},
	}
	got := estimateTokens(msgs)
	// Sanity: must be > body-only zero (Text empty for both) and reflect
	// at least the 400 tool-input + 800 tool-result bytes (~300 tokens).
	if got < 250 {
		t.Errorf("estimateTokens should count tool inputs + results; got %d", got)
	}
}

func TestShouldCompact(t *testing.T) {
	p := &fakeSummarizer{}
	// maxCtx=200, threshold=0.5 → triggers at >= 100 tokens
	c := NewCompactor(Config{Threshold: 0.5}, "m", 200, p)
	short := []llm.Message{msg(llm.RoleUser, "hi")} // ~50 tokens
	if c.ShouldCompact(short) {
		t.Error("should not compact short conversation")
	}
	long := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 800))} // 50 + 200 = 250 tokens
	if !c.ShouldCompact(long) {
		t.Error("should compact long conversation past threshold")
	}
}

// TestShouldCompact_AccountsForMaxTokens locks in the bug-#6 fix: the
// effective input cap subtracts the per-request max_tokens budget so
// compaction fires *before* the API's `input + max_tokens > window`
// check rejects the next request.
//
// Repro of the user's MiniMax-M2.7 case in miniature:
//   - context window 1000, max_tokens 400, threshold 0.85
//   - effective input cap = 1000 - 400 = 600
//   - threshold trigger    = 600 * 0.85 = 510 tokens
//
// Without the fix the threshold would be 1000 * 0.85 = 850, so a 700-
// token conversation would NOT compact, then the next request (700
// input + 400 max_tokens = 1100 > 1000) would be rejected by the API.
func TestShouldCompact_AccountsForMaxTokens(t *testing.T) {
	p := &fakeSummarizer{}
	c := NewCompactor(Config{Threshold: 0.85}, "m", 1000, p)
	c.MaxOutputTokens = 400

	// 700 tokens (under raw 850 threshold, OVER effective 510 threshold).
	// estimateTokens charges ~12 tokens of envelope per message + 4 chars/token
	// for text, so ~2700 chars of body tips us over 700 tokens of input.
	mid := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 2700))}
	if !c.ShouldCompact(mid) {
		t.Errorf("should compact: input ~700 tokens > effective threshold (1000-400)*0.85=510")
	}

	// Verify the legacy path: when MaxOutputTokens is 0, falls back to
	// using full MaxContextTokens (so the original 850 threshold applies
	// and the same 700-token convo does NOT compact).
	c.MaxOutputTokens = 0
	if c.ShouldCompact(mid) {
		t.Errorf("legacy path: 700 tokens < 1000*0.85=850 threshold should NOT compact")
	}
}

// TestEffectiveInputCap_DefaultsSafely covers the degenerate config:
// max_tokens accidentally set bigger than context_window. We don't want
// effectiveInputCap to go negative or zero (would compact every turn).
func TestEffectiveInputCap_DefaultsSafely(t *testing.T) {
	c := &Compactor{
		MaxContextTokens: 1000,
		MaxOutputTokens:  2000, // user mis-config
	}
	if got := c.effectiveInputCap(); got != 1000 {
		t.Errorf("oversized MaxOutputTokens should fall back to full window; got %d", got)
	}
}

func istr(n int) string {
	return itoa(n)
}
